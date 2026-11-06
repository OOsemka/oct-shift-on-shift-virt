package k8s

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeAddresses(t *testing.T) {
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "b", Annotations: map[string]string{ovnPrimaryIfAddr: `{"ipv4":"10.100.0.101/16"}`}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.100.0.101"},
				{Type: corev1.NodeHostName, Address: "b"},
				{Type: corev1.NodeInternalIP, Address: "fd00::1"},
			}},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "a", Annotations: map[string]string{ovnPrimaryIfAddr: "not json"}},
			Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeExternalIP, Address: "198.51.100.7"},
				{Type: corev1.NodeInternalIP, Address: "192.0.2.5"},
				{Type: corev1.NodeInternalIP, Address: "192.0.2.5"},
			}},
		},
	}
	got := NodeAddresses(nodes)
	want := []struct{ node, addr, subnet string }{
		{"a", "192.0.2.5", ""},
		{"a", "198.51.100.7", ""},
		{"b", "10.100.0.101", "10.100.0.0/16"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if got[i].Node != w.node || got[i].Address != w.addr || got[i].Subnet != w.subnet {
			t.Errorf("[%d] got %+v, want %+v", i, got[i], w)
		}
	}
}
