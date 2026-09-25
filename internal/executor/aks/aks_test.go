package aks

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

// fakeARM models a managed cluster whose PUTs go through Upgrading before settling. It first
// reports the stale "Succeeded" state once after each PUT, as ARM can.
type fakeARM struct {
	mu        sync.Mutex
	version   string
	state     string
	pools     map[string]map[string]any
	pending   int
	stale     bool
	puts      []map[string]any
	poolPuts  []string
	upgrades  []map[string]any
	target    string
	poolDone  map[string]string
	poolStale map[string]bool
}

const cl = "/subscriptions/sub-1/resourceGroups/rg-prod/providers/Microsoft.ContainerService/managedClusters/payments"

func (f *fakeARM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("api-version") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	enc := json.NewEncoder(w)
	p := r.URL.Path
	switch {
	case p == cl && r.Method == "GET":
		state := f.state
		if f.stale {
			f.stale, state = false, "Succeeded"
		} else if f.state == "Upgrading" {
			if f.pending--; f.pending <= 0 {
				f.state, f.version = "Succeeded", f.target
			}
		}
		_ = enc.Encode(map[string]any{"location": "westeurope", "properties": map[string]any{
			"currentKubernetesVersion": f.version, "kubernetesVersion": f.version, "provisioningState": state,
			"agentPoolProfiles": []any{map[string]any{"name": "system", "orchestratorVersion": "1.30.5"}},
		}})
	case p == cl && r.Method == "PUT":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.puts = append(f.puts, body)
		f.target = body["properties"].(map[string]any)["kubernetesVersion"].(string)
		f.state, f.pending, f.stale = "Upgrading", 2, true
		_ = enc.Encode(body)
	case p == cl+"/upgradeProfiles/default":
		_ = enc.Encode(map[string]any{"properties": map[string]any{"controlPlaneProfile": map[string]any{"upgrades": f.upgrades}}})
	case p == cl+"/agentPools":
		var v []any
		for _, pool := range f.pools {
			v = append(v, pool)
		}
		_ = enc.Encode(map[string]any{"value": v})
	case strings.HasPrefix(p, cl+"/agentPools/"):
		name := strings.TrimPrefix(p, cl+"/agentPools/")
		pool := f.pools[name]
		props := pool["properties"].(map[string]any)
		if r.Method == "PUT" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.poolPuts = append(f.poolPuts, name+" "+body["properties"].(map[string]any)["orchestratorVersion"].(string))
			f.poolDone[name] = body["properties"].(map[string]any)["orchestratorVersion"].(string)
			props["provisioningState"] = "Upgrading"
			f.poolStale[name] = true
			_ = enc.Encode(pool)
			return
		}
		if f.poolStale[name] {
			f.poolStale[name] = false
			cp := map[string]any{"name": name, "properties": map[string]any{"provisioningState": "Succeeded", "currentOrchestratorVersion": props["currentOrchestratorVersion"]}}
			_ = enc.Encode(cp)
			return
		}
		if props["provisioningState"] == "Upgrading" {
			props["provisioningState"] = "Succeeded"
			props["currentOrchestratorVersion"] = f.poolDone[name]
			props["orchestratorVersion"] = f.poolDone[name]
		}
		_ = enc.Encode(pool)
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

func setup(t *testing.T) (*fakeARM, *Executor) {
	f := &fakeARM{version: "1.30.5", state: "Succeeded", poolDone: map[string]string{}, poolStale: map[string]bool{},
		pools: map[string]map[string]any{"system": {"name": "system", "properties": map[string]any{
			"orchestratorVersion": "1.30.5", "currentOrchestratorVersion": "1.30.5", "provisioningState": "Succeeded", "count": 3.0, "mode": "System"}}},
		upgrades: []map[string]any{
			{"kubernetesVersion": "1.31.1"}, {"kubernetesVersion": "1.31.4"}, {"kubernetesVersion": "1.31.9", "isPreview": true}, {"kubernetesVersion": "1.32.0"},
		}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &Executor{HTTP: srv.Client(), BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "tok", nil },
		SubscriptionID: "sub-1", ResourceGroup: "rg-prod", Cluster: "payments", Poll: time.Millisecond,
		Nodes: func(context.Context) ([]inventory.Node, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			v, _ := kube.ParseVersion(f.pools["system"]["properties"].(map[string]any)["currentOrchestratorVersion"].(string))
			return []inventory.Node{{Name: "aks-system-0", Pool: "system", PoolType: inventory.PoolAKS, Version: v, Ready: true}}, nil
		}}
}

func TestAKSControlPlaneOnlyThenPools(t *testing.T) {
	f, x := setup(t)
	to := kube.MustParseVersion("1.31")
	log := &logSink{}
	if err := x.ControlPlane(context.Background(), to, log); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	put := f.puts[0]
	props := put["properties"].(map[string]any)
	if props["kubernetesVersion"] != "1.31.4" {
		t.Fatalf("newest GA patch must be chosen (not preview 1.31.9): %v", props["kubernetesVersion"])
	}
	if pool := props["agentPoolProfiles"].([]any)[0].(map[string]any); pool["orchestratorVersion"] != "1.30.5" {
		t.Fatalf("control-plane-only: pools must keep their version: %v", pool)
	}
	if _, ok := props["provisioningState"]; ok || put["location"] != "westeurope" {
		t.Fatalf("read-only fields must be stripped and location kept: %v", put)
	}
	if f.version != "1.31.4" {
		t.Fatalf("stale Succeeded must not end the wait early: version %s", f.version)
	}
	f.mu.Unlock()

	if err := x.DataPlane(context.Background(), to, log); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	puts, poolVersion := strings.Join(f.poolPuts, ","), f.pools["system"]["properties"].(map[string]any)["currentOrchestratorVersion"]
	f.mu.Unlock()
	if puts != "system 1.31.4" {
		t.Fatalf("pool puts: %v", puts)
	}
	if poolVersion != "1.31.4" {
		t.Fatal("pool wait ended before the version changed")
	}
	if err := x.ControlPlane(context.Background(), to, log); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.puts) != 1 {
		t.Fatalf("re-run must be a no-op: puts=%d", len(f.puts))
	}
}

func TestAKSGuards(t *testing.T) {
	f, x := setup(t)
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.32"), &logSink{}); err == nil || !strings.Contains(err.Error(), "one minor version") {
		t.Fatalf("skip guard: %v", err)
	}
	f.upgrades = []map[string]any{{"kubernetesVersion": "1.31.9", "isPreview": true}}
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), &logSink{}); err == nil || !strings.Contains(err.Error(), "no generally available") {
		t.Fatalf("preview-only must fail: %v", err)
	}
	x.Token = func(context.Context) (string, error) { return "bad", nil }
	if err := x.ControlPlane(context.Background(), kube.MustParseVersion("1.31"), &logSink{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("auth failure must surface: %v", err)
	}
}
