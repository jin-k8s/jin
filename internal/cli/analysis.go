package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jin-k8s/jin/internal/migrate"
	"github.com/jin-k8s/jin/internal/snapshot"
)

func newCompareCmd(g *globalOptions) *cobra.Command {
	var green, output string
	cmd := &cobra.Command{
		Use:   "compare --context BLUE --to GREEN",
		Short: "Check a replacement cluster against the live one before a blue/green cutover (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if green == "" {
				return errors.New("--to is required")
			}
			env, err := g.env()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			snap := func(name string) (*snapshot.Snapshot, error) {
				c, err := env.Connect(name)
				if err != nil {
					return nil, err
				}
				return env.Snapshot(ctx, c)
			}
			sp := newSpinner(cmd.ErrOrStderr())
			sp.Start("Inspecting both clusters (read-only)")
			blue, err := snap(g.context)
			if err != nil {
				sp.Stop(false, "Inspection failed")
				return err
			}
			dst, err := snap(green)
			if err != nil {
				sp.Stop(false, "Inspection failed")
				return err
			}
			sp.Stop(true, "Inspected "+blue.Context+" and "+dst.Context)
			p := snapshot.Compare(blue, dst)
			if output == "json" {
				return writeJSONOut(cmd.OutOrStdout(), p)
			}
			renderParity(cmd.OutOrStdout(), p)
			return nil
		},
	}
	cmd.Flags().StringVar(&green, "to", "", "kubeconfig context of the replacement (green) cluster")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json")
	return cmd
}

func renderParity(w io.Writer, p *snapshot.Parity) {
	verdict := "READY FOR CUTOVER"
	if !p.Ready {
		verdict = "NOT READY"
	}
	fmt.Fprintf(w, "\n  Blue/green: %s (%s) → %s (%s)\n  Result:     %s  missing %d · not ready %d · mismatched %d · actions %d · ok %d\n\n",
		p.Source, p.SourceVersion, p.Target, p.TargetVersion, verdict, p.Summary.Missing, p.Summary.NotReady, p.Summary.Mismatch, p.Summary.Actions, p.Summary.OK)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  STATUS\tCATEGORY\tNAME\tBLUE\tGREEN")
	for _, it := range p.Items {
		if it.Status == snapshot.ParityOK {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", strings.ToUpper(string(it.Status)), it.Category, it.Name, dash(it.Source), dash(it.Target))
	}
	_ = tw.Flush()
	fmt.Fprintln(w, "\n  Cutover checklist")
	for i, c := range p.Checklist {
		fmt.Fprintf(w, "  %2d. %s\n", i+1, c)
	}
	fmt.Fprintln(w)
}

func newAssessCmd(g *globalOptions) *cobra.Command {
	var target, output string
	cmd := &cobra.Command{
		Use:   "assess --to gke|aks|eks",
		Short: "Assess what moving a cluster to another cloud involves (read-only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := g.env()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			c, err := env.Connect(g.context)
			if err != nil {
				return err
			}
			sp := newSpinner(cmd.ErrOrStderr())
			sp.Start("Inspecting " + c.Context + " (read-only)")
			s, err := env.Snapshot(ctx, c)
			if err != nil {
				sp.Stop(false, "Inspection failed")
				return err
			}
			sp.Stop(true, "Inspected "+c.Context)
			a, err := migrate.Assess(s, target)
			if err != nil {
				return err
			}
			if output == "json" {
				return writeJSONOut(cmd.OutOrStdout(), a)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "\n  Migration assessment: %s (%s) → %s\n  Size: %s · %d bindings · %.1f GiB of volume data · %d external endpoints · %d workloads\n\n",
				a.Context, a.Source, a.Target, a.Size, a.Summary.Items, a.Summary.DataGiB, a.Summary.ExternalEndpoints, a.Summary.Workloads)
			for _, it := range a.Items {
				fmt.Fprintf(w, "  [%s] %s: %s (%d)\n        → %s\n", strings.ToUpper(string(it.Effort)), it.Category, it.Binding, it.Count, it.Mapping)
			}
			fmt.Fprintln(w)
			for _, n := range a.Notes {
				fmt.Fprintf(w, "  • %s\n", n)
			}
			fmt.Fprintln(w)
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "to", "", "target provider: eks, gke or aks")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func writeJSONOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
