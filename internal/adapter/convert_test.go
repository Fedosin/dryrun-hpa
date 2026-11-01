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

package adapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

func newDryrunHPA() *v1alpha1.DryrunHorizontalPodAutoscaler {
	return &v1alpha1.DryrunHorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod", UID: "uid-1", Annotations: map[string]string{"team": "a"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "web"},
			MinReplicas:    ptr.To[int32](2),
			MaxReplicas:    10,
		},
	}
}

func TestToHPARewritesTarget(t *testing.T) {
	d := newDryrunHPA()
	hpa := ToHPA(d)

	assert.Equal(t, autoscalingv2.CrossVersionObjectReference{
		APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: v1alpha1.KindVirtualWorkload, Name: "dhpa-web",
	}, hpa.Spec.ScaleTargetRef)
	ref, err := OriginalTarget(hpa)
	require.NoError(t, err)
	assert.Equal(t, d.Spec.ScaleTargetRef, ref)
	assert.Equal(t, "a", hpa.Annotations["team"])
	assert.Len(t, hpa.Annotations[AnnotationSpecHash], 16)
	// The source object is not modified.
	assert.Equal(t, "Deployment", d.Spec.ScaleTargetRef.Kind)
	assert.NotContains(t, d.Annotations, AnnotationScaleTarget)
}

func TestToHPAAppliesDefaults(t *testing.T) {
	hpa := ToHPA(newDryrunHPA())
	require.Len(t, hpa.Spec.Metrics, 1, "default CPU metric")
	assert.Equal(t, autoscalingv2.ResourceMetricSourceType, hpa.Spec.Metrics[0].Type)
	assert.Nil(t, hpa.Spec.Behavior, "like the API server, an unset behavior stays unset")

	d := newDryrunHPA()
	d.Spec.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{
		ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: ptr.To[int32](60)},
	}
	hpa = ToHPA(d)
	require.NotNil(t, hpa.Spec.Behavior.ScaleUp)
	require.NotNil(t, hpa.Spec.Behavior.ScaleDown)
	assert.NotNil(t, hpa.Spec.Behavior.ScaleDown.SelectPolicy)
	assert.NotEmpty(t, hpa.Spec.Behavior.ScaleDown.Policies)
	assert.Equal(t, int32(60), *hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds)
}

func TestSpecHash(t *testing.T) {
	base := ToHPA(newDryrunHPA()).Annotations[AnnotationSpecHash]
	assert.Equal(t, base, ToHPA(newDryrunHPA()).Annotations[AnnotationSpecHash], "stable")

	d := newDryrunHPA()
	d.Annotations["unrelated"] = "x"
	assert.Equal(t, base, ToHPA(d).Annotations[AnnotationSpecHash], "unrelated annotations don't matter")

	d = newDryrunHPA()
	d.Spec.MaxReplicas = 20
	assert.NotEqual(t, base, ToHPA(d).Annotations[AnnotationSpecHash], "spec changes matter")

	d = newDryrunHPA()
	d.Annotations[v1alpha1.AnnotationMetricsDelay] = "1m"
	assert.NotEqual(t, base, ToHPA(d).Annotations[AnnotationSpecHash], "simulation annotations matter")
}

func TestToDryrunRoundTrip(t *testing.T) {
	hpa := ToHPA(newDryrunHPA())
	hpa.Status.DesiredReplicas = 4

	d := ToDryrun(hpa)
	assert.Equal(t, "Deployment", d.Spec.ScaleTargetRef.Kind)
	assert.Equal(t, map[string]string{"team": "a"}, d.Annotations)
	assert.Equal(t, int32(4), d.Status.DesiredReplicas)
	assert.Equal(t, hpa.Annotations[AnnotationSpecHash], d.Status.ObservedSpecHash)
	require.NotNil(t, d.Status.VirtualWorkloadRef)
	assert.Equal(t, "dhpa-web", d.Status.VirtualWorkloadRef.Name)
}

func TestTransformPassesOtherObjects(t *testing.T) {
	obj, err := Transform("tombstone")
	require.NoError(t, err)
	assert.Equal(t, "tombstone", obj)
}

func TestVirtualWorkloadNames(t *testing.T) {
	name, ok := DryrunHPANameForVirtualWorkload(VirtualWorkloadName("web"))
	assert.True(t, ok)
	assert.Equal(t, "web", name)
	_, ok = DryrunHPANameForVirtualWorkload("web")
	assert.False(t, ok)
}

func TestVirtualWorkloadUID(t *testing.T) {
	uid, ok := VirtualWorkloadUID(labels.SelectorFromSet(labels.Set{v1alpha1.LabelVirtualWorkload: "abc"}))
	assert.True(t, ok)
	assert.EqualValues(t, "abc", uid)
	_, ok = VirtualWorkloadUID(labels.SelectorFromSet(labels.Set{"app": "web"}))
	assert.False(t, ok)
}
