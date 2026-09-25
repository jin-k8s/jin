// Package plan turns an inventory into an ordered, hop-by-hop upgrade plan.
package plan

import (
	"fmt"
	"sort"
	"time"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/provider"
	"github.com/jin-k8s/jin/internal/support"
)

type Hop struct {
	From      kube.Version             `json:"from"`
	To        kube.Version             `json:"to"`
	DataPlane provider.DataPlaneAction `json:"dataPlane"`
	Findings  []check.Finding          `json:"findings"`
	Steps     []provider.Step          `json:"steps"`
}

type Summary struct {
	Blockers int `json:"blockers"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

type Plan struct {
	Provider string       `json:"provider"`
	Current  kube.Version `json:"current"`
	Target   kube.Version `json:"target"`
	Hops     []Hop        `json:"hops"`
	Summary  Summary      `json:"summary"`
	// Ready is true when no blockers were found.
	Ready bool `json:"ready"`
	// Complete is false when some inventory data could not be collected.
	Complete           bool     `json:"complete"`
	CollectionWarnings []string `json:"collectionWarnings,omitempty"`
	// Support is set when the provider publishes a support calendar (EKS).
	Support *support.Assessment `json:"support,omitempty"`
}

type Options struct {
	// Target defaults to the next minor version.
	Target kube.Version
	Checks []check.Check
	// Calendar enables support-window findings and cost reporting.
	Calendar *support.Calendar
	Now      time.Time
}

func Build(inv *inventory.Inventory, kb *compat.KB, opts Options) (*Plan, error) {
	current := inv.ServerVersion
	target := opts.Target
	if target.IsZero() {
		target = current.Next()
	}
	if target.Major != current.Major {
		return nil, fmt.Errorf("major-version upgrades (%s to %s) are not supported", current, target)
	}
	if !current.Less(target) {
		return nil, fmt.Errorf("target %s must be newer than the current control-plane version %s", target, current)
	}
	checks := opts.Checks
	if checks == nil {
		checks = check.Default()
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	p := &Plan{Provider: inv.Provider, Current: current, Target: target, Support: support.Assess(opts.Calendar, current, target, now)}
	hopIndex := map[kube.Version]int{}
	for from := current; from.Less(target); from = from.Next() {
		hopIndex[from.Next()] = len(p.Hops)
		p.Hops = append(p.Hops, Hop{From: from, To: from.Next()})
	}

	in := check.Input{Inventory: inv, KB: kb, Current: current, Target: target, Support: p.Support, Now: now}
	for _, c := range checks {
		for _, f := range c.Run(in) {
			f.CheckID = c.ID()
			i, ok := hopIndex[f.Hop]
			if !ok {
				i = 0
				f.Hop = p.Hops[0].To
			}
			p.Hops[i].Findings = append(p.Hops[i].Findings, f)
			switch f.Severity {
			case check.SeverityBlocker:
				p.Summary.Blockers++
			case check.SeverityWarning:
				p.Summary.Warnings++
			default:
				p.Summary.Info++
			}
		}
	}

	actions := dataPlaneActions(inv, p.Hops)
	prov := provider.For(inv.Provider)
	hc := provider.HopContext{PoolTypes: poolTypes(inv), Addons: addonNames(inv)}
	for i := range p.Hops {
		h := &p.Hops[i]
		sortFindings(h.Findings)
		h.DataPlane = actions[i]
		hc.From, hc.To, hc.DataPlane = h.From, h.To, h.DataPlane
		var steps []provider.Step
		if n := countBlockers(h.Findings); n > 0 {
			steps = append(steps, provider.Step{
				Phase: provider.PhasePrepare,
				Title: fmt.Sprintf("Resolve the %d blocker(s) listed for this hop", n),
			})
		}
		h.Steps = append(steps, prov.HopSteps(hc)...)
	}

	p.Ready = p.Summary.Blockers == 0
	p.Complete = len(inv.Warnings) == 0
	p.CollectionWarnings = inv.Warnings
	return p, nil
}

// dataPlaneActions decides per hop whether nodes must be rolled, assuming the oldest node is
// brought up to that hop's version whenever it is rolled. Skipping node rolls within the skew
// policy avoids disrupting workloads on every hop of a multi-hop upgrade.
func dataPlaneActions(inv *inventory.Inventory, hops []Hop) []provider.DataPlaneAction {
	oldest := inv.ServerVersion
	for _, n := range inv.Nodes {
		if !n.Version.IsZero() && n.Version.Less(oldest) {
			oldest = n.Version
		}
	}
	out := make([]provider.DataPlaneAction, len(hops))
	for i, h := range hops {
		if i == len(hops)-1 {
			out[i] = provider.DataPlaneFinal
			continue
		}
		next := hops[i+1].To
		if oldest.MinorsBehind(next) > kube.MaxKubeletSkew(next) {
			out[i] = provider.DataPlaneRequired
			oldest = h.To
		} else {
			out[i] = provider.DataPlaneOptional
		}
	}
	return out
}

func poolTypes(inv *inventory.Inventory) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range inv.Nodes {
		if !seen[n.PoolType] {
			seen[n.PoolType] = true
			out = append(out, n.PoolType)
		}
	}
	sort.Strings(out)
	return out
}

func addonNames(inv *inventory.Inventory) []string {
	var out []string
	for _, a := range inv.Addons {
		out = append(out, a.Name)
	}
	return out
}

func countBlockers(fs []check.Finding) int {
	n := 0
	for _, f := range fs {
		if f.Severity == check.SeverityBlocker {
			n++
		}
	}
	return n
}

func sortFindings(fs []check.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Severity.Rank() != fs[j].Severity.Rank() {
			return fs[i].Severity.Rank() < fs[j].Severity.Rank()
		}
		if fs[i].CheckID != fs[j].CheckID {
			return fs[i].CheckID < fs[j].CheckID
		}
		return fs[i].Resource < fs[j].Resource
	})
}
