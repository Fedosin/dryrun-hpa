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

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

func TestParseSimulation(t *testing.T) {
	sim, err := ParseSimulation(map[string]string{
		v1alpha1.AnnotationPodReadinessDelay: "45s",
		v1alpha1.AnnotationMetricsDelay:      "bogus",
		v1alpha1.AnnotationRequestOverrides:  `{"app":{"cpu":"2"}}`,
	})
	assert.Error(t, err, "invalid metrics delay is reported")
	require.NotNil(t, sim.PodReadinessDelay)
	assert.Equal(t, 45*time.Second, sim.PodReadinessDelay.Duration)
	assert.Nil(t, sim.MetricsDelay)
	app := sim.RequestOverrides["app"]
	assert.Equal(t, "2", app.Cpu().String())

	sim, err = ParseSimulation(nil)
	assert.NoError(t, err)
	assert.Nil(t, sim.PodReadinessDelay)
}

func TestApplyRequestOverrides(t *testing.T) {
	containers := []v1alpha1.VirtualContainer{
		{Name: "app", Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}},
		{Name: "proxy"},
	}
	out, err := ApplyRequestOverrides(containers, map[string]corev1.ResourceList{
		"app":     {corev1.ResourceCPU: resource.MustParse("1")},
		"proxy":   {corev1.ResourceCPU: resource.MustParse("50m")},
		"missing": {corev1.ResourceCPU: resource.MustParse("1")},
	})
	assert.Error(t, err)
	assert.Equal(t, "1", out[0].Requests.Cpu().String())
	assert.Equal(t, "1Gi", out[0].Requests.Memory().String(), "other resources are kept")
	assert.Equal(t, "50m", out[1].Requests.Cpu().String())
	assert.Equal(t, "500m", containers[0].Requests.Cpu().String(), "input is not modified")
}

func TestContainersFromPods(t *testing.T) {
	older := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Unix(100, 0))},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "old"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	newer := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Unix(200, 0))},
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "app"}},
			InitContainers: []corev1.Container{{Name: "istio-proxy", RestartPolicy: new(corev1.ContainerRestartPolicyAlways)}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(time.Unix(300, 0))},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "pending"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	got := ContainersFromPods([]*corev1.Pod{older, pending, newer})
	require.Len(t, got, 2)
	assert.Equal(t, "app", got[0].Name)
	assert.Equal(t, "istio-proxy", got[1].Name)
	assert.True(t, got[1].RestartableInit)

	assert.Nil(t, ContainersFromPods([]*corev1.Pod{pending}))
}
