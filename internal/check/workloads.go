package check

import (
	"fmt"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/inventory"
)

// PDBBlockers flags budgets that currently allow zero disruptions, which stall node drains.
type PDBBlockers struct{}

func (PDBBlockers) ID() string { return "pdb-drain" }

func (PDBBlockers) Run(in Input) []Finding {
	var out []Finding
	for _, p := range in.Inventory.PDBs {
		if p.ExpectedPods == 0 || p.DisruptionsAllowed > 0 {
			continue
		}
		out = append(out, Finding{
			Severity: SeverityBlocker,
			Title:    "PodDisruptionBudget allows zero disruptions; node drains will stall",
			Resource: fmt.Sprintf("PodDisruptionBudget %s/%s", p.Namespace, p.Name),
			Detail: fmt.Sprintf("expected pods %d, healthy %d, desired healthy %d. "+
				"EKS managed node group updates fail with PodEvictionFailure; Karpenter and GKE/AKS surge upgrades wait on it.",
				p.ExpectedPods, p.CurrentHealthy, p.DesiredHealthy),
			Remediation: "Add replicas so healthy pods exceed minAvailable, fix unhealthy pods, or switch the budget to maxUnavailable: 1.",
		})
	}
	return out
}

// AddonCompat reports detected add-ons that lack verified compatibility data for the target.
type AddonCompat struct{}

func (AddonCompat) ID() string { return "addon-compat" }

func (AddonCompat) Run(in Input) []Finding {
	var out []Finding
	for _, a := range in.Inventory.Addons {
		meta, _ := in.KB.Addon(a.Name)
		if meta.VersionPolicy == compat.PolicyKubeletSkew {
			continue
		}
		fix := fmt.Sprintf("Confirm the %s release notes list support for Kubernetes %s.", a.Name, in.Target)
		if meta.EKSAddonName != "" && in.Inventory.Provider == inventory.ProviderEKS {
			fix = fmt.Sprintf("List compatible versions with `aws eks describe-addon-versions --addon-name %s --kubernetes-version <hop>` for each hop up to %s.",
				meta.EKSAddonName, in.Target)
		}
		out = append(out, Finding{
			Severity:    SeverityInfo,
			Title:       fmt.Sprintf("%s %s detected; verify compatibility with %s", a.Name, orUnknown(a.Version), in.Target),
			Resource:    fmt.Sprintf("%s %s/%s", a.Kind, a.Namespace, a.Workload),
			Detail:      "Jin has no verified compatibility matrix for this add-on yet.",
			Remediation: fix,
		})
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown version)"
	}
	return s
}
