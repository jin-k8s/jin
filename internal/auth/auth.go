// Package auth handles sessions (HMAC-signed cookies), the bootstrap token and OIDC single sign-on.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/jin-k8s/jin/internal/config"
)

var b64 = base64.RawURLEncoding.Strict()

const (
	SessionCookie = "jin_session"
	flowCookie    = "jin_oauth"
)

type Identity struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name"`
	Groups  []string `json:"groups,omitempty"`
	Method  string   `json:"method"` // "token" or "oidc"
	// Role is resolved on every request from the current RBAC configuration.
	Role config.Role `json:"-"`
}

// Display is what audit records and approvals show.
func (i *Identity) Display() string {
	if i.Email != "" {
		return i.Email
	}
	return i.Name
}

type session struct {
	Identity
	Exp int64 `json:"exp"`
}

// Manager issues and verifies sessions. The signing key lives in $JIN_HOME (0600).
type Manager struct {
	cfg      *config.Config
	key      []byte
	token    string
	operator string
	oidc     *oidcClient
}

func LoadKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if k, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(k) >= 32 {
			return k, nil
		}
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return k, os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600)
}

// New builds a Manager. token is the bootstrap/API token; operator names token sessions.
func New(ctx context.Context, cfg *config.Config, key []byte, token, operator string) (*Manager, error) {
	m := &Manager{cfg: cfg, key: key, token: token, operator: operator}
	if o := cfg.Auth.OIDC; o != nil {
		c, err := newOIDC(ctx, o)
		if err != nil {
			return nil, err
		}
		m.oidc = c
	}
	return m, nil
}

func (m *Manager) SSOEnabled() bool        { return m.oidc != nil }
func (m *Manager) TokenLoginEnabled() bool { return !m.cfg.Auth.DisableTokenLogin }

func (m *Manager) sign(payload []byte) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (m *Manager) verify(v string) ([]byte, error) {
	p, s, ok := strings.Cut(v, ".")
	if !ok {
		return nil, errors.New("malformed")
	}
	// Strict decoding rejects non-canonical encodings, so a cookie has exactly one valid form.
	payload, err := b64.DecodeString(p)
	if err != nil {
		return nil, err
	}
	sig, err := b64.DecodeString(s)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("bad signature")
	}
	return payload, nil
}

func (m *Manager) setCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure whenever TLS is used; plain HTTP is loopback-only.
		Name: name, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https", MaxAge: int(ttl.Seconds()),
	})
}

// Issue starts a session for id.
func (m *Manager) Issue(w http.ResponseWriter, r *http.Request, id Identity) {
	ttl := time.Duration(m.cfg.Auth.SessionHours) * time.Hour
	b, _ := json.Marshal(session{Identity: id, Exp: time.Now().Add(ttl).Unix()})
	m.setCookie(w, r, SessionCookie, m.sign(b), ttl)
}

func (m *Manager) Clear(w http.ResponseWriter, r *http.Request) {
	m.setCookie(w, r, SessionCookie, "", -time.Second)
}

func (m *Manager) ValidToken(t string) bool {
	return t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(m.token)) == 1
}

// TokenIdentity is the admin identity behind the bootstrap token and Bearer API calls.
func (m *Manager) TokenIdentity() Identity {
	return Identity{Subject: "token:" + m.operator, Name: m.operator, Method: "token"}
}

// Authenticate resolves the caller from a Bearer token or the session cookie.
func (m *Manager) Authenticate(r *http.Request) (*Identity, bool, error) {
	if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		if !m.ValidToken(bearer) {
			return nil, false, errors.New("invalid token")
		}
		id := m.TokenIdentity()
		id.Role = config.RoleAdmin
		return &id, true, nil
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, false, errors.New("no session")
	}
	payload, err := m.verify(c.Value)
	if err != nil {
		return nil, false, errors.New("invalid session")
	}
	var s session
	if err := json.Unmarshal(payload, &s); err != nil || time.Now().Unix() > s.Exp {
		return nil, false, errors.New("session expired")
	}
	id := s.Identity
	switch id.Method {
	case "token":
		if !m.TokenLoginEnabled() {
			return nil, false, errors.New("token login is disabled")
		}
		id.Role = config.RoleAdmin
	case "oidc":
		if m.oidc == nil {
			return nil, false, errors.New("SSO is no longer configured")
		}
		id.Role = m.cfg.RoleFor(id.Email, id.Groups)
	default:
		return nil, false, errors.New("invalid session")
	}
	return &id, false, nil
}

// --- OIDC -------------------------------------------------------------------

type oidcClient struct {
	cfg      *config.OIDC
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

func newOIDC(ctx context.Context, o *config.OIDC) (*oidcClient, error) {
	if o.ClientSecret() == "" {
		return nil, fmt.Errorf("OIDC client secret is not set (environment variable %s)", orDefault(o.ClientSecretEnv, "JIN_OIDC_CLIENT_SECRET"))
	}
	p, err := oidc.NewProvider(ctx, o.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s: %w", o.Issuer, err)
	}
	return &oidcClient{
		cfg: o, provider: p,
		verifier: p.Verifier(&oidc.Config{ClientID: o.ClientID}),
		oauth: oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret(), Endpoint: p.Endpoint(),
			RedirectURL: o.RedirectURL, Scopes: o.Scopes},
	}, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

type flow struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Exp      int64  `json:"e"`
}

func random() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Login redirects to the identity provider (authorization code flow with PKCE, state and nonce).
func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	if m.oidc == nil {
		http.Error(w, "SSO is not configured", http.StatusNotFound)
		return
	}
	f := flow{State: random(), Nonce: random(), Verifier: oauth2.GenerateVerifier(), Exp: time.Now().Add(10 * time.Minute).Unix()}
	b, _ := json.Marshal(f)
	m.setCookie(w, r, flowCookie, m.sign(b), 10*time.Minute)
	http.Redirect(w, r, m.oidc.oauth.AuthCodeURL(f.State, oidc.Nonce(f.Nonce), oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

// Callback completes sign-in and returns the new identity.
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) (*Identity, error) {
	if m.oidc == nil {
		return nil, errors.New("SSO is not configured")
	}
	c, err := r.Cookie(flowCookie)
	if err != nil {
		return nil, errors.New("sign-in expired; start again")
	}
	m.setCookie(w, r, flowCookie, "", -time.Second)
	payload, err := m.verify(c.Value)
	if err != nil {
		return nil, errors.New("invalid sign-in state")
	}
	var f flow
	if err := json.Unmarshal(payload, &f); err != nil || time.Now().Unix() > f.Exp {
		return nil, errors.New("sign-in expired; start again")
	}
	if e := r.URL.Query().Get("error"); e != "" {
		return nil, fmt.Errorf("identity provider: %s %s", e, r.URL.Query().Get("error_description"))
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(f.State)) != 1 {
		return nil, errors.New("state mismatch")
	}
	tok, err := m.oidc.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return nil, fmt.Errorf("code exchange: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok {
		return nil, errors.New("no id_token in token response")
	}
	idt, err := m.oidc.verifier.Verify(r.Context(), raw)
	if err != nil {
		return nil, fmt.Errorf("verify id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(f.Nonce)) != 1 {
		return nil, errors.New("nonce mismatch")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return nil, err
	}
	id := Identity{Subject: "oidc:" + idt.Subject, Method: "oidc"}
	id.Email, _ = claims["email"].(string)
	if v, ok := claims["email_verified"].(bool); ok && !v {
		return nil, errors.New("email address is not verified")
	}
	id.Name, _ = claims["name"].(string)
	if id.Name == "" {
		id.Name = id.Email
	}
	switch g := claims[m.oidc.cfg.GroupsClaim].(type) {
	case []any:
		for _, x := range g {
			if s, ok := x.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	case string:
		id.Groups = []string{g}
	}
	if doms := m.oidc.cfg.AllowedDomains; len(doms) > 0 {
		_, dom, _ := strings.Cut(id.Email, "@")
		if !slices.Contains(doms, strings.ToLower(dom)) {
			return nil, fmt.Errorf("%s is not allowed to sign in", id.Email)
		}
	}
	m.Issue(w, r, id)
	id.Role = m.cfg.RoleFor(id.Email, id.Groups)
	return &id, nil
}
