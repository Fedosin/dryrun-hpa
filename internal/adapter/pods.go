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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

// VirtualPods lists the virtual pods of a VirtualWorkload.
type VirtualPods interface {
	// VirtualPods returns the pods of the VirtualWorkload with the given UID.
	// The returned pods must not be modified.
	VirtualPods(namespace string, uid types.UID) []*corev1.Pod
}

// VirtualWorkloadUID returns the VirtualWorkload UID a selector selects, if any.
func VirtualWorkloadUID(selector labels.Selector) (types.UID, bool) {
	if selector == nil {
		return "", false
	}
	v, ok := selector.RequiresExactMatch(v1alpha1.LabelVirtualWorkload)
	return types.UID(v), ok
}

// podInformer serves virtual pods for VirtualWorkload selectors and real pods
// for everything else. Informer() is the real pod informer, used by the
// upstream loop only to wait for its cache to sync.
type podInformer struct {
	real    coreinformers.PodInformer
	virtual VirtualPods
}

// NewPodInformer wraps the real pod informer.
func NewPodInformer(real coreinformers.PodInformer, virtual VirtualPods) coreinformers.PodInformer {
	return &podInformer{real: real, virtual: virtual}
}

func (i *podInformer) Informer() cache.SharedIndexInformer {
	return i.real.Informer()
}

func (i *podInformer) Lister() corelisters.PodLister {
	return &podLister{real: i.real.Lister(), virtual: i.virtual}
}

type podLister struct {
	real    corelisters.PodLister
	virtual VirtualPods
}

func (l *podLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return l.real.List(selector)
}

func (l *podLister) Pods(namespace string) corelisters.PodNamespaceLister {
	return &podNamespaceLister{podLister: l, namespace: namespace}
}

type podNamespaceLister struct {
	*podLister
	namespace string
}

func (l *podNamespaceLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	if uid, ok := VirtualWorkloadUID(selector); ok {
		var out []*corev1.Pod
		for _, p := range l.virtual.VirtualPods(l.namespace, uid) {
			if selector.Matches(labels.Set(p.Labels)) {
				out = append(out, p)
			}
		}
		return out, nil
	}
	return l.real.Pods(l.namespace).List(selector)
}

func (l *podNamespaceLister) Get(name string) (*corev1.Pod, error) {
	return l.real.Pods(l.namespace).Get(name)
}
