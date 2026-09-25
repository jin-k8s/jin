package check

import (
	"fmt"
	"time"

	"github.com/jin-k8s/jin/internal/support"
)

// SupportWindow reports extended-support charges and approaching end-of-support dates.
type SupportWindow struct{}

func (SupportWindow) ID() string { return "support-window" }

func (SupportWindow) Run(in Input) []Finding {
	a := in.Support
	if a == nil {
		return nil
	}
	var out []Finding
	switch {
	case a.CurrentStatus == support.StatusUnsupported:
		out = append(out, Finding{
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("Kubernetes %s is no longer supported by %s", a.Current, a.Provider),
			Detail:      "The provider may upgrade the control plane automatically.",
			Remediation: "Upgrade now, on your schedule.",
		})
	case a.CurrentStatus == support.StatusExtended:
		f := Finding{
			Severity: SeverityWarning,
			Title:    fmt.Sprintf("Cluster is in extended support: about $%s per year extra", money(a.CurrentSurchargePerYearUSD)),
			Detail:   a.PricingSource + ".",
		}
		if a.EndOfExtended != nil {
			f.Detail += " Extended support ends " + day(*a.EndOfExtended) + "."
		}
		switch {
		case a.TargetStatus == "" || a.TargetStatus == support.StatusStandard:
			f.Remediation = fmt.Sprintf("Upgrading to %s returns the cluster to standard pricing.", a.Target)
		case a.FirstStandard != nil:
			f.Remediation = fmt.Sprintf("%s is also in extended support; the cluster returns to standard pricing at %s.", a.Target, *a.FirstStandard)
		default:
			f.Remediation = fmt.Sprintf("%s is also in extended support.", a.Target)
		}
		out = append(out, f)
	case a.DaysToEndOfStandard != nil && *a.DaysToEndOfStandard <= 90:
		msg := fmt.Sprintf("Standard support for %s ends %s (in %d days)", a.Current, day(*a.EndOfStandard), *a.DaysToEndOfStandard)
		f := Finding{Severity: SeverityWarning, Title: msg}
		if a.ExtendedSupportCostPerYearUSD > 0 {
			f.Detail = fmt.Sprintf("After that the cluster moves to extended support at about $%s per year extra. %s.", money(a.ExtendedSupportCostPerYearUSD), a.PricingSource)
		}
		out = append(out, f)
	}
	if a.TargetStatus == support.StatusExtended {
		f := Finding{
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("Target %s is already in extended support", a.Target),
		}
		if a.FirstStandard != nil {
			f.Remediation = fmt.Sprintf("Plan through to %s to stop paying for extended support.", *a.FirstStandard)
		}
		out = append(out, f)
	} else if a.TargetEndOfStandard != nil && a.TargetEndOfStandard.Sub(in.Now) < 180*24*time.Hour {
		out = append(out, Finding{
			Severity:    SeverityInfo,
			Title:       fmt.Sprintf("Target %s leaves standard support on %s", a.Target, day(*a.TargetEndOfStandard)),
			Remediation: "Consider planning to a newer target to avoid another upgrade soon after this one.",
		})
	}
	return out
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

func money(v float64) string {
	s := fmt.Sprintf("%.0f", v)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
