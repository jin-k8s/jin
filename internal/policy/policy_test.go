package policy

import (
	"strings"
	"testing"
	"time"
)

func TestMatchAndChecks(t *testing.T) {
	prod := Policy{Name: "production", Match: Match{Environments: []string{"prod"}}, MinApprovals: 2, ForbidSelfApproval: true, RequireComment: true, RequiredGroups: []string{"sre"}}
	ps := []Policy{prod, {Name: "payments", Match: Match{Contexts: []string{"arn:aws:eks:*:cluster/payments-*"}}}}

	if p := For(ps, "anything", "prod"); p.Name != "production" || p.Required() != 2 {
		t.Fatalf("env match: %+v", p)
	}
	if p := For(ps, "arn:aws:eks:ap-south-1:1:cluster/payments-dev", "dev"); p.Name != "payments" || p.Required() != 1 {
		t.Fatalf("glob match: %+v", p)
	}
	if p := For(ps, "kind-x", ""); p.Name != "default" {
		t.Fatalf("default: %+v", p)
	}

	now := time.Now()
	alice := Approver{Subject: "alice@x", Groups: []string{"sre"}}
	cases := map[string]error{
		"self":    prod.Check(alice, "alice@x", "CHG-1", nil, now),
		"repeat":  prod.Check(alice, "bob@x", "CHG-1", []string{"alice@x"}, now),
		"comment": prod.Check(alice, "bob@x", " ", nil, now),
		"group":   prod.Check(Approver{Subject: "carol@x", Groups: []string{"dev"}}, "bob@x", "CHG-1", nil, now),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	if err := prod.Check(alice, "bob@x", "CHG-1", nil, now); err != nil {
		t.Fatalf("valid approval rejected: %v", err)
	}
}

func TestWindows(t *testing.T) {
	p := Policy{Name: "w", Windows: []Window{
		{Days: []string{"Tue", "Wed"}, Start: "09:00", End: "16:00", Timezone: "Asia/Kolkata"},
		{Days: []string{"Sat"}, Start: "22:00", End: "02:00"},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	at := func(s string, loc *time.Location) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", s, loc)
		return v
	}
	checks := []struct {
		t    time.Time
		want bool
	}{
		{at("2026-09-22 10:00", ist), true},       // Tue 10:00 IST
		{at("2026-09-22 16:00", ist), false},      // end is exclusive
		{at("2026-09-24 10:00", ist), false},      // Thursday
		{at("2026-09-26 23:30", time.UTC), true},  // Sat night
		{at("2026-09-27 01:30", time.UTC), true},  // Sun early morning belongs to Saturday's window
		{at("2026-09-28 01:30", time.UTC), false}, // Mon early morning
	}
	for _, c := range checks {
		if got := p.InWindow(c.t); got != c.want {
			t.Errorf("%s: got %v want %v", c.t, got, c.want)
		}
	}
	if err := p.Check(Approver{Subject: "a"}, "b", "", nil, at("2026-09-24 10:00", ist)); err == nil || !strings.Contains(err.Error(), "Tue, Wed 09:00-16:00 Asia/Kolkata") {
		t.Fatalf("window error must describe the window: %v", err)
	}
	if err := (Policy{Name: "bad", Windows: []Window{{Start: "25:00", End: "01:00"}}}).Validate(); err == nil {
		t.Fatal("invalid time must fail validation")
	}
}
