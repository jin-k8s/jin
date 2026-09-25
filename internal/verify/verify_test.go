package verify

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/jin-k8s/jin/internal/kube"
)

type logSink struct{ lines []string }

func (l *logSink) Info(f string, a ...any) { l.lines = append(l.lines, fmt.Sprintf(f, a...)) }
func (l *logSink) Warn(f string, a ...any) { l.lines = append(l.lines, fmt.Sprintf(f, a...)) }
func (l *logSink) Progress(_, _ int, _, f string, a ...any) {
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func node(name, ver string, ready bool) *corev1.Node {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: ver},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: st}},
		},
	}
}

func TestVerifyHealthy(t *testing.T) {
	replicas := int32(2)
	cs := fake.NewClientset(
		node("a", "v1.31.2", true),
		node("b", "v1.29.9", true),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "coredns", Namespace: "kube-system"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{UpdatedReplicas: 2, AvailableReplicas: 2},
		},
	)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.31.1-eks-abc"}
	log := &logSink{}
	if err := Cluster(context.Background(), cs, kube.MustParseVersion("1.31"), log, Options{Poll: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyReportsProblemsUntilTimeout(t *testing.T) {
	cs := fake.NewClientset(
		node("a", "v1.31.2", false),
		node("old", "v1.27.1", true),
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "aws-node", Namespace: "kube-system"},
			Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, NumberReady: 1, UpdatedNumberScheduled: 2, NumberUnavailable: 1},
		},
	)
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.30.5"}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := Cluster(ctx, cs, kube.MustParseVersion("1.31"), &logSink{}, Options{Poll: 5 * time.Millisecond})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, want := range []string{"API server reports 1.30", "1/2 nodes not Ready", "outside the version-skew policy: old (1.27)", "daemonset kube-system/aws-node: 1/2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}
