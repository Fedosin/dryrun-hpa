# Performance harness

Measures the CPU and memory the HPA controller needs as a function of the
number of HPA objects and their parameters (goal 1). Because the dry-run
controller runs the upstream HPA loop unchanged, its cost is a good proxy for
the HPA part of kube-controller-manager; the dry-run layer (virtual workloads,
projection, decision store) is accounted for separately in the metrics below.

The load is synthetic so that thousands of HPAs fit on a laptop:

- **kwok** simulates nodes and makes the load pods Running and Ready without
  containers.
- **fake-metrics-server** (`cmd/fake-metrics-server`) replaces metrics-server
  and serves `metrics.k8s.io` for those pods. Each Deployment's pod template
  carries its total CPU load (a sine wave) in `perf.dryrun-hpa.io/*`
  annotations; the load is split evenly over the Deployment's ready pods, so
  the DryrunHPAs keep making real scaling decisions.
- **loadgen** (`cmd/loadgen`) creates Deployments with DryrunHPAs, samples the
  controller's `/metrics` through the API server service proxy, and writes the
  report.

The real Deployments are never scaled, so their pod count (and the pod
informer the HPA loop keeps) stays at `count × replicas`.

## Running

Use a disposable cluster: setup replaces metrics-server.

```sh
make kind-up
make kind-load IMG=localhost/dryrun-hpa:perf
make perf-setup IMG=localhost/dryrun-hpa:perf   # kwok, fake nodes, fake-metrics-server, config/perf
make perf                                        # default matrix, ~40 minutes
```

`make perf` runs `test/perf/run-matrix.sh`. Every point of the matrix deletes
the previous load, restarts the controller so the heap starts clean, creates
the objects, waits until every DryrunHPA has its VirtualWorkload plus
`WARMUP`, and then samples for `DURATION`. Results go to
`test/perf/results/<timestamp>/`:

- `samples.jsonl`: one JSON line per point (see `Sample` in `cmd/loadgen/sample.go`),
- `report.md`: tables per parameter set, with least-squares fits of CPU and
  memory against the object count,
- `params.txt` and, with `PROFILES=true`, heap and CPU profiles per point.

| Variable     | Default              | Meaning                                                         |
|--------------|----------------------|-----------------------------------------------------------------|
| `COUNTS`     | `0 50 100 250 500`   | Number of Deployments with DryrunHPAs                           |
| `REPLICAS`   | `3`                  | Replicas per Deployment (pods per HPA)                          |
| `METRICS`    | `1`                  | Metrics per HPA: 1 CPU, 2 + memory, 3 + CPU average value        |
| `BEHAVIOR`   | `false`              | `true` sets scale-up and scale-down policies on every DryrunHPA  |
| `WARMUP`     | `2m`                 | Wait after provisioning before sampling                          |
| `DURATION`   | `5m`                 | Sampling window                                                  |
| `INTERVAL`   | `15s`                | Scrape interval inside the window                                |
| `PROFILES`   | `false`              | Save heap and 30s CPU profiles (`--enable-pprof` is on in `config/perf`) |
| `RESULTS`    | timestamped dir      | Output directory                                                 |

Each variable takes a space-separated list, so

```sh
COUNTS="0 500 1000 2000" REPLICAS="1 5" METRICS="1 3" make perf
```

runs 16 points. Scale the kwok nodes with `KWOK_NODES` and
`KWOK_PODS_PER_NODE` in `perf-setup` if you go beyond 20000 pods. At that
scale the bottleneck is usually the kind API server and etcd, so watch the
`API req/s` column and `rest_client_rate_limiter_duration_seconds`.

The report fits

```
cpu_cores ≈ base + slope × objects
rss_bytes ≈ base + slope × objects
```

for each parameter set; the slope is the marginal cost of one HPA. Compare the
slopes across `REPLICAS` (pods per HPA, which drives the pod informer and the
metrics computation) and `METRICS` (metrics API calls per reconcile).

## Reading the cost

`sync-period` (`--horizontal-pod-autoscaler-sync-period`, 15s by default)
fixes the reconcile rate at `objects / 15s`, so CPU is roughly
`reconciles/s × cost of a reconcile`. The columns of the report:

- **CPU (cores)**: `rate(process_cpu_seconds_total)` over the window.
- **RSS / Heap**: `process_resident_memory_bytes` (mean and max) and
  `go_memstats_heap_inuse_bytes`. `GOMEMLIMIT` is set to the container limit.
- **Reconciles/s, p50, p99**: from the upstream
  `horizontal_pod_autoscaler_controller_reconciliation_duration_seconds`
  histogram. A p99 above the sync period means the workers can't keep up;
  raise `--concurrent-horizontal-pod-autoscaler-syncs`.
- **Queue p99**: time HPAs wait in the upstream workqueue before a worker
  picks them up.
- **API req/s**: all client requests, including the dry-run layer's writes
  (`dryrun_hpa_api_writes_total`), which a real HPA controller doesn't make.

To separate the upstream loop from the dry-run layer, use the CPU profiles:
goroutines are labeled with `component` (`hpa-loop`, `projection`, `recorder`,
`store`, `provisioner`, `vw-controller`, `informers`, `http`), so

```sh
go tool pprof -tagfocus component=hpa-loop -top results/.../profiles/*cpu.pb.gz
go tool pprof -tagignore component=hpa-loop -top results/.../profiles/*cpu.pb.gz
```

show the upstream share and the overhead. PromQL for live clusters is in
[queries.md](queries.md).

## Cleaning up

```sh
make perf-teardown    # delete the load, kwok and fake-metrics-server; reinstall metrics-server
```

The e2e tests need the real metrics-server, so run this before `make e2e`
on a cluster that was set up for perf, or delete the cluster with
`make kind-down`.
