package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/audit"
	"github.com/jin-k8s/jin/internal/clusters"
	"github.com/jin-k8s/jin/internal/gitops"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/upgrade"
)

type liveVersion struct {
	version string
	nodes   int
	err     string
	at      time.Time
}

type fleetRow struct {
	app.Context
	Provider      string         `json:"provider"`
	Version       string         `json:"version,omitempty"`
	Nodes         int            `json:"nodes"`
	Reachable     bool           `json:"reachable"`
	Error         string         `json:"error,omitempty"`
	LastPlan      *runSummary    `json:"lastPlan,omitempty"`
	ActiveUpgrade *activeUpgrade `json:"activeUpgrade,omitempty"`
	Policy        string         `json:"policy"`
}

type activeUpgrade struct {
	ID         string         `json:"id"`
	Status     upgrade.Status `json:"status"`
	From       kube.Version   `json:"from"`
	To         kube.Version   `json:"to"`
	CurrentHop int            `json:"currentHop"`
	Hops       int            `json:"hops"`
}

// fleet aggregates every context: live version (cached for a minute), last plan, active upgrade.
func (s *Server) fleet(w http.ResponseWriter, r *http.Request) {
	ctxs, err := s.cfg.Env.Contexts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	runs, _ := s.cfg.Env.Runs.List()
	ups, _ := s.cfg.Engine.List()
	live := s.liveVersions(r.Context(), ctxs, r.URL.Query().Get("refresh") == "1")

	rows := make([]fleetRow, 0, len(ctxs))
	for _, c := range ctxs {
		row := fleetRow{Context: c, Provider: c.Provider(), Policy: policyName(s, c)}
		if lv := live[c.Name]; lv.err == "" {
			row.Version, row.Nodes, row.Reachable = lv.version, lv.nodes, true
		} else {
			row.Error = lv.err
		}
		for _, rec := range runs {
			if rec.Cluster.Context == c.Name {
				sum := summarize(rec)
				row.LastPlan = &sum
				if row.Provider == "" {
					row.Provider = rec.Cluster.Provider
				}
				break
			}
		}
		for _, u := range ups {
			if u.Cluster.Context == c.Name && u.Status.Active() {
				row.ActiveUpgrade = &activeUpgrade{ID: u.ID, Status: u.Status, From: u.From, To: u.To, CurrentHop: u.CurrentHop, Hops: len(u.Hops)}
				break
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, rows)
}

func policyName(s *Server, c app.Context) string {
	for _, p := range s.cfg.Cfg.Policies {
		if p.Matches(c.Name, c.Environment) {
			return p.Name
		}
	}
	return "default"
}

func (s *Server) liveVersions(ctx context.Context, ctxs []app.Context, refresh bool) map[string]liveVersion {
	out := map[string]liveVersion{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, c := range ctxs {
		s.fleetMu.Lock()
		cached, ok := s.fleetCache[c.Name]
		s.fleetMu.Unlock()
		if ok && !refresh && time.Since(cached.at) < time.Minute {
			out[c.Name] = cached
			continue
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			lv := liveVersion{at: time.Now()}
			cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			if clients, err := s.cfg.Env.Connect(name); err != nil {
				lv.err = err.Error()
			} else if sv, err := discoverVersion(cctx, clients); err != nil {
				lv.err = err.Error()
			} else {
				lv.version = sv.version
				lv.nodes = sv.nodes
			}
			mu.Lock()
			out[name] = lv
			mu.Unlock()
			s.fleetMu.Lock()
			s.fleetCache[name] = lv
			s.fleetMu.Unlock()
		}(c.Name)
	}
	wg.Wait()
	return out
}

type versionInfo struct {
	version string
	nodes   int
}

func discoverVersion(ctx context.Context, c *app.Clients) (versionInfo, error) {
	type res struct {
		v   versionInfo
		err error
	}
	ch := make(chan res, 1)
	go func() {
		sv, err := c.Kube.Discovery().ServerVersion()
		if err != nil {
			ch <- res{err: err}
			return
		}
		v, _ := kube.ParseVersion(sv.GitVersion)
		info := versionInfo{version: v.String()}
		if nodes, err := c.Kube.CoreV1().Nodes().List(ctx, listLimit(500)); err == nil {
			info.nodes = len(nodes.Items)
		}
		ch <- res{v: info}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		return versionInfo{}, fmt.Errorf("unreachable (timed out)")
	}
}

func (s *Server) policies(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		policyView
		Match map[string][]string `json:"match"`
	}
	out := []view{}
	for _, p := range s.cfg.Cfg.Policies {
		pv := withPolicy(&upgrade.Upgrade{}, p).Policy
		out = append(out, view{policyView: pv, Match: map[string][]string{"environments": p.Match.Environments, "contexts": p.Match.Contexts}})
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out, "default": withPolicy(&upgrade.Upgrade{}, policyDefault()).Policy})
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("context")
	if name == "" {
		writeError(w, http.StatusBadRequest, "context is required")
		return
	}
	st, err := s.cfg.Env.Settings.Get(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"settings": st}
	if st.GitOps != nil {
		tok, source := s.cfg.Env.GitHubToken(st.GitOps)
		resp["tokenPresent"] = tok != ""
		resp["tokenSource"] = source
	}
	_, resp["registered"] = s.cfg.Env.Settings.Registration(name)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var st clusters.Settings
	if !decode(w, r, &st) {
		return
	}
	if st.Context == "" {
		writeError(w, http.StatusBadRequest, "context is required")
		return
	}
	if st.GitOps != nil {
		if err := s.cfg.Cfg.AllowedGitHubURL(st.GitOps.BaseURL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.cfg.Env.Settings.Put(&st); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, _ := json.Marshal(st)
	s.record(r, "settings.update", st.Context, string(b))
	writeJSON(w, http.StatusOK, st)
}

// discover scans a repository for Kubernetes version fields and ranks them for the cluster.
func (s *Server) discover(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Context string          `json:"context"`
		GitOps  clusters.GitOps `json:"gitops"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.GitOps.Provider = "github"
	if err := (&clusters.Settings{Context: req.Context, GitOps: &req.GitOps}).Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.cfg.Cfg.AllowedGitHubURL(req.GitOps.BaseURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	token, _ := s.cfg.Env.GitHubToken(&req.GitOps)
	if token == "" {
		writeError(w, http.StatusBadRequest, "no GitHub token: add one under Integrations, or set "+orDefault(req.GitOps.TokenEnv, "GITHUB_TOKEN")+" on the Jin server")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	repo := &gitops.GitHub{BaseURL: req.GitOps.BaseURL, Owner: req.GitOps.Owner, Name: req.GitOps.Repo, Token: token}
	branch := req.GitOps.BaseBranch
	if branch == "" {
		b, err := repo.DefaultBranch(ctx)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		branch = b
	}
	sha, err := repo.HeadSHA(ctx, branch)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	ws, err := gitops.NewRepoWorkspace(ctx, repo, sha)
	if ws == nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	var warnings []string
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	var files []string
	for _, f := range ws.Files() {
		if strings.Contains(f, ".terraform/") || strings.Contains(f, "vendor/") || strings.Contains(f, "node_modules/") {
			continue
		}
		if strings.HasSuffix(f, ".tf") || strings.HasSuffix(f, ".yaml") || strings.HasSuffix(f, ".yml") {
			files = append(files, f)
		}
	}
	const maxFiles = 400
	if len(files) > maxFiles {
		warnings = append(warnings, fmt.Sprintf("scanned the first %d of %d candidate files; add remaining targets manually", maxFiles, len(files)))
		files = files[:maxFiles]
	}
	cands := gitops.Discover(ws, files)

	hint := ""
	if refs, err := s.cfg.Env.ClusterRef(req.Context, ""); err == nil {
		switch {
		case refs.EKS != nil:
			hint = refs.EKS.Name
		case refs.GKE != nil:
			hint = refs.GKE.Name
		case refs.AKS != nil:
			hint = refs.AKS.Name
		}
	}
	type ranked struct {
		gitops.Candidate
		Suggested bool `json:"suggested"`
	}
	out := make([]ranked, 0, len(cands))
	for _, c := range cands {
		out = append(out, ranked{Candidate: c, Suggested: hint != "" && c.Error == "" && strings.EqualFold(c.Cluster, hint)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Suggested && !out[j].Suggested })
	s.record(r, "settings.discover", req.Context, fmt.Sprintf("%s/%s@%s: %d candidates", req.GitOps.Owner, req.GitOps.Repo, branch, len(out)))
	writeJSON(w, http.StatusOK, map[string]any{"branch": branch, "candidates": out, "warnings": warnings, "clusterHint": hint})
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *Server) auditExport(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		since = t
	}
	entries, err := s.cfg.Audit.Entries(since)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, "audit.export", "", r.URL.Query().Get("format"))
	switch r.URL.Query().Get("format") {
	case "csv":
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="jin-audit.csv"`)
		_ = audit.WriteCSV(w, entries)
	case "jsonl":
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="jin-audit.jsonl"`)
		enc := json.NewEncoder(w)
		for _, e := range entries {
			_ = enc.Encode(e)
		}
	default:
		if entries == nil {
			entries = []audit.Entry{}
		}
		writeJSON(w, http.StatusOK, entries)
	}
}

func (s *Server) auditVerify(w http.ResponseWriter, _ *http.Request) {
	ok, at, err := s.cfg.Audit.Verify()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"intact": ok, "brokenAt": at})
}
