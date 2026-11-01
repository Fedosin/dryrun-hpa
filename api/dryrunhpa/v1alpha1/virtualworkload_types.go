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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// LabelVirtualWorkload is set on virtual pods to the UID of their VirtualWorkload.
	// It is the only label in VirtualWorkload.status.selector.
	LabelVirtualWorkload = GroupName + "/vw"

	// KindVirtualWorkload is the kind of the VirtualWorkload resource.
	KindVirtualWorkload = "VirtualWorkload"

	// ConditionSourceAvailable reports whether the source workload could be read.
	ConditionSourceAvailable = "SourceAvailable"
	// ReasonFailedGetSourceWorkload is used when the source workload is missing or unreadable.
	ReasonFailedGetSourceWorkload = "FailedGetSourceWorkload"
)

// LifecycleProfileType selects how virtual pods appear and disappear.
//
// +kubebuilder:validation:Enum=Parallel;OrderedReady
type LifecycleProfileType string

const (
	// LifecycleProfileParallel starts and removes pods like a ReplicaSet does:
	// all at once, removing unready and then newest pods first.
	LifecycleProfileParallel LifecycleProfileType = "Parallel"
	// LifecycleProfileOrderedReady is reserved for StatefulSets with
	// podManagementPolicy OrderedReady.
	LifecycleProfileOrderedReady LifecycleProfileType = "OrderedReady"
)

// VirtualWorkload mirrors a real workload. The dry-run HPA loop scales it
// through its scale subresource, and its virtual pods receive the real
// workload's load. It is managed by the controller; do not create it by hand.
//
// +genclient
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:shortName=vwl,categories=dryrun
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceRef.name`
// +kubebuilder:printcolumn:name="Virtual",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Real",type=integer,JSONPath=`.status.sourceReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type VirtualWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec VirtualWorkloadSpec `json:"spec"`

	// +optional
	Status VirtualWorkloadStatus `json:"status,omitempty"`
}

// VirtualWorkloadSpec describes the simulated workload.
type VirtualWorkloadSpec struct {
	// Replicas is the virtual replica count, written by the dry-run HPA loop.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// SourceRef is the real workload this object mirrors.
	SourceRef SourceReference `json:"sourceRef"`

	// LifecycleProfile selects how virtual pods start and stop.
	// +kubebuilder:default=Parallel
	// +optional
	LifecycleProfile LifecycleProfileType `json:"lifecycleProfile,omitempty"`

	// Containers are the containers of every virtual pod, with the requests
	// used to compute utilization.
	// +listType=map
	// +listMapKey=name
	// +optional
	Containers []VirtualContainer `json:"containers,omitempty"`

	// Simulation tunes the virtual pod lifecycle.
	// +optional
	Simulation SimulationSpec `json:"simulation,omitempty"`
}

// SourceReference is a full reference to a workload in the same namespace.
type SourceReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// VirtualContainer is a container of a virtual pod.
type VirtualContainer struct {
	Name string `json:"name"`
	// Requests are the resource requests of the container.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`
	// RestartableInit marks a sidecar (restartable init container). The HPA
	// includes its requests in pod-level utilization.
	// +optional
	RestartableInit bool `json:"restartableInit,omitempty"`
}

// SimulationSpec tunes the virtual pod lifecycle.
type SimulationSpec struct {
	// PodReadinessDelay is how long a new virtual pod stays unready.
	// +optional
	PodReadinessDelay *metav1.Duration `json:"podReadinessDelay,omitempty"`
	// MetricsDelay is how long a new virtual pod reports no metrics.
	// +optional
	MetricsDelay *metav1.Duration `json:"metricsDelay,omitempty"`
}

// VirtualWorkloadStatus is the observed state of the simulated workload.
type VirtualWorkloadStatus struct {
	// Replicas is the number of virtual pods.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// ReadyReplicas is the number of ready virtual pods.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Selector selects the virtual pods. It is used by the scale subresource.
	// +optional
	Selector string `json:"selector,omitempty"`
	// SourceSelector selects the pods of the real workload.
	// +optional
	SourceSelector string `json:"sourceSelector,omitempty"`
	// SourceReplicas is the replica count of the real workload.
	// +optional
	SourceReplicas int32 `json:"sourceReplicas,omitempty"`
	// SourceUsage is the total usage of the real workload, per container.
	// +listType=map
	// +listMapKey=name
	// +optional
	SourceUsage []ContainerUsage `json:"sourceUsage,omitempty"`
	// LastSyncTime is when the status was last written.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// ObservedGeneration is the generation the status was computed for.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ContainerUsage is the summed usage of one container across the real pods.
type ContainerUsage struct {
	Name string `json:"name"`
	// Usage is the summed usage.
	// +optional
	Usage corev1.ResourceList `json:"usage,omitempty"`
	// Pods is the number of real pods that contributed to the sum.
	// +optional
	Pods int32 `json:"pods,omitempty"`
}

// VirtualWorkloadList is a list of VirtualWorkload.
//
// +kubebuilder:object:root=true
type VirtualWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualWorkload `json:"items"`
}
