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

package source

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	appsinformers "k8s.io/client-go/informers/apps/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

// DeploymentGroupKind is apps Deployment.
var DeploymentGroupKind = schema.GroupKind{Group: appsv1.GroupName, Kind: "Deployment"}

type deploymentAdapter struct {
	informer cache.SharedIndexInformer
	lister   appslisters.DeploymentLister
}

// NewDeploymentAdapter reads Deployments from an informer.
func NewDeploymentAdapter(informer appsinformers.DeploymentInformer) Adapter {
	return &deploymentAdapter{informer: informer.Informer(), lister: informer.Lister()}
}

func (a *deploymentAdapter) GroupKind() schema.GroupKind {
	return DeploymentGroupKind
}

func (a *deploymentAdapter) Informer() cache.SharedIndexInformer {
	return a.informer
}

func (a *deploymentAdapter) Get(namespace, name string) (*Info, error) {
	d, err := a.lister.Deployments(namespace).Get(name)
	if err != nil {
		return nil, err
	}
	return DeploymentInfo(d)
}

// DeploymentInfo extracts the source information of a Deployment.
func DeploymentInfo(d *appsv1.Deployment) (*Info, error) {
	selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector of Deployment %s/%s: %w", d.Namespace, d.Name, err)
	}
	replicas := int32(1)
	if d.Spec.Replicas != nil {
		replicas = *d.Spec.Replicas
	}
	return &Info{
		Replicas:         replicas,
		Selector:         selector,
		Containers:       ContainersFromPodSpec(&d.Spec.Template.Spec),
		LifecycleProfile: v1alpha1.LifecycleProfileParallel,
	}, nil
}
