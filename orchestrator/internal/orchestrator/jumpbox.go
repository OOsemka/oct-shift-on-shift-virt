package orchestrator

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// jumpboxEnabled reports whether the troubleshooting jumpbox VM is deployed.
// It is ON by default; set JUMPBOX=off to suppress. Rationale: every deep
// debugging session (DNS contradictions, wire captures, MCS probes) required
// an interactive SSH box inside the machine network, hand-built mid-incident.
// A 1c/2Gi VM provisioned with the cluster costs nothing and turns "build a
// jumpbox" from an incident step into a precondition.
func jumpboxEnabled() bool { return os.Getenv("JUMPBOX") != "off" }

// deployJumpbox provisions <cluster>-jumpbox (cloud-init: core user + cluster
// ssh key, nothing else running) on the same pod-network pattern as the DNS
// VM (kubevirt bridges the guest onto the launcher's primary network, which
// in a cluster-UDN namespace IS the machine network), and records its
// address as an annotation on the NestedCluster CR so operators find it via
// `oc get nestedcluster <name> -o yaml` without reading orchestrator logs.
//
// Inbound reachability (catch #C): the cUDN address is only reachable from
// inside the machine network (UDN isolation blocks pods/hosts outside it).
// When spec.externalAccess carries jumpboxIP + nadName, the VM gets a SECOND
// interface on the external NAD (the same vlan2111 localnet the HAProxy VIP
// pods use — the workstation-proven inbound path) with a static address, so
// operators can `ssh core@<jumpboxIP>` directly from the outside and hop
// into every cUDN guest. Best-effort: an unreachable second NIC never
// blocks the install.
//
// Best-effort by contract: callers log failures and continue — a jumpbox
// must never block an install.
func (s *StepExecutor) deployJumpbox(ctx context.Context, nc *crd.NestedCluster) error {
	log := s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("jumpbox")
	clusterNs := nc.Spec.Namespace
	clusterName := nc.Spec.ClusterName

	pubKey, err := s.client.GetSecretData(ctx, clusterNs, clusterName+"-ssh-key", "ssh-publickey")
	if err != nil {
		return fmt.Errorf("jumpbox needs the cluster ssh public key (%s-ssh-key): %w", clusterName, err)
	}

	plan, err := computeIPPlan(clusterName, nc.Spec.Network.CUDNSubnet, nc.Spec.Masters.Count,
		nc.Spec.Network.APIVIP, nc.Spec.Network.IngressVIP)
	if err != nil {
		return fmt.Errorf("computing IP plan: %w", err)
	}

	// Optional external (vlan) second interface.
	extIP := strings.TrimSpace(nc.Spec.ExternalAccess.JumpboxIP)
	nad := strings.TrimSpace(nc.Spec.ExternalAccess.NADName)
	if i := strings.LastIndex(nad, "/"); i >= 0 {
		nad = nad[i+1:] // CR carries "default/vlan2111"; DeployHAProxy CopyNAD'd it into the cluster ns under the bare name
	}
	var extra []k8s.InfraNetwork
	extUserData := ""
	cudnPrefix := "10.101."
	if base, _, perr := net.ParseCIDR(nc.Spec.Network.CUDNSubnet); perr == nil {
		if v4 := base.To4(); v4 != nil {
			cudnPrefix = fmt.Sprintf("%d.%d.", v4[0], v4[1])
		}
	}
	if extIP != "" && nad != "" {
		extra = []k8s.InfraNetwork{{Name: "ext0", NetworkName: nad}}
		extUserData = jumpboxExtNetworkScript(cudnPrefix, extIP+"/24")
	}

	vmName := jumpboxVMName(clusterName)
	if err := s.client.CreateInfraVM(ctx, k8s.InfraVMParams{
		Name:             vmName,
		Namespace:        clusterNs,
		ClusterName:      clusterName,
		Cores:            1,
		MemoryGi:         2,
		DiskGi:           32, // clone target must be >= DataSource size
		StorageClassName: nc.Spec.StorageClassName,
		DataSourceNS:     "openshift-virtualization-os-images",
		DataSourceName:   dnsBaseImage(),
		UserData:         jumpboxUserData(vmName, strings.TrimSpace(string(pubKey)), extUserData),
		ExtraNetworks:    extra,
	}); err != nil {
		return fmt.Errorf("creating jumpbox VM: %w", err)
	}

	log.Info("waiting for jumpbox VM to boot", "vm", vmName)
	ip, err := s.client.WaitForVMIReady(ctx, clusterNs, vmName, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("jumpbox VM never got an IP: %w", err)
	}
	if plan.Jumpbox.String() != ip {
		log.Error("jumpbox cUDN IP drifted from the reserved allocation (diagnostics annotation reflects the ACTUAL ip)",
			"reserved", plan.Jumpbox.String(), "actual", ip)
	}

	annos := fmt.Sprintf(`{"metadata":{"annotations":{"sos.octools.io/jumpbox":"ssh core@%s","sos.octools.io/jumpbox-access":"oc -n %s run diag --rm -it --restart=Never --image=%s --command -- bash  # in-ns pods reach every cUDN IP",`,
		ip, clusterNs, sshHelperImage())
	if extIP != "" {
		annos += fmt.Sprintf(`"sos.octools.io/jumpbox-ext":"ssh core@%s (from the external vlan)",`, extIP)
	}
	annos += "}}"
	_, err = s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
		Patch(ctx, nc.Name, types.MergePatchType, []byte(annos), metav1.PatchOptions{})
	if err != nil {
		log.Info("failed to annotate CR with jumpbox address", "error", err.Error())
	}
	log.Info("jumpbox ready", "ip", ip, "ssh", "core@"+ip, "external", extIP)
	return nil
}

// jumpboxUserData builds a minimal cloud-config: ONE named user with the
// cluster key and wheel sudo, no services. The explicit users: block is the
// point — unlike the DNS VM (which needs no shell), the jumpbox exists to be
// logged into, so it carries credentials deliberately. extUserData (may be
// empty) is appended as one more runcmd entry configuring the external NIC.
func jumpboxUserData(hostname, pubKey, extUserData string) string {
	authorized := ""
	if pubKey != "" {
		authorized = fmt.Sprintf("users:\n  - name: core\n    ssh_authorized_keys:\n      - %s\n    sudo: ['ALL=(ALL) NOPASSWD:ALL']\n    groups: [wheel]\n", pubKey)
	}
	ext := ""
	if extUserData != "" {
		ext = "  - bash /tmp/sos-extnet.sh\n"
	}
	extraFile := ""
	if extUserData != "" {
		extraFile = fmt.Sprintf("write_files:\n  - path: /tmp/sos-extnet.sh\n    permissions: '0755'\n    content: |\n%s", indentBlock(extUserData, "      "))
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
%s%sruncmd:
  - hostnamectl set-hostname %s
%s
`, hostname, authorized, extraFile, hostname, ext)
}

// jumpboxExtNetworkScript waits for the multus-attached second interface (no
// DHCP server on the vlan NAD, so it arrives unaddressed), then pins a
// static address on it with NO default route — the cUDN interface keeps the
// default gateway and only the external /24 rides the vlan.
func jumpboxExtNetworkScript(cudnPrefix, extCIDR string) string {
	return fmt.Sprintf(`#!/bin/bash
# sos-extnet: configure the external diagnostics NIC (static %s, no gateway).
set -u
CUDN_PREFIX=%q
EXT_CIDR=%q
DEV=""
for i in $(seq 1 90); do
  CUDNDEV=$(ip -o -4 addr show 2>/dev/null | awk -v p="$CUDN_PREFIX" '$4 ~ "^"p {print $2; exit}')
  if [ -n "$CUDNDEV" ]; then
    for d in $(ls /sys/class/net | grep -vE '^(lo|te[0-9]|br-|ovs|ovs-if)'); do
      [ "$d" = "$CUDNDEV" ] && continue
      ip -4 addr show dev "$d" 2>/dev/null | grep -q "inet " && continue
      DEV="$d"; break
    done
  fi
  [ -n "$DEV" ] && break
  sleep 2
done
if [ -z "$DEV" ]; then echo "sos-extnet: no unaddressed second NIC found; continuing"; exit 0; fi
nmcli connection add type ethernet con-name sos-ext ifname "$DEV" ipv4.method manual ipv4.addresses "$EXT_CIDR" ipv4.route-metric 700 >/dev/null 2>&1 || \
  nmcli connection modify sos-ext ipv4.method manual ipv4.addresses "$EXT_CIDR" ipv4.route-metric 700
nmcli connection up sos-ext >/dev/null 2>&1 || true
echo "sos-extnet: configured $DEV = $EXT_CIDR"
`, extCIDR, cudnPrefix, extCIDR)
}
