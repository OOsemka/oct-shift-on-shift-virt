package orchestrator

import (
	"context"
	"fmt"
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

	vmName := clusterName + "-jumpbox"
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
		UserData:         jumpboxUserData(vmName, strings.TrimSpace(string(pubKey))),
	}); err != nil {
		return fmt.Errorf("creating jumpbox VM: %w", err)
	}

	log.Info("waiting for jumpbox VM to boot", "vm", vmName)
	ip, err := s.client.WaitForVMIReady(ctx, clusterNs, vmName, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("jumpbox VM never got an IP: %w", err)
	}

	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"sos.octools.io/jumpbox":"ssh core@%s"}}}`, ip))
	_, err = s.client.DynamicClient.Resource(nestedClusterGVR).Namespace(nc.Namespace).
		Patch(ctx, nc.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		log.Info("failed to annotate CR with jumpbox address", "error", err.Error())
	}
	log.Info("jumpbox ready", "ip", ip, "ssh", "core@"+ip)
	return nil
}

// jumpboxUserData builds a minimal cloud-config: ONE named user with the
// cluster key and wheel sudo, no services. The explicit users: block is the
// point — unlike the DNS VM (which needs no shell), the jumpbox exists to be
// logged into, so it carries credentials deliberately.
func jumpboxUserData(hostname, pubKey string) string {
	authorized := ""
	if pubKey != "" {
		authorized = fmt.Sprintf("users:\n  - name: core\n    ssh_authorized_keys:\n      - %s\n    sudo: ['ALL=(ALL) NOPASSWD:ALL']\n    groups: [wheel]\n", pubKey)
	}
	return fmt.Sprintf(`#cloud-config
hostname: %s
%sruncmd:
  - hostnamectl set-hostname %s
`, hostname, authorized, hostname)
}
