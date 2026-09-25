// Package snapshot captures a read-only picture of what runs in a cluster and how it is wired to its
// cloud. It feeds blue/green parity checks and cross-cloud migration assessments.
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
)

type Workload struct {
	Kind           string            `json:"kind"`
	Namespace      string            `json:"namespace"`
	Name           string            `json:"name"`
	Replicas       int32             `json:"replicas"`
	Ready          int32             `json:"ready"`
	Images         []string          `json:"images"`
	ServiceAccount string            `json:"serviceAccount,omitempty"`
	NodeSelector   map[string]string `json:"nodeSelector,omitempty"`
	// AffinityKeys are node-affinity label keys and values ("key=value").
	AffinityKeys []string `json:"affinityKeys,omitempty"`
}

func (w Workload) ID() string { return w.Kind + " " + w.Namespace + "/" + w.Name }

type Object struct {
	Namespace   string            `json:"namespace,omitempty"`
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

func (o Object) ID() string {
	if o.Namespace == "" {
		return o.Name
	}
	return o.Namespace + "/" + o.Name
}

type Service struct {
	Object
	Type string `json:"type"`
}

type Ingress struct {
	Object
	Class string `json:"class,omitempty"`
}

type StorageClass struct {
	Name        string `json:"name"`
	Provisioner string `json:"provisioner"`
	Default     bool   `json:"default,omitempty"`
}

type PVC struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	StorageClass string `json:"storageClass"`
	Bytes        int64  `json:"bytes"`
}

type SecretBackend struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Provider  string `json:"provider"`
}

type Snapshot struct {
	Context         string                    `json:"context"`
	CollectedAt     time.Time                 `json:"collectedAt"`
	ServerVersion   kube.Version              `json:"serverVersion"`
	Provider        string                    `json:"provider"`
	Namespaces      []string                  `json:"namespaces"`
	Workloads       []Workload                `json:"workloads"`
	ServiceAccounts []Object                  `json:"serviceAccounts"`
	Services        []Service                 `json:"services"`
	Ingresses       []Ingress                 `json:"ingresses"`
	IngressClasses  map[string]string         `json:"ingressClasses"`
	StorageClasses  []StorageClass            `json:"storageClasses"`
	PVCs            []PVC                     `json:"pvcs"`
	CRDs            []string                  `json:"crds"`
	SecretBackends  []SecretBackend           `json:"secretBackends"`
	HelmReleases    []inventory.HelmRelease   `json:"helmReleases"`
	Addons          []inventory.AddonInstance `json:"addons"`
	Nodes           []inventory.Node          `json:"nodes"`
	Warnings        []string                  `json:"warnings,omitempty"`
}

// cloudKey reports whether an annotation or label key binds an object to a cloud provider.
func cloudKey(k string) bool {
	for _, p := range []string{
		"eks.amazonaws.com/", "iam.gke.io/", "azure.workload.identity/",
		"service.beta.kubernetes.io/aws-", "service.beta.kubernetes.io/azure-", "cloud.google.com/", "networking.gke.io/",
		"alb.ingress.kubernetes.io/", "appgw.ingress.kubernetes.io/", "kubernetes.io/ingress.class", "external-dns.alpha.kubernetes.io/",
	} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func filterCloud(m map[string]string) map[string]string {
	var out map[string]string
	for k, v := range m {
		if cloudKey(k) {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// System namespaces are managed by the platform, not migrated.
func systemNamespace(ns string) bool {
	switch ns {
	case "kube-system", "kube-public", "kube-node-lease", "local-path-storage":
		return true
	}
	return strings.HasPrefix(ns, "gke-") || strings.HasPrefix(ns, "gmp-")
}

type Collector struct {
	Client  kubernetes.Interface
	Dynamic dynamic.Interface
	KB      *compat.KB
	Context string
}

func (c *Collector) Collect(ctx context.Context) (*Snapshot, error) {
	inv, err := (&inventory.Collector{Client: c.Client, KB: c.KB, Options: inventory.Options{SkipLastApplied: true, SkipMetrics: true}}).Collect(ctx)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{
		Context: c.Context, CollectedAt: inv.CollectedAt, ServerVersion: inv.ServerVersion, Provider: inv.Provider,
		HelmReleases: inv.HelmReleases, Addons: inv.Addons, Nodes: inv.Nodes, Warnings: inv.Warnings,
		IngressClasses: map[string]string{},
	}
	steps := []struct {
		name string
		fn   func(context.Context, *Snapshot) error
	}{
		{"namespaces", c.namespaces},
		{"workloads", c.workloads},
		{"service accounts", c.serviceAccounts},
		{"services", c.services},
		{"ingresses", c.ingresses},
		{"storage", c.storage},
		{"custom resource definitions", c.crds},
		{"secret backends", c.secretBackends},
	}
	for _, st := range steps {
		if err := st.fn(ctx, s); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			s.Warnings = append(s.Warnings, fmt.Sprintf("%s: %v", st.name, err))
		}
	}
	return s, nil
}

func (c *Collector) namespaces(ctx context.Context, s *Snapshot) error {
	l, err := c.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, n := range l.Items {
		if !systemNamespace(n.Name) {
			s.Namespaces = append(s.Namespaces, n.Name)
		}
	}
	sort.Strings(s.Namespaces)
	return nil
}

func podWorkload(kind, ns, name string, replicas, ready int32, spec corev1.PodSpec) Workload {
	w := Workload{Kind: kind, Namespace: ns, Name: name, Replicas: replicas, Ready: ready, ServiceAccount: spec.ServiceAccountName, NodeSelector: spec.NodeSelector}
	for _, ctr := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		w.Images = append(w.Images, ctr.Image)
	}
	if a := spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		for _, t := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, e := range t.MatchExpressions {
				w.AffinityKeys = append(w.AffinityKeys, e.Key+"="+strings.Join(e.Values, ","))
			}
		}
	}
	return w
}

func (c *Collector) workloads(ctx context.Context, s *Snapshot) error {
	var errs []error
	deps, err := c.Client.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, d := range deps.Items {
			if systemNamespace(d.Namespace) {
				continue
			}
			r := int32(1)
			if d.Spec.Replicas != nil {
				r = *d.Spec.Replicas
			}
			s.Workloads = append(s.Workloads, podWorkload("Deployment", d.Namespace, d.Name, r, d.Status.ReadyReplicas, d.Spec.Template.Spec))
		}
	}
	sts, err := c.Client.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, d := range sts.Items {
			if systemNamespace(d.Namespace) {
				continue
			}
			r := int32(1)
			if d.Spec.Replicas != nil {
				r = *d.Spec.Replicas
			}
			s.Workloads = append(s.Workloads, podWorkload("StatefulSet", d.Namespace, d.Name, r, d.Status.ReadyReplicas, d.Spec.Template.Spec))
		}
	}
	dss, err := c.Client.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		errs = append(errs, err)
	} else {
		for _, d := range dss.Items {
			if systemNamespace(d.Namespace) {
				continue
			}
			s.Workloads = append(s.Workloads, podWorkload("DaemonSet", d.Namespace, d.Name, d.Status.DesiredNumberScheduled, d.Status.NumberReady, d.Spec.Template.Spec))
		}
	}
	sort.Slice(s.Workloads, func(i, j int) bool { return s.Workloads[i].ID() < s.Workloads[j].ID() })
	return errors.Join(errs...)
}

func (c *Collector) serviceAccounts(ctx context.Context, s *Snapshot) error {
	l, err := c.Client.CoreV1().ServiceAccounts("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, sa := range l.Items {
		ann, lab := filterCloud(sa.Annotations), filterCloud(sa.Labels)
		if len(ann)+len(lab) == 0 || systemNamespace(sa.Namespace) {
			continue
		}
		s.ServiceAccounts = append(s.ServiceAccounts, Object{Namespace: sa.Namespace, Name: sa.Name, Annotations: ann, Labels: lab})
	}
	return nil
}

func (c *Collector) services(ctx context.Context, s *Snapshot) error {
	l, err := c.Client.CoreV1().Services("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, sv := range l.Items {
		if sv.Spec.Type != corev1.ServiceTypeLoadBalancer || systemNamespace(sv.Namespace) {
			continue
		}
		s.Services = append(s.Services, Service{Object: Object{Namespace: sv.Namespace, Name: sv.Name, Annotations: filterCloud(sv.Annotations)}, Type: string(sv.Spec.Type)})
	}
	return nil
}

func (c *Collector) ingresses(ctx context.Context, s *Snapshot) error {
	classes, err := c.Client.NetworkingV1().IngressClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, ic := range classes.Items {
		s.IngressClasses[ic.Name] = ic.Spec.Controller
	}
	l, err := c.Client.NetworkingV1().Ingresses("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, in := range l.Items {
		class := in.Annotations["kubernetes.io/ingress.class"]
		if in.Spec.IngressClassName != nil {
			class = *in.Spec.IngressClassName
		}
		s.Ingresses = append(s.Ingresses, Ingress{Object: Object{Namespace: in.Namespace, Name: in.Name, Annotations: filterCloud(in.Annotations)}, Class: class})
	}
	return nil
}

func (c *Collector) storage(ctx context.Context, s *Snapshot) error {
	scs, err := c.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	def := ""
	for _, sc := range scs.Items {
		isDef := sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true"
		if isDef {
			def = sc.Name
		}
		s.StorageClasses = append(s.StorageClasses, StorageClass{Name: sc.Name, Provisioner: sc.Provisioner, Default: isDef})
	}
	pvcs, err := c.Client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, p := range pvcs.Items {
		class := def
		if p.Spec.StorageClassName != nil {
			class = *p.Spec.StorageClassName
		}
		q := p.Status.Capacity[corev1.ResourceStorage]
		if q.IsZero() {
			q = p.Spec.Resources.Requests[corev1.ResourceStorage]
		}
		s.PVCs = append(s.PVCs, PVC{Namespace: p.Namespace, Name: p.Name, StorageClass: class, Bytes: q.Value()})
	}
	return nil
}

var (
	crdGVR            = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	secretBackendGVRs = []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}, "SecretStore"},
		{schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}, "ClusterSecretStore"},
		{schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1beta1", Resource: "secretstores"}, "SecretStore"},
		{schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1beta1", Resource: "clustersecretstores"}, "ClusterSecretStore"},
		{schema.GroupVersionResource{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Resource: "secretproviderclasses"}, "SecretProviderClass"},
	}
)

func (c *Collector) crds(ctx context.Context, s *Snapshot) error {
	if c.Dynamic == nil {
		return errors.New("dynamic client unavailable")
	}
	l, err := c.Dynamic.Resource(crdGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, i := range l.Items {
		s.CRDs = append(s.CRDs, i.GetName())
	}
	sort.Strings(s.CRDs)
	return nil
}

func (c *Collector) secretBackends(ctx context.Context, s *Snapshot) error {
	if c.Dynamic == nil {
		return nil
	}
	crds := map[string]bool{}
	for _, n := range s.CRDs {
		crds[n] = true
	}
	seen := map[string]bool{}
	for _, b := range secretBackendGVRs {
		if !crds[b.gvr.Resource+"."+b.gvr.Group] {
			continue
		}
		l, err := c.Dynamic.Resource(b.gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue // version not served; the other version is tried
		}
		for _, i := range l.Items {
			key := b.kind + i.GetNamespace() + "/" + i.GetName()
			if seen[key] {
				continue
			}
			seen[key] = true
			s.SecretBackends = append(s.SecretBackends, SecretBackend{Kind: b.kind, Namespace: i.GetNamespace(), Name: i.GetName(), Provider: secretProvider(b.kind, i)})
		}
	}
	return nil
}

func secretProvider(kind string, u unstructured.Unstructured) string {
	if kind == "SecretProviderClass" {
		p, _, _ := unstructured.NestedString(u.Object, "spec", "provider")
		return p
	}
	prov, _, _ := unstructured.NestedMap(u.Object, "spec", "provider")
	for k := range prov {
		return k
	}
	return ""
}
