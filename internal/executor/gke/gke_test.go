package gke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
)

type fakeGKE struct {
	mu      sync.Mutex
	master  string
	pools   map[string]string
	ops     map[string]*operation
	polls   map[string]int
	calls   []string
	failOp  bool
	imgSeen string
	applies map[string]func()
}

func (f *fakeGKE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const cl = "/projects/acme-prod/locations/europe-west1/clusters/web"
	p := r.URL.Path
	enc := json.NewEncoder(w)
	newOp := func(typ, target string, apply func()) {
		name := fmt.Sprintf("op-%d", len(f.ops)+1)
		op := &operation{Name: name, OperationType: typ, Status: "RUNNING", TargetLink: "https://container.googleapis.com/v1" + target}
		f.ops[name] = op
		f.polls[name] = 2
		f.pending(name, apply)
		_ = enc.Encode(op)
	}
	switch {
	case p == cl && r.Method == "GET":
		var nps []map[string]any
		for n, v := range f.pools {
			nps = append(nps, map[string]any{"name": n, "version": v, "config": map[string]string{"imageType": "COS_CONTAINERD"}})
		}
		_ = enc.Encode(map[string]any{"currentMasterVersion": f.master, "status": "RUNNING", "nodePools": nps})
	case p == cl+":updateMaster":
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, "updateMaster "+body["masterVersion"])
		newOp("UPGRADE_MASTER", cl, func() { f.master = body["masterVersion"] + ".5-gke.100" })
	case strings.HasPrefix(p, cl+"/nodePools/") && r.Method == "PUT":
		name := strings.TrimPrefix(p, cl+"/nodePools/")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, "updateNodePool "+name+" "+body["nodeVersion"])
		f.imgSeen = body["imageType"]
		newOp("UPGRADE_NODES", cl+"/nodePools/"+name, func() { f.pools[name] = f.master })
	case p == "/projects/acme-prod/locations/europe-west1/operations":
		var ops []operation
		for _, o := range f.ops {
			ops = append(ops, *o)
		}
		_ = enc.Encode(map[string]any{"operations": ops})
	case strings.HasPrefix(p, "/projects/acme-prod/locations/europe-west1/operations/"):
		name := strings.TrimPrefix(p, "/projects/acme-prod/locations/europe-west1/operations/")
		op := f.ops[name]
		if op.Status != "DONE" {
			f.polls[name]--
			if f.polls[name] <= 0 {
				op.Status = "DONE"
				if f.failOp {
					op.Error = &struct {
						Message string `json:"message"`
					}{"Insufficient quota to satisfy the request"}
				} else if fn := f.applies[name]; fn != nil {
					fn()
				}
			}
		}
		_ = enc.Encode(op)
	default:
		http.Error(w, `{"error":{"message":"unexpected"}}`, http.StatusNotImplemented)
	}
}

func (f *fakeGKE) pending(name string, apply func()) {
	if f.applies == nil {
		f.applies = map[string]func(){}
	}
	f.applies[name] = apply
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

func setup(t *testing.T) (*fakeGKE, *Executor) {
	f := &fakeGKE{master: "1.30.9-gke.100", pools: map[string]string{"default": "1.30.9-gke.100"}, ops: map[string]*operation{}, polls: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Executor{HTTP: srv.Client(), BaseURL: srv.URL, Project: "acme-prod", Location: "europe-west1", Cluster: "web", Poll: time.Millisecond,
		Nodes: func(context.Context) ([]inventory.Node, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			v, _ := kube.ParseVersion(f.pools["default"])
			return []inventory.Node{{Name: "n1", Pool: "default", PoolType: inventory.PoolGKE, Version: v, Ready: true}}, nil
		}}
}

func TestGKEUpgrade(t *testing.T) {
	f, x := setup(t)
	log := &logSink{}
	to := kube.MustParseVersion("1.31")
	if err := x.ControlPlane(context.Background(), to, log); err != nil {
		t.Fatal(err)
	}
	if err := x.ControlPlane(context.Background(), to, log); err != nil {
		t.Fatal(err)
	}
	if err := x.DataPlane(context.Background(), to, log); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Join(f.calls, "|") != "updateMaster 1.31|updateNodePool default -" {
		t.Fatalf("calls: %v", f.calls)
	}
	if f.pools["default"] != "1.31.5-gke.100" || f.imgSeen != "COS_CONTAINERD" {
		t.Fatalf("pool %s image %s", f.pools["default"], f.imgSeen)
	}
	if !strings.Contains(log.String(), "Node pool default: 1/1 nodes Ready on 1.31") {
		t.Fatalf("progress missing:\n%s", log)
	}
}

func TestGKEResumeAndFailures(t *testing.T) {
	f, x := setup(t)
	f.ops["op-9"] = &operation{Name: "op-9", OperationType: "UPGRADE_MASTER", Status: "RUNNING", TargetLink: "https://x/v1/projects/acme-prod/locations/europe-west1/clusters/web"}
	f.polls["op-9"] = 1
	f.pending("op-9", func() { f.master = "1.31.1-gke.1" })
	log := &logSink{}
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), log); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || !strings.Contains(log.String(), "Resuming") {
		t.Fatalf("must attach to running op: %v\n%s", f.calls, log)
	}
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.33"), log); err == nil || !strings.Contains(err.Error(), "one minor version") {
		t.Fatalf("skip guard: %v", err)
	}

	f2, x2 := setup(t)
	f2.failOp = true
	err := x2.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), &logSink{})
	if err == nil || !strings.Contains(err.Error(), "Insufficient quota") {
		t.Fatalf("operation error must surface: %v", err)
	}
}

func TestParseContext(t *testing.T) {
	p, l, c, err := ParseContext("gke_acme-prod_europe-west1_web")
	if err != nil || p != "acme-prod" || l != "europe-west1" || c != "web" {
		t.Fatalf("%s %s %s %v", p, l, c, err)
	}
	if _, _, _, err := ParseContext("kind-kind"); err == nil {
		t.Fatal("non-GKE context must fail")
	}
}
