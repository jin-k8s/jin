package report

import (
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/runrecord"
)

const (
	defaultWidth = 100
	maxWidth     = 120
	labelWidth   = 10
	sevWidth     = 11
)

// theme adapts to the writer: colors on a TTY, plain text in CI, pipes and when NO_COLOR is set.
type theme struct {
	width                          int
	title, label, dim, text, fix   lipgloss.Style
	blocker, warning, info, ok     lipgloss.Style
	badgeBlocked, badgeReady, rule lipgloss.Style
	hop, phase, num                lipgloss.Style
}

func newTheme(w io.Writer) theme {
	r := lipgloss.NewRenderer(w)
	width := defaultWidth
	if f, ok := w.(*os.File); ok {
		if cols, _, err := term.GetSize(int(f.Fd())); err == nil && cols > 40 {
			width = min(cols, maxWidth)
		}
	}
	c := func(light, dark string) lipgloss.AdaptiveColor {
		return lipgloss.AdaptiveColor{Light: light, Dark: dark}
	}
	red, amber, blue, green := c("#B91C1C", "#F87171"), c("#B45309", "#FBBF24"), c("#1D4ED8", "#60A5FA"), c("#047857", "#34D399")
	muted := c("#64748B", "#94A3B8")
	return theme{
		width:        width,
		title:        r.NewStyle().Bold(true).Foreground(c("#4338CA", "#A5B4FC")),
		label:        r.NewStyle().Foreground(muted).Width(labelWidth),
		dim:          r.NewStyle().Foreground(muted),
		text:         r.NewStyle(),
		fix:          r.NewStyle().Foreground(green),
		blocker:      r.NewStyle().Bold(true).Foreground(red),
		warning:      r.NewStyle().Bold(true).Foreground(amber),
		info:         r.NewStyle().Foreground(blue),
		ok:           r.NewStyle().Bold(true).Foreground(green),
		badgeBlocked: r.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#DC2626")).Padding(0, 1),
		badgeReady:   r.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#059669")).Padding(0, 1),
		rule:         r.NewStyle().Foreground(c("#CBD5E1", "#334155")),
		hop:          r.NewStyle().Bold(true).Foreground(c("#0F766E", "#5EEAD4")),
		phase:        r.NewStyle().Foreground(c("#6D28D9", "#C4B5FD")).Width(15),
		num:          r.NewStyle().Foreground(muted).Width(3).Align(lipgloss.Right),
	}
}

// wrap renders s wrapped to the remaining width and indents continuation lines.
func (t theme) wrap(st lipgloss.Style, s string, indent int) string {
	w := t.width - indent - 2
	if w < 30 {
		w = 30
	}
	lines := strings.Split(st.Width(w).Render(s), "\n")
	pad := strings.Repeat(" ", indent)
	for i := 1; i < len(lines); i++ {
		lines[i] = pad + strings.TrimRight(lines[i], " ")
	}
	lines[0] = strings.TrimRight(lines[0], " ")
	return strings.Join(lines, "\n")
}

func (t theme) severity(s check.Severity) string {
	switch s {
	case check.SeverityBlocker:
		return t.blocker.Width(sevWidth).Render("✗ BLOCKER")
	case check.SeverityWarning:
		return t.warning.Width(sevWidth).Render("! WARNING")
	}
	return t.info.Width(sevWidth).Render("i INFO")
}

func renderText(w io.Writer, r *runrecord.Record) error {
	t := newTheme(w)
	var b strings.Builder
	line := func(s ...string) { b.WriteString(strings.Join(s, "") + "\n") }
	field := func(k, v string) { line("  ", t.label.Render(k), v) }
	rule := t.rule.Render(strings.Repeat("─", t.width-4))

	line()
	line("  ", t.title.Render("jin"), t.dim.Render("  ·  upgrade plan"))
	line("  ", rule)

	p := r.Plan
	cluster := r.Cluster.Context
	if p != nil && p.Provider != "" {
		cluster += t.dim.Render("  ·  " + p.Provider)
	}
	field("Cluster", cluster)
	if p == nil {
		field("Result", t.badgeBlocked.Render("FAILED")+"  "+t.wrap(t.text, r.Error, 2+labelWidth+10))
		field("Run", t.dim.Render(r.ID))
		line()
		_, err := io.WriteString(w, b.String())
		return err
	}

	field("Upgrade", p.Current.String()+" → "+p.Target.String()+t.dim.Render("  ·  "+plural(len(p.Hops), "hop")))
	badge := t.badgeReady.Render(verdict(p))
	if !p.Ready {
		badge = t.badgeBlocked.Render(verdict(p))
	}
	field("Result", badge+"  "+counts(p))
	if line := supportLine(p); line != "" {
		field("Support", line)
	}
	field("Run", t.dim.Render(r.ID))

	if len(p.CollectionWarnings) > 0 {
		line()
		line("  ", t.warning.Render("! Data collection incomplete: findings may be missing"))
		for _, cw := range p.CollectionWarnings {
			line("    ", t.dim.Render("• "), t.wrap(t.dim, cw, 6))
		}
	}

	findingIndent := 2 + sevWidth + 1
	for i, h := range p.Hops {
		line()
		head := t.hop.Render("HOP "+strconv.Itoa(i+1)) + "  " + h.From.String() + " → " + h.To.String() + "  "
		line("  ", head, t.rule.Render(strings.Repeat("─", max(4, t.width-4-lipgloss.Width(head)))))
		line()

		if len(h.Findings) == 0 {
			line("  ", t.ok.Render("✓ No findings for this hop"))
		}
		for _, f := range h.Findings {
			line("  ", t.severity(f.Severity), " ", t.wrap(t.text.Bold(true), f.Title, findingIndent))
			pad := strings.Repeat(" ", findingIndent)
			if f.Resource != "" {
				line(pad, t.wrap(t.text, f.Resource, findingIndent))
			}
			if f.Detail != "" {
				line(pad, t.wrap(t.dim, f.Detail, findingIndent))
			}
			if f.Remediation != "" {
				line(pad, t.fix.Render("→ "), t.wrap(t.fix, f.Remediation, findingIndent+2))
			}
			line()
		}

		line("  ", t.dim.Render("Steps"))
		stepIndent := 2 + 3 + 2 + 15
		for j, s := range h.Steps {
			line("  ", t.num.Render(strconv.Itoa(j+1)), "  ", t.phase.Render(s.Phase), t.wrap(t.text, s.Title, stepIndent))
			if s.Detail != "" {
				line(strings.Repeat(" ", stepIndent), t.wrap(t.dim, s.Detail, stepIndent))
			}
		}
	}

	line()
	line("  ", rule)
	if p.Ready {
		line("  ", t.ok.Render("✓ No blockers."), t.dim.Render(" Start the upgrade from the UI: jin server"))
	} else {
		line("  ", t.dim.Render("Resolve the blockers, then re-run "), "jin plan", t.dim.Render(". Share this plan: "), "jin runs show "+r.ID+" -o markdown")
	}
	line()
	_, err := io.WriteString(w, b.String())
	return err
}
