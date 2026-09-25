// Package provider describes how each Kubernetes offering executes an upgrade hop.
package provider

import (
	"fmt"
	"slices"
	"strings"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
)

const (
	PhasePrepare      = "prepare"
	PhaseControlPlane = "control-plane"
	PhaseAddons       = "add-ons"
	PhaseDataPlane    = "data-plane"
	PhaseVerify       = "verify"
)

type Step struct {
	Phase  string `json:"phase"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

type DataPlaneAction string

const (
	// DataPlaneRequired: nodes would exceed the skew limit at the next hop.
	DataPlaneRequired DataPlaneAction = "required"
	// DataPlaneOptional: nodes can stay put for this hop and still be within skew at the next.
	DataPlaneOptional DataPlaneAction = "optional"
	// DataPlaneFinal: last hop; bring nodes to the target version.
	DataPlaneFinal DataPlaneAction = "final"
)

type HopContext struct {
	From, To  kube.Version
	DataPlane DataPlaneAction
	PoolTypes []string
	Addons    []string
}

func (h HopContext) hasPool(t string) bool { return slices.Contains(h.PoolTypes, t) }

type Provider interface {
	Name() string
	HopSteps(h HopContext) []Step
}

func For(name string) Provider {
	switch name {
	case inventory.ProviderEKS:
		return EKS{}
	case inventory.ProviderGKE:
		return GKE{}
	case inventory.ProviderAKS:
		return AKS{}
	}
	return Generic{name: name}
}

func dataPlaneTitle(h HopContext) string {
	switch h.DataPlane {
	case DataPlaneOptional:
		return fmt.Sprintf("Optional: nodes may stay on their current version for this hop (still within skew of %s)", h.To)
	case DataPlaneRequired:
		return fmt.Sprintf("Upgrade the data plane to %s (required: nodes would exceed the skew limit at the next hop)", h.To)
	}
	return fmt.Sprintf("Upgrade the data plane to %s", h.To)
}

func verifyStep(h HopContext) Step {
	return Step{
		Phase: PhaseVerify,
		Title: fmt.Sprintf("Verify the cluster on %s", h.To),
		Detail: "Confirm all nodes are Ready, workloads are healthy and SLOs are unaffected, then re-run `jin plan` " +
			"to check for new deprecated API calls before the next hop.",
	}
}

type Generic struct{ name string }

func (g Generic) Name() string {
	if g.name == "" {
		return inventory.ProviderGeneric
	}
	return g.name
}

func (Generic) HopSteps(h HopContext) []Step {
	return []Step{
		{
			Phase:  PhaseControlPlane,
			Title:  fmt.Sprintf("Upgrade the control plane from %s to %s", h.From, h.To),
			Detail: "Follow your distribution's procedure (kubeadm: `kubeadm upgrade plan` then `kubeadm upgrade apply`). Upgrade one minor version at a time.",
		},
		{
			Phase:  PhaseAddons,
			Title:  "Update cluster add-ons (kube-proxy, CoreDNS, CNI, CSI drivers) to versions supporting " + h.To.String(),
			Detail: "kube-proxy must not be newer than the API server.",
		},
		{
			Phase:  PhaseDataPlane,
			Title:  dataPlaneTitle(h),
			Detail: "Cordon and drain nodes one at a time (or roll node pools), respecting PodDisruptionBudgets.",
		},
		verifyStep(h),
	}
}

func joinAddons(names []string, candidates ...string) string {
	var found []string
	for _, c := range candidates {
		if slices.Contains(names, c) {
			found = append(found, c)
		}
	}
	if len(found) == 0 {
		return strings.Join(candidates, ", ")
	}
	return strings.Join(found, ", ")
}
