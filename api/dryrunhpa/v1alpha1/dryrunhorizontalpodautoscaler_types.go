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

package v1alpha1

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// AnnotationPodReadinessDelay overrides how long a new virtual pod stays unready.
	AnnotationPodReadinessDelay = GroupName + "/pod-readiness-delay"
	// AnnotationMetricsDelay overrides how long a new virtual pod reports no metrics.
	AnnotationMetricsDelay = GroupName + "/metrics-delay"
	// AnnotationRequestOverrides overrides container requests of the virtual pods.
	// The value is a JSON object keyed by container name, for example
	// {"app":{"cpu":"200m","memory":"256Mi"}}.
	AnnotationRequestOverrides = GroupName + "/request-overrides"

	// KindDryrunHorizontalPodAutoscaler is the kind of the DryrunHorizontalPodAutoscaler resource.
	KindDryrunHorizontalPodAutoscaler = "DryrunHorizontalPodAutoscaler"
)

// DryrunHorizontalPodAutoscaler is a HorizontalPodAutoscaler that never
// touches its target. It scales a controller-managed VirtualWorkload that
// mirrors the target instead, and records every decision.
//
// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dhpa,categories=dryrun
// +kubebuilder:printcolumn:name="Reference",type=string,JSONPath=`.spec.scaleTargetRef.name`
// +kubebuilder:printcolumn:name="Targets",type=string,JSONPath=`.status.currentMetrics[*].resource.current.averageUtilization`,description="Current utilization of the first resource metric"
// +kubebuilder:printcolumn:name="MinPods",type=integer,JSONPath=`.spec.minReplicas`
// +kubebuilder:printcolumn:name="MaxPods",type=integer,JSONPath=`.spec.maxReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DryrunHorizontalPodAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is identical to the autoscaling/v2 HorizontalPodAutoscaler spec.
	//
	// +kubebuilder:validation:XValidation:rule="(self.scaleTargetRef.apiVersion + '/' + self.scaleTargetRef.kind) in ['apps/v1/Deployment']",message="scaleTargetRef must reference a supported kind: apps/v1 Deployment"
	// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must be less than or equal to maxReplicas"
	// +kubebuilder:validation:XValidation:rule="self.maxReplicas >= 1",message="maxReplicas must be greater than 0"
	// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas >= 0",message="minReplicas must not be negative"
	// +kubebuilder:validation:XValidation:rule="!has(self.metrics) || self.metrics.all(m, m.type != 'Resource' || (has(m.resource) && ((m.resource.target.type == 'Utilization' && has(m.resource.target.averageUtilization)) || (m.resource.target.type == 'AverageValue' && has(m.resource.target.averageValue)))))",message="resource metrics need a Utilization target with averageUtilization or an AverageValue target with averageValue"
	// +kubebuilder:validation:XValidation:rule="!has(self.metrics) || self.metrics.all(m, m.type != 'ContainerResource' || (has(m.containerResource) && ((m.containerResource.target.type == 'Utilization' && has(m.containerResource.target.averageUtilization)) || (m.containerResource.target.type == 'AverageValue' && has(m.containerResource.target.averageValue)))))",message="containerResource metrics need a Utilization target with averageUtilization or an AverageValue target with averageValue"
	// +kubebuilder:validation:XValidation:rule="!has(self.metrics) || self.metrics.all(m, m.type != 'Pods' || (has(m.pods) && m.pods.target.type == 'AverageValue' && has(m.pods.target.averageValue)))",message="pods metrics need an AverageValue target with averageValue"
	// +kubebuilder:validation:XValidation:rule="!has(self.metrics) || self.metrics.all(m, m.type != 'Object' || (has(m.object) && ((m.object.target.type == 'Value' && has(m.object.target.value)) || (m.object.target.type == 'AverageValue' && has(m.object.target.averageValue)))))",message="object metrics need a Value target with value or an AverageValue target with averageValue"
	// +kubebuilder:validation:XValidation:rule="!has(self.metrics) || self.metrics.all(m, m.type != 'External' || (has(m.external) && ((m.external.target.type == 'Value' && has(m.external.target.value)) || (m.external.target.type == 'AverageValue' && has(m.external.target.averageValue)))))",message="external metrics need a Value target with value or an AverageValue target with averageValue"
	// +kubebuilder:validation:XValidation:rule="!has(self.behavior) || !has(self.behavior.scaleUp) || !has(self.behavior.scaleUp.policies) || self.behavior.scaleUp.policies.all(p, p.value > 0 && p.periodSeconds > 0 && p.periodSeconds <= 1800)",message="scaleUp policies need value > 0 and 0 < periodSeconds <= 1800"
	// +kubebuilder:validation:XValidation:rule="!has(self.behavior) || !has(self.behavior.scaleDown) || !has(self.behavior.scaleDown.policies) || self.behavior.scaleDown.policies.all(p, p.value > 0 && p.periodSeconds > 0 && p.periodSeconds <= 1800)",message="scaleDown policies need value > 0 and 0 < periodSeconds <= 1800"
	Spec autoscalingv2.HorizontalPodAutoscalerSpec `json:"spec"`

	// +optional
	Status DryrunHorizontalPodAutoscalerStatus `json:"status,omitempty"`
}

// DryrunHorizontalPodAutoscalerStatus is the autoscaling/v2 status plus
// dry-run specific fields.
type DryrunHorizontalPodAutoscalerStatus struct {
	autoscalingv2.HorizontalPodAutoscalerStatus `json:",inline"`

	// VirtualWorkloadRef points at the VirtualWorkload this object scales.
	// +optional
	VirtualWorkloadRef *VirtualWorkloadReference `json:"virtualWorkloadRef,omitempty"`

	// ObservedSpecHash identifies the configuration (spec plus simulation
	// annotations) that produced the current status. Decisions in the
	// decision store carry the same hash.
	// +optional
	ObservedSpecHash string `json:"observedSpecHash,omitempty"`

	// LastDecisionTime is the time of the last reconciliation that changed the status.
	// +optional
	LastDecisionTime *metav1.Time `json:"lastDecisionTime,omitempty"`
}

// VirtualWorkloadReference identifies a VirtualWorkload in the same namespace.
type VirtualWorkloadReference struct {
	Name string `json:"name"`
}

// DryrunHorizontalPodAutoscalerList is a list of DryrunHorizontalPodAutoscaler.
//
// +kubebuilder:object:root=true
type DryrunHorizontalPodAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DryrunHorizontalPodAutoscaler `json:"items"`
}
