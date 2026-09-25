package inventory

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/kube"
)

const helmManifest = `---
# Source: backup/templates/cronjob.yaml
apiVersion: batch/v1beta1
kind: CronJob
metadata:
  name: backup
  namespace: ops
---
# Source: backup/templates/sa.yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: backup
---
# only a comment
`

func encodeHelmRelease(t *testing.T, rel map[string]any) []byte {
	t.Helper()
	js, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(js); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))
}

func helmReleaseSecret(t *testing.T) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sh.helm.release.v1.backup.v3", Namespace: "ops",
			Labels: map[string]string{"owner": "helm", "status": "deployed", "name": "backup"},
		},
		Type: helmSecretType,
		Data: map[string][]byte{"release": encodeHelmRelease(t, map[string]any{
			"name": "backup", "namespace": "ops", "version": 3, "manifest": helmManifest,
			"chart":  map[string]any{"metadata": map[string]any{"name": "backup", "version": "1.2.0", "appVersion": "2.0"}},
			"config": map[string]any{"password": "must-not-leak"},
		})},
	}
}

func TestDecodeHelmReleaseUncompressed(t *testing.T) {
	data := []byte(base64.StdEncoding.EncodeToString([]byte(`{"name":"x","namespace":"y","version":1}`)))
	rel, err := decodeHelmRelease(data)
	if err != nil || rel.Name != "x" || rel.Version != 1 {
		t.Fatalf("got %+v, %v", rel, err)
	}
	if _, err := decodeHelmRelease([]byte("!!!")); err == nil {
		t.Fatal("expected base64 error")
	}
}

func TestParseManifest(t *testing.T) {
	hs, err := parseManifest(helmManifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(hs) != 2 || hs[0].Kind != "CronJob" || hs[0].Metadata.Namespace != "ops" || hs[1].Kind != "ServiceAccount" {
		t.Fatalf("got %+v", hs)
	}
}

func TestParseDeprecatedAPIMetrics(t *testing.T) {
	raw := []byte(`# HELP apiserver_requested_deprecated_apis Gauge of deprecated APIs that have been requested
# TYPE apiserver_requested_deprecated_apis gauge
apiserver_requested_deprecated_apis{group="flowcontrol.apiserver.k8s.io",removed_release="1.32",resource="flowschemas",subresource="",version="v1beta3"} 1
apiserver_requested_deprecated_apis{group="",removed_release="",resource="componentstatuses",subresource="",version="v1"} 1
apiserver_requested_deprecated_apis{group="batch",removed_release="1.25",resource="cronjobs",subresource="",version="v1beta1"} 0
apiserver_request_total{code="200"} 5
`)
	got := parseDeprecatedAPIMetrics(raw)
	if len(got) != 2 {
		t.Fatalf("got %d entries: %+v", len(got), got)
	}
	var fs DeprecatedAPIRequest
	for _, r := range got {
		if r.Resource == "flowschemas" {
			fs = r
		}
	}
	if fs.APIVersion() != "flowcontrol.apiserver.k8s.io/v1beta3" || fs.RemovedRelease != kube.MustParseVersion("1.32") {
		t.Fatalf("flowschemas entry = %+v", fs)
	}
}

func TestImageTagAndPools(t *testing.T) {
	cases := map[string]string{
		"602401143452.dkr.ecr.us-west-2.amazonaws.com/amazon-k8s-cni:v1.18.3-eksbuild.1": "v1.18.3-eksbuild.1",
		"registry:5000/coredns":                "",
		"coredns/coredns:1.11.1@sha256:abcdef": "1.11.1",
		"ghcr.io/org/img@sha256:abcdef":        "",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
	if _, pt := nodePool(map[string]string{"eks.amazonaws.com/compute-type": "auto", "karpenter.sh/nodepool": "general"}); pt != PoolEKSAuto {
		t.Errorf("auto mode detected as %s", pt)
	}
	if p, pt := nodePool(map[string]string{"karpenter.sh/nodepool": "general"}); pt != PoolKarpenter || p != "general" {
		t.Errorf("karpenter detected as %s/%s", p, pt)
	}
}

func TestDetectProvider(t *testing.T) {
	if DetectProvider("v1.30.4-eks-a737599", nil) != ProviderEKS {
		t.Error("eks gitVersion")
	}
	if DetectProvider("v1.30.3-gke.1639000", nil) != ProviderGKE {
		t.Error("gke gitVersion")
	}
	if DetectProvider("v1.30.3", []Node{{ProviderID: "azure:///subscriptions/x"}}) != ProviderAKS {
		t.Error("aks providerID")
	}
	if DetectProvider("v1.33.1", []Node{{ProviderID: "kind://docker/kind/kind-control-plane"}}) != ProviderKind {
		t.Error("kind providerID")
	}
	if DetectProvider("v1.33.1", nil) != ProviderGeneric {
		t.Error("generic")
	}
}

func TestCollect(t *testing.T) {
	kb, err := compat.Load()
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-1-1", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "general"}},
			Spec:       corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-0abc"},
			Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.23.17-eks-8ccc7ba"}},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "kube-proxy", Namespace: "kube-system"},
			Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "kube-proxy", Image: "602401143452.dkr.ecr.us-east-1.amazonaws.com/eks/kube-proxy:v1.24.10-minimal-eksbuild.2"},
			}}}},
		},
		&policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
			Status:     policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0, ExpectedPods: 2, CurrentHealthy: 2, DesiredHealthy: 2},
		},
		helmReleaseSecret(t),
	)
	fd := cs.Discovery().(*fakediscovery.FakeDiscovery)
	fd.FakedServerVersion = &version.Info{GitVersion: "v1.24.17-eks-5e0fdde"}
	fd.Resources = []*metav1.APIResourceList{{
		GroupVersion: "batch/v1",
		APIResources: []metav1.APIResource{
			{Name: "cronjobs", Kind: "CronJob", Namespaced: true, Verbs: []string{"get", "list"}},
			{Name: "cronjobs/status", Kind: "CronJob", Namespaced: true, Verbs: []string{"get"}},
		},
	}}

	scheme := runtime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	md := metadatafake.NewSimpleMetadataClient(scheme, &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"},
		ObjectMeta: metav1.ObjectMeta{Name: "report", Namespace: "finance", Annotations: map[string]string{
			lastAppliedAnnotation: `{"apiVersion":"batch/v1beta1","kind":"CronJob","metadata":{"name":"report"}}`,
		}},
	})

	fixed := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	c := &Collector{Client: cs, Metadata: md, KB: kb, Now: func() time.Time { return fixed }}
	inv, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if inv.Provider != ProviderEKS || inv.ServerVersion != kube.MustParseVersion("1.24") || !inv.CollectedAt.Equal(fixed) {
		t.Fatalf("header: %+v", inv)
	}
	if len(inv.Nodes) != 1 || inv.Nodes[0].PoolType != PoolEKSManaged || inv.Nodes[0].Version != kube.MustParseVersion("1.23") {
		t.Fatalf("nodes: %+v", inv.Nodes)
	}
	if len(inv.Addons) != 1 || inv.Addons[0].Name != "kube-proxy" || inv.Addons[0].Version != "v1.24.10-minimal-eksbuild.2" {
		t.Fatalf("addons: %+v", inv.Addons)
	}
	if len(inv.PDBs) != 1 || inv.PDBs[0].ExpectedPods != 2 {
		t.Fatalf("pdbs: %+v", inv.PDBs)
	}
	if len(inv.HelmReleases) != 1 || inv.HelmReleases[0].ChartVersion != "1.2.0" || inv.HelmReleases[0].Revision != 3 {
		t.Fatalf("helm: %+v", inv.HelmReleases)
	}

	var helmCron, appliedCron bool
	for _, m := range inv.Manifests {
		if m.Kind == "CronJob" && m.APIVersion == "batch/v1beta1" {
			switch m.Source {
			case SourceHelm:
				helmCron = m.Release == "backup" && m.ReleaseNamespace == "ops"
			case SourceLastApplied:
				appliedCron = m.Namespace == "finance" && m.Name == "report"
			}
		}
	}
	if !helmCron || !appliedCron {
		t.Fatalf("manifests: %+v", inv.Manifests)
	}

	b, _ := json.Marshal(inv)
	if bytes.Contains(b, []byte("must-not-leak")) {
		t.Fatal("helm values leaked into inventory")
	}

	// The fake discovery client has no REST client, so only the metrics step may warn.
	if len(inv.Warnings) != 1 {
		t.Fatalf("warnings: %v", inv.Warnings)
	}
}
