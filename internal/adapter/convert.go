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

// Package adapter lets the copied upstream HPA controller run against
// DryrunHorizontalPodAutoscalers and VirtualWorkloads without changes to its
// scaling logic.
package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	autoscalingapiv2 "github.com/fedosin/dryrun-hpa/internal/upstream/apis/autoscaling/v2"
)

const (
	// AnnotationScaleTarget holds the original scaleTargetRef (JSON) on the
	// in-memory HPA objects. It is never written to the API server.
	AnnotationScaleTarget = "internal." + v1alpha1.GroupName + "/scale-target"
	// AnnotationSpecHash holds the configuration hash on the in-memory HPA objects.
	AnnotationSpecHash = "internal." + v1alpha1.GroupName + "/spec-hash"

	virtualWorkloadNamePrefix = "dhpa-"
)

// simulationAnnotations are part of a configuration and therefore of its hash.
var simulationAnnotations = []string{
	v1alpha1.AnnotationPodReadinessDelay,
	v1alpha1.AnnotationMetricsDelay,
	v1alpha1.AnnotationRequestOverrides,
}

// VirtualWorkloadGroupResource is the resource the HPA loop scales.
var VirtualWorkloadGroupResource = v1alpha1.Resource("virtualworkloads")

// VirtualWorkloadName is the name of the VirtualWorkload owned by a DryrunHPA.
func VirtualWorkloadName(dhpaName string) string {
	return virtualWorkloadNamePrefix + dhpaName
}

// DryrunHPANameForVirtualWorkload is the inverse of VirtualWorkloadName.
func DryrunHPANameForVirtualWorkload(vwName string) (string, bool) {
	if len(vwName) <= len(virtualWorkloadNamePrefix) || vwName[:len(virtualWorkloadNamePrefix)] != virtualWorkloadNamePrefix {
		return "", false
	}
	return vwName[len(virtualWorkloadNamePrefix):], true
}

// ToHPA converts a DryrunHPA into the autoscaling/v2 object the upstream
// loop works on. It applies the API server defaults of autoscaling/v2,
// redirects scaleTargetRef to the VirtualWorkload and records the original
// target and the configuration hash in internal annotations.
func ToHPA(d *v1alpha1.DryrunHorizontalPodAutoscaler) *autoscalingv2.HorizontalPodAutoscaler {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: d.ObjectMeta,
		Spec:       *d.Spec.DeepCopy(),
		Status:     d.Status.HorizontalPodAutoscalerStatus,
	}
	autoscalingapiv2.SetDefaults_HorizontalPodAutoscaler(hpa)

	target, _ := json.Marshal(hpa.Spec.ScaleTargetRef)
	annotations := make(map[string]string, len(d.Annotations)+2)
	maps.Copy(annotations, d.Annotations)
	annotations[AnnotationScaleTarget] = string(target)
	annotations[AnnotationSpecHash] = SpecHash(&hpa.Spec, d.Annotations)
	hpa.Annotations = annotations

	hpa.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{
		APIVersion: v1alpha1.SchemeGroupVersion.String(),
		Kind:       v1alpha1.KindVirtualWorkload,
		Name:       VirtualWorkloadName(d.Name),
	}
	return hpa
}

// Transform is an informer transform that stores DryrunHPAs as HPAs.
func Transform(obj interface{}) (interface{}, error) {
	if d, ok := obj.(*v1alpha1.DryrunHorizontalPodAutoscaler); ok {
		return ToHPA(d), nil
	}
	return obj, nil
}

// OriginalTarget returns the scaleTargetRef of the DryrunHPA an HPA was converted from.
func OriginalTarget(hpa *autoscalingv2.HorizontalPodAutoscaler) (autoscalingv2.CrossVersionObjectReference, error) {
	var ref autoscalingv2.CrossVersionObjectReference
	raw, ok := hpa.Annotations[AnnotationScaleTarget]
	if !ok {
		return ref, fmt.Errorf("%s/%s was not converted from a DryrunHorizontalPodAutoscaler", hpa.Namespace, hpa.Name)
	}
	err := json.Unmarshal([]byte(raw), &ref)
	return ref, err
}

// TargetGroupKind returns the group and kind of a scale target reference.
func TargetGroupKind(ref autoscalingv2.CrossVersionObjectReference) (schema.GroupKind, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupKind{}, err
	}
	return schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, nil
}

// EffectiveSpec returns the defaulted spec with the original scale target.
func EffectiveSpec(hpa *autoscalingv2.HorizontalPodAutoscaler) autoscalingv2.HorizontalPodAutoscalerSpec {
	spec := *hpa.Spec.DeepCopy()
	if ref, err := OriginalTarget(hpa); err == nil {
		spec.ScaleTargetRef = ref
	}
	return spec
}

// SimulationAnnotations returns the annotations that are part of a configuration.
func SimulationAnnotations(annotations map[string]string) map[string]string {
	out := map[string]string{}
	for _, k := range simulationAnnotations {
		if v, ok := annotations[k]; ok {
			out[k] = v
		}
	}
	return out
}

// SpecHash identifies a configuration: the defaulted spec (with the original
// target) plus the simulation annotations.
func SpecHash(spec *autoscalingv2.HorizontalPodAutoscalerSpec, annotations map[string]string) string {
	sim := SimulationAnnotations(annotations)
	keys := slices.Sorted(maps.Keys(sim))
	h := sha256.New()
	specJSON, _ := json.Marshal(spec)
	h.Write(specJSON)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, sim[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ToDryrun converts the HPA the upstream loop wrote back into a DryrunHPA
// for a status update.
func ToDryrun(hpa *autoscalingv2.HorizontalPodAutoscaler) *v1alpha1.DryrunHorizontalPodAutoscaler {
	d := &v1alpha1.DryrunHorizontalPodAutoscaler{
		ObjectMeta: *hpa.ObjectMeta.DeepCopy(),
		Spec:       EffectiveSpec(hpa),
	}
	d.Status.HorizontalPodAutoscalerStatus = *hpa.Status.DeepCopy()
	d.Status.ObservedSpecHash = hpa.Annotations[AnnotationSpecHash]
	delete(d.Annotations, AnnotationScaleTarget)
	delete(d.Annotations, AnnotationSpecHash)
	if len(d.Annotations) == 0 {
		d.Annotations = nil
	}
	d.Status.VirtualWorkloadRef = &v1alpha1.VirtualWorkloadReference{Name: VirtualWorkloadName(hpa.Name)}
	return d
}
