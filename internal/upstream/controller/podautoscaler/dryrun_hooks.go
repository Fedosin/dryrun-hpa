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

// This file is not part of upstream Kubernetes. It holds the hooks that
// hack/upstream-patches wires into horizontal.go.

package podautoscaler

import (
	"context"
	"sync"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
)

// ReconcileObserver is notified about every reconciliation of the HPA loop.
type ReconcileObserver interface {
	// BeginReconcile is called before the reconciliation starts. The returned
	// context is used for the rest of the reconciliation.
	BeginReconcile(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, key string) context.Context
	// EndReconcile is called once the reconciliation finished, including
	// reconciliations that failed early.
	EndReconcile(ctx context.Context, d *Decision)
}

// MetricProposal is the replica count proposed by one entry of spec.metrics.
type MetricProposal struct {
	Replicas int32
	Err      error
}

// Decision is everything the HPA loop decided during one reconciliation.
type Decision struct {
	Key   string
	Start time.Time
	End   time.Time

	// HPA is the working copy at the end of the reconciliation; its status
	// is what the loop computed (and wrote, if it changed).
	HPA *autoscalingv2.HorizontalPodAutoscaler

	// ScaleRead reports whether the scale subresource was read successfully.
	ScaleRead bool
	// CurrentReplicas is spec.replicas of the target's scale subresource.
	CurrentReplicas int32

	// MetricsComputed reports whether replicas were computed from metrics.
	// It is false when the current count was outside [minReplicas, maxReplicas]
	// or scaling was disabled.
	MetricsComputed bool
	// MetricReplicas is the highest replica count proposed by the metrics.
	MetricReplicas int32
	// MetricName describes the metric that proposed MetricReplicas.
	MetricName string
	// MetricProposals has one entry per spec.metrics entry.
	MetricProposals []MetricProposal

	// StabilizedReplicas is the recommendation after the stabilization
	// window, before rate limits and min/max are applied.
	StabilizedReplicas *int32
	// DesiredReplicas is the final decision.
	DesiredReplicas int32
	// Rescale reports whether the loop decided to change the replica count.
	Rescale bool
	// RescaleReason is the reason reported in the SuccessfulRescale event.
	RescaleReason string

	// Err is the error returned by the reconciliation, if any.
	Err error

	mu sync.Mutex
}

func (d *Decision) observeScale(currentReplicas int32) {
	if d == nil {
		return
	}
	d.ScaleRead = true
	d.CurrentReplicas = currentReplicas
}

func (d *Decision) observeDesired(metricsComputed bool, metricReplicas int32, metricName string, desiredReplicas int32, rescale bool, reason string) {
	if d == nil {
		return
	}
	d.MetricsComputed = metricsComputed
	d.MetricReplicas = metricReplicas
	d.MetricName = metricName
	d.DesiredReplicas = desiredReplicas
	d.Rescale = rescale
	d.RescaleReason = reason
}

type dryrunHooks struct {
	observer ReconcileObserver
	inflight sync.Map // HPA key -> *Decision
}

// SetReconcileObserver registers an observer. It must be called before Run.
func (a *HorizontalController) SetReconcileObserver(o ReconcileObserver) {
	a.dryrun.observer = o
}

func (h *dryrunHooks) begin(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, key string) (context.Context, *Decision) {
	if h.observer == nil {
		return ctx, nil
	}
	d := &Decision{Key: key, Start: time.Now(), HPA: hpa}
	h.inflight.Store(key, d)
	return h.observer.BeginReconcile(ctx, hpa, key), d
}

func (h *dryrunHooks) end(ctx context.Context, d *Decision, hpa *autoscalingv2.HorizontalPodAutoscaler, err error) {
	if d == nil {
		return
	}
	h.inflight.Delete(d.Key)
	d.End = time.Now()
	d.HPA = hpa
	d.Err = err
	if !d.Rescale {
		d.DesiredReplicas = hpa.Status.DesiredReplicas
	}
	h.observer.EndReconcile(ctx, d)
}

func (h *dryrunHooks) decision(key string) *Decision {
	if h.observer == nil {
		return nil
	}
	v, ok := h.inflight.Load(key)
	if !ok {
		return nil
	}
	return v.(*Decision)
}

func (h *dryrunHooks) observeStabilized(key string, stabilized int32) {
	d := h.decision(key)
	if d == nil {
		return
	}
	d.StabilizedReplicas = &stabilized
}

func (h *dryrunHooks) observeMetricProposal(hpa *autoscalingv2.HorizontalPodAutoscaler, index int, replicas int32, err error) {
	d := h.decision(hpa.Namespace + "/" + hpa.Name)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for len(d.MetricProposals) <= index {
		d.MetricProposals = append(d.MetricProposals, MetricProposal{})
	}
	d.MetricProposals[index] = MetricProposal{Replicas: replicas, Err: err}
}

// EventObjectFunc maps the HPA objects the loop emits events for to the
// object the events should be attached to.
type EventObjectFunc func(hpa *autoscalingv2.HorizontalPodAutoscaler) runtime.Object

// EventReasonPrefix is prepended to the reason of every event the loop emits.
const EventReasonPrefix = "DryRun"

// eventObjectFunc is set by SetEventObjectFunc before NewHorizontalController.
var eventObjectFunc EventObjectFunc

// SetEventObjectFunc sets how events are attached; call it before
// NewHorizontalController.
func SetEventObjectFunc(f EventObjectFunc) {
	eventObjectFunc = f
}

type dryrunEventRecorder struct {
	inner record.EventRecorder
}

func wrapEventRecorder(r record.EventRecorder) record.EventRecorder {
	return &dryrunEventRecorder{inner: r}
}

// rewrite attaches the event to the object returned by eventObjectFunc and
// prefixes the reason. Without an eventObjectFunc (as in the upstream unit
// tests) events are passed through unchanged.
func (r *dryrunEventRecorder) rewrite(obj runtime.Object, reason string) (runtime.Object, string) {
	if eventObjectFunc == nil {
		return obj, reason
	}
	if hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler); ok {
		obj = eventObjectFunc(hpa)
	}
	return obj, EventReasonPrefix + reason
}

func (r *dryrunEventRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	object, reason = r.rewrite(object, reason)
	r.inner.Event(object, eventtype, reason, message)
}

func (r *dryrunEventRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	object, reason = r.rewrite(object, reason)
	r.inner.Eventf(object, eventtype, reason, messageFmt, args...)
}

func (r *dryrunEventRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	object, reason = r.rewrite(object, reason)
	r.inner.AnnotatedEventf(object, annotations, eventtype, reason, messageFmt, args...)
}
