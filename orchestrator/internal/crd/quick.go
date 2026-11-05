package crd

import (
	"fmt"
	"regexp"
	"strings"
)

var clusterNameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

// ValidateClusterName mirrors the wizard's rule.
func ValidateClusterName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("cluster name is required")
	case len(name) > 63:
		return fmt.Errorf("cluster name must be 63 characters or fewer")
	case !clusterNameRe.MatchString(name) || strings.Contains(name, "--"):
		return fmt.Errorf("cluster name must start with a letter and use only lowercase letters, digits and single hyphens")
	}
	return nil
}

// SuggestQuickName returns quick-<n> with the lowest n not already taken.
func SuggestQuickName(existing []string) string {
	taken := map[string]bool{}
	for _, n := range existing {
		taken[n] = true
	}
	for i := 1; ; i++ {
		if n := fmt.Sprintf("quick-%d", i); !taken[n] {
			return n
		}
	}
}

// BuildQuickSpec builds a compact 3-node cluster from the lab defaults and the
// default IP pool's free addresses. Credentials are left empty on purpose:
// the orchestrator takes them from the lab defaults Secret, so the pull secret
// never travels through the browser or lands in the cluster object.
// It returns every reason the quick cluster cannot be created.
func BuildQuickSpec(name string, s LabSettings, hasPullSecret bool, pool *IPPool, free []string) (NestedClusterSpec, []string) {
	s = s.WithDefaults()
	var problems []string
	if err := ValidateClusterName(name); err != nil {
		problems = append(problems, err.Error())
	}
	if !hasPullSecret {
		problems = append(problems, "the lab defaults have no pull secret")
	}
	// Same minimums as the NestedCluster CRD (masters).
	if s.MasterCores < 4 || s.MasterMemoryGi < 16 || s.MasterDiskGi < 120 {
		problems = append(problems, fmt.Sprintf("compact node size %d vCPU / %d GiB / %d GiB is below the minimum 4 vCPU / 16 GiB / 120 GiB",
			s.MasterCores, s.MasterMemoryGi, s.MasterDiskGi))
	}
	if strings.TrimSpace(s.BaseDomain) == "" {
		problems = append(problems, "the lab defaults have no base domain")
	}
	if strings.TrimSpace(s.OCPVersion) == "" {
		problems = append(problems, "the lab defaults have no OpenShift version")
	}
	if s.IPPool == "" {
		problems = append(problems, "the lab defaults have no IP pool")
	} else if pool == nil {
		problems = append(problems, fmt.Sprintf("IP pool %q does not exist", s.IPPool))
	} else if err := pool.Spec.Validate(); err != nil {
		problems = append(problems, fmt.Sprintf("IP pool %q is invalid: %v", s.IPPool, err))
	} else if len(free) < 2 {
		problems = append(problems, fmt.Sprintf("IP pool %q has %d free address(es); a cluster needs 2", s.IPPool, len(free)))
	}

	spec := NestedClusterSpec{
		ClusterName: name,
		BaseDomain:  strings.TrimSpace(s.BaseDomain),
		Namespace:   name + "-ns",
		OCPVersion:  strings.TrimSpace(s.OCPVersion),
		Topology:    "compact",
		Masters:     NodeSizing{Count: 3, CPUCores: s.MasterCores, MemoryGi: s.MasterMemoryGi, DiskGi: s.MasterDiskGi},
		// The CRD requires sizes even for zero-count pools; same values as the wizard.
		Workers: NodeSizing{Count: 0, CPUCores: 4, MemoryGi: 16, DiskGi: 120},
		Infra:   NodeSizing{Count: 0, CPUCores: 4, MemoryGi: 16, DiskGi: 120},
		NodeNames: []NodeNameEntry{
			{Name: "cluster-master-0", Role: "master"},
			{Name: "cluster-master-1", Role: "master"},
			{Name: "cluster-master-2", Role: "master"},
		},
		Network: NetworkConfig{
			CUDNSubnet: "10.100.0.0/16", APIVIP: "10.100.0.10", IngressVIP: "10.100.0.11",
			ClusterNetworkCIDR: "10.128.0.0/14", ServiceNetworkCIDR: "172.30.0.0/16",
		},
		StorageClassName: strings.TrimSpace(s.StorageClassName),
		Bastion:          BastionConfig{Username: "core"},
	}
	if pool != nil {
		spec.ExternalAccess = ExternalAccessConfig{
			NADName:      pool.Spec.NADName,
			PrefixLength: pool.Spec.PrefixLength,
			Gateway:      pool.Spec.Gateway,
			IPPool:       pool.Name,
		}
		if len(free) >= 2 {
			spec.ExternalAccess.APIIP, spec.ExternalAccess.IngressIP = free[0], free[1]
		}
	}
	return spec, problems
}

// FreePoolAddresses lists the pool's addresses no cluster uses on its network.
func FreePoolAddresses(pool *IPPool, clusters []NestedClusterSpec) []string {
	if pool == nil {
		return nil
	}
	addrs, err := pool.Spec.Expand()
	if err != nil {
		return nil
	}
	used := ExternalAddressUse(clusters)[pool.Spec.NADName]
	var free []string
	for _, a := range addrs {
		if _, taken := used[a]; !taken {
			free = append(free, a)
		}
	}
	return free
}
