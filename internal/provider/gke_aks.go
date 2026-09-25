package provider

import (
	"fmt"

	"github.com/jin-k8s/jin/internal/inventory"
)

type GKE struct{}

func (GKE) Name() string { return inventory.ProviderGKE }

func (GKE) HopSteps(h HopContext) []Step {
	return []Step{
		{
			Phase: PhaseControlPlane,
			Title: fmt.Sprintf("Upgrade the GKE control plane from %s to %s", h.From, h.To),
			Detail: "Set `google_container_cluster.min_master_version` (or move the release channel) in IaC and apply. " +
				"Clusters on a release channel are auto-upgraded; use maintenance windows and exclusions to control timing.",
		},
		{
			Phase:  PhaseAddons,
			Title:  "Check self-managed controllers and CRDs for " + h.To.String() + " support",
			Detail: "GKE manages kube-proxy, CoreDNS/kube-dns and the CNI; verify only what you installed yourself.",
		},
		{
			Phase:  PhaseDataPlane,
			Title:  dataPlaneTitle(h),
			Detail: "Set `google_container_node_pool.version` and choose surge or blue-green upgrade settings. GKE honours PDBs for up to one hour per node during upgrades.",
		},
		verifyStep(h),
	}
}

type AKS struct{}

func (AKS) Name() string { return inventory.ProviderAKS }

func (AKS) HopSteps(h HopContext) []Step {
	return []Step{
		{
			Phase: PhaseControlPlane,
			Title: fmt.Sprintf("Upgrade the AKS control plane from %s to %s", h.From, h.To),
			Detail: "Set `azurerm_kubernetes_cluster.kubernetes_version` in IaC and apply (equivalent to `az aks upgrade --control-plane-only`). " +
				"Check the cluster's auto-upgrade channel so it does not race your change.",
		},
		{
			Phase:  PhaseAddons,
			Title:  "Check self-managed controllers and CRDs for " + h.To.String() + " support",
			Detail: "AKS manages core add-ons with the control plane; verify only what you installed yourself.",
		},
		{
			Phase:  PhaseDataPlane,
			Title:  dataPlaneTitle(h),
			Detail: "Set `orchestrator_version` on each node pool and configure `upgrade_settings.max_surge`; drains respect PDBs.",
		},
		verifyStep(h),
	}
}
