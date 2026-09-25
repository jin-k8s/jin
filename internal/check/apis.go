package check

import (
	"fmt"

	"github.com/jin-k8s/jin/internal/inventory"
)

type RemovedAPIs struct{}

func (RemovedAPIs) ID() string { return "removed-apis" }

func (RemovedAPIs) Run(in Input) []Finding {
	var out []Finding
	for _, m := range in.Inventory.Manifests {
		r, ok := in.KB.Removal(m.APIVersion, m.Kind)
		if !ok {
			continue
		}
		fix := migrateHint(m, r.Replacement, r.Notes)
		res := m.Resource()
		if m.Origin != "" {
			res += " (" + m.Origin + ")"
		}

		switch {
		case !in.Current.Less(r.RemovedIn) && m.Source == inventory.SourceHelm:
			out = append(out, Finding{
				Severity: SeverityWarning,
				Title:    fmt.Sprintf("Deployed Helm release still references %s %s, removed in %s", m.APIVersion, m.Kind, r.RemovedIn),
				Resource: res,
				Detail:   "The cluster already dropped this API. The next `helm upgrade` or `helm rollback` of this release will fail while building the current release manifest.",
				Remediation: "Rewrite the stored release with the helm-mapkubeapis plugin (`helm mapkubeapis " + releaseName(m) + "`), then " +
					"upgrade to a chart version that uses " + orNone(r.Replacement) + ".",
			})
		case !in.Current.Less(r.RemovedIn):
			out = append(out, Finding{
				Severity:    SeverityWarning,
				Title:       fmt.Sprintf("Source manifest last applied with %s %s, removed in %s", m.APIVersion, m.Kind, r.RemovedIn),
				Resource:    res,
				Detail:      "The object works, but re-applying the same manifest will be rejected by the API server.",
				Remediation: fix,
			})
		case in.inPath(r.RemovedIn):
			out = append(out, Finding{
				Severity: SeverityBlocker,
				Hop:      r.RemovedIn,
				Title:    fmt.Sprintf("Uses %s %s, removed in %s", m.APIVersion, m.Kind, r.RemovedIn),
				Resource: res,
				Detail: "Existing objects keep working after the upgrade, but the next deploy of this manifest " +
					"(CI/CD, GitOps sync, helm upgrade) will fail.",
				Remediation: fix,
			})
		}
	}
	return out
}

func migrateHint(m inventory.ManifestRef, replacement, notes string) string {
	var fix string
	switch {
	case replacement == "":
		fix = "Remove this object's dependency on the API."
	case m.Source == inventory.SourceHelm:
		fix = fmt.Sprintf("Upgrade the chart (or its templates) to %s before the control-plane upgrade.", replacement)
	default:
		fix = fmt.Sprintf("Update the source manifest to %s and re-apply it.", replacement)
	}
	if notes != "" {
		fix += " " + notes
	}
	return fix
}

func releaseName(m inventory.ManifestRef) string {
	if m.Release == "" {
		return "<release> --namespace <namespace>"
	}
	return m.Release + " --namespace " + m.ReleaseNamespace
}

func orNone(s string) string {
	if s == "" {
		return "no deprecated APIs"
	}
	return s
}

type DeprecatedAPICalls struct{}

func (DeprecatedAPICalls) ID() string { return "deprecated-api-calls" }

func (DeprecatedAPICalls) Run(in Input) []Finding {
	var out []Finding
	for _, r := range in.Inventory.DeprecatedAPIRequests {
		if r.RemovedRelease.IsZero() || !in.inPath(r.RemovedRelease) {
			continue
		}
		res := r.APIVersion() + " " + r.Resource
		if r.Subresource != "" {
			res += "/" + r.Subresource
		}
		out = append(out, Finding{
			Severity: SeverityBlocker,
			Hop:      r.RemovedRelease,
			Title:    fmt.Sprintf("A client is still calling %s, removed in %s", res, r.RemovedRelease),
			Resource: res,
			Detail: "Reported by the apiserver_requested_deprecated_apis metric since this API server instance started. " +
				"Control planes with several API server instances (EKS, GKE, AKS) may have served additional deprecated calls not visible here.",
			Remediation: "Find the caller in the API server audit log: filter on the annotation k8s.io/deprecated=\"true\" " +
				"and inspect userAgent (on EKS, enable the audit log type and query it with CloudWatch Logs Insights). " +
				"Upgrade that client or controller before the control-plane upgrade.",
		})
	}
	return out
}
