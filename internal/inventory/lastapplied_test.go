package inventory

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/jin-k8s/jin/internal/compat"
)

// preferredDiscovery reports v1 as the preferred flowcontrol version while listing v1beta3 first,
// the way aggregated discovery may order them.
type preferredDiscovery struct{ *fakediscovery.FakeDiscovery }

func (d preferredDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	fs := metav1.APIResource{Name: "flowschemas", Kind: "FlowSchema", Verbs: []string{"get", "list"}}
	lists := []*metav1.APIResourceList{
		{GroupVersion: "flowcontrol.apiserver.k8s.io/v1beta3", APIResources: []metav1.APIResource{fs}},
		{GroupVersion: "flowcontrol.apiserver.k8s.io/v1", APIResources: []metav1.APIResource{fs}},
		// Only served by a non-preferred version: still scanned.
		{GroupVersion: "legacy.example.com/v1beta1", APIResources: []metav1.APIResource{{Name: "widgets", Kind: "FlowSchema", Verbs: []string{"list"}}}},
	}
	groups := []*metav1.APIGroup{
		{Name: "flowcontrol.apiserver.k8s.io", PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "flowcontrol.apiserver.k8s.io/v1", Version: "v1"}},
		{Name: "legacy.example.com", PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "legacy.example.com/v1", Version: "v1"}},
	}
	return groups, lists, nil
}

type clientWithDiscovery struct {
	kubernetes.Interface
	d discovery.DiscoveryInterfaces
}

func (c clientWithDiscovery) Discovery() discovery.DiscoveryInterfaces { return c.d }

func TestLastAppliedUsesPreferredVersions(t *testing.T) {
	kb, err := compat.Load()
	if err != nil {
		t.Fatal(err)
	}
	cs := fake.NewClientset()
	client := clientWithDiscovery{cs, preferredDiscovery{cs.Discovery().(*fakediscovery.FakeDiscovery)}}

	scheme := runtime.NewScheme()
	metav1.AddMetaToScheme(scheme)
	md := metadatafake.NewSimpleMetadataClient(scheme)

	c := &Collector{Client: client, Metadata: md, KB: kb}
	if err := c.collectLastApplied(context.Background(), &Inventory{}); err != nil {
		t.Fatal(err)
	}

	var listed []string
	for _, a := range md.Actions() {
		if l, ok := a.(k8stesting.ListAction); ok {
			listed = append(listed, l.GetResource().GroupVersion().String()+"/"+l.GetResource().Resource)
		}
	}
	want := []string{"flowcontrol.apiserver.k8s.io/v1/flowschemas", "legacy.example.com/v1beta1/widgets"}
	if len(listed) != len(want) || listed[0] != want[0] || listed[1] != want[1] {
		// Listing flowcontrol v1beta3 would register a deprecated-API call on the API server.
		t.Fatalf("listed %v, want %v", listed, want)
	}
}
