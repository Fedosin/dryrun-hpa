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
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	scaleclient "k8s.io/client-go/scale"

	dryrunlisters "github.com/fedosin/dryrun-hpa/pkg/generated/listers/dryrunhpa/v1alpha1"
)

// scaleClient waits for a VirtualWorkload to be provisioned (exist and
// report a selector) before reading its scale, so a newly created DryrunHPA
// doesn't fail its first reconciliation while its VirtualWorkload is still
// being created.
type scaleClient struct {
	inner    scaleclient.ScalesGetter
	vwLister dryrunlisters.VirtualWorkloadLister
	wait     time.Duration
}

// NewScaleClient wraps a scale client. wait bounds how long a Get waits for
// the VirtualWorkload to be provisioned.
func NewScaleClient(inner scaleclient.ScalesGetter, vwLister dryrunlisters.VirtualWorkloadLister, wait time.Duration) scaleclient.ScalesGetter {
	return &scaleClient{inner: inner, vwLister: vwLister, wait: wait}
}

func (c *scaleClient) Scales(namespace string) scaleclient.ScaleInterface {
	return &scaleNamespaceClient{scaleClient: c, namespace: namespace, inner: c.inner.Scales(namespace)}
}

type scaleNamespaceClient struct {
	*scaleClient
	namespace string
	inner     scaleclient.ScaleInterface
}

func (c *scaleNamespaceClient) Get(ctx context.Context, resource schema.GroupResource, name string, opts metav1.GetOptions) (*autoscalingv1.Scale, error) {
	if resource == VirtualWorkloadGroupResource && c.wait > 0 {
		c.waitProvisioned(ctx, name)
	}
	return c.inner.Get(ctx, resource, name, opts)
}

func (c *scaleNamespaceClient) waitProvisioned(ctx context.Context, name string) {
	_ = wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, c.wait, true, func(context.Context) (bool, error) {
		vw, err := c.vwLister.VirtualWorkloads(c.namespace).Get(name)
		if k8serrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return vw.Status.Selector != "", nil
	})
}

func (c *scaleNamespaceClient) Update(ctx context.Context, resource schema.GroupResource, scale *autoscalingv1.Scale, opts metav1.UpdateOptions) (*autoscalingv1.Scale, error) {
	return c.inner.Update(ctx, resource, scale, opts)
}

func (c *scaleNamespaceClient) Patch(ctx context.Context, gvr schema.GroupVersionResource, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions) (*autoscalingv1.Scale, error) {
	return nil, fmt.Errorf("patching scale of %s %s/%s is not supported", gvr, c.namespace, name)
}
