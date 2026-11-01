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

// Package v2 contains the API server defaulting for autoscaling/v2
// HorizontalPodAutoscalers, copied from k8s.io/kubernetes/pkg/apis/autoscaling/v2.
package v2

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/runtime"
)

// RegisterDefaults registers SetObjectDefaults_HorizontalPodAutoscaler, the
// part of the generated upstream defaulters that this project needs.
func RegisterDefaults(scheme *runtime.Scheme) error {
	scheme.AddTypeDefaultingFunc(&autoscalingv2.HorizontalPodAutoscaler{}, func(obj interface{}) {
		SetObjectDefaults_HorizontalPodAutoscaler(obj.(*autoscalingv2.HorizontalPodAutoscaler))
	})
	return nil
}

// SetObjectDefaults_HorizontalPodAutoscaler applies the same defaults the API
// server applies to an autoscaling/v2 HorizontalPodAutoscaler.
func SetObjectDefaults_HorizontalPodAutoscaler(in *autoscalingv2.HorizontalPodAutoscaler) {
	SetDefaults_HorizontalPodAutoscaler(in)
}
