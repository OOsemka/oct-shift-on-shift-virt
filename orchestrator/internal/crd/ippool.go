package crd

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// IPPoolKind is a set of free addresses on one external VLAN (one NAD) that
// admins prepare so people creating clusters pick addresses from a list
// instead of guessing which ones are free. Usage is never stored: an address
// is in use while a NestedCluster on the same network references it, so
// deleting a cluster frees its addresses with nothing to clean up.
const IPPoolKind = "IPPool"

// IPPoolResource is the plural resource name.
const IPPoolResource = "ippools"

// MaxPoolAddresses caps how many addresses one pool may expand to.
const MaxPoolAddresses = 4096

type IPPoolSpec struct {
	// NADName is the <namespace>/<name> of the external VLAN's
	// NetworkAttachmentDefinition (same form as externalAccess.nadName).
	NADName string `json:"nadName"`
	// PrefixLength of the VLAN subnet (8-30).
	PrefixLength int `json:"prefixLength"`
	// Addresses: single IPv4 addresses or inclusive ranges "a.b.c.d-a.b.c.e".
	Addresses []string `json:"addresses"`
	// Gateway of the VLAN, shown to users; optional.
	Gateway     string `json:"gateway,omitempty"`
	Description string `json:"description,omitempty"`
}

type IPPool struct {
	Name      string     `json:"name"`
	Namespace string     `json:"namespace"`
	Spec      IPPoolSpec `json:"spec"`
}

func ipToU32(ip net.IP) uint32 {
	v := ip.To4()
	return uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
}

func u32ToIP(n uint32) net.IP {
	return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).To4()
}

// Expand returns the pool's addresses in order, without duplicates.
func (p IPPoolSpec) Expand() ([]string, error) {
	seen := map[uint32]bool{}
	var nums []uint32
	for _, raw := range p.Addresses {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		from, to := entry, entry
		if i := strings.Index(entry, "-"); i >= 0 {
			from, to = strings.TrimSpace(entry[:i]), strings.TrimSpace(entry[i+1:])
		}
		a, b := parseIPv4(from), parseIPv4(to)
		if a == nil || b == nil {
			return nil, fmt.Errorf("%q is not an IPv4 address or range a.b.c.d-a.b.c.e", entry)
		}
		lo, hi := ipToU32(a), ipToU32(b)
		if lo > hi {
			return nil, fmt.Errorf("range %q ends before it starts", entry)
		}
		if hi-lo >= MaxPoolAddresses || len(nums)+int(hi-lo)+1 > MaxPoolAddresses {
			return nil, fmt.Errorf("a pool may hold at most %d addresses", MaxPoolAddresses)
		}
		for n := lo; ; n++ {
			if !seen[n] {
				seen[n] = true
				nums = append(nums, n)
			}
			if n == hi {
				break
			}
		}
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	out := make([]string, len(nums))
	for i, n := range nums {
		out[i] = u32ToIP(n).String()
	}
	return out, nil
}

// Validate checks a pool before it is used: a NAD, a sane prefix, at least one
// address, every address (and the gateway) in one VLAN subnet and not its
// network or broadcast address, and the gateway not offered as an address.
func (p IPPoolSpec) Validate() error {
	var problems []string
	add := func(format string, a ...interface{}) { problems = append(problems, fmt.Sprintf(format, a...)) }
	nad := strings.TrimSpace(p.NADName)
	if nad == "" {
		add("network (NAD) is required")
	} else {
		for _, part := range strings.SplitN(nad, "/", 2) {
			if !dns1123.MatchString(part) {
				add("network %q is not a valid <namespace>/<name> reference", nad)
				break
			}
		}
	}
	prefixOK := p.PrefixLength >= 8 && p.PrefixLength <= 30
	if !prefixOK {
		add("prefix length %d must be between 8 and 30", p.PrefixLength)
	}
	addrs, err := p.Expand()
	if err != nil {
		add("%v", err)
	} else if len(addrs) == 0 {
		add("add at least one address or range")
	}
	if prefixOK && err == nil && len(addrs) > 0 {
		mask := net.CIDRMask(p.PrefixLength, 32)
		first := parseIPv4(addrs[0])
		subnet := &net.IPNet{IP: first.Mask(mask), Mask: mask}
		for _, a := range addrs {
			ip := parseIPv4(a)
			if !subnet.Contains(ip) {
				add("%s is not in the same /%d subnet as %s (%s)", a, p.PrefixLength, addrs[0], subnet)
				break
			}
			if ip.Equal(subnet.IP) || ip.Equal(broadcast(subnet)) {
				add("%s is the network or broadcast address of %s", a, subnet)
			}
		}
		if gw := strings.TrimSpace(p.Gateway); gw != "" {
			g := parseIPv4(gw)
			switch {
			case g == nil:
				add("gateway %q is not an IPv4 address", gw)
			case !subnet.Contains(g):
				add("gateway %s is outside the pool subnet %s", gw, subnet)
			default:
				for _, a := range addrs {
					if a == g.String() {
						add("the gateway %s must not be one of the pool's addresses", gw)
						break
					}
				}
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid IP pool: %s", strings.Join(problems, "; "))
}

// AddressUse is one external address held by a cluster.
type AddressUse struct {
	Cluster string `json:"cluster"`
	Role    string `json:"role"` // API, Ingress or Jumpbox
}

// ExternalAddressUse maps NAD -> address -> who uses it, from the clusters'
// external access settings. Clusters being deleted still hold their addresses.
func ExternalAddressUse(clusters []NestedClusterSpec) map[string]map[string]AddressUse {
	out := map[string]map[string]AddressUse{}
	for _, c := range clusters {
		nad := strings.TrimSpace(c.ExternalAccess.NADName)
		for _, u := range []struct{ role, ip string }{
			{"API", c.ExternalAccess.APIIP},
			{"Ingress", c.ExternalAccess.IngressIP},
			{"Jumpbox", c.ExternalAccess.JumpboxIP},
		} {
			ip := parseIPv4(u.ip)
			if ip == nil {
				continue
			}
			if out[nad] == nil {
				out[nad] = map[string]AddressUse{}
			}
			out[nad][ip.String()] = AddressUse{Cluster: c.ClusterName, Role: u.role}
		}
	}
	return out
}

// CheckAddressesFree refuses external addresses another cluster already uses
// on the same network (two people picking the same free address at once, or
// a typo). others must not include the cluster being created.
func (s *NestedClusterSpec) CheckAddressesFree(others []NestedClusterSpec) error {
	use := ExternalAddressUse(others)[strings.TrimSpace(s.ExternalAccess.NADName)]
	var problems []string
	for _, u := range []struct{ label, ip string }{
		{"external API IP", s.ExternalAccess.APIIP},
		{"external Ingress IP", s.ExternalAccess.IngressIP},
		{"jumpbox IP", s.ExternalAccess.JumpboxIP},
	} {
		ip := parseIPv4(u.ip)
		if ip == nil {
			continue
		}
		if who, taken := use[ip.String()]; taken {
			problems = append(problems, fmt.Sprintf("%s %s is already used by cluster %s (%s)", u.label, ip, who.Cluster, who.Role))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("address already in use: %s", strings.Join(problems, "; "))
}

// CheckPool verifies that a cluster which names an IP pool really uses that
// pool's network, prefix and addresses.
func (s *NestedClusterSpec) CheckPool(pool *IPPool) error {
	ea := s.ExternalAccess
	if pool == nil {
		return fmt.Errorf("IP pool %q does not exist", ea.IPPool)
	}
	var problems []string
	if strings.TrimSpace(ea.NADName) != strings.TrimSpace(pool.Spec.NADName) {
		problems = append(problems, fmt.Sprintf("network %q is not the pool's network %q", ea.NADName, pool.Spec.NADName))
	}
	if ea.Prefix() != pool.Spec.PrefixLength {
		problems = append(problems, fmt.Sprintf("prefix length %d is not the pool's /%d", ea.Prefix(), pool.Spec.PrefixLength))
	}
	if gw, pgw := strings.TrimSpace(ea.Gateway), strings.TrimSpace(pool.Spec.Gateway); gw != "" && pgw != "" && gw != pgw {
		problems = append(problems, fmt.Sprintf("gateway %s is not the pool's gateway %s", gw, pgw))
	}
	addrs, err := pool.Spec.Expand()
	if err != nil {
		return fmt.Errorf("IP pool %q is invalid: %w", pool.Name, err)
	}
	in := map[string]bool{}
	for _, a := range addrs {
		in[a] = true
	}
	for _, u := range []struct{ label, ip string }{
		{"external API IP", ea.APIIP},
		{"external Ingress IP", ea.IngressIP},
		{"jumpbox IP", ea.JumpboxIP},
	} {
		if ip := parseIPv4(u.ip); ip != nil && !in[ip.String()] {
			problems = append(problems, fmt.Sprintf("%s %s is not in pool %q", u.label, ip, pool.Name))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("does not match IP pool %q: %s", pool.Name, strings.Join(problems, "; "))
}
