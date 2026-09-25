package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/buildinfo"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/policy"
	"github.com/jin-k8s/jin/internal/runrecord"
	"github.com/jin-k8s/jin/internal/support"
	"github.com/jin-k8s/jin/internal/upgrade"
)

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"version": buildinfo.Version, "commit": buildinfo.Commit,
		"modes": []string{upgrade.ModeDirect, upgrade.ModeGitOps},
	})
}

func (s *Server) contexts(w http.ResponseWriter, _ *http.Request) {
	ctxs, err := s.cfg.Env.Contexts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ctxs)
}

type runSummary struct {
	ID         string              `json:"id"`
	StartedAt  time.Time           `json:"startedAt"`
	Status     runrecord.Status    `json:"status"`
	Error      string              `json:"error,omitempty"`
	Context    string              `json:"context"`
	Provider   string              `json:"provider,omitempty"`
	Current    kube.Version        `json:"current"`
	Target     kube.Version        `json:"target"`
	Hops       int                 `json:"hops"`
	Blockers   int                 `json:"blockers"`
	Warnings   int                 `json:"warnings"`
	Ready      bool                `json:"ready"`
	Incomplete bool                `json:"incomplete"`
	Support    *support.Assessment `json:"support,omitempty"`
}

func summarize(r *runrecord.Record) runSummary {
	sum := runSummary{ID: r.ID, StartedAt: r.StartedAt, Status: r.Status, Error: r.Error, Context: r.Cluster.Context, Provider: r.Cluster.Provider}
	if p := r.Plan; p != nil {
		sum.Current, sum.Target, sum.Hops = p.Current, p.Target, len(p.Hops)
		sum.Blockers, sum.Warnings, sum.Ready, sum.Incomplete = p.Summary.Blockers, p.Summary.Warnings, p.Ready, !p.Complete
		sum.Support = p.Support
	}
	return sum
}

func (s *Server) listRuns(w http.ResponseWriter, _ *http.Request) {
	recs, err := s.cfg.Env.Runs.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]runSummary, 0, len(recs))
	for _, r := range recs {
		out = append(out, summarize(r))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	rec, err := s.cfg.Env.Runs.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Context string `json:"context"`
		Target  string `json:"target"`
	}
	if !decode(w, r, &req) {
		return
	}
	var target kube.Version
	if req.Target != "" {
		v, err := kube.ParseVersion(req.Target)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		target = v
	}
	clients, err := s.cfg.Env.Connect(req.Context)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	select {
	case s.planSem <- struct{}{}:
		defer func() { <-s.planSem }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.PlanTimeout)
	defer cancel()
	rec, err := s.cfg.Env.Plan(ctx, clients, app.PlanOptions{Target: target, Record: true})
	if err != nil && rec.Status == runrecord.StatusSucceeded {
		writeError(w, http.StatusInternalServerError, err.Error()) // plan built but not persisted
		return
	}
	s.record(r, "plan.create", clients.Context, string(rec.Status)+" "+rec.ID)
	// Failed inspections are recorded runs too; the record carries the error.
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) listUpgrades(w http.ResponseWriter, _ *http.Request) {
	ups, err := s.cfg.Engine.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ups == nil {
		ups = []*upgrade.Upgrade{}
	}
	writeJSON(w, http.StatusOK, ups)
}

func (s *Server) createUpgrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID string `json:"runId"`
		Mode  string `json:"mode"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Mode == "" {
		req.Mode = upgrade.ModeDirect
	}
	rec, err := s.cfg.Env.Runs.Get(req.RunID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	ref, err := s.cfg.Env.ClusterRef(rec.Cluster.Context, rec.Cluster.Provider)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id := identity(r)
	u, err := s.cfg.Engine.Create(r.Context(), upgrade.CreateRequest{Run: rec, Mode: req.Mode, Cluster: ref, By: id.Display(), Subject: id.Subject})
	if err != nil {
		writeEngineError(w, err)
		return
	}
	s.record(r, "upgrade.create", u.ID, fmt.Sprintf("%s %s→%s %s mode", u.Cluster.Context, u.From, u.To, u.Mode))
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) getUpgrade(w http.ResponseWriter, r *http.Request) {
	u, err := s.cfg.Engine.Get(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, withPolicy(u, s.policyFor(u)))
}

type upgradeView struct {
	*upgrade.Upgrade
	Policy policyView `json:"policy"`
}

type policyView struct {
	Name               string   `json:"name"`
	RequiredApprovals  int      `json:"requiredApprovals"`
	ForbidSelfApproval bool     `json:"forbidSelfApproval"`
	RequireComment     bool     `json:"requireComment"`
	RequiredGroups     []string `json:"requiredGroups,omitempty"`
	Windows            string   `json:"windows"`
	InWindow           bool     `json:"inWindow"`
}

func (s *Server) policyFor(u *upgrade.Upgrade) policy.Policy {
	return policy.For(s.cfg.Cfg.Policies, u.Cluster.Context, u.Cluster.Environment)
}

func withPolicy(u *upgrade.Upgrade, p policy.Policy) upgradeView {
	return upgradeView{Upgrade: u, Policy: policyView{
		Name: p.Name, RequiredApprovals: p.Required(), ForbidSelfApproval: p.ForbidSelfApproval, RequireComment: p.RequireComment,
		RequiredGroups: p.RequiredGroups, Windows: p.DescribeWindows(), InWindow: p.InWindow(time.Now()),
	}}
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hop              int    `json:"hop"`
		Comment          string `json:"comment"`
		OverrideBlockers bool   `json:"overrideBlockers"`
		// Confirm must equal the hop's target version: approving is irreversible.
		Confirm string `json:"confirm"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	u, err := s.cfg.Engine.Get(id)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if req.Hop < 1 || req.Hop > len(u.Hops) {
		writeError(w, http.StatusBadRequest, "invalid hop")
		return
	}
	hop := u.Hops[req.Hop-1]
	if want := hop.To.String(); strings.TrimSpace(req.Confirm) != want {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("type the target version %q to confirm this irreversible step", want))
		return
	}
	if req.OverrideBlockers && strings.TrimSpace(req.Comment) == "" {
		writeError(w, http.StatusBadRequest, "overriding blockers requires a justification comment")
		return
	}
	who := identity(r)
	pol := s.policyFor(u)
	if err := pol.Check(policy.Approver{Subject: who.Subject, Groups: who.Groups}, u.CreatedBySubject, req.Comment, hop.Approvers(), s.cfg.Now()); err != nil {
		s.record(r, "upgrade.approve.denied", u.ID, err.Error())
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	u, err = s.cfg.Engine.Approve(id, upgrade.ApproveRequest{
		Hop: req.Hop, By: who.Display(), Subject: who.Subject, Comment: req.Comment, OverrideBlockers: req.OverrideBlockers,
		Required: pol.Required(), Policy: pol.Name,
	})
	if err != nil {
		writeEngineError(w, err)
		return
	}
	detail := fmt.Sprintf("hop %d %s→%s policy %s", req.Hop, hop.From, hop.To, pol.Name)
	if req.OverrideBlockers {
		detail += " override-blockers"
	}
	s.record(r, "upgrade.approve", u.ID, detail)
	writeJSON(w, http.StatusOK, withPolicy(u, pol))
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Comment string `json:"comment"`
	}
	if !decode(w, r, &req) {
		return
	}
	u, err := s.cfg.Engine.Get(r.PathValue("id"))
	if err != nil {
		writeEngineError(w, err)
		return
	}
	if pol := s.policyFor(u); !pol.InWindow(s.cfg.Now()) {
		writeError(w, http.StatusForbidden, fmt.Sprintf("policy %q only allows upgrades during %s", pol.Name, pol.DescribeWindows()))
		return
	}
	u, err = s.cfg.Engine.Retry(u.ID, identity(r).Display(), req.Comment)
	if err != nil {
		writeEngineError(w, err)
		return
	}
	s.record(r, "upgrade.retry", u.ID, req.Comment)
	writeJSON(w, http.StatusOK, withPolicy(u, s.policyFor(u)))
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	u, err := s.cfg.Engine.Cancel(r.PathValue("id"), identity(r).Display())
	if err != nil {
		writeEngineError(w, err)
		return
	}
	s.record(r, "upgrade.cancel", u.ID, "")
	writeJSON(w, http.StatusOK, withPolicy(u, s.policyFor(u)))
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	id := r.PathValue("id")
	if _, err := s.cfg.Engine.Get(id); err != nil {
		writeEngineError(w, err)
		return
	}
	evs, err := s.cfg.Engine.Events(id, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if evs == nil {
		evs = []upgrade.Event{}
	}
	writeJSON(w, http.StatusOK, evs)
}

// stream serves Server-Sent Events. It subscribes before reading the backlog so no event is lost,
// and honours Last-Event-ID so reconnecting browsers resume where they left off.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.cfg.Engine.Get(id); err != nil {
		writeEngineError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if q := r.URL.Query().Get("after"); q != "" && after == 0 {
		after, _ = strconv.ParseInt(q, 10, 64)
	}

	ch, unsub := s.cfg.Engine.Subscribe(id)
	defer unsub()
	backlog, err := s.cfg.Engine.Events(id, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(ev upgrade.Event) bool {
		b, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "id: %d\nevent: upgrade\ndata: %s\n\n", ev.Seq, b); err != nil {
			return false
		}
		after = ev.Seq
		return true
	}
	for _, ev := range backlog {
		if !send(ev) {
			return
		}
	}
	flusher.Flush()

	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return // fell behind; the browser reconnects with Last-Event-ID
			}
			if ev.Seq <= after {
				continue
			}
			if !send(ev) {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
