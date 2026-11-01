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

package observability

import (
	"strconv"
	"sync"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// ObjectMetrics are per-DryrunHPA gauges. Their cardinality grows with the
// number of objects, so they are only registered with --per-object-metrics;
// when disabled every method is a no-op.
type ObjectMetrics struct {
	enabled bool

	virtualReplicas      *metrics.GaugeVec
	virtualReadyReplicas *metrics.GaugeVec
	sourceReplicas       *metrics.GaugeVec
	sourceUsage          *metrics.GaugeVec
	recommendation       *metrics.GaugeVec
	metricValue          *metrics.GaugeVec
	condition            *metrics.GaugeVec
	realHPADesired       *metrics.GaugeVec
}

var (
	objectMetricsOnce sync.Once
	objectMetrics     = &ObjectMetrics{}
)

// Objects returns the per-object metrics. They are no-ops until
// EnableObjectMetrics is called.
func Objects() *ObjectMetrics {
	return objectMetrics
}

// EnableObjectMetrics registers the per-object gauges.
func EnableObjectMetrics() *ObjectMetrics {
	objectMetricsOnce.Do(func() {
		gauge := func(name, help string, labels ...string) *metrics.GaugeVec {
			g := metrics.NewGaugeVec(&metrics.GaugeOpts{Namespace: namespace, Name: name, Help: help, StabilityLevel: metrics.ALPHA},
				append([]string{"namespace", "name"}, labels...))
			legacyregistry.MustRegister(g)
			return g
		}
		objectMetrics.virtualReplicas = gauge("virtual_replicas", "Virtual replica count of a DryrunHPA's VirtualWorkload.")
		objectMetrics.virtualReadyReplicas = gauge("virtual_ready_replicas", "Ready virtual pods of a DryrunHPA's VirtualWorkload.")
		objectMetrics.sourceReplicas = gauge("source_replicas", "Replica count (spec.replicas) of the real workload of a DryrunHPA.")
		objectMetrics.sourceUsage = gauge("source_usage", "Summed usage of the real workload's pods, in cores or bytes.", "container", "resource")
		objectMetrics.recommendation = gauge("recommendation_replicas",
			"Replica counts at each stage of the last decision: metric (highest metric proposal), stabilized, desired (final).", "stage")
		objectMetrics.metricValue = gauge("metric_value",
			"Current and target value of each spec.metrics entry, as reported in the status (utilization in percent, values in base units).",
			"index", "metric", "kind")
		objectMetrics.condition = gauge("condition", "Status of the HPA conditions (1 for the current status).", "type", "status", "reason")
		objectMetrics.realHPADesired = gauge("real_hpa_desired_replicas", "desiredReplicas of the real HPA targeting the same workload, if any.")
		objectMetrics.enabled = true
	})
	return objectMetrics
}

// Enabled reports whether per-object metrics are registered.
func (m *ObjectMetrics) Enabled() bool {
	return m.enabled
}

// SetVirtualReplicas records the virtual replica counts.
func (m *ObjectMetrics) SetVirtualReplicas(ns, name string, replicas, ready int32) {
	if !m.enabled {
		return
	}
	m.virtualReplicas.WithLabelValues(ns, name).Set(float64(replicas))
	m.virtualReadyReplicas.WithLabelValues(ns, name).Set(float64(ready))
}

// SetSourceReplicas records the real replica count.
func (m *ObjectMetrics) SetSourceReplicas(ns, name string, replicas int32) {
	if !m.enabled {
		return
	}
	m.sourceReplicas.WithLabelValues(ns, name).Set(float64(replicas))
}

// SetSourceUsage records summed real usage.
func (m *ObjectMetrics) SetSourceUsage(ns, name, container, resource string, value float64) {
	if !m.enabled {
		return
	}
	m.sourceUsage.WithLabelValues(ns, name, container, resource).Set(value)
}

// SetRecommendation records one stage of a decision.
func (m *ObjectMetrics) SetRecommendation(ns, name, stage string, replicas int32) {
	if !m.enabled {
		return
	}
	m.recommendation.WithLabelValues(ns, name, stage).Set(float64(replicas))
}

// SetMetricValue records the current or target value of a metric.
func (m *ObjectMetrics) SetMetricValue(ns, name string, index int, metric, kind string, value float64) {
	if !m.enabled {
		return
	}
	m.metricValue.WithLabelValues(ns, name, strconv.Itoa(index), metric, kind).Set(value)
}

// SetCondition records a condition; previous statuses of the type are reset.
func (m *ObjectMetrics) SetCondition(ns, name, conditionType, status, reason string) {
	if !m.enabled {
		return
	}
	m.condition.DeletePartialMatch(map[string]string{"namespace": ns, "name": name, "type": conditionType})
	m.condition.WithLabelValues(ns, name, conditionType, status, reason).Set(1)
}

// SetRealHPADesired records the desired replicas of the real HPA.
func (m *ObjectMetrics) SetRealHPADesired(ns, name string, replicas int32) {
	if !m.enabled {
		return
	}
	m.realHPADesired.WithLabelValues(ns, name).Set(float64(replicas))
}

// Delete removes all series of a DryrunHPA.
func (m *ObjectMetrics) Delete(ns, name string) {
	if !m.enabled {
		return
	}
	match := map[string]string{"namespace": ns, "name": name}
	for _, g := range []*metrics.GaugeVec{m.virtualReplicas, m.virtualReadyReplicas, m.sourceReplicas, m.sourceUsage,
		m.recommendation, m.metricValue, m.condition, m.realHPADesired} {
		g.DeletePartialMatch(match)
	}
}
