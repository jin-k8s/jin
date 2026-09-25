package migrate

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/snapshot"
)

func ptr[T any](v T) *T { return &v }

func crd(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apiextensions.k8s.io/v1")
	u.SetKind("CustomResourceDefinition")
	u.SetName(name)
	return u
}

func eksCluster(t *testing.T, ctxName string, readyReplicas int32, withPVC bool) *snapshot.Snapshot {
	t.Helper()
	objs := []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"eks.amazonaws.com/nodegroup": "general"}},
			Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.31.2-eks-1"}}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments", Annotations: map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::1:role/api", "unrelated": "x"}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "payments"},
			Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(3)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				ServiceAccountName: "api",
				NodeSelector:       map[string]string{"karpenter.sh/capacity-type": "spot"},
				Containers:         []corev1.Container{{Name: "api", Image: "111122223333.dkr.ecr.ap-south-1.amazonaws.com/payments/api:2.4.1"}, {Name: "proxy", Image: "envoyproxy/envoy:v1.31"}},
			}}},
			Status: appsv1.DeploymentStatus{ReadyReplicas: readyReplicas}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "api-nlb", Namespace: "payments", Annotations: map[string]string{"service.beta.kubernetes.io/aws-load-balancer-type": "external"}},
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}},
		&networkingv1.IngressClass{ObjectMeta: metav1.ObjectMeta{Name: "alb"}, Spec: networkingv1.IngressClassSpec{Controller: "ingress.k8s.aws/alb"}},
		&networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "payments", Annotations: map[string]string{"alb.ingress.kubernetes.io/scheme": "internet-facing"}},
			Spec: networkingv1.IngressSpec{IngressClassName: ptr("alb")}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "gp3", Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"}}, Provisioner: "ebs.csi.aws.com"},
	}
	if withPVC {
		objs = append(objs, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "ledger-data", Namespace: "payments"},
			Status: corev1.PersistentVolumeClaimStatus{Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("100Gi")}}})
	}
	cs := fake.NewClientset(objs...)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.31.4-eks-abc"}

	store := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "external-secrets.io/v1", "kind": "SecretStore",
		"metadata": map[string]any{"name": "aws-sm", "namespace": "payments"},
		"spec":     map[string]any{"provider": map[string]any{"aws": map[string]any{"service": "SecretsManager"}}},
	}}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}:   "CustomResourceDefinitionList",
		{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}:                 "SecretStoreList",
		{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}:          "ClusterSecretStoreList",
		{Group: "external-secrets.io", Version: "v1beta1", Resource: "secretstores"}:            "SecretStoreList",
		{Group: "external-secrets.io", Version: "v1beta1", Resource: "clustersecretstores"}:     "ClusterSecretStoreList",
		{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Resource: "secretproviderclasses"}: "SecretProviderClassList",
	}, crd("nodepools.karpenter.sh"), crd("ec2nodeclasses.karpenter.k8s.aws"), crd("dbinstances.rds.services.k8s.aws"),
		crd("secretstores.external-secrets.io"), store)

	kb, err := compat.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := (&snapshot.Collector{Client: cs, Dynamic: dyn, KB: kb, Context: ctxName}).Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSnapshotCapturesCloudBindings(t *testing.T) {
	s := eksCluster(t, "blue", 3, true)
	if s.Provider != "eks" || len(s.Namespaces) != 1 || s.Namespaces[0] != "payments" {
		t.Fatalf("snapshot: provider %s namespaces %v", s.Provider, s.Namespaces)
	}
	if len(s.ServiceAccounts) != 1 || s.ServiceAccounts[0].Annotations["unrelated"] != "" {
		t.Fatalf("only cloud-bound service accounts, cloud keys only: %+v", s.ServiceAccounts)
	}
	if len(s.PVCs) != 1 || s.PVCs[0].StorageClass != "gp3" || s.PVCs[0].Bytes != 100<<30 {
		t.Fatalf("PVC must use the default class and report capacity: %+v", s.PVCs)
	}
	if len(s.SecretBackends) != 1 || s.SecretBackends[0].Provider != "aws" {
		t.Fatalf("secret backends: %+v", s.SecretBackends)
	}
}

func TestAssessEKSToGKE(t *testing.T) {
	s := eksCluster(t, "blue", 3, true)
	a, err := Assess(s, GKE)
	if err != nil {
		t.Fatal(err)
	}
	byBinding := map[string]Item{}
	for _, it := range a.Items {
		byBinding[it.Binding] = it
	}
	want := map[string]string{
		"IRSA (IAM roles for service accounts)":                            "Workload Identity Federation for GKE",
		"StorageClass gp3 (Amazon EKS block volumes)":                      "Persistent Disk CSI",
		"100.0 GiB of persistent volume data":                              "Velero",
		"LoadBalancer Services with cloud-specific annotations":            "networking.gke.io",
		"Ingresses on a cloud load balancer (alb)":                         "Gateway API",
		"Images pulled from 111122223333.dkr.ecr.ap-south-1.amazonaws.com": "Artifact Registry",
		"SecretStore provider \"aws\"":                                     "Google Secret Manager",
		"Node selectors and affinities on cloud-specific labels":           "machine types",
		"Custom resources in rds.services.k8s.aws":                         "Config Connector",
		"Custom resources in karpenter.sh":                                 "node auto-provisioning",
	}
	for binding, mappingHas := range want {
		it, ok := byBinding[binding]
		if !ok || !strings.Contains(it.Mapping, mappingHas) {
			t.Errorf("%s: got %+v", binding, it)
		}
	}
	if byBinding["100.0 GiB of persistent volume data"].Effort != High || a.Items[0].Effort != High {
		t.Error("data and cloud operators must rank as high effort first")
	}
	if a.Summary.DataGiB != 100 || a.Summary.ExternalEndpoints != 2 || a.Size == "" {
		t.Fatalf("summary: %+v size %s", a.Summary, a.Size)
	}
	if _, err := Assess(s, EKS); err == nil {
		t.Fatal("assessing to the same provider must fail")
	}
}

func TestBlueGreenParity(t *testing.T) {
	blue := eksCluster(t, "blue", 3, true)
	green := eksCluster(t, "green", 1, false)
	p := snapshot.Compare(blue, green)
	if p.Ready || p.Summary.NotReady != 1 {
		t.Fatalf("green with 1/3 ready must not be ready: %+v", p.Summary)
	}
	var data, traffic bool
	for _, it := range p.Items {
		data = data || (it.Category == "data" && it.Source == "100.0 GiB")
		traffic = traffic || (it.Category == "traffic" && strings.Contains(it.Name, "2 external"))
	}
	if !data || !traffic {
		t.Fatalf("data and traffic actions missing: %+v", p.Items)
	}
	if p.Items[0].Status != snapshot.ParityNotReady {
		t.Fatal("problems must sort first")
	}

	green = eksCluster(t, "green", 3, true)
	if p := snapshot.Compare(blue, green); !p.Ready {
		t.Fatalf("identical ready clusters must be ready: %+v", p.Items)
	}
}
