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

package virtual

import (
	"context"
	"slices"
	"strings"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/observability"
	"github.com/fedosin/dryrun-hpa/internal/virtual/source"
	dryrunclient "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned/typed/dryrunhpa/v1alpha1"
	dryrunlisters "github.com/fedosin/dryrun-hpa/pkg/generated/listers/dryrunhpa/v1alpha1"
)

const (
	controllerVirtualWorkload = "virtualworkload"
	indexVWBySource           = "bySource"

	// ReasonSourceFound is the SourceAvailable reason when the source exists.
	ReasonSourceFound = "SourceFound"
)

// +kubebuilder:rbac:groups=dryrun-hpa.io,resources=virtualworkloads/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dryrun-hpa.io,resources=virtualworkloads/scale,verbs=get;update;patch

// Controller simulates the pods of VirtualWorkloads and writes their status.
type Controller struct {
	vwLister   dryrunlisters.VirtualWorkloadLister
	client     dryrunclient.VirtualWorkloadsGetter
	sources    *source.Registry
	store      *Store
	projector  *Projector
	objects    *observability.ObjectMetrics
	statusSync time.Duration
	now        func() time.Time

	queue workqueue.TypedRateLimitingInterface[string]
}

// ControllerOptions configures a Controller.
type ControllerOptions struct {
	VWInformer cache.SharedIndexInformer
	VWLister   dryrunlisters.VirtualWorkloadLister
	Client     dryrunclient.VirtualWorkloadsGetter
	Sources    *source.Registry
	Store      *Store
	Projector  *Projector
	// StatusSyncPeriod is how often status.sourceUsage is refreshed when
	// nothing else changed.
	StatusSyncPeriod time.Duration
}

func vwSourceIndex(obj interface{}) ([]string, error) {
	vw, ok := obj.(*v1alpha1.VirtualWorkload)
	if !ok {
		return nil, nil
	}
	gv, err := schema.ParseGroupVersion(vw.Spec.SourceRef.APIVersion)
	if err != nil {
		return nil, nil
	}
	return []string{source.Key(gv.WithKind(vw.Spec.SourceRef.Kind).GroupKind(), vw.Namespace, vw.Spec.SourceRef.Name)}, nil
}

// NewController registers event handlers and indexes; call it before the informers start.
func NewController(o ControllerOptions) (*Controller, error) {
	c := &Controller{
		vwLister:   o.VWLister,
		client:     o.Client,
		sources:    o.Sources,
		store:      o.Store,
		projector:  o.Projector,
		objects:    observability.Objects(),
		statusSync: o.StatusSyncPeriod,
		now:        time.Now,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "dryrun_virtualworkload"}),
	}
	if err := o.VWInformer.AddIndexers(cache.Indexers{indexVWBySource: vwSourceIndex}); err != nil {
		return nil, err
	}
	if _, err := o.VWInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.enqueue,
		UpdateFunc: func(old, cur interface{}) {
			// Status updates are our own writes.
			if old.(*v1alpha1.VirtualWorkload).Generation != cur.(*v1alpha1.VirtualWorkload).Generation {
				c.enqueue(cur)
			}
		},
		DeleteFunc: c.deleted,
	}); err != nil {
		return nil, err
	}
	indexer := o.VWInformer.GetIndexer()
	err := o.Sources.AddEventHandler(func(key string) {
		objs, err := indexer.ByIndex(indexVWBySource, key)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		for _, obj := range objs {
			c.enqueue(obj)
		}
	})
	return c, err
}

func (c *Controller) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	c.queue.Add(key)
}

// Enqueue requests a sync of a VirtualWorkload.
func (c *Controller) Enqueue(namespace, name string) {
	c.queue.Add(namespace + "/" + name)
}

func (c *Controller) deleted(obj interface{}) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	vw, ok := obj.(*v1alpha1.VirtualWorkload)
	if !ok {
		return
	}
	c.store.Delete(vw.UID)
	c.projector.Forget(vw.UID)
	if name, ok := adapter.DryrunHPANameForVirtualWorkload(vw.Name); ok {
		c.objects.Delete(vw.Namespace, name)
	}
	c.updateGauges()
}

func (c *Controller) updateGauges() {
	workloads, pods := c.store.Len()
	observability.VirtualWorkloads.Set(float64(workloads))
	observability.VirtualPods.Set(float64(pods))
}

// Run processes the queue until ctx is done.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	logger := klog.FromContext(ctx).WithName(controllerVirtualWorkload)
	logger.Info("Starting VirtualWorkload controller")
	for range workers {
		go wait.UntilWithContext(ctx, func(ctx context.Context) {
			for c.processNext(klog.NewContext(ctx, logger)) {
			}
		}, time.Second)
	}
	<-ctx.Done()
}

func (c *Controller) processNext(ctx context.Context) bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)
	start := time.Now()
	requeue, err := c.sync(ctx, key)
	observability.ControllerSyncDuration.WithLabelValues(controllerVirtualWorkload).Observe(time.Since(start).Seconds())
	if err != nil {
		observability.ControllerSyncs.WithLabelValues(controllerVirtualWorkload, "error").Inc()
		utilruntime.HandleErrorWithContext(ctx, err, "Syncing VirtualWorkload failed", "virtualWorkload", key)
		c.queue.AddRateLimited(key)
		return true
	}
	observability.ControllerSyncs.WithLabelValues(controllerVirtualWorkload, "success").Inc()
	c.queue.Forget(key)
	if requeue > 0 {
		c.queue.AddAfter(key, requeue)
	}
	return true
}

func (c *Controller) sync(ctx context.Context, key string) (time.Duration, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return 0, nil
	}
	vw, err := c.vwLister.VirtualWorkloads(namespace).Get(name)
	if k8serrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if vw.DeletionTimestamp != nil {
		return 0, nil
	}

	var info *source.Info
	var srcErr error
	gv, err := schema.ParseGroupVersion(vw.Spec.SourceRef.APIVersion)
	if err != nil {
		srcErr = err
	} else if src, err := c.sources.Get(gv.WithKind(vw.Spec.SourceRef.Kind).GroupKind()); err != nil {
		srcErr = err
	} else {
		info, srcErr = src.Get(namespace, vw.Spec.SourceRef.Name)
	}
	var selector labels.Selector
	if info != nil {
		selector = info.Selector
	}
	if err := c.store.Sync(vw, selector); err != nil {
		return 0, err
	}
	c.updateGauges()
	replicas, ready, nextReady := c.store.Counts(vw.UID)

	status := vw.Status.DeepCopy()
	status.Replicas = replicas
	status.ReadyReplicas = ready
	status.Selector = labels.Set{v1alpha1.LabelVirtualWorkload: string(vw.UID)}.String()
	status.ObservedGeneration = vw.Generation
	cond := metav1.Condition{Type: v1alpha1.ConditionSourceAvailable, Status: metav1.ConditionTrue, Reason: ReasonSourceFound,
		Message: "the source workload exists", ObservedGeneration: vw.Generation}
	if info != nil {
		status.SourceSelector = info.Selector.String()
		status.SourceReplicas = info.Replicas
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = v1alpha1.ReasonFailedGetSourceWorkload
		cond.Message = srcErr.Error()
	}
	apimeta.SetStatusCondition(&status.Conditions, cond)

	usage := c.sourceUsage(vw)
	changed := !apiequality.Semantic.DeepEqual(withoutUsage(&vw.Status), withoutUsage(status))
	now := c.now()
	due := status.LastSyncTime == nil || now.Sub(status.LastSyncTime.Time) >= c.statusSync
	usageChanged := !apiequality.Semantic.DeepEqual(vw.Status.SourceUsage, usage)
	if changed || (due && usageChanged) {
		status.SourceUsage = usage
		status.LastSyncTime = &metav1.Time{Time: now}
		updated := vw.DeepCopy()
		updated.Status = *status
		if _, err := c.client.VirtualWorkloads(namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return 0, err
		}
		observability.APIWrites.WithLabelValues("virtualworkloads", "update_status").Inc()
	}

	if dhpa, ok := adapter.DryrunHPANameForVirtualWorkload(vw.Name); ok && c.objects.Enabled() {
		c.objects.SetVirtualReplicas(namespace, dhpa, replicas, ready)
		if info != nil {
			c.objects.SetSourceReplicas(namespace, dhpa, info.Replicas)
		}
		for _, u := range usage {
			for r, q := range u.Usage {
				c.objects.SetSourceUsage(namespace, dhpa, u.Name, string(r), float64(q.MilliValue())/1000)
			}
		}
	}

	requeue := c.statusSync
	if nextReady > 0 && nextReady < requeue {
		requeue = nextReady + 10*time.Millisecond
	}
	return requeue, nil
}

func (c *Controller) sourceUsage(vw *v1alpha1.VirtualWorkload) []v1alpha1.ContainerUsage {
	u, ok := c.projector.Usage(vw.UID)
	if !ok {
		return vw.Status.SourceUsage
	}
	out := make([]v1alpha1.ContainerUsage, 0, len(u.Containers))
	for name, rl := range u.Containers {
		out = append(out, v1alpha1.ContainerUsage{Name: name, Usage: rl, Pods: u.Pods})
	}
	slices.SortFunc(out, func(a, b v1alpha1.ContainerUsage) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func withoutUsage(s *v1alpha1.VirtualWorkloadStatus) *v1alpha1.VirtualWorkloadStatus {
	c := s.DeepCopy()
	c.SourceUsage = nil
	c.LastSyncTime = nil
	for i := range c.Conditions {
		c.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	return c
}
