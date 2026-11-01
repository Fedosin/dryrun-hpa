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

// Package recorder turns the decisions of the upstream HPA loop into
// stored records, metrics and trace spans.
package recorder

import (
	"context"
	"encoding/json"
	"strconv"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	autoscalingv2 "k8s.io/api/autoscaling/v2"

	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/observability"
	"github.com/fedosin/dryrun-hpa/internal/store"
	"github.com/fedosin/dryrun-hpa/internal/upstream/controller/podautoscaler"
)

// RealState reports the state of the real objects next to a DryrunHPA.
type RealState interface {
	// SourceReplicas returns spec.replicas of the real workload.
	SourceReplicas(namespace string, target autoscalingv2.CrossVersionObjectReference) *int32
	// RealHPADesired returns status.desiredReplicas of a real HPA on the
	// same workload, or nil.
	RealHPADesired(namespace string, target autoscalingv2.CrossVersionObjectReference) *int32
}

// Recorder implements podautoscaler.ReconcileObserver.
type Recorder struct {
	writer  *store.Writer
	real    RealState
	objects *observability.ObjectMetrics
	tracer  trace.Tracer
}

var _ podautoscaler.ReconcileObserver = &Recorder{}

// New returns a recorder. writer may be nil when decisions aren't stored.
func New(writer *store.Writer, real RealState) *Recorder {
	return &Recorder{
		writer:  writer,
		real:    real,
		objects: observability.Objects(),
		tracer:  otel.Tracer("github.com/fedosin/dryrun-hpa/internal/recorder"),
	}
}

// BeginReconcile implements podautoscaler.ReconcileObserver.
func (r *Recorder) BeginReconcile(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, key string) context.Context {
	ctx, _ = r.tracer.Start(ctx, "ReconcileDryrunHPA", trace.WithAttributes(
		attribute.String("namespace", hpa.Namespace), attribute.String("name", hpa.Name),
		attribute.String("specHash", hpa.Annotations[adapter.AnnotationSpecHash])))
	return ctx
}

// EndReconcile implements podautoscaler.ReconcileObserver.
func (r *Recorder) EndReconcile(ctx context.Context, d *podautoscaler.Decision) {
	span := trace.SpanFromContext(ctx)
	defer span.End()
	observability.Do(ctx, observability.ComponentRecorder, func(context.Context) {
		rec := r.record(d)
		span.SetAttributes(attribute.Int("currentReplicas", int(rec.CurrentReplicas)),
			attribute.Int("desiredReplicas", int(rec.DesiredReplicas)), attribute.Bool("rescale", rec.Rescale))
		if d.Err != nil {
			span.RecordError(d.Err)
			span.SetStatus(codes.Error, d.Err.Error())
		}
		r.observe(d, &rec)
		queued := "disabled"
		if r.writer != nil {
			queued = "queued"
			if !r.writer.Enqueue(rec) {
				queued = "dropped"
			}
		}
		observability.Decisions.WithLabelValues(outcome(&rec), queued).Inc()
	})
}

func outcome(r *store.Record) string {
	switch {
	case r.Error != "":
		return "error"
	case r.DesiredReplicas > r.CurrentReplicas:
		return "scale_up"
	case r.DesiredReplicas < r.CurrentReplicas:
		return "scale_down"
	default:
		return "none"
	}
}

func (r *Recorder) record(d *podautoscaler.Decision) store.Record {
	hpa := d.HPA
	spec := adapter.EffectiveSpec(hpa)
	rec := store.Record{
		Time:            d.End,
		Namespace:       hpa.Namespace,
		Name:            hpa.Name,
		UID:             string(hpa.UID),
		SpecHash:        hpa.Annotations[adapter.AnnotationSpecHash],
		TargetKind:      spec.ScaleTargetRef.Kind,
		TargetName:      spec.ScaleTargetRef.Name,
		MaxReplicas:     spec.MaxReplicas,
		CurrentReplicas: d.CurrentReplicas,
		DesiredReplicas: d.DesiredReplicas,
		MetricName:      d.MetricName,
		Rescale:         d.Rescale,
		RescaleReason:   d.RescaleReason,
		Duration:        store.Duration(d.End.Sub(d.Start)),
	}
	if spec.MinReplicas != nil {
		rec.MinReplicas = *spec.MinReplicas
	}
	if d.MetricsComputed {
		mr := d.MetricReplicas
		rec.MetricReplicas = &mr
	}
	rec.StabilizedReplicas = d.StabilizedReplicas
	if d.Err != nil {
		rec.Error = d.Err.Error()
	}
	if raw, err := json.Marshal(spec); err == nil {
		rec.Spec = raw
	}
	if r.real != nil {
		rec.SourceReplicas = r.real.SourceReplicas(hpa.Namespace, spec.ScaleTargetRef)
		rec.RealHPADesired = r.real.RealHPADesired(hpa.Namespace, spec.ScaleTargetRef)
	}
	for _, c := range hpa.Status.Conditions {
		rec.Conditions = append(rec.Conditions, store.Condition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message})
	}
	for i, m := range spec.Metrics {
		mr := metricRecord(i, m)
		if i < len(hpa.Status.CurrentMetrics) {
			mr.Current = currentValue(hpa.Status.CurrentMetrics[i], mr.TargetType)
		}
		if i < len(d.MetricProposals) {
			p := d.MetricProposals[i]
			if p.Err != nil {
				mr.Error = p.Err.Error()
			} else {
				n := p.Replicas
				mr.ProposedReplicas = &n
			}
		}
		rec.Metrics = append(rec.Metrics, mr)
	}
	return rec
}

func metricRecord(i int, m autoscalingv2.MetricSpec) store.Metric {
	mr := store.Metric{Index: i, Type: string(m.Type)}
	var target autoscalingv2.MetricTarget
	switch m.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if m.Resource != nil {
			mr.Name, target = string(m.Resource.Name), m.Resource.Target
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		if m.ContainerResource != nil {
			mr.Name, mr.Container, target = string(m.ContainerResource.Name), m.ContainerResource.Container, m.ContainerResource.Target
		}
	case autoscalingv2.PodsMetricSourceType:
		if m.Pods != nil {
			mr.Name, target = m.Pods.Metric.Name, m.Pods.Target
		}
	case autoscalingv2.ObjectMetricSourceType:
		if m.Object != nil {
			mr.Name, target = m.Object.Metric.Name, m.Object.Target
		}
	case autoscalingv2.ExternalMetricSourceType:
		if m.External != nil {
			mr.Name, target = m.External.Metric.Name, m.External.Target
		}
	}
	mr.TargetType = string(target.Type)
	switch {
	case target.AverageUtilization != nil:
		mr.TargetValue = strconv.Itoa(int(*target.AverageUtilization))
	case target.AverageValue != nil:
		mr.TargetValue = target.AverageValue.String()
	case target.Value != nil:
		mr.TargetValue = target.Value.String()
	}
	return mr
}

func metricValueStatus(s autoscalingv2.MetricStatus) *autoscalingv2.MetricValueStatus {
	switch s.Type {
	case autoscalingv2.ResourceMetricSourceType:
		if s.Resource != nil {
			return &s.Resource.Current
		}
	case autoscalingv2.ContainerResourceMetricSourceType:
		if s.ContainerResource != nil {
			return &s.ContainerResource.Current
		}
	case autoscalingv2.PodsMetricSourceType:
		if s.Pods != nil {
			return &s.Pods.Current
		}
	case autoscalingv2.ObjectMetricSourceType:
		if s.Object != nil {
			return &s.Object.Current
		}
	case autoscalingv2.ExternalMetricSourceType:
		if s.External != nil {
			return &s.External.Current
		}
	}
	return nil
}

func currentValue(s autoscalingv2.MetricStatus, targetType string) string {
	v := metricValueStatus(s)
	if v == nil {
		return ""
	}
	switch {
	case targetType == string(autoscalingv2.UtilizationMetricType) && v.AverageUtilization != nil:
		return strconv.Itoa(int(*v.AverageUtilization))
	case targetType == string(autoscalingv2.ValueMetricType) && v.Value != nil:
		return v.Value.String()
	case v.AverageValue != nil:
		return v.AverageValue.String()
	case v.Value != nil:
		return v.Value.String()
	}
	return ""
}

func (r *Recorder) observe(d *podautoscaler.Decision, rec *store.Record) {
	if !r.objects.Enabled() {
		return
	}
	ns, name := rec.Namespace, rec.Name
	if rec.MetricReplicas != nil {
		r.objects.SetRecommendation(ns, name, "metric", *rec.MetricReplicas)
	}
	if rec.StabilizedReplicas != nil {
		r.objects.SetRecommendation(ns, name, "stabilized", *rec.StabilizedReplicas)
	}
	r.objects.SetRecommendation(ns, name, "desired", rec.DesiredReplicas)
	if rec.RealHPADesired != nil {
		r.objects.SetRealHPADesired(ns, name, *rec.RealHPADesired)
	}
	for _, c := range rec.Conditions {
		r.objects.SetCondition(ns, name, c.Type, c.Status, c.Reason)
	}
	for i, m := range adapter.EffectiveSpec(d.HPA).Metrics {
		mr := rec.Metrics[i]
		if i < len(d.HPA.Status.CurrentMetrics) {
			if v := metricValueStatus(d.HPA.Status.CurrentMetrics[i]); v != nil {
				switch {
				case v.AverageUtilization != nil:
					r.objects.SetMetricValue(ns, name, i, mr.Name, "current", float64(*v.AverageUtilization))
				case v.AverageValue != nil:
					r.objects.SetMetricValue(ns, name, i, mr.Name, "current", v.AverageValue.AsApproximateFloat64())
				case v.Value != nil:
					r.objects.SetMetricValue(ns, name, i, mr.Name, "current", v.Value.AsApproximateFloat64())
				}
			}
		}
		if t := targetFloat(m); t != nil {
			r.objects.SetMetricValue(ns, name, i, mr.Name, "target", *t)
		}
	}
}

func targetFloat(m autoscalingv2.MetricSpec) *float64 {
	var t autoscalingv2.MetricTarget
	switch {
	case m.Resource != nil:
		t = m.Resource.Target
	case m.ContainerResource != nil:
		t = m.ContainerResource.Target
	case m.Pods != nil:
		t = m.Pods.Target
	case m.Object != nil:
		t = m.Object.Target
	case m.External != nil:
		t = m.External.Target
	}
	var v float64
	switch {
	case t.AverageUtilization != nil:
		v = float64(*t.AverageUtilization)
	case t.AverageValue != nil:
		v = t.AverageValue.AsApproximateFloat64()
	case t.Value != nil:
		v = t.Value.AsApproximateFloat64()
	default:
		return nil
	}
	return &v
}
