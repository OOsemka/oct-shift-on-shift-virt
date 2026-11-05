package orchestrator

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"
)

func lbCluster() *crd.NestedCluster {
	nc := &crd.NestedCluster{}
	nc.Spec.ClusterName = "demo"
	nc.Spec.ExternalAccess = crd.ExternalAccessConfig{
		NADName: "default/vlan-a", APIIP: "192.0.2.10", IngressIP: "192.0.2.11", Gateway: "192.0.2.1", PrefixLength: 24,
	}
	nc.Status.Nodes = []crd.NodeStatus{
		{Name: "m0", Role: "master", IP: "10.100.0.100"},
		{Name: "m1", Role: "master", IP: "10.100.0.101"},
		{Name: "w0", Role: "worker", IP: "10.100.0.110"},
		{Name: "pending", Role: "worker"},
	}
	nc.Status.LoadBalancer = crd.LoadBalancerBastion
	return nc
}

func TestBastionLBConfig(t *testing.T) {
	lb, err := newBastionLB(lbCluster())
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Frontends map[string][][2]interface{} `json:"frontends"`
	}
	if err := json.Unmarshal([]byte(lb.configJSON()), &cfg); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, lb.configJSON())
	}
	if n := len(cfg.Frontends["6443"]); n != 2 {
		t.Errorf("6443 must front the 2 masters only, got %d", n)
	}
	if n := len(cfg.Frontends["443"]); n != 3 {
		t.Errorf("443 must front every node with an IP (3), got %d", n)
	}
	if b := cfg.Frontends["22623"]; len(b) != 2 || b[0][1].(float64) != 22623 {
		t.Errorf("22623 must go straight to the masters' 22623, got %v", b)
	}
	if lb.MAC != externalMAC("demo") || !strings.HasPrefix(lb.MAC, "02:") || externalMAC("other") == lb.MAC {
		t.Errorf("MAC must be stable, locally administered and per cluster: %s", lb.MAC)
	}
}

func TestBastionLBRequiresGateway(t *testing.T) {
	nc := lbCluster()
	nc.Spec.ExternalAccess.Gateway = ""
	if _, err := newBastionLB(nc); err == nil {
		t.Fatal("missing gateway must be an error")
	}
}

func TestBastionUserDataLoadBalancer(t *testing.T) {
	lb, _ := newBastionLB(lbCluster())
	ud := bastionUserData("demo-bastion", "", []string{"10.100.0.100"}, false, "", "", lb)
	for _, want := range []string{
		"/opt/sos-lb/lb.py", "/etc/sos-lb.json", "sos-lb.service", "sos-external.service",
		"MAC=" + lb.MAC, "API_IP=192.0.2.10", "INGRESS_IP=192.0.2.11", "GATEWAY=192.0.2.1", "PREFIX=24",
		"systemctl enable --now sos-external.service", "systemctl enable --now sos-lb.service",
		`ipv4.routes "0.0.0.0/0 $GATEWAY table=100"`, "from $API_IP/32 table 100",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("load balancer user data lacks %q", want)
		}
	}
	for _, want := range []string{"/etc/ssh/sshd_config.d/00-sos-keys-only.conf", "PasswordAuthentication no", "PermitRootLogin no", "systemctl reload sshd"} {
		if !strings.Contains(ud, want) {
			t.Errorf("user data must pin SSH to keys only; missing %q", want)
		}
	}
	if strings.Contains(ud, "mcs-relay.service") {
		t.Error("load balancer mode must not also start the old relay")
	}
	// Nothing environment-specific: every address in the user data comes from
	// the cluster's own settings (routing only follows the VLAN gateway).
	allowed := map[string]bool{"0.0.0.0": true, "127.0.0.1": true, "192.0.2.10": true, "192.0.2.11": true,
		"192.0.2.1": true, "10.100.0.100": true, "10.100.0.101": true, "10.100.0.110": true}
	for _, ip := range regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`).FindAllString(ud, -1) {
		if !allowed[ip] {
			t.Errorf("user data contains an address not taken from the cluster settings: %s", ip)
		}
	}
	legacy := bastionUserData("demo-bastion", "", []string{"10.100.0.100"}, false, "", "", nil)
	if !strings.Contains(legacy, "mcs-relay.service") || strings.Contains(legacy, "sos-lb") {
		t.Error("HAProxy-pod clusters must keep the relay and get no load balancer")
	}
}
