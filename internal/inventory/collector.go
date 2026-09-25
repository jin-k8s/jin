package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/kube"
)

const (
	lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"
	helmSelector          = "owner=helm,status=deployed"
	listPageSize          = 250
	// Helm release secrets can each approach 1 MiB, so page them small.
	helmPageSize = 20
)

type Options struct {
	SkipHelm        bool
	SkipLastApplied bool
	SkipMetrics     bool
}

// Collector performs read-only (get/list) calls against the Kubernetes API.
type Collector struct {
	Client   kubernetes.Interface
	Metadata metadata.Interface
	KB       *compat.KB
	Options  Options
	Now      func() time.Time
	// Progress, when set, is called with the name of each collection step as it starts.
	Progress func(step string)
}

func (c *Collector) Collect(ctx context.Context) (*Inventory, error) {
	sv, err := c.Client.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("get server version: %w", err)
	}
	v, err := kube.ParseVersion(sv.GitVersion)
	if err != nil {
		return nil, err
	}

	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	inv := &Inventory{CollectedAt: now().UTC(), ServerVersion: v, ServerGitVersion: sv.GitVersion}

	steps := []struct {
		name string
		skip bool
		fn   func(context.Context, *Inventory) error
	}{
		{"nodes", false, c.collectNodes},
		{"add-ons", false, c.collectAddons},
		{"pod disruption budgets", false, c.collectPDBs},
		{"helm releases", c.Options.SkipHelm, c.collectHelm},
		{"last-applied manifests", c.Options.SkipLastApplied, c.collectLastApplied},
		{"apiserver deprecated-API metrics", c.Options.SkipMetrics, c.collectMetrics},
	}
	for _, s := range steps {
		if s.skip {
			continue
		}
		if c.Progress != nil {
			c.Progress(s.name)
		}
		if err := s.fn(ctx, inv); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			inv.Warnings = append(inv.Warnings, fmt.Sprintf("%s: %v", s.name, err))
		}
	}

	inv.Provider = DetectProvider(inv.ServerGitVersion, inv.Nodes)
	sortInventory(inv)
	return inv, nil
}

func (c *Collector) collectNodes(ctx context.Context, inv *Inventory) error {
	nodes, err := ListNodes(ctx, c.Client)
	inv.Nodes = nodes
	return err
}

// ListNodes returns every node with its version, pool and readiness.
func ListNodes(ctx context.Context, client kubernetes.Interface) ([]Node, error) {
	var out []Node
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := client.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return out, err
		}
		for i := range list.Items {
			out = append(out, nodeFrom(&list.Items[i]))
		}
		if list.Continue == "" {
			return out, nil
		}
		opts.Continue = list.Continue
	}
}

func nodeFrom(n *corev1.Node) Node {
	kv := n.Status.NodeInfo.KubeletVersion
	v, _ := kube.ParseVersion(kv)
	pool, poolType := nodePool(n.Labels)
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	return Node{Name: n.Name, KubeletVersion: kv, Version: v, ProviderID: n.Spec.ProviderID, Pool: pool, PoolType: poolType,
		Ready: ready, Unschedulable: n.Spec.Unschedulable}
}

func (c *Collector) collectAddons(ctx context.Context, inv *Inventory) error {
	var errs []error
	for _, a := range c.KB.Addons() {
		for _, w := range a.Workloads {
			for _, ns := range w.Namespaces {
				for _, name := range w.Names {
					containers, found, err := c.podContainers(ctx, w.Kind, ns, name)
					if err != nil {
						errs = append(errs, fmt.Errorf("%s %s/%s: %w", w.Kind, ns, name, err))
						continue
					}
					if !found {
						continue
					}
					img := pickImage(containers, w.Container)
					inv.Addons = append(inv.Addons, AddonInstance{
						Name: a.Name, Kind: w.Kind, Namespace: ns, Workload: name, Image: img, Version: imageTag(img),
					})
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Collector) podContainers(ctx context.Context, kind, ns, name string) ([]corev1.Container, bool, error) {
	var (
		spec corev1.PodSpec
		err  error
	)
	switch kind {
	case "Deployment":
		var d *appsv1.Deployment
		d, err = c.Client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			spec = d.Spec.Template.Spec
		}
	case "DaemonSet":
		var d *appsv1.DaemonSet
		d, err = c.Client.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			spec = d.Spec.Template.Spec
		}
	default:
		return nil, false, fmt.Errorf("unsupported workload kind %q", kind)
	}
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return spec.Containers, true, nil
}

func pickImage(containers []corev1.Container, want string) string {
	for _, ctr := range containers {
		if ctr.Name == want {
			return ctr.Image
		}
	}
	if len(containers) > 0 {
		return containers[0].Image
	}
	return ""
}

func (c *Collector) collectPDBs(ctx context.Context, inv *Inventory) error {
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := c.Client.PolicyV1().PodDisruptionBudgets("").List(ctx, opts)
		if err != nil {
			return err
		}
		for _, p := range list.Items {
			inv.PDBs = append(inv.PDBs, PDB{
				Namespace:          p.Namespace,
				Name:               p.Name,
				DisruptionsAllowed: p.Status.DisruptionsAllowed,
				ExpectedPods:       p.Status.ExpectedPods,
				CurrentHealthy:     p.Status.CurrentHealthy,
				DesiredHealthy:     p.Status.DesiredHealthy,
			})
		}
		if list.Continue == "" {
			return nil
		}
		opts.Continue = list.Continue
	}
}

// collectHelm reads only apiVersion/kind/name from rendered manifests; values and secret data are never retained.
func (c *Collector) collectHelm(ctx context.Context, inv *Inventory) error {
	opts := metav1.ListOptions{LabelSelector: helmSelector, Limit: helmPageSize}
	for {
		list, err := c.Client.CoreV1().Secrets("").List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range list.Items {
			s := &list.Items[i]
			if s.Type != helmSecretType {
				continue
			}
			rel, err := decodeHelmRelease(s.Data["release"])
			if err != nil {
				inv.Warnings = append(inv.Warnings, fmt.Sprintf("helm release secret %s/%s: %v", s.Namespace, s.Name, err))
				continue
			}
			inv.HelmReleases = append(inv.HelmReleases, HelmRelease{
				Name:         rel.Name,
				Namespace:    rel.Namespace,
				Revision:     rel.Version,
				Chart:        rel.Chart.Metadata.Name,
				ChartVersion: rel.Chart.Metadata.Version,
				AppVersion:   rel.Chart.Metadata.AppVersion,
			})
			headers, err := parseManifest(rel.Manifest)
			if err != nil {
				inv.Warnings = append(inv.Warnings, fmt.Sprintf("helm release %s/%s: some manifest documents could not be parsed: %v", rel.Namespace, rel.Name, err))
			}
			origin := fmt.Sprintf("helm release %s/%s (rev %d)", rel.Namespace, rel.Name, rel.Version)
			for _, h := range headers {
				inv.Manifests = append(inv.Manifests, ManifestRef{
					Source: SourceHelm, Origin: origin, APIVersion: h.APIVersion, Kind: h.Kind,
					Namespace: h.Metadata.Namespace, Name: h.Metadata.Name,
					Release: rel.Name, ReleaseNamespace: rel.Namespace,
				})
			}
		}
		if list.Continue == "" {
			return nil
		}
		opts.Continue = list.Continue
	}
}

func (c *Collector) collectLastApplied(ctx context.Context, inv *Inventory) error {
	if c.Metadata == nil {
		return errors.New("metadata client unavailable")
	}
	groups, lists, err := c.Client.Discovery().ServerGroupsAndResources()
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) || lists == nil {
			return err
		}
		inv.Warnings = append(inv.Warnings, fmt.Sprintf("API discovery partially failed: %v", err))
	}

	// List through each group's preferred version. Listing through a deprecated version would
	// itself show up in apiserver_requested_deprecated_apis, the metric Jin reports on, and the
	// annotation is the same whichever version serves the object. Non-preferred versions are
	// used only for resources the preferred version does not serve.
	preferred := map[string]string{}
	for _, g := range groups {
		if g != nil {
			preferred[g.Name] = g.PreferredVersion.GroupVersion
		}
	}
	isPreferred := func(gv schema.GroupVersion) bool {
		p, ok := preferred[gv.Group]
		return !ok || p == gv.String()
	}

	kinds := c.KB.RemovedKinds()
	seen := map[string]bool{}
	var gvrs []schema.GroupVersionResource
	for _, pass := range []bool{true, false} {
		for _, l := range lists {
			gv, err := schema.ParseGroupVersion(l.GroupVersion)
			if err != nil || isPreferred(gv) != pass {
				continue
			}
			for _, r := range l.APIResources {
				// Events are high-volume and never declared in manifests.
				if strings.Contains(r.Name, "/") || !kinds[r.Kind] || r.Kind == "Event" || !slices.Contains(r.Verbs, "list") {
					continue
				}
				key := gv.Group + "/" + r.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				gvrs = append(gvrs, gv.WithResource(r.Name))
			}
		}
	}
	sort.Slice(gvrs, func(i, j int) bool { return gvrs[i].String() < gvrs[j].String() })

	var errs []error
	for _, gvr := range gvrs {
		if err := c.lastAppliedFor(ctx, gvr, inv); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", gvr.String(), err))
		}
	}
	return errors.Join(errs...)
}

func (c *Collector) lastAppliedFor(ctx context.Context, gvr schema.GroupVersionResource, inv *Inventory) error {
	opts := metav1.ListOptions{Limit: listPageSize}
	for {
		list, err := c.Metadata.Resource(gvr).List(ctx, opts)
		if err != nil {
			return err
		}
		for _, item := range list.Items {
			ann := item.Annotations[lastAppliedAnnotation]
			if ann == "" {
				continue
			}
			var h manifestHeader
			if err := json.Unmarshal([]byte(ann), &h); err != nil || h.APIVersion == "" || h.Kind == "" {
				continue
			}
			inv.Manifests = append(inv.Manifests, ManifestRef{
				Source: SourceLastApplied, Origin: "kubectl apply", APIVersion: h.APIVersion, Kind: h.Kind,
				Namespace: item.Namespace, Name: item.Name,
			})
		}
		if list.Continue == "" {
			return nil
		}
		opts.Continue = list.Continue
	}
}

func (c *Collector) collectMetrics(ctx context.Context, inv *Inventory) error {
	rc := c.Client.Discovery().RESTClient()
	if rc == nil {
		return errors.New("REST client unavailable")
	}
	raw, err := rc.Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("read /metrics (requires RBAC get on nonResourceURL /metrics): %w", err)
	}
	inv.DeprecatedAPIRequests = parseDeprecatedAPIMetrics(raw)
	return nil
}

func sortInventory(inv *Inventory) {
	sort.Slice(inv.Nodes, func(i, j int) bool { return inv.Nodes[i].Name < inv.Nodes[j].Name })
	sort.Slice(inv.Addons, func(i, j int) bool {
		if inv.Addons[i].Name != inv.Addons[j].Name {
			return inv.Addons[i].Name < inv.Addons[j].Name
		}
		return inv.Addons[i].Namespace < inv.Addons[j].Namespace
	})
	sort.Slice(inv.HelmReleases, func(i, j int) bool {
		a, b := inv.HelmReleases[i], inv.HelmReleases[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	sort.Slice(inv.Manifests, func(i, j int) bool {
		a, b := inv.Manifests[i], inv.Manifests[j]
		return a.Source+a.Origin+a.Kind+a.Namespace+a.Name < b.Source+b.Origin+b.Kind+b.Namespace+b.Name
	})
	sort.Slice(inv.PDBs, func(i, j int) bool {
		return inv.PDBs[i].Namespace+"/"+inv.PDBs[i].Name < inv.PDBs[j].Namespace+"/"+inv.PDBs[j].Name
	})
}
