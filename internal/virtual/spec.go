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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/virtual/source"
)

// Simulation holds the simulation settings parsed from DryrunHPA annotations.
type Simulation struct {
	PodReadinessDelay *metav1.Duration
	MetricsDelay      *metav1.Duration
	RequestOverrides  map[string]corev1.ResourceList
}

// ParseSimulation parses the simulation annotations. Invalid annotations are
// ignored and reported in the returned error.
func ParseSimulation(annotations map[string]string) (Simulation, error) {
	var sim Simulation
	var errs []error
	parseDuration := func(key string) *metav1.Duration {
		raw, ok := annotations[key]
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("annotation %s: invalid duration %q", key, raw))
			return nil
		}
		return &metav1.Duration{Duration: d}
	}
	sim.PodReadinessDelay = parseDuration(v1alpha1.AnnotationPodReadinessDelay)
	sim.MetricsDelay = parseDuration(v1alpha1.AnnotationMetricsDelay)
	if raw, ok := annotations[v1alpha1.AnnotationRequestOverrides]; ok {
		var overrides map[string]corev1.ResourceList
		if err := json.Unmarshal([]byte(raw), &overrides); err != nil {
			errs = append(errs, fmt.Errorf("annotation %s: %w", v1alpha1.AnnotationRequestOverrides, err))
		} else {
			sim.RequestOverrides = overrides
		}
	}
	return sim, errors.Join(errs...)
}

// ContainersFromPods returns the containers of the newest running pod, so
// requests set by admission (LimitRanges, webhooks, injected sidecars) match
// what the HPA sees on real pods. It returns nil when no pod is running.
func ContainersFromPods(pods []*corev1.Pod) []v1alpha1.VirtualContainer {
	var newest *corev1.Pod
	for _, p := range pods {
		if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
			continue
		}
		if newest == nil || newest.CreationTimestamp.Before(&p.CreationTimestamp) {
			newest = p
		}
	}
	if newest == nil {
		return nil
	}
	return source.ContainersFromPodSpec(&newest.Spec)
}

// ApplyRequestOverrides replaces requests per container.
func ApplyRequestOverrides(containers []v1alpha1.VirtualContainer, overrides map[string]corev1.ResourceList) ([]v1alpha1.VirtualContainer, error) {
	if len(overrides) == 0 {
		return containers, nil
	}
	out := make([]v1alpha1.VirtualContainer, len(containers))
	seen := map[string]bool{}
	for i, c := range containers {
		out[i] = *c.DeepCopy()
		if o, ok := overrides[c.Name]; ok {
			seen[c.Name] = true
			if out[i].Requests == nil {
				out[i].Requests = corev1.ResourceList{}
			}
			maps.Copy(out[i].Requests, o)
		}
	}
	var errs []error
	for name := range overrides {
		if !seen[name] {
			errs = append(errs, fmt.Errorf("annotation %s: container %q not found", v1alpha1.AnnotationRequestOverrides, name))
		}
	}
	return out, errors.Join(errs...)
}
