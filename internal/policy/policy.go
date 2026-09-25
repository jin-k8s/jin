// Package policy decides who may approve an upgrade hop, how many approvals it needs and when.
package policy

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
)

type Window struct {
	// Days are three-letter English weekday names (Mon..Sun). Empty means every day.
	Days     []string `json:"days,omitempty"`
	Start    string   `json:"start"` // HH:MM
	End      string   `json:"end"`   // HH:MM, exclusive; may be earlier than Start to cross midnight
	Timezone string   `json:"timezone,omitempty"`
}

type Match struct {
	Environments []string `json:"environments,omitempty"`
	// Contexts are glob patterns (path.Match syntax) on the kubeconfig context name.
	Contexts []string `json:"contexts,omitempty"`
}

type Policy struct {
	Name               string   `json:"name"`
	Match              Match    `json:"match"`
	MinApprovals       int      `json:"minApprovals,omitempty"`
	ForbidSelfApproval bool     `json:"forbidSelfApproval,omitempty"`
	RequireComment     bool     `json:"requireComment,omitempty"`
	RequiredGroups     []string `json:"requiredGroups,omitempty"`
	Windows            []Window `json:"windows,omitempty"`
}

// Default applies when no policy matches: one approval by anyone with the approver role.
var Default = Policy{Name: "default", MinApprovals: 1}

func (p Policy) Required() int {
	if p.MinApprovals < 1 {
		return 1
	}
	return p.MinApprovals
}

func (p Policy) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("policy needs a name")
	}
	for _, g := range p.Match.Contexts {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("policy %s: bad context pattern %q", p.Name, g)
		}
	}
	for _, w := range p.Windows {
		if _, err := w.location(); err != nil {
			return fmt.Errorf("policy %s: %w", p.Name, err)
		}
		if _, err := minutes(w.Start); err != nil {
			return fmt.Errorf("policy %s: %w", p.Name, err)
		}
		if _, err := minutes(w.End); err != nil {
			return fmt.Errorf("policy %s: %w", p.Name, err)
		}
		for _, d := range w.Days {
			if _, ok := weekdays[strings.ToLower(d)]; !ok {
				return fmt.Errorf("policy %s: unknown day %q", p.Name, d)
			}
		}
	}
	return nil
}

func (p Policy) Matches(context, environment string) bool {
	if len(p.Match.Environments) > 0 && !slices.Contains(p.Match.Environments, environment) {
		return false
	}
	if len(p.Match.Contexts) > 0 {
		for _, g := range p.Match.Contexts {
			if ok, _ := path.Match(g, context); ok {
				return true
			}
		}
		return false
	}
	return true
}

// For returns the first matching policy, or Default.
func For(ps []Policy, context, environment string) Policy {
	for _, p := range ps {
		if p.Matches(context, environment) {
			return p
		}
	}
	return Default
}

type Approver struct {
	Subject string
	Groups  []string
}

// Check validates one approval against the policy; prior are subjects that already approved this hop.
func (p Policy) Check(a Approver, creator, comment string, prior []string, now time.Time) error {
	if p.ForbidSelfApproval && a.Subject == creator {
		return fmt.Errorf("policy %q forbids approving an upgrade you started", p.Name)
	}
	if slices.Contains(prior, a.Subject) {
		return fmt.Errorf("you already approved this hop; policy %q needs %d different approvers", p.Name, p.Required())
	}
	if p.RequireComment && strings.TrimSpace(comment) == "" {
		return fmt.Errorf("policy %q requires a comment (for example a change ticket)", p.Name)
	}
	if len(p.RequiredGroups) > 0 {
		ok := false
		for _, g := range a.Groups {
			if slices.Contains(p.RequiredGroups, g) {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("policy %q requires an approver in %s", p.Name, strings.Join(p.RequiredGroups, " or "))
		}
	}
	if !p.InWindow(now) {
		return fmt.Errorf("policy %q only allows upgrades during %s", p.Name, p.DescribeWindows())
	}
	return nil
}

func (p Policy) InWindow(now time.Time) bool {
	if len(p.Windows) == 0 {
		return true
	}
	for _, w := range p.Windows {
		if w.contains(now) {
			return true
		}
	}
	return false
}

func (p Policy) DescribeWindows() string {
	if len(p.Windows) == 0 {
		return "any time"
	}
	var parts []string
	for _, w := range p.Windows {
		days := "every day"
		if len(w.Days) > 0 {
			days = strings.Join(w.Days, ", ")
		}
		tz := w.Timezone
		if tz == "" {
			tz = "UTC"
		}
		parts = append(parts, fmt.Sprintf("%s %s-%s %s", days, w.Start, w.End, tz))
	}
	return strings.Join(parts, "; ")
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

func (w Window) location() (*time.Location, error) {
	if w.Timezone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(w.Timezone)
}

func minutes(hhmm string) (int, error) {
	t, err := time.Parse("15:04", hhmm)
	if err != nil {
		return 0, fmt.Errorf("bad time %q (HH:MM)", hhmm)
	}
	return t.Hour()*60 + t.Minute(), nil
}

func (w Window) contains(now time.Time) bool {
	loc, err := w.location()
	if err != nil {
		return false
	}
	local := now.In(loc)
	start, err1 := minutes(w.Start)
	end, err2 := minutes(w.End)
	if err1 != nil || err2 != nil {
		return false
	}
	m := local.Hour()*60 + local.Minute()
	day := local.Weekday()
	if start > end && m < end {
		// Early-morning part of a window that started the previous day.
		day = (day + 6) % 7
	}
	if len(w.Days) > 0 {
		ok := false
		for _, d := range w.Days {
			if weekdays[strings.ToLower(d)] == day {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if start <= end {
		return m >= start && m < end
	}
	return m >= start || m < end
}
