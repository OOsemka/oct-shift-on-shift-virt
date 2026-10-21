package orchestrator

import (
	"regexp"
	"strings"
	"testing"

	"github.com/OOsemka/oct-shift-on-shift-virt/orchestrator/internal/crd"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestStorageQuotaGi(t *testing.T) {
	const gi = int64(1) << 30
	cases := []struct {
		existing int64
		allow    int
		virt     bool
		want     int64
	}{
		{0, 0, false, defaultStorageAllowanceGi},
		{360 * gi, 50, false, 410},
		{360*gi + 1, 50, false, 411}, // partial Gi rounds up
		{360 * gi, 50, true, 410 + virtualizationBootSourcesGi},
	}
	for _, c := range cases {
		if got := storageQuotaGi(c.existing, c.allow, c.virt); got != c.want {
			t.Errorf("storageQuotaGi(%d, %d, %v) = %d, want %d", c.existing, c.allow, c.virt, got, c.want)
		}
	}
}

func TestEnforcementYAML(t *testing.T) {
	var with, without map[string]interface{}
	if err := yaml.Unmarshal([]byte(enforcementYAML("rbd", "rbd-snap")), &with); err != nil {
		t.Fatal(err)
	}
	if with["allowAll"] != false || with["allowDefault"] != false {
		t.Fatalf("enforcement must be explicit deny-by-default: %v", with)
	}
	m := with["storageSnapshotMapping"].([]interface{})[0].(map[string]interface{})
	if m["volumeSnapshotClasses"].([]interface{})[0] != "rbd-snap" || m["storageClasses"].([]interface{})[0] != "rbd" {
		t.Fatalf("bad snapshot mapping: %v", m)
	}
	if err := yaml.Unmarshal([]byte(enforcementYAML("rbd", "")), &without); err != nil {
		t.Fatal(err)
	}
	if _, ok := without["storageSnapshotMapping"]; ok {
		t.Fatal("no snapshot class must mean no mapping")
	}
}

func TestParseImageLines(t *testing.T) {
	d := strings.Repeat("a", 64)
	logs := "OK nested API reachable\nIMAGE IMG_DRIVER=quay.io/x/y@sha256:" + d + "\n" +
		"IMAGE IMG_BAD=quay.io/x/y:latest\n+ echo IMAGE IMG_TRACE=quay.io/x@sha256:" + d + "\n"
	got := parseImageLines(logs)
	if len(got) != 1 || got["IMG_DRIVER"] != "quay.io/x/y@sha256:"+d {
		t.Fatalf("got %v", got)
	}
}

func TestRenderControllerTemplate(t *testing.T) {
	vars := map[string]string{"HOST_NS": "c-ns", "CLUSTER": "c"}
	for _, k := range []string{"IMG_DRIVER", "IMG_PROVISIONER", "IMG_ATTACHER", "IMG_RESIZER", "IMG_SNAPSHOTTER", "IMG_LIVENESS"} {
		vars[k] = "quay.io/x/" + k + "@sha256:" + strings.Repeat("b", 64)
	}
	out, err := renderTemplate(mustReadCSI("controller.yaml.tmpl"), vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "$(CSI_ENDPOINT)") {
		t.Fatal("Kubernetes $(VAR) expansion must survive rendering")
	}
	var dep appsv1.Deployment
	if err := yaml.UnmarshalStrict([]byte(out), &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Namespace != "c-ns" || len(dep.Spec.Template.Spec.Containers) != 6 {
		t.Fatalf("unexpected deployment: ns=%s containers=%d", dep.Namespace, len(dep.Spec.Template.Spec.Containers))
	}
	delete(vars, "IMG_RESIZER")
	if _, err := renderTemplate(mustReadCSI("controller.yaml.tmpl"), vars); err == nil {
		t.Fatal("a missing image must fail rendering")
	}
}

// Every ${VAR} the nested templates use must be one the job script exports.
func TestNestedTemplatePlaceholdersAreProvided(t *testing.T) {
	script := mustReadCSI("configure-storage.sh")
	for _, f := range []string{"nested.yaml.tmpl", "nested-snapclass.yaml.tmpl"} {
		for _, p := range placeholderRe.FindAllString(mustReadCSI(f), -1) {
			v := p[2 : len(p)-1]
			exported := strings.HasPrefix(v, "IMG_") && regexp.MustCompile(`\b`+v+`=[a-z]`).MatchString(script)
			if !exported && v != "INFRA_SC" && v != "INFRA_VSC" {
				t.Errorf("%s uses %s but configure-storage.sh never sets it", f, p)
			}
		}
	}
}

func TestVMMap(t *testing.T) {
	nc := &crd.NestedCluster{}
	nc.Status.Nodes = []crd.NodeStatus{{Name: "c-master-0", IP: "10.0.0.100"}, {Name: "c-master-1"}}
	if got := vmMap(nc); got != "10.0.0.100=c-master-0" {
		t.Fatalf("got %q", got)
	}
}

func TestLastFatal(t *testing.T) {
	if got := lastFatal("OK a\nFATAL: first\nFATAL: second\n"); got != "second" {
		t.Fatalf("got %q", got)
	}
}
