package orchestrator

import (
	"context"
	"fmt"
	"strings"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// VLAN return route for the HAProxy pods.
//
// The HAProxy pods live in a primary-UDN namespace, so their default route
// points into the UDN (ovn-udn1). A client on a routed subnet reaches the
// external VLAN address on net1, but the reply follows that default route,
// leaves through the UDN with the node's address and is dropped: the
// external IPs only work for clients on the VLAN subnet itself.
//
// Measured on OCP 4.22, neither CNI-side fix works:
//   - "default-route" in the network-selection annotation fails pod creation
//     ("failed to add gateway route to link 'net1': file exists"), because the
//     primary UDN already owns the default route.
//   - A localnet cUDN with IPAM disabled carries no routes of its own.
//
// So when spec.externalAccess.gateway is set, an init container installs
// source-based policy routing in the pod's own network namespace:
//
//	ip rule add from <vlanIP> lookup 100
//	ip route replace default via <gateway> dev <vlan iface> table 100
//
// Everything sent FROM the VLAN address (replies to VLAN clients) goes back
// out of the VLAN; backend traffic (sourced from the UDN address) and the
// UDN default route are untouched. It needs NET_ADMIN in the pod's network
// namespace only, granted by the dedicated SCC oct-sos-haproxy-netadmin
// (anyuid + NET_ADMIN, nothing else), bound per cluster to the HAProxy SA
// and removed at teardown. Without a gateway nothing changes.
const (
	// haproxyVLANGatewayAnnotation records on the pod template which gateway
	// the return route uses ("" or absent = none). A difference from the spec
	// makes the Deployment roll.
	haproxyVLANGatewayAnnotation = "sos.octools.io/vlan-gateway"
	haproxyRouteInitName         = "vlan-return-route"
	// haproxyNetAdminClusterRole grants "use" on the SCC oct-sos-haproxy-netadmin
	// (deploy/install.yaml).
	haproxyNetAdminClusterRole   = "oct-sos-haproxy-netadmin-scc"
	haproxyNetAdminBindingPrefix = "sos-haproxy-netadmin-"
	haproxyReturnRouteTable      = "100"
)

// vlanReturnRouteScript finds the interface holding the VLAN address (Multus
// names it net1 today, but nothing depends on that) and routes everything
// sourced from that address via the VLAN gateway. Re-runnable: a retried init
// container in the same sandbox finds the rule already there.
func vlanReturnRouteScript() string {
	return `dev=$(ip -o -4 addr show | awk -v a="$VLAN_IP" '{split($4, p, "/"); if (p[1] == a) {print $2; exit}}')
if [ -z "$dev" ]; then echo "no interface holds $VLAN_IP" >&2; ip -o -4 addr show >&2; exit 1; fi
while ip rule del from "$VLAN_IP" lookup ` + haproxyReturnRouteTable + ` 2>/dev/null; do :; done
ip rule add from "$VLAN_IP" lookup ` + haproxyReturnRouteTable + ` priority ` + haproxyReturnRouteTable + `
ip route replace default via "$VLAN_GATEWAY" dev "$dev" table ` + haproxyReturnRouteTable + `
echo "replies from $VLAN_IP go via $VLAN_GATEWAY on $dev"
ip rule show
ip route show table ` + haproxyReturnRouteTable + `
`
}

// haproxyRouteInitContainer is the init container that installs the return route.
func haproxyRouteInitContainer(image, vlanIP, gateway string) map[string]interface{} {
	return map[string]interface{}{
		"name":    haproxyRouteInitName,
		"image":   image,
		"command": []interface{}{"/bin/sh", "-ec", vlanReturnRouteScript()},
		"env": []interface{}{
			map[string]interface{}{"name": "VLAN_IP", "value": vlanIP},
			map[string]interface{}{"name": "VLAN_GATEWAY", "value": gateway},
		},
		"securityContext": map[string]interface{}{
			"capabilities": map[string]interface{}{
				"add":  []interface{}{"NET_ADMIN"},
				"drop": []interface{}{"MKNOD"},
			},
		},
	}
}

// setHAProxyVLANRoute makes a HAProxy Deployment's pod template match the
// wanted return route: with a gateway, the init container and the annotation;
// without one, neither. It reports whether the template changed.
func setHAProxyVLANRoute(dep *unstructured.Unstructured, image, vlanIP, gateway string) bool {
	gateway = strings.TrimSpace(gateway)
	have, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", haproxyVLANGatewayAnnotation)
	inits, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
	var kept []interface{}
	hadInit := false
	for _, c := range inits {
		if m, ok := c.(map[string]interface{}); ok && m["name"] == haproxyRouteInitName {
			hadInit = true
			continue
		}
		kept = append(kept, c)
	}
	if have == gateway && hadInit == (gateway != "") {
		return false
	}
	if gateway != "" {
		kept = append(kept, haproxyRouteInitContainer(image, vlanIP, gateway))
		_ = unstructured.SetNestedField(dep.Object, gateway, "spec", "template", "metadata", "annotations", haproxyVLANGatewayAnnotation)
	} else {
		unstructured.RemoveNestedField(dep.Object, "spec", "template", "metadata", "annotations", haproxyVLANGatewayAnnotation)
	}
	if len(kept) == 0 {
		unstructured.RemoveNestedField(dep.Object, "spec", "template", "spec", "initContainers")
	} else {
		_ = unstructured.SetNestedSlice(dep.Object, kept, "spec", "template", "spec", "initContainers")
	}
	return true
}

// ensureHAProxyNetAdmin grants the HAProxy SA the NET_ADMIN SCC while a
// gateway is set, and removes the grant when it is not.
func (s *StepExecutor) ensureHAProxyNetAdmin(ctx context.Context, nc *crd.NestedCluster) error {
	name := haproxyNetAdminBindingPrefix + nc.Spec.ClusterName
	if strings.TrimSpace(nc.Spec.ExternalAccess.Gateway) == "" {
		return s.client.DeleteClusterRoleBinding(ctx, name)
	}
	labels := map[string]string{"oct-shift-on-shift-virt/cluster": nc.Spec.ClusterName}
	if err := s.client.CreateClusterRoleBinding(ctx, name, haproxyNetAdminClusterRole, nc.Spec.ClusterName+"-haproxy", nc.Spec.Namespace, labels); err != nil {
		return fmt.Errorf("granting the HAProxy NET_ADMIN SCC: %w", err)
	}
	return nil
}

// SyncHAProxyVLANRoute brings the return route of an installed (Ready)
// cluster's HAProxy pods in line with spec.externalAccess.gateway. Phases do
// not run on Ready clusters, and re-running them from DeployHAProxy would
// rebuild the agent ISO, so this touches only the two HAProxy Deployments
// (rolling them when the gateway changed) and the SCC binding. It does
// nothing when the Deployments already match, so it is cheap to call on
// every event.
func (s *StepExecutor) SyncHAProxyVLANRoute(ctx context.Context, nc *crd.NestedCluster) error {
	deployGVR := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	res := s.client.DynamicClient.Resource(deployGVR).Namespace(nc.Spec.Namespace)
	gateway := strings.TrimSpace(nc.Spec.ExternalAccess.Gateway)
	bindingDone, rolled := false, false
	for _, d := range []struct{ name, vlanIP string }{
		{nc.Spec.ClusterName + "-haproxy-api", nc.Spec.ExternalAccess.APIIP},
		{nc.Spec.ClusterName + "-haproxy-ingress", nc.Spec.ExternalAccess.IngressIP},
	} {
		dep, err := res.Get(ctx, d.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !setHAProxyVLANRoute(dep, haproxyImage, strings.TrimSpace(d.vlanIP), gateway) {
			continue
		}
		// Grant before the pods roll, so the new pods are admitted.
		if !bindingDone && gateway != "" {
			if err := s.ensureHAProxyNetAdmin(ctx, nc); err != nil {
				return err
			}
			bindingDone = true
		}
		if _, err := res.Update(ctx, dep, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating %s: %w", d.name, err)
		}
		rolled = true
		s.logger.WithCluster(nc.Spec.ClusterName).WithComponent("haproxy").Info("haproxy VLAN return route changed; rolling pods",
			"deployment", d.name, "gateway", gateway)
	}
	if gateway == "" && rolled {
		// Removed after the pods no longer ask for NET_ADMIN.
		return s.ensureHAProxyNetAdmin(ctx, nc)
	}
	return nil
}

// haproxyInitEnv reads an env value of the return-route init container of a
// built Deployment ("" when it has none).
func haproxyInitEnv(dep *unstructured.Unstructured, name string) string {
	inits, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
	for _, c := range inits {
		m, ok := c.(map[string]interface{})
		if !ok || m["name"] != haproxyRouteInitName {
			continue
		}
		env, _ := m["env"].([]interface{})
		for _, e := range env {
			if em, ok := e.(map[string]interface{}); ok && em["name"] == name {
				v, _ := em["value"].(string)
				return v
			}
		}
	}
	return ""
}
