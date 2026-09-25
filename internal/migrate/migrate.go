// Package migrate assesses what ties a cluster to its cloud and what moving it to another costs.
package migrate

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jin-k8s/jin/internal/snapshot"
)

const (
	EKS = "eks"
	GKE = "gke"
	AKS = "aks"
)

type Effort string

const (
	Low    Effort = "low"
	Medium Effort = "medium"
	High   Effort = "high"
)

func (e Effort) points() int {
	switch e {
	case Low:
		return 1
	case Medium:
		return 3
	}
	return 8
}

type Item struct {
	Category string   `json:"category"`
	Binding  string   `json:"binding"`
	Mapping  string   `json:"mapping"`
	Effort   Effort   `json:"effort"`
	Count    int      `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

type Assessment struct {
	Context string `json:"context"`
	Source  string `json:"source"`
	Target  string `json:"target"`
	Items   []Item `json:"items"`
	Summary struct {
		Items             int            `json:"items"`
		ByCategory        map[string]int `json:"byCategory"`
		EffortPoints      int            `json:"effortPoints"`
		DataGiB           float64        `json:"dataGiB"`
		ExternalEndpoints int            `json:"externalEndpoints"`
		Workloads         int            `json:"workloads"`
	} `json:"summary"`
	// Size is a t-shirt estimate of engineering effort: S, M, L or XL.
	Size     string   `json:"size"`
	Notes    []string `json:"notes"`
	Warnings []string `json:"warnings,omitempty"`
}

var names = map[string]string{EKS: "Amazon EKS", GKE: "Google GKE", AKS: "Azure AKS"}

// mapping holds the target-side equivalent for each source binding, keyed by target provider.
var mapping = map[string]map[string]string{
	"identity:irsa": {
		GKE: "Workload Identity Federation for GKE: annotate the KSA with iam.gke.io/gcp-service-account and translate the IAM role's policies to GCP roles.",
		AKS: "Microsoft Entra Workload ID: annotate with azure.workload.identity/client-id, label pods azure.workload.identity/use, add a federated credential, and translate policies to Azure RBAC.",
	},
	"identity:gke-wi": {
		EKS: "EKS Pod Identity or IRSA: associate the service account with an IAM role and translate GCP roles to IAM policies.",
		AKS: "Microsoft Entra Workload ID with a federated credential; translate GCP roles to Azure RBAC.",
	},
	"identity:azure-wi": {
		EKS: "EKS Pod Identity or IRSA; translate Azure RBAC assignments to IAM policies.",
		GKE: "Workload Identity Federation for GKE; translate Azure RBAC assignments to GCP roles.",
	},
	"storage:block": {
		EKS: "EBS CSI driver (gp3 StorageClass).",
		GKE: "Persistent Disk CSI driver (pd-balanced / hyperdisk StorageClass).",
		AKS: "Azure Disk CSI driver (managed-csi StorageClass).",
	},
	"storage:file": {
		EKS: "EFS CSI driver.",
		GKE: "Filestore CSI driver.",
		AKS: "Azure Files CSI driver.",
	},
	"data": {
		EKS: "Copy volume data (Velero with file-system backup, or application-level replication); snapshots do not cross clouds.",
		GKE: "Copy volume data (Velero with file-system backup, or application-level replication); snapshots do not cross clouds.",
		AKS: "Copy volume data (Velero with file-system backup, or application-level replication); snapshots do not cross clouds.",
	},
	"lb:annotated": {
		EKS: "AWS Load Balancer Controller annotations (service.beta.kubernetes.io/aws-load-balancer-*).",
		GKE: "GKE Service annotations (networking.gke.io/load-balancer-type, cloud.google.com/l4-rbs).",
		AKS: "Azure load balancer annotations (service.beta.kubernetes.io/azure-load-balancer-*).",
	},
	"lb:plain": {
		EKS: "Works as is; the external address changes, so DNS must move.",
		GKE: "Works as is; the external address changes, so DNS must move.",
		AKS: "Works as is; the external address changes, so DNS must move.",
	},
	"ingress": {
		EKS: "AWS Load Balancer Controller (IngressClass alb) or Gateway API; rewrite alb.ingress.kubernetes.io annotations.",
		GKE: "GKE Gateway API (gke-l7-global-external-managed) or GCE Ingress; rewrite provider-specific annotations.",
		AKS: "Application Gateway for Containers (Gateway API) or AGIC; rewrite provider-specific annotations.",
	},
	"registry": {
		EKS: "Replicate images to Amazon ECR and grant node/IRSA pull access.",
		GKE: "Replicate images to Artifact Registry and grant the node service account read access.",
		AKS: "Replicate images to Azure Container Registry and attach it to the cluster (az aks update --attach-acr).",
	},
	"secrets": {
		EKS: "AWS Secrets Manager / SSM provider (External Secrets Operator or Secrets Store CSI).",
		GKE: "Google Secret Manager provider (External Secrets Operator or Secrets Store CSI).",
		AKS: "Azure Key Vault provider (External Secrets Operator or Secrets Store CSI).",
	},
	"scheduling": {
		EKS: "Replace cloud-specific node labels, instance types and zone names with EKS equivalents (or topology-agnostic rules).",
		GKE: "Replace cloud-specific node labels, machine types and zone names with GKE equivalents (or topology-agnostic rules).",
		AKS: "Replace cloud-specific node labels, VM sizes and zone names with AKS equivalents (or topology-agnostic rules).",
	},
	"autoscaling": {
		EKS: "Karpenter or Cluster Autoscaler with managed node groups.",
		GKE: "GKE cluster autoscaler with node auto-provisioning, or Autopilot.",
		AKS: "AKS Node Auto Provisioning (Karpenter-based) or the cluster autoscaler.",
	},
	"cloud-operator": {
		EKS: "AWS Controllers for Kubernetes (ACK) or Crossplane AWS providers; the managed resources themselves must be re-created and their data migrated.",
		GKE: "Config Connector or Crossplane GCP providers; the managed resources themselves must be re-created and their data migrated.",
		AKS: "Azure Service Operator or Crossplane Azure providers; the managed resources themselves must be re-created and their data migrated.",
	},
}

func mapTo(key, target string) string {
	if m := mapping[key][target]; m != "" {
		return m
	}
	return "Re-implement with the " + names[target] + " equivalent."
}

type group struct {
	item Item
	seen map[string]bool
}

type builder struct {
	target string
	groups map[string]*group
	order  []string
}

func (b *builder) add(category, binding, key string, effort Effort, example string) {
	id := category + "|" + binding
	g := b.groups[id]
	if g == nil {
		g = &group{item: Item{Category: category, Binding: binding, Mapping: mapTo(key, b.target), Effort: effort}, seen: map[string]bool{}}
		b.groups[id] = g
		b.order = append(b.order, id)
	}
	if example != "" && !g.seen[example] {
		g.seen[example] = true
		g.item.Count++
		if len(g.item.Examples) < 5 {
			g.item.Examples = append(g.item.Examples, example)
		}
	} else if example == "" {
		g.item.Count++
	}
}

func registryOf(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return ""
	}
	return first
}

func registryCloud(host string) string {
	switch {
	case strings.Contains(host, ".dkr.ecr.") && strings.HasSuffix(host, ".amazonaws.com"):
		return EKS
	case host == "gcr.io" || strings.HasSuffix(host, ".gcr.io") || strings.HasSuffix(host, "-docker.pkg.dev"):
		return GKE
	case strings.HasSuffix(host, ".azurecr.io"):
		return AKS
	}
	return ""
}

func provisionerKind(p string) (cloud, kind string) {
	switch p {
	case "ebs.csi.aws.com", "kubernetes.io/aws-ebs":
		return EKS, "block"
	case "efs.csi.aws.com", "fsx.csi.aws.com":
		return EKS, "file"
	case "pd.csi.storage.gke.io", "kubernetes.io/gce-pd":
		return GKE, "block"
	case "filestore.csi.storage.gke.io":
		return GKE, "file"
	case "disk.csi.azure.com", "kubernetes.io/azure-disk":
		return AKS, "block"
	case "file.csi.azure.com", "kubernetes.io/azure-file":
		return AKS, "file"
	}
	return "", ""
}

var cloudLabelPrefixes = []string{"eks.amazonaws.com/", "karpenter.sh/", "karpenter.k8s.aws/", "cloud.google.com/", "kubernetes.azure.com/", "node.kubernetes.io/instance-type", "topology.kubernetes.io/zone", "topology.kubernetes.io/region"}

var cloudCRDGroups = []struct{ suffix, category, key string }{
	{".services.k8s.aws", "cloud operators", "cloud-operator"},
	{".cnrm.cloud.google.com", "cloud operators", "cloud-operator"},
	{".azure.com", "cloud operators", "cloud-operator"},
	{".aws.upbound.io", "cloud operators", "cloud-operator"},
	{".gcp.upbound.io", "cloud operators", "cloud-operator"},
	{".azure.upbound.io", "cloud operators", "cloud-operator"},
	{".karpenter.sh", "autoscaling", "autoscaling"},
	{".karpenter.k8s.aws", "autoscaling", "autoscaling"},
	{".elbv2.k8s.aws", "load balancing", "ingress"},
}

// Assess maps every cloud-specific binding in the snapshot to the target provider.
func Assess(s *snapshot.Snapshot, target string) (*Assessment, error) {
	if _, ok := names[target]; !ok {
		return nil, fmt.Errorf("unknown target %q (eks, gke or aks)", target)
	}
	if s.Provider == target {
		return nil, fmt.Errorf("the cluster already runs on %s", names[target])
	}
	b := &builder{target: target, groups: map[string]*group{}}

	for _, sa := range s.ServiceAccounts {
		switch {
		case sa.Annotations["eks.amazonaws.com/role-arn"] != "":
			b.add("identity", "IRSA (IAM roles for service accounts)", "identity:irsa", Medium, sa.ID())
		case sa.Annotations["iam.gke.io/gcp-service-account"] != "":
			b.add("identity", "GKE Workload Identity", "identity:gke-wi", Medium, sa.ID())
		case sa.Annotations["azure.workload.identity/client-id"] != "" || sa.Labels["azure.workload.identity/use"] != "":
			b.add("identity", "Azure Workload Identity", "identity:azure-wi", Medium, sa.ID())
		}
	}
	if s.Provider == EKS {
		s.Warnings = append(s.Warnings, "EKS Pod Identity associations live in the EKS API, not in Kubernetes; list them with `aws eks list-pod-identity-associations`.")
	}

	classCloud := map[string][2]string{}
	for _, sc := range s.StorageClasses {
		if c, k := provisionerKind(sc.Provisioner); c != "" && c != target {
			classCloud[sc.Name] = [2]string{c, k}
		}
	}
	var dataBytes int64
	for _, pvc := range s.PVCs {
		ck, ok := classCloud[pvc.StorageClass]
		if !ok {
			continue
		}
		b.add("storage", fmt.Sprintf("StorageClass %s (%s %s volumes)", pvc.StorageClass, names[ck[0]], ck[1]), "storage:"+ck[1], Low, pvc.Namespace+"/"+pvc.Name)
		dataBytes += pvc.Bytes
	}
	if dataBytes > 0 {
		b.add("data", fmt.Sprintf("%.1f GiB of persistent volume data", float64(dataBytes)/(1<<30)), "data", High, "")
	}

	for _, sv := range s.Services {
		if len(sv.Annotations) > 0 {
			b.add("load balancing", "LoadBalancer Services with cloud-specific annotations", "lb:annotated", Medium, sv.ID())
		} else {
			b.add("load balancing", "LoadBalancer Services", "lb:plain", Low, sv.ID())
		}
	}
	for _, in := range s.Ingresses {
		ctrl := s.IngressClasses[in.Class]
		cloudSpecific := len(in.Annotations) > 0 || strings.Contains(ctrl, "ingress.k8s.aws") || strings.Contains(ctrl, "gce") || strings.Contains(ctrl, "azure") || in.Class == "alb" || in.Class == "gce"
		if cloudSpecific {
			b.add("load balancing", fmt.Sprintf("Ingresses on a cloud load balancer (%s)", orDash(in.Class)), "ingress", Medium, in.ID())
		}
	}

	regs := map[string][]string{}
	for _, w := range s.Workloads {
		for _, img := range w.Images {
			if h := registryOf(img); h != "" && registryCloud(h) != "" && registryCloud(h) != target {
				regs[h] = append(regs[h], w.ID())
			}
		}
		keys := append([]string{}, w.AffinityKeys...)
		for k, v := range w.NodeSelector {
			keys = append(keys, k+"="+v)
		}
		for _, k := range keys {
			for _, p := range cloudLabelPrefixes {
				if strings.HasPrefix(k, p) {
					b.add("scheduling", "Node selectors and affinities on cloud-specific labels", "scheduling", Low, w.ID())
					break
				}
			}
		}
	}
	hosts := make([]string, 0, len(regs))
	for h := range regs {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		for _, w := range regs[h] {
			b.add("images", "Images pulled from "+h, "registry", Medium, w)
		}
	}

	for _, sb := range s.SecretBackends {
		b.add("secrets", fmt.Sprintf("%s provider %q", sb.Kind, sb.Provider), "secrets", Medium, strings.TrimPrefix(sb.Namespace+"/"+sb.Name, "/"))
	}

	crdGroups := map[string]bool{}
	for _, c := range s.CRDs {
		_, grp, _ := strings.Cut(c, ".")
		for _, g := range cloudCRDGroups {
			if strings.HasSuffix("."+grp, g.suffix) && !crdGroups[grp] {
				crdGroups[grp] = true
				effort := Medium
				if g.key == "cloud-operator" {
					effort = High
				}
				b.add(g.category, "Custom resources in "+grp, g.key, effort, grp)
			}
		}
	}

	for _, a := range s.Addons {
		switch a.Name {
		case "karpenter", "cluster-autoscaler":
			b.add("autoscaling", "Node autoscaler: "+a.Name, "autoscaling", Medium, a.Namespace+"/"+a.Workload)
		case "aws-load-balancer-controller":
			b.add("load balancing", "AWS Load Balancer Controller", "ingress", Medium, a.Namespace+"/"+a.Workload)
		}
	}

	a := &Assessment{Context: s.Context, Source: s.Provider, Target: target, Warnings: s.Warnings}
	a.Summary.ByCategory = map[string]int{}
	for _, id := range b.order {
		it := b.groups[id].item
		a.Items = append(a.Items, it)
		a.Summary.ByCategory[it.Category] += it.Count
		pts := it.Effort.points()
		if it.Count > 1 && it.Effort != High {
			pts += (it.Count - 1) / 3
		}
		a.Summary.EffortPoints += pts
	}
	sort.SliceStable(a.Items, func(i, j int) bool { return a.Items[i].Effort.points() > a.Items[j].Effort.points() })
	a.Summary.Items = len(a.Items)
	a.Summary.DataGiB = float64(dataBytes) / (1 << 30)
	a.Summary.ExternalEndpoints = len(s.Services) + len(s.Ingresses)
	a.Summary.Workloads = len(s.Workloads)
	switch p := a.Summary.EffortPoints; {
	case p < 10:
		a.Size = "S"
	case p < 30:
		a.Size = "M"
	case p < 80:
		a.Size = "L"
	default:
		a.Size = "XL"
	}
	a.Notes = []string{
		"Databases, queues, caches and object storage outside Kubernetes are usually the largest part of a cloud migration and are not visible from the cluster. Inventory them separately.",
		"Kubernetes manifests themselves (Deployments, Services, ConfigMaps) generally move unchanged; the effort is in the cloud bindings listed here.",
		fmt.Sprintf("Build the %s cluster, then validate it with Jin's blue/green comparison before shifting traffic.", names[target]),
	}
	return a, nil
}

func orDash(s string) string {
	if s == "" {
		return "no class"
	}
	return s
}
