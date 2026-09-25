package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "docs", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Policies) != 2 || c.Policies[0].Required() != 2 || c.Auth.OIDC.GroupsClaim != "groups" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if got := c.RoleFor("ana@example.com", []string{"sre", "developers"}); got != RoleApprover {
		t.Fatalf("highest binding must win: %s", got)
	}
	if got := c.RoleFor("x@example.com", nil); got != RoleViewer {
		t.Fatalf("default role: %s", got)
	}
}

func TestInvalidConfigs(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": "auth:\n  nope: true\n",
		"bad role":      "rbac:\n  bindings:\n    - role: god\n      subjects: ['*']\n",
		"bad subject":   "rbac:\n  bindings:\n    - role: admin\n      subjects: ['alice']\n",
		"lockout":       "auth:\n  disableTokenLogin: true\n",
		"http redirect": "auth:\n  oidc: {issuer: https://x, clientID: a, redirectURL: 'http://jin.example.com/cb'}\n",
		"bad window":    "policies:\n  - name: p\n    windows: [{start: '9', end: '10:00'}]\n",
	} {
		p := filepath.Join(dir, "c.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.yaml")); err != nil {
		t.Fatalf("missing file must yield defaults: %v", err)
	}
}
