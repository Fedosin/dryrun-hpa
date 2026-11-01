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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
)

// Sample is the controller's resource use over a steady-state window.
type Sample struct {
	Labels   map[string]string `json:"labels,omitempty"`
	Start    time.Time         `json:"start"`
	Duration float64           `json:"durationSeconds"`

	Objects        float64 `json:"objects"`
	VirtualPods    float64 `json:"virtualPods"`
	CachedPods     float64 `json:"cachedPods"`
	CPUCores       float64 `json:"cpuCores"`
	RSSBytesMean   float64 `json:"rssBytesMean"`
	RSSBytesMax    float64 `json:"rssBytesMax"`
	HeapInuseMean  float64 `json:"heapInuseBytesMean"`
	HeapObjects    float64 `json:"heapObjectsMean"`
	Goroutines     float64 `json:"goroutinesMean"`
	GCPerSecond    float64 `json:"gcPerSecond"`
	ReconcilesPerS float64 `json:"reconcilesPerSecond"`
	ReconcileP50   float64 `json:"reconcileP50Seconds"`
	ReconcileP99   float64 `json:"reconcileP99Seconds"`
	QueueDepthMean float64 `json:"queueDepthMean"`
	QueueP99       float64 `json:"queueLatencyP99Seconds"`
	APIRequestsPS  float64 `json:"apiRequestsPerSecond"`
	DecisionsPS    float64 `json:"decisionsPerSecond"`
	DroppedPS      float64 `json:"droppedDecisionsPerSecond"`
	StoreBytes     float64 `json:"storeBytes"`
	ErrorsPS       float64 `json:"reconcileErrorsPerSecond"`
}

type scrape struct {
	at       time.Time
	families map[string]*dto.MetricFamily
}

func fetch(ctx context.Context, kube kubernetes.Interface, ns, svc, port, path string) (io.ReadCloser, error) {
	return kube.CoreV1().Services(ns).ProxyGet("http", svc, port, path, nil).Stream(ctx)
}

func scrapeMetrics(ctx context.Context, kube kubernetes.Interface, ns, svc string) (scrape, error) {
	body, err := fetch(ctx, kube, ns, svc, "metrics", "/metrics")
	if err != nil {
		return scrape{}, err
	}
	defer body.Close()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(body)
	return scrape{at: time.Now(), families: families}, err
}

// sum adds all series of a counter or gauge matching the label filter.
func (s scrape) sum(name string, match map[string]string) float64 {
	f, ok := s.families[name]
	if !ok {
		return 0
	}
	var total float64
next:
	for _, m := range f.Metric {
		for k, v := range match {
			found := false
			for _, l := range m.Label {
				if l.GetName() == k && l.GetValue() == v {
					found = true
				}
			}
			if !found {
				continue next
			}
		}
		switch {
		case m.Counter != nil:
			total += m.Counter.GetValue()
		case m.Gauge != nil:
			total += m.Gauge.GetValue()
		case m.Untyped != nil:
			total += m.Untyped.GetValue()
		case m.Histogram != nil:
			total += float64(m.Histogram.GetSampleCount())
		case m.Summary != nil:
			total += float64(m.Summary.GetSampleCount())
		}
	}
	return total
}

// buckets returns the cumulative histogram buckets summed over all series.
func (s scrape) buckets(name string, match map[string]string) map[float64]float64 {
	out := map[float64]float64{}
	f, ok := s.families[name]
	if !ok {
		return out
	}
next:
	for _, m := range f.Metric {
		for k, v := range match {
			found := false
			for _, l := range m.Label {
				if l.GetName() == k && l.GetValue() == v {
					found = true
				}
			}
			if !found {
				continue next
			}
		}
		if m.Histogram == nil {
			continue
		}
		for _, b := range m.Histogram.Bucket {
			if !math.IsInf(b.GetUpperBound(), 1) {
				out[b.GetUpperBound()] += float64(b.GetCumulativeCount())
			}
		}
		out[math.Inf(1)] += float64(m.Histogram.GetSampleCount())
	}
	return out
}

// quantile estimates a quantile from the bucket increase between two scrapes,
// like PromQL's histogram_quantile. It is 0 without observations, so samples
// stay JSON-encodable.
func quantile(q float64, first, last map[float64]float64) float64 {
	bounds := make([]float64, 0, len(last))
	for b := range last {
		bounds = append(bounds, b)
	}
	sort.Float64s(bounds)
	total := last[math.Inf(1)] - first[math.Inf(1)]
	if len(bounds) == 0 || total <= 0 {
		return 0
	}
	rank := q * total
	prevBound, prevCount := 0.0, 0.0
	for _, b := range bounds {
		count := last[b] - first[b]
		if count >= rank {
			if math.IsInf(b, 1) {
				return prevBound
			}
			if count == prevCount {
				return b
			}
			return prevBound + (b-prevBound)*(rank-prevCount)/(count-prevCount)
		}
		prevBound, prevCount = b, count
	}
	return bounds[len(bounds)-1]
}

func newSampleCommand(c *clients) *cobra.Command {
	var ns, svc, output, profileDir string
	var duration, interval time.Duration
	var labels map[string]string
	cmd := &cobra.Command{
		Use:   "sample",
		Short: "Measure the controller's resource use over a window",
		RunE: func(cmd *cobra.Command, _ []string) error {
			kube, _, err := c.kube()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			s, err := sample(ctx, kube, ns, svc, duration, interval)
			if err != nil {
				return err
			}
			s.Labels = labels
			if profileDir != "" {
				if err := saveProfiles(ctx, kube, ns, svc, profileDir, labels); err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "saving profiles:", err)
				}
			}
			if output == "" || output == "-" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(s)
			}
			f, err := os.OpenFile(output, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			return errors.Join(json.NewEncoder(f).Encode(s), f.Close())
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&ns, "namespace", "dryrun-hpa-system", "Namespace of the controller.")
	fs.StringVar(&svc, "service", "dryrun-hpa-controller", "Service of the controller.")
	fs.DurationVar(&duration, "duration", 5*time.Minute, "Measurement window.")
	fs.DurationVar(&interval, "interval", 15*time.Second, "Scrape interval.")
	fs.StringVarP(&output, "output", "o", "-", "Append the sample as a JSON line to this file.")
	fs.StringVar(&profileDir, "profile-dir", "", "Save heap and 30s CPU profiles here (requires --enable-pprof on the controller).")
	fs.StringToStringVar(&labels, "label", nil, "Labels to attach to the sample, e.g. count=1000,replicas=3.")
	return cmd
}

func sample(ctx context.Context, kube kubernetes.Interface, ns, svc string, duration, interval time.Duration) (*Sample, error) {
	var scrapes []scrape
	deadline := time.Now().Add(duration)
	for {
		s, err := scrapeMetrics(ctx, kube, ns, svc)
		if err != nil {
			return nil, fmt.Errorf("scraping metrics: %w", err)
		}
		scrapes = append(scrapes, s)
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-time.After(min(interval, time.Until(deadline))):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if len(scrapes) < 2 {
		return nil, fmt.Errorf("need at least two scrapes")
	}
	first, last := scrapes[0], scrapes[len(scrapes)-1]
	secs := last.at.Sub(first.at).Seconds()
	rate := func(name string, match map[string]string) float64 {
		return (last.sum(name, match) - first.sum(name, match)) / secs
	}
	mean := func(name string, match map[string]string) float64 {
		var t float64
		for _, s := range scrapes {
			t += s.sum(name, match)
		}
		return t / float64(len(scrapes))
	}
	var rssMax float64
	for _, s := range scrapes {
		rssMax = math.Max(rssMax, s.sum("process_resident_memory_bytes", nil))
	}
	const reconcile = "horizontal_pod_autoscaler_controller_reconciliation_duration_seconds"
	hpaQueue := map[string]string{"name": "horizontalpodautoscaler"}
	out := &Sample{
		Start:          first.at,
		Duration:       secs,
		Objects:        last.sum("horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers", nil),
		VirtualPods:    mean("dryrun_hpa_virtual_pods", nil),
		CachedPods:     mean("dryrun_hpa_informer_objects", map[string]string{"resource": "pods"}),
		CPUCores:       rate("process_cpu_seconds_total", nil),
		RSSBytesMean:   mean("process_resident_memory_bytes", nil),
		RSSBytesMax:    rssMax,
		HeapInuseMean:  mean("go_memstats_heap_inuse_bytes", nil),
		HeapObjects:    mean("go_memstats_heap_objects", nil),
		Goroutines:     mean("go_goroutines", nil),
		GCPerSecond:    rate("go_gc_duration_seconds", nil),
		ReconcilesPerS: rate("horizontal_pod_autoscaler_controller_reconciliations_total", nil),
		ReconcileP50:   quantile(0.5, first.buckets(reconcile, nil), last.buckets(reconcile, nil)),
		ReconcileP99:   quantile(0.99, first.buckets(reconcile, nil), last.buckets(reconcile, nil)),
		QueueDepthMean: mean("workqueue_depth", hpaQueue),
		QueueP99:       quantile(0.99, first.buckets("workqueue_queue_duration_seconds", hpaQueue), last.buckets("workqueue_queue_duration_seconds", hpaQueue)),
		APIRequestsPS:  rate("rest_client_requests_total", nil),
		DecisionsPS:    rate("dryrun_hpa_decisions_total", nil),
		DroppedPS:      rate("dryrun_hpa_decisions_total", map[string]string{"store": "dropped"}),
		StoreBytes:     last.sum("dryrun_hpa_store_size_bytes", nil),
		ErrorsPS:       rate("dryrun_hpa_decisions_total", map[string]string{"outcome": "error"}),
	}
	return out, nil
}

func saveProfiles(ctx context.Context, kube kubernetes.Interface, ns, svc, dir string, labels map[string]string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{time.Now().UTC().Format("20060102T150405")}
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	prefix := filepath.Join(dir, strings.Join(parts, "_"))
	for name, path := range map[string]string{"heap": "/debug/pprof/heap", "cpu": "/debug/pprof/profile?seconds=30"} {
		p, q, _ := strings.Cut(path, "?")
		params := map[string]string{}
		if k, v, ok := strings.Cut(q, "="); ok {
			params[k] = v
		}
		body, err := kube.CoreV1().Services(ns).ProxyGet("http", svc, "metrics", p, params).Stream(ctx)
		if err != nil {
			return err
		}
		f, err := os.Create(prefix + "." + name + ".pb.gz")
		if err != nil {
			body.Close()
			return err
		}
		_, err = io.Copy(f, body)
		body.Close()
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	return nil
}
