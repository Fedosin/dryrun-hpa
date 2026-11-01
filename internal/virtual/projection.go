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
	"math"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	resourceclient "k8s.io/metrics/pkg/client/clientset/versioned/typed/metrics/v1beta1"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/observability"
	podutil "github.com/fedosin/dryrun-hpa/internal/upstream/api/v1/pod"
	metricsclient "github.com/fedosin/dryrun-hpa/internal/upstream/controller/podautoscaler/metrics"
)

// MissingPods selects how ready real pods without a metric sample count.
type MissingPods string

const (
	// MissingPodsExtrapolate scales the sum up as if those pods used the
	// average of the pods that reported.
	MissingPodsExtrapolate MissingPods = "extrapolate"
	// MissingPodsIgnore uses the plain sum of the reported samples.
	MissingPodsIgnore MissingPods = "ignore"
)

// Usage is the summed usage of a real workload.
type Usage struct {
	// Containers maps container name to summed usage.
	Containers map[string]corev1.ResourceList
	// Pods is the number of real pods that contributed.
	Pods int32
	// Time is when the usage was observed.
	Time time.Time
}

// Projector implements the upstream MetricsClient. For selectors of
// VirtualWorkloads it reads the real metrics of the source workload, sums
// them per container and divides the sums evenly over the virtual pods that
// report metrics. Other selectors, object and external metrics go to the
// real metrics APIs unchanged.
type Projector struct {
	store          *Store
	podLister      corelisters.PodLister
	resourceClient resourceclient.PodMetricsesGetter
	rest           metricsclient.MetricsClient
	missingPods    MissingPods
	now            func() time.Time
	tracer         trace.Tracer

	mu    sync.RWMutex
	usage map[types.UID]Usage
}

var _ metricsclient.MetricsClient = &Projector{}

// NewProjector returns a projector. rest is the upstream REST metrics
// client, used for pass-through and custom metrics.
func NewProjector(store *Store, podLister corelisters.PodLister, resourceClient resourceclient.PodMetricsesGetter,
	rest metricsclient.MetricsClient, missingPods MissingPods) *Projector {
	return &Projector{
		store:          store,
		podLister:      podLister,
		resourceClient: resourceClient,
		rest:           rest,
		missingPods:    missingPods,
		now:            time.Now,
		tracer:         otel.Tracer("github.com/fedosin/dryrun-hpa/internal/virtual"),
		usage:          map[types.UID]Usage{},
	}
}

// Usage returns the last observed usage of the source of a VirtualWorkload.
func (p *Projector) Usage(uid types.UID) (Usage, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	u, ok := p.usage[uid]
	return u, ok
}

// Forget drops the usage of a deleted VirtualWorkload.
func (p *Projector) Forget(uid types.UID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.usage, uid)
}

func (p *Projector) view(namespace string, selector labels.Selector) (*View, bool, error) {
	uid, ok := adapter.VirtualWorkloadUID(selector)
	if !ok {
		return nil, false, nil
	}
	v, found := p.store.Get(uid)
	if !found || v.Namespace != namespace {
		return nil, true, fmt.Errorf("virtual workload %s is not simulated yet", uid)
	}
	if v.SourceSelector == nil {
		return nil, true, fmt.Errorf("%s: the source workload of %s/%s is unknown", v1alpha1.ReasonFailedGetSourceWorkload, v.Namespace, v.Name)
	}
	return v, true, nil
}

// readyRealPods returns the names of real pods that serve load: running,
// ready and not being deleted.
func (p *Projector) readyRealPods(namespace string, selector labels.Selector) (map[string]bool, error) {
	pods, err := p.podLister.Pods(namespace).List(selector)
	if err != nil {
		return nil, err
	}
	ready := make(map[string]bool, len(pods))
	for _, pod := range pods {
		if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && podutil.IsPodReady(pod) {
			ready[pod.Name] = true
		}
	}
	return ready, nil
}

// reportingVirtualPods returns the virtual pods that have passed metricsDelay.
func (p *Projector) reportingVirtualPods(v *View) []string {
	now := p.now()
	names := make([]string, 0, len(v.Pods))
	for _, pod := range v.Pods {
		if !now.Before(pod.Created.Add(v.MetricsDelay)) {
			names = append(names, v.PodName(pod))
		}
	}
	return names
}

// scale returns the factor that extrapolates a sum over reporting pods to
// all ready pods.
func (p *Projector) scale(readyCount, reportingReady int) float64 {
	if p.missingPods != MissingPodsExtrapolate || reportingReady == 0 || reportingReady >= readyCount {
		return 1
	}
	observability.SourcePodsExtrapolated.Add(float64(readyCount - reportingReady))
	return float64(readyCount) / float64(reportingReady)
}

// GetResourceMetric implements MetricsClient.
func (p *Projector) GetResourceMetric(ctx context.Context, res corev1.ResourceName, namespace string, selector labels.Selector, container string) (metricsclient.PodMetricsInfo, time.Time, error) {
	v, isVirtual, err := p.view(namespace, selector)
	if !isVirtual {
		start := time.Now()
		info, ts, err := p.rest.GetResourceMetric(ctx, res, namespace, selector, container)
		observeAPI("resource", start, err)
		return info, ts, err
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	ctx, span := p.tracer.Start(ctx, "ProjectResourceMetric", trace.WithAttributes(
		attribute.String("namespace", namespace), attribute.String("virtualWorkload", v.Name),
		attribute.String("resource", string(res)), attribute.String("container", container)))
	defer span.End()

	start := time.Now()
	list, err := p.resourceClient.PodMetricses(namespace).List(ctx, metav1.ListOptions{LabelSelector: v.SourceSelector.String()})
	observeAPI("resource", start, err)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("unable to fetch metrics of the source workload from resource metrics API: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, time.Time{}, fmt.Errorf("no metrics returned from resource metrics API for the source workload")
	}

	var info metricsclient.PodMetricsInfo
	observability.Do(ctx, observability.ComponentProjection, func(context.Context) {
		defer observeProjection(time.Now(), autoscalingv2.ResourceMetricSourceType)
		info, err = p.projectResource(v, list.Items, res, container)
	})
	if err != nil {
		return nil, time.Time{}, err
	}
	return info, list.Items[0].Timestamp.Time, nil
}

func (p *Projector) projectResource(v *View, items []metricsapi.PodMetrics, res corev1.ResourceName, container string) (metricsclient.PodMetricsInfo, error) {
	ready, err := p.readyRealPods(v.Namespace, v.SourceSelector)
	if err != nil {
		return nil, err
	}
	// Sum every resource per container over ready real pods; keep all
	// resources so status.sourceUsage is complete.
	totals := map[string]map[corev1.ResourceName]int64{}
	reporting := 0
	for i := range items {
		m := &items[i]
		if !ready[m.Name] || len(m.Containers) == 0 {
			continue
		}
		reporting++
		for _, c := range m.Containers {
			t, ok := totals[c.Name]
			if !ok {
				t = map[corev1.ResourceName]int64{}
				totals[c.Name] = t
			}
			for r, q := range c.Usage {
				t[r] += q.MilliValue()
			}
		}
	}
	factor := p.scale(len(ready), reporting)
	p.recordUsage(v.UID, totals, factor, reporting)

	var total float64
	found := false
	for name, t := range totals {
		if container != "" && name != container {
			continue
		}
		if val, ok := t[res]; ok {
			total += float64(val) * factor
			found = true
		}
	}
	if !found {
		// Same outcome as upstream when no pod reports the container or resource.
		return metricsclient.PodMetricsInfo{}, nil
	}
	return p.distribute(v, total, items[0].Timestamp.Time, items[0].Window.Duration), nil
}

func (p *Projector) distribute(v *View, total float64, timestamp time.Time, window time.Duration) metricsclient.PodMetricsInfo {
	names := p.reportingVirtualPods(v)
	info := make(metricsclient.PodMetricsInfo, len(names))
	if len(names) == 0 {
		return info
	}
	perPod := int64(math.Round(total / float64(len(names))))
	for _, n := range names {
		info[n] = metricsclient.PodMetric{Timestamp: timestamp, Window: window, Value: perPod}
	}
	return info
}

func (p *Projector) recordUsage(uid types.UID, totals map[string]map[corev1.ResourceName]int64, factor float64, pods int) {
	u := Usage{Containers: make(map[string]corev1.ResourceList, len(totals)), Pods: int32(pods), Time: p.now()}
	for name, t := range totals {
		rl := corev1.ResourceList{}
		for r, milli := range t {
			format := resource.DecimalSI
			if r == corev1.ResourceMemory {
				format = resource.BinarySI
			}
			rl[r] = *resource.NewMilliQuantity(int64(math.Round(float64(milli)*factor)), format)
		}
		u.Containers[name] = rl
	}
	p.mu.Lock()
	p.usage[uid] = u
	p.mu.Unlock()
}

// GetRawMetric implements MetricsClient for Pods metrics.
func (p *Projector) GetRawMetric(metricName string, namespace string, selector labels.Selector, metricSelector labels.Selector) (metricsclient.PodMetricsInfo, time.Time, error) {
	v, isVirtual, err := p.view(namespace, selector)
	if !isVirtual {
		start := time.Now()
		info, ts, err := p.rest.GetRawMetric(metricName, namespace, selector, metricSelector)
		observeAPI("custom", start, err)
		return info, ts, err
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	start := time.Now()
	real, ts, err := p.rest.GetRawMetric(metricName, namespace, v.SourceSelector, metricSelector)
	observeAPI("custom", start, err)
	if err != nil {
		return nil, time.Time{}, err
	}
	var info metricsclient.PodMetricsInfo
	observability.Do(context.Background(), observability.ComponentProjection, func(context.Context) {
		defer observeProjection(time.Now(), autoscalingv2.PodsMetricSourceType)
		var ready map[string]bool
		ready, err = p.readyRealPods(namespace, v.SourceSelector)
		if err != nil {
			return
		}
		var total int64
		reporting := 0
		var window time.Duration
		for name, m := range real {
			if !ready[name] {
				continue
			}
			total += m.Value
			reporting++
			window = m.Window
		}
		info = p.distribute(v, float64(total)*p.scale(len(ready), reporting), ts, window)
	})
	return info, ts, err
}

// GetObjectMetric implements MetricsClient; object metrics don't depend on the replica count.
func (p *Projector) GetObjectMetric(metricName string, namespace string, objectRef *autoscalingv2.CrossVersionObjectReference, metricSelector labels.Selector) (int64, time.Time, error) {
	start := time.Now()
	v, ts, err := p.rest.GetObjectMetric(metricName, namespace, objectRef, metricSelector)
	observeAPI("custom", start, err)
	return v, ts, err
}

// GetExternalMetric implements MetricsClient; external metrics don't depend on the replica count.
func (p *Projector) GetExternalMetric(metricName string, namespace string, selector labels.Selector) ([]int64, time.Time, error) {
	start := time.Now()
	v, ts, err := p.rest.GetExternalMetric(metricName, namespace, selector)
	observeAPI("external", start, err)
	return v, ts, err
}

func observeAPI(api string, start time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	observability.MetricsAPIRequestDuration.WithLabelValues(api, result).Observe(time.Since(start).Seconds())
}

func observeProjection(start time.Time, t autoscalingv2.MetricSourceType) {
	observability.ProjectionDuration.WithLabelValues(string(t)).Observe(time.Since(start).Seconds())
}
