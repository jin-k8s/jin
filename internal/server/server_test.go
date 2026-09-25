package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/audit"
	"github.com/jin-k8s/jin/internal/auth"
	"github.com/jin-k8s/jin/internal/clusters"
	"github.com/jin-k8s/jin/internal/config"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/policy"
	"github.com/jin-k8s/jin/internal/provider"
	"github.com/jin-k8s/jin/internal/runrecord"
	"github.com/jin-k8s/jin/internal/upgrade"
)

const token = "0123456789abcdef0123456789abcdef"

type fakeCluster struct {
	mu      sync.Mutex
	version kube.Version
}

func (f *fakeCluster) ServerVersion(context.Context) (kube.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}
func (f *fakeCluster) Plan(_ context.Context, t kube.Version) (*plan.Plan, error) {
	v, _ := f.ServerVersion(context.Background())
	return &plan.Plan{Current: v, Target: t, Hops: []plan.Hop{{From: v, To: t}}}, nil
}
func (f *fakeCluster) Verify(context.Context, kube.Version, upgrade.Logger) error { return nil }
func (f *fakeCluster) Executor() upgrade.Executor                                 { return f }
func (f *fakeCluster) Name() string                                               { return "fake" }
func (f *fakeCluster) ControlPlane(_ context.Context, to kube.Version, l upgrade.Logger) error {
	f.mu.Lock()
	f.version = to
	f.mu.Unlock()
	l.Info("control plane on %s", to)
	return nil
}
func (f *fakeCluster) Addons(context.Context, kube.Version, upgrade.Logger) error    { return nil }
func (f *fakeCluster) DataPlane(context.Context, kube.Version, upgrade.Logger) error { return nil }

const kubeconfig = `apiVersion: v1
kind: Config
current-context: arn:aws:eks:us-east-1:111122223333:cluster/dev
clusters:
- name: arn:aws:eks:us-east-1:111122223333:cluster/dev
  cluster: {server: "https://127.0.0.1:1"}
contexts:
- name: arn:aws:eks:us-east-1:111122223333:cluster/dev
  context: {cluster: "arn:aws:eks:us-east-1:111122223333:cluster/dev", user: dev}
users:
- name: dev
  user: {token: secret-token-never-returned}
`

type fixture struct {
	srv    *httptest.Server
	runID  string
	engine *upgrade.Engine
	audit  *audit.Log
	dir    string
}

func setup(t *testing.T, cfg *config.Config) *fixture {
	t.Helper()
	if cfg == nil {
		cfg = &config.Config{}
	}
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kc, []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	runs := runrecord.NewStore(filepath.Join(dir, "runs"))
	rec := runrecord.New(runrecord.KindPlanRun, time.Now())
	rec.Cluster.Context = "arn:aws:eks:us-east-1:111122223333:cluster/dev"
	rec.Cluster.Provider = "eks"
	rec.Plan = &plan.Plan{Provider: "eks", Current: kube.MustParseVersion("1.30"), Target: kube.MustParseVersion("1.31"), Ready: true, Complete: true,
		Hops: []plan.Hop{{From: kube.MustParseVersion("1.30"), To: kube.MustParseVersion("1.31"), DataPlane: provider.DataPlaneFinal}}}
	rec.Finish(time.Now(), nil)
	if err := runs.Save(rec); err != nil {
		t.Fatal(err)
	}

	settings := clusters.NewStore(filepath.Join(dir, "clusters.json"))
	_ = settings.Put(&clusters.Settings{Context: rec.Cluster.Context, Environment: "prod"})
	env := &app.Env{Kubeconfig: kc, Runs: runs, Settings: settings}

	fc := &fakeCluster{version: kube.MustParseVersion("1.30")}
	engine := upgrade.NewEngine(upgrade.NewStore(filepath.Join(dir, "upgrades")), func(_ context.Context, u *upgrade.Upgrade) (upgrade.Cluster, error) {
		if u.Cluster.EKS == nil || u.Cluster.EKS.Name != "dev" || u.Cluster.Environment != "prod" {
			t.Errorf("identity and environment not resolved: %+v", u.Cluster)
		}
		return fc, nil
	})
	t.Cleanup(engine.Shutdown)

	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	authn, err := auth.New(context.Background(), cfg, key, token, "ops-bot")
	if err != nil {
		t.Fatal(err)
	}
	al, err := audit.Open(filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		Env: env, Engine: engine, Auth: authn, Cfg: cfg, Audit: al, LoopbackOnly: true,
		UI: fstest.MapFS{"index.html": {Data: []byte("<html>jin-ui</html>")}, "assets/app.js": {Data: []byte("console.log(1)")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &fixture{srv: ts, runID: rec.ID, engine: engine, audit: al, dir: dir}
}

func (f *fixture) do(t *testing.T, c *http.Client, method, path, body string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	// Use localhost so cookies set during SSO redirects match the callback host.
	req, err := http.NewRequest(method, strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1)+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

var (
	bearer = map[string]string{"Authorization": "Bearer " + token}
	csrf   = map[string]string{csrfHeader: "1"}
)

func browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func TestTokenLoginAndCSRF(t *testing.T) {
	f := setup(t, nil)
	plain := &http.Client{}
	if resp, _ := f.do(t, plain, "GET", "/api/v1/runs", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no credentials: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, plain, "GET", "/api/v1/runs", "", map[string]string{"Authorization": "Bearer nope"}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad bearer: %d", resp.StatusCode)
	}
	if resp, _ := f.do(t, plain, "GET", "/?token=wrong", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login token: %d", resp.StatusCode)
	}

	b := browser()
	resp, body := f.do(t, b, "GET", "/?token="+token, "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "jin-ui") {
		t.Fatalf("token login: %d %s", resp.StatusCode, body)
	}
	_, body = f.do(t, b, "GET", "/api/v1/session", "", nil)
	if !strings.Contains(body, `"role":"admin"`) || !strings.Contains(body, `"method":"token"`) {
		t.Fatalf("token session must be admin: %s", body)
	}
	create := `{"runId":"` + f.runID + `"}`
	if resp, _ := f.do(t, b, "POST", "/api/v1/upgrades", create, nil); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie POST without CSRF header must be rejected: %d", resp.StatusCode)
	}
	if resp, body := f.do(t, b, "POST", "/api/v1/upgrades", create, csrf); resp.StatusCode != http.StatusCreated {
		t.Fatalf("cookie POST with header: %d %s", resp.StatusCode, body)
	}

	// Tampering with the signed session invalidates it.
	u, _ := url.Parse(strings.Replace(f.srv.URL, "127.0.0.1", "localhost", 1))
	for _, c := range b.Jar.Cookies(u) {
		if c.Name == auth.SessionCookie {
			// Flip one character inside the payload (the signature is after the dot).
			payload, sig, _ := strings.Cut(c.Value, ".")
			mid := len(payload) / 2
			repl := byte('A')
			if payload[mid] == 'A' {
				repl = 'B'
			}
			c.Value = payload[:mid] + string(repl) + payload[mid+1:] + "." + sig
			b.Jar.SetCookies(u, []*http.Cookie{c})
		}
	}
	if resp, _ := f.do(t, b, "GET", "/api/v1/runs", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered session must be rejected: %d", resp.StatusCode)
	}

	if ok, _, _ := f.audit.Verify(); !ok {
		t.Fatal("audit chain broken")
	}
	es, _ := f.audit.Entries(time.Time{})
	var actions []string
	for _, e := range es {
		actions = append(actions, e.Action)
	}
	if got := strings.Join(actions, ","); !strings.Contains(got, "login.failed,login,upgrade.create") {
		t.Fatalf("audit trail: %s", got)
	}
}

func TestHostCheckAndHeaders(t *testing.T) {
	f := setup(t, nil)
	req, _ := http.NewRequest("GET", f.srv.URL+"/", nil)
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign Host must be rejected: %d", resp.StatusCode)
	}
	resp, _ = f.do(t, http.DefaultClient, "GET", "/upgrades/some-client-route", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("SPA fallback / CSP: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestContextsAndSettings(t *testing.T) {
	f := setup(t, nil)
	_, body := f.do(t, http.DefaultClient, "GET", "/api/v1/contexts", "", bearer)
	if strings.Contains(body, "secret-token") || !strings.Contains(body, `"name":"dev"`) || !strings.Contains(body, `"environment":"prod"`) {
		t.Fatalf("contexts: %s", body)
	}
	evil := `{"context":"x","gitops":{"provider":"github","owner":"a","repo":"b","baseUrl":"https://attacker.example","tokenEnv":"GITHUB_TOKEN","targets":[]}}`
	if resp, body := f.do(t, http.DefaultClient, "PUT", "/api/v1/settings", evil, bearer); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "not an allowed GitHub host") {
		t.Fatalf("foreign GitHub host must be refused: %d %s", resp.StatusCode, body)
	}
	secret := `{"context":"x","gitops":{"provider":"github","owner":"a","repo":"b","tokenEnv":"AWS_SECRET_ACCESS_KEY","targets":[]}}`
	if resp, _ := f.do(t, http.DefaultClient, "PUT", "/api/v1/settings", secret, bearer); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("arbitrary token env must be refused: %d", resp.StatusCode)
	}
}

func TestUpgradeFlowOverAPI(t *testing.T) {
	f := setup(t, nil)
	c := http.DefaultClient
	resp, body := f.do(t, c, "POST", "/api/v1/upgrades", `{"runId":"`+f.runID+`","mode":"direct"}`, bearer)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var u upgrade.Upgrade
	_ = json.Unmarshal([]byte(body), &u)

	if resp, _ := f.do(t, c, "POST", "/api/v1/upgrades", `{"runId":"`+f.runID+`"}`, bearer); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second upgrade for the same cluster: %d", resp.StatusCode)
	}
	if resp, body := f.do(t, c, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.30"}`, bearer); resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "1.31") {
		t.Fatalf("wrong confirmation must be rejected: %d %s", resp.StatusCode, body)
	}

	req, _ := http.NewRequest("GET", f.srv.URL+"/api/v1/upgrades/"+u.ID+"/stream", nil)
	req.Host = strings.Replace(req.URL.Host, "127.0.0.1", "localhost", 1)
	req.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()

	if resp, body := f.do(t, c, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.31","comment":"CHG-42"}`, bearer); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve: %d %s", resp.StatusCode, body)
	}
	sc := bufio.NewScanner(stream.Body)
	var sawApproval, sawComplete bool
	for sc.Scan() && !sawComplete {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var ev upgrade.Event
			_ = json.Unmarshal([]byte(data), &ev)
			sawApproval = sawApproval || strings.Contains(ev.Message, "approved by ops-bot: CHG-42")
			sawComplete = strings.Contains(ev.Message, "Upgrade complete")
		}
	}
	if !sawApproval || !sawComplete {
		t.Fatalf("stream: approval %v complete %v", sawApproval, sawComplete)
	}
}

// --- SSO, RBAC and approval policies against a fake OpenID Connect provider ---

type fakeIdP struct {
	srv       *httptest.Server
	key       *rsa.PrivateKey
	mu        sync.Mutex
	user      map[string]any
	nonce     string
	challenge string
	clientID  string
}

func newIdP(t *testing.T) *fakeIdP {
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	p := &fakeIdP{key: k, clientID: "jin"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.srv.URL, "authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
			"jwks_uri": p.srv.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		p.mu.Lock()
		p.nonce, p.challenge = q.Get("nonce"), q.Get("code_challenge")
		p.mu.Unlock()
		if q.Get("code_challenge_method") != "S256" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=abc&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		p.mu.Lock()
		defer p.mu.Unlock()
		if base64.RawURLEncoding.EncodeToString(sum[:]) != p.challenge || r.Form.Get("code") != "abc" {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		claims := map[string]any{"iss": p.srv.URL, "aud": p.clientID, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": p.nonce}
		for k, v := range p.user {
			claims[k] = v
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k, KeyID: "k1"}}, nil)
		payload, _ := json.Marshal(claims)
		jws, _ := signer.Sign(payload)
		idt, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idt, "expires_in": 3600})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeIdP) as(claims map[string]any) {
	p.mu.Lock()
	p.user = claims
	p.mu.Unlock()
}

// login signs a fresh browser in through the full authorization-code + PKCE flow.
func login(t *testing.T, f *fixture, idp *fakeIdP, claims map[string]any) *http.Client {
	t.Helper()
	idp.as(claims)
	b := browser()
	resp, body := f.do(t, b, "GET", "/auth/login", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "jin-ui") {
		t.Fatalf("SSO login for %v: %d %s", claims["email"], resp.StatusCode, body)
	}
	return b
}

func TestSSORBACAndTwoPersonRule(t *testing.T) {
	idp := newIdP(t)
	t.Setenv("JIN_OIDC_CLIENT_SECRET", "s3cret")

	cfg := &config.Config{
		Auth: config.Auth{OIDC: &config.OIDC{Issuer: idp.srv.URL, ClientID: "jin", RedirectURL: "http://localhost/auth/callback"}},
		RBAC: config.RBAC{Bindings: []config.Binding{
			{Role: config.RoleApprover, Subjects: []string{"group:sre"}},
			{Role: config.RolePlanner, Subjects: []string{"user:dev@acme.io"}},
		}},
		Policies: []policy.Policy{{Name: "production", Match: policy.Match{Environments: []string{"prod"}}, MinApprovals: 2, ForbidSelfApproval: true, RequireComment: true}},
	}
	f := rebuild(t, cfg)

	viewer := login(t, f, idp, map[string]any{"sub": "u1", "email": "viewer@acme.io", "email_verified": true, "name": "Vi"})
	_, body := f.do(t, viewer, "GET", "/api/v1/session", "", nil)
	if !strings.Contains(body, `"role":"viewer"`) {
		t.Fatalf("default role: %s", body)
	}
	if resp, _ := f.do(t, viewer, "POST", "/api/v1/plans", `{"context":"x"}`, csrf); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer must not plan: %d", resp.StatusCode)
	}

	dev := login(t, f, idp, map[string]any{"sub": "u2", "email": "dev@acme.io", "email_verified": true, "name": "Dev"})
	resp, body := f.do(t, dev, "POST", "/api/v1/upgrades", `{"runId":"`+f.runID+`"}`, csrf)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("planner creates upgrade: %d %s", resp.StatusCode, body)
	}
	var u upgrade.Upgrade
	_ = json.Unmarshal([]byte(body), &u)
	if resp, _ := f.do(t, dev, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.31","comment":"x"}`, csrf); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("planner must not approve: %d", resp.StatusCode)
	}

	sre1 := login(t, f, idp, map[string]any{"sub": "u3", "email": "ana@acme.io", "email_verified": true, "name": "Ana", "groups": []string{"sre"}})
	if resp, body := f.do(t, sre1, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.31"}`, csrf); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "requires a comment") {
		t.Fatalf("policy comment requirement: %d %s", resp.StatusCode, body)
	}
	resp, body = f.do(t, sre1, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.31","comment":"CHG-7"}`, csrf)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"awaiting-approval"`) {
		t.Fatalf("first of two approvals: %d %s", resp.StatusCode, body)
	}
	if resp, _ := f.do(t, sre1, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.31","comment":"again"}`, csrf); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("same approver twice must be refused: %d", resp.StatusCode)
	}

	sre2 := login(t, f, idp, map[string]any{"sub": "u4", "email": "raj@acme.io", "email_verified": true, "name": "Raj", "groups": []string{"sre"}})
	if resp, body := f.do(t, sre2, "POST", "/api/v1/upgrades/"+u.ID+"/approve", `{"hop":1,"confirm":"1.31","comment":"CHG-7 second"}`, csrf); resp.StatusCode != http.StatusOK {
		t.Fatalf("second approval: %d %s", resp.StatusCode, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := f.engine.Get(u.ID)
		if got.Status == upgrade.StatusSucceeded {
			if a := got.Hops[0].Approvers(); len(a) != 2 {
				t.Fatalf("approvers: %v", a)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("upgrade did not complete: %s", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	unverified := browser()
	idp.as(map[string]any{"sub": "u5", "email": "x@acme.io", "email_verified": false})
	if resp, _ := f.do(t, unverified, "GET", "/auth/login", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unverified email must be refused: %d", resp.StatusCode)
	}
}

// rebuild reserves a listener first so the OIDC redirect URL can point at the server itself.
func rebuild(t *testing.T, cfg *config.Config) *fixture {
	t.Helper()
	var h http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	ts.Start()
	t.Cleanup(ts.Close)
	cfg.Auth.OIDC.RedirectURL = "http://localhost:" + ts.URL[strings.LastIndex(ts.URL, ":")+1:] + "/auth/callback"
	f := setup(t, cfg)
	f.srv.Close()
	h = f.srv.Config.Handler
	f.srv = ts
	return f
}
