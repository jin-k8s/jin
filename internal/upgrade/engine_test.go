package upgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jin-k8s/jin/internal/check"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/plan"
	"github.com/jin-k8s/jin/internal/provider"
	"github.com/jin-k8s/jin/internal/runrecord"
)

var v = kube.MustParseVersion

// fakeCluster simulates a cluster whose control plane moves when ControlPlane runs.
type fakeCluster struct {
	mu        sync.Mutex
	version   kube.Version
	blockers  map[kube.Version]int
	failOnce  map[StageName]bool
	block     StageName // stage that blocks until ctx is cancelled
	calls     []string
	dataPlane int
}

func (f *fakeCluster) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeCluster) maybeFail(ctx context.Context, s StageName) error {
	f.mu.Lock()
	fail := f.failOnce[s]
	delete(f.failOnce, s)
	block := f.block == s
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if fail {
		return fmt.Errorf("simulated %s failure", s)
	}
	return nil
}

func (f *fakeCluster) ServerVersion(context.Context) (kube.Version, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, nil
}

func (f *fakeCluster) Plan(_ context.Context, target kube.Version) (*plan.Plan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := plan.Hop{From: f.version, To: target}
	for i := 0; i < f.blockers[target]; i++ {
		h.Findings = append(h.Findings, check.Finding{Severity: check.SeverityBlocker, Title: "PDB", Resource: fmt.Sprintf("pdb-%d", i)})
	}
	return &plan.Plan{Current: f.version, Target: target, Hops: []plan.Hop{h}}, nil
}

func (f *fakeCluster) Verify(ctx context.Context, want kube.Version, log Logger) error {
	f.record("verify " + want.String())
	if err := f.maybeFail(ctx, StageVerify); err != nil {
		return err
	}
	if f.version != want {
		return fmt.Errorf("cluster on %s, want %s", f.version, want)
	}
	log.Info("all nodes ready")
	return nil
}

func (f *fakeCluster) Executor() Executor { return f }
func (f *fakeCluster) Name() string       { return "fake" }

func (f *fakeCluster) ControlPlane(ctx context.Context, to kube.Version, log Logger) error {
	f.record("control-plane " + to.String())
	if err := f.maybeFail(ctx, StageControlPlane); err != nil {
		return err
	}
	f.mu.Lock()
	f.version = to
	f.mu.Unlock()
	log.Info("control plane on %s", to)
	return nil
}

func (f *fakeCluster) Addons(ctx context.Context, to kube.Version, log Logger) error {
	f.record("addons " + to.String())
	return f.maybeFail(ctx, StageAddons)
}

func (f *fakeCluster) DataPlane(ctx context.Context, to kube.Version, log Logger) error {
	f.record("data-plane " + to.String())
	f.mu.Lock()
	f.dataPlane++
	f.mu.Unlock()
	log.Progress(3, 3, "nodes", "nodes rolled")
	return f.maybeFail(ctx, StageDataPlane)
}

func planRun(from, to string, actions ...provider.DataPlaneAction) *runrecord.Record {
	r := runrecord.New(runrecord.KindPlanRun, time.Now())
	p := &plan.Plan{Current: v(from), Target: v(to)}
	i := 0
	for cur := v(from); cur.Less(v(to)); cur = cur.Next() {
		a := provider.DataPlaneFinal
		if i < len(actions) {
			a = actions[i]
		}
		p.Hops = append(p.Hops, plan.Hop{From: cur, To: cur.Next(), DataPlane: a})
		i++
	}
	r.Plan = p
	r.Finish(time.Now(), nil)
	return r
}

func newTestEngine(t *testing.T, fc *fakeCluster) (*Engine, *Store) {
	t.Helper()
	s := NewStore(t.TempDir())
	e := NewEngine(s, func(context.Context, *Upgrade) (Cluster, error) { return fc, nil })
	t.Cleanup(e.Shutdown)
	return e, s
}

func waitStatus(t *testing.T, e *Engine, id string, want Status) *Upgrade {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		u, err := e.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		_, busy := e.running[id]
		e.mu.Unlock()
		if u.Status == want && !busy {
			return u
		}
		time.Sleep(5 * time.Millisecond)
	}
	u, _ := e.Get(id)
	t.Fatalf("upgrade %s: status %s, want %s (error %q)", id, u.Status, want, u.Error)
	return nil
}

func create(t *testing.T, e *Engine, r *runrecord.Record) *Upgrade {
	t.Helper()
	u, err := e.Create(context.Background(), CreateRequest{Run: r, Mode: ModeDirect, Cluster: ClusterRef{Context: "test", Provider: "eks"}, By: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestHappyPathWithApprovalPerHop(t *testing.T) {
	fc := &fakeCluster{version: v("1.30")}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.32", provider.DataPlaneOptional, provider.DataPlaneFinal))

	if u.Status != StatusAwaitingApproval || u.CurrentHop != 1 || len(u.Hops) != 2 {
		t.Fatalf("created: %+v", u)
	}
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 2, By: "bob"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("approving the wrong hop must conflict, got %v", err)
	}

	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob", Comment: "CAB-123"}); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusAwaitingApproval)
	if u.CurrentHop != 2 || u.Hops[0].Status != StatusSucceeded || u.Hops[1].Status != StatusAwaitingApproval {
		t.Fatalf("after hop 1: %+v", u)
	}
	if u.Hops[0].Stages[3].Status != StatusSkipped {
		t.Fatalf("optional data plane must be skipped: %+v", u.Hops[0].Stages[3])
	}
	if fc.dataPlane != 0 {
		t.Fatal("data plane must not run on an optional hop")
	}
	if a := u.Hops[0].Approvals; len(a) != 1 || a[0].By != "bob" || a[0].Comment != "CAB-123" {
		t.Fatalf("approval not recorded: %+v", a)
	}

	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 2, By: "bob"}); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusSucceeded)
	if u.FinishedAt == nil || fc.version != v("1.32") || fc.dataPlane != 1 {
		t.Fatalf("final: %+v, cluster %s, data plane runs %d", u, fc.version, fc.dataPlane)
	}

	evs, err := e.Events(u.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var joined []string
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event sequence gap at %d: %+v", i, ev)
		}
		joined = append(joined, ev.Message)
	}
	all := strings.Join(joined, "\n")
	for _, want := range []string{"Awaiting approval for hop 1", "approved by bob: CAB-123", "Skipped data-plane", "Upgrade complete: 1.30 → 1.32"} {
		if !strings.Contains(all, want) {
			t.Fatalf("events missing %q:\n%s", want, all)
		}
	}
	var progress bool
	for _, ev := range evs {
		progress = progress || (ev.Progress != nil && ev.Progress.Done == 3)
	}
	if !progress {
		t.Fatal("progress event missing")
	}
}

func TestFailureThenRetryResumesFailedStage(t *testing.T) {
	fc := &fakeCluster{version: v("1.30"), failOnce: map[StageName]bool{StageAddons: true}}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.31"))
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob"}); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusFailed)
	if u.Hops[0].Stages[1].Status != StatusSucceeded || u.Hops[0].Stages[2].Status != StatusFailed || !strings.Contains(u.Error, "simulated add-ons failure") {
		t.Fatalf("failed state: %+v", u.Hops[0].Stages)
	}
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob"}); !errors.Is(err, ErrConflict) {
		t.Fatal("failed upgrade must not be approvable")
	}
	if _, err := e.Retry(u.ID, "carol", "fixed IAM"); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e, u.ID, StatusSucceeded)

	cp := 0
	for _, c := range fc.calls {
		if strings.HasPrefix(c, "control-plane") {
			cp++
		}
	}
	if cp != 1 {
		t.Fatalf("control plane must not re-run on retry after it succeeded: %v", fc.calls)
	}
}

func TestPreflightBlockersRequireOverride(t *testing.T) {
	fc := &fakeCluster{version: v("1.30"), blockers: map[kube.Version]int{v("1.31"): 2}}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.31"))
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob"}); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusFailed)
	if u.Hops[0].Stages[0].Status != StatusFailed || !strings.Contains(u.Error, "2 blocker(s)") || len(fc.calls) != 0 {
		t.Fatalf("preflight must stop before touching the cluster: %+v calls %v", u, fc.calls)
	}
	if _, err := e.Cancel(u.ID, "bob"); err != nil {
		t.Fatal(err)
	}

	u = create(t, e, planRun("1.30", "1.31"))
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob", OverrideBlockers: true, Comment: "single-replica dev app"}); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e, u.ID, StatusSucceeded)
}

func TestCancelWhileRunning(t *testing.T) {
	fc := &fakeCluster{version: v("1.30"), block: StageControlPlane}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.31"))
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		fc.mu.Lock()
		started := len(fc.calls) > 0
		fc.mu.Unlock()
		if started || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := e.Cancel(u.ID, "bob"); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusCancelled)
	if u.Hops[0].Stages[1].Status != StatusCancelled || u.FinishedAt == nil {
		t.Fatalf("cancelled state: %+v", u.Hops[0].Stages[1])
	}
	if _, err := e.Cancel(u.ID, "bob"); !errors.Is(err, ErrConflict) {
		t.Fatal("cancelling twice must conflict")
	}
}

func TestOneActiveUpgradePerCluster(t *testing.T) {
	fc := &fakeCluster{version: v("1.30")}
	e, _ := newTestEngine(t, fc)
	create(t, e, planRun("1.30", "1.31"))
	_, err := e.Create(context.Background(), CreateRequest{Run: planRun("1.30", "1.31"), Mode: ModeDirect, Cluster: ClusterRef{Context: "test"}, By: "x"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("second active upgrade must conflict, got %v", err)
	}
	failed := runrecord.New(runrecord.KindPlanRun, time.Now())
	failed.Finish(time.Now(), errors.New("boom"))
	if _, err := e.Create(context.Background(), CreateRequest{Run: failed, Mode: ModeDirect, Cluster: ClusterRef{Context: "other"}}); !errors.Is(err, ErrConflict) {
		t.Fatal("failed plan run must be rejected")
	}
}

func TestPreflightDetectsExternalChange(t *testing.T) {
	fc := &fakeCluster{version: v("1.29")}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.31"))
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob"}); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusFailed)
	if !strings.Contains(u.Error, "cluster changed since planning") {
		t.Fatalf("error = %q", u.Error)
	}
}

func TestResumeAfterRestart(t *testing.T) {
	fc := &fakeCluster{version: v("1.31")}
	s := NewStore(t.TempDir())
	e1 := NewEngine(s, func(context.Context, *Upgrade) (Cluster, error) { return fc, nil })
	u := create(t, e1, planRun("1.30", "1.31"))
	e1.Shutdown()

	// Simulate a crash in the middle of the control-plane stage.
	u, _ = s.Get(u.ID)
	u.Status = StatusRunning
	u.Hops[0].Status = StatusRunning
	u.Hops[0].Stages[0].Status = StatusSucceeded
	u.Hops[0].Stages[1].Status = StatusRunning
	if err := s.Save(u); err != nil {
		t.Fatal(err)
	}

	e2 := NewEngine(s, func(context.Context, *Upgrade) (Cluster, error) { return fc, nil })
	t.Cleanup(e2.Shutdown)
	if err := e2.Resume(); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, e2, u.ID, StatusSucceeded)
	evs, _ := e2.Events(u.ID, 0)
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			t.Fatal("sequence must continue across restarts")
		}
	}
}

func TestSubscribeReceivesLiveEvents(t *testing.T) {
	fc := &fakeCluster{version: v("1.30")}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.31"))
	ch, unsub := e.Subscribe(u.ID)
	defer unsub()
	if _, err := e.Approve(u.ID, ApproveRequest{Hop: 1, By: "bob"}); err != nil {
		t.Fatal(err)
	}
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if strings.Contains(ev.Message, "Upgrade complete") {
				return
			}
		case <-timeout:
			t.Fatal("no completion event received")
		}
	}
}

func TestMultiApproverPolicy(t *testing.T) {
	fc := &fakeCluster{version: v("1.30")}
	e, _ := newTestEngine(t, fc)
	u := create(t, e, planRun("1.30", "1.31"))
	req := ApproveRequest{Hop: 1, By: "alice@x", Subject: "oidc:alice", Required: 2, Policy: "production"}
	u, err := e.Approve(u.ID, req)
	fc.mu.Lock()
	calls := len(fc.calls)
	fc.mu.Unlock()
	if err != nil || u.Status != StatusAwaitingApproval || calls != 0 {
		t.Fatalf("first of two approvals must not start the hop: %v %+v", err, u)
	}
	if _, err := e.Approve(u.ID, req); !errors.Is(err, ErrConflict) {
		t.Fatalf("same approver twice must conflict: %v", err)
	}
	req.By, req.Subject = "bob@x", "oidc:bob"
	if _, err := e.Approve(u.ID, req); err != nil {
		t.Fatal(err)
	}
	u = waitStatus(t, e, u.ID, StatusSucceeded)
	if got := u.Hops[0].Approvers(); len(got) != 2 || u.Hops[0].Policy != "production" {
		t.Fatalf("approvers: %v policy %q", got, u.Hops[0].Policy)
	}
}
