package crd

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

var dns1123 = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateNetwork checks the network and external access settings of a new
// cluster before anything is created. Every problem is reported at once, in
// words a person filling in the wizard understands. Mistakes it stops (all
// seen in real use): a blank external IP, which made the HAProxy
// pod annotation "ips":["/24"] and left the pod in ContainerCreating forever;
// the internal VIPs typed in as external IPs; internal VIPs outside the
// cluster network.
func (s *NestedClusterSpec) ValidateNetwork() error {
	var problems []string
	add := func(format string, a ...interface{}) { problems = append(problems, fmt.Sprintf(format, a...)) }

	n := s.Network
	_, cudn, err := net.ParseCIDR(strings.TrimSpace(n.CUDNSubnet))
	if err != nil || cudn.IP.To4() == nil {
		add("internal network subnet %q is not an IPv4 CIDR such as 10.100.0.0/16", n.CUDNSubnet)
		cudn = nil
	}
	if cudn != nil {
		// Masters sit at base+100..; anything smaller than a /24 cannot hold the plan.
		if ones, _ := cudn.Mask.Size(); ones > 24 {
			add("internal network %s must be a /24 or larger (for example /16)", cudn)
		}
		for _, c := range []struct{ label, value string }{
			{"cluster (pod) network", n.ClusterNetworkCIDR},
			{"service network", n.ServiceNetworkCIDR},
		} {
			if _, o, err := net.ParseCIDR(strings.TrimSpace(c.value)); err == nil && (o.Contains(cudn.IP) || cudn.Contains(o.IP)) {
				add("internal network %s overlaps the %s %s", cudn, c.label, o)
			}
		}
	}
	internal := map[string]string{}
	for _, v := range []struct{ field, label, value string }{
		{"network.apiVIP", "internal API VIP", n.APIVIP},
		{"network.ingressVIP", "internal Ingress VIP", n.IngressVIP},
	} {
		ip := parseIPv4(v.value)
		switch {
		case ip == nil:
			add("%s (%s) %q is not an IPv4 address", v.label, v.field, v.value)
		case cudn != nil && !cudn.Contains(ip):
			add("%s %s is outside the internal network %s", v.label, ip, cudn)
		default:
			internal[ip.String()] = v.label
		}
	}
	if a, b := strings.TrimSpace(n.APIVIP), strings.TrimSpace(n.IngressVIP); a != "" && a == b {
		add("internal API VIP and Ingress VIP are both %s; they must differ", a)
	}

	ea := s.ExternalAccess
	nad := strings.TrimSpace(ea.NADName)
	if nad == "" {
		add("external access network (NAD) is not selected")
	} else {
		for _, part := range strings.SplitN(nad, "/", 2) {
			if !dns1123.MatchString(part) {
				add("external access network %q is not a valid <namespace>/<name> reference", nad)
				break
			}
		}
	}

	prefix := ea.Prefix()
	if prefix < 8 || prefix > 30 {
		add("external VLAN prefix length %d must be between 8 and 30", prefix)
		prefix = 0
	}

	var others []*net.IPNet
	for _, c := range []string{n.ClusterNetworkCIDR, n.ServiceNetworkCIDR} {
		if _, ipn, err := net.ParseCIDR(strings.TrimSpace(c)); err == nil {
			others = append(others, ipn)
		}
	}

	external := map[string]string{}
	var vlan *net.IPNet
	checkExternal := func(field, label, value string, required bool) {
		v := strings.TrimSpace(value)
		if v == "" {
			if required {
				add("%s (%s) is empty: type a free address on the external VLAN; it is not assigned automatically", label, field)
			}
			return
		}
		ip := parseIPv4(v)
		if ip == nil {
			add("%s %q is not an IPv4 address", label, v)
			return
		}
		if who, ok := internal[ip.String()]; ok {
			add("%s %s is the %s: external addresses must be free addresses on the external VLAN, not the internal VIPs", label, ip, who)
		} else if cudn != nil && cudn.Contains(ip) {
			add("%s %s is inside the internal network %s: use a free address on the external VLAN", label, ip, cudn)
		}
		for _, o := range others {
			if o.Contains(ip) {
				add("%s %s is inside the nested cluster's pod or service network %s", label, ip, o)
			}
		}
		if who, ok := external[ip.String()]; ok {
			add("%s and %s are both %s; each needs its own address", who, label, ip)
		}
		external[ip.String()] = label
		if prefix > 0 {
			sub := &net.IPNet{IP: ip.Mask(net.CIDRMask(prefix, 32)), Mask: net.CIDRMask(prefix, 32)}
			if ip.Equal(sub.IP) || ip.Equal(broadcast(sub)) {
				add("%s %s is the network or broadcast address of %s", label, ip, sub)
			}
			if vlan == nil {
				vlan = sub
			} else if !vlan.Contains(ip) {
				add("%s %s is not in the same /%d VLAN subnet as the other external addresses (%s)", label, ip, prefix, vlan)
			}
		}
	}
	checkExternal("externalAccess.apiIP", "external API IP", ea.APIIP, true)
	checkExternal("externalAccess.ingressIP", "external Ingress IP", ea.IngressIP, true)
	checkExternal("externalAccess.jumpboxIP", "jumpbox IP", ea.JumpboxIP, false)

	// The gateway lets the HAProxy pods answer clients outside the VLAN.
	switch gw := strings.TrimSpace(ea.Gateway); {
	case gw == "":
		add("external VLAN gateway (externalAccess.gateway) is empty: without it the cluster answers only clients on the VLAN itself")
	case parseIPv4(gw) == nil:
		add("external VLAN gateway %q is not an IPv4 address", gw)
	default:
		g := parseIPv4(gw)
		if who, ok := external[g.String()]; ok {
			add("external VLAN gateway %s is also the %s", g, who)
		} else if vlan != nil && !vlan.Contains(g) {
			add("external VLAN gateway %s is outside the VLAN subnet %s", g, vlan)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid network settings: %s", strings.Join(problems, "; "))
}

func parseIPv4(s string) net.IP {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return nil
	}
	return ip.To4()
}

func broadcast(n *net.IPNet) net.IP {
	ip := make(net.IP, 4)
	for i := range ip {
		ip[i] = n.IP[i] | ^n.Mask[i]
	}
	return ip
}
