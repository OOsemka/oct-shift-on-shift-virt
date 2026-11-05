package orchestrator

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func testHAProxyDeployment(gateway string) *unstructured.Unstructured {
	s := &StepExecutor{}
	return s.buildHAProxyDeployment("c-haproxy-api", "c-ns", "c-haproxy", "c-cfg", `[{"name":"vlan","ips":["192.0.2.10/24"]}]`,
		map[string]interface{}{"app": "x"}, nil, "sha", "192.0.2.10", gateway)
}

func routeInit(t *testing.T, dep *unstructured.Unstructured) map[string]interface{} {
	t.Helper()
	inits, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
	for _, c := range inits {
		if m := c.(map[string]interface{}); m["name"] == haproxyRouteInitName {
			return m
		}
	}
	return nil
}

func TestHAProxyDeploymentWithoutGatewayIsUnchanged(t *testing.T) {
	dep := testHAProxyDeployment("")
	if _, found, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers"); found {
		t.Fatal("no gateway must add no init container (pods stay on plain anyuid)")
	}
	if _, found, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", haproxyVLANGatewayAnnotation); found {
		t.Fatal("no gateway must add no annotation")
	}
}

func TestHAProxyDeploymentWithGateway(t *testing.T) {
	dep := testHAProxyDeployment("192.0.2.1")
	c := routeInit(t, dep)
	if c == nil {
		t.Fatal("gateway set: want the return-route init container")
	}
	if got := haproxyInitEnv(dep, "VLAN_IP"); got != "192.0.2.10" {
		t.Errorf("VLAN_IP = %q", got)
	}
	if got := haproxyInitEnv(dep, "VLAN_GATEWAY"); got != "192.0.2.1" {
		t.Errorf("VLAN_GATEWAY = %q", got)
	}
	add, _, _ := unstructured.NestedStringSlice(c, "securityContext", "capabilities", "add")
	if len(add) != 1 || add[0] != "NET_ADMIN" {
		t.Errorf("init container must add exactly NET_ADMIN, got %v", add)
	}
	if priv, found, _ := unstructured.NestedBool(c, "securityContext", "privileged"); found && priv {
		t.Error("init container must not be privileged")
	}
	script := c["command"].([]interface{})[2].(string)
	for _, want := range []string{"ip rule add from \"$VLAN_IP\" lookup 100", "ip route replace default via \"$VLAN_GATEWAY\" dev \"$dev\" table 100"} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not contain %q", want)
		}
	}
	if strings.Contains(script, "ip route replace default via \"$VLAN_GATEWAY\" dev \"$dev\"\n") {
		t.Error("the main table default route must never be touched")
	}
	if got, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", haproxyVLANGatewayAnnotation); got != "192.0.2.1" {
		t.Errorf("annotation = %q", got)
	}
}

func TestSetHAProxyVLANRouteTransitions(t *testing.T) {
	dep := testHAProxyDeployment("")
	if setHAProxyVLANRoute(dep, haproxyImage, "192.0.2.10", "") {
		t.Fatal("no gateway -> no gateway must be a no-op")
	}
	if !setHAProxyVLANRoute(dep, haproxyImage, "192.0.2.10", "192.0.2.1") || routeInit(t, dep) == nil {
		t.Fatal("adding a gateway must change the template and add the init container")
	}
	if setHAProxyVLANRoute(dep, haproxyImage, "192.0.2.10", "192.0.2.1") {
		t.Fatal("same gateway again must be a no-op (no roll on every event)")
	}
	if !setHAProxyVLANRoute(dep, haproxyImage, "192.0.2.10", "192.0.2.254") || haproxyInitEnv(dep, "VLAN_GATEWAY") != "192.0.2.254" {
		t.Fatal("changing the gateway must update the init container")
	}
	inits, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers")
	if len(inits) != 1 {
		t.Fatalf("changing the gateway must not duplicate the init container, got %d", len(inits))
	}
	if !setHAProxyVLANRoute(dep, haproxyImage, "192.0.2.10", "") {
		t.Fatal("removing the gateway must change the template")
	}
	if _, found, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "initContainers"); found {
		t.Fatal("removing the gateway must remove the init container")
	}
	if _, found, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", haproxyVLANGatewayAnnotation); found {
		t.Fatal("removing the gateway must remove the annotation")
	}
	if got, _, _ := unstructured.NestedString(dep.Object, "spec", "template", "metadata", "annotations", "sos.octools.io/config-sha"); got != "sha" {
		t.Fatalf("other annotations must survive, config-sha = %q", got)
	}
}
