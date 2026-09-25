package gitops

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
)

// fakeGitHub is a small in-memory model of the GitHub REST endpoints Jin uses.
type fakeGitHub struct {
	mu       sync.Mutex
	commits  map[string]map[string][]byte
	branches map[string]string
	prs      []*ghPR
	nextSHA  int
	onMerge  func(pr int)
	writes   int
}

func newFakeGitHub(files map[string][]byte) *fakeGitHub {
	f := &fakeGitHub{commits: map[string]map[string][]byte{}, branches: map[string]string{}}
	f.branches["main"] = f.commit(files)
	return f
}

func (f *fakeGitHub) commit(files map[string][]byte) string {
	f.nextSHA++
	sha := fmt.Sprintf("c%04d", f.nextSHA)
	cp := map[string][]byte{}
	for k, v := range files {
		cp[k] = v
	}
	f.commits[sha] = cp
	return sha
}

func blobSHA(b []byte) string { h := sha1.Sum(b); return hex.EncodeToString(h[:]) }

func (f *fakeGitHub) resolve(ref string) map[string][]byte {
	if sha, ok := f.branches[ref]; ok {
		return f.commits[sha]
	}
	return f.commits[ref]
}

func (f *fakeGitHub) merge(n int) {
	f.mu.Lock()
	pr := f.prs[n-1]
	pr.Merged, pr.State = true, "closed"
	f.branches[pr.Base.Ref] = f.commit(f.commits[f.branches[pr.Head.Ref]])
	f.mu.Unlock()
	if f.onMerge != nil {
		f.onMerge(n)
	}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/repos/acme/infra")
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case p == "" && r.Method == "GET":
		write(map[string]string{"default_branch": "main"})
	case strings.HasPrefix(p, "/git/ref/heads/"):
		sha, ok := f.branches[strings.TrimPrefix(p, "/git/ref/heads/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(map[string]any{"object": map[string]string{"sha": sha}})
	case strings.HasPrefix(p, "/git/trees/"):
		files := f.resolve(strings.TrimPrefix(p, "/git/trees/"))
		var tree []map[string]string
		for k := range files {
			tree = append(tree, map[string]string{"path": k, "type": "blob"})
		}
		write(map[string]any{"tree": tree})
	case p == "/git/refs" && r.Method == "POST":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := strings.TrimPrefix(body["ref"], "refs/heads/")
		if _, ok := f.branches[name]; ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			write(map[string]string{"message": "Reference already exists"})
			return
		}
		f.branches[name] = body["sha"]
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(p, "/contents/") && r.Method == "GET":
		file := strings.TrimPrefix(p, "/contents/")
		b, ok := f.resolve(r.URL.Query().Get("ref"))[file]
		if !ok {
			http.NotFound(w, r)
			return
		}
		write(map[string]string{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(b), "sha": blobSHA(b)})
	case strings.HasPrefix(p, "/contents/") && r.Method == "PUT":
		file := strings.TrimPrefix(p, "/contents/")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		files := f.commits[f.branches[body["branch"]]]
		if old, ok := files[file]; ok && blobSHA(old) != body["sha"] {
			w.WriteHeader(http.StatusConflict)
			write(map[string]string{"message": "sha does not match"})
			return
		}
		content, _ := base64.StdEncoding.DecodeString(body["content"])
		next := map[string][]byte{}
		for k, v := range files {
			next[k] = v
		}
		next[file] = content
		f.branches[body["branch"]] = f.commit(next)
		f.writes++
		write(map[string]any{})
	case p == "/pulls" && r.Method == "POST":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		pr := &ghPR{Number: len(f.prs) + 1, HTMLURL: fmt.Sprintf("https://github.com/acme/infra/pull/%d", len(f.prs)+1), State: "open"}
		pr.Head.Ref, pr.Base.Ref = body["head"], body["base"]
		f.prs = append(f.prs, pr)
		w.WriteHeader(http.StatusCreated)
		write(pr)
	case p == "/pulls" && r.Method == "GET":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), "acme:")
		var out []ghPR
		for i := len(f.prs) - 1; i >= 0; i-- {
			if f.prs[i].Head.Ref == head {
				cp := *f.prs[i]
				if cp.Merged {
					t := time.Now()
					cp.MergedAt, cp.Merged = &t, false // list responses only carry merged_at
				}
				out = append(out, cp)
			}
		}
		write(out)
	case strings.HasPrefix(p, "/pulls/"):
		n, _ := strconv.Atoi(strings.TrimPrefix(p, "/pulls/"))
		if n < 1 || n > len(f.prs) {
			http.NotFound(w, r)
			return
		}
		write(f.prs[n-1])
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) add(s string)            { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *logSink) Info(f string, a ...any) { l.add(fmt.Sprintf(f, a...)) }
func (l *logSink) Warn(f string, a ...any) { l.add("WARN " + fmt.Sprintf(f, a...)) }
func (l *logSink) Progress(_, _ int, _, f string, a ...any) {
	l.add(fmt.Sprintf(f, a...))
}
func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type fakeCluster struct {
	mu      sync.Mutex
	version kube.Version
	nodes   []inventory.Node
}

func (c *fakeCluster) ServerVersion(context.Context) (kube.Version, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version, nil
}

func (c *fakeCluster) Nodes(context.Context) ([]inventory.Node, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]inventory.Node(nil), c.nodes...), nil
}

func setup(t *testing.T) (*fakeGitHub, *Executor, *fakeCluster) {
	t.Helper()
	gh := newFakeGitHub(map[string][]byte{
		"envs/prod/eks.tf":           []byte(eksTF),
		"envs/prod/variables.tf":     []byte("variable \"node_version\" {\n  default = \"1.30\"\n}\n"),
		"envs/prod/prod.auto.tfvars": []byte("node_version = \"1.30\"\n"),
		"README.md":                  []byte("# infra\n"),
	})
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	cl := &fakeCluster{version: kube.MustParseVersion("1.30"), nodes: []inventory.Node{
		{Name: "a", Version: kube.MustParseVersion("1.30"), Ready: true, PoolType: inventory.PoolEKSManaged},
		{Name: "f", Version: kube.MustParseVersion("1.28"), Ready: true, PoolType: inventory.PoolEKSFargate},
	}}
	x := &Executor{
		Repo:        &GitHub{BaseURL: srv.URL, Owner: "acme", Name: "infra", Token: "test-token"},
		UpgradeID:   "up-1",
		ClusterName: "payments-prod",
		Cluster:     cl,
		Poll:        2 * time.Millisecond,
		Targets: []Target{
			{Role: RoleControlPlane, Kind: KindTerraform, File: "envs/prod/eks.tf", Address: "module.eks", Attribute: "cluster_version"},
			{Role: RoleNodeGroup, Name: "legacy", Kind: KindTerraform, File: "envs/prod/eks.tf", Address: "aws_eks_node_group.legacy", Attribute: "version"},
			{Role: RoleAddon, Name: "coredns", Kind: KindTerraform, File: "envs/prod/eks.tf", Address: "aws_eks_addon.coredns", Attribute: "addon_version"},
		},
	}
	return gh, x, cl
}

// autoMerge merges every new PR and lets the "pipeline" apply it.
func autoMerge(t *testing.T, gh *fakeGitHub, apply func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		merged := 0
		for {
			select {
			case <-done:
				return
			case <-time.After(3 * time.Millisecond):
			}
			gh.mu.Lock()
			n := len(gh.prs)
			gh.mu.Unlock()
			for merged < n {
				merged++
				gh.merge(merged)
				apply()
			}
		}
	}()
	return func() { close(done) }
}

func TestControlPlanePRMergeAndApply(t *testing.T) {
	gh, x, cl := setup(t)
	stop := autoMerge(t, gh, func() { cl.mu.Lock(); cl.version = kube.MustParseVersion("1.31"); cl.mu.Unlock() })
	defer stop()

	log := &logSink{}
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), log); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.prs) != 1 || gh.prs[0].Head.Ref != "jin/up-1/1.31-control-plane" || !gh.prs[0].Merged {
		t.Fatalf("prs: %+v", gh.prs)
	}
	main := gh.commits[gh.branches["main"]]["envs/prod/eks.tf"]
	if !strings.Contains(string(main), `cluster_version = "1.31" # bumped by Jin`) {
		t.Fatalf("main after merge:\n%s", main)
	}
	for _, want := range []string{"Opened pull request #1", "Pull request #1 merged"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestIdempotentResumeReusesPR(t *testing.T) {
	gh, x, cl := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- x.ControlPlane(ctx, kube.MustParseVersion("1.31"), &logSink{}) }()
	for {
		gh.mu.Lock()
		n := len(gh.prs)
		gh.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel() // Jin restarts while the PR awaits review
	<-errCh

	stop := autoMerge(t, gh, func() { cl.mu.Lock(); cl.version = kube.MustParseVersion("1.31"); cl.mu.Unlock() })
	defer stop()
	log := &logSink{}
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), log); err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.prs) != 1 || gh.writes != 1 || !strings.Contains(log.String(), "Resuming with pull request #1") {
		t.Fatalf("resume must reuse PR #1 without new commits: prs=%d writes=%d\n%s", len(gh.prs), gh.writes, log)
	}
}

func TestClosedPRFailsStage(t *testing.T) {
	gh, x, _ := setup(t)
	go func() {
		for {
			gh.mu.Lock()
			if len(gh.prs) == 1 {
				gh.prs[0].State = "closed"
				gh.mu.Unlock()
				return
			}
			gh.mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), &logSink{})
	if err == nil || !strings.Contains(err.Error(), "closed without merging") {
		t.Fatalf("got %v", err)
	}
}

func TestAddonsAndDataPlane(t *testing.T) {
	gh, x, cl := setup(t)
	running := map[string]string{"coredns": "v1.11.1-eksbuild.9"}
	var mu sync.Mutex
	x.AddonCatalog = AddonVersions{
		Desired: func(_ context.Context, name string, to kube.Version) (string, error) {
			return "v1.11.4-eksbuild.2", nil
		},
		Running: func(_ context.Context, name string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return running[name], nil
		},
	}
	stop := autoMerge(t, gh, func() {
		mu.Lock()
		running["coredns"] = "v1.11.4-eksbuild.2"
		mu.Unlock()
		cl.mu.Lock()
		cl.nodes[0].Version = kube.MustParseVersion("1.31")
		cl.mu.Unlock()
	})
	defer stop()

	if err := x.Addons(context.Background(), kube.MustParseVersion("1.31"), &logSink{}); err != nil {
		t.Fatal(err)
	}
	log := &logSink{}
	if err := x.DataPlane(context.Background(), kube.MustParseVersion("1.31"), log); err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	main := gh.commits[gh.branches["main"]]
	if !strings.Contains(string(main["envs/prod/eks.tf"]), `addon_version = "v1.11.4-eksbuild.2"`) {
		t.Fatalf("add-on not bumped:\n%s", main["envs/prod/eks.tf"])
	}
	if !strings.Contains(string(main["envs/prod/prod.auto.tfvars"]), `node_version = "1.31"`) {
		t.Fatalf("node version must be bumped via tfvars:\n%s", main["envs/prod/prod.auto.tfvars"])
	}
	var heads []string
	for _, p := range gh.prs {
		heads = append(heads, p.Head.Ref)
	}
	sort.Strings(heads)
	if strings.Join(heads, ",") != "jin/up-1/1.31-add-ons,jin/up-1/1.31-data-plane" {
		t.Fatalf("one PR per stage: %v", heads)
	}
	if !strings.Contains(log.String(), "1/1 nodes Ready on 1.31") {
		t.Fatalf("Fargate nodes must be excluded from the roll: %s", log)
	}
}

func TestNothingToChangeSkipsPR(t *testing.T) {
	gh, x, cl := setup(t)
	x.Targets = x.Targets[:1]
	cl.version = kube.MustParseVersion("1.30")
	// Repository already declares 1.31 (e.g. someone merged it manually); the pipeline applies it.
	gh.mu.Lock()
	files := gh.commits[gh.branches["main"]]
	files["envs/prod/eks.tf"] = []byte(strings.Replace(eksTF, `"1.30"`, `"1.31"`, 1))
	gh.mu.Unlock()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cl.mu.Lock()
		cl.version = kube.MustParseVersion("1.31")
		cl.mu.Unlock()
	}()
	log := &logSink{}
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), log); err != nil {
		t.Fatal(err)
	}
	if len(gh.prs) != 0 || !strings.Contains(log.String(), "already declares") {
		t.Fatalf("expected no PR: %s", log)
	}
}

func TestGitHubAuthErrorsSurface(t *testing.T) {
	gh := newFakeGitHub(nil)
	srv := httptest.NewServer(gh)
	defer srv.Close()
	g := &GitHub{BaseURL: srv.URL, Owner: "acme", Name: "infra", Token: "wrong"}
	if _, err := g.DefaultBranch(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("got %v", err)
	}
}

func TestGitHubPermissionHint(t *testing.T) {
	e := &apiError{Status: 403, Message: "Resource not accessible by personal access token", Op: "GET /git/ref/heads/main"}
	if !strings.Contains(e.Error(), "Contents and Pull requests") {
		t.Fatalf("403 needs a permission hint: %s", e)
	}
	if e := (&apiError{Status: 422, Message: "Reference already exists"}); strings.Contains(e.Error(), "permission") {
		t.Fatalf("unrelated errors must not get the hint: %s", e)
	}
}
