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

package recorder

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/component-base/metrics/testutil"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/observability"
	"github.com/fedosin/dryrun-hpa/internal/store"
	"github.com/fedosin/dryrun-hpa/internal/upstream/controller/podautoscaler"
)

type fakeReal struct{}

func (fakeReal) SourceReplicas(string, autoscalingv2.CrossVersionObjectReference) *int32 {
	return ptr.To[int32](3)
}
func (fakeReal) RealHPADesired(string, autoscalingv2.CrossVersionObjectReference) *int32 {
	return ptr.To[int32](5)
}

func decision() *podautoscaler.Decision {
	hpa := adapter.ToHPA(&v1alpha1.DryrunHorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", UID: "uid"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"},
			MinReplicas:    ptr.To[int32](2),
			MaxReplicas:    10,
			Metrics: []autoscalingv2.MetricSpec{
				{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](60)}}},
				{Type: autoscalingv2.PodsMetricSourceType, Pods: &autoscalingv2.PodsMetricSource{
					Metric: autoscalingv2.MetricIdentifier{Name: "rps"},
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType, AverageValue: ptr.To(resource.MustParse("100"))}}},
			},
		},
	})
	hpa.Status.CurrentMetrics = []autoscalingv2.MetricStatus{
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricStatus{
			Name: corev1.ResourceCPU, Current: autoscalingv2.MetricValueStatus{AverageUtilization: ptr.To[int32](90), AverageValue: ptr.To(resource.MustParse("450m"))}}},
		{},
	}
	hpa.Status.Conditions = []autoscalingv2.HorizontalPodAutoscalerCondition{{Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue, Reason: "SucceededRescale"}}
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	return &podautoscaler.Decision{
		Key: "prod/web", Start: start, End: start.Add(4 * time.Millisecond), HPA: hpa,
		ScaleRead: true, CurrentReplicas: 4, MetricsComputed: true, MetricReplicas: 6, MetricName: "cpu resource utilization (percentage of request)",
		MetricProposals:    []podautoscaler.MetricProposal{{Replicas: 6}, {Err: errors.New("no metrics")}},
		StabilizedReplicas: ptr.To[int32](6), DesiredReplicas: 6, Rescale: true, RescaleReason: "cpu resource utilization (percentage of request) above target",
	}
}

func TestRecord(t *testing.T) {
	r := New(nil, fakeReal{})
	rec := r.record(decision())

	assert.Equal(t, "prod", rec.Namespace)
	assert.Equal(t, "Deployment", rec.TargetKind, "original target, not the VirtualWorkload")
	assert.Equal(t, "web", rec.TargetName)
	assert.Len(t, rec.SpecHash, 16)
	assert.Equal(t, int32(2), rec.MinReplicas)
	assert.Equal(t, int32(4), rec.CurrentReplicas)
	assert.Equal(t, int32(6), *rec.MetricReplicas)
	assert.Equal(t, int32(6), rec.DesiredReplicas)
	assert.Equal(t, int32(3), *rec.SourceReplicas)
	assert.Equal(t, int32(5), *rec.RealHPADesired)
	assert.Equal(t, store.Duration(4*time.Millisecond), rec.Duration)
	assert.Contains(t, string(rec.Spec), `"kind":"Deployment"`)
	require.Len(t, rec.Conditions, 1)

	require.Len(t, rec.Metrics, 2)
	assert.Equal(t, store.Metric{Index: 0, Type: "Resource", Name: "cpu", TargetType: "Utilization", TargetValue: "60",
		Current: "90", ProposedReplicas: ptr.To[int32](6)}, rec.Metrics[0])
	assert.Equal(t, "rps", rec.Metrics[1].Name)
	assert.Equal(t, "100", rec.Metrics[1].TargetValue)
	assert.Equal(t, "no metrics", rec.Metrics[1].Error)
	assert.Nil(t, rec.Metrics[1].ProposedReplicas)
	assert.Empty(t, rec.Metrics[1].Current)
}

func TestEndReconcileWritesAndObserves(t *testing.T) {
	observability.Register()
	observability.EnableObjectMetrics()
	st, err := store.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	require.NoError(t, err)
	defer st.Close()
	w := store.NewWriter(st, store.WriterOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	r := New(w, fakeReal{})
	d := decision()
	r.EndReconcile(r.BeginReconcile(context.Background(), d.HPA, d.Key), d)
	cancel()
	w.Wait()

	n := 0
	for rec, err := range st.Query(context.Background(), store.Filter{}) {
		require.NoError(t, err)
		assert.Equal(t, int32(6), rec.DesiredReplicas)
		n++
	}
	assert.Equal(t, 1, n)

	expected := `
# HELP dryrun_hpa_recommendation_replicas [ALPHA] Replica counts at each stage of the last decision: metric (highest metric proposal), stabilized, desired (final).
# TYPE dryrun_hpa_recommendation_replicas gauge
dryrun_hpa_recommendation_replicas{name="web",namespace="prod",stage="desired"} 6
dryrun_hpa_recommendation_replicas{name="web",namespace="prod",stage="metric"} 6
dryrun_hpa_recommendation_replicas{name="web",namespace="prod",stage="stabilized"} 6
`
	require.NoError(t, testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(expected), "dryrun_hpa_recommendation_replicas"))
	require.NoError(t, testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(`
# HELP dryrun_hpa_decisions_total [ALPHA] Decisions made by the HPA loop, by outcome (scale_up, scale_down, none, error) and whether they were queued for the store (queued, dropped, disabled).
# TYPE dryrun_hpa_decisions_total counter
dryrun_hpa_decisions_total{outcome="scale_up",store="queued"} 1
`), "dryrun_hpa_decisions_total"))

	observability.Objects().Delete("prod", "web")
	require.NoError(t, testutil.GatherAndCompare(legacyregistry.DefaultGatherer, strings.NewReader(""), "dryrun_hpa_recommendation_replicas"))
}
