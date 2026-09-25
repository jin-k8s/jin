// Package cli wires Jin's commands.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/jin-k8s/jin/internal/app"
	"github.com/jin-k8s/jin/internal/buildinfo"
	"github.com/jin-k8s/jin/internal/compat"
	"github.com/jin-k8s/jin/internal/runrecord"
)

// ExitBlockers is returned by `jin plan --fail-on-blockers` when the plan has blockers.
const ExitBlockers = 2

type exitError struct{ code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

type globalOptions struct {
	kubeconfig string
	context    string
}

func (g *globalOptions) env() (*app.Env, error) {
	kb, err := compat.Load()
	if err != nil {
		return nil, err
	}
	dir, err := runrecord.DefaultDir()
	if err != nil {
		return nil, err
	}
	return &app.Env{Kubeconfig: g.kubeconfig, KB: kb, Runs: runrecord.NewStore(dir)}, nil
}

func Execute() int {
	return run(os.Args[1:], os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			return ee.code
		}
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	g := &globalOptions{}
	root := &cobra.Command{
		Use:   "jin",
		Short: "Plan, approve and automate Kubernetes upgrades",
		Long: "Jin plans Kubernetes upgrades hop by hop, surfaces what will break before it breaks,\n" +
			"and runs approved upgrades with a human in the loop for every hop.\n\n" +
			"  jin plan      inspect a cluster and build an upgrade plan (read-only)\n" +
			"  jin server    web UI: approvals, automated upgrades, fleet view, SSO and audit\n" +
			"  jin compare   blue/green parity check before a cutover (read-only)\n" +
			"  jin assess    cross-cloud migration assessment (read-only)\n" +
			"  jin runs      list and show recorded plan runs",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&g.kubeconfig, "kubeconfig", "", "path to the kubeconfig file (defaults to $KUBECONFIG or ~/.kube/config)")
	root.PersistentFlags().StringVar(&g.context, "context", "", "kubeconfig context to use (defaults to the current context)")

	root.AddCommand(newPlanCmd(g), newServerCmd(g), newCompareCmd(g), newAssessCmd(g), newRunsCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the Jin version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "jin %s (commit %s)\n", buildinfo.Version, buildinfo.Commit)
			return err
		},
	}
}
