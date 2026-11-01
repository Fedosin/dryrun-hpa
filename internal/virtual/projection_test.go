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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	corelisters "k8s.io/client-go/listers/core/v1"
	core "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsfake "k8s.io/metrics/pkg/client/clientset/versioned/fake"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	metricsclient "github.com/fedosin/dryrun-hpa/internal/upstream/controller/podautoscaler/metrics"
)

var sourceLabels = labels.Set{"app": "web"}

type stubREST struct {
	raw      metricsclient.PodMetricsInfo
	selector labels.Selector
	calls    int
}

func (s *stubREST) GetResourceMetric(context.Context, corev1.ResourceName, string, labels.Selector, string) (metricsclient.PodMetricsInfo, time.Time, error) {
	s.calls++
	return metricsclient.PodMetricsInfo{"real": {Value: 1}}, time.Time{}, nil
}

func (s *stubREST) GetRawMetric(_ string, _ string, selector labels.Selector, _ labels.Selector) (metricsclient.PodMetricsInfo, time.Time, error) {
	s.calls++
	s.selector = selector
	return s.raw, time.Time{}, nil
}

func (s *stubREST) GetObjectMetric(string, string, *autoscalingv2.CrossVersionObjectReference, labels.Selector) (int64, time.Time, error) {
	s.calls++
	return 42, time.Time{}, nil
}

func (s *stubREST) GetExternalMetric(string, string, labels.Selector) ([]int64, time.Time, error) {
	s.calls++
	return []int64{7}, time.Time{}, nil
}

func realPod(name string, ready bool) *corev1.Pod {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: sourceLabels},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func podMetrics(name string, usage map[string]string) metricsapi.PodMetrics {
	m := metricsapi.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "prod", Labels: sourceLabels},
		Timestamp:  metav1.Time{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
		Window:     metav1.Duration{Duration: 30 * time.Second},
	}
	for c, cpu := range usage {
		m.Containers = append(m.Containers, metricsapi.ContainerMetrics{Name: c, Usage: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}})
	}
	return m
}

type projectorFixture struct {
	projector *Projector
	store     *Store
	clock     *fakeClock
	rest      *stubREST
	selector  labels.Selector
}

func newProjectorFixture(t *testing.T, missing MissingPods, pods []*corev1.Pod, metrics []metricsapi.PodMetrics, virtualReplicas int32) *projectorFixture {
	t.Helper()
	s, clock := newTestStore()
	require.NoError(t, s.Sync(newVW(virtualReplicas), labels.SelectorFromSet(sourceLabels)))

	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, p := range pods {
		require.NoError(t, indexer.Add(p))
	}
	client := &metricsfake.Clientset{}
	client.AddReactor("list", "pods", func(core.Action) (bool, runtime.Object, error) {
		return true, &metricsapi.PodMetricsList{Items: metrics}, nil
	})
	rest := &stubREST{}
	p := NewProjector(s, corelisters.NewPodLister(indexer), client.MetricsV1beta1(), rest, missing)
	p.now = clock.now
	return &projectorFixture{
		projector: p, store: s, clock: clock, rest: rest,
		selector: labels.SelectorFromSet(labels.Set{v1alpha1.LabelVirtualWorkload: "vw-uid"}),
	}
}

func TestProjectResourceMetricDividesSum(t *testing.T) {
	f := newProjectorFixture(t, MissingPodsExtrapolate,
		[]*corev1.Pod{realPod("a", true), realPod("b", true), realPod("c", true)},
		[]metricsapi.PodMetrics{
			podMetrics("a", map[string]string{"app": "300m", "proxy": "10m"}),
			podMetrics("b", map[string]string{"app": "600m", "proxy": "20m"}),
			podMetrics("c", map[string]string{"app": "900m", "proxy": "30m"}),
		}, 2)

	info, ts, err := f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", f.selector, "")
	require.NoError(t, err)
	assert.False(t, ts.IsZero())
	require.Len(t, info, 2)
	for name, m := range info {
		assert.Contains(t, name, "dhpa-web-v")
		assert.Equal(t, int64(930), m.Value, "(1800m + 60m) / 2 virtual pods")
		assert.Equal(t, 30*time.Second, m.Window)
	}

	info, _, err = f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", f.selector, "app")
	require.NoError(t, err)
	for _, m := range info {
		assert.Equal(t, int64(900), m.Value, "container metric")
	}

	usage, ok := f.projector.Usage("vw-uid")
	require.True(t, ok)
	assert.Equal(t, int32(3), usage.Pods)
	appUsage := usage.Containers["app"]
	assert.Equal(t, int64(1800), appUsage.Cpu().MilliValue())
	assert.Zero(t, f.rest.calls, "virtual selectors don't reach the real API")
}

func TestProjectResourceMetricMissingPods(t *testing.T) {
	pods := []*corev1.Pod{realPod("a", true), realPod("b", true), realPod("unready", false)}
	metrics := []metricsapi.PodMetrics{
		podMetrics("a", map[string]string{"app": "400m"}),
		podMetrics("unready", map[string]string{"app": "5"}),
	}

	f := newProjectorFixture(t, MissingPodsExtrapolate, pods, metrics, 1)
	info, _, err := f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", f.selector, "")
	require.NoError(t, err)
	for _, m := range info {
		assert.Equal(t, int64(800), m.Value, "b is ready without a sample: extrapolated from a; unready pods are ignored")
	}

	f = newProjectorFixture(t, MissingPodsIgnore, pods, metrics, 1)
	info, _, err = f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", f.selector, "")
	require.NoError(t, err)
	for _, m := range info {
		assert.Equal(t, int64(400), m.Value)
	}
}

func TestProjectResourceMetricSkipsYoungVirtualPods(t *testing.T) {
	f := newProjectorFixture(t, MissingPodsExtrapolate,
		[]*corev1.Pod{realPod("a", true)},
		[]metricsapi.PodMetrics{podMetrics("a", map[string]string{"app": "1"})}, 1)
	require.NoError(t, f.store.Sync(newVW(2), nil))

	info, _, err := f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", f.selector, "")
	require.NoError(t, err)
	require.Len(t, info, 1, "the new pod has no metrics before metricsDelay")
	for _, m := range info {
		assert.Equal(t, int64(1000), m.Value)
	}

	f.clock.step(30 * time.Second)
	info, _, err = f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", f.selector, "")
	require.NoError(t, err)
	require.Len(t, info, 2)
	for _, m := range info {
		assert.Equal(t, int64(500), m.Value)
	}
}

func TestProjectorUnknownWorkload(t *testing.T) {
	f := newProjectorFixture(t, MissingPodsExtrapolate, nil, nil, 1)
	other := labels.SelectorFromSet(labels.Set{v1alpha1.LabelVirtualWorkload: "unknown"})
	_, _, err := f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", other, "")
	assert.Error(t, err)
}

func TestProjectorPassThrough(t *testing.T) {
	f := newProjectorFixture(t, MissingPodsExtrapolate, nil, nil, 1)
	info, _, err := f.projector.GetResourceMetric(context.Background(), corev1.ResourceCPU, "prod", labels.SelectorFromSet(sourceLabels), "")
	require.NoError(t, err)
	assert.Contains(t, info, "real")

	v, _, err := f.projector.GetObjectMetric("qps", "prod", &autoscalingv2.CrossVersionObjectReference{}, labels.Everything())
	require.NoError(t, err)
	assert.Equal(t, int64(42), v)
	ext, _, err := f.projector.GetExternalMetric("queue", "prod", labels.Everything())
	require.NoError(t, err)
	assert.Equal(t, []int64{7}, ext)
	assert.Equal(t, 3, f.rest.calls)
}

func TestProjectRawMetric(t *testing.T) {
	f := newProjectorFixture(t, MissingPodsExtrapolate, []*corev1.Pod{realPod("a", true), realPod("b", true)}, nil, 4)
	f.rest.raw = metricsclient.PodMetricsInfo{"a": {Value: 3000}, "b": {Value: 5000}, "gone": {Value: 100000}}

	info, _, err := f.projector.GetRawMetric("requests_per_second", "prod", f.selector, labels.Everything())
	require.NoError(t, err)
	assert.Equal(t, labels.SelectorFromSet(sourceLabels).String(), f.rest.selector.String(), "queried with the source selector")
	require.Len(t, info, 4)
	for _, m := range info {
		assert.Equal(t, int64(2000), m.Value, "8000 / 4 virtual pods; pods that aren't ready real pods are ignored")
	}
}
