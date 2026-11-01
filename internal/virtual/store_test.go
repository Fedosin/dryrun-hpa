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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	podutil "github.com/fedosin/dryrun-hpa/internal/upstream/api/v1/pod"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time       { return c.t }
func (c *fakeClock) step(d time.Duration) { c.t = c.t.Add(d) }

func newVW(replicas int32) *v1alpha1.VirtualWorkload {
	return &v1alpha1.VirtualWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "dhpa-web", Namespace: "prod", UID: "vw-uid"},
		Spec: v1alpha1.VirtualWorkloadSpec{
			Replicas:  replicas,
			SourceRef: v1alpha1.SourceReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"},
			Containers: []v1alpha1.VirtualContainer{
				{Name: "app", Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}},
				{Name: "proxy", Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}, RestartableInit: true},
			},
		},
	}
}

func newTestStore() (*Store, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	s := NewStore(Defaults{PodReadinessDelay: 15 * time.Second, MetricsDelay: 30 * time.Second})
	s.now = clock.now
	return s, clock
}

func TestStoreInitialPodsAreMature(t *testing.T) {
	s, _ := newTestStore()
	require.NoError(t, s.Sync(newVW(3), labels.Everything()))

	replicas, ready, next := s.Counts("vw-uid")
	assert.Equal(t, int32(3), replicas)
	assert.Equal(t, int32(3), ready)
	assert.Zero(t, next)
	for _, p := range s.VirtualPods("prod", "vw-uid") {
		assert.True(t, podutil.IsPodReady(p))
		assert.Equal(t, "vw-uid", p.Labels[v1alpha1.LabelVirtualWorkload])
		require.Len(t, p.Spec.Containers, 1)
		require.Len(t, p.Spec.InitContainers, 1)
		assert.Equal(t, corev1.ContainerRestartPolicyAlways, *p.Spec.InitContainers[0].RestartPolicy)
	}
}

func TestStoreScaleUpWaitsForReadiness(t *testing.T) {
	s, clock := newTestStore()
	require.NoError(t, s.Sync(newVW(2), nil))
	require.NoError(t, s.Sync(newVW(4), nil))

	replicas, ready, next := s.Counts("vw-uid")
	assert.Equal(t, int32(4), replicas)
	assert.Equal(t, int32(2), ready)
	assert.Equal(t, 15*time.Second, next)

	clock.step(15 * time.Second)
	_, ready, next = s.Counts("vw-uid")
	assert.Equal(t, int32(4), ready)
	assert.Zero(t, next)
}

func TestStoreScaleDownRemovesUnreadyFirst(t *testing.T) {
	s, _ := newTestStore()
	require.NoError(t, s.Sync(newVW(2), nil))
	require.NoError(t, s.Sync(newVW(3), nil))
	require.NoError(t, s.Sync(newVW(2), nil))

	v, ok := s.Get("vw-uid")
	require.True(t, ok)
	require.Len(t, v.Pods, 2)
	for _, p := range v.Pods {
		assert.Less(t, p.Ordinal, int64(2), "the new unready pod was removed")
	}
	assert.Len(t, s.VirtualPods("prod", "vw-uid"), 2)
}

func TestStoreKeepsSelectorAndNamespace(t *testing.T) {
	s, _ := newTestStore()
	sel := labels.SelectorFromSet(labels.Set{"app": "web"})
	require.NoError(t, s.Sync(newVW(1), sel))
	require.NoError(t, s.Sync(newVW(1), nil))

	v, _ := s.Get("vw-uid")
	assert.Equal(t, sel.String(), v.SourceSelector.String())
	assert.Empty(t, s.VirtualPods("other", "vw-uid"))

	s.Delete("vw-uid")
	_, ok := s.Get("vw-uid")
	assert.False(t, ok)
}

func TestStoreContainerChangeRebuildsPods(t *testing.T) {
	s, _ := newTestStore()
	require.NoError(t, s.Sync(newVW(1), nil))
	before := s.VirtualPods("prod", "vw-uid")[0]

	vw := newVW(1)
	vw.Spec.Containers[0].Requests[corev1.ResourceCPU] = resource.MustParse("1")
	require.NoError(t, s.Sync(vw, nil))
	after := s.VirtualPods("prod", "vw-uid")[0]

	assert.Equal(t, "500m", before.Spec.Containers[0].Resources.Requests.Cpu().String())
	assert.Equal(t, "1", after.Spec.Containers[0].Resources.Requests.Cpu().String())
}

func TestStoreSimulationOverrides(t *testing.T) {
	s, _ := newTestStore()
	vw := newVW(1)
	vw.Spec.Simulation.MetricsDelay = &metav1.Duration{Duration: time.Minute}
	require.NoError(t, s.Sync(vw, nil))
	v, _ := s.Get("vw-uid")
	assert.Equal(t, time.Minute, v.MetricsDelay)
}
