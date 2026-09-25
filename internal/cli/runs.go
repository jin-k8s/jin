package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jin-k8s/jin/internal/report"
	"github.com/jin-k8s/jin/internal/runrecord"
)

func newRunsCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "Inspect recorded runs",
	}
	cmd.PersistentFlags().StringVar(&dir, "runs-dir", "", "run record directory (default $JIN_HOME/runs or ~/.jin/runs)")

	store := func() (*runrecord.Store, error) {
		if dir != "" {
			return runrecord.NewStore(dir), nil
		}
		d, err := runrecord.DefaultDir()
		if err != nil {
			return nil, err
		}
		return runrecord.NewStore(d), nil
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List recorded runs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := store()
			if err != nil {
				return err
			}
			recs, err := s.List()
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTARTED\tKIND\tCONTEXT\tUPGRADE\tSTATUS\tBLOCKERS")
			for _, r := range recs {
				upgrade, blockers := "-", "-"
				if r.Plan != nil {
					upgrade = fmt.Sprintf("%s->%s", r.Plan.Current, r.Plan.Target)
					blockers = fmt.Sprint(r.Plan.Summary.Blockers)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					r.ID, r.StartedAt.Local().Format("2006-01-02 15:04"), r.Kind, r.Cluster.Context, upgrade, r.Status, blockers)
			}
			return tw.Flush()
		},
	}

	var output string
	show := &cobra.Command{
		Use:   "show RUN_ID",
		Short: "Show a recorded run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validFormat(output); err != nil {
				return err
			}
			s, err := store()
			if err != nil {
				return err
			}
			r, err := s.Get(args[0])
			if err != nil {
				return err
			}
			return report.Render(cmd.OutOrStdout(), r, output)
		},
	}
	show.Flags().StringVarP(&output, "output", "o", report.FormatTable, "output format: table|markdown|json")

	cmd.AddCommand(list, show)
	return cmd
}
