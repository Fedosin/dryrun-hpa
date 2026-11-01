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
	"context"
	"fmt"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	autoscalinginformers "k8s.io/client-go/informers/autoscaling/v2"
	autoscalingclient "k8s.io/client-go/kubernetes/typed/autoscaling/v2"
	autoscalinglisters "k8s.io/client-go/listers/autoscaling/v2"
	"k8s.io/client-go/tools/cache"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	dryrunclient "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned/typed/dryrunhpa/v1alpha1"
)

// SourceErrors reports why the source workload of a DryrunHPA is unusable.
type SourceErrors interface {
	// SourceError returns a non-nil error when the source workload of the
	// DryrunHPA namespace/name is missing or unsupported.
	SourceError(namespace, name string) error
}

// hpaInformer exposes a DryrunHPA informer (with Transform installed) as an
// autoscaling/v2 HPA informer.
type hpaInformer struct {
	informer cache.SharedIndexInformer
}

// NewHPAInformer installs Transform on the DryrunHPA informer and wraps it.
// It must be called before the informer is started.
func NewHPAInformer(informer cache.SharedIndexInformer) (autoscalinginformers.HorizontalPodAutoscalerInformer, error) {
	if err := informer.SetTransform(Transform); err != nil {
		return nil, fmt.Errorf("installing DryrunHPA transform: %w", err)
	}
	return &hpaInformer{informer: informer}, nil
}

func (i *hpaInformer) Informer() cache.SharedIndexInformer {
	return i.informer
}

func (i *hpaInformer) Lister() autoscalinglisters.HorizontalPodAutoscalerLister {
	return autoscalinglisters.NewHorizontalPodAutoscalerLister(i.informer.GetIndexer())
}

// hpaClient implements the part of the autoscaling/v2 client the upstream
// loop uses (UpdateStatus) on top of the DryrunHPA client.
type hpaClient struct {
	client  dryrunclient.DryrunHorizontalPodAutoscalersGetter
	sources SourceErrors
}

// NewHPAClient returns an HPA client that writes DryrunHPA status.
func NewHPAClient(client dryrunclient.DryrunHorizontalPodAutoscalersGetter, sources SourceErrors) autoscalingclient.HorizontalPodAutoscalersGetter {
	return &hpaClient{client: client, sources: sources}
}

func (c *hpaClient) HorizontalPodAutoscalers(namespace string) autoscalingclient.HorizontalPodAutoscalerInterface {
	return &hpaNamespaceClient{hpaClient: c, namespace: namespace}
}

// hpaNamespaceClient embeds the interface so it satisfies it; only the
// methods the upstream loop calls are implemented.
type hpaNamespaceClient struct {
	autoscalingclient.HorizontalPodAutoscalerInterface
	*hpaClient
	namespace string
}

func (c *hpaNamespaceClient) UpdateStatus(ctx context.Context, hpa *autoscalingv2.HorizontalPodAutoscaler, opts metav1.UpdateOptions) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	d := ToDryrun(hpa)
	now := metav1.Now()
	d.Status.LastDecisionTime = &now
	if c.sources != nil {
		if err := c.sources.SourceError(hpa.Namespace, hpa.Name); err != nil {
			d.Status.Conditions = setCondition(d.Status.Conditions, autoscalingv2.ScalingActive, corev1.ConditionFalse,
				v1alpha1.ReasonFailedGetSourceWorkload, fmt.Sprintf("the dry-run HPA was unable to read the source workload: %v", err))
		}
	}
	updated, err := c.client.DryrunHorizontalPodAutoscalers(c.namespace).UpdateStatus(ctx, d, opts)
	if err != nil {
		return nil, err
	}
	return ToHPA(updated), nil
}

func (c *hpaNamespaceClient) Get(ctx context.Context, name string, opts metav1.GetOptions) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	d, err := c.client.DryrunHorizontalPodAutoscalers(c.namespace).Get(ctx, name, opts)
	if err != nil {
		return nil, err
	}
	return ToHPA(d), nil
}

func setCondition(conditions []autoscalingv2.HorizontalPodAutoscalerCondition, t autoscalingv2.HorizontalPodAutoscalerConditionType,
	status corev1.ConditionStatus, reason, message string) []autoscalingv2.HorizontalPodAutoscalerCondition {
	for i := range conditions {
		if conditions[i].Type != t {
			continue
		}
		if conditions[i].Status != status {
			conditions[i].LastTransitionTime = metav1.Now()
		}
		conditions[i].Status = status
		conditions[i].Reason = reason
		conditions[i].Message = message
		return conditions
	}
	return append(conditions, autoscalingv2.HorizontalPodAutoscalerCondition{
		Type: t, Status: status, Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
	})
}

// EventObject attaches events for an HPA to the DryrunHPA it was converted from.
func EventObject(hpa *autoscalingv2.HorizontalPodAutoscaler) runtime.Object {
	return &corev1.ObjectReference{
		APIVersion:      v1alpha1.SchemeGroupVersion.String(),
		Kind:            v1alpha1.KindDryrunHorizontalPodAutoscaler,
		Namespace:       hpa.Namespace,
		Name:            hpa.Name,
		UID:             hpa.UID,
		ResourceVersion: hpa.ResourceVersion,
	}
}
