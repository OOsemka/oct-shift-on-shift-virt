package k8s

import (
	"context"
	"fmt"
	"os"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// supplementalRoleName carries every permission the orchestrator needs that
// the shipped base ClusterRole predates: KubeVirt power subresources (stop /
// start — ISO mount and eject are impossible without them; the octools base
// role lists them under kubevirt.io/create, but the KubeVirt API actually
// serves them as PUTs under subresources.kubevirt.io, RBAC verb update),
// pods/exec (best-effort haproxy reload), and the endpoint-exposure surface
// (services, endpoints, routes) used by PostInstall.
//
// The base role grants create on clusterroles/clusterrolebindings precisely
// so an operator can provision what it needs at runtime; we use that
// in-band instead of requiring an admin to hand-edit RBAC. The role is
// content-versioned (name carries a version suffix) so create-if-absent is
// sufficient even without update rights on clusterroles: bump the version
// constant when the rule set changes.
const supplementalRoleName = "sos-orchestrator-supplemental-v2"

func ensureSupplementalRBAC(ctx context.Context, c *Client) error {
	sa := os.Getenv("SOS_SERVICE_ACCOUNT")
	if sa == "" {
		sa = "sos-orchestrator"
	}
	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			return fmt.Errorf("cannot determine orchestrator namespace for supplemental RBAC: %w", err)
		}
		ns = strings.TrimSpace(string(b))
	}

	want := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: supplementalRoleName},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"subresources.kubevirt.io"},
				Resources: []string{"virtualmachines/start", "virtualmachines/stop", "virtualmachines/restart"},
				Verbs:     []string{"update"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/exec"},
				Verbs:     []string{"create"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"services", "endpoints"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				APIGroups: []string{"route.openshift.io"},
				Resources: []string{"routes"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch", "delete"},
			},
			{
				// Deterministic-IP plan: preseed persistent-IP claims on
				// the cluster UDN before VMs attach (v2).
				APIGroups: []string{"k8s.cni.cncf.io"},
				Resources: []string{"ipamclaims", "ipamclaims/status"},
				Verbs:     []string{"get", "list", "watch", "create", "update", "patch"},
			},
		},
	}

	if _, err := c.Clientset.RbacV1().ClusterRoles().Create(ctx, want, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating %s: %w", supplementalRoleName, err)
	}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: supplementalRoleName},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: supplementalRoleName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: sa, Namespace: ns}},
	}
	if _, err := c.Clientset.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating %s binding: %w", supplementalRoleName, err)
	}
	return nil
}

// EnsureSelfRBAC is the exported startup hook (best effort: the base role
// covers everything except ISO mount / exposure, and those degrade loudly in
// their own phase logs rather than crashing the operator).
func EnsureSelfRBAC(ctx context.Context, c *Client) error {
	return ensureSupplementalRBAC(ctx, c)
}
