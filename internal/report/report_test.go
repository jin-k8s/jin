package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/provider"
	"github.com/jin-k8s/jin/internal/runrecord"
)

func sample() *runrecord.Record {
	r := runrecord.New(runrecord.KindPlanRun, time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC))
	r.Cluster.Context = "prod"
	r.Plan = &plan.Plan{
		Provider: "eks",
		Current:  kube.MustParseVersion("1.30"),
		Target:   kube.MustParseVersion("1.31"),
		Summary:  plan.Summary{Blockers: 1},
		Hops: []plan.Hop{{
			From: kube.MustParseVersion("1.30"), To: kube.MustParseVersion("1.31"),
			Findings: []check.Finding{{CheckID: "pdb-drain", Severity: check.SeverityBlocker, Title: "PDB | blocks", Resource: "PodDisruptionBudget a/b", Remediation: "relax it"}},
			Steps:    []provider.Step{{Phase: provider.PhaseControlPlane, Title: "Upgrade"}},
		}},
		CollectionWarnings: []string{"metrics: forbidden"},
	}
	r.Finish(r.StartedAt, nil)
	return r
}

func TestRenderFormats(t *testing.T) {
	r := sample()
	for _, f := range Formats() {
		var buf bytes.Buffer
		if err := Render(&buf, r, f); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out := buf.String()
		switch f {
		case FormatJSON:
			var back runrecord.Record
			if err := json.Unmarshal(buf.Bytes(), &back); err != nil || back.Plan.Summary.Blockers != 1 {
				t.Fatalf("json round-trip: %v", err)
			}
		case FormatMarkdown:
			if !strings.Contains(out, `PDB \| blocks`) || !strings.Contains(out, "**BLOCKER**") {
				t.Fatalf("markdown:\n%s", out)
			}
		case FormatTable:
			if strings.Contains(out, "\x1b[") {
				t.Fatal("non-TTY output must not contain ANSI escapes")
			}
			for _, want := range []string{"BLOCKED", "✗ BLOCKER", "→ relax it", "metrics: forbidden", "1.30 → 1.31", "HOP 1", "1 blocker"} {
				if !strings.Contains(out, want) {
					t.Fatalf("table missing %q:\n%s", want, out)
				}
			}
		}
	}
	if err := Render(&bytes.Buffer{}, r, "yaml"); err == nil {
		t.Fatal("unknown format should fail")
	}
}

func TestRenderFailedRun(t *testing.T) {
	r := runrecord.New(runrecord.KindPlanRun, time.Now())
	r.Finish(time.Now(), errTest("connection refused"))
	var buf bytes.Buffer
	if err := Render(&buf, r, FormatTable); err != nil || !strings.Contains(buf.String(), "FAILED") || !strings.Contains(buf.String(), "connection refused") {
		t.Fatalf("got %q, %v", buf.String(), err)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
