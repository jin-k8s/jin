package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/inventory"
	"github.com/jin-k8s/jin/internal/kube"
	"github.com/jin-k8s/jin/internal/report"
)

type planOptions struct {
	target         string
	output         string
	timeout        time.Duration
	noRecord       bool
	failOnBlockers bool
	skipSupport    bool
	collect        inventory.Options
}

func newPlanCmd(g *globalOptions) *cobra.Command {
	o := &planOptions{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Build a hop-by-hop upgrade plan for a cluster (read-only)",
		Long: "Inspects the cluster with read-only API calls and reports what blocks an upgrade to the target\n" +
			"version, hop by hop, together with the ordered steps for each hop. Nothing in the cluster is changed.",
		Example: "  jin plan --target 1.33\n  jin plan --context prod-eu --target 1.34 -o markdown > plan.md\n  jin plan --fail-on-blockers   # exit code 2 when blockers exist (CI gate)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPlan(cmd, g, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.target, "target", "", "target Kubernetes minor version, e.g. 1.33 (default: next minor)")
	f.StringVarP(&o.output, "output", "o", report.FormatTable, "output format: "+strings.Join(report.Formats(), "|"))
	f.DurationVar(&o.timeout, "timeout", 5*time.Minute, "overall timeout for cluster inspection")
	f.BoolVar(&o.noRecord, "no-record", false, "do not save a run record")
	f.BoolVar(&o.failOnBlockers, "fail-on-blockers", false, fmt.Sprintf("exit with code %d when the plan contains blockers", ExitBlockers))
	f.BoolVar(&o.collect.SkipHelm, "skip-helm", false, "do not read Helm release secrets (avoids needing list on Secrets)")
	f.BoolVar(&o.collect.SkipLastApplied, "skip-last-applied", false, "do not scan last-applied-configuration annotations")
	f.BoolVar(&o.collect.SkipMetrics, "skip-metrics", false, "do not read deprecated-API metrics from the API server")
	f.BoolVar(&o.skipSupport, "skip-support", false, "do not read the provider support calendar (EKS: needs AWS credentials)")
	return cmd
}

func runPlan(cmd *cobra.Command, g *globalOptions, o *planOptions) error {
	var target kube.Version
	if o.target != "" {
		v, err := kube.ParseVersion(o.target)
		if err != nil {
			return err
		}
		target = v
	}
	if err := validFormat(o.output); err != nil {
		return err
	}

	env, err := g.env()
	if err != nil {
		return err
	}
	clients, err := env.Connect(g.context)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), o.timeout)
	defer cancel()
	sp := newSpinner(cmd.ErrOrStderr())
	sp.Start(fmt.Sprintf("Inspecting %s (read-only)", clients.Context))
	rec, runErr := env.Plan(ctx, clients, app.PlanOptions{
		Target:      target,
		Collect:     o.collect,
		SkipSupport: o.skipSupport,
		Record:      !o.noRecord,
		Progress:    func(step string) { sp.Update(fmt.Sprintf("Inspecting %s: %s", clients.Context, step)) },
	})
	if runErr != nil {
		sp.Stop(false, fmt.Sprintf("Inspection of %s failed", clients.Context))
		return runErr
	}
	sp.Stop(true, fmt.Sprintf("Inspected %s in %s", clients.Context, rec.FinishedAt.Sub(rec.StartedAt).Round(100*time.Millisecond)))

	if err := report.Render(cmd.OutOrStdout(), rec, o.output); err != nil {
		return err
	}
	if o.failOnBlockers && !rec.Plan.Ready {
		return &exitError{code: ExitBlockers}
	}
	return nil
}

func validFormat(f string) error {
	for _, v := range report.Formats() {
		if f == v {
			return nil
		}
	}
	return fmt.Errorf("unknown output format %q (use %s)", f, strings.Join(report.Formats(), ", "))
}
