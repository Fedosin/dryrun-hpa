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

package app

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/client-go/tools/cache"

	"github.com/fedosin/dryrun-hpa/internal/adapter"
	"github.com/fedosin/dryrun-hpa/internal/virtual/source"
)

const indexHPAByTarget = "byTarget"

func hpaTargetIndex(obj interface{}) ([]string, error) {
	hpa, ok := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if !ok {
		return nil, nil
	}
	gk, err := adapter.TargetGroupKind(hpa.Spec.ScaleTargetRef)
	if err != nil {
		return nil, nil
	}
	return []string{source.Key(gk, hpa.Namespace, hpa.Spec.ScaleTargetRef.Name)}, nil
}

// realState implements recorder.RealState from informer caches.
type realState struct {
	sources *source.Registry
	// realHPAs indexes real HPAs by target; nil when comparison is off.
	realHPAs cache.Indexer
}

func (r *realState) SourceReplicas(namespace string, target autoscalingv2.CrossVersionObjectReference) *int32 {
	gk, err := adapter.TargetGroupKind(target)
	if err != nil {
		return nil
	}
	src, err := r.sources.Get(gk)
	if err != nil {
		return nil
	}
	info, err := src.Get(namespace, target.Name)
	if err != nil {
		return nil
	}
	return &info.Replicas
}

func (r *realState) RealHPADesired(namespace string, target autoscalingv2.CrossVersionObjectReference) *int32 {
	if r.realHPAs == nil {
		return nil
	}
	gk, err := adapter.TargetGroupKind(target)
	if err != nil {
		return nil
	}
	objs, err := r.realHPAs.ByIndex(indexHPAByTarget, source.Key(gk, namespace, target.Name))
	if err != nil || len(objs) == 0 {
		return nil
	}
	hpa := objs[0].(*autoscalingv2.HorizontalPodAutoscaler)
	desired := hpa.Status.DesiredReplicas
	return &desired
}
