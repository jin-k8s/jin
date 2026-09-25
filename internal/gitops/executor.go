package gitops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/upgrade"
)

// ClusterState is how the executor observes the pipeline applying a merged change.
type ClusterState interface {
	ServerVersion(ctx context.Context) (kube.Version, error)
	Nodes(ctx context.Context) ([]inventory.Node, error)
}

// AddonVersions resolves the add-on version to declare for a Kubernetes version and reads the
// version currently running. Either may be nil when the provider has no add-on catalogue.
type AddonVersions struct {
	Desired func(ctx context.Context, name string, to kube.Version) (string, error)
	Running func(ctx context.Context, name string) (string, error)
}

// Executor upgrades a cluster by raising one pull request per stage and waiting for the team's
// pipeline to apply it. It never changes the cluster directly.
type Executor struct {
	Repo         Repo
	BaseBranch   string
	Targets      []Target
	Cluster      ClusterState
	AddonCatalog AddonVersions
	UpgradeID    string
	// ClusterName appears in branch names, commit messages and PR titles.
	ClusterName string
	// Poll is the interval for PR and cluster polling. Defaults to 30s.
	Poll time.Duration
	// ApplyTimeout bounds waiting for the pipeline after a merge. Defaults to 3h.
	ApplyTimeout time.Duration
	// ReviewTimeout bounds each stage including review. Defaults to 7 days.
	ReviewTimeout time.Duration
}

func (x *Executor) Name() string { return "gitops" }

// StageTimeout lets pull requests wait for human review far longer than direct API calls.
func (x *Executor) StageTimeout(upgrade.StageName) time.Duration {
	if x.ReviewTimeout > 0 {
		return x.ReviewTimeout
	}
	return 7 * 24 * time.Hour
}

func (x *Executor) poll() time.Duration {
	if x.Poll > 0 {
		return x.Poll
	}
	return 30 * time.Second
}

func (x *Executor) applyTimeout() time.Duration {
	if x.ApplyTimeout > 0 {
		return x.ApplyTimeout
	}
	return 3 * time.Hour
}

func (x *Executor) targets(role string) []Target {
	var out []Target
	for _, t := range x.Targets {
		if t.Role == role {
			out = append(out, t)
		}
	}
	return out
}

func (x *Executor) ControlPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	if v, err := x.Cluster.ServerVersion(ctx); err == nil && v == to {
		log.Info("Control plane already on %s", to)
		return nil
	}
	targets := x.targets(RoleControlPlane)
	if len(targets) == 0 {
		return fmt.Errorf("no control-plane target configured for GitOps mode; add one in the cluster settings")
	}
	values := map[int]string{}
	for i := range targets {
		values[i] = to.String()
	}
	if err := x.proposeAndMerge(ctx, "control-plane", to, targets, values, log); err != nil {
		return err
	}
	return x.waitFor(ctx, log, fmt.Sprintf("the pipeline to upgrade the control plane to %s", to), func() (bool, string) {
		v, err := x.Cluster.ServerVersion(ctx)
		if err != nil {
			return false, err.Error()
		}
		return v == to, "API server reports " + v.String()
	})
}

func (x *Executor) Addons(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	targets := x.targets(RoleAddon)
	if len(targets) == 0 {
		log.Info("No add-on targets configured; add-ons are left to your existing process")
		return nil
	}
	if x.AddonCatalog.Desired == nil {
		log.Warn("No add-on version catalogue for this provider; skipping %d add-on target(s)", len(targets))
		return nil
	}
	var use []Target
	values := map[int]string{}
	for _, t := range targets {
		want, err := x.AddonCatalog.Desired(ctx, t.Name, to)
		if err != nil {
			return fmt.Errorf("resolve %s version for %s: %w", t.Name, to, err)
		}
		if want == "" {
			log.Warn("No default %s version published for %s; leaving it unchanged", t.Name, to)
			continue
		}
		values[len(use)] = want
		use = append(use, t)
	}
	if len(use) == 0 {
		return nil
	}
	if err := x.proposeAndMerge(ctx, "add-ons", to, use, values, log); err != nil {
		return err
	}
	if x.AddonCatalog.Running == nil {
		return nil
	}
	return x.waitFor(ctx, log, "the pipeline to roll out the add-on versions", func() (bool, string) {
		var pending []string
		for i, t := range use {
			cur, err := x.AddonCatalog.Running(ctx, t.Name)
			if err != nil || cur != values[i] {
				pending = append(pending, fmt.Sprintf("%s %s (want %s)", t.Name, cur, values[i]))
			}
		}
		return len(pending) == 0, "pending: " + strings.Join(pending, ", ")
	})
}

func (x *Executor) DataPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	targets := x.targets(RoleNodeGroup)
	if len(targets) > 0 {
		values := map[int]string{}
		for i := range targets {
			values[i] = to.String()
		}
		if err := x.proposeAndMerge(ctx, "data-plane", to, targets, values, log); err != nil {
			return err
		}
	} else {
		log.Info("No node-group targets configured; waiting for nodes to be replaced (Karpenter drift, Auto Mode or your own process)")
	}
	last := -1
	return x.waitFor(ctx, log, fmt.Sprintf("all nodes to run %s", to), func() (bool, string) {
		nodes, err := x.Cluster.Nodes(ctx)
		if err != nil {
			return false, err.Error()
		}
		done, total := 0, 0
		for _, n := range nodes {
			if n.PoolType == inventory.PoolEKSFargate {
				continue
			}
			total++
			if !n.Version.Less(to) && n.Ready {
				done++
			}
		}
		if done != last {
			last = done
			log.Progress(done, total, "nodes", "%d/%d nodes Ready on %s", done, total, to)
		}
		return total > 0 && done == total, fmt.Sprintf("%d/%d nodes on %s", done, total, to)
	})
}

func (x *Executor) branch(stage string, to kube.Version) string {
	return fmt.Sprintf("jin/%s/%s-%s", x.UpgradeID, to, stage)
}

// proposeAndMerge raises (or finds) the pull request for a stage and waits until it is merged.
// It is safe to re-run: an existing PR for the deterministic branch is reused.
func (x *Executor) proposeAndMerge(ctx context.Context, stage string, to kube.Version, targets []Target, values map[int]string, log upgrade.Logger) error {
	branch := x.branch(stage, to)
	pr, err := x.Repo.FindPR(ctx, branch)
	if err != nil {
		return fmt.Errorf("look up pull request: %w", err)
	}
	if pr == nil {
		pr, err = x.propose(ctx, stage, to, branch, targets, values, log)
		if err != nil || pr == nil {
			return err
		}
	} else {
		log.Info("Resuming with pull request #%d (%s)", pr.Number, pr.URL)
	}
	return x.waitMerged(ctx, pr, log)
}

func (x *Executor) propose(ctx context.Context, stage string, to kube.Version, branch string, targets []Target, values map[int]string, log upgrade.Logger) (*PR, error) {
	base := x.BaseBranch
	if base == "" {
		b, err := x.Repo.DefaultBranch(ctx)
		if err != nil {
			return nil, err
		}
		base = b
	}
	sha, err := x.Repo.HeadSHA(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", base, err)
	}
	ws, err := NewRepoWorkspace(ctx, x.Repo, sha)
	if err != nil {
		return nil, err
	}
	ov := &overlay{base: ws, edits: map[string][]byte{}}
	var changes []*Change
	for i, t := range targets {
		c, err := Apply(ov, t, values[i])
		if err != nil {
			return nil, err
		}
		if c == nil {
			continue
		}
		for f, b := range c.Files {
			ov.edits[f] = b
		}
		changes = append(changes, c)
	}
	if len(changes) == 0 {
		log.Info("%s already declares the %s versions; waiting for the pipeline", base, stage)
		return nil, nil
	}

	if err := x.Repo.CreateBranch(ctx, branch, sha); err != nil {
		return nil, fmt.Errorf("create branch %s: %w", branch, err)
	}
	files := make([]string, 0, len(ov.edits))
	for f := range ov.edits {
		files = append(files, f)
	}
	sort.Strings(files)
	msg := fmt.Sprintf("jin: upgrade %s %s to %s", x.ClusterName, stage, to)
	for _, f := range files {
		cur, blob, err := x.Repo.ReadFile(ctx, branch, f)
		if err != nil {
			return nil, err
		}
		if string(cur) == string(ov.edits[f]) {
			continue
		}
		if err := x.Repo.WriteFile(ctx, branch, f, msg, ov.edits[f], blob); err != nil {
			return nil, fmt.Errorf("commit %s: %w", f, err)
		}
	}
	pr, err := x.Repo.OpenPR(ctx, branch, base, msg, x.body(stage, to, changes))
	if err != nil {
		return nil, fmt.Errorf("open pull request: %w", err)
	}
	log.Info("Opened pull request #%d: %s", pr.Number, pr.URL)
	return pr, nil
}

func (x *Executor) body(stage string, to kube.Version, changes []*Change) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Jin upgrade `%s` for **%s**: %s stage of the hop to **%s**.\n\n", x.UpgradeID, x.ClusterName, stage, to)
	b.WriteString("| Field | From | To |\n|---|---|---|\n")
	for _, c := range changes {
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` |\n", c.Target.String(), c.From, c.To)
	}
	b.WriteString("\nMerge when ready. Jin waits for your pipeline to apply this change, verifies the cluster, and asks for approval before the next hop.\n")
	return b.String()
}

func (x *Executor) waitMerged(ctx context.Context, pr *PR, log upgrade.Logger) error {
	for i := 0; ; i++ {
		cur, err := x.Repo.GetPR(ctx, pr.Number)
		switch {
		case err != nil:
			log.Warn("Could not read pull request #%d: %v", pr.Number, err)
		case cur.Merged:
			log.Info("Pull request #%d merged", pr.Number)
			return nil
		case cur.State == "closed":
			return fmt.Errorf("pull request #%d was closed without merging; reopen and merge it, or cancel the upgrade", pr.Number)
		case i == 0 || i%20 == 0:
			log.Info("Waiting for pull request #%d to be reviewed and merged: %s", pr.Number, pr.URL)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(x.poll()):
		}
	}
}

func (x *Executor) waitFor(ctx context.Context, log upgrade.Logger, what string, done func() (bool, string)) error {
	deadline := time.Now().Add(x.applyTimeout())
	for i := 0; ; i++ {
		ok, status := done()
		if ok {
			return nil
		}
		if i == 0 || i%10 == 0 {
			log.Info("Waiting for %s (%s)", what, status)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for %s (%s); check the pipeline run", x.applyTimeout(), what, status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(x.poll()):
		}
	}
}
