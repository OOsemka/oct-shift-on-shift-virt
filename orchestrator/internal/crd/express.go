package crd

import "fmt"

// ApplyMaster turns a planned spec into a clone of master: the nodes'
// internal layout (network, sizing, storage class, version) must be the
// master's; the name, namespace, external addresses, credentials and the
// nested storage/virtualization choice stay this cluster's. Host routes are always on: a clone keeps the master's
// internal DNS name, so external DNS records cannot tell clones apart.
func ApplyMaster(spec *NestedClusterSpec, master *NestedCluster) error {
	if master.Status.Phase != PhaseMaster || master.Status.Master == nil {
		return fmt.Errorf("%q is not a master", master.Name)
	}
	m := master.Spec
	spec.Source = &ClusterSource{Master: master.Name, InternalName: m.ClusterName, InternalBaseDomain: m.BaseDomain}
	spec.Network = m.Network
	spec.Topology = m.Topology
	spec.Masters = m.Masters
	spec.Workers = m.Workers
	spec.Infra = m.Infra
	spec.NodeNames = m.NodeNames
	spec.OCPVersion = m.OCPVersion
	spec.StorageClassName = m.StorageClassName
	// Nested storage and virtualization are not part of the master: the clone
	// adds them after it is ready (ConfigureStorage/ConfigureVirtualization in
	// ClonePhaseOrder), so spec.storage/virtualization stay as requested.
	spec.ExternalAccess.HostRoutes = true
	return nil
}
