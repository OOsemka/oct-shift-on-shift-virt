package k8s

import (
	"context"
	"encoding/json"
	"net"
	"sort"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ovnPrimaryIfAddr is OVN-Kubernetes' record of a node's primary interface
// address with its prefix, e.g. {"ipv4":"10.100.0.100/16"}.
const ovnPrimaryIfAddr = "k8s.ovn.org/node-primary-ifaddr"

// HostNodeAddresses lists the host cluster nodes' IPv4 InternalIP and
// ExternalIP addresses (read-only: nodes get/list), with the node network
// from the OVN-Kubernetes annotation when present.
func (c *Client) HostNodeAddresses(ctx context.Context) ([]crd.HostNodeAddress, error) {
	list, err := c.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	return NodeAddresses(list.Items), nil
}

// NodeAddresses extracts the IPv4 node addresses, sorted by node and address.
func NodeAddresses(nodes []corev1.Node) []crd.HostNodeAddress {
	out := []crd.HostNodeAddress{}
	for _, n := range nodes {
		subnet := ""
		var ifaddr struct {
			IPv4 string `json:"ipv4"`
		}
		if raw := n.Annotations[ovnPrimaryIfAddr]; raw != "" && json.Unmarshal([]byte(raw), &ifaddr) == nil {
			if _, sub, err := net.ParseCIDR(ifaddr.IPv4); err == nil && sub.IP.To4() != nil {
				subnet = sub.String()
			}
		}
		seen := map[string]bool{}
		for _, a := range n.Status.Addresses {
			if a.Type != corev1.NodeInternalIP && a.Type != corev1.NodeExternalIP {
				continue
			}
			ip := net.ParseIP(a.Address)
			if ip == nil || ip.To4() == nil || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			out = append(out, crd.HostNodeAddress{Node: n.Name, Address: ip.String(), Subnet: subnet})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Address < out[j].Address
	})
	return out
}
