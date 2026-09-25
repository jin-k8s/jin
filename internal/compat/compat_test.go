package compat

import (
	"testing"

	"github.com/jin-k8s/jin/internal/kube"
)

func TestLoad(t *testing.T) {
	kb, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r, ok := kb.Removal("batch/v1beta1", "CronJob")
	if !ok || r.RemovedIn != kube.MustParseVersion("1.25") || r.Replacement != "batch/v1" {
		t.Fatalf("CronJob removal = %+v, %v", r, ok)
	}
	if _, ok := kb.Removal("apps/v1", "Deployment"); ok {
		t.Fatal("apps/v1 Deployment must not be a removal")
	}
	if !kb.RemovedKinds()["Ingress"] {
		t.Fatal("Ingress should be a removed kind")
	}
	rs := kb.Removals()
	for i := 1; i < len(rs); i++ {
		if rs[i].RemovedIn.Less(rs[i-1].RemovedIn) {
			t.Fatal("Removals not sorted by release")
		}
	}
	a, ok := kb.Addon("kube-proxy")
	if !ok || a.VersionPolicy != PolicyKubeletSkew {
		t.Fatalf("kube-proxy addon = %+v", a)
	}
	for _, a := range kb.Addons() {
		if len(a.Workloads) == 0 {
			t.Errorf("addon %s has no workload matchers", a.Name)
		}
	}
}
