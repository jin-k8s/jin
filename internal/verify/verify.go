// Package verify checks cluster health after an upgrade hop.
package verify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/upgrade"
)

type Options struct {
	// Poll is the interval between attempts. Defaults to 15s.
	Poll time.Duration
	// Namespaces whose Deployments and DaemonSets must be fully available. Defaults to kube-system.
	Namespaces []string
}

// Cluster polls until the control plane reports v, every node is Ready and within the skew
// policy, and system workloads are available, or until ctx ends.
func Cluster(ctx context.Context, client kubernetes.Interface, v kube.Version, log upgrade.Logger, opts Options) error {
	if opts.Poll == 0 {
		opts.Poll = 15 * time.Second
	}
	if len(opts.Namespaces) == 0 {
		opts.Namespaces = []string{"kube-system"}
	}

	var last []string
	for attempt := 0; ; attempt++ {
		problems, err := check(ctx, client, v, opts)
		if err != nil {
			problems = []string{err.Error()}
		}
		if len(problems) == 0 {
			log.Info("Cluster healthy on %s: control plane, nodes and %s workloads verified", v, strings.Join(opts.Namespaces, ", "))
			return nil
		}
		if attempt == 0 || strings.Join(problems, "|") != strings.Join(last, "|") {
			log.Info("Waiting for the cluster to settle: %s", summarize(problems))
		}
		last = problems
		select {
		case <-ctx.Done():
			return fmt.Errorf("verification did not pass: %s: %w", strings.Join(last, "; "), ctx.Err())
		case <-time.After(opts.Poll):
		}
	}
}

func check(ctx context.Context, client kubernetes.Interface, v kube.Version, opts Options) ([]string, error) {
	var problems []string

	sv, err := client.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("read server version: %w", err)
	}
	cur, err := kube.ParseVersion(sv.GitVersion)
	if err != nil {
		return nil, err
	}
	if cur != v {
		problems = append(problems, fmt.Sprintf("API server reports %s, expected %s", cur, v))
	}

	nodes, err := inventory.ListNodes(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var notReady, skewed []string
	for _, n := range nodes {
		if !n.Ready {
			notReady = append(notReady, n.Name)
		}
		if behind := n.Version.MinorsBehind(v); behind > kube.MaxKubeletSkew(v) || behind < 0 {
			skewed = append(skewed, n.Name+" ("+n.Version.String()+")")
		}
	}
	if len(notReady) > 0 {
		problems = append(problems, fmt.Sprintf("%d/%d nodes not Ready: %s", len(notReady), len(nodes), sample(notReady)))
	}
	if len(skewed) > 0 {
		problems = append(problems, "nodes outside the version-skew policy: "+sample(skewed))
	}

	for _, ns := range opts.Namespaces {
		deps, err := client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list deployments in %s: %w", ns, err)
		}
		for _, d := range deps.Items {
			want := int32(1)
			if d.Spec.Replicas != nil {
				want = *d.Spec.Replicas
			}
			if d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < want || d.Status.AvailableReplicas < want {
				problems = append(problems, fmt.Sprintf("deployment %s/%s: %d/%d available", ns, d.Name, d.Status.AvailableReplicas, want))
			}
		}
		dss, err := client.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list daemonsets in %s: %w", ns, err)
		}
		for _, d := range dss.Items {
			s := d.Status
			if s.ObservedGeneration < d.Generation || s.NumberUnavailable > 0 || s.NumberReady < s.DesiredNumberScheduled || s.UpdatedNumberScheduled < s.DesiredNumberScheduled {
				problems = append(problems, fmt.Sprintf("daemonset %s/%s: %d/%d ready", ns, d.Name, s.NumberReady, s.DesiredNumberScheduled))
			}
		}
	}
	return problems, nil
}

func sample(items []string) string {
	sort.Strings(items)
	if len(items) > 5 {
		return strings.Join(items[:5], ", ") + fmt.Sprintf(" and %d more", len(items)-5)
	}
	return strings.Join(items, ", ")
}

func summarize(problems []string) string {
	if len(problems) > 3 {
		return strings.Join(problems[:3], "; ") + fmt.Sprintf("; and %d more", len(problems)-3)
	}
	return strings.Join(problems, "; ")
}
