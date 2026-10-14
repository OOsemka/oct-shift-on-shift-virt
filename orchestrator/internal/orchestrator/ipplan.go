package orchestrator

import (
	"fmt"
	"net"
)

// ipPlan is the deterministic cUDN address plan for one nested cluster.
//
// Why: every run so far let OVN-K DHCP assign master IPs at discovery boot
// and read them back from VMI status. That raced: run 6's agent-config baked
// master-0=.7 as rendezvousIP while leases drifted across boots (.7/.11
// swapped roles between discovery boot and agent-ISO boot), and infra VMs
// (DNS at the karg'd IP!) changed addresses on reschedule. The plan pins
// every VM we create through preseeded IPAMClaims (see k8s.PreseedIPAMClaim)
// BEFORE the first boot, and every consumer verifies VMI status against it.
//
// Offsets from the subnet base (/16 e.g. 10.101.0.0):
//
//	gateway        +1   (OVN-K first usable address — never claim)
//	mcs-relay      +12  (matches run-era observed .12; low offset on purpose)
//	dns-vm         +18  (baked as the nameserver= karg — must never drift)
//	jumpbox        +19
//	masters        +100, +101, +102, ... (master-0 == rendezvousIP)
//
// HAProxy pods (the api/ingress VIP holders) keep dynamic leases: their
// IPs live in DNS records + node-guard env, not in any baked artifact.
type ipPlan struct {
	NetworkName string // cluster_udn_<cluster>, matches the generated NAD
	Prefix      int    // subnet prefix length (mask bits)
	MCSRelay    net.IP
	DNSVM       net.IP
	Jumpbox     net.IP
	Masters     []net.IP
}

const (
	offsetMCSRelay = 12
	offsetDNSVM    = 18
	offsetJumpbox  = 19
	offsetMasters  = 100
)

func computeIPPlan(clusterName, cudnSubnet string, masterCount int, apiVIP, ingressVIP string) (*ipPlan, error) {
	_, ipNet, err := net.ParseCIDR(cudnSubnet)
	if err != nil {
		return nil, fmt.Errorf("parsing cudnSubnet %q: %w", cudnSubnet, err)
	}
	base := ipNet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("cudnSubnet %q: only IPv4 plans are supported", cudnSubnet)
	}
	ones, bits := ipNet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("cudnSubnet %q: not an IPv4 CIDR", cudnSubnet)
	}

	at := func(off int) net.IP {
		ip := make(net.IP, 4)
		copy(ip, base)
		n := uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
		n += uint32(off)
		ip[1] = byte(n >> 16)
		ip[2] = byte(n >> 8)
		ip[3] = byte(n)
		return ip
	}

	// Size guard: the reserved offsets must live inside the subnet.
	last := at(offsetMasters + masterCount - 1)
	if !ipNet.Contains(last) || !ipNet.Contains(at(offsetJumpbox)) {
		return nil, fmt.Errorf("cudnSubnet %q too small for the deterministic plan (needs base+%d)", cudnSubnet, offsetMasters+masterCount-1)
	}

	p := &ipPlan{
		NetworkName: "cluster_udn_" + clusterName,
		Prefix:      ones,
		MCSRelay:    at(offsetMCSRelay),
		DNSVM:       at(offsetDNSVM),
		Jumpbox:     at(offsetJumpbox),
	}
	for i := 0; i < masterCount; i++ {
		p.Masters = append(p.Masters, at(offsetMasters+i))
	}

	// Collisions with the gateway or the CR-declared VIPs are fatal: the
	// plan is the source of truth the moment it is computed.
	reservedByOthers := map[string]string{
		at(1).String(): "UDN gateway (first usable address)",
	}
	for k, v := range map[string]string{apiVIP: "spec.network.apiVIP", ingressVIP: "spec.network.ingressVIP"} {
		if v != "" {
			reservedByOthers[k] = v + " (spec.network)"
		}
	}
	wants := map[string]string{p.MCSRelay.String(): "mcs-relay", p.DNSVM.String(): "dns-vm", p.Jumpbox.String(): "jumpbox"}
	for i, m := range p.Masters {
		wants[m.String()] = fmt.Sprintf("master-%d", i)
	}
	for ip, who := range wants {
		if other, clash := reservedByOthers[ip]; clash {
			return nil, fmt.Errorf("ip plan: %s wants %s which is %s", who, ip, other)
		}
	}
	return p, nil
}

// dnsVMName/jumpboxVMName/relayVMName are the infra VM names (also the IPAM
// claim key — keep in lockstep with the deploy* functions).
func dnsVMName(cluster string) string     { return cluster + "-dns-vm" }
func jumpboxVMName(cluster string) string { return cluster + "-jumpbox" }
func relayVMName(cluster string) string   { return cluster + "-mcs-relay" }
