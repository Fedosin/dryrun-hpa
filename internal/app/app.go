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

// Package app wires the dry-run HPA controller together.
package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/pprof"
	"os"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
	resourceclient "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"
	"k8s.io/metrics/pkg/client/custom_metrics"
	"k8s.io/metrics/pkg/client/external_metrics"

	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/exportapi"
	"github.com/fedosin/dryrun-hpa/internal/observability"
	"github.com/fedosin/dryrun-hpa/internal/recorder"
	"github.com/fedosin/dryrun-hpa/internal/store"
	"github.com/fedosin/dryrun-hpa/internal/upstream/controller/podautoscaler"
	metricsclient "github.com/fedosin/dryrun-hpa/internal/upstream/controller/podautoscaler/metrics"
	"github.com/fedosin/dryrun-hpa/internal/version"
	"github.com/fedosin/dryrun-hpa/internal/virtual"
	"github.com/fedosin/dryrun-hpa/internal/virtual/source"
	dryrunclientset "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned"
	dryruninformers "github.com/fedosin/dryrun-hpa/pkg/generated/informers/externalversions"
)

// +kubebuilder:rbac:groups=dryrun-hpa.io,resources=dryrunhorizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=dryrun-hpa.io,resources=dryrunhorizontalpodautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=custom.metrics.k8s.io,resources=*,verbs=get;list
// +kubebuilder:rbac:groups=external.metrics.k8s.io,resources=*,verbs=get;list
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch

// trimManagedFields drops managed fields from cached objects, as
// kube-controller-manager does.
func trimManagedFields(obj interface{}) (interface{}, error) {
	if accessor, err := meta.Accessor(obj); err == nil && accessor.GetManagedFields() != nil {
		accessor.SetManagedFields(nil)
	}
	return obj, nil
}

func restConfig(o *Options) (*rest.Config, error) {
	var cfg *rest.Config
	var err error
	if o.Kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", o.Kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
		if errors.Is(err, rest.ErrNotInCluster) {
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
		}
	}
	if err != nil {
		return nil, err
	}
	cfg.QPS = o.KubeAPIQPS
	cfg.Burst = int(o.KubeAPIBurst)
	cfg.UserAgent = "dryrun-hpa-controller/" + version.Version
	return cfg, nil
}

// Run runs the controller until ctx is done.
func Run(ctx context.Context, o *Options) error {
	logger := klog.FromContext(ctx)
	observability.Register()
	if o.PerObjectMetrics {
		observability.EnableObjectMetrics()
	}
	stopTracing, err := observability.SetupTracing(ctx, observability.TracingOptions{
		Endpoint: o.TracingEndpoint, Insecure: o.TracingInsecure, SamplingRatio: o.TracingSamplingRatio})
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = stopTracing(shutdownCtx)
	}()

	cfg, err := restConfig(o)
	if err != nil {
		return fmt.Errorf("building client config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dryrun, err := dryrunclientset.NewForConfig(cfg)
	if err != nil {
		return err
	}

	var st store.Store = store.None{}
	if o.DecisionStore == "sqlite" {
		sq, err := store.OpenSQLite(ctx, o.DecisionDBPath)
		if err != nil {
			return fmt.Errorf("opening decision database: %w", err)
		}
		st = sq
	}
	defer st.Close()

	var ready atomic.Bool
	servers := startServers(ctx, o, st, &ready)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(shutdownCtx)
		}
	}()

	run := func(ctx context.Context) {
		if err := runControllers(ctx, o, cfg, kube, dryrun, st, &ready); err != nil {
			logger.Error(err, "Controllers failed")
			klog.FlushAndExit(klog.ExitFlushTimeout, 1)
		}
	}
	if !o.LeaderElection.LeaderElect {
		run(ctx)
		return nil
	}
	return runWithLeaderElection(ctx, o, kube, run)
}

func runWithLeaderElection(ctx context.Context, o *Options, kube kubernetes.Interface, run func(context.Context)) error {
	logger := klog.FromContext(ctx)
	id, err := os.Hostname()
	if err != nil {
		return err
	}
	id += "_" + string(uuid.NewUUID())
	broadcaster := record.NewBroadcaster(record.WithContext(ctx))
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	defer broadcaster.Shutdown()
	lock, err := resourcelock.New(o.LeaderElection.ResourceLock, o.LeaderElection.ResourceNamespace, o.LeaderElection.ResourceName,
		kube.CoreV1(), kube.CoordinationV1(), resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "dryrun-hpa-controller"}),
		})
	if err != nil {
		return err
	}
	var started atomic.Bool
	finished := make(chan struct{})
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   o.LeaderElection.LeaseDuration.Duration,
		RenewDeadline:   o.LeaderElection.RenewDeadline.Duration,
		RetryPeriod:     o.LeaderElection.RetryPeriod.Duration,
		ReleaseOnCancel: true,
		Name:            o.LeaderElection.ResourceName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				started.Store(true)
				defer close(finished)
				run(ctx)
			},
			OnStoppedLeading: func() {
				if ctx.Err() == nil {
					logger.Info("Leader election lost")
					klog.FlushAndExit(klog.ExitFlushTimeout, 1)
				}
			},
		},
	})
	// RunOrDie doesn't wait for OnStartedLeading; queued decisions must be
	// written before the store closes.
	if started.Load() {
		<-finished
	}
	return nil
}

func runControllers(ctx context.Context, o *Options, cfg *rest.Config, kube kubernetes.Interface,
	dryrun dryrunclientset.Interface, st store.Store, ready *atomic.Bool) error {
	logger := klog.FromContext(ctx)

	kubeFactory := informers.NewSharedInformerFactoryWithOptions(kube, resyncPeriod(), informers.WithTransform(trimManagedFields))
	dryrunFactory := dryruninformers.NewSharedInformerFactoryWithOptions(dryrun, resyncPeriod())

	dhpaInformer := dryrunFactory.Dryrun().V1alpha1().DryrunHorizontalPodAutoscalers().Informer()
	vwInformer := dryrunFactory.Dryrun().V1alpha1().VirtualWorkloads()
	podInformer := kubeFactory.Core().V1().Pods()

	// The DryrunHPA informer stores HPAs; trimming happens before the conversion.
	hpaInformer, err := adapter.NewHPAInformer(dhpaInformer)
	if err != nil {
		return err
	}

	sources := source.NewRegistry(source.NewDeploymentAdapter(kubeFactory.Apps().V1().Deployments()))
	vstore := virtual.NewStore(virtual.Defaults{PodReadinessDelay: o.PodReadinessDelay.Duration, MetricsDelay: o.MetricsDelay.Duration})

	// Discovery-backed REST mapper and scale client, as in kube-controller-manager.
	cachedDiscovery := memory.NewMemCacheClient(kube.Discovery())
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscovery)
	go wait.Until(mapper.Reset, 30*time.Second, ctx.Done())
	scaleClient, err := scale.NewForConfig(cfg, mapper, dynamic.LegacyAPIPathResolverFunc, scale.NewDiscoveryScaleKindResolver(kube.Discovery()))
	if err != nil {
		return err
	}

	apiVersionsGetter := custom_metrics.NewAvailableAPIsGetter(kube.Discovery())
	go custom_metrics.PeriodicallyInvalidate(apiVersionsGetter, o.HPASyncPeriod.Duration, ctx.Done())
	resourceMetrics, err := resourceclient.NewForConfig(cfg)
	if err != nil {
		return err
	}
	externalMetrics, err := external_metrics.NewForConfig(cfg)
	if err != nil {
		return err
	}
	restMetrics := metricsclient.NewRESTMetricsClient(resourceMetrics, custom_metrics.NewForConfig(cfg, mapper, apiVersionsGetter), externalMetrics)
	projector := virtual.NewProjector(vstore, podInformer.Lister(), resourceMetrics, restMetrics, virtual.MissingPods(o.SourceMissingPods))

	eventBroadcaster := record.NewBroadcaster(record.WithContext(ctx))
	eventBroadcaster.StartStructuredLogging(3)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	defer eventBroadcaster.Shutdown()
	events := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "dryrun-hpa-provisioner"})

	vwController, err := virtual.NewController(virtual.ControllerOptions{
		VWInformer:       vwInformer.Informer(),
		VWLister:         vwInformer.Lister(),
		Client:           dryrun.DryrunV1alpha1(),
		Sources:          sources,
		Store:            vstore,
		Projector:        projector,
		StatusSyncPeriod: o.VirtualStatusSyncPeriod.Duration,
	})
	if err != nil {
		return err
	}
	provisioner, err := virtual.NewProvisioner(virtual.ProvisionerOptions{
		DryrunHPAInformer: dhpaInformer,
		VWInformer:        vwInformer.Informer(),
		VWLister:          vwInformer.Lister(),
		Client:            dryrun.DryrunV1alpha1(),
		Sources:           sources,
		PodLister:         podInformer.Lister(),
		Events:            events,
		Resync:            o.ProvisionerResyncPeriod.Duration,
		OnSynced:          vwController.Enqueue,
	})
	if err != nil {
		return err
	}

	real := &realState{sources: sources}
	if o.CompareRealHPA {
		realHPAs := kubeFactory.Autoscaling().V2().HorizontalPodAutoscalers().Informer()
		if err := realHPAs.AddIndexers(cache.Indexers{indexHPAByTarget: hpaTargetIndex}); err != nil {
			return err
		}
		real.realHPAs = realHPAs.GetIndexer()
	}

	var writer *store.Writer
	if o.DecisionStore != "none" {
		writer = store.NewWriter(st, store.WriterOptions{
			QueueSize:     o.DecisionQueueSize,
			BatchSize:     o.DecisionBatchSize,
			FlushInterval: o.DecisionFlushInterval.Duration,
			Retention:     o.DecisionRetention.Duration,
		})
	}

	podautoscaler.SetEventObjectFunc(adapter.EventObject)
	hpaController := podautoscaler.NewHorizontalController(
		ctx,
		kube.CoreV1(),
		adapter.NewScaleClient(scaleClient, vwInformer.Lister(), o.VirtualWorkloadWait.Duration),
		adapter.NewHPAClient(dryrun.DryrunV1alpha1(), provisioner),
		mapper,
		projector,
		hpaInformer,
		adapter.NewPodInformer(podInformer, vstore),
		o.HPASyncPeriod.Duration,
		o.HPADownscaleStabilization.Duration,
		o.HPATolerance,
		o.HPACPUInitializationPeriod.Duration,
		o.HPAInitialReadinessDelay.Duration,
	)
	hpaController.SetReconcileObserver(recorder.New(writer, real))

	observability.Do(ctx, observability.ComponentInformers, func(ctx context.Context) {
		kubeFactory.Start(ctx.Done())
		dryrunFactory.Start(ctx.Done())
	})
	defer kubeFactory.Shutdown()
	defer dryrunFactory.Shutdown()
	logger.Info("Waiting for informer caches to sync")
	for t, ok := range kubeFactory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("cache for %v did not sync", t)
		}
	}
	for t, ok := range dryrunFactory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("cache for %v did not sync", t)
		}
	}
	ready.Store(true)
	go reportInformerSizes(ctx, map[string]cache.SharedIndexInformer{
		"pods":                           podInformer.Informer(),
		"deployments":                    kubeFactory.Apps().V1().Deployments().Informer(),
		"dryrunhorizontalpodautoscalers": dhpaInformer,
		"virtualworkloads":               vwInformer.Informer(),
	})

	done := make(chan struct{})
	var running int
	start := func(component string, f func(context.Context)) {
		running++
		observability.Go(ctx, component, func(ctx context.Context) {
			defer func() { done <- struct{}{} }()
			f(ctx)
		})
	}
	if writer != nil {
		start(observability.ComponentStore, writer.Run)
	}
	start(observability.ComponentProvisioner, func(ctx context.Context) { provisioner.Run(ctx, int(o.ConcurrentProvisionerSyncs)) })
	start(observability.ComponentVirtualWorkload, func(ctx context.Context) { vwController.Run(ctx, int(o.ConcurrentVirtualSyncs)) })
	start(observability.ComponentHPALoop, func(ctx context.Context) { hpaController.Run(ctx, int(o.ConcurrentHPASyncs)) })
	logger.Info("Controllers started", "version", version.Version, "upstream", version.UpstreamKubernetes)
	for range running {
		<-done
	}
	return nil
}

func reportInformerSizes(ctx context.Context, informers map[string]cache.SharedIndexInformer) {
	wait.UntilWithContext(ctx, func(context.Context) {
		for name, inf := range informers {
			observability.InformerObjects.WithLabelValues(name).Set(float64(len(inf.GetStore().ListKeys())))
		}
	}, 30*time.Second)
}

func startServers(ctx context.Context, o *Options, st store.Store, ready *atomic.Bool) []*http.Server {
	logger := klog.FromContext(ctx)
	var servers []*http.Server
	serve := func(name, addr string, h http.Handler) {
		if addr == "" || addr == "0" {
			return
		}
		s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, s)
		observability.Go(ctx, observability.ComponentHTTP, func(context.Context) {
			logger.Info("Serving", "endpoint", name, "address", addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				utilruntime.HandleErrorWithContext(ctx, err, "HTTP server failed", "endpoint", name)
			}
		})
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", legacyregistry.Handler())
	if o.EnablePprof {
		metricsMux.HandleFunc("/debug/pprof/", pprof.Index)
		metricsMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		metricsMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		metricsMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		metricsMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	serve("metrics", o.MetricsBindAddress, metricsMux)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		// Standby replicas are ready: they serve the export API.
		if o.LeaderElection.LeaderElect || ready.Load() {
			_, _ = w.Write([]byte("ok"))
			return
		}
		http.Error(w, "informer caches not synced", http.StatusServiceUnavailable)
	})
	serve("health", o.HealthBindAddress, healthMux)

	serve("export", o.ExportBindAddress, exportapi.NewHandler(st))
	return servers
}

// resyncPeriod matches kube-controller-manager's default informer resync:
// a random duration in [12h, 24h) so factories don't resync in lockstep.
func resyncPeriod() time.Duration {
	const minResync = 12 * time.Hour
	return time.Duration(float64(minResync) * (1 + rand.Float64()))
}
