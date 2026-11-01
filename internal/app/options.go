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

package app

import (
	"fmt"
	"time"

	"github.com/spf13/pflag"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	componentbaseconfig "k8s.io/component-base/config"
	"k8s.io/component-base/config/options"
	logsapi "k8s.io/component-base/logs/api/v1"

	"github.com/fedosin/dryrun-hpa/internal/virtual"
)

// Options are the command line options of the controller.
type Options struct {
	Kubeconfig   string
	KubeAPIQPS   float32
	KubeAPIBurst int32

	// Upstream HPA controller options, with kube-controller-manager defaults.
	HPASyncPeriod              metav1.Duration
	HPATolerance               float64
	HPADownscaleStabilization  metav1.Duration
	HPACPUInitializationPeriod metav1.Duration
	HPAInitialReadinessDelay   metav1.Duration
	ConcurrentHPASyncs         int32

	// Decision store.
	DecisionStore         string
	DecisionDBPath        string
	DecisionRetention     metav1.Duration
	DecisionQueueSize     int
	DecisionBatchSize     int
	DecisionFlushInterval metav1.Duration

	// Virtual workloads.
	PodReadinessDelay          metav1.Duration
	MetricsDelay               metav1.Duration
	SourceMissingPods          string
	VirtualStatusSyncPeriod    metav1.Duration
	VirtualWorkloadWait        metav1.Duration
	ProvisionerResyncPeriod    metav1.Duration
	ConcurrentVirtualSyncs     int32
	ConcurrentProvisionerSyncs int32
	CompareRealHPA             bool

	// Observability.
	PerObjectMetrics     bool
	EnablePprof          bool
	TracingEndpoint      string
	TracingInsecure      bool
	TracingSamplingRatio float64
	Logs                 *logsapi.LoggingConfiguration

	MetricsBindAddress string
	HealthBindAddress  string
	ExportBindAddress  string

	LeaderElection componentbaseconfig.LeaderElectionConfiguration
}

// NewOptions returns the default options.
func NewOptions() *Options {
	return &Options{
		KubeAPIQPS:                 20,
		KubeAPIBurst:               30,
		HPASyncPeriod:              metav1.Duration{Duration: 15 * time.Second},
		HPATolerance:               0.1,
		HPADownscaleStabilization:  metav1.Duration{Duration: 5 * time.Minute},
		HPACPUInitializationPeriod: metav1.Duration{Duration: 5 * time.Minute},
		HPAInitialReadinessDelay:   metav1.Duration{Duration: 30 * time.Second},
		ConcurrentHPASyncs:         5,

		DecisionStore:         "sqlite",
		DecisionDBPath:        "/data/decisions.db",
		DecisionRetention:     metav1.Duration{Duration: 7 * 24 * time.Hour},
		DecisionQueueSize:     10000,
		DecisionBatchSize:     500,
		DecisionFlushInterval: metav1.Duration{Duration: time.Second},

		PodReadinessDelay:          metav1.Duration{Duration: 15 * time.Second},
		MetricsDelay:               metav1.Duration{Duration: 30 * time.Second},
		SourceMissingPods:          string(virtual.MissingPodsExtrapolate),
		VirtualStatusSyncPeriod:    metav1.Duration{Duration: 30 * time.Second},
		VirtualWorkloadWait:        metav1.Duration{Duration: 10 * time.Second},
		ProvisionerResyncPeriod:    metav1.Duration{Duration: 5 * time.Minute},
		ConcurrentVirtualSyncs:     5,
		ConcurrentProvisionerSyncs: 2,

		TracingSamplingRatio: 0.1,
		Logs:                 logsapi.NewLoggingConfiguration(),

		MetricsBindAddress: ":8080",
		HealthBindAddress:  ":8081",
		ExportBindAddress:  ":8082",

		LeaderElection: componentbaseconfig.LeaderElectionConfiguration{
			LeaderElect:       true,
			LeaseDuration:     metav1.Duration{Duration: 15 * time.Second},
			RenewDeadline:     metav1.Duration{Duration: 10 * time.Second},
			RetryPeriod:       metav1.Duration{Duration: 2 * time.Second},
			ResourceLock:      "leases",
			ResourceName:      "dryrun-hpa-controller",
			ResourceNamespace: "dryrun-hpa-system",
		},
	}
}

// AddFlags registers the options.
func (o *Options) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.Kubeconfig, "kubeconfig", o.Kubeconfig, "Path to a kubeconfig. Only required if out-of-cluster.")
	fs.Float32Var(&o.KubeAPIQPS, "kube-api-qps", o.KubeAPIQPS, "QPS to use while talking with the Kubernetes API server.")
	fs.Int32Var(&o.KubeAPIBurst, "kube-api-burst", o.KubeAPIBurst, "Burst to use while talking with the Kubernetes API server.")

	fs.DurationVar(&o.HPASyncPeriod.Duration, "horizontal-pod-autoscaler-sync-period", o.HPASyncPeriod.Duration,
		"The period for syncing the number of pods in horizontal pod autoscaler.")
	fs.Float64Var(&o.HPATolerance, "horizontal-pod-autoscaler-tolerance", o.HPATolerance,
		"The minimum change (from 1.0) in the desired-to-actual metrics ratio for the horizontal pod autoscaler to consider scaling.")
	fs.DurationVar(&o.HPADownscaleStabilization.Duration, "horizontal-pod-autoscaler-downscale-stabilization", o.HPADownscaleStabilization.Duration,
		"The period for which autoscaler will look backwards and not scale down below any recommendation it made during that period.")
	fs.DurationVar(&o.HPACPUInitializationPeriod.Duration, "horizontal-pod-autoscaler-cpu-initialization-period", o.HPACPUInitializationPeriod.Duration,
		"The period after pod start when CPU samples might be skipped.")
	fs.DurationVar(&o.HPAInitialReadinessDelay.Duration, "horizontal-pod-autoscaler-initial-readiness-delay", o.HPAInitialReadinessDelay.Duration,
		"The period after pod start during which readiness changes will be treated as initial readiness.")
	fs.Int32Var(&o.ConcurrentHPASyncs, "concurrent-horizontal-pod-autoscaler-syncs", o.ConcurrentHPASyncs,
		"The number of horizontal pod autoscaler objects that are allowed to sync concurrently.")

	fs.StringVar(&o.DecisionStore, "decision-store", o.DecisionStore, "Where decisions are stored: sqlite or none.")
	fs.StringVar(&o.DecisionDBPath, "decision-db-path", o.DecisionDBPath, "Path of the SQLite database.")
	fs.DurationVar(&o.DecisionRetention.Duration, "decision-retention", o.DecisionRetention.Duration, "How long decisions are kept; 0 keeps them forever.")
	fs.IntVar(&o.DecisionQueueSize, "decision-queue-size", o.DecisionQueueSize, "Decisions waiting to be written; more are dropped and counted.")
	fs.IntVar(&o.DecisionBatchSize, "decision-batch-size", o.DecisionBatchSize, "Maximum decisions per database transaction.")
	fs.DurationVar(&o.DecisionFlushInterval.Duration, "decision-flush-interval", o.DecisionFlushInterval.Duration, "Maximum time a decision waits before it is written.")

	fs.DurationVar(&o.PodReadinessDelay.Duration, "pod-readiness-delay", o.PodReadinessDelay.Duration,
		"Default time a new virtual pod takes to become ready. Overridden by the dryrun-hpa.io/pod-readiness-delay annotation.")
	fs.DurationVar(&o.MetricsDelay.Duration, "metrics-delay", o.MetricsDelay.Duration,
		"Default time after creation until a virtual pod reports metrics. Overridden by the dryrun-hpa.io/metrics-delay annotation.")
	fs.StringVar(&o.SourceMissingPods, "source-missing-pods", o.SourceMissingPods,
		"How ready real pods without metrics count: extrapolate (assume the average of the others) or ignore.")
	fs.DurationVar(&o.VirtualStatusSyncPeriod.Duration, "virtual-status-sync-period", o.VirtualStatusSyncPeriod.Duration,
		"How often VirtualWorkload status (source usage) is refreshed.")
	fs.DurationVar(&o.VirtualWorkloadWait.Duration, "virtual-workload-wait", o.VirtualWorkloadWait.Duration,
		"How long the HPA loop waits for a new VirtualWorkload to be provisioned before it reports an error.")
	fs.DurationVar(&o.ProvisionerResyncPeriod.Duration, "provisioner-resync-period", o.ProvisionerResyncPeriod.Duration,
		"How often VirtualWorkloads are re-provisioned to pick up request changes on real pods.")
	fs.Int32Var(&o.ConcurrentVirtualSyncs, "concurrent-virtual-workload-syncs", o.ConcurrentVirtualSyncs, "Concurrent VirtualWorkload status syncs.")
	fs.Int32Var(&o.ConcurrentProvisionerSyncs, "concurrent-provisioner-syncs", o.ConcurrentProvisionerSyncs, "Concurrent VirtualWorkload provisioning syncs.")
	fs.BoolVar(&o.CompareRealHPA, "compare-real-hpa", o.CompareRealHPA,
		"Watch real HPAs and record the desired replicas of a real HPA on the same workload with every decision.")

	fs.BoolVar(&o.PerObjectMetrics, "per-object-metrics", o.PerObjectMetrics,
		"Export metrics labelled by DryrunHPA. The number of series grows with the number of objects.")
	fs.BoolVar(&o.EnablePprof, "enable-pprof", o.EnablePprof, "Serve /debug/pprof on the metrics address.")
	fs.StringVar(&o.TracingEndpoint, "tracing-endpoint", o.TracingEndpoint, "OTLP gRPC endpoint (host:port) for traces. Tracing is off when empty.")
	fs.BoolVar(&o.TracingInsecure, "tracing-insecure", o.TracingInsecure, "Connect to the tracing endpoint without TLS.")
	fs.Float64Var(&o.TracingSamplingRatio, "tracing-sampling-ratio", o.TracingSamplingRatio, "Fraction of reconciles that are traced.")
	logsapi.AddFlags(o.Logs, fs)

	fs.StringVar(&o.MetricsBindAddress, "metrics-bind-address", o.MetricsBindAddress, "Address of the metrics endpoint; 0 disables it.")
	fs.StringVar(&o.HealthBindAddress, "health-probe-bind-address", o.HealthBindAddress, "Address of the health probes; 0 disables them.")
	fs.StringVar(&o.ExportBindAddress, "export-bind-address", o.ExportBindAddress, "Address of the decision export API; 0 disables it.")

	options.BindLeaderElectionFlags(&o.LeaderElection, fs)
}

// Validate checks the options.
func (o *Options) Validate() error {
	switch o.DecisionStore {
	case "sqlite", "none":
	default:
		return fmt.Errorf("--decision-store must be sqlite or none, got %q", o.DecisionStore)
	}
	switch virtual.MissingPods(o.SourceMissingPods) {
	case virtual.MissingPodsExtrapolate, virtual.MissingPodsIgnore:
	default:
		return fmt.Errorf("--source-missing-pods must be extrapolate or ignore, got %q", o.SourceMissingPods)
	}
	if o.HPASyncPeriod.Duration <= 0 {
		return fmt.Errorf("--horizontal-pod-autoscaler-sync-period must be positive")
	}
	if o.ConcurrentHPASyncs < 1 {
		return fmt.Errorf("--concurrent-horizontal-pod-autoscaler-syncs must be at least 1")
	}
	if o.TracingSamplingRatio < 0 || o.TracingSamplingRatio > 1 {
		return fmt.Errorf("--tracing-sampling-ratio must be between 0 and 1")
	}
	return nil
}
