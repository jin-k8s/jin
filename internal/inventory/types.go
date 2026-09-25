// Package inventory collects a read-only snapshot of a cluster for upgrade planning.
package inventory

import (
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/kube"
)

const (
	ProviderEKS     = "eks"
	ProviderGKE     = "gke"
	ProviderAKS     = "aks"
	ProviderKind    = "kind"
	ProviderGeneric = "generic"
)

const (
	PoolEKSManaged  = "eks-managed-nodegroup"
	PoolEKSAuto     = "eks-auto-mode"
	PoolEKSFargate  = "eks-fargate"
	PoolKarpenter   = "karpenter"
	PoolGKE         = "gke-nodepool"
	PoolAKS         = "aks-agentpool"
	PoolSelfManaged = "self-managed"
)

const (
	SourceHelm        = "helm"
	SourceLastApplied = "last-applied"
)

type Inventory struct {
	CollectedAt           time.Time              `json:"collectedAt"`
	ServerVersion         kube.Version           `json:"serverVersion"`
	ServerGitVersion      string                 `json:"serverGitVersion"`
	Provider              string                 `json:"provider"`
	Nodes                 []Node                 `json:"nodes"`
	Addons                []AddonInstance        `json:"addons"`
	HelmReleases          []HelmRelease          `json:"helmReleases"`
	Manifests             []ManifestRef          `json:"manifests"`
	PDBs                  []PDB                  `json:"pdbs"`
	DeprecatedAPIRequests []DeprecatedAPIRequest `json:"deprecatedApiRequests"`
	// Warnings lists data that could not be collected; findings may be incomplete when non-empty.
	Warnings []string `json:"warnings,omitempty"`
}

type Node struct {
	Name           string       `json:"name"`
	KubeletVersion string       `json:"kubeletVersion"`
	Version        kube.Version `json:"version"`
	ProviderID     string       `json:"providerId,omitempty"`
	Pool           string       `json:"pool,omitempty"`
	PoolType       string       `json:"poolType"`
	Ready          bool         `json:"ready"`
	Unschedulable  bool         `json:"unschedulable,omitempty"`
}

type AddonInstance struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Image     string `json:"image"`
	Version   string `json:"version"`
}

type HelmRelease struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Revision     int    `json:"revision"`
	Chart        string `json:"chart"`
	ChartVersion string `json:"chartVersion"`
	AppVersion   string `json:"appVersion,omitempty"`
}

// ManifestRef records the apiVersion a resource was declared with, not the version it is stored at.
type ManifestRef struct {
	Source     string `json:"source"`
	Origin     string `json:"origin,omitempty"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	// Release and ReleaseNamespace are set for SourceHelm.
	Release          string `json:"release,omitempty"`
	ReleaseNamespace string `json:"releaseNamespace,omitempty"`
}

func (m ManifestRef) Resource() string {
	if m.Namespace == "" {
		return m.Kind + "/" + m.Name
	}
	return m.Kind + " " + m.Namespace + "/" + m.Name
}

type PDB struct {
	Namespace          string `json:"namespace"`
	Name               string `json:"name"`
	DisruptionsAllowed int32  `json:"disruptionsAllowed"`
	ExpectedPods       int32  `json:"expectedPods"`
	CurrentHealthy     int32  `json:"currentHealthy"`
	DesiredHealthy     int32  `json:"desiredHealthy"`
}

type DeprecatedAPIRequest struct {
	Group          string       `json:"group"`
	Version        string       `json:"version"`
	Resource       string       `json:"resource"`
	Subresource    string       `json:"subresource,omitempty"`
	RemovedRelease kube.Version `json:"removedRelease"`
}

func (d DeprecatedAPIRequest) APIVersion() string {
	if d.Group == "" {
		return d.Version
	}
	return d.Group + "/" + d.Version
}

// DetectProvider infers the managed Kubernetes offering from the server version string and node metadata.
func DetectProvider(gitVersion string, nodes []Node) string {
	switch {
	case strings.Contains(gitVersion, "-eks-"):
		return ProviderEKS
	case strings.Contains(gitVersion, "-gke."):
		return ProviderGKE
	}
	for _, n := range nodes {
		switch {
		case n.PoolType == PoolEKSManaged || n.PoolType == PoolEKSAuto || n.PoolType == PoolEKSFargate:
			return ProviderEKS
		case n.PoolType == PoolGKE:
			return ProviderGKE
		case n.PoolType == PoolAKS || strings.HasPrefix(n.ProviderID, "azure://"):
			return ProviderAKS
		case strings.HasPrefix(n.ProviderID, "kind://"):
			return ProviderKind
		}
	}
	return ProviderGeneric
}

func nodePool(labels map[string]string) (pool, poolType string) {
	switch labels["eks.amazonaws.com/compute-type"] {
	case "auto":
		return labels["karpenter.sh/nodepool"], PoolEKSAuto
	case "fargate":
		return "", PoolEKSFargate
	}
	switch {
	case labels["karpenter.sh/nodepool"] != "":
		return labels["karpenter.sh/nodepool"], PoolKarpenter
	case labels["eks.amazonaws.com/nodegroup"] != "":
		return labels["eks.amazonaws.com/nodegroup"], PoolEKSManaged
	case labels["cloud.google.com/gke-nodepool"] != "":
		return labels["cloud.google.com/gke-nodepool"], PoolGKE
	case labels["kubernetes.azure.com/agentpool"] != "":
		return labels["kubernetes.azure.com/agentpool"], PoolAKS
	case labels["alpha.eksctl.io/nodegroup-name"] != "":
		return labels["alpha.eksctl.io/nodegroup-name"], PoolSelfManaged
	}
	return "", PoolSelfManaged
}

func imageTag(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return ""
}
