package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

// mcsRelayScript is the TCP forwarder that runs on the bastion VM:
// 12262/12264 -> masters 22623/22624. It must run on a VM, not a pod: the
// hypervisors' nftables reject SYNs to 22623/22624 from host- and
// pod-originated traffic, while VM->VM traffic never traverses those hooks.
// Listening on 12262/12264 (unrestricted) is what makes the HAProxy pod ->
// bastion hop legal.
const mcsRelayScript = `import socket, threading, sys, itertools
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
`
