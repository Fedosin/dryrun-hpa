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

// Command dashboards generates the Grafana dashboards in config/grafana.
//
//	go run ./hack/dashboards config/grafana
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type target struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat,omitempty"`
	RefID        string `json:"refId"`
	Instant      bool   `json:"instant,omitempty"`
	Format       string `json:"format,omitempty"`
}

type gridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type panel struct {
	ID          int            `json:"id"`
	Type        string         `json:"type"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	GridPos     gridPos        `json:"gridPos"`
	Datasource  map[string]any `json:"datasource,omitempty"`
	Targets     []target       `json:"targets,omitempty"`
	FieldConfig map[string]any `json:"fieldConfig,omitempty"`
	Options     map[string]any `json:"options,omitempty"`
	Collapsed   *bool          `json:"collapsed,omitempty"`
	Panels      []panel        `json:"panels,omitempty"`
}

type builder struct {
	panels []panel
	id     int
	x, y   int
	rowH   int
}

var datasource = map[string]any{"type": "prometheus", "uid": "${datasource}"}

func (b *builder) row(title string) {
	if b.x > 0 {
		b.y += b.rowH
		b.x = 0
	}
	b.id++
	collapsed := false
	b.panels = append(b.panels, panel{ID: b.id, Type: "row", Title: title, GridPos: gridPos{H: 1, W: 24, Y: b.y}, Collapsed: &collapsed})
	b.y++
	b.rowH = 0
}

// add places a panel of width w (of 24) and height h, wrapping as needed.
func (b *builder) add(p panel, w, h int) {
	if b.x+w > 24 {
		b.y += b.rowH
		b.x, b.rowH = 0, 0
	}
	b.id++
	p.ID = b.id
	p.GridPos = gridPos{H: h, W: w, X: b.x, Y: b.y}
	p.Datasource = datasource
	for i := range p.Targets {
		p.Targets[i].RefID = string(rune('A' + i))
	}
	b.panels = append(b.panels, p)
	b.x += w
	b.rowH = max(b.rowH, h)
}

func timeseries(title, unit, desc string, targets ...target) panel {
	return panel{
		Type: "timeseries", Title: title, Description: desc, Targets: targets,
		FieldConfig: map[string]any{"defaults": map[string]any{"unit": unit, "custom": map[string]any{"fillOpacity": 10, "showPoints": "never"}}},
		Options:     map[string]any{"legend": map[string]any{"displayMode": "list", "placement": "bottom"}, "tooltip": map[string]any{"mode": "multi"}},
	}
}

func stat(title, unit, desc string, t target) panel {
	return panel{
		Type: "stat", Title: title, Description: desc, Targets: []target{t},
		FieldConfig: map[string]any{"defaults": map[string]any{"unit": unit}},
		Options:     map[string]any{"reduceOptions": map[string]any{"calcs": []string{"lastNotNull"}}, "colorMode": "none"},
	}
}

func q(expr, legend string) target { return target{Expr: expr, LegendFormat: legend} }

func quantile(qv, metric, by, legend string) target {
	return q(fmt.Sprintf(`histogram_quantile(%s, sum by (le%s) (rate(%s_bucket{job=~"$job"}[$__rate_interval])))`, qv, by, metric), legend)
}

func dashboard(uid, title string, panels []panel, vars []map[string]any) map[string]any {
	return map[string]any{
		"uid": uid, "title": title, "editable": true, "schemaVersion": 39, "version": 1,
		"tags":       []string{"dryrun-hpa"},
		"time":       map[string]any{"from": "now-3h", "to": "now"},
		"refresh":    "30s",
		"panels":     panels,
		"templating": map[string]any{"list": vars},
	}
}

func dsVar() map[string]any {
	return map[string]any{"name": "datasource", "type": "datasource", "query": "prometheus", "label": "Data source"}
}

func queryVar(name, label, query string, multi bool) map[string]any {
	return map[string]any{
		"name": name, "label": label, "type": "query", "datasource": datasource,
		"query": map[string]any{"query": query, "refId": "A"}, "refresh": 2,
		"includeAll": multi, "multi": multi, "allValue": ".*", "sort": 1,
	}
}

func costDashboard() map[string]any {
	b := &builder{}
	const job = `{job=~"$job"}`
	b.row("Controller resources (Goal 1)")
	b.add(stat("DryrunHPAs", "short", "Objects in the HPA loop.", q(`sum(horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers`+job+`)`, "")), 4, 4)
	b.add(stat("Virtual pods", "short", "Simulated pods across all VirtualWorkloads.", q(`sum(dryrun_hpa_virtual_pods`+job+`)`, "")), 4, 4)
	b.add(stat("CPU per 1000 DryrunHPAs", "short", "Cores used by the controller per 1000 objects.",
		q(`1000 * sum(rate(process_cpu_seconds_total`+job+`[$__rate_interval])) / sum(horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers`+job+`)`, "")), 4, 4)
	b.add(stat("Memory per DryrunHPA", "bytes", "Resident memory divided by the number of objects.",
		q(`sum(process_resident_memory_bytes`+job+`) / sum(horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers`+job+`)`, "")), 4, 4)
	b.add(stat("Reconciles per second", "reqps", "", q(`sum(rate(horizontal_pod_autoscaler_controller_reconciliations_total`+job+`[$__rate_interval]))`, "")), 4, 4)
	b.add(stat("Decision DB size", "bytes", "", q(`sum(dryrun_hpa_store_size_bytes`+job+`)`, "")), 4, 4)
	b.add(timeseries("CPU", "short", "Cores used by the whole controller process.",
		q(`rate(process_cpu_seconds_total`+job+`[$__rate_interval])`, "{{pod}}")), 8, 8)
	b.add(timeseries("Memory", "bytes", "",
		q(`process_resident_memory_bytes`+job, "RSS {{pod}}"),
		q(`go_memstats_heap_inuse_bytes`+job, "heap in use {{pod}}"),
		q(`go_memstats_heap_objects`+job, "heap objects {{pod}}")), 8, 8)
	b.add(timeseries("Goroutines and GC", "short", "",
		q(`go_goroutines`+job, "goroutines {{pod}}"),
		q(`rate(go_gc_duration_seconds_count`+job+`[$__rate_interval])`, "GC/s {{pod}}")), 8, 8)
	b.add(timeseries("Cached objects", "short", "Objects in the informer caches; pods dominate memory.",
		q(`dryrun_hpa_informer_objects`+job, "{{resource}}"),
		q(`dryrun_hpa_virtual_workloads`+job, "virtual workloads"),
		q(`dryrun_hpa_virtual_pods`+job, "virtual pods")), 12, 8)
	b.add(timeseries("Cost per object", "short", "CPU millicores and MiB of memory per DryrunHPA.",
		q(`1000 * sum(rate(process_cpu_seconds_total`+job+`[$__rate_interval])) / sum(horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers`+job+`)`, "millicores"),
		q(`sum(process_resident_memory_bytes`+job+`) / 1048576 / sum(horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers`+job+`)`, "MiB")), 12, 8)

	b.row("HPA loop (upstream metrics)")
	b.add(timeseries("Reconciliations", "reqps", "",
		q(`sum by (action, error) (rate(horizontal_pod_autoscaler_controller_reconciliations_total`+job+`[$__rate_interval]))`, "{{action}} {{error}}")), 8, 8)
	b.add(timeseries("Reconciliation duration", "s", "",
		quantile("0.5", "horizontal_pod_autoscaler_controller_reconciliation_duration_seconds", "", "p50"),
		quantile("0.99", "horizontal_pod_autoscaler_controller_reconciliation_duration_seconds", "", "p99")), 8, 8)
	b.add(timeseries("Metric computation p99", "s", "",
		quantile("0.99", "horizontal_pod_autoscaler_controller_metric_computation_duration_seconds", ", metric_type", "{{metric_type}}")), 8, 8)
	b.add(timeseries("Work queue", "short", "",
		q(`workqueue_depth{job=~"$job", name="horizontalpodautoscaler"}`, "depth"),
		q(`rate(workqueue_adds_total{job=~"$job", name="horizontalpodautoscaler"}[$__rate_interval])`, "adds/s")), 8, 8)
	b.add(timeseries("Work queue latency p99", "s", "Time in queue and time to process, per queue.",
		q(`histogram_quantile(0.99, sum by (le, name) (rate(workqueue_queue_duration_seconds_bucket`+job+`[$__rate_interval])))`, "queued {{name}}"),
		q(`histogram_quantile(0.99, sum by (le, name) (rate(workqueue_work_duration_seconds_bucket`+job+`[$__rate_interval])))`, "work {{name}}")), 8, 8)
	b.add(timeseries("Decisions", "reqps", "",
		q(`sum by (outcome) (rate(dryrun_hpa_decisions_total`+job+`[$__rate_interval]))`, "{{outcome}}")), 8, 8)

	b.row("Metrics and API server")
	b.add(timeseries("Metrics API latency p99", "s", "",
		quantile("0.99", "dryrun_hpa_metrics_api_request_duration_seconds", ", api", "{{api}}")), 8, 8)
	b.add(timeseries("Projection p99", "s", "Time to sum real usage and divide it across virtual pods.",
		quantile("0.99", "dryrun_hpa_projection_duration_seconds", ", metric_type", "{{metric_type}}")), 8, 8)
	b.add(timeseries("API requests", "reqps", "Client-go requests by verb and code.",
		q(`sum by (verb, code) (rate(rest_client_requests_total`+job+`[$__rate_interval]))`, "{{verb}} {{code}}")), 8, 8)
	b.add(timeseries("Writes by resource", "reqps", "",
		q(`sum by (resource, verb) (rate(dryrun_hpa_api_writes_total`+job+`[$__rate_interval]))`, "{{resource}} {{verb}}")), 8, 8)
	b.add(timeseries("Controller syncs", "reqps", "Provisioner and VirtualWorkload controller.",
		q(`sum by (controller, result) (rate(dryrun_hpa_controller_syncs_total`+job+`[$__rate_interval]))`, "{{controller}} {{result}}")), 8, 8)
	b.add(timeseries("Extrapolated source pods", "short", "Ready real pods without metrics that were extrapolated.",
		q(`rate(dryrun_hpa_source_pods_extrapolated_total`+job+`[$__rate_interval])`, "pods/s")), 8, 8)

	b.row("Decision store")
	b.add(timeseries("Writes", "short", "",
		q(`rate(dryrun_hpa_store_written_total`+job+`[$__rate_interval])`, "records/s"),
		q(`sum by (operation) (rate(dryrun_hpa_store_errors_total`+job+`[$__rate_interval]))`, "errors {{operation}}")), 8, 8)
	b.add(timeseries("Queue", "short", "Dropped records show as errors with operation=dropped.",
		q(`dryrun_hpa_store_queue_length`+job, "queued")), 8, 8)
	b.add(timeseries("Write latency", "s", "",
		quantile("0.99", "dryrun_hpa_store_write_duration_seconds", "", "p99 per batch")), 8, 8)

	return dashboard("dryrun-hpa-cost", "Dry-run HPA: controller cost", b.panels, []map[string]any{
		dsVar(), queryVar("job", "Job", `label_values(dryrun_hpa_build_info, job)`, true),
	})
}

func objectsDashboard() map[string]any {
	b := &builder{}
	const sel = `{job=~"$job", namespace=~"$namespace", name=~"$name"}`
	b.row("Replicas (Goal 2)")
	b.add(timeseries("Dry-run vs real replicas", "short",
		"desired is the dry-run decision; source is the real workload; real HPA requires --compare-real-hpa.",
		q(`dryrun_hpa_recommendation_replicas{job=~"$job", namespace=~"$namespace", name=~"$name", stage="desired"}`, "dry-run desired {{name}}"),
		q(`dryrun_hpa_virtual_ready_replicas`+sel, "virtual ready {{name}}"),
		q(`dryrun_hpa_source_replicas`+sel, "real {{name}}"),
		q(`dryrun_hpa_real_hpa_desired_replicas`+sel, "real HPA desired {{name}}")), 24, 10)
	b.add(timeseries("Recommendation stages", "short",
		"metric: highest metric proposal; stabilized: after the stabilization window; desired: after behavior policies and min/max.",
		q(`dryrun_hpa_recommendation_replicas`+sel, "{{name}} {{stage}}")), 12, 9)
	b.add(timeseries("Metric current vs target", "short", "Utilization in percent, values in base units.",
		q(`dryrun_hpa_metric_value`+sel, "{{name}} #{{index}} {{metric}} {{kind}}")), 12, 9)

	b.row("Source workload")
	b.add(timeseries("Summed usage", "short", "Usage of all ready real pods, per container (cores or bytes).",
		q(`dryrun_hpa_source_usage`+sel, "{{name}} {{container}} {{resource}}")), 12, 9)
	b.add(panel{
		Type: "table", Title: "Conditions",
		Targets: []target{{Expr: `dryrun_hpa_condition` + sel + ` == 1`, Instant: true, Format: "table"}},
		Options: map[string]any{"showHeader": true},
	}, 12, 9)

	return dashboard("dryrun-hpa-objects", "Dry-run HPA: objects", b.panels, []map[string]any{
		dsVar(),
		queryVar("job", "Job", `label_values(dryrun_hpa_build_info, job)`, true),
		queryVar("namespace", "Namespace", `label_values(dryrun_hpa_recommendation_replicas{job=~"$job"}, namespace)`, true),
		queryVar("name", "DryrunHPA", `label_values(dryrun_hpa_recommendation_replicas{job=~"$job", namespace=~"$namespace"}, name)`, true),
	})
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: dashboards OUTPUT_DIR")
		os.Exit(2)
	}
	for name, d := range map[string]map[string]any{
		"dryrun-hpa-cost.json":    costDashboard(),
		"dryrun-hpa-objects.json": objectsDashboard(),
	} {
		data, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(os.Args[1], name), append(data, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
