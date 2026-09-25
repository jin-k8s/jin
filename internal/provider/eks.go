package provider

import (
	"fmt"
	"strings"

	"github.com/jin-k8s/jin/internal/inventory"
)

type EKS struct{}

func (EKS) Name() string { return inventory.ProviderEKS }

func (EKS) HopSteps(h HopContext) []Step {
	steps := []Step{
		{
			Phase: PhaseControlPlane,
			Title: fmt.Sprintf("Upgrade the EKS control plane from %s to %s", h.From, h.To),
			Detail: "Change the cluster version in IaC (Terraform `aws_eks_cluster.version`, or the version input of the " +
				"terraform-aws-modules/eks module) and apply through your pipeline. EKS upgrades one minor version at a time " +
				"and a control-plane upgrade cannot be rolled back. The cluster subnets need free IP addresses for the upgrade.",
		},
		{
			Phase: PhaseAddons,
			Title: "Update EKS add-ons: " + joinAddons(h.Addons, "kube-proxy", "coredns", "vpc-cni"),
			Detail: fmt.Sprintf("`aws eks describe-addon-versions --kubernetes-version %s --addon-name <name>` lists compatible versions; "+
				"pin them in `aws_eks_addon.addon_version`. Update self-managed controllers (load balancer controller, CSI drivers, Karpenter) "+
				"to releases that support %s.", h.To, h.To),
		},
	}

	var dp []string
	if h.hasPool(inventory.PoolEKSManaged) {
		dp = append(dp, "Managed node groups: set `aws_eks_node_group.version` (or `release_version`/launch-template AMI) to "+h.To.String()+"; the rolling update respects PDBs.")
	}
	if h.hasPool(inventory.PoolKarpenter) {
		dp = append(dp, "Karpenter: with an EC2NodeClass `amiSelectorTerms` alias (e.g. al2023@latest), nodes drift to "+h.To.String()+" AMIs after the control-plane upgrade; with pinned AMIs, update the pin. Disruption budgets on the NodePool control the pace.")
	}
	if h.hasPool(inventory.PoolEKSAuto) {
		dp = append(dp, "EKS Auto Mode: AWS replaces nodes after the control-plane upgrade, respecting PDBs; watch for stuck replacements.")
	}
	if h.hasPool(inventory.PoolEKSFargate) {
		dp = append(dp, "Fargate: restart Fargate workloads (`kubectl rollout restart`) so pods reschedule onto the new version.")
	}
	if h.hasPool(inventory.PoolSelfManaged) {
		dp = append(dp, "Self-managed nodes: point the launch template at an EKS-optimized AMI for "+h.To.String()+" and roll the Auto Scaling group.")
	}
	if len(dp) == 0 {
		dp = append(dp, "Roll every node pool onto "+h.To.String()+" images, respecting PDBs.")
	}
	steps = append(steps, Step{Phase: PhaseDataPlane, Title: dataPlaneTitle(h), Detail: strings.Join(dp, " ")})
	return append(steps, verifyStep(h))
}
