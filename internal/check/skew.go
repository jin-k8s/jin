package check

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
)

// VersionSkew enforces the upstream skew policy for kubelets and kube-proxy along the upgrade path.
type VersionSkew struct{}

func (VersionSkew) ID() string { return "version-skew" }

type nodeGroupKey struct {
	pool, poolType string
	version        kube.Version
	raw            string
}

func (VersionSkew) Run(in Input) []Finding {
	var out []Finding

	groups := map[nodeGroupKey][]string{}
	for _, n := range in.Inventory.Nodes {
		k := nodeGroupKey{pool: n.Pool, poolType: n.PoolType, version: n.Version}
		if n.Version.IsZero() {
			k.raw = n.KubeletVersion
		}
		groups[k] = append(groups[k], n.Name)
	}
	keys := make([]nodeGroupKey, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].poolType+keys[i].pool != keys[j].poolType+keys[j].pool {
			return keys[i].poolType+keys[i].pool < keys[j].poolType+keys[j].pool
		}
		return keys[i].version.Less(keys[j].version)
	})

	for _, k := range keys {
		names := groups[k]
		res := describeGroup(k, names)
		switch {
		case k.version.IsZero():
			out = append(out, Finding{
				Severity: SeverityWarning,
				Title:    fmt.Sprintf("Could not parse kubelet version %q", k.raw),
				Resource: res,
			})
		case in.Current.Less(k.version):
			out = append(out, Finding{
				Severity:    SeverityWarning,
				Title:       fmt.Sprintf("Kubelet %s is newer than the control plane %s (unsupported skew)", k.version, in.Current),
				Resource:    res,
				Remediation: "Kubelets must never be newer than kube-apiserver. Upgrade the control plane first or roll these nodes back to a supported AMI/image.",
			})
		default:
			if f, ok := skewFinding(in, k.version, res, "Nodes"); ok {
				f.Remediation = nodeRemediation(k.poolType, f.Hop)
				out = append(out, f)
			}
		}
	}

	for _, a := range in.Inventory.Addons {
		addon, ok := in.KB.Addon(a.Name)
		if !ok || addon.VersionPolicy != compat.PolicyKubeletSkew {
			continue
		}
		v, err := kube.ParseVersion(a.Version)
		res := fmt.Sprintf("%s %s/%s", a.Kind, a.Namespace, a.Workload)
		if err != nil {
			out = append(out, Finding{Severity: SeverityWarning, Title: fmt.Sprintf("Could not parse %s version from image tag %q", a.Name, a.Version), Resource: res})
			continue
		}
		if in.Current.Less(v) {
			out = append(out, Finding{Severity: SeverityWarning, Title: fmt.Sprintf("%s %s is newer than the control plane %s", a.Name, v, in.Current), Resource: res})
			continue
		}
		if f, ok := skewFinding(in, v, res, a.Name); ok {
			f.Remediation = fmt.Sprintf("Update %s to a %s-compatible build before upgrading the control plane to %s.", a.Name, in.Current, f.Hop)
			out = append(out, f)
		}
	}
	return out
}

// skewFinding reports the first hop at which a component at version v falls outside the allowed skew.
func skewFinding(in Input, v kube.Version, res, what string) (Finding, bool) {
	for _, hop := range in.Hops() {
		maxSkew := kube.MaxKubeletSkew(hop)
		if behind := v.MinorsBehind(hop); behind > maxSkew {
			return Finding{
				Severity: SeverityBlocker,
				Hop:      hop,
				Title:    fmt.Sprintf("%s at %s would be %d minors behind a %s control plane (max %d)", what, v, behind, hop, maxSkew),
				Resource: res,
			}, true
		}
	}
	return Finding{}, false
}

func nodeRemediation(poolType string, hop kube.Version) string {
	floor := kube.Version{Major: hop.Major, Minor: hop.Minor - kube.MaxKubeletSkew(hop)}
	base := fmt.Sprintf("Upgrade these nodes to at least %s before the control plane reaches %s.", floor, hop)
	switch poolType {
	case inventory.PoolEKSManaged:
		return base + " Update the managed node group version (aws_eks_node_group `version` / release_version)."
	case inventory.PoolKarpenter:
		return base + " Update the EC2NodeClass AMI selection so Karpenter drifts these nodes."
	case inventory.PoolEKSFargate:
		return base + " Restart the Fargate pods; they pick up the current platform version on reschedule."
	case inventory.PoolEKSAuto:
		return base + " EKS Auto Mode replaces nodes itself; check for PDBs or NodePool disruption budgets holding them."
	}
	return base
}

func describeGroup(k nodeGroupKey, names []string) string {
	label := k.poolType
	if k.pool != "" {
		label += " " + k.pool
	}
	sample := names
	if len(sample) > 3 {
		sample = sample[:3]
	}
	s := fmt.Sprintf("%s: %d node(s) [%s", label, len(names), strings.Join(sample, ", "))
	if len(names) > 3 {
		s += ", ..."
	}
	return s + "]"
}
