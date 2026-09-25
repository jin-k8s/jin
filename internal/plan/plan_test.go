package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/provider"
	"github.com/jin-k8s/jin/internal/support"
)

var v = kube.MustParseVersion

func loadKB(t *testing.T) *compat.KB {
	t.Helper()
	kb, err := compat.Load()
	if err != nil {
		t.Fatal(err)
	}
	return kb
}

func findings(p *Plan, checkID string) []check.Finding {
	var out []check.Finding
	for _, h := range p.Hops {
		for _, f := range h.Findings {
			if f.CheckID == checkID {
				out = append(out, f)
			}
		}
	}
	return out
}

func TestBuildMultiHopEKS(t *testing.T) {
	inv := &inventory.Inventory{
		ServerVersion: v("1.24"),
		Provider:      inventory.ProviderEKS,
		Nodes: []inventory.Node{
			{Name: "a", Version: v("1.22"), KubeletVersion: "v1.22.17", Pool: "legacy", PoolType: inventory.PoolEKSManaged},
			{Name: "b", Version: v("1.24"), KubeletVersion: "v1.24.17", PoolType: inventory.PoolKarpenter, Pool: "general"},
		},
		Addons: []inventory.AddonInstance{
			{Name: "kube-proxy", Kind: "DaemonSet", Namespace: "kube-system", Workload: "kube-proxy", Version: "v1.24.10-eksbuild.2"},
			{Name: "coredns", Kind: "Deployment", Namespace: "kube-system", Workload: "coredns", Version: "v1.9.3-eksbuild.3"},
		},
		Manifests: []inventory.ManifestRef{
			{Source: inventory.SourceHelm, Origin: "helm release ops/backup (rev 3)", Release: "backup", ReleaseNamespace: "ops", APIVersion: "batch/v1beta1", Kind: "CronJob", Namespace: "ops", Name: "backup"},
			{Source: inventory.SourceLastApplied, APIVersion: "autoscaling/v2beta2", Kind: "HorizontalPodAutoscaler", Namespace: "shop", Name: "web"},
			{Source: inventory.SourceHelm, Release: "old", ReleaseNamespace: "ops", APIVersion: "extensions/v1beta1", Kind: "Ingress", Name: "old"},
			{Source: inventory.SourceHelm, APIVersion: "apps/v1", Kind: "Deployment", Name: "fine"},
		},
		PDBs: []inventory.PDB{
			{Namespace: "shop", Name: "web", DisruptionsAllowed: 0, ExpectedPods: 2},
			{Namespace: "shop", Name: "ok", DisruptionsAllowed: 1, ExpectedPods: 3},
			{Namespace: "shop", Name: "empty", DisruptionsAllowed: 0, ExpectedPods: 0},
		},
		DeprecatedAPIRequests: []inventory.DeprecatedAPIRequest{
			{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta2", Resource: "flowschemas", RemovedRelease: v("1.29")},
			{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Resource: "flowschemas", RemovedRelease: v("1.32")},
		},
	}

	p, err := Build(inv, loadKB(t), Options{Target: v("1.29")})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Hops) != 5 || p.Hops[0].From != v("1.24") || p.Hops[4].To != v("1.29") {
		t.Fatalf("hops: %+v", p.Hops)
	}
	if p.Ready {
		t.Fatal("plan with blockers must not be ready")
	}

	hopOf := func(fs []check.Finding, substr string) kube.Version {
		for _, f := range fs {
			if strings.Contains(f.Title, substr) || strings.Contains(f.Resource, substr) {
				return f.Hop
			}
		}
		t.Fatalf("no finding containing %q in %+v", substr, fs)
		return kube.Version{}
	}

	apis := findings(p, "removed-apis")
	if hopOf(apis, "CronJob") != v("1.25") || hopOf(apis, "HorizontalPodAutoscaler") != v("1.26") {
		t.Fatalf("removed-apis hops wrong: %+v", apis)
	}
	for _, f := range apis {
		if strings.Contains(f.Title, "Ingress") {
			if f.Severity != check.SeverityWarning || !strings.Contains(f.Remediation, "helm mapkubeapis old --namespace ops") {
				t.Fatalf("already-removed helm API should be a mapkubeapis warning: %+v", f)
			}
		}
		if strings.Contains(f.Resource, "fine") {
			t.Fatal("apps/v1 must not be flagged")
		}
	}

	calls := findings(p, "deprecated-api-calls")
	if len(calls) != 1 || calls[0].Hop != v("1.29") {
		t.Fatalf("deprecated calls: %+v", calls)
	}

	// Node at 1.22: n-2 at 1.24 is fine; 1.25 control plane would make it 3 behind, max 2.
	skew := findings(p, "version-skew")
	if hopOf(skew, "legacy") != v("1.25") {
		t.Fatalf("skew: %+v", skew)
	}
	if !strings.Contains(skew[0].Remediation, "managed node group") {
		t.Fatalf("remediation should be pool specific: %s", skew[0].Remediation)
	}
	// kube-proxy 1.24 is 3 behind at 1.27 (allowed: skew 2 before 1.28), so it blocks the 1.27 hop.
	if hopOf(skew, "kube-proxy") != v("1.27") {
		t.Fatalf("kube-proxy skew: %+v", skew)
	}

	if pdbs := findings(p, "pdb-drain"); len(pdbs) != 1 || !strings.Contains(pdbs[0].Resource, "shop/web") {
		t.Fatalf("pdb: %+v", pdbs)
	}
	if ac := findings(p, "addon-compat"); len(ac) != 1 || !strings.Contains(ac[0].Remediation, "describe-addon-versions --addon-name coredns") {
		t.Fatalf("addon-compat: %+v", ac)
	}

	first := p.Hops[0]
	if first.Steps[0].Phase != provider.PhasePrepare {
		t.Fatalf("hop with blockers must start with a prepare step: %+v", first.Steps)
	}
	for i := 1; i < len(first.Findings); i++ {
		if first.Findings[i].Severity.Rank() < first.Findings[i-1].Severity.Rank() {
			t.Fatal("findings not sorted by severity")
		}
	}
	var dp string
	for _, s := range first.Steps {
		if s.Phase == provider.PhaseDataPlane {
			dp = s.Detail
		}
	}
	if !strings.Contains(dp, "Karpenter") || !strings.Contains(dp, "Managed node groups") {
		t.Fatalf("EKS data-plane step should cover detected pool types: %s", dp)
	}
}

func TestDataPlaneActions(t *testing.T) {
	inv := &inventory.Inventory{
		ServerVersion: v("1.29"),
		Nodes:         []inventory.Node{{Version: v("1.29")}},
	}
	p, err := Build(inv, loadKB(t), Options{Target: v("1.33"), Checks: []check.Check{}})
	if err != nil {
		t.Fatal(err)
	}
	got := []provider.DataPlaneAction{}
	for _, h := range p.Hops {
		got = append(got, h.DataPlane)
	}
	want := []provider.DataPlaneAction{provider.DataPlaneOptional, provider.DataPlaneOptional, provider.DataPlaneRequired, provider.DataPlaneFinal}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if !p.Ready || !p.Complete {
		t.Fatal("no checks and no warnings: plan should be ready and complete")
	}
}

func TestKBCoverageWarnsBeyondVerifiedData(t *testing.T) {
	kb := loadKB(t)
	vt := kb.VerifiedThrough()
	inv := &inventory.Inventory{ServerVersion: vt}
	p, err := Build(inv, kb, Options{Target: vt.Next().Next()})
	if err != nil {
		t.Fatal(err)
	}
	if cov := findings(p, "kb-coverage"); len(cov) != 2 || cov[0].Hop != vt.Next() {
		t.Fatalf("kb-coverage: %+v", cov)
	}

	inv.ServerVersion = kube.Version{Major: vt.Major, Minor: vt.Minor - 2}
	p, err = Build(inv, kb, Options{Target: vt})
	if err != nil {
		t.Fatal(err)
	}
	if cov := findings(p, "kb-coverage"); len(cov) != 0 {
		t.Fatalf("no coverage warning expected within verified range: %+v", cov)
	}
}

func TestSupportWindowFindings(t *testing.T) {
	d := func(s string) *time.Time { x, _ := time.Parse("2006-01-02", s); return &x }
	now := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	cal := &support.Calendar{Provider: "eks", Versions: []support.Version{
		{Version: v("1.30"), Status: support.StatusExtended, EndOfStandard: d("2025-07-23"), EndOfExtended: d("2026-12-01")},
		{Version: v("1.31"), Status: support.StatusStandard, EndOfStandard: d("2026-11-26")},
		{Version: v("1.32"), Status: support.StatusStandard, EndOfStandard: d("2027-03-23")},
	}}
	inv := &inventory.Inventory{ServerVersion: v("1.30"), Provider: "eks"}
	p, err := Build(inv, loadKB(t), Options{Target: v("1.31"), Calendar: cal, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	fs := findings(p, "support-window")
	if len(fs) != 2 || !strings.Contains(fs[0].Title, "$4,380 per year") || !strings.Contains(fs[1].Title, "leaves standard support on 2026-11-26") {
		t.Fatalf("support findings: %+v", fs)
	}
	if p.Support == nil || p.Support.CurrentSurchargePerYearUSD != 4380 {
		t.Fatalf("plan support: %+v", p.Support)
	}

	inv.ServerVersion = v("1.31")
	p, _ = Build(inv, loadKB(t), Options{Target: v("1.32"), Calendar: cal, Now: now})
	fs = findings(p, "support-window")
	if len(fs) != 1 || !strings.Contains(fs[0].Title, "ends 2026-11-26 (in 63 days)") {
		t.Fatalf("approaching end of standard: %+v", fs)
	}
}

func TestBuildValidation(t *testing.T) {
	inv := &inventory.Inventory{ServerVersion: v("1.30"), Warnings: []string{"nodes: forbidden"}}
	if _, err := Build(inv, loadKB(t), Options{Target: v("1.30")}); err == nil {
		t.Fatal("same-version target must fail")
	}
	if _, err := Build(inv, loadKB(t), Options{Target: v("2.0")}); err == nil {
		t.Fatal("major upgrade must fail")
	}
	p, err := Build(inv, loadKB(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if p.Target != v("1.31") || p.Complete {
		t.Fatalf("default target / completeness wrong: %+v", p)
	}
}
