# PromQL for the controller's cost

These assume the controller is scraped as `job="dryrun-hpa-controller"` (the
ServiceMonitor in `config/prometheus` does that). The same panels are in the
`dryrun-hpa-cost` Grafana dashboard.

## Resources

```promql
# CPU cores
rate(process_cpu_seconds_total{job="dryrun-hpa-controller"}[5m])

# Memory
process_resident_memory_bytes{job="dryrun-hpa-controller"}
go_memstats_heap_inuse_bytes{job="dryrun-hpa-controller"}

# Marginal cost per HPA object (meaningful once the count is large)
rate(process_cpu_seconds_total{job="dryrun-hpa-controller"}[5m])
  / on(instance) horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers
process_resident_memory_bytes{job="dryrun-hpa-controller"}
  / on(instance) horizontal_pod_autoscaler_controller_num_horizontal_pod_autoscalers
```

## Upstream HPA loop

```promql
# Reconciles per second, by action and error
sum by (action, error) (rate(horizontal_pod_autoscaler_controller_reconciliations_total[5m]))

# Reconcile latency
histogram_quantile(0.99, sum by (le) (rate(horizontal_pod_autoscaler_controller_reconciliation_duration_seconds_bucket[5m])))

# Metric computation latency, by metric type
histogram_quantile(0.99, sum by (le, metric_type) (rate(horizontal_pod_autoscaler_controller_metric_computation_duration_seconds_bucket[5m])))

# Are the workers keeping up with the sync period?
workqueue_depth{name="horizontalpodautoscaler"}
histogram_quantile(0.99, sum by (le) (rate(workqueue_queue_duration_seconds_bucket{name="horizontalpodautoscaler"}[5m])))
```

## API and metrics calls

```promql
# Client requests, by verb and code
sum by (verb, code) (rate(rest_client_requests_total{job="dryrun-hpa-controller"}[5m]))

# Client-side throttling (raise --kube-api-qps/--kube-api-burst if this grows)
histogram_quantile(0.99, sum by (le) (rate(rest_client_rate_limiter_duration_seconds_bucket{job="dryrun-hpa-controller"}[5m])))

# Metrics API latency, by API (resource, custom, external)
histogram_quantile(0.99, sum by (le, api) (rate(dryrun_hpa_metrics_api_request_duration_seconds_bucket[5m])))

# Writes made only by the dry-run layer (not part of a real HPA's cost)
sum by (resource, verb) (rate(dryrun_hpa_api_writes_total[5m]))
```

## Dry-run layer overhead

```promql
# Projection of real metrics onto virtual pods
histogram_quantile(0.99, sum by (le) (rate(dryrun_hpa_projection_duration_seconds_bucket[5m])))

# Provisioner and VirtualWorkload controller
sum by (controller, result) (rate(dryrun_hpa_controller_syncs_total[5m]))
histogram_quantile(0.99, sum by (le, controller) (rate(dryrun_hpa_controller_sync_duration_seconds_bucket[5m])))

# Decision store
sum by (outcome, store) (rate(dryrun_hpa_decisions_total[5m]))
rate(dryrun_hpa_decisions_total{store="dropped"}[5m]) > 0
dryrun_hpa_store_queue_length
histogram_quantile(0.99, sum by (le) (rate(dryrun_hpa_store_write_duration_seconds_bucket[5m])))
dryrun_hpa_store_size_bytes

# Simulation size
dryrun_hpa_virtual_workloads
dryrun_hpa_virtual_pods
dryrun_hpa_informer_objects
```
