// Package eks upgrades Amazon EKS clusters through the EKS API.
package eks

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/eks/types"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/upgrade"
)

// API is the subset of the EKS client the executor uses.
type API interface {
	DescribeCluster(context.Context, *eks.DescribeClusterInput, ...func(*eks.Options)) (*eks.DescribeClusterOutput, error)
	UpdateClusterVersion(context.Context, *eks.UpdateClusterVersionInput, ...func(*eks.Options)) (*eks.UpdateClusterVersionOutput, error)
	DescribeUpdate(context.Context, *eks.DescribeUpdateInput, ...func(*eks.Options)) (*eks.DescribeUpdateOutput, error)
	ListUpdates(context.Context, *eks.ListUpdatesInput, ...func(*eks.Options)) (*eks.ListUpdatesOutput, error)
	ListAddons(context.Context, *eks.ListAddonsInput, ...func(*eks.Options)) (*eks.ListAddonsOutput, error)
	DescribeAddon(context.Context, *eks.DescribeAddonInput, ...func(*eks.Options)) (*eks.DescribeAddonOutput, error)
	DescribeAddonVersions(context.Context, *eks.DescribeAddonVersionsInput, ...func(*eks.Options)) (*eks.DescribeAddonVersionsOutput, error)
	UpdateAddon(context.Context, *eks.UpdateAddonInput, ...func(*eks.Options)) (*eks.UpdateAddonOutput, error)
	ListNodegroups(context.Context, *eks.ListNodegroupsInput, ...func(*eks.Options)) (*eks.ListNodegroupsOutput, error)
	DescribeNodegroup(context.Context, *eks.DescribeNodegroupInput, ...func(*eks.Options)) (*eks.DescribeNodegroupOutput, error)
	UpdateNodegroupVersion(context.Context, *eks.UpdateNodegroupVersionInput, ...func(*eks.Options)) (*eks.UpdateNodegroupVersionOutput, error)
}

type NodeLister func(ctx context.Context) ([]inventory.Node, error)

type Executor struct {
	API     API
	Cluster string
	Nodes   NodeLister
	// Poll is the interval between status checks. Defaults to 20s.
	Poll time.Duration
	// NodeRollTimeout bounds waiting for Karpenter / Auto Mode to replace nodes. Defaults to 2h.
	NodeRollTimeout time.Duration
}

func (x *Executor) Name() string { return "eks-direct" }

func (x *Executor) poll() time.Duration {
	if x.Poll > 0 {
		return x.Poll
	}
	return 20 * time.Second
}

func (x *Executor) ControlPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	out, err := x.API.DescribeCluster(ctx, &eks.DescribeClusterInput{Name: aws.String(x.Cluster)})
	if err != nil {
		return fmt.Errorf("describe cluster %s: %w", x.Cluster, err)
	}
	c := out.Cluster
	cur, err := kube.ParseVersion(aws.ToString(c.Version))
	if err != nil {
		return err
	}
	if cur == to {
		log.Info("Control plane already on %s", to)
		return nil
	}
	if c.Status == types.ClusterStatusUpdating {
		id, err := x.inProgress(ctx, nil, nil, types.UpdateTypeVersionUpdate)
		if err != nil {
			return err
		}
		if id == "" {
			return errors.New("cluster is UPDATING with a non-version change in progress; retry once it is ACTIVE")
		}
		log.Info("Resuming: waiting for control-plane update %s already in progress", id)
		return x.wait(ctx, id, nil, nil, "Control-plane upgrade", log, nil)
	}
	if c.Status != types.ClusterStatusActive {
		return fmt.Errorf("cluster status is %s; control-plane upgrades require ACTIVE", c.Status)
	}
	if cur.Next() != to {
		return fmt.Errorf("control plane is on %s; EKS upgrades one minor version at a time, so %s is not reachable", cur, to)
	}

	up, err := x.API.UpdateClusterVersion(ctx, &eks.UpdateClusterVersionInput{Name: aws.String(x.Cluster), Version: aws.String(to.String())})
	if err != nil {
		return fmt.Errorf("start control-plane upgrade: %w", err)
	}
	id := aws.ToString(up.Update.Id)
	log.Info("Started control-plane upgrade %s → %s (EKS update %s). This step cannot be rolled back.", cur, to, id)
	return x.wait(ctx, id, nil, nil, "Control-plane upgrade", log, nil)
}

// addonPriority orders networking first so pods keep connectivity while other add-ons roll.
var addonPriority = map[string]int{"vpc-cni": 0, "kube-proxy": 1, "coredns": 2}

func (x *Executor) Addons(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	names, err := x.listAddons(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		log.Warn("No EKS managed add-ons found. Self-managed kube-proxy, CoreDNS and CNI must be updated through your own tooling.")
		return nil
	}
	sort.SliceStable(names, func(i, j int) bool {
		pi, iok := addonPriority[names[i]]
		pj, jok := addonPriority[names[j]]
		switch {
		case iok && jok:
			return pi < pj
		case iok != jok:
			return iok
		}
		return names[i] < names[j]
	})

	for _, name := range names {
		if err := x.updateAddon(ctx, name, to, log); err != nil {
			return fmt.Errorf("add-on %s: %w", name, err)
		}
	}
	return nil
}

func (x *Executor) updateAddon(ctx context.Context, name string, to kube.Version, log upgrade.Logger) error {
	d, err := x.API.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(x.Cluster), AddonName: aws.String(name)})
	if err != nil {
		return err
	}
	if d.Addon.Status == types.AddonStatusUpdating {
		id, err := x.inProgress(ctx, nil, aws.String(name), types.UpdateTypeAddonUpdate)
		if err != nil {
			return err
		}
		if id != "" {
			log.Info("Resuming: waiting for %s update %s already in progress", name, id)
			if err := x.wait(ctx, id, nil, aws.String(name), "Add-on "+name+" update", log, nil); err != nil {
				return err
			}
			if d, err = x.API.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(x.Cluster), AddonName: aws.String(name)}); err != nil {
				return err
			}
		}
	}
	cur := aws.ToString(d.Addon.AddonVersion)

	def, compatible, err := x.addonVersions(ctx, name, to)
	if err != nil {
		return err
	}
	if def == "" {
		log.Warn("EKS publishes no default %s version for %s; leaving %s unchanged", name, to, cur)
		return nil
	}
	switch {
	case cur == def:
		log.Info("%s already on %s (default for %s)", name, cur, to)
		return nil
	case compatible[cur] && compareAddonVersions(cur, def) > 0:
		log.Info("%s %s is newer than the %s default %s and compatible; leaving it", name, cur, to, def)
		return nil
	}

	up, err := x.API.UpdateAddon(ctx, &eks.UpdateAddonInput{
		ClusterName:  aws.String(x.Cluster),
		AddonName:    aws.String(name),
		AddonVersion: aws.String(def),
		// PRESERVE keeps configuration changes made outside the add-on API.
		ResolveConflicts: types.ResolveConflictsPreserve,
	})
	if err != nil {
		return err
	}
	id := aws.ToString(up.Update.Id)
	log.Info("Updating %s %s → %s (EKS update %s)", name, cur, def, id)
	return x.wait(ctx, id, nil, aws.String(name), "Add-on "+name+" update", log, nil)
}

// addonVersions returns the default version for k8s version to, plus all compatible versions.
func (x *Executor) addonVersions(ctx context.Context, name string, to kube.Version) (string, map[string]bool, error) {
	compatible := map[string]bool{}
	var def string
	in := &eks.DescribeAddonVersionsInput{AddonName: aws.String(name), KubernetesVersion: aws.String(to.String())}
	for {
		out, err := x.API.DescribeAddonVersions(ctx, in)
		if err != nil {
			return "", nil, err
		}
		for _, a := range out.Addons {
			for _, av := range a.AddonVersions {
				ver := aws.ToString(av.AddonVersion)
				for _, c := range av.Compatibilities {
					if aws.ToString(c.ClusterVersion) != to.String() {
						continue
					}
					compatible[ver] = true
					if c.DefaultVersion && def == "" {
						def = ver
					}
				}
			}
		}
		if out.NextToken == nil {
			return def, compatible, nil
		}
		in.NextToken = out.NextToken
	}
}

func (x *Executor) DataPlane(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	ngs, err := x.listNodegroups(ctx)
	if err != nil {
		return err
	}
	for _, ng := range ngs {
		if err := x.upgradeNodegroup(ctx, ng, to, log); err != nil {
			return fmt.Errorf("node group %s: %w", ng, err)
		}
	}
	return x.waitForReplacedNodes(ctx, to, log)
}

func (x *Executor) upgradeNodegroup(ctx context.Context, name string, to kube.Version, log upgrade.Logger) error {
	d, err := x.API.DescribeNodegroup(ctx, &eks.DescribeNodegroupInput{ClusterName: aws.String(x.Cluster), NodegroupName: aws.String(name)})
	if err != nil {
		return err
	}
	ng := d.Nodegroup
	cur, err := kube.ParseVersion(aws.ToString(ng.Version))
	if err != nil {
		return err
	}
	if cur == to {
		log.Info("Node group %s already on %s", name, to)
		return nil
	}
	progress := x.nodegroupProgress(ctx, name, to, log)
	switch ng.Status {
	case types.NodegroupStatusUpdating:
		id, err := x.inProgress(ctx, aws.String(name), nil, types.UpdateTypeVersionUpdate)
		if err != nil {
			return err
		}
		if id == "" {
			return errors.New("node group is UPDATING with a non-version change in progress; retry once it is ACTIVE")
		}
		log.Info("Resuming: waiting for node group %s update %s already in progress", name, id)
		return x.wait(ctx, id, aws.String(name), nil, "Node group "+name+" upgrade", log, progress)
	case types.NodegroupStatusActive:
	default:
		return fmt.Errorf("status is %s; version updates require ACTIVE", ng.Status)
	}

	up, err := x.API.UpdateNodegroupVersion(ctx, &eks.UpdateNodegroupVersionInput{
		ClusterName: aws.String(x.Cluster), NodegroupName: aws.String(name), Version: aws.String(to.String()),
	})
	if err != nil {
		return fmt.Errorf("start upgrade (node groups whose launch template pins a custom AMI must be upgraded by updating the template): %w", err)
	}
	id := aws.ToString(up.Update.Id)
	log.Info("Rolling node group %s %s → %s (EKS update %s); drains respect PodDisruptionBudgets", name, cur, to, id)
	return x.wait(ctx, id, aws.String(name), nil, "Node group "+name+" upgrade", log, progress)
}

func (x *Executor) nodegroupProgress(ctx context.Context, name string, to kube.Version, log upgrade.Logger) func() {
	if x.Nodes == nil {
		return nil
	}
	last := -1
	return func() {
		nodes, err := x.Nodes(ctx)
		if err != nil {
			return
		}
		done, total := 0, 0
		for _, n := range nodes {
			if n.PoolType != inventory.PoolEKSManaged || n.Pool != name {
				continue
			}
			total++
			if n.Version == to && n.Ready {
				done++
			}
		}
		if total > 0 && done != last {
			last = done
			log.Progress(done, total, "nodes", "Node group %s: %d/%d nodes Ready on %s", name, done, total, to)
		}
	}
}

// waitForReplacedNodes waits for nodes EKS replaces on its own (Karpenter drift, Auto Mode) and
// reports nodes Jin cannot roll (Fargate, self-managed).
func (x *Executor) waitForReplacedNodes(ctx context.Context, to kube.Version, log upgrade.Logger) error {
	if x.Nodes == nil {
		return nil
	}
	timeout := x.NodeRollTimeout
	if timeout == 0 {
		timeout = 2 * time.Hour
	}
	deadline := time.Now().Add(timeout)
	last := -1
	for {
		nodes, err := x.Nodes(ctx)
		if err != nil {
			return fmt.Errorf("list nodes: %w", err)
		}
		var auto, autoDone, fargate, selfManaged int
		for _, n := range nodes {
			old := n.Version.Less(to)
			switch n.PoolType {
			case inventory.PoolKarpenter, inventory.PoolEKSAuto:
				auto++
				if !old && n.Ready {
					autoDone++
				}
			case inventory.PoolEKSFargate:
				if old {
					fargate++
				}
			case inventory.PoolSelfManaged:
				if old {
					selfManaged++
				}
			}
		}
		if last == -1 {
			if fargate > 0 {
				log.Warn("%d Fargate node(s) still run an older version. Restart those workloads (kubectl rollout restart) when convenient; Jin does not restart workloads.", fargate)
			}
			if selfManaged > 0 {
				log.Warn("%d self-managed node(s) are on an older version. Roll them through your launch template / Auto Scaling group process.", selfManaged)
			}
		}
		if auto == 0 || autoDone == auto {
			if auto > 0 {
				log.Progress(autoDone, auto, "nodes", "Karpenter / Auto Mode nodes: %d/%d Ready on %s", autoDone, auto, to)
			}
			return nil
		}
		if autoDone != last {
			last = autoDone
			log.Progress(autoDone, auto, "nodes", "Waiting for Karpenter / Auto Mode to replace nodes: %d/%d Ready on %s", autoDone, auto, to)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d Karpenter / Auto Mode node(s) not replaced within %s. Check that the EC2NodeClass selects AMIs by alias (e.g. al2023@latest) so drift triggers, and that NodePool disruption budgets and PDBs allow replacement", auto-autoDone, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(x.poll()):
		}
	}
}

// wait polls an EKS update until it finishes. Transient API errors are tolerated three times in a row.
func (x *Executor) wait(ctx context.Context, id string, nodegroup, addon *string, what string, log upgrade.Logger, onTick func()) error {
	start := time.Now()
	errs := 0
	for i := 0; ; i++ {
		out, err := x.API.DescribeUpdate(ctx, &eks.DescribeUpdateInput{
			Name: aws.String(x.Cluster), UpdateId: aws.String(id), NodegroupName: nodegroup, AddonName: addon,
		})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs++
			if errs >= 3 {
				return fmt.Errorf("describe update %s: %w", id, err)
			}
			log.Warn("Could not read status of update %s (attempt %d/3): %v", id, errs, err)
		default:
			errs = 0
			switch out.Update.Status {
			case types.UpdateStatusSuccessful:
				if onTick != nil {
					onTick()
				}
				log.Info("%s completed in %s", what, time.Since(start).Round(time.Second))
				return nil
			case types.UpdateStatusFailed, types.UpdateStatusCancelled:
				return fmt.Errorf("%s %s: %s", what, strings.ToLower(string(out.Update.Status)), updateErrors(out.Update.Errors))
			}
			if onTick != nil {
				onTick()
			}
			if i > 0 && i%6 == 0 {
				log.Info("%s in progress (%s elapsed)", what, time.Since(start).Round(time.Second))
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(x.poll()):
		}
	}
}

func (x *Executor) inProgress(ctx context.Context, nodegroup, addon *string, typ types.UpdateType) (string, error) {
	in := &eks.ListUpdatesInput{Name: aws.String(x.Cluster), NodegroupName: nodegroup, AddonName: addon}
	for {
		out, err := x.API.ListUpdates(ctx, in)
		if err != nil {
			return "", fmt.Errorf("list updates: %w", err)
		}
		for _, id := range out.UpdateIds {
			d, err := x.API.DescribeUpdate(ctx, &eks.DescribeUpdateInput{Name: aws.String(x.Cluster), UpdateId: aws.String(id), NodegroupName: nodegroup, AddonName: addon})
			if err != nil {
				return "", err
			}
			if d.Update.Status == types.UpdateStatusInProgress && d.Update.Type == typ {
				return id, nil
			}
		}
		if out.NextToken == nil {
			return "", nil
		}
		in.NextToken = out.NextToken
	}
}

func (x *Executor) listAddons(ctx context.Context) ([]string, error) {
	var names []string
	in := &eks.ListAddonsInput{ClusterName: aws.String(x.Cluster)}
	for {
		out, err := x.API.ListAddons(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("list add-ons: %w", err)
		}
		names = append(names, out.Addons...)
		if out.NextToken == nil {
			return names, nil
		}
		in.NextToken = out.NextToken
	}
}

func (x *Executor) listNodegroups(ctx context.Context) ([]string, error) {
	var names []string
	in := &eks.ListNodegroupsInput{ClusterName: aws.String(x.Cluster)}
	for {
		out, err := x.API.ListNodegroups(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("list node groups: %w", err)
		}
		names = append(names, out.Nodegroups...)
		if out.NextToken == nil {
			sort.Strings(names)
			return names, nil
		}
		in.NextToken = out.NextToken
	}
}

func updateErrors(errs []types.ErrorDetail) string {
	if len(errs) == 0 {
		return "no error details returned by EKS"
	}
	var msgs []string
	for _, e := range errs {
		m := aws.ToString(e.ErrorMessage)
		if e.ErrorCode != "" {
			m = string(e.ErrorCode) + ": " + m
		}
		msgs = append(msgs, m)
	}
	return strings.Join(msgs, "; ")
}

var addonVersionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:-eksbuild\.(\d+))?`)

// compareAddonVersions compares versions like v1.18.3-eksbuild.1; unparsable versions compare equal.
func compareAddonVersions(a, b string) int {
	ma, mb := addonVersionRe.FindStringSubmatch(a), addonVersionRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0
	}
	for i := 1; i <= 4; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// DefaultAddonVersion is the version EKS publishes as default for an add-on on Kubernetes `to`.
func (x *Executor) DefaultAddonVersion(ctx context.Context, name string, to kube.Version) (string, error) {
	def, _, err := x.addonVersions(ctx, name, to)
	return def, err
}

// RunningAddonVersion is the add-on version currently installed.
func (x *Executor) RunningAddonVersion(ctx context.Context, name string) (string, error) {
	d, err := x.API.DescribeAddon(ctx, &eks.DescribeAddonInput{ClusterName: aws.String(x.Cluster), AddonName: aws.String(name)})
	if err != nil {
		return "", err
	}
	return aws.ToString(d.Addon.AddonVersion), nil
}
