package eks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
)

var v = kube.MustParseVersion

type pendingUpdate struct {
	typ      types.UpdateType
	polls    int // DescribeUpdate calls until completion
	fail     bool
	apply    func()
	status   types.UpdateStatus
	ng, addn string
}

// fakeEKS is a small stateful model of the EKS update APIs.
type fakeEKS struct {
	mu            sync.Mutex
	version       string
	status        types.ClusterStatus
	addons        map[string]string
	addonVersions map[string][]types.AddonVersionInfo
	nodegroups    map[string]string
	updates       map[string]*pendingUpdate
	nextID        int
	calls         []string
	failNext      bool
}

func (f *fakeEKS) call(s string) { f.calls = append(f.calls, s) }

func (f *fakeEKS) newUpdate(u *pendingUpdate) string {
	f.nextID++
	id := fmt.Sprintf("u-%d", f.nextID)
	u.status = types.UpdateStatusInProgress
	f.updates[id] = u
	return id
}

func (f *fakeEKS) DescribeCluster(_ context.Context, _ *eks.DescribeClusterInput, _ ...func(*eks.Options)) (*eks.DescribeClusterOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &eks.DescribeClusterOutput{Cluster: &types.Cluster{Version: aws.String(f.version), Status: f.status}}, nil
}

func (f *fakeEKS) UpdateClusterVersion(_ context.Context, in *eks.UpdateClusterVersionInput, _ ...func(*eks.Options)) (*eks.UpdateClusterVersionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.call("UpdateClusterVersion " + aws.ToString(in.Version))
	target := aws.ToString(in.Version)
	f.status = types.ClusterStatusUpdating
	id := f.newUpdate(&pendingUpdate{typ: types.UpdateTypeVersionUpdate, polls: 2, fail: f.failNext, apply: func() { f.version = target }})
	return &eks.UpdateClusterVersionOutput{Update: &types.Update{Id: aws.String(id)}}, nil
}

func (f *fakeEKS) DescribeUpdate(_ context.Context, in *eks.DescribeUpdateInput, _ ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.updates[aws.ToString(in.UpdateId)]
	if u == nil {
		return nil, errors.New("no such update")
	}
	if u.status == types.UpdateStatusInProgress {
		u.polls--
		if u.polls <= 0 {
			f.status = types.ClusterStatusActive
			if u.fail {
				u.status = types.UpdateStatusFailed
			} else {
				u.status = types.UpdateStatusSuccessful
				if u.apply != nil {
					u.apply()
				}
			}
		}
	}
	out := &types.Update{Id: in.UpdateId, Status: u.status, Type: u.typ}
	if u.status == types.UpdateStatusFailed {
		out.Errors = []types.ErrorDetail{{ErrorCode: types.ErrorCodeInsufficientFreeAddresses, ErrorMessage: aws.String("subnet has too few free IPs")}}
	}
	return &eks.DescribeUpdateOutput{Update: out}, nil
}

func (f *fakeEKS) ListUpdates(_ context.Context, in *eks.ListUpdatesInput, _ ...func(*eks.Options)) (*eks.ListUpdatesOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id, u := range f.updates {
		if u.ng == aws.ToString(in.NodegroupName) && u.addn == aws.ToString(in.AddonName) {
			ids = append(ids, id)
		}
	}
	return &eks.ListUpdatesOutput{UpdateIds: ids}, nil
}

func (f *fakeEKS) ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for n := range f.addons {
		names = append(names, n)
	}
	return &eks.ListAddonsOutput{Addons: names}, nil
}

func (f *fakeEKS) DescribeAddon(_ context.Context, in *eks.DescribeAddonInput, _ ...func(*eks.Options)) (*eks.DescribeAddonOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &eks.DescribeAddonOutput{Addon: &types.Addon{AddonName: in.AddonName, AddonVersion: aws.String(f.addons[aws.ToString(in.AddonName)]), Status: types.AddonStatusActive}}, nil
}

func (f *fakeEKS) DescribeAddonVersions(_ context.Context, in *eks.DescribeAddonVersionsInput, _ ...func(*eks.Options)) (*eks.DescribeAddonVersionsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &eks.DescribeAddonVersionsOutput{Addons: []types.AddonInfo{{AddonName: in.AddonName, AddonVersions: f.addonVersions[aws.ToString(in.AddonName)]}}}, nil
}

func (f *fakeEKS) UpdateAddon(_ context.Context, in *eks.UpdateAddonInput, _ ...func(*eks.Options)) (*eks.UpdateAddonOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ver := aws.ToString(in.AddonName), aws.ToString(in.AddonVersion)
	f.call("UpdateAddon " + name + " " + ver + " " + string(in.ResolveConflicts))
	id := f.newUpdate(&pendingUpdate{typ: types.UpdateTypeAddonUpdate, polls: 1, addn: name, apply: func() { f.addons[name] = ver }})
	return &eks.UpdateAddonOutput{Update: &types.Update{Id: aws.String(id)}}, nil
}

func (f *fakeEKS) ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for n := range f.nodegroups {
		names = append(names, n)
	}
	return &eks.ListNodegroupsOutput{Nodegroups: names}, nil
}

func (f *fakeEKS) DescribeNodegroup(_ context.Context, in *eks.DescribeNodegroupInput, _ ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &eks.DescribeNodegroupOutput{Nodegroup: &types.Nodegroup{Version: aws.String(f.nodegroups[aws.ToString(in.NodegroupName)]), Status: types.NodegroupStatusActive}}, nil
}

func (f *fakeEKS) UpdateNodegroupVersion(_ context.Context, in *eks.UpdateNodegroupVersionInput, _ ...func(*eks.Options)) (*eks.UpdateNodegroupVersionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ver := aws.ToString(in.NodegroupName), aws.ToString(in.Version)
	f.call("UpdateNodegroupVersion " + name + " " + ver)
	id := f.newUpdate(&pendingUpdate{typ: types.UpdateTypeVersionUpdate, polls: 2, ng: name, apply: func() { f.nodegroups[name] = ver }})
	return &eks.UpdateNodegroupVersionOutput{Update: &types.Update{Id: aws.String(id)}}, nil
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) add(s string)            { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *logSink) Info(f string, a ...any) { l.add("INFO " + fmt.Sprintf(f, a...)) }
func (l *logSink) Warn(f string, a ...any) { l.add("WARN " + fmt.Sprintf(f, a...)) }
func (l *logSink) Progress(d, t int, _, f string, a ...any) {
	l.add(fmt.Sprintf("PROGRESS %d/%d ", d, t) + fmt.Sprintf(f, a...))
}
func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func compat(k8s string, versions ...string) []types.AddonVersionInfo {
	var out []types.AddonVersionInfo
	for i, ver := range versions {
		out = append(out, types.AddonVersionInfo{AddonVersion: aws.String(ver), Compatibilities: []types.Compatibility{{ClusterVersion: aws.String(k8s), DefaultVersion: i == 0}}})
	}
	return out
}

func newFake() *fakeEKS {
	return &fakeEKS{version: "1.30", status: types.ClusterStatusActive, addons: map[string]string{}, addonVersions: map[string][]types.AddonVersionInfo{},
		nodegroups: map[string]string{}, updates: map[string]*pendingUpdate{}}
}

func TestControlPlaneUpgradeIsIdempotent(t *testing.T) {
	f := newFake()
	x := &Executor{API: f, Cluster: "dev", Poll: time.Millisecond}
	log := &logSink{}
	if err := x.ControlPlane(context.Background(), v("1.31"), log); err != nil {
		t.Fatal(err)
	}
	if f.version != "1.31" || len(f.calls) != 1 {
		t.Fatalf("version %s calls %v", f.version, f.calls)
	}
	if err := x.ControlPlane(context.Background(), v("1.31"), log); err != nil || len(f.calls) != 1 {
		t.Fatalf("second run must be a no-op: %v %v", err, f.calls)
	}
	if !strings.Contains(log.String(), "cannot be rolled back") {
		t.Fatalf("log: %s", log)
	}
}

func TestControlPlaneResumesInProgressUpdate(t *testing.T) {
	f := newFake()
	f.status = types.ClusterStatusUpdating
	f.updates["u-9"] = &pendingUpdate{typ: types.UpdateTypeVersionUpdate, polls: 2, status: types.UpdateStatusInProgress, apply: func() { f.version = "1.31" }}
	x := &Executor{API: f, Cluster: "dev", Poll: time.Millisecond}
	log := &logSink{}
	if err := x.ControlPlane(context.Background(), v("1.31"), log); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 || f.version != "1.31" || !strings.Contains(log.String(), "Resuming") {
		t.Fatalf("must wait on the existing update, calls %v log %s", f.calls, log)
	}
}

func TestControlPlaneGuards(t *testing.T) {
	f := newFake()
	x := &Executor{API: f, Cluster: "dev", Poll: time.Millisecond}
	if err := x.ControlPlane(context.Background(), v("1.32"), &logSink{}); err == nil || !strings.Contains(err.Error(), "one minor version at a time") {
		t.Fatalf("skipping a minor must fail: %v", err)
	}
	f.failNext = true
	err := x.ControlPlane(context.Background(), v("1.31"), &logSink{})
	if err == nil || !strings.Contains(err.Error(), "InsufficientFreeAddresses: subnet has too few free IPs") {
		t.Fatalf("EKS failure details must surface: %v", err)
	}
}

func TestAddons(t *testing.T) {
	f := newFake()
	f.addons = map[string]string{"coredns": "v1.11.1-eksbuild.9", "kube-proxy": "v1.30.0-eksbuild.3", "vpc-cni": "v1.19.0-eksbuild.1", "custom": "v1.0.0"}
	f.addonVersions = map[string][]types.AddonVersionInfo{
		"kube-proxy": compat("1.31", "v1.31.2-eksbuild.3", "v1.31.0-eksbuild.2"),
		"vpc-cni":    compat("1.31", "v1.18.5-eksbuild.1", "v1.19.0-eksbuild.1"),
		"coredns":    compat("1.31", "v1.11.3-eksbuild.1"),
	}
	x := &Executor{API: f, Cluster: "dev", Poll: time.Millisecond}
	log := &logSink{}
	if err := x.Addons(context.Background(), v("1.31"), log); err != nil {
		t.Fatal(err)
	}
	want := []string{"UpdateAddon kube-proxy v1.31.2-eksbuild.3 PRESERVE", "UpdateAddon coredns v1.11.3-eksbuild.1 PRESERVE"}
	if strings.Join(f.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	out := log.String()
	for _, s := range []string{"vpc-cni v1.19.0-eksbuild.1 is newer than the 1.31 default", "no default custom version"} {
		if !strings.Contains(out, s) {
			t.Errorf("log missing %q:\n%s", s, out)
		}
	}
}

func TestDataPlaneRollsNodegroupsAndWaitsForKarpenter(t *testing.T) {
	f := newFake()
	f.nodegroups = map[string]string{"general": "1.30", "already": "1.31"}
	polls := 0
	nodes := func(context.Context) ([]inventory.Node, error) {
		polls++
		kv := v("1.30")
		if polls > 3 {
			kv = v("1.31")
		}
		return []inventory.Node{
			{Name: "k1", PoolType: inventory.PoolKarpenter, Version: kv, Ready: true},
			{Name: "g1", PoolType: inventory.PoolEKSManaged, Pool: "general", Version: v("1.31"), Ready: true},
			{Name: "f1", PoolType: inventory.PoolEKSFargate, Version: v("1.30"), Ready: true},
		}, nil
	}
	x := &Executor{API: f, Cluster: "dev", Poll: time.Millisecond, Nodes: nodes}
	log := &logSink{}
	if err := x.DataPlane(context.Background(), v("1.31"), log); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0] != "UpdateNodegroupVersion general 1.31" {
		t.Fatalf("calls %v", f.calls)
	}
	out := log.String()
	for _, s := range []string{"Node group already already on 1.31", "PROGRESS 1/1 Node group general", "1 Fargate node(s)", "Karpenter / Auto Mode nodes: 1/1 Ready"} {
		if !strings.Contains(out, s) {
			t.Errorf("log missing %q:\n%s", s, out)
		}
	}
}

func TestKarpenterTimeout(t *testing.T) {
	f := newFake()
	nodes := func(context.Context) ([]inventory.Node, error) {
		return []inventory.Node{{Name: "k1", PoolType: inventory.PoolKarpenter, Version: v("1.30"), Ready: true}}, nil
	}
	x := &Executor{API: f, Cluster: "dev", Poll: time.Millisecond, Nodes: nodes, NodeRollTimeout: 5 * time.Millisecond}
	err := x.DataPlane(context.Background(), v("1.31"), &logSink{})
	if err == nil || !strings.Contains(err.Error(), "amiSelectorTerms") && !strings.Contains(err.Error(), "alias") {
		t.Fatalf("expected actionable timeout, got %v", err)
	}
}

func TestCompareAddonVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.19.0-eksbuild.1", "v1.18.5-eksbuild.1", 1},
		{"v1.11.1-eksbuild.9", "v1.11.3-eksbuild.1", -1},
		{"v1.31.2-eksbuild.3", "v1.31.2-eksbuild.3", 0},
		{"v1.31.2-eksbuild.2", "v1.31.2-eksbuild.10", -1},
		{"weird", "v1.0.0", 0},
	}
	for _, c := range cases {
		if got := compareAddonVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
