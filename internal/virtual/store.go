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

// Package virtual simulates the pods of VirtualWorkloads and projects the
// real load of their source workloads onto them.
package virtual

import (
	"fmt"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/virtual/lifecycle"
)

// MatureAge is the age given to the virtual pods a VirtualWorkload starts
// with (or gets back after a restart). It is longer than any CPU
// initialization period, so those pods count as long-running.
const MatureAge = time.Hour

// Defaults are used when a VirtualWorkload doesn't set simulation values.
type Defaults struct {
	PodReadinessDelay time.Duration
	MetricsDelay      time.Duration
}

type podObjects struct {
	unready *corev1.Pod
	ready   *corev1.Pod
}

type workload struct {
	uid            types.UID
	namespace      string
	name           string
	sourceSelector labels.Selector
	containers     []v1alpha1.VirtualContainer
	readinessDelay time.Duration
	metricsDelay   time.Duration
	profile        lifecycle.Profile
	pods           []lifecycle.Pod
	nextOrdinal    int64
	objects        map[int64]podObjects
}

// View is a read-only snapshot of a virtual workload.
type View struct {
	UID            types.UID
	Namespace      string
	Name           string
	SourceSelector labels.Selector
	MetricsDelay   time.Duration
	Pods           []lifecycle.Pod
}

// PodName returns the name of a virtual pod.
func (v *View) PodName(p lifecycle.Pod) string {
	return podName(v.Name, p.Ordinal)
}

func podName(workloadName string, ordinal int64) string {
	return fmt.Sprintf("%s-v%d", workloadName, ordinal)
}

// Store holds the virtual pods of all VirtualWorkloads in memory.
type Store struct {
	defaults Defaults
	now      func() time.Time

	mu    sync.RWMutex
	byUID map[types.UID]*workload
}

// NewStore returns an empty store.
func NewStore(defaults Defaults) *Store {
	return &Store{defaults: defaults, now: time.Now, byUID: map[types.UID]*workload{}}
}

// Sync reconciles the virtual pods of vw with vw.spec.replicas.
// sourceSelector selects the real pods; nil keeps the previously known one.
func (s *Store) Sync(vw *v1alpha1.VirtualWorkload, sourceSelector labels.Selector) error {
	profile, err := lifecycle.Get(vw.Spec.LifecycleProfile)
	if err != nil {
		return err
	}
	now := s.now()
	readiness := durationOr(vw.Spec.Simulation.PodReadinessDelay, s.defaults.PodReadinessDelay)
	metricsDelay := durationOr(vw.Spec.Simulation.MetricsDelay, s.defaults.MetricsDelay)

	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.byUID[vw.UID]
	if !ok {
		w = &workload{uid: vw.UID, namespace: vw.Namespace, name: vw.Name, objects: map[int64]podObjects{}}
		created := now.Add(-MatureAge)
		for i := range int(vw.Spec.Replicas) {
			w.pods = append(w.pods, lifecycle.Pod{Ordinal: int64(i), Created: created, ReadyAt: created.Add(readiness)})
		}
		w.nextOrdinal = int64(vw.Spec.Replicas)
		s.byUID[vw.UID] = w
	}
	if sourceSelector != nil {
		w.sourceSelector = sourceSelector
	}
	if !apiequality.Semantic.DeepEqual(w.containers, vw.Spec.Containers) {
		w.containers = slices.Clone(vw.Spec.Containers)
		clear(w.objects)
	}
	w.readinessDelay = readiness
	w.metricsDelay = metricsDelay
	w.profile = profile

	switch desired := int(vw.Spec.Replicas); {
	case desired > len(w.pods):
		added := profile.ScaleUp(w.pods, desired-len(w.pods), w.nextOrdinal, now, readiness)
		w.pods = append(w.pods, added...)
		w.nextOrdinal += int64(len(added))
	case desired < len(w.pods):
		w.pods = profile.ScaleDown(w.pods, len(w.pods)-desired, now)
		live := map[int64]bool{}
		for _, p := range w.pods {
			live[p.Ordinal] = true
		}
		for o := range w.objects {
			if !live[o] {
				delete(w.objects, o)
			}
		}
	}
	return nil
}

// Delete forgets a VirtualWorkload.
func (s *Store) Delete(uid types.UID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byUID, uid)
}

// Get returns a snapshot of a virtual workload.
func (s *Store) Get(uid types.UID) (*View, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.byUID[uid]
	if !ok {
		return nil, false
	}
	return &View{
		UID:            w.uid,
		Namespace:      w.namespace,
		Name:           w.name,
		SourceSelector: w.sourceSelector,
		MetricsDelay:   w.metricsDelay,
		Pods:           slices.Clone(w.pods),
	}, true
}

// Counts returns the number of virtual pods, how many are ready, and how
// long until the next pod becomes ready (0 if none is pending).
func (s *Store) Counts(uid types.UID) (replicas, ready int32, nextReady time.Duration) {
	now := s.now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.byUID[uid]
	if !ok {
		return 0, 0, 0
	}
	for _, p := range w.pods {
		replicas++
		if p.Ready(now) {
			ready++
			continue
		}
		if d := p.ReadyAt.Sub(now); nextReady == 0 || d < nextReady {
			nextReady = d
		}
	}
	return replicas, ready, nextReady
}

// Len returns the number of virtual workloads and pods.
func (s *Store) Len() (workloads, pods int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, w := range s.byUID {
		pods += len(w.pods)
	}
	return len(s.byUID), pods
}

// VirtualPods implements adapter.VirtualPods.
func (s *Store) VirtualPods(namespace string, uid types.UID) []*corev1.Pod {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.byUID[uid]
	if !ok || w.namespace != namespace {
		return nil
	}
	out := make([]*corev1.Pod, 0, len(w.pods))
	for _, p := range w.pods {
		objs, ok := w.objects[p.Ordinal]
		if !ok {
			objs = buildPodObjects(w, p)
			w.objects[p.Ordinal] = objs
		}
		if p.Ready(now) {
			out = append(out, objs.ready)
		} else {
			out = append(out, objs.unready)
		}
	}
	return out
}

func buildPodObjects(w *workload, p lifecycle.Pod) podObjects {
	created := metav1.NewTime(p.Created)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              podName(w.name, p.Ordinal),
			Namespace:         w.namespace,
			UID:               types.UID(fmt.Sprintf("%s-%d", w.uid, p.Ordinal)),
			Labels:            map[string]string{v1alpha1.LabelVirtualWorkload: string(w.uid)},
			CreationTimestamp: created,
		},
		Status: corev1.PodStatus{
			Phase:     corev1.PodRunning,
			StartTime: &created,
		},
	}
	for _, c := range w.containers {
		container := corev1.Container{Name: c.Name, Resources: corev1.ResourceRequirements{Requests: c.Requests}}
		if c.RestartableInit {
			container.RestartPolicy = ptr.To(corev1.ContainerRestartPolicyAlways)
			pod.Spec.InitContainers = append(pod.Spec.InitContainers, container)
			continue
		}
		pod.Spec.Containers = append(pod.Spec.Containers, container)
	}
	unready := pod.DeepCopy()
	unready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: created}}
	ready := pod
	ready.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(p.ReadyAt)}}
	return podObjects{unready: unready, ready: ready}
}

func durationOr(d *metav1.Duration, def time.Duration) time.Duration {
	if d == nil {
		return def
	}
	return d.Duration
}
