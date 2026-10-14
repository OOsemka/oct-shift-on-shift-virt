package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mcsRelayBaseImage is the DataSource (OS image template) cloned for the
// relay VM. Cluster templates in openshift-virtualization-os-images are
// always present in OpenShift Virtualization and their DataSources are
// namespace-referenceable from any namespace.
func mcsRelayBaseImage() string {
	if v := os.Getenv("MCS_RELAY_BASE_IMAGE"); v != "" {
		return v
	}
	return "centos-stream9"
}

// deployMCSRelay provisions the Machine-Config-Server relay and rewires the
// API HAProxy through it.
//
// WHY this exists (proven live, chris-sos-odf): the nested MCS listens
// hostNetwork on the masters at 22623/22624. The host cluster's node
// nftables chain REJECTS tcp dport 22623 unless BOTH ends are in the pod
// network (10.128.0.0/14). HAProxy pods reach masters on 6443/443 (open
// ports) but NOT on 22623 — so ignition fetches from freshly booted
// masters ("https://api-int.<cluster>:22623/config/...") die at the HAProxy
// hop and masters retry forever ("Ignition (fetch) start job, no limit").
// Host infra must not be touched, and pod-side source-binding cannot evade
// the rule (no policy-routing rights in the pod). VM-to-VM traffic,
// however, is untouched. So we clone a tiny VM on the cUDN that forwards
// 12262/12264 -> masters 22623/22624, and HAProxy's MCS backends point at
// IT. HAProxy config is then hot-loaded (USR2) — pod IPs (and therefore DNS
// records and DHCP-sticky VIPs) never change.
func (s *StepExecutor) deployMCSRelay(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("mcs-relay")
	clusterNs := nc.Spec.Namespace

	var masterIPs []string
	for _, n := range nc.Status.Nodes {
		if n.IP != "" {
			masterIPs = append(masterIPs, n.IP)
		}
	}
	if len(masterIPs) == 0 {
		return fmt.Errorf("no node IPs in status for MCS relay backends")
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return err
	}

	pubKey := ""
	if b, err := s.client.GetSecretData(ctx, clusterNs, nc.Spec.ClusterName+"-ssh-key", "ssh-publickey"); err == nil {
		pubKey = strings.TrimSpace(string(b))
	}

	relayName := nc.Spec.ClusterName + "-mcs-relay"
	if err := s.client.CreateInfraVM(ctx, k8sInfraParams(nc, relayName, pubKey, masterIPs)); err != nil {
		return fmt.Errorf("creating MCS relay VM: %w", err)
	}

	log.Info("waiting for MCS relay VM to boot", "vm", relayName)
	relayIP, err := s.client.WaitForVMIReady(ctx, clusterNs, relayName, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("MCS relay VM never got an IP: %w", err)
	}
	// Deterministic plan: the relay's address is preseeded (IPAMClaim), so
	// a drift here means the claim was ignored — HAProxy rewire would then
	// silently target the wrong box. Fail loudly.
	if plan, perr := computeIPPlan(nc.Spec.ClusterName, nc.Spec.Network.CUDNSubnet, nc.Spec.Masters.Count,
		nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP); perr == nil && plan.MCSRelay.String() != relayIP {
		return fmt.Errorf("MCS relay got IP %s but reserved allocation is %s (check IPAMClaim %s.default)",
			relayIP, plan.MCSRelay.String(), relayName)
	}
	nc.Status.MCSRelayIP = relayIP
	s.updateCRStatus(ctx, nc)
	log.Info("MCS relay up", "ip", relayIP)

	if err := s.rewireHAProxyMCSToRelay(ctx, nc, relayIP); err != nil {
		return err
	}
	log.Info("HAProxy MCS backends rewired to relay (hot reload, no pod restart)")
	return nil
}

// rewireHAProxyMCSToRelay regenerates the API HAProxy config (now with MCS
// servers pointing at the relay) into the config secret and asks the running
// HAProxy process to reload gracefully.
func (s *StepExecutor) rewireHAProxyMCSToRelay(ctx context.Context, nc *crd.NestedCluster, relayIP string) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("mcs-relay")
	clusterNs := nc.Spec.Namespace

	cfg := s.generateHAProxyConfigAPI(nc)
	if !strings.Contains(cfg, relayIP) {
		return fmt.Errorf("generated HAProxy config does not reference relay %s (status.MCSRelayIP not visible?)", relayIP)
	}
	secretName := nc.Spec.ClusterName + "-haproxy-api-config"
	if _, err := s.client.Clientset.CoreV1().Secrets(clusterNs).Get(ctx, secretName, metav1.GetOptions{}); err != nil {
		// Normal first-install path: DeployHAProxy has not run yet and will
		// bake this config (relay included) into the FIRST generation.
		// APIVIPCUDN is legitimately empty at this point (it comes from the
		// HAProxy pod's cUDN lease AFTER DeployHAProxy) — do NOT gate on it
		// here or every clean install deadlocks the phase.
		log.Info("haproxy config secret not present yet; DeployHAProxy will include the relay backend", "ip", relayIP)
		return nil
	}
	if nc.Status.APIVIPCUDN == "" {
		return fmt.Errorf("API VIP cUDN IP missing while HAProxy config exists; cannot regenerate")
	}
	if err := s.client.UpdateSecretData(ctx, clusterNs, secretName, "haproxy.cfg", []byte(cfg)); err != nil {
		return fmt.Errorf("updating haproxy api config secret: %w", err)
	}

	// Give the kubelet time to propagate the secret into the mounted volume
	// (kubelet sync period + atomic dir swap).
	time.Sleep(20 * time.Second)

	pods, err := s.client.Clientset.CoreV1().Pods(clusterNs).List(ctx, metav1.ListOptions{
		LabelSelector: "oct-shift-on-shift-virt/cluster=" + nc.Spec.ClusterName + ",oct-shift-on-shift-virt/component=haproxy-api",
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no haproxy-api pods to reload: %w", err)
	}
	// Best effort: pods/exec may not be granted to the orchestrator. When
	// the phase runs BEFORE DeployHAProxy (the shipped order) this path is
	// never needed; on requeues the secret change is picked up when the
	// deployment rolls for any other reason. Warn, never fail the install.
	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning {
			continue
		}
		if _, stderr, err := s.client.ExecInPod(ctx, clusterNs, pod.Name, "haproxy",
			[]string{"/bin/sh", "-c", "haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg && kill -USR2 1"}); err != nil {
			log.Info("haproxy hot-reload not possible (config will apply on next pod roll)",
				"pod", pod.Name, "reason", err.Error(), "stderr", stderr)
			return nil
		}
		return nil
	}
	return nil
}

// relayUserData renders cloud-init user-data: a python TCP forwarder service
// (12262 -> masters:22623, 12264 -> masters:22624) plus the cluster SSH key
// for debugging. python3 ships in the base image; socat does not.
func relayUserData(hostname, pubKey string, masterIPs []string) string {
	masters := strings.Join(masterIPs, " ")
	authorized := ""
	if pubKey != "" {
		authorized = fmt.Sprintf("ssh_authorized_keys:\n  - %s\n", pubKey)
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
%swrite_files:
  - path: /opt/mcs-relay/relay.py
    permissions: '0755'
    content: |
      import socket, threading, sys, itertools
      MASTERS = sys.argv[1:]
      BACKENDS = {
          12262: [(m, 22623) for m in MASTERS], 12264: [(m, 22624) for m in MASTERS],
          6443:  [(m, 6443) for m in MASTERS],
          22623: [(m, 22623) for m in MASTERS], 22624: [(m, 22624) for m in MASTERS],
      }
      RR = {p: itertools.cycle(v) for p, v in BACKENDS.items()}
      LOCK = threading.Lock()

      def pump(src, dst):
          try:
              while True:
                  data = src.recv(65536)
                  if not data:
                      break
                  dst.sendall(data)
          except OSError:
              pass
          finally:
              for s in (src, dst):
                  try:
                      s.shutdown(socket.SHUT_RDWR)
                  except OSError:
                      pass

      def handle(client, listen_port):
          with LOCK:
              target = next(RR[listen_port])
          try:
              remote = socket.create_connection(target, timeout=10)
          except OSError:
              client.close()
              return
          t1 = threading.Thread(target=pump, args=(client, remote), daemon=True)
          t2 = threading.Thread(target=pump, args=(remote, client), daemon=True)
          t1.start(); t2.start(); t1.join(); t2.join()

      def serve(listen_port):
          srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
          srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
          srv.bind(("0.0.0.0", listen_port))
          srv.listen(128)
          while True:
              client, _ = srv.accept()
              threading.Thread(target=handle, args=(client, listen_port), daemon=True).start()

      for p in (12262, 12264):
          threading.Thread(target=serve, args=(p,), daemon=True).start()
      threading.Event().wait()
  - path: /etc/systemd/system/mcs-relay.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS MCS relay (12262/12264 -> masters 22623/22624)
      After=network-online.target
      [Service]
      ExecStart=/usr/bin/python3 /opt/mcs-relay/relay.py %s
      Restart=always
      RestartSec=3
      [Install]
      WantedBy=multi-user.target
runcmd:
  - systemctl daemon-reload
  - systemctl enable --now mcs-relay.service
`, hostname, authorized, masters)
}

func k8sInfraParams(nc *crd.NestedCluster, relayName, pubKey string, masterIPs []string) k8s.InfraVMParams {
	return k8s.InfraVMParams{
		Name:             relayName,
		Namespace:        nc.Spec.Namespace,
		ClusterName:      nc.Spec.ClusterName,
		Cores:            1,
		MemoryGi:         2,
		DiskGi:           32, // the centos-stream9 DataSource is 30Gi; clone must be >= source
		StorageClassName: nc.Spec.StorageClassName,
		DataSourceNS:     "openshift-virtualization-os-images",
		DataSourceName:   mcsRelayBaseImage(),
		UserData:         relayUserData(relayName, pubKey, masterIPs),
	}
}
