package k8s

import (
	"errors"
	"testing"
)

func TestStartIsIdempotent(t *testing.T) {
	for _, s := range []string{"Running", "Starting", "Provisioning", "WaitingForVolumeBinding", "Migrating"} {
		if !vmAlreadyStarted(s) {
			t.Errorf("%s must count as started (PowerOn is repeated after an orchestrator restart)", s)
		}
	}
	for _, s := range []string{"Stopped", "Stopping", "", "ErrorUnschedulable"} {
		if vmAlreadyStarted(s) {
			t.Errorf("%s must not count as started", s)
		}
	}
	err := errors.New(`starting VM cluster-master-2: Operation cannot be fulfilled on virtualmachine.kubevirt.io "cluster-master-2": VM is already running`)
	if !isAlreadyRunning(err) || isAlreadyRunning(errors.New("quota exceeded")) || isAlreadyRunning(nil) {
		t.Error("isAlreadyRunning must match exactly KubeVirt's already-running refusal")
	}
}
