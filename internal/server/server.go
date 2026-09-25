// Package server exposes Jin's HTTP API, live event streams and the embedded web UI.
package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/audit"
	"github.com/jin-k8s/jin/internal/auth"
	"github.com/jin-k8s/jin/internal/config"
	"github.com/jin-k8s/jin/internal/upgrade"
)

//go:embed all:ui/dist
var embedded embed.FS

const (
	csrfHeader = "X-Jin-Request"
	maxBody    = 256 << 10
)

type Config struct {
	Env    *app.Env
	Engine *upgrade.Engine
	Auth   *auth.Manager
	Cfg    *config.Config
	Audit  *audit.Log
	// LoopbackOnly rejects requests whose Host is not a loopback name (DNS-rebinding defence).
	LoopbackOnly bool
	PlanTimeout  time.Duration
	// UI overrides the embedded assets (tests).
	UI  fs.FS
	Now func() time.Time
	// GitHubAPI is where stored tokens are validated. Defaults to https://api.github.com.
	GitHubAPI string
}

type Server struct {
	cfg     Config
	ui      fs.FS
	planSem chan struct{}

	fleetMu    sync.Mutex
	fleetCache map[string]liveVersion
}

func New(cfg Config) (*Server, error) {
	if cfg.Auth == nil || cfg.Cfg == nil || cfg.Audit == nil {
		return nil, errors.New("auth, config and audit are required")
	}
	if cfg.PlanTimeout == 0 {
		cfg.PlanTimeout = 5 * time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.GitHubAPI == "" {
		cfg.GitHubAPI = "https://api.github.com"
	}
	ui := cfg.UI
	if ui == nil {
		sub, err := fs.Sub(embedded, "ui/dist")
		if err != nil {
			return nil, err
		}
		ui = sub
	}
	return &Server{cfg: cfg, ui: ui, planSem: make(chan struct{}, 2), fleetCache: map[string]liveVersion{}}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	r := func(pattern string, role config.Role, h http.HandlerFunc) { mux.Handle(pattern, s.authorize(role, h)) }
	viewer, planner, approver, admin := config.RoleViewer, config.RolePlanner, config.RoleApprover, config.RoleAdmin

	mux.HandleFunc("GET /auth/login", s.cfg.Auth.Login)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("POST /auth/logout", s.logout)
	mux.HandleFunc("GET /api/v1/session", s.session)

	r("GET /api/v1/info", viewer, s.info)
	r("GET /api/v1/contexts", viewer, s.contexts)
	r("GET /api/v1/fleet", viewer, s.fleet)
	r("GET /api/v1/policies", viewer, s.policies)
	r("GET /api/v1/runs", viewer, s.listRuns)
	r("GET /api/v1/runs/{id}", viewer, s.getRun)
	r("GET /api/v1/upgrades", viewer, s.listUpgrades)
	r("GET /api/v1/upgrades/{id}", viewer, s.getUpgrade)
	r("GET /api/v1/upgrades/{id}/events", viewer, s.events)
	r("GET /api/v1/upgrades/{id}/stream", viewer, s.stream)
	r("GET /api/v1/settings", viewer, s.getSettings)

	r("POST /api/v1/plans", planner, s.createPlan)
	r("POST /api/v1/upgrades", planner, s.createUpgrade)
	r("POST /api/v1/compare", planner, s.compare)
	r("POST /api/v1/assess", planner, s.assess)

	r("POST /api/v1/upgrades/{id}/approve", approver, s.approve)
	r("POST /api/v1/upgrades/{id}/retry", approver, s.retry)
	r("POST /api/v1/upgrades/{id}/cancel", approver, s.cancel)

	r("GET /api/v1/integrations/github", admin, s.getGitHubIntegration)
	r("PUT /api/v1/integrations/github", admin, s.putGitHubIntegration)
	r("DELETE /api/v1/integrations/github", admin, s.deleteGitHubIntegration)
	r("GET /api/v1/aws/profiles", admin, s.awsProfiles)
	r("GET /api/v1/aws/eks-clusters", admin, s.eksClusters)
	r("POST /api/v1/clusters", admin, s.addCluster)
	r("DELETE /api/v1/clusters", admin, s.removeCluster)

	r("PUT /api/v1/settings", admin, s.putSettings)
	r("POST /api/v1/settings/discover", admin, s.discover)
	r("GET /api/v1/audit", admin, s.auditExport)
	r("GET /api/v1/audit/verify", admin, s.auditVerify)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/"):
			writeError(w, http.StatusNotFound, "unknown API endpoint")
		case r.Method != http.MethodGet && r.Method != http.MethodHead:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		default:
			s.static(w, r)
		}
	})
	return s.hostCheck(securityHeaders(mux))
}

// --- middleware -------------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.LoopbackOnly {
			host := r.Host
			if h, _, err := net.SplitHostPort(r.Host); err == nil {
				host = h
			}
			host = strings.Trim(host, "[]")
			if host != "localhost" && host != "127.0.0.1" && host != "::1" {
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type ctxKey struct{}

func identity(r *http.Request) *auth.Identity {
	id, _ := r.Context().Value(ctxKey{}).(*auth.Identity)
	return id
}

// authorize authenticates the caller, enforces the minimum role and, for cookie sessions, requires
// the custom header on writes. Browsers cannot send it cross-site without a CORS preflight, which
// is never granted, so it doubles as CSRF protection.
func (s *Server) authorize(min config.Role, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, bearer, err := s.cfg.Auth.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "sign in required: "+err.Error())
			return
		}
		if !bearer && r.Method != http.MethodGet && r.Header.Get(csrfHeader) != "1" {
			writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		if !id.Role.Allows(min) {
			writeError(w, http.StatusForbidden, fmt.Sprintf("the %s role is required; you have %s", min, id.Role))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

func (s *Server) record(r *http.Request, action, target, detail string) {
	e := audit.Entry{Action: action, Target: target, Detail: detail, Remote: remote(r)}
	if id := identity(r); id != nil {
		e.Actor, e.Role = id.Display(), string(id.Role)
	}
	_ = s.cfg.Audit.Record(e)
}

func remote(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// --- auth endpoints -----------------------------------------------------------

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"sso": s.cfg.Auth.SSOEnabled(), "tokenLogin": s.cfg.Auth.TokenLoginEnabled(), "authenticated": false}
	if id, _, err := s.cfg.Auth.Authenticate(r); err == nil {
		out["authenticated"] = true
		out["identity"] = map[string]any{"name": id.Name, "email": id.Email, "groups": id.Groups, "method": id.Method, "subject": id.Subject}
		out["role"] = id.Role
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	id, err := s.cfg.Auth.Callback(w, r)
	if err != nil {
		_ = s.cfg.Audit.Record(audit.Entry{Actor: "anonymous", Action: "login.failed", Detail: err.Error(), Remote: remote(r)})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `<!doctype html><title>Jin</title><p style="font-family:sans-serif">Sign-in failed: %s. <a href="/">Try again</a>.</p>`, html.EscapeString(err.Error())) //nolint:gosec // escaped above
		return
	}
	_ = s.cfg.Audit.Record(audit.Entry{Actor: id.Display(), Role: string(id.Role), Action: "login", Detail: "sso", Remote: remote(r)})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(csrfHeader) != "1" {
		writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	if id, _, err := s.cfg.Auth.Authenticate(r); err == nil {
		_ = s.cfg.Audit.Record(audit.Entry{Actor: id.Display(), Role: string(id.Role), Action: "logout", Remote: remote(r)})
	}
	s.cfg.Auth.Clear(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// --- static UI ----------------------------------------------------------------

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if t := r.URL.Query().Get("token"); t != "" {
		if !s.cfg.Auth.TokenLoginEnabled() || !s.cfg.Auth.ValidToken(t) {
			_ = s.cfg.Audit.Record(audit.Entry{Actor: "anonymous", Action: "login.failed", Detail: "token", Remote: remote(r)})
			http.Error(w, "invalid or disabled token login", http.StatusUnauthorized)
			return
		}
		id := s.cfg.Auth.TokenIdentity()
		s.cfg.Auth.Issue(w, r, id)
		_ = s.cfg.Audit.Record(audit.Entry{Actor: id.Display(), Role: string(config.RoleAdmin), Action: "login", Detail: "token", Remote: remote(r)})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if st, err := fs.Stat(s.ui, name); err != nil || st.IsDir() {
		name = "index.html" // client-side routing
	}
	b, err := fs.ReadFile(s.ui, name)
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<!doctype html><title>Jin</title><p style="font-family:sans-serif">The web UI is not built into this binary. Run <code>make build</code>.</p>`)
		return
	}
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

// --- helpers ------------------------------------------------------------------

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeEngineError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, upgrade.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, upgrade.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}
