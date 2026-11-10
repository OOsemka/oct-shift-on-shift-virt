package crd

import "testing"

func TestApplyMaster(t *testing.T) {
	m := &NestedCluster{Status: NestedClusterStatus{Phase: PhaseMaster, Master: &MasterStatus{Snapshots: map[string]string{"a": "a-golden"}}}}
	m.Name = "m1"
	m.Spec = NestedClusterSpec{ClusterName: "m1", BaseDomain: "lab.test", OCPVersion: "4.22", StorageClassName: "rbd",
		Masters: NodeSizing{Count: 3, CPUCores: 8, MemoryGi: 32, DiskGi: 120}, Network: NetworkConfig{CUDNSubnet: "10.100.0.0/16"}}
	spec := NestedClusterSpec{ClusterName: "c1", BaseDomain: "other.test", Namespace: "c1-ns", Storage: StorageConfig{Enabled: true},
		ExternalAccess: ExternalAccessConfig{APIIP: "192.0.2.12", IngressIP: "192.0.2.13"}}
	if err := ApplyMaster(&spec, m); err != nil {
		t.Fatal(err)
	}
	nc := &NestedCluster{Spec: spec}
	if !nc.IsClone() || nc.FQDN() != "m1.lab.test" {
		t.Errorf("clone FQDN must be the master's internal one: %s", nc.FQDN())
	}
	if spec.Network != m.Spec.Network || spec.Masters != m.Spec.Masters || spec.StorageClassName != "rbd" || spec.OCPVersion != "4.22" {
		t.Error("internal layout must come from the master")
	}
	if spec.ClusterName != "c1" || spec.Namespace != "c1-ns" || spec.ExternalAccess.APIIP != "192.0.2.12" {
		t.Error("name, namespace and external addresses stay the clone's")
	}
	if !spec.ExternalAccess.HostRoutes || !spec.Storage.Enabled {
		t.Error("clones use host routes and keep their nested storage choice")
	}
	order := PhasesFor(nc)
	idx := map[ClusterPhase]int{}
	for i, p := range order {
		idx[p] = i
	}
	if idx[PhaseConfigureStorage] <= idx[PhaseCloneCreds] || idx[PhaseConfigureVirtualization] <= idx[PhaseConfigureStorage] {
		t.Error("clones configure storage and virtualization after they have their own credentials")
	}
	if PhasesFor(nc)[3] != PhaseCloneVMs || PhasesFor(&NestedCluster{})[3] != PhaseCreateVMs {
		t.Error("phase order")
	}
	notMaster := &NestedCluster{}
	if ApplyMaster(&spec, notMaster) == nil {
		t.Error("a non-master must be refused")
	}
}

func TestMasterRefusal(t *testing.T) {
	ok := &NestedCluster{Status: NestedClusterStatus{LoadBalancer: LoadBalancerBastion}}
	if ok.MasterRefusal() != "" {
		t.Error("plain bastion-LB cluster must be allowed")
	}
	st := *ok
	st.Spec.Storage.Enabled = true
	if st.MasterRefusal() == "" {
		t.Error("nested storage must be refused for now")
	}
	clone := *ok
	clone.Spec.Source = &ClusterSource{Master: "m"}
	if clone.MasterRefusal() == "" {
		t.Error("a clone cannot become a master")
	}
}
