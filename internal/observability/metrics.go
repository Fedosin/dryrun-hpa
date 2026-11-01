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

// Package observability defines the metrics, profiling labels and tracing
// of the dry-run controller. Everything is registered in the component-base
// legacy registry, next to the upstream HPA controller metrics and the
// client-go, workqueue, Go runtime and process metrics.
package observability

import (
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"

	"github.com/fedosin/dryrun-hpa/internal/version"
)

const namespace = "dryrun_hpa"

var (
	// MetricsAPIRequestDuration is the latency of resource, custom and external metrics API requests.
	MetricsAPIRequestDuration = metrics.NewHistogramVec(&metrics.HistogramOpts{
		Namespace:      namespace,
		Name:           "metrics_api_request_duration_seconds",
		Help:           "Latency of metrics API requests made for dry-run HPAs, by API (resource, custom, external) and result (success, error).",
		Buckets:        metrics.ExponentialBuckets(0.001, 2, 15),
		StabilityLevel: metrics.ALPHA,
	}, []string{"api", "result"})

	// ProjectionDuration is the time spent projecting real metrics onto virtual pods, excluding API calls.
	ProjectionDuration = metrics.NewHistogramVec(&metrics.HistogramOpts{
		Namespace:      namespace,
		Name:           "projection_duration_seconds",
		Help:           "Time spent aggregating real pod metrics and distributing them over virtual pods, excluding metrics API latency, by metric type.",
		Buckets:        metrics.ExponentialBuckets(0.00001, 2, 18),
		StabilityLevel: metrics.ALPHA,
	}, []string{"metric_type"})

	// SourcePodsExtrapolated counts real ready pods whose missing samples were extrapolated.
	SourcePodsExtrapolated = metrics.NewCounter(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "source_pods_extrapolated_total",
		Help:           "Number of ready real pods without a metric sample whose usage was extrapolated from the other pods.",
		StabilityLevel: metrics.ALPHA,
	})

	// ControllerSyncs counts syncs of the provisioner and the VirtualWorkload controller.
	ControllerSyncs = metrics.NewCounterVec(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "controller_syncs_total",
		Help:           "Number of syncs by controller (provisioner, virtualworkload) and result (success, conflict, error).",
		StabilityLevel: metrics.ALPHA,
	}, []string{"controller", "result"})

	// ControllerSyncDuration is the duration of syncs of the provisioner and the VirtualWorkload controller.
	ControllerSyncDuration = metrics.NewHistogramVec(&metrics.HistogramOpts{
		Namespace:      namespace,
		Name:           "controller_sync_duration_seconds",
		Help:           "Duration of syncs by controller (provisioner, virtualworkload).",
		Buckets:        metrics.ExponentialBuckets(0.0001, 2, 16),
		StabilityLevel: metrics.ALPHA,
	}, []string{"controller"})

	// APIWrites counts writes the dry-run layer makes to the API server.
	APIWrites = metrics.NewCounterVec(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "api_writes_total",
		Help:           "Writes made by the dry-run layer (not the upstream HPA loop), by resource and verb.",
		StabilityLevel: metrics.ALPHA,
	}, []string{"resource", "verb"})

	// Decisions counts decisions handed to the decision store.
	Decisions = metrics.NewCounterVec(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "decisions_total",
		Help:           "Decisions made by the HPA loop, by outcome (scale_up, scale_down, none, error) and whether they were queued for the store (queued, dropped, disabled).",
		StabilityLevel: metrics.ALPHA,
	}, []string{"outcome", "store"})

	// StoreWriteDuration is the duration of one batched store write.
	StoreWriteDuration = metrics.NewHistogram(&metrics.HistogramOpts{
		Namespace:      namespace,
		Name:           "store_write_duration_seconds",
		Help:           "Duration of a batched decision store write.",
		Buckets:        metrics.ExponentialBuckets(0.0001, 2, 18),
		StabilityLevel: metrics.ALPHA,
	})

	// StoreBatchSize is the number of decisions per batched write.
	StoreBatchSize = metrics.NewHistogram(&metrics.HistogramOpts{
		Namespace:      namespace,
		Name:           "store_batch_size",
		Help:           "Number of decisions per batched decision store write.",
		Buckets:        metrics.ExponentialBuckets(1, 2, 14),
		StabilityLevel: metrics.ALPHA,
	})

	// StoreQueueLength is the number of decisions waiting to be written.
	StoreQueueLength = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      namespace,
		Name:           "store_queue_length",
		Help:           "Number of decisions waiting to be written to the decision store.",
		StabilityLevel: metrics.ALPHA,
	})

	// StoreWritten counts decisions written to the store.
	StoreWritten = metrics.NewCounter(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "store_written_total",
		Help:           "Number of decisions written to the decision store.",
		StabilityLevel: metrics.ALPHA,
	})

	// StoreErrors counts failed store operations.
	StoreErrors = metrics.NewCounterVec(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "store_errors_total",
		Help:           "Number of failed decision store operations, by operation.",
		StabilityLevel: metrics.ALPHA,
	}, []string{"operation"})

	// StoreSizeBytes is the size of the decision database.
	StoreSizeBytes = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      namespace,
		Name:           "store_size_bytes",
		Help:           "Size of the decision database on disk.",
		StabilityLevel: metrics.ALPHA,
	})

	// StoreRetentionDeleted counts decisions deleted by the retention job.
	StoreRetentionDeleted = metrics.NewCounter(&metrics.CounterOpts{
		Namespace:      namespace,
		Name:           "store_retention_deleted_total",
		Help:           "Number of decisions deleted by the retention job.",
		StabilityLevel: metrics.ALPHA,
	})

	// VirtualWorkloads is the number of virtual workloads in memory.
	VirtualWorkloads = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      namespace,
		Name:           "virtual_workloads",
		Help:           "Number of VirtualWorkloads simulated in memory.",
		StabilityLevel: metrics.ALPHA,
	})

	// VirtualPods is the number of virtual pods in memory.
	VirtualPods = metrics.NewGauge(&metrics.GaugeOpts{
		Namespace:      namespace,
		Name:           "virtual_pods",
		Help:           "Number of virtual pods simulated in memory.",
		StabilityLevel: metrics.ALPHA,
	})

	// InformerObjects is the number of objects in each informer cache.
	InformerObjects = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      namespace,
		Name:           "informer_objects",
		Help:           "Number of objects in each informer cache, by resource.",
		StabilityLevel: metrics.ALPHA,
	}, []string{"resource"})

	// BuildInfo exposes the version and upstream release.
	BuildInfo = metrics.NewGaugeVec(&metrics.GaugeOpts{
		Namespace:      namespace,
		Name:           "build_info",
		Help:           "Build information: version and the Kubernetes release the HPA controller is copied from.",
		StabilityLevel: metrics.ALPHA,
	}, []string{"version", "upstream"})
)

var registerOnce sync.Once

// Register registers the global metrics.
func Register() {
	registerOnce.Do(func() {
		for _, m := range []metrics.Registerable{
			MetricsAPIRequestDuration, ProjectionDuration, SourcePodsExtrapolated,
			ControllerSyncs, ControllerSyncDuration, APIWrites, Decisions,
			StoreWriteDuration, StoreBatchSize, StoreQueueLength, StoreWritten, StoreErrors,
			StoreSizeBytes, StoreRetentionDeleted, VirtualWorkloads, VirtualPods, InformerObjects, BuildInfo,
		} {
			legacyregistry.MustRegister(m)
		}
		BuildInfo.WithLabelValues(version.Version, version.UpstreamKubernetes).Set(1)
	})
}
