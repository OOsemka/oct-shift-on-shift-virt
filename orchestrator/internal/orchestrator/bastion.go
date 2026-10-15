package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// bastionBaseImage is the DataSource cloned for the helper VM. Cluster
// templates in openshift-virtualization-os-images are always present with
// OpenShift Virtualization and are referenceable from any namespace.
func bastionBaseImage() string {
	if v := os.Getenv("BASTION_BASE_IMAGE"); v != "" {
		return v
	}
	return "centos-stream9"
}

// deployBastionEarly creates the bastion VM in the MCSRelay phase, BEFORE
// DeployHAProxy.
//
// The VM must exist this early for two separate reasons:
//
//  1. HAProxy's first generated config needs the MCS backend address, and a
//     later rewrite rolls the HAProxy pod — which changes its cUDN lease and
//     silently stales the DNS records built from it.
//  2. More importantly, the VM has to physically OCCUPY its address. A
//     preseeded IPAMClaim does NOT remove an address from OVN's dynamic pool;
//     it only pins it once bound to a running workload. An earlier attempt to
//     merely record the planned address here (and create the VM later) left
//     base+12 free, and OVN promptly handed it to the haproxy-ingress pod —
//     a direct collision with the bastion's own claim.
//
// DNS records are therefore NOT known yet (they come from the HAProxy leases),
// so dns.py is pushed in the DeployDNS phase by configureBastionDNS. The
// sos-dns unit is installed here with Restart=always and simply retries until
// that file lands.
func (s *StepExecutor) deployBastionEarly(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("bastion")
	clusterNs := nc.Spec.Namespace

	var masterIPs []string
	for _, n := range nc.Status.Nodes {
		if n.IP != "" {
			masterIPs = append(masterIPs, n.IP)
		}
	}
	if len(masterIPs) == 0 {
		return fmt.Errorf("no node IPs in status for the bastion's MCS backends")
	}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return err
	}

	pubKey := ""
	if b, err := s.client.GetSecretData(ctx, clusterNs, nc.Spec.ClusterName+"-ssh-key", "ssh-publickey"); err == nil {
		pubKey = strings.TrimSpace(string(b))
	}

	name := bastionVMName(nc.Spec.ClusterName)
	// A desktop plus a browser is unusable in 1 vCPU / 2Gi.
	cores, memGi := 1, 2
	if nc.Spec.Bastion.Graphical {
		cores, memGi = 2, 4
	}
	desktopUser, err := s.bastionDesktopCreds(ctx, nc)
	if err != nil {
		return err
	}
	if err := s.client.CreateInfraVM(ctx, k8s.InfraVMParams{
		Name:             name,
		Namespace:        clusterNs,
		ClusterName:      nc.Spec.ClusterName,
		Cores:            cores,
		MemoryGi:         memGi,
		DiskGi:           32, // the centos-stream9 DataSource is 30Gi; a clone must be >= source
		StorageClassName: nc.Spec.StorageClassName,
		DataSourceNS:     "openshift-virtualization-os-images",
		DataSourceName:   bastionBaseImage(),
		UserData:         bastionUserData(name, pubKey, masterIPs, nc.Spec.Bastion.Graphical, desktopUser),
	}); err != nil {
		return fmt.Errorf("creating bastion VM: %w", err)
	}

	log.Info("waiting for bastion VM to boot (holds its address before HAProxy pods appear)", "vm", name)
	ip, err := s.client.WaitForVMIReady(ctx, clusterNs, name, 15*time.Minute)
	if err != nil {
		return fmt.Errorf("bastion VM never got an IP: %w", err)
	}
	plan, perr := computeIPPlan(nc.Spec.ClusterName, nc.Spec.Network.CUDNSubnet, nc.Spec.Masters.Count,
		nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP)
	if perr == nil && plan.Bastion.String() != ip {
		return fmt.Errorf("bastion got IP %s but the reserved allocation is %s (check IPAMClaim %s.default)",
			ip, plan.Bastion.String(), name)
	}
	nc.Status.MCSRelayIP = ip
	s.updateCRStatus(ctx, nc)
	log.Info("bastion up (MCS relay + sshd); DNS records follow in DeployDNS", "ip", ip)

	return s.rewireHAProxyMCSToRelay(ctx, nc, ip)
}

// configureBastionDNS runs in the DeployDNS phase: it renders the nested
// DNS server (now that the HAProxy cUDN leases exist) and pushes it onto the
// bastion over SSH, then starts the unit.
//
// The push happens from a Job inside the cluster namespace because the
// orchestrator pod cannot reach cUDN overlay IPs — the same vantage point and
// machinery ConfigureNodes already uses (ops SA, sos-ssh-helper, cluster key).
func (s *StepExecutor) configureBastionDNS(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("bastion")
	clusterNs := nc.Spec.Namespace

	bastionIP := nc.Status.MCSRelayIP
	if bastionIP == "" {
		return fmt.Errorf("bastion IP missing from status (MCSRelay phase must run first)")
	}
	apiVIP := s.resolveControlPlaneVIP(ctx, nc)
	ingressVIP := s.resolveIngressVIP(ctx, nc)
	if apiVIP == "" || ingressVIP == "" {
		return fmt.Errorf("cannot build nested DNS records: apiVIP=%q ingressVIP=%q (DeployHAProxy must run first)", apiVIP, ingressVIP)
	}
	if apiVIP == bastionIP || ingressVIP == bastionIP {
		return fmt.Errorf("haproxy lease collides with the bastion address %s (api=%s ingress=%s) — the bastion must claim its IP before the haproxy pods start",
			bastionIP, apiVIP, ingressVIP)
	}
	localZone := nc.Spec.ClusterName + "." + nc.Spec.BaseDomain

	cmName := nc.Spec.ClusterName + "-dns-script"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: clusterNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
				"oct-shift-on-shift-virt/component": "bastion-dns",
			},
		},
		Data: map[string]string{
			"dns.py":        s.generateDNSScript(apiVIP, ingressVIP, localZone),
			"push-dns.sh":   bastionPushDNSScript,
			"verify-dns.py": bastionVerifyDNSScript,
		},
	}
	if _, err := s.client.Clientset.CoreV1().ConfigMaps(clusterNs).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("creating dns script configmap: %w", err)
		}
		if _, uerr := s.client.Clientset.CoreV1().ConfigMaps(clusterNs).Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("updating dns script configmap: %w", uerr)
		}
	}

	sshSecretName := nc.Spec.SSHKeyRef.Name
	if sshSecretName == "" {
		sshSecretName = nc.Spec.ClusterName + "-ssh-key"
	}
	jobName := nc.Spec.ClusterName + "-bastion-dns"
	backoff := int32(4)
	deadline := int64(900)
	ttl := int32(3600)
	labels := map[string]string{
		"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
		"oct-shift-on-shift-virt/component": "bastion-dns",
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: clusterNs, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: nc.Spec.ClusterName + "-ops",
					Containers: []corev1.Container{{
						Name:            "push",
						Image:           sshHelperImage(),
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"/bin/bash", "/dns/push-dns.sh"},
						Env: []corev1.EnvVar{
							{Name: "BASTION_IP", Value: bastionIP},
							{Name: "DESKTOP_USER", Value: bastionDesktopName(nc)},
							{Name: "GRAPHICAL", Value: fmt.Sprintf("%t", nc.Spec.Bastion.Graphical)},
							{Name: "API_IP", Value: apiVIP},
							{Name: "INGRESS_IP", Value: ingressVIP},
							{Name: "CLUSTER_DOMAIN", Value: localZone},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "dns", MountPath: "/dns", ReadOnly: true},
							{Name: "sshkey", MountPath: "/sshkey", ReadOnly: true},
							{Name: "deskpw", MountPath: "/deskpw", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "dns", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName}}}},
						{Name: "sshkey", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: sshSecretName,
							Items:      []corev1.KeyToPath{{Key: "ssh-privatekey", Path: "ssh-privatekey"}}}}},
						{Name: "deskpw", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
							SecretName: nc.Spec.ClusterName + "-bastion-password",
							Optional:   boolPtr(true)}}},
					},
				},
			},
		},
	}
	if err := s.client.DeleteJob(ctx, clusterNs, jobName); err != nil {
		return err
	}
	if err := s.client.CreateJob(ctx, job); err != nil {
		return fmt.Errorf("creating bastion-dns job: %w", err)
	}
	log.Info("pushing nested DNS onto the bastion", "bastion", bastionIP, "api", apiVIP, "ingress", ingressVIP)
	if err := s.client.WaitForJobCompletion(ctx, clusterNs, jobName, 20*time.Minute); err != nil {
		return fmt.Errorf("bastion-dns push failed: %w", err)
	}

	nc.Status.DNSIPCUDN = bastionIP
	s.updateCRStatus(ctx, nc)
	log.Info("nested DNS live on the bastion", "ip", bastionIP)

	ann := map[string]string{
		"sos.octools.io/bastion":        "ssh core@" + bastionIP,
		"sos.octools.io/bastion-access": "oc -n " + clusterNs + " run diag --rm -it --restart=Never --image=" + sshHelperImage() + " --command -- bash  # in-ns pods reach every cUDN IP",
	}
	if nc.Spec.Bastion.Graphical {
		// The desktop is reached through the OpenShift console's own VM
		// console (KubeVirt VNC), so it needs no Route, no VLAN IP and no
		// extra exposure — and the browser on that box resolves the nested
		// zone from the DNS server running beside it.
		ann["sos.octools.io/bastion-console"] = fmt.Sprintf(
			"OpenShift console -> Virtualization -> VirtualMachines -> %s -> Console (login %s, password in secret %s-bastion-password)",
			bastionVMName(nc.Spec.ClusterName), bastionDesktopName(nc), nc.Spec.ClusterName)
		ann["sos.octools.io/nested-console"] = "https://console-openshift-console.apps." + localZone + " (open in a browser ON the bastion)"
	}
	if patchBody, merr := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"annotations": ann}}); merr != nil {
		log.Info("failed to encode bastion annotation patch", "error", merr.Error())
	} else if _, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
		Patch(ctx, nc.Name, types.MergePatchType, patchBody, metav1.PatchOptions{}); err != nil {
		log.Info("failed to annotate CR with the bastion address (non-fatal)", "error", err.Error())
	}
	return nil
}

// bastionUserData renders cloud-init for the combined helper VM: the nested
// DNS server, the MCS relay and the operator SSH key. python3 ships in the
// cloud image; the VM runs as root so binding :53 needs no privilege work.
func bastionUserData(hostname, pubKey string, masterIPs []string, graphical bool, desktopUser string) string {
	masters := strings.Join(masterIPs, " ")
	// An explicit users: block is REQUIRED. The top-level ssh_authorized_keys
	// key installs onto the image's DEFAULT user, which on CentOS Stream 9 is
	// cloud-user — so SSHing as core (what every helper job does, matching
	// RHCOS) is denied. That is the same trap that left the old dns-vm with
	// "no working SSH user"; the jumpbox got it right with a users: block.
	authorized := ""
	if pubKey != "" {
		var items []string
		for _, k := range strings.Split(pubKey, "\n") {
			if k = strings.TrimSpace(k); k != "" {
				items = append(items, "      - "+k)
			}
		}
		if len(items) > 0 {
			authorized = "users:\n  - name: core\n    groups: wheel\n    shell: /bin/bash\n" +
				"    sudo: 'ALL=(ALL) NOPASSWD:ALL'\n    ssh_authorized_keys:\n" +
				strings.Join(items, "\n") + "\n"
		}
	}
	desktop := ""
	if graphical && desktopUser != "" {
		// The desktop is installed in the BACKGROUND: it is a few hundred MB
		// over UDN egress and must never hold up the DeployDNS phase, which
		// only needs sshd and the relay to be up.
		//
		// resolv.conf is pinned to 127.0.0.1 so a browser on this box
		// resolves the nested zone from the DNS server running beside it —
		// that is the whole point of putting the console here rather than
		// behind a Route (no Host rewriting, no nested OAuth redirect
		// problem).
		desktop = fmt.Sprintf(`  - useradd -m -s /bin/bash -G wheel %[1]s || true
  - printf 'nameserver 127.0.0.1\n' > /etc/resolv.conf
  - chattr +i /etc/resolv.conf || true
  - nohup bash -c 'dnf -y group install "Xfce" || dnf -y install @xfce-desktop-environment; dnf -y install firefox tigervnc-server; systemctl set-default graphical.target; systemctl isolate graphical.target' >/var/log/sos-desktop.log 2>&1 &
`, desktopUser)
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
%swrite_files:
  - path: /opt/mcs-relay/relay.py
    permissions: '0755'
    content: |
%s  - path: /etc/systemd/system/sos-dns.service
    permissions: '0644'
    content: |
      [Unit]
      Description=SOS nested DNS (authoritative for the nested zone, forwards the rest)
      After=network-online.target
      [Service]
      # dns.py is pushed by the DeployDNS phase once the HAProxy leases exist;
      # until then this simply retries every few seconds.
      ExecStart=/usr/bin/python3 /opt/sos-dns/dns.py
      Restart=always
      RestartSec=5
      StartLimitIntervalSec=0
      [Install]
      WantedBy=multi-user.target
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
  - systemctl enable --now sos-dns.service
  - systemctl enable --now mcs-relay.service
%s`, hostname, authorized, indentBlock(mcsRelayScript, "      "), masters, desktop)
}

// bastionPushDNSScript installs the rendered dns.py onto the bastion and
// proves the server answers before the phase is allowed to pass. Runs in a
// Job inside the cluster namespace — the only vantage point that can reach
// cUDN overlay addresses.
const bastionPushDNSScript = `#!/bin/bash
set -uo pipefail
install -m 600 /sshkey/ssh-privatekey /tmp/k
SSHOPT="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=8 -o LogLevel=ERROR -i /tmp/k"

# core is what every helper job uses (RHCOS convention, and the bastion's
# cloud-init creates it). Fall back to the cloud image's default users so a
# different BASTION_BASE_IMAGE cannot silently wedge this phase.
USER_OK=""
for i in $(seq 1 60); do
    for u in core cloud-user centos fedora; do
        if ssh $SSHOPT "$u@$BASTION_IP" true 2>/dev/null; then USER_OK=$u; break; fi
    done
    [ -n "$USER_OK" ] && break
    sleep 10
done
if [ -z "$USER_OK" ]; then
    echo "FATAL: bastion $BASTION_IP is not reachable over ssh as core/cloud-user/centos/fedora after 10min"
    exit 1
fi
echo "OK: bastion $BASTION_IP reachable as $USER_OK"

ssh $SSHOPT "$USER_OK@$BASTION_IP" "sudo install -D -m 0755 /dev/stdin /opt/sos-dns/dns.py" < /dns/dns.py || {
    echo "FATAL: could not write dns.py onto the bastion"; exit 1; }
ssh $SSHOPT "$USER_OK@$BASTION_IP" "sudo systemctl restart sos-dns.service" || {
    echo "FATAL: sos-dns.service would not start"; exit 1; }
echo "OK: dns.py installed and service restarted"

# Prove it ANSWERS, against the server itself — a getent here would query the
# bastion's own (DHCP) resolver, not ours, and tell us nothing.
ssh $SSHOPT "$USER_OK@$BASTION_IP" "sudo install -D -m 0755 /dev/stdin /tmp/verify-dns.py" < /dns/verify-dns.py || exit 1
VERIFIED=0
for i in $(seq 1 30); do
    if ssh $SSHOPT "$USER_OK@$BASTION_IP" "python3 /tmp/verify-dns.py api-int.$CLUSTER_DOMAIN $API_IP"; then
        echo "DNS_VERIFIED"
        VERIFIED=1
        break
    fi
    sleep 5
done

# Desktop password: set over SSH, never baked into the VM's cloud-init (that
# user-data is readable by anyone who can describe the VMI).
if [ "${GRAPHICAL:-false}" = "true" ] && [ -s /deskpw/password ]; then
    if ssh $SSHOPT "$USER_OK@$BASTION_IP" "sudo chpasswd" < <(printf '%s:%s' "$DESKTOP_USER" "$(cat /deskpw/password)"); then
        echo "OK: desktop password set for $DESKTOP_USER"
    else
        echo "WARN: could not set the desktop password (console login will not work)"
    fi
fi

if [ "$VERIFIED" = "1" ]; then
    exit 0
fi
echo "FATAL: api-int.$CLUSTER_DOMAIN never resolved to $API_IP from the bastion's own DNS"
exit 1
`

// bastionVerifyDNSScript asks the bastion's own resolver for an A record and
// also asserts AAAA answers promptly — a silently dropped AAAA starves every
// AF_UNSPEC client (curl, Go, kubelet) and is invisible to getent.
const bastionVerifyDNSScript = `import socket, struct, sys, time

def query(name, qtype):
    hdr = struct.pack(">HHHHHH", 0x1234, 0x0100, 1, 0, 0, 0)
    qn = b"".join(bytes([len(p)]) + p.encode() for p in name.split(".")) + b"\x00"
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(3)
    t = time.time()
    s.sendto(hdr + qn + struct.pack(">HH", qtype, 1), ("127.0.0.1", 53))
    data, _ = s.recvfrom(4096)
    return data, time.time() - t

name, want = sys.argv[1], sys.argv[2]
try:
    data, _ = query(name, 1)
except Exception as e:
    print("A query failed: %s" % e)
    sys.exit(1)
if (data[3] & 0xF) != 0 or struct.unpack(">H", data[6:8])[0] < 1:
    print("A query: rcode=%d answers=%d" % (data[3] & 0xF, struct.unpack(">H", data[6:8])[0]))
    sys.exit(1)
got = ".".join(str(b) for b in data[-4:])
if got != want:
    print("A %s -> %s, wanted %s" % (name, got, want))
    sys.exit(1)
try:
    aaaa, elapsed = query(name, 28)
except Exception as e:
    print("AAAA DROPPED (%s) — AF_UNSPEC clients would hang" % e)
    sys.exit(1)
if (aaaa[3] & 0xF) != 0:
    print("AAAA rcode=%d (want NOERROR/empty)" % (aaaa[3] & 0xF))
    sys.exit(1)
print("A %s -> %s, AAAA NOERROR in %.3fs" % (name, got, elapsed))
`

// bastionDesktopName is the desktop login. Defaults to the same "core"
// identity used for SSH so operators have one name to remember.
func bastionDesktopName(nc *crd.NestedCluster) string {
	if u := strings.TrimSpace(nc.Spec.Bastion.Username); u != "" {
		return u
	}
	return "core"
}

// bastionDesktopCreds stores the desktop password in its OWN Secret and
// clears it from the CR.
//
// The password deliberately never reaches the VM's cloud-init: that user-data
// is itself a Secret, but it is mounted into the VM and readable by anyone
// who can describe the VMI, whereas this Secret is only mounted into the
// short-lived push Job. A wizard-supplied password is also wiped from
// spec.bastion.password once copied, so it does not sit in the CR (which is
// world-readable to anyone with get on nestedclusters).
func (s *StepExecutor) bastionDesktopCreds(ctx context.Context, nc *crd.NestedCluster) (string, error) {
	if !nc.Spec.Bastion.Graphical {
		return "", nil
	}
	user := bastionDesktopName(nc)
	secretName := nc.Spec.ClusterName + "-bastion-password"

	if existing, err := s.client.GetSecretData(ctx, nc.Spec.Namespace, secretName, "password"); err == nil && len(existing) > 0 {
		return user, nil
	}
	pw := strings.TrimSpace(nc.Spec.Bastion.Password)
	generated := false
	if pw == "" {
		b := make([]byte, 12)
		if _, err := rand.Read(b); err != nil {
			return "", fmt.Errorf("generating bastion password: %w", err)
		}
		pw = base64.RawURLEncoding.EncodeToString(b)
		generated = true
	}
	if err := s.client.CreateOpaqueSecret(ctx, nc.Spec.Namespace, secretName, map[string][]byte{
		"username": []byte(user),
		"password": []byte(pw),
	}); err != nil {
		return "", fmt.Errorf("storing bastion desktop password: %w", err)
	}
	s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("bastion").
		Info("bastion desktop credentials stored", "secret", secretName, "user", user, "generated", generated)

	if nc.Spec.Bastion.Password != "" {
		patch := []byte(`{"spec":{"bastion":{"password":""}}}`)
		if _, err := s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
			Patch(ctx, nc.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("bastion").
				Info("could not clear the password from the CR (it is already in a Secret)", "error", err.Error())
		}
		nc.Spec.Bastion.Password = ""
	}
	return user, nil
}

func boolPtr(b bool) *bool { return &b }
