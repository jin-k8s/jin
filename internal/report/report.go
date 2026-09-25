// Package report renders run records for humans (table, markdown) and machines (json).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/runrecord"
	"github.com/jin-k8s/jin/internal/support"
)

const (
	FormatTable    = "table"
	FormatMarkdown = "markdown"
	FormatJSON     = "json"
)

func Formats() []string { return []string{FormatTable, FormatMarkdown, FormatJSON} }

func Render(w io.Writer, r *runrecord.Record, format string) error {
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case FormatMarkdown:
		return renderMarkdown(w, r)
	case FormatTable, "":
		return renderText(w, r)
	}
	return fmt.Errorf("unknown output format %q (use %s)", format, strings.Join(Formats(), ", "))
}

func counts(p *plan.Plan) string {
	return fmt.Sprintf("%s · %s · %d info",
		plural(p.Summary.Blockers, "blocker"), plural(p.Summary.Warnings, "warning"), p.Summary.Info)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func verdict(p *plan.Plan) string {
	if p.Ready {
		return "READY"
	}
	return "BLOCKED"
}

func renderMarkdown(w io.Writer, r *runrecord.Record) error {
	var b strings.Builder
	p := r.Plan
	b.WriteString("# Jin upgrade plan\n\n")
	fmt.Fprintf(&b, "| | |\n|---|---|\n| Context | `%s` |\n", md(r.Cluster.Context))
	if p == nil {
		fmt.Fprintf(&b, "| Status | FAILED: %s |\n| Run ID | `%s` |\n", md(r.Error), r.ID)
		_, err := io.WriteString(w, b.String())
		return err
	}
	status := verdict(p) + ": " + counts(p)
	if !p.Complete {
		status += " (incomplete data)"
	}
	fmt.Fprintf(&b, "| Provider | %s |\n| Upgrade | %s → %s (%s) |\n| Status | %s |\n| Run ID | `%s` |\n",
		md(p.Provider), p.Current, p.Target, plural(len(p.Hops), "hop"), md(status), r.ID)

	if line := supportLine(p); line != "" {
		fmt.Fprintf(&b, "| Support | %s |\n", md(line))
	}

	if len(p.CollectionWarnings) > 0 {
		b.WriteString("\n> **Data collection warnings** (findings may be missing):\n")
		for _, cw := range p.CollectionWarnings {
			fmt.Fprintf(&b, "> - %s\n", md(cw))
		}
	}

	for i, h := range p.Hops {
		fmt.Fprintf(&b, "\n## Hop %d: %s → %s\n\n", i+1, h.From, h.To)
		if len(h.Findings) > 0 {
			b.WriteString("| Severity | Check | Resource | Finding | Fix |\n|---|---|---|---|---|\n")
			for _, f := range h.Findings {
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
					severityBadge(f.Severity), f.CheckID, md(f.Resource), md(f.Title), md(f.Remediation))
			}
			b.WriteString("\n")
		}
		for j, s := range h.Steps {
			fmt.Fprintf(&b, "%d. **%s**: %s", j+1, s.Phase, md(s.Title))
			if s.Detail != "" {
				fmt.Fprintf(&b, "<br>%s", md(s.Detail))
			}
			b.WriteString("\n")
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func severityBadge(s check.Severity) string {
	switch s {
	case check.SeverityBlocker:
		return "**BLOCKER**"
	case check.SeverityWarning:
		return "warning"
	}
	return "info"
}

func md(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", " ")
}

// supportLine summarises the support window, e.g. "1.30 standard support ends 2026-07-23 (in 30 days)".
func supportLine(p *plan.Plan) string {
	a := p.Support
	if a == nil || a.CurrentStatus == "" {
		return ""
	}
	var s string
	switch {
	case a.CurrentStatus == "extended-support":
		s = fmt.Sprintf("%s is in extended support (+$%.0f/year)", a.Current, a.CurrentSurchargePerYearUSD)
		if a.EndOfExtended != nil {
			s += ", ends " + a.EndOfExtended.UTC().Format("2006-01-02")
		}
	case a.CurrentStatus == "unsupported":
		s = fmt.Sprintf("%s is unsupported", a.Current)
	case a.EndOfStandard != nil && a.DaysToEndOfStandard != nil:
		s = fmt.Sprintf("%s standard support ends %s (in %d days)", a.Current, a.EndOfStandard.UTC().Format("2006-01-02"), *a.DaysToEndOfStandard)
	default:
		s = fmt.Sprintf("%s: %s", a.Current, a.CurrentStatus)
	}
	switch {
	case a.TargetStatus == support.StatusExtended:
		s += fmt.Sprintf("; %s is also in extended support", a.Target)
		if a.FirstStandard != nil {
			s += fmt.Sprintf(" (standard pricing from %s)", *a.FirstStandard)
		}
	case a.TargetEndOfStandard != nil:
		s += fmt.Sprintf("; %s supported until %s", a.Target, a.TargetEndOfStandard.UTC().Format("2006-01-02"))
	}
	return s
}
