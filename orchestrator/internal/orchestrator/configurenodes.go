package orchestrator

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sshHelperImage is a tiny image (centos stream9 + openssh-clients) used to
// SSH into nested VMs from INSIDE the cluster namespace. The orchestrator pod
// itself cannot reach cUDN overlay IPs (cross-namespace overlay traffic is
// blocked), which is why this runs as a Job in the cluster namespace.
func sshHelperImage() string {
	if v := os.Getenv("SSH_HELPER_IMAGE"); v != "" {
		return v
	}
	return "quay.io/cjanisze/sos-ssh-helper:latest"
}

// configureNodes runs after PowerOn and before WaitBootstrap. It fixes two
// facts of life on OVN-K primary user-defined networks:
//
//  1. UDN DHCP hands every VM the HOST cluster's CoreDNS IP + host search
//     domains. The UDN is isolated from the host service network, so that
//     resolver is dead on arrival — and the OCP installer's checks (and the
//     assisted-agent's domain-resolution steps) then fail forever with
//     "api-int.<zone>: no such host". We pin NM to the nested DNS pod on the
//     UDN instead. The change lands in a persisted .nmconnection which
//     coreos-installer --copy-network carries into the installed OS.
//
//  2. The assisted-agent's next-step-runner container snapshots
//     /etc/resolv.conf at creation time, so the agent must be restarted
//     after the DNS change or domain-resolution keeps using stale config.
//
// It also discovers the real rendezvous node (assisted-service :8090) from
// inside the namespace and rewrites the installer state secret when it
// differs from the IP baked into the ISO — the wait-bootstrap job polls that
// IP and hangs forever if it's wrong.
func (s *StepExecutor) configureNodes(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("configure-nodes")
	clusterNs := nc.Spec.Namespace

	var nodeIPs []string
	for _, n := range nc.Status.Nodes {
		if n.IP != "" {
			nodeIPs = append(nodeIPs, n.IP)
		}
	}
	if len(nodeIPs) == 0 {
		return fmt.Errorf("no node IPs in status — CollectIPs must have run")
	}

	dnsIP := nc.Status.DNSIPCUDN
	if dnsIP == "" && !dnsVMMode() {
		ip, err := s.client.WaitForDeploymentCUDNIP(ctx, clusterNs, nc.Spec.ClusterName+"-dns", 2*time.Minute)
		if err != nil {
			return fmt.Errorf("DNS pod cUDN IP unavailable for node configuration: %w", err)
		}
		dnsIP = ip
		nc.Status.DNSIPCUDN = ip
	}
	// HAProxy lease is the authoritative api VIP (VM->pod MCS rides the OVN
	// pipeline, unfiltered); the relay VM is a fallback front only.
	apiIP := nc.Status.APIVIPCUDN
	if apiIP == "" {
		apiIP = nc.Status.MCSRelayIP
	}
	ingressIP := nc.Status.IngressVIPCUDN
	if apiIP == "" || ingressIP == "" {
		return fmt.Errorf("HAProxy cUDN IPs not in status (DeployHAProxy/DeployDNS must run first)")
	}
	clusterDomain := nc.Spec.ClusterName + "." + nc.Spec.BaseDomain

	// anyuid for the helper job (needs root only to chmod the SSH key to the
	// 0600 ssh demands — the SA/CRB are created early, in deployHAProxy, so
	// the SCC admission cache has long caught up by the time this Job lands).
	opsSA := nc.Spec.ClusterName + "-ops"
	ownerLabels := map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}
	if err := s.ensureOpsSA(ctx, nc); err != nil {
		return err
	}

	cmName := nc.Spec.ClusterName + "-configure-script"
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: clusterNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
				"oct-shift-on-shift-virt/component": "configure-nodes",
			},
		},
		Data: map[string]string{
			"configure-nodes.sh": configureNodesScript,
			"fix-node.sh":        fixNodeScript,
			"guard-loop.sh":      guardLoopScript,
			"guard-node.sh":      guardNodeScript,
		},
	}
	if _, err := s.client.Clientset.CoreV1().ConfigMaps(clusterNs).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("creating configure-nodes configmap: %w", err)
		}
		if _, uerr := s.client.Clientset.CoreV1().ConfigMaps(clusterNs).Update(ctx, cm, metav1.UpdateOptions{}); uerr != nil {
			return fmt.Errorf("updating configure-nodes configmap: %w", uerr)
		}
	}

	sshSecretName := nc.Spec.SSHKeyRef.Name
	if sshSecretName == "" {
		sshSecretName = nc.Spec.ClusterName + "-ssh-key"
	}

	jobName := nc.Spec.ClusterName + "-configure-nodes"
	backoffLimit := int32(6) // absorbs an SCC cache-lag miss or a transient pull error
	deadlineSec := int64(2400)
	ttl := int32(604800)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: clusterNs,
			Labels: map[string]string{
				"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
				"oct-shift-on-shift-virt/component": "configure-nodes",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			ActiveDeadlineSeconds:   &deadlineSec,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: ownerLabels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: opsSA,
					Containers: []corev1.Container{{
						Name:            "configure",
						Image:           sshHelperImage(),
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"/bin/bash", "/scripts/configure-nodes.sh"},
						Env: []corev1.EnvVar{
							{Name: "NODE_IPS", Value: strings.Join(nodeIPs, " ")},
							{Name: "DNS_IP", Value: dnsIP},
							{Name: "API_IP", Value: apiIP},
							{Name: "INGRESS_IP", Value: ingressIP},
							{Name: "CLUSTER_DOMAIN", Value: clusterDomain},
							{Name: "SSH_USER", Value: "core"},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
							{Name: "sshkey", MountPath: "/sshkey", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "scripts", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
							},
						}},
						{Name: "sshkey", VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: sshSecretName,
								Items:      []corev1.KeyToPath{{Key: "ssh-privatekey", Path: "ssh-privatekey"}},
							},
						}},
					},
				},
			},
		},
	}

	// A stale Failed job (e.g. from before sshPrivateKey was supplied) must
	// not wedge retries: delete then recreate.
	if err := s.client.DeleteJob(ctx, clusterNs, jobName); err != nil {
		return err
	}
	if err := s.client.CreateJob(ctx, job); err != nil {
		return fmt.Errorf("creating configure-nodes job: %w", err)
	}

	log.Info("running in-namespace node configuration job",
		"job", jobName, "nodes", nodeIPs, "dns", dnsIP, "api", apiIP)
	if err := s.client.WaitForJobCompletion(ctx, clusterNs, jobName, 45*time.Minute); err != nil {
		return fmt.Errorf("configure-nodes failed: %w", err)
	}

	logs, err := s.client.JobPodLogs(ctx, clusterNs, jobName)
	if err != nil {
		log.Info("could not read configure-nodes job logs", "error", err.Error())
	}
	s.applyRendezvousFindings(ctx, nc, logs, log)

	if err := s.ensureNodeGuard(ctx, nc, nodeIPs, dnsIP, apiIP, clusterDomain, sshSecretName); err != nil {
		// Non-fatal: the guard repairs post-pivot regressions (DNS revert,
		// bootstrap kubeconfig pointing at localhost:6443). Losing it is
		// painful but not install-fatal; the one-shot fixes above landed.
		log.Error("node-guard deployment failed (continuing)", "error", err.Error())
	}
	return nil
}

// ensureNodeGuard maintains <cluster>-node-guard: a long-lived in-namespace
// ssh loop that permanently repairs the two post-pivot regressions proven
// live on chris-sos-odf:
//  1. the pivot rewrites /etc/resolv.conf back to the (unreachable) host
//     cluster CoreDNS handed out by UDN DHCP — re-pin on whatever connection
//     owns the default route (ovs-if-br-ex post-pivot, not the OVS port conn);
//  2. master-1..n get /etc/kubernetes/kubeconfig with server:
//     https://localhost:6443, so kubelet talks to itself and NO node object
//     or CSR ever appears — rewrite to the API VIP and restart kubelet once.
func (s *StepExecutor) ensureNodeGuard(ctx context.Context, nc *crd.NestedCluster, nodeIPs []string, dnsIP, apiIP, clusterDomain, sshSecretName string) error {
	clusterNs := nc.Spec.Namespace
	name := nc.Spec.ClusterName + "-node-guard"
	labels := map[string]string{
		"oct-shift-on-shift-virt/cluster":   nc.Spec.ClusterName,
		"oct-shift-on-shift-virt/component": "node-guard",
	}
	replicas := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: clusterNs, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: nc.Spec.ClusterName + "-ops",
					SecurityContext:    &corev1.PodSecurityContext{RunAsUser: ptrInt64(0)},
					Containers: []corev1.Container{{
						Name:            "guard",
						Image:           sshHelperImage(),
						ImagePullPolicy: corev1.PullAlways,
						Command:         []string{"/bin/bash", "/scripts/guard-loop.sh"},
						Env: []corev1.EnvVar{
							{Name: "NODE_IPS", Value: strings.Join(nodeIPs, " ")},
							{Name: "DNS_IP", Value: dnsIP},
							{Name: "API_IP", Value: apiIP},
							{Name: "CLUSTER_DOMAIN", Value: clusterDomain},
							{Name: "SSH_USER", Value: "core"},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "scripts", MountPath: "/scripts", ReadOnly: true},
							{Name: "sshkey", MountPath: "/sshkey", ReadOnly: true},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "scripts", VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: nc.Spec.ClusterName + "-configure-script"},
							},
						}},
						{Name: "sshkey", VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: sshSecretName},
						}},
					},
				},
			},
		},
	}
	appsv1c := s.client.Clientset.AppsV1().Deployments(clusterNs)
	if _, err := appsv1c.Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
		existing, gerr := appsv1c.Get(ctx, name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		dep.ResourceVersion = existing.ResourceVersion
		if _, err := appsv1c.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("configure-nodes").
		Info("node-guard deployment ensured", "deployment", name)
	return nil
}

func ptrInt64(v int64) *int64 { return &v }

// ensureOpsSA creates (idempotently) the helper-job ServiceAccount + anyuid
// ClusterRoleBinding. Called from deployHAProxy so the grant is old enough by
// the time the job pod is admitted.
func (s *StepExecutor) ensureOpsSA(ctx context.Context, nc *crd.NestedCluster) error {
	opsSA := nc.Spec.ClusterName + "-ops"
	ownerLabels := map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}
	// Re-ensure the registry secret here too: ConfigureNodes is the last
	// gate before private-image pulls, and clusters created before this
	// phase existed never ran the copy in CreateSecrets.
	if err := s.client.CopySecret(ctx, "oct-shift-on-shift-virt", "quay-pull-secret", nc.Spec.Namespace, "quay-pull-secret"); err != nil &&
		!strings.Contains(err.Error(), "already exists") && !strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("copying quay-pull-secret: %w", err)
	}
	if err := s.client.CreateServiceAccount(ctx, nc.Spec.Namespace, opsSA, ownerLabels); err != nil {
		return fmt.Errorf("creating ops service account: %w", err)
	}
	if err := s.client.CreateClusterRoleBinding(ctx, "sos-ops-anyuid-"+nc.Spec.ClusterName, "system:openshift:scc:anyuid", opsSA, nc.Spec.Namespace, ownerLabels); err != nil {
		return fmt.Errorf("granting anyuid SCC to ops: %w", err)
	}
	// Private-registry pull: jobs (configure-nodes, node-guard) and wait-jobs
	// run under ops/default in the cluster namespace.
	for _, sa := range []string{opsSA, "default"} {
		if err := s.ensurePullSecretOnSA(ctx, nc.Spec.Namespace, sa); err != nil {
			s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("configure-nodes").
				Info("imagePullSecrets not attached", "sa", sa, "error", err.Error())
		}
	}
	return nil
}

func (s *StepExecutor) ensurePullSecretOnSA(ctx context.Context, namespace, saName string) error {
	sas := s.client.Clientset.CoreV1().ServiceAccounts(namespace)
	sa, err := sas.Get(ctx, saName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, is := range sa.ImagePullSecrets {
		if is.Name == "quay-pull-secret" {
			return nil
		}
	}
	sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: "quay-pull-secret"})
	_, err = sas.Update(ctx, sa, metav1.UpdateOptions{})
	return err
}

var rendezvousRe = regexp.MustCompile(`RENDEZVOUS_IP=(\S+)`)

// applyRendezvousFindings extracts the discovered rendezvous IP from the job
// logs, records it on status, and repairs the installer-state secret when it
// disagrees with the IP baked into the ISO (nodes[0].IP at ISO time).
func (s *StepExecutor) applyRendezvousFindings(ctx context.Context, nc *crd.NestedCluster, logs string, log interface {
	Info(msg string, keysAndValues ...interface{})
	Error(msg string, keysAndValues ...interface{})
}) {
	matches := rendezvousRe.FindAllStringSubmatch(logs, -1)
	if len(matches) == 0 {
		log.Info("configure-nodes job reported no rendezvous IP; waiting job will use ISO-configured node")
		return
	}
	found := matches[len(matches)-1][1]
	if found == "" || found == "none" {
		log.Info("rendezvous probe found no node serving :8090 before its deadline")
		return
	}
	for _, n := range nc.Status.Nodes {
		if n.IP == found {
			nc.Status.RendezvousNode = n.Name
		}
	}

	// The ISO bakes nodes[0].IP as rendezvousIP (see installer package).
	// Capture the ISO's belief BEFORE recording reality (an earlier version
	// read status after writing it, so the mismatch repair was dead code).
	configured := ""
	if len(nc.Status.Nodes) > 0 {
		configured = nc.Status.Nodes[0].IP
	}
	nc.Status.RendezvousIP = found
	if configured == "" || configured == found {
		log.Info("rendezvous matches ISO configuration", "ip", found)
		return
	}

	// Rewrite the installer state: wait-for bootstrap-complete polls
	// <rendezvousIP>:8090 from this state file. Replace ONLY the rendezvous
	// fields, never every occurrence of the old IP (it legitimately appears as
	// a host entry's own IP).
	secretName := nc.Spec.ClusterName + "-installer-state"
	state, err := s.client.GetSecretData(ctx, nc.Spec.Namespace, secretName, ".openshift_install_state.json")
	if err != nil {
		log.Error("rendezvous mismatch but installer state unreadable", "configured", configured, "actual", found, "error", err.Error())
		return
	}
	patched := state
	reState := regexp.MustCompile(`("RendezvousIP"\s*:\s*")` + regexp.QuoteMeta(configured) + `(")`)
	patched = reState.ReplaceAll(patched, []byte("${1}"+found+"${2}"))
	reAgent := regexp.MustCompile(`("rendezvous_ip"\s*:\s*")` + regexp.QuoteMeta(configured) + `(")`)
	patched = reAgent.ReplaceAll(patched, []byte("${1}"+found+"${2}"))
	if string(patched) == string(state) {
		log.Error("rendezvous mismatch but no state field matched for rewrite", "configured", configured, "actual", found)
		return
	}
	if err := s.client.UpdateSecretData(ctx, nc.Spec.Namespace, secretName, ".openshift_install_state.json", patched); err != nil {
		log.Error("failed to rewrite installer state rendezvousIP", "error", err.Error())
		return
	}
	log.Info("rewrote installer-state rendezvousIP to match reality", "from", configured, "to", found)
}

// configureNodesScript runs in the helper Job pod (inside the cluster
// namespace, attached to the cUDN — this is the only vantage point that can
// reach the VMs' overlay IPs).
const configureNodesScript = `#!/bin/bash
# One-shot per-cluster node bring-up: SSH into each nested VM to replace the
# bogus DHCP-provided resolver, restart the assisted agent so its runner
# re-reads resolv.conf, then discover the rendezvous node via :8090.
set -uo pipefail

if [ ! -f /sshkey/ssh-privatekey ]; then
    echo "FATAL: ssh private key not mounted at /sshkey/ssh-privatekey."
    echo "Set spec.sshPrivateKey on the NestedCluster CR (ConfigureNodes needs it)."
    exit 1
fi
install -m 600 /sshkey/ssh-privatekey /tmp/id_key

SSHOPT="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=6 -o LogLevel=ERROR -i /tmp/id_key"
configured=0

for ip in $NODE_IPS; do
    up=0
    for i in $(seq 1 18); do
        if ssh $SSHOPT "$SSH_USER@$ip" true 2>/dev/null; then up=1; break; fi
        sleep 10
    done
    if [ $up -ne 1 ]; then
        # Not fatal: a node stuck pre-sshd (e.g. ignition retry loop) is
        # still picked up by the node-guard Deployment, which re-runs the
        # same fix every 30s for the life of the install.
        echo "WARN $ip: ssh not up (3min) — leaving to node-guard"
        continue
    fi
    echo "OK $ip: ssh reachable"
    if ssh $SSHOPT "$SSH_USER@$ip" "bash -s -- $DNS_IP $API_IP $INGRESS_IP $CLUSTER_DOMAIN" < /scripts/fix-node.sh; then
        echo "OK $ip: dns configured"
        configured=$((configured+1))
    else
        echo "FAIL $ip: dns configuration"
    fi
done

# Rendezvous discovery: only node-zero serves the assisted-service on 8090.
rdv=""
for i in $(seq 1 90); do
    for ip in $NODE_IPS; do
        if (exec 3<>/dev/tcp/"$ip"/8090) 2>/dev/null; then
            exec 3>&- 3<&-
            rdv=$ip
            break
        fi
    done
    [ -n "$rdv" ] && break
    sleep 10
done
echo "RENDEZVOUS_IP=${rdv:-none}"

# Surface the install progress that is visible from the rendezvous node.
if [ -n "$rdv" ]; then
    curl -s --max-time 10 "http://$rdv:8090/api/assisted-install/v2/clusters" >/dev/null 2>&1 || true
fi

if [ $configured -eq 0 ]; then
    echo "configure-nodes: no node could be configured over ssh"
    exit 1
fi
echo "configure-nodes: $configured node(s) configured now; stragglers owned by node-guard"
`

// guardLoopScript runs in the node-guard Deployment pod (cluster namespace).
const guardLoopScript = `#!/bin/bash
set -uo pipefail
install -m 600 /sshkey/ssh-privatekey /tmp/id_key 2>/dev/null || { echo "no ssh key mounted; sleeping"; while true; do sleep 3600; done; }
SSHOPT="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=6 -o LogLevel=ERROR -i /tmp/id_key"
echo "node-guard started: nodes=$NODE_IPS dns=$DNS_IP api=$API_IP domain=$CLUSTER_DOMAIN"
while true; do
    for ip in $NODE_IPS; do
        out=$(ssh $SSHOPT "$SSH_USER@$ip" "bash -s -- $DNS_IP $API_IP $CLUSTER_DOMAIN" < /scripts/guard-node.sh 2>&1)
        rc=$?
        if [ $rc -ne 0 ]; then
            echo "$ip guard rc=$rc: $(echo "$out" | tail -3 | tr '\n' ';') 2>/dev/null"
        fi
        echo "$out" | grep -E '^(FIX|WARN)' | sed "s/^/$ip /"
    done
    sleep 60
done
`

// guardNodeScript runs ON each nested VM; args: DNS_IP API_IP DOMAIN.
// Deliberately cheap and idempotent — runs every 60s forever.
const guardNodeScript = `#!/bin/bash
set -uo pipefail
DNS_IP=$1; API_IP=$2; DOMAIN=$3

# 1. Resolver revert FIRST: pivot/MCO rewrites resolv.conf from DHCP (host
#    CoreDNS — unreachable on this UDN). Everything below resolves names.
if ! grep -q "nameserver $DNS_IP" /etc/resolv.conf 2>/dev/null; then
    DEV=$(ip route show default | awk '/default/ {print $5; exit}')
    CON=$(nmcli -g NAME,DEVICE connection show --active 2>/dev/null | awk -F: -v d="$DEV" '$2==d {print $1; exit}')
    if [ -n "$CON" ]; then
        if sudo nmcli connection modify "$CON" ipv4.ignore-auto-dns yes ipv4.dns "$DNS_IP" ipv4.dns-search "$DOMAIN" ipv4.dns-priority 10 >/dev/null 2>&1            && sudo nmcli connection up "$CON" >/dev/null 2>&1; then
            echo "FIX dns re-pinned via conn $CON"
        else
            echo "WARN dns re-pin failed via conn $CON"
        fi
    else
        echo "WARN no NM connection owns default dev $DEV yet"
    fi
    # Some RHCOS builds (especially the live agent ISO) do not honour
    # ipv4.dns on activation — the profile updates but resolv.conf keeps
    # the stale servers. Writing the file directly is sticky: NM leaves a
    # hand-managed resolv.conf alone on this image.
    if ! grep -q "nameserver $DNS_IP" /etc/resolv.conf 2>/dev/null; then
        printf 'search %s\nnameserver %s\n' "$DOMAIN" "$DNS_IP" | sudo tee /etc/resolv.conf >/dev/null
        echo "FIX resolv.conf written directly (nmcli ineffective)"
    fi
    # During install the agent's next-step-runner snapshots resolv.conf at
    # container creation; restart recreates it.
    systemctl cat agent.service >/dev/null 2>&1 && sudo systemctl restart agent.service
fi

# 2. Post-pivot kubeconfig bug: kubelet must reach the API through the
#    api-int NAME, never "localhost:6443" (on non-bootstrap masters that
#    points at nothing and the node silently never registers). The NAME
#    (not the HAProxy cUDN IP) is deliberate: haproxy pod rolls change the
#    cUDN lease, DNS absorbs that. Only once per change.
if [ -f /etc/kubernetes/kubeconfig ] && sudo grep -q "server: https://localhost:6443" /etc/kubernetes/kubeconfig 2>/dev/null; then
    sudo sed -i "s#server: https://localhost:6443#server: https://api-int.$DOMAIN:6443#" /etc/kubernetes/kubeconfig
    sudo systemctl restart kubelet
    echo "FIX kubeconfig server -> https://api-int.$DOMAIN:6443 (kubelet restarted)"
fi
exit 0
`

// fixNodeScript runs ON each nested VM (RHCOS, as core with sudo).
const fixNodeScript = `#!/bin/bash
# args: DNS_IP API_IP INGRESS_IP CLUSTER_DOMAIN
set -uo pipefail
DNS_IP=$1; API_IP=$2; INGRESS_IP=$3; DOMAIN=$4

CON=$(nmcli -g NAME,DEVICE connection show --active | awk -F: '$2=="enp1s0"{print $1}')
if [ -z "$CON" ]; then
    echo "FAIL: no NetworkManager connection on enp1s0"
    exit 1
fi

resolve_ok() {
    # api/api-int must hit the API VIP, wildcard apps must hit the ingress
    # VIP. (Do NOT add a "does the server answer at all" probe via getent:
    # getent exits rc=2 for a *correct* NXDOMAIN, which once failed this
    # gate for a perfectly healthy DNS server.)
    getent hosts "api.$DOMAIN" 2>/dev/null | grep -q "$API_IP" && \
    getent hosts "api-int.$DOMAIN" 2>/dev/null | grep -q "$API_IP" && \
    getent hosts "anything.apps.$DOMAIN" 2>/dev/null | grep -q "$INGRESS_IP"
}

ALREADY_OK=0
if grep -q "nameserver $DNS_IP" /etc/resolv.conf 2>/dev/null && resolve_ok; then
    ALREADY_OK=1
fi

# ALWAYS write the pin into the PERSISTED connection — even when
# resolv.conf already looks right (run 6 lesson: the old early-SKIP meant
# the pin never landed, and after the pivot the installed OS re-ran the
# ignition fetch against the DHCP resolver and deadlocked in initramfs).
# coreos-installer --copy-network carries .nmconnection files into the
# installed system, so what we write here is the second DNS layer after
# the installed-OS grub karg.
sudo nmcli connection modify "$CON" \
    ipv4.ignore-auto-dns yes \
    ipv4.dns "$DNS_IP" \
    ipv4.dns-search "$DOMAIN" \
    ipv4.dns-priority 10

# Evidence for the run record: the on-disk keyfile must carry the pin.
if grep -qs "dns=$DNS_IP" /etc/NetworkManager/system-connections/*.nmconnection; then
    echo "PERSIST: keyfile carries dns pin"
else
    echo "WARN PERSIST: no keyfile with the pin (NM kept it volatile?)"
fi

if [ "$ALREADY_OK" -eq 1 ]; then
    echo "OK: resolv.conf already correct; persisted pin ensured"
    exit 0
fi

sudo nmcli connection up "$CON" >/dev/null

# nmcli activation silently keeps stale servers on some RHCOS builds (live
# agent ISO): if resolv.conf didn't follow, write it directly. Sticky — NM
# leaves a hand-managed resolv.conf alone on this image.
if ! grep -q "nameserver $DNS_IP" /etc/resolv.conf 2>/dev/null; then
    printf 'search %s\nnameserver %s\n' "$DOMAIN" "$DNS_IP" | sudo tee /etc/resolv.conf >/dev/null
    echo "resolv.conf written directly (nmcli ineffective)"
fi

# next-step-runner caches /etc/resolv.conf for its whole life — restarting
# the agent recreates it with the new resolver. (Never 'podman kill
# next-step-runner': its exit code triggers a 1-hour agent backoff.)
sudo systemctl restart agent.service

for i in $(seq 1 36); do
    if resolve_ok; then echo "OK: resolution via $DNS_IP verified"; exit 0; fi
    sleep 5
    # runner may still hold the old resolv.conf if agent restart raced
    if [ "$i" -eq 18 ]; then sudo systemctl restart agent.service; fi
done

echo "FAIL: api.$DOMAIN never resolved to $API_IP via $DNS_IP"
getent hosts "api.$DOMAIN" || true
exit 1
`
