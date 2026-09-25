// Package config loads the server configuration ($JIN_HOME/config.yaml): SSO, RBAC and approval policies.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/jin-k8s/jin/internal/policy"
)

type Role string

const (
	RoleViewer   Role = "viewer"
	RolePlanner  Role = "planner"
	RoleApprover Role = "approver"
	RoleAdmin    Role = "admin"
)

var roleRank = map[Role]int{RoleViewer: 0, RolePlanner: 1, RoleApprover: 2, RoleAdmin: 3}

func (r Role) Valid() bool { _, ok := roleRank[r]; return ok }

// Allows reports whether r includes the permissions of want.
func (r Role) Allows(want Role) bool { return r.Valid() && roleRank[r] >= roleRank[want] }

type OIDC struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"clientID"`
	// ClientSecretEnv names the environment variable holding the client secret.
	ClientSecretEnv string   `json:"clientSecretEnv,omitempty"`
	RedirectURL     string   `json:"redirectURL"`
	Scopes          []string `json:"scopes,omitempty"`
	GroupsClaim     string   `json:"groupsClaim,omitempty"`
	// AllowedDomains restricts sign-in to these email domains when set.
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

func (o *OIDC) ClientSecret() string {
	name := o.ClientSecretEnv
	if name == "" {
		name = "JIN_OIDC_CLIENT_SECRET"
	}
	return os.Getenv(name)
}

type Auth struct {
	OIDC *OIDC `json:"oidc,omitempty"`
	// DisableTokenLogin turns off the bootstrap token login once SSO is configured.
	DisableTokenLogin bool `json:"disableTokenLogin,omitempty"`
	SessionHours      int  `json:"sessionHours,omitempty"`
}

type Binding struct {
	Role Role `json:"role"`
	// Subjects are "user:<email>", "group:<name>" or "*".
	Subjects []string `json:"subjects"`
}

type RBAC struct {
	// DefaultRole applies to signed-in SSO users without a binding. Defaults to viewer.
	DefaultRole Role      `json:"defaultRole,omitempty"`
	Bindings    []Binding `json:"bindings,omitempty"`
}

type GitHub struct {
	// EnterpriseHosts are GitHub Enterprise Server hostnames repositories may use (api.github.com is always allowed).
	EnterpriseHosts []string `json:"enterpriseHosts,omitempty"`
}

type Config struct {
	Auth     Auth            `json:"auth"`
	RBAC     RBAC            `json:"rbac"`
	Policies []policy.Policy `json:"policies,omitempty"`
	GitHub   GitHub          `json:"github,omitempty"`
}

// AllowedGitHubURL checks that a repository API base URL targets github.com or an allowed GHES host.
func (c *Config) AllowedGitHubURL(base string) error {
	if base == "" {
		return nil
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("repository API URL must be an https URL")
	}
	if u.Host == "api.github.com" || slices.Contains(c.GitHub.EnterpriseHosts, u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%s is not an allowed GitHub host; add it to github.enterpriseHosts in config.yaml", u.Hostname())
}

// Load reads path; a missing file yields the defaults (token login, single admin).
func Load(path string) (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, c.Validate()
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.UnmarshalStrict(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.RBAC.DefaultRole == "" {
		c.RBAC.DefaultRole = RoleViewer
	}
	if !c.RBAC.DefaultRole.Valid() {
		return fmt.Errorf("rbac.defaultRole: unknown role %q", c.RBAC.DefaultRole)
	}
	for _, b := range c.RBAC.Bindings {
		if !b.Role.Valid() {
			return fmt.Errorf("rbac binding: unknown role %q", b.Role)
		}
		for _, s := range b.Subjects {
			if s != "*" && !strings.HasPrefix(s, "user:") && !strings.HasPrefix(s, "group:") {
				return fmt.Errorf("rbac subject %q must be user:<email>, group:<name> or *", s)
			}
		}
	}
	if o := c.Auth.OIDC; o != nil {
		if o.Issuer == "" || o.ClientID == "" || o.RedirectURL == "" {
			return errors.New("auth.oidc needs issuer, clientID and redirectURL")
		}
		if !strings.HasPrefix(o.RedirectURL, "https://") && !strings.HasPrefix(o.RedirectURL, "http://localhost") && !strings.HasPrefix(o.RedirectURL, "http://127.0.0.1") {
			return errors.New("auth.oidc.redirectURL must use https (or localhost)")
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = "groups"
		}
		if len(o.Scopes) == 0 {
			o.Scopes = []string{"openid", "email", "profile"}
		}
	}
	if c.Auth.DisableTokenLogin && c.Auth.OIDC == nil {
		return errors.New("auth.disableTokenLogin requires auth.oidc, otherwise nobody can sign in")
	}
	if c.Auth.SessionHours <= 0 {
		c.Auth.SessionHours = 12
	}
	for _, p := range c.Policies {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// RoleFor resolves the highest role bound to an SSO identity.
func (c *Config) RoleFor(email string, groups []string) Role {
	best := c.RBAC.DefaultRole
	for _, b := range c.RBAC.Bindings {
		for _, s := range b.Subjects {
			match := s == "*" || (email != "" && s == "user:"+email)
			if g, ok := strings.CutPrefix(s, "group:"); ok && slices.Contains(groups, g) {
				match = true
			}
			if match && roleRank[b.Role] > roleRank[best] {
				best = b.Role
			}
		}
	}
	return best
}
