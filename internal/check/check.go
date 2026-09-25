// Package check evaluates an inventory against an upgrade path and produces findings.
package check

import (
	"time"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/support"
)

type Severity string

const (
	SeverityBlocker Severity = "blocker"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

func (s Severity) Rank() int {
	switch s {
	case SeverityBlocker:
		return 0
	case SeverityWarning:
		return 1
	}
	return 2
}

type Finding struct {
	CheckID  string   `json:"checkId"`
	Severity Severity `json:"severity"`
	// Hop is the control-plane version that must not be reached before the finding is resolved.
	// Zero means "before starting".
	Hop         kube.Version `json:"hop"`
	Title       string       `json:"title"`
	Resource    string       `json:"resource,omitempty"`
	Detail      string       `json:"detail,omitempty"`
	Remediation string       `json:"remediation,omitempty"`
}

type Input struct {
	Inventory *inventory.Inventory
	KB        *compat.KB
	Current   kube.Version
	Target    kube.Version
	// Support is nil when no support calendar is available for the provider.
	Support *support.Assessment
	Now     time.Time
}

// Hops returns each control-plane version visited after Current, up to and including Target.
func (in Input) Hops() []kube.Version {
	var out []kube.Version
	for v := in.Current.Next(); !in.Target.Less(v); v = v.Next() {
		out = append(out, v)
	}
	return out
}

func (in Input) inPath(v kube.Version) bool {
	return in.Current.Less(v) && !in.Target.Less(v)
}

type Check interface {
	ID() string
	Run(in Input) []Finding
}

func Default() []Check {
	return []Check{KBCoverage{}, SupportWindow{}, RemovedAPIs{}, DeprecatedAPICalls{}, VersionSkew{}, PDBBlockers{}, AddonCompat{}}
}

// KBCoverage warns when hops go beyond the releases the knowledge base has been verified for.
type KBCoverage struct{}

func (KBCoverage) ID() string { return "kb-coverage" }

func (KBCoverage) Run(in Input) []Finding {
	vt := in.KB.VerifiedThrough()
	var out []Finding
	for _, hop := range in.Hops() {
		if !vt.Less(hop) {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityWarning,
			Hop:      hop,
			Title:    "API removal data is not verified for " + hop.String(),
			Detail: "Jin's knowledge base covers removals through " + vt.String() +
				"; the removed-API check cannot rule out removals in this release.",
			Remediation: "Review the upstream deprecation guide and the " + hop.String() + " release notes, or update Jin.",
		})
	}
	return out
}
