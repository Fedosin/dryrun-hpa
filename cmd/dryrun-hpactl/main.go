/*
Copyright The dryrun-hpa Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command dryrun-hpactl exports and analyzes the decisions of DryrunHPAs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/fedosin/dryrun-hpa/internal/analysis"
	"github.com/fedosin/dryrun-hpa/internal/store"
	"github.com/fedosin/dryrun-hpa/internal/version"
)

type filterFlags struct {
	namespace    string
	name         string
	specHash     string
	since        string
	until        string
	onlyRescales bool
	limit        int
}

func (f *filterFlags) add(cmd *cobra.Command) {
	fs := cmd.Flags()
	fs.StringVarP(&f.namespace, "namespace", "n", "", "Only DryrunHPAs in this namespace.")
	fs.StringVar(&f.name, "name", "", "Only the DryrunHPA with this name.")
	fs.StringVar(&f.specHash, "spec-hash", "", "Only decisions made with this configuration (status.observedSpecHash).")
	fs.StringVar(&f.since, "since", "", "Start time: RFC 3339 or a duration before now, e.g. 24h.")
	fs.StringVar(&f.until, "until", "", "End time: RFC 3339 or a duration before now.")
	fs.BoolVar(&f.onlyRescales, "only-rescales", false, "Only decisions that changed the replica count.")
	fs.IntVar(&f.limit, "limit", 0, "Maximum number of decisions; 0 is unlimited.")
}

func (f *filterFlags) filter() (store.Filter, error) {
	now := time.Now()
	since, err := analysis.ParseTime(f.since, now)
	if err != nil {
		return store.Filter{}, err
	}
	until, err := analysis.ParseTime(f.until, now)
	if err != nil {
		return store.Filter{}, err
	}
	return store.Filter{Namespace: f.namespace, Name: f.name, SpecHash: f.specHash, Since: since, Until: until,
		OnlyRescales: f.onlyRescales, Limit: f.limit}, nil
}

func addSourceFlags(cmd *cobra.Command, s *sourceFlags) {
	fs := cmd.PersistentFlags()
	fs.StringVar(&s.db, "db", "", "Read a decision database file directly.")
	fs.StringVar(&s.server, "server", "", "URL of the export API, e.g. http://localhost:8082.")
	fs.StringVar(&s.service, "service", "",
		"Reach the export API through the API server proxy: namespace/name[:port] (default dryrun-hpa-system/dryrun-hpa-controller:export).")
	fs.StringVar(&s.kubeconfig, "kubeconfig", "", "Path to the kubeconfig used with --service.")
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := newRootCommand().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var src sourceFlags
	root := &cobra.Command{
		Use:          "dryrun-hpactl",
		Short:        "Export and analyze the decisions of DryrunHorizontalPodAutoscalers",
		Version:      version.Version,
		SilenceUsage: true,
	}
	addSourceFlags(root, &src)
	root.AddCommand(newExportCommand(&src), newReportCommand(&src), newCompareCommand(&src), newSpecsCommand(&src))
	return root
}

func withSource(cmd *cobra.Command, src *sourceFlags, f func(decisionSource) error) error {
	s, err := src.open(cmd.Context())
	if err != nil {
		return err
	}
	defer s.Close()
	return f(s)
}

func newExportCommand(src *sourceFlags) *cobra.Command {
	var ff filterFlags
	var format, output string
	var metrics int
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write decisions as NDJSON, JSON or CSV",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := ff.filter()
			if err != nil {
				return err
			}
			fm, err := analysis.ParseFormat(format)
			if err != nil {
				return err
			}
			if output == "" || output == "-" {
				return withSource(cmd, src, func(s decisionSource) error {
					_, err := analysis.WriteRecords(cmd.OutOrStdout(), fm, s.Records(cmd.Context(), f), metrics)
					return err
				})
			}
			file, err := os.Create(output)
			if err != nil {
				return err
			}
			err = withSource(cmd, src, func(s decisionSource) error {
				n, err := analysis.WriteRecords(file, fm, s.Records(cmd.Context(), f), metrics)
				fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d decisions to %s\n", n, output)
				return err
			})
			return errors.Join(err, file.Close())
		},
	}
	ff.add(cmd)
	cmd.Flags().StringVarP(&format, "format", "o", "ndjson", "Output format: ndjson, json or csv.")
	cmd.Flags().StringVarP(&output, "file", "f", "-", "Output file.")
	cmd.Flags().IntVar(&metrics, "csv-metrics", 4, "Number of metric column groups in CSV output.")
	return cmd
}

func newReportCommand(src *sourceFlags) *cobra.Command {
	var ff filterFlags
	var format string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Summarize decisions per DryrunHPA and configuration",
		Long: `Summarize decisions per DryrunHPA and configuration (spec hash).

REPLICA-H is the integral of the dry-run replica count over time: the
capacity the configuration would have used. REAL-H is the same for the real
workload. FLAPS counts rescales that reversed the previous one within 5m.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := ff.filter()
			if err != nil {
				return err
			}
			return withSource(cmd, src, func(s decisionSource) error {
				out, err := analysis.Summarize(s.Records(cmd.Context(), f))
				if err != nil {
					return err
				}
				if format == "json" {
					return printJSON(cmd.OutOrStdout(), out)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAMESPACE\tNAME\tSPEC\tFROM\tTO\tDECISIONS\tUP\tDOWN\tFLAPS\tMIN\tMAX\tMEAN\tAT-MAX\tREPLICA-H\tREAL-H\tERRORS")
				for _, r := range out {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.1f\t%.0f%%\t%.1f\t%.1f\t%d\n",
						r.Namespace, r.Name, r.SpecHash, r.First.Format(time.RFC3339), r.Last.Format(time.RFC3339),
						r.Decisions, r.ScaleUps, r.ScaleDowns, r.Flaps, r.MinDesired, r.MaxDesired, r.MeanDesired,
						100*r.AtMaxRatio, r.ReplicaHours, r.SourceReplicaHours, r.Errors)
				}
				return tw.Flush()
			})
		},
	}
	ff.add(cmd)
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table or json.")
	return cmd
}

func newCompareCommand(src *sourceFlags) *cobra.Command {
	var ff filterFlags
	var format string
	cmd := &cobra.Command{
		Use:   "compare",
		Short: "Compare dry-run decisions with the real HPA and real replica counts",
		Long: `Compare dry-run decisions with the real HPA on the same workload (requires
--compare-real-hpa on the controller) and with the real replica count.

DIFF is the mean of dry-run minus real HPA desired replicas: positive means
the dry-run configuration would run more replicas.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := ff.filter()
			if err != nil {
				return err
			}
			return withSource(cmd, src, func(s decisionSource) error {
				out, err := analysis.Compare(s.Records(cmd.Context(), f))
				if err != nil {
					return err
				}
				if format == "json" {
					return printJSON(cmd.OutOrStdout(), out)
				}
				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAMESPACE\tNAME\tSPEC\tTARGET\tSAMPLES\tMATCH\tMEAN-ABS-DIFF\tMAX-ABS-DIFF\tDIFF\tVS-REAL-REPLICAS")
				for _, c := range out {
					match, absDiff, diff, vsReal := "-", "-", "-", "-"
					if c.Samples > 0 {
						match = fmt.Sprintf("%.0f%%", 100*c.MatchRatio)
						absDiff = strconv.FormatFloat(c.MeanAbsDiff, 'f', 2, 64)
						diff = strconv.FormatFloat(c.MeanDiff, 'f', 2, 64)
					}
					if c.SourceSamples > 0 {
						vsReal = strconv.FormatFloat(c.MeanSourceDiff, 'f', 2, 64)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%d\t%s\t%s\n",
						c.Namespace, c.Name, c.SpecHash, c.Target, c.Samples, match, absDiff, c.MaxAbsDiff, diff, vsReal)
				}
				return tw.Flush()
			})
		},
	}
	ff.add(cmd)
	cmd.Flags().StringVarP(&format, "output", "o", "table", "Output format: table or json.")
	return cmd
}

func newSpecsCommand(src *sourceFlags) *cobra.Command {
	var namespace, name string
	cmd := &cobra.Command{
		Use:   "specs",
		Short: "Show the configurations (spec hashes) DryrunHPAs made decisions with",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withSource(cmd, src, func(s decisionSource) error {
				specs, err := s.Specs(cmd.Context(), namespace, name)
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), specs)
			})
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Only DryrunHPAs in this namespace.")
	cmd.Flags().StringVar(&name, "name", "", "Only the DryrunHPA with this name.")
	return cmd
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
