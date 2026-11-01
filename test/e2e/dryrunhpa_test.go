//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

func TestValidationRejectsInvalidSpecs(t *testing.T) {
	ns := namespace(t)
	ctx := ctxFor(t, time.Minute)
	cases := map[string]func(*autoscalingv2.HorizontalPodAutoscalerSpec){
		"StatefulSet target": func(s *autoscalingv2.HorizontalPodAutoscalerSpec) {
			s.ScaleTargetRef.Kind = "StatefulSet"
		},
		"VirtualWorkload target": func(s *autoscalingv2.HorizontalPodAutoscalerSpec) {
			s.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{APIVersion: v1alpha1.SchemeGroupVersion.String(), Kind: v1alpha1.KindVirtualWorkload, Name: "x"}
		},
		"min above max": func(s *autoscalingv2.HorizontalPodAutoscalerSpec) {
			s.MinReplicas = ptr.To[int32](5)
			s.MaxReplicas = 2
		},
		"utilization without value": func(s *autoscalingv2.HorizontalPodAutoscalerSpec) {
			s.Metrics[0].Resource.Target.AverageUtilization = nil
		},
		"resource metric without source": func(s *autoscalingv2.HorizontalPodAutoscalerSpec) {
			s.Metrics[0].Resource = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := cpuSpec("web", 1, 4, 50)
			mutate(&spec)
			_, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Create(ctx, dryrunHPA(ns, "invalid", spec), metav1.CreateOptions{})
			if !k8serrors.IsInvalid(err) {
				t.Fatalf("expected an Invalid error, got %v", err)
			}
		})
	}
}

// TestScaleUpIsVirtual runs pods that each burn their whole CPU limit, so the
// HPA wants to scale up. Only the VirtualWorkload may change.
func TestScaleUpIsVirtual(t *testing.T) {
	ns := namespace(t)
	ctx := ctxFor(t, 10*time.Minute)
	burner := deployment(ns, "burner", 2, "100m", corev1.Container{
		Image: busyImage, Command: []string{"sh", "-c", "while true; do :; done"},
	})
	if _, err := kube.AppsV1().Deployments(ns).Create(ctx, burner, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ns, "burner")

	dhpa, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Create(ctx,
		dryrunHPA(ns, "burner", cpuSpec("burner", 1, 6, 50)), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	vwName := "dhpa-burner"
	eventually(t, time.Minute, "the VirtualWorkload to be provisioned", func(ctx context.Context) error {
		vw, err := dryrun.DryrunV1alpha1().VirtualWorkloads(ns).Get(ctx, vwName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		owner := metav1.GetControllerOf(vw)
		if owner == nil || owner.UID != dhpa.UID {
			return fmt.Errorf("not owned by the DryrunHPA: %v", vw.OwnerReferences)
		}
		if vw.Status.Selector == "" {
			return fmt.Errorf("no status.selector yet")
		}
		return nil
	})

	eventually(t, 5*time.Minute, "a virtual scale-up", func(ctx context.Context) error {
		d, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Get(ctx, "burner", metav1.GetOptions{})
		if err != nil {
			return err
		}
		vw, err := dryrun.DryrunV1alpha1().VirtualWorkloads(ns).Get(ctx, vwName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if d.Status.DesiredReplicas < 4 || vw.Spec.Replicas < 4 {
			return fmt.Errorf("desiredReplicas=%d virtual replicas=%d, conditions=%v", d.Status.DesiredReplicas, vw.Spec.Replicas, d.Status.Conditions)
		}
		if d.Status.ObservedSpecHash == "" || d.Status.VirtualWorkloadRef == nil || d.Status.VirtualWorkloadRef.Name != vwName {
			return fmt.Errorf("status not filled: %+v", d.Status)
		}
		if d.Spec.ScaleTargetRef.Kind != "Deployment" {
			return fmt.Errorf("the stored spec was rewritten: %+v", d.Spec.ScaleTargetRef)
		}
		return nil
	})

	real, err := kube.AppsV1().Deployments(ns).Get(ctx, "burner", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if *real.Spec.Replicas != 2 || real.Generation != 1 {
		t.Fatalf("the real Deployment was changed: replicas=%d generation=%d", *real.Spec.Replicas, real.Generation)
	}

	eventually(t, time.Minute, "DryRun events on the DryrunHPA", func(ctx context.Context) error {
		events, err := kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: fields.Set{
			"involvedObject.kind": v1alpha1.KindDryrunHorizontalPodAutoscaler, "involvedObject.name": "burner",
		}.String()})
		if err != nil {
			return err
		}
		for _, e := range events.Items {
			if e.Reason == "DryRunSuccessfulRescale" {
				return nil
			}
		}
		return fmt.Errorf("no DryRunSuccessfulRescale event in %d events", len(events.Items))
	})

	eventually(t, time.Minute, "stored decisions", func(ctx context.Context) error {
		records, err := decisions(ctx, ns, "burner")
		if err != nil {
			return err
		}
		rescales := 0
		for _, r := range records {
			if r.TargetKind != "Deployment" || r.TargetName != "burner" {
				return fmt.Errorf("record with wrong target: %+v", r)
			}
			if r.SourceReplicas == nil || *r.SourceReplicas != 2 {
				return fmt.Errorf("record without the real replica count: %+v", r)
			}
			if r.Rescale {
				rescales++
			}
		}
		if rescales == 0 {
			return fmt.Errorf("%d records, none rescaled", len(records))
		}
		return nil
	})

	eventually(t, time.Minute, "per-object metrics", func(ctx context.Context) error {
		lines, err := metricLines(ctx, "dryrun_hpa_recommendation_replicas", `namespace="`+ns+`"`, `stage="desired"`)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return fmt.Errorf("no recommendation metric")
		}
		return nil
	})

	if err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Delete(ctx, "burner", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Minute, "the VirtualWorkload to be garbage collected", func(ctx context.Context) error {
		_, err := dryrun.DryrunV1alpha1().VirtualWorkloads(ns).Get(ctx, vwName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("still exists (err=%v)", err)
	})
}

func TestMissingSourceWorkload(t *testing.T) {
	ns := namespace(t)
	ctx := ctxFor(t, 5*time.Minute)
	if _, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Create(ctx,
		dryrunHPA(ns, "ghost", cpuSpec("does-not-exist", 1, 4, 50)), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 2*time.Minute, "ScalingActive=False/FailedGetSourceWorkload", func(ctx context.Context) error {
		d, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Get(ctx, "ghost", metav1.GetOptions{})
		if err != nil {
			return err
		}
		c := condition(d.Status.Conditions, autoscalingv2.ScalingActive)
		if c == nil || c.Status != corev1.ConditionFalse || c.Reason != v1alpha1.ReasonFailedGetSourceWorkload {
			return fmt.Errorf("conditions: %v", d.Status.Conditions)
		}
		if !strings.Contains(c.Message, "does-not-exist") {
			return fmt.Errorf("message doesn't name the source: %q", c.Message)
		}
		return nil
	})
}
