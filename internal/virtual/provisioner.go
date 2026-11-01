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
	"fmt"
	"sync"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/observability"
	"github.com/fedosin/dryrun-hpa/internal/virtual/source"
	dryrunclient "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned/typed/dryrunhpa/v1alpha1"
	dryrunlisters "github.com/fedosin/dryrun-hpa/pkg/generated/listers/dryrunhpa/v1alpha1"
)

// LabelDryrunHPA is set on VirtualWorkloads to the name of their DryrunHPA.
const LabelDryrunHPA = v1alpha1.GroupName + "/dryrun-hpa"

const (
	indexBySource         = "bySource"
	controllerProvisioner = "provisioner"
)

func dhpaSourceIndex(obj interface{}) ([]string, error) {
	hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		return nil, nil
	}
	ref, err := adapter.OriginalTarget(hpa)
	if err != nil {
		return nil, nil
	}
	gk, err := adapter.TargetGroupKind(ref)
	if err != nil {
		return nil, nil
	}
	return []string{source.Key(gk, hpa.Namespace, ref.Name)}, nil
}

// +kubebuilder:rbac:groups=dryrun-hpa.io,resources=virtualworkloads,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dryrun-hpa.io,resources=dryrunhorizontalpodautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch;update

// Provisioner creates and updates the VirtualWorkload of every DryrunHPA.
type Provisioner struct {
	dhpaIndexer cache.Indexer
	vwLister    dryrunlisters.VirtualWorkloadLister
	client      dryrunclient.VirtualWorkloadsGetter
	sources     *source.Registry
	podLister   corelisters.PodLister
	events      record.EventRecorder
	resync      time.Duration
	onSynced    func(namespace, vwName string)

	queue workqueue.TypedRateLimitingInterface[string]

	sourceErrs sync.Map // DryrunHPA key -> error
}

// ProvisionerOptions configures a Provisioner.
type ProvisionerOptions struct {
	// DryrunHPAInformer is the DryrunHPA informer with adapter.Transform installed.
	DryrunHPAInformer cache.SharedIndexInformer
	VWInformer        cache.SharedIndexInformer
	VWLister          dryrunlisters.VirtualWorkloadLister
	Client            dryrunclient.VirtualWorkloadsGetter
	Sources           *source.Registry
	PodLister         corelisters.PodLister
	Events            record.EventRecorder
	// Resync is how often every DryrunHPA is re-provisioned, to pick up
	// request changes on real pods.
	Resync time.Duration
	// OnSynced is called after a VirtualWorkload was provisioned.
	OnSynced func(namespace, vwName string)
}

// NewProvisioner registers event handlers and indexes; call it before the informers start.
func NewProvisioner(o ProvisionerOptions) (*Provisioner, error) {
	p := &Provisioner{
		dhpaIndexer: o.DryrunHPAInformer.GetIndexer(),
		vwLister:    o.VWLister,
		client:      o.Client,
		sources:     o.Sources,
		podLister:   o.PodLister,
		events:      o.Events,
		resync:      o.Resync,
		onSynced:    o.OnSynced,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "dryrun_provisioner"}),
	}
	if err := o.DryrunHPAInformer.AddIndexers(cache.Indexers{indexBySource: dhpaSourceIndex}); err != nil {
		return nil, err
	}
	if _, err := o.DryrunHPAInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    p.enqueue,
		UpdateFunc: func(_, obj interface{}) { p.enqueue(obj) },
		DeleteFunc: p.forget,
	}); err != nil {
		return nil, err
	}
	if _, err := o.VWInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(old, cur interface{}) {
			if old.(*v1alpha1.VirtualWorkload).Generation != cur.(*v1alpha1.VirtualWorkload).Generation {
				p.enqueueOwner(cur)
			}
		},
		DeleteFunc: p.enqueueOwner,
	}); err != nil {
		return nil, err
	}
	err := o.Sources.AddEventHandler(func(key string) {
		objs, err := p.dhpaIndexer.ByIndex(indexBySource, key)
		if err != nil {
			utilruntime.HandleError(err)
			return
		}
		for _, obj := range objs {
			p.enqueue(obj)
		}
	})
	return p, err
}

func (p *Provisioner) enqueue(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}
	p.queue.Add(key)
}

func (p *Provisioner) forget(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	p.sourceErrs.Delete(key)
}

func (p *Provisioner) enqueueOwner(obj interface{}) {
	if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	vw, ok := obj.(*v1alpha1.VirtualWorkload)
	if !ok {
		return
	}
	if name, ok := adapter.DryrunHPANameForVirtualWorkload(vw.Name); ok {
		p.queue.Add(vw.Namespace + "/" + name)
	}
}

// SourceError implements adapter.SourceErrors.
func (p *Provisioner) SourceError(namespace, name string) error {
	v, ok := p.sourceErrs.Load(namespace + "/" + name)
	if !ok {
		return nil
	}
	return v.(error)
}

// Run processes the queue until ctx is done.
func (p *Provisioner) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer p.queue.ShutDown()
	logger := klog.FromContext(ctx).WithName(controllerProvisioner)
	logger.Info("Starting VirtualWorkload provisioner")
	for range workers {
		go wait.UntilWithContext(ctx, func(ctx context.Context) {
			for p.processNext(klog.NewContext(ctx, logger)) {
			}
		}, time.Second)
	}
	<-ctx.Done()
}

func (p *Provisioner) processNext(ctx context.Context) bool {
	key, quit := p.queue.Get()
	if quit {
		return false
	}
	defer p.queue.Done(key)
	start := time.Now()
	requeue, err := p.sync(ctx, key)
	observability.ControllerSyncDuration.WithLabelValues(controllerProvisioner).Observe(time.Since(start).Seconds())
	if k8serrors.IsConflict(err) {
		observability.ControllerSyncs.WithLabelValues(controllerProvisioner, "conflict").Inc()
		klog.FromContext(ctx).V(4).Info("Conflict provisioning VirtualWorkload, retrying", "dryrunHPA", key)
		p.queue.AddRateLimited(key)
		return true
	}
	if err != nil {
		observability.ControllerSyncs.WithLabelValues(controllerProvisioner, "error").Inc()
		utilruntime.HandleErrorWithContext(ctx, err, "Provisioning VirtualWorkload failed", "dryrunHPA", key)
		p.queue.AddRateLimited(key)
		return true
	}
	observability.ControllerSyncs.WithLabelValues(controllerProvisioner, "success").Inc()
	p.queue.Forget(key)
	if requeue > 0 {
		p.queue.AddAfter(key, requeue)
	} else if p.resync > 0 {
		p.queue.AddAfter(key, p.resync)
	}
	return true
}

func (p *Provisioner) setSourceError(key string, err error) {
	if err == nil {
		p.sourceErrs.Delete(key)
		return
	}
	p.sourceErrs.Store(key, err)
}

func (p *Provisioner) sync(ctx context.Context, key string) (time.Duration, error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return 0, nil
	}
	obj, exists, err := p.dhpaIndexer.GetByKey(key)
	if err != nil {
		return 0, err
	}
	if !exists {
		p.sourceErrs.Delete(key)
		return 0, nil
	}
	hpa := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	ref, err := adapter.OriginalTarget(hpa)
	if err != nil {
		return 0, err
	}
	gk, err := adapter.TargetGroupKind(ref)
	if err != nil {
		p.setSourceError(key, err)
		return 0, nil
	}
	src, err := p.sources.Get(gk)
	if err != nil {
		p.setSourceError(key, err)
		return 0, nil
	}
	info, err := src.Get(namespace, ref.Name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			p.setSourceError(key, fmt.Errorf("%s %s/%s not found", ref.Kind, namespace, ref.Name))
			p.notifySynced(namespace, name)
			return 0, nil
		}
		p.setSourceError(key, err)
		return 0, err
	}
	p.setSourceError(key, nil)

	pods, err := p.podLister.Pods(namespace).List(info.Selector)
	if err != nil {
		return 0, err
	}
	containers := ContainersFromPods(pods)
	if containers == nil {
		containers = info.Containers
	}
	sim, simErr := ParseSimulation(hpa.Annotations)
	containers, overrideErr := ApplyRequestOverrides(containers, sim.RequestOverrides)
	if simErr != nil || overrideErr != nil {
		p.events.Eventf(adapter.EventObject(hpa), corev1.EventTypeWarning, "DryRunInvalidAnnotation", "%v", joinErrs(simErr, overrideErr))
	}

	desired := v1alpha1.VirtualWorkloadSpec{
		Replicas:         info.Replicas,
		SourceRef:        v1alpha1.SourceReference{APIVersion: ref.APIVersion, Kind: ref.Kind, Name: ref.Name},
		LifecycleProfile: info.LifecycleProfile,
		Containers:       containers,
		Simulation:       v1alpha1.SimulationSpec{PodReadinessDelay: sim.PodReadinessDelay, MetricsDelay: sim.MetricsDelay},
	}

	vwName := adapter.VirtualWorkloadName(name)
	vw, err := p.vwLister.VirtualWorkloads(namespace).Get(vwName)
	switch {
	case k8serrors.IsNotFound(err):
		return 0, p.create(ctx, hpa, vwName, desired)
	case err != nil:
		return 0, err
	}

	owner := metav1.GetControllerOf(vw)
	if owner == nil || owner.Kind != v1alpha1.KindDryrunHorizontalPodAutoscaler {
		err := fmt.Errorf("VirtualWorkload %s/%s exists and is not owned by a DryrunHorizontalPodAutoscaler", namespace, vwName)
		p.setSourceError(key, err)
		return 0, nil
	}
	if owner.UID != hpa.UID || !sameSource(vw.Spec.SourceRef, desired.SourceRef) {
		// Owned by a previous incarnation of this DryrunHPA, or the target
		// changed: start over from the real replica count.
		return time.Second, p.delete(ctx, vw)
	}

	updated := vw.DeepCopy()
	updated.Spec.LifecycleProfile = desired.LifecycleProfile
	updated.Spec.SourceRef = desired.SourceRef
	updated.Spec.Containers = desired.Containers
	updated.Spec.Simulation = desired.Simulation
	if !apiequality.Semantic.DeepEqual(vw.Spec, updated.Spec) {
		if _, err := p.client.VirtualWorkloads(namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return 0, err
		}
		observability.APIWrites.WithLabelValues("virtualworkloads", "update").Inc()
		klog.FromContext(ctx).V(2).Info("Updated VirtualWorkload", "virtualWorkload", klog.KObj(vw))
	}
	p.notifySynced(namespace, name)
	return 0, nil
}

func (p *Provisioner) create(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, name string, spec v1alpha1.VirtualWorkloadSpec) error {
	vw := &v1alpha1.VirtualWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: hpa.Namespace,
			Labels:    map[string]string{LabelDryrunHPA: hpa.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         v1alpha1.SchemeGroupVersion.String(),
				Kind:               v1alpha1.KindDryrunHorizontalPodAutoscaler,
				Name:               hpa.Name,
				UID:                hpa.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: spec,
	}
	if _, err := p.client.VirtualWorkloads(hpa.Namespace).Create(ctx, vw, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
		return err
	}
	observability.APIWrites.WithLabelValues("virtualworkloads", "create").Inc()
	klog.FromContext(ctx).Info("Created VirtualWorkload", "virtualWorkload", klog.KRef(hpa.Namespace, name), "replicas", spec.Replicas,
		"source", spec.SourceRef.Kind+"/"+spec.SourceRef.Name)
	p.notifySynced(hpa.Namespace, hpa.Name)
	return nil
}

func (p *Provisioner) delete(ctx context.Context, vw *v1alpha1.VirtualWorkload) error {
	err := p.client.VirtualWorkloads(vw.Namespace).Delete(ctx, vw.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &vw.UID}})
	if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsConflict(err) {
		return err
	}
	observability.APIWrites.WithLabelValues("virtualworkloads", "delete").Inc()
	klog.FromContext(ctx).Info("Deleted outdated VirtualWorkload", "virtualWorkload", klog.KObj(vw))
	return nil
}

func (p *Provisioner) notifySynced(namespace, dhpaName string) {
	if p.onSynced != nil {
		p.onSynced(namespace, adapter.VirtualWorkloadName(dhpaName))
	}
}

func sameSource(a, b v1alpha1.SourceReference) bool {
	ga, errA := schema.ParseGroupVersion(a.APIVersion)
	gb, errB := schema.ParseGroupVersion(b.APIVersion)
	return errA == nil && errB == nil && ga.Group == gb.Group && a.Kind == b.Kind && a.Name == b.Name
}

func joinErrs(errs ...error) string {
	msg := ""
	for _, e := range errs {
		if e == nil {
			continue
		}
		if msg != "" {
			msg += "; "
		}
		msg += e.Error()
	}
	return msg
}
