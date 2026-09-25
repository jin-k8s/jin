package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/secrets"
	"github.com/jin-k8s/jin/internal/upgrade"
)

// --- GitHub token -------------------------------------------------------------

func (s *Server) getGitHubIntegration(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"configured": false}
	if s.cfg.Env.Secrets != nil {
		if info, err := s.cfg.Env.Secrets.Info(app.GitHubSecret); err == nil {
			out = map[string]any{"configured": true, "source": "jin", "hint": info.Hint, "setAt": info.SetAt, "setBy": info.SetBy,
				"login": info.Meta["login"], "scopes": info.Meta["scopes"]}
		}
	}
	if out["configured"] == false {
		if tok, _ := s.cfg.Env.GitHubToken(nil); tok != "" {
			out = map[string]any{"configured": true, "source": "env:GITHUB_TOKEN"}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// putGitHubIntegration validates a token against GitHub and stores it encrypted. It is never returned.
func (s *Server) putGitHubIntegration(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Env.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secret storage is not available")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	tok := strings.TrimSpace(req.Token)
	if len(tok) < 20 || len(tok) > 255 || strings.ContainsAny(tok, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "that does not look like a GitHub token")
		return
	}
	login, scopes, err := s.checkGitHubToken(r.Context(), tok)
	if err != nil {
		s.record(r, "integration.github.rejected", "", err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.cfg.Env.Secrets.Put(app.GitHubSecret, tok, identity(r).Display(), map[string]string{"login": login, "scopes": scopes}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, "integration.github.set", login, "token stored encrypted")
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "source": "jin", "login": login, "scopes": scopes})
}

func (s *Server) checkGitHubToken(ctx context.Context, tok string) (login, scopes string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.cfg.GitHubAPI, "/")+"/user", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("could not reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return "", "", errors.New("GitHub rejected the token (expired, revoked or mistyped)")
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub returned %d while checking the token", resp.StatusCode)
	}
	var u struct {
		Login string `json:"login"`
	}
	_ = json.Unmarshal(b, &u)
	return u.Login, resp.Header.Get("X-OAuth-Scopes"), nil
}

func (s *Server) deleteGitHubIntegration(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Env.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, "secret storage is not available")
		return
	}
	if err := s.cfg.Env.Secrets.Delete(app.GitHubSecret); err != nil && !errors.Is(err, secrets.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.record(r, "integration.github.removed", "", "")
	w.WriteHeader(http.StatusNoContent)
}

// --- AWS / EKS ------------------------------------------------------------------

func (s *Server) awsProfiles(w http.ResponseWriter, _ *http.Request) {
	p := app.AWSProfiles()
	if p == nil {
		p = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": p})
}

func eksRefFromQuery(r *http.Request) upgrade.EKSRef {
	q := r.URL.Query()
	return upgrade.EKSRef{Region: q.Get("region"), Profile: q.Get("profile"), RoleARN: q.Get("roleArn")}
}

func (s *Server) eksClusters(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	names, err := s.cfg.Env.ListEKSClusters(ctx, eksRefFromQuery(r))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": names})
}

func (s *Server) addCluster(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Region   string `json:"region"`
		Profile  string `json:"profile"`
		RoleARN  string `json:"roleArn"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Provider != "eks" {
		writeError(w, http.StatusBadRequest, "only EKS clusters can be added in the UI today; GKE and AKS are read from the kubeconfig")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	reg, version, err := s.cfg.Env.RegisterEKS(ctx, upgrade.EKSRef{Name: req.Name, Region: req.Region, Profile: req.Profile, RoleARN: req.RoleARN}, identity(r).Display())
	if err != nil {
		s.record(r, "cluster.add.failed", req.Region+"/"+req.Name, err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "cluster.add", reg.Context, "EKS "+version)
	writeJSON(w, http.StatusCreated, map[string]any{"context": reg.Context, "version": version, "endpoint": reg.Endpoint})
}

func (s *Server) removeCluster(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("context")
	if err := s.cfg.Env.Settings.Unregister(name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.record(r, "cluster.remove", name, "")
	w.WriteHeader(http.StatusNoContent)
}
