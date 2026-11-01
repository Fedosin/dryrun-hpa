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

package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// Fit is a least-squares line y = Intercept + Slope*x.
type Fit struct {
	Slope     float64
	Intercept float64
	R2        float64
}

func linearFit(xs, ys []float64) (Fit, bool) {
	n := float64(len(xs))
	if len(xs) < 2 {
		return Fit{}, false
	}
	var sx, sy, sxx, sxy, syy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
		syy += ys[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return Fit{}, false
	}
	slope := (n*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / n
	var ssTot, ssRes float64
	meanY := sy / n
	for i := range xs {
		pred := intercept + slope*xs[i]
		ssRes += (ys[i] - pred) * (ys[i] - pred)
		ssTot += (ys[i] - meanY) * (ys[i] - meanY)
	}
	r2 := 1.0
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	return Fit{Slope: slope, Intercept: intercept, R2: r2}, true
}

func readSamples(r io.Reader) ([]Sample, error) {
	var out []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s Sample
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// groupKey identifies the parameters of a sample except the object count.
func groupKey(s Sample) string {
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		if k != "count" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+s.Labels[k])
	}
	if len(parts) == 0 {
		return "all"
	}
	return strings.Join(parts, " ")
}

func writeReport(w io.Writer, samples []Sample) {
	groups := map[string][]Sample{}
	for _, s := range samples {
		groups[groupKey(s)] = append(groups[groupKey(s)], s)
	}
	names := make([]string, 0, len(groups))
	for k := range groups {
		names = append(names, k)
	}
	slices.Sort(names)

	fmt.Fprintln(w, "# Dry-run HPA controller cost")
	fmt.Fprintln(w)
	for _, name := range names {
		g := groups[name]
		slices.SortFunc(g, func(a, b Sample) int { return cmp.Compare(a.Objects, b.Objects) })
		fmt.Fprintf(w, "## %s\n\n", name)
		fmt.Fprintln(w, "| Objects | Virtual pods | Cached pods | CPU (cores) | RSS mean (MiB) | RSS max (MiB) | Heap (MiB) | Goroutines | Reconciles/s | p50 (ms) | p99 (ms) | Queue p99 (s) | API req/s | Dropped/s |")
		fmt.Fprintln(w, "|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
		var xs, cpu, rss []float64
		for _, s := range g {
			fmt.Fprintf(w, "| %.0f | %.0f | %.0f | %.3f | %.1f | %.1f | %.1f | %.0f | %.1f | %.1f | %.1f | %.2f | %.1f | %.2f |\n",
				s.Objects, s.VirtualPods, s.CachedPods, s.CPUCores, s.RSSBytesMean/(1<<20), s.RSSBytesMax/(1<<20), s.HeapInuseMean/(1<<20),
				s.Goroutines, s.ReconcilesPerS, 1000*s.ReconcileP50, 1000*s.ReconcileP99, s.QueueP99, s.APIRequestsPS, s.DroppedPS)
			xs = append(xs, s.Objects)
			cpu = append(cpu, s.CPUCores)
			rss = append(rss, s.RSSBytesMean)
		}
		fmt.Fprintln(w)
		if f, ok := linearFit(xs, cpu); ok {
			fmt.Fprintf(w, "- CPU: %.3f millicores per object + %.0f millicores base (R² %.3f)\n", 1000*f.Slope, 1000*f.Intercept, f.R2)
		}
		if f, ok := linearFit(xs, rss); ok {
			fmt.Fprintf(w, "- Memory: %.1f KiB per object + %.0f MiB base (R² %.3f)\n", f.Slope/1024, f.Intercept/(1<<20), f.R2)
		}
		fmt.Fprintln(w)
	}
}

func newReportCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "report [SAMPLES.jsonl...]",
		Short: "Summarize samples as Markdown with per-object cost fits",
		RunE: func(cmd *cobra.Command, args []string) error {
			var samples []Sample
			if len(args) == 0 {
				args = []string{"-"}
			}
			for _, a := range args {
				r := cmd.InOrStdin()
				if a != "-" {
					f, err := os.Open(a)
					if err != nil {
						return err
					}
					defer f.Close()
					r = f
				}
				s, err := readSamples(r)
				if err != nil {
					return fmt.Errorf("%s: %w", a, err)
				}
				samples = append(samples, s...)
			}
			writeReport(cmd.OutOrStdout(), samples)
			return nil
		},
	}
}
