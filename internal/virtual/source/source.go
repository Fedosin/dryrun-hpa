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

// Package source reads the real workloads that VirtualWorkloads mirror.
// Each supported kind has an Adapter; nothing outside this package knows
// about specific workload kinds.
package source

import (
	"fmt"
	"slices"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

// Info is what a VirtualWorkload needs to know about its source.
type Info struct {
	// Replicas is the desired replica count of the source (spec.replicas).
	Replicas int32
	// Selector selects the pods of the source.
	Selector labels.Selector
	// Containers are the containers of the source's pods, with their requests.
	Containers []v1alpha1.VirtualContainer
	// LifecycleProfile is how the source starts and stops pods.
	LifecycleProfile v1alpha1.LifecycleProfileType
}

// Adapter reads one kind of workload.
type Adapter interface {
	// GroupKind is the kind this adapter reads.
	GroupKind() schema.GroupKind
	// Get returns the source namespace/name from the informer cache. It
	// returns a NotFound API error when the source doesn't exist.
	Get(namespace, name string) (*Info, error)
	// Informer delivers changes of sources of this kind.
	Informer() cache.SharedIndexInformer
}

// Registry holds the adapters of all supported kinds.
type Registry struct {
	mu       sync.RWMutex
	adapters map[schema.GroupKind]Adapter
}

// NewRegistry returns a registry with the given adapters.
func NewRegistry(adapters ...Adapter) *Registry {
	r := &Registry{adapters: map[schema.GroupKind]Adapter{}}
	for _, a := range adapters {
		r.Register(a)
	}
	return r
}

// Register adds an adapter.
func (r *Registry) Register(a Adapter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adapters[a.GroupKind()] = a
}

// Get returns the adapter for a kind.
func (r *Registry) Get(gk schema.GroupKind) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[gk]
	if !ok {
		return nil, fmt.Errorf("unsupported source kind %s", gk)
	}
	return a, nil
}

// All returns all adapters.
func (r *Registry) All() []Adapter {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(r.adapters))
	for _, a := range r.adapters {
		out = append(out, a)
	}
	return out
}

// Key identifies a source workload in informer indexes.
func Key(gk schema.GroupKind, namespace, name string) string {
	return gk.String() + "/" + namespace + "/" + name
}

// AddEventHandler calls handler with the Key of every source that is added,
// deleted or whose spec changed (status-only updates are skipped), for all
// registered kinds.
func (r *Registry) AddEventHandler(handler func(key string)) error {
	for _, a := range r.All() {
		gk := a.GroupKind()
		notify := func(obj interface{}) {
			if t, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			m, err := meta.Accessor(obj)
			if err != nil {
				return
			}
			handler(Key(gk, m.GetNamespace(), m.GetName()))
		}
		if _, err := a.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: notify,
			UpdateFunc: func(old, cur interface{}) {
				om, err1 := meta.Accessor(old)
				cm, err2 := meta.Accessor(cur)
				if err1 == nil && err2 == nil && om.GetGeneration() == cm.GetGeneration() {
					return
				}
				notify(cur)
			},
			DeleteFunc: notify,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ContainersFromPodSpec returns the containers of a pod template that count
// for the HPA: regular containers and restartable init containers (sidecars).
func ContainersFromPodSpec(spec *corev1.PodSpec) []v1alpha1.VirtualContainer {
	out := make([]v1alpha1.VirtualContainer, 0, len(spec.Containers))
	for _, c := range spec.Containers {
		out = append(out, v1alpha1.VirtualContainer{Name: c.Name, Requests: c.Resources.Requests.DeepCopy()})
	}
	for _, c := range spec.InitContainers {
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			out = append(out, v1alpha1.VirtualContainer{Name: c.Name, Requests: c.Resources.Requests.DeepCopy(), RestartableInit: true})
		}
	}
	slices.SortStableFunc(out, func(a, b v1alpha1.VirtualContainer) int {
		if a.RestartableInit != b.RestartableInit {
			if a.RestartableInit {
				return 1
			}
			return -1
		}
		return 0
	})
	return out
}
