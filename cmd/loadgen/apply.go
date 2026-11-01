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

package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	dryrunclientset "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned"
)

// LabelRun marks the namespaces of a load run.
const LabelRun = "perf.dryrun-hpa.io/run"

type applyOptions struct {
	run             string
	count           int
	perNamespace    int
	replicas        int32
	metrics         int
	behavior        bool
	utilization     float64
	wavePeriod      time.Duration
	amplitude       float64
	kwok            bool
	concurrency     int
	seed            uint64
	cpuRequestMilli int64
}

func newApplyCommand(c *clients) *cobra.Command {
	o := applyOptions{}
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create Deployments with DryrunHPAs",
		Long: `Create COUNT Deployments, each with a DryrunHPA, spread over namespaces.

Pods are scheduled on kwok nodes, and their usage comes from
fake-metrics-server through pod annotations: each Deployment gets a fixed
total CPU load (a random utilization around --utilization of its requests at
--replicas) that follows a sine wave, so the DryrunHPAs keep scaling.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kube, dryrun, err := c.kube()
			if err != nil {
				return err
			}
			return apply(cmd.Context(), kube, dryrun, o, cmd)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&o.run, "run", "perf", "Name of the run; namespaces are <run>-<n>.")
	fs.IntVar(&o.count, "count", 100, "Number of Deployments and DryrunHPAs.")
	fs.IntVar(&o.perNamespace, "per-namespace", 100, "Objects per namespace.")
	fs.Int32Var(&o.replicas, "replicas", 3, "Replicas per Deployment.")
	fs.IntVar(&o.metrics, "metrics", 1, "Metrics per DryrunHPA (1-3: CPU utilization, memory utilization, CPU average value).")
	fs.BoolVar(&o.behavior, "behavior", false, "Set custom scale-up and scale-down behavior on every DryrunHPA.")
	fs.Float64Var(&o.utilization, "utilization", 0.7, "Mean CPU utilization of the load at --replicas.")
	fs.DurationVar(&o.wavePeriod, "wave-period", 10*time.Minute, "Mean period of the load wave.")
	fs.Float64Var(&o.amplitude, "amplitude", 0.5, "Relative amplitude of the load wave.")
	fs.BoolVar(&o.kwok, "kwok", true, "Schedule pods on kwok nodes.")
	fs.IntVar(&o.concurrency, "concurrency", 16, "Concurrent creates.")
	fs.Uint64Var(&o.seed, "seed", 1, "Random seed for load parameters.")
	fs.Int64Var(&o.cpuRequestMilli, "cpu-request", 100, "CPU request per pod in millicores.")
	return cmd
}

func (o *applyOptions) namespace(i int) string {
	return fmt.Sprintf("%s-%d", o.run, i/o.perNamespace)
}

func (o *applyOptions) objects(i int, r *rand.Rand) (*appsv1.Deployment, *v1alpha1.DryrunHorizontalPodAutoscaler) {
	ns, name := o.namespace(i), fmt.Sprintf("app-%d", i)
	labels := map[string]string{"app": name}
	util := o.utilization * (0.5 + r.Float64())
	totalCPU := util * float64(o.replicas) * float64(o.cpuRequestMilli)
	period := time.Duration(float64(o.wavePeriod) * (0.5 + r.Float64()))
	pod := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "app",
			Image: "fake-image",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(o.cpuRequestMilli, resource.DecimalSI),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			}},
		}},
	}
	if o.kwok {
		pod.NodeSelector = map[string]string{"type": "kwok"}
		pod.Tolerations = []corev1.Toleration{{Key: "kwok.x-k8s.io/node", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	}
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(o.replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{
					"perf.dryrun-hpa.io/total-cpu": strconv.FormatFloat(totalCPU, 'f', 0, 64),
					"perf.dryrun-hpa.io/memory":    strconv.Itoa((64 + r.IntN(64)) << 20),
					"perf.dryrun-hpa.io/period":    period.Round(time.Second).String(),
					"perf.dryrun-hpa.io/amplitude": strconv.FormatFloat(o.amplitude, 'f', 2, 64),
					"perf.dryrun-hpa.io/phase":     strconv.Itoa(r.IntN(int(period.Seconds()))),
				}},
				Spec: pod,
			},
		},
	}

	metrics := []autoscalingv2.MetricSpec{
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](60)}}},
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceMemory, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To[int32](80)}}},
		{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
			Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.AverageValueMetricType,
				AverageValue: resource.NewMilliQuantity(o.cpuRequestMilli*7/10, resource.DecimalSI)}}},
	}
	spec := autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: name},
		MinReplicas:    ptr.To[int32](1),
		MaxReplicas:    max(o.replicas*4, 4),
		Metrics:        metrics[:min(max(o.metrics, 1), len(metrics))],
	}
	if o.behavior {
		spec.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{
			ScaleUp: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To[int32](30),
				Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 2, PeriodSeconds: 30}},
			},
			ScaleDown: &autoscalingv2.HPAScalingRules{
				StabilizationWindowSeconds: ptr.To[int32](120),
				Policies:                   []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PercentScalingPolicy, Value: 20, PeriodSeconds: 60}},
			},
		}
	}
	return d, &v1alpha1.DryrunHorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
}

func apply(ctx context.Context, kube kubernetes.Interface, dryrun dryrunclientset.Interface, o applyOptions, cmd *cobra.Command) error {
	if o.perNamespace < 1 {
		return fmt.Errorf("--per-namespace must be positive")
	}
	start := time.Now()
	for n := 0; n*o.perNamespace < o.count; n++ {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: o.namespace(n * o.perNamespace), Labels: map[string]string{LabelRun: o.run}}}
		if _, err := kube.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil && !k8serrors.IsAlreadyExists(err) {
			return err
		}
	}

	r := rand.New(rand.NewPCG(o.seed, 0))
	type item struct {
		d *appsv1.Deployment
		h *v1alpha1.DryrunHorizontalPodAutoscaler
	}
	items := make(chan item)
	var created atomic.Int64
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup
	for range o.concurrency {
		wg.Go(func() {
			for it := range items {
				err := retry(ctx, func() error {
					_, err := kube.AppsV1().Deployments(it.d.Namespace).Create(ctx, it.d, metav1.CreateOptions{})
					return err
				})
				if err == nil {
					err = retry(ctx, func() error {
						_, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(it.h.Namespace).Create(ctx, it.h, metav1.CreateOptions{})
						return err
					})
				}
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					continue
				}
				if n := created.Add(1); n%500 == 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "created %d/%d\n", n, o.count)
				}
			}
		})
	}
	for i := range o.count {
		d, h := o.objects(i, r)
		select {
		case items <- item{d, h}:
		case <-ctx.Done():
		}
	}
	close(items)
	wg.Wait()
	fmt.Fprintf(cmd.ErrOrStderr(), "created %d Deployments and DryrunHPAs in %s\n", created.Load(), time.Since(start).Round(time.Second))
	return firstErr
}

// retry retries throttling and transient errors; existing objects are fine.
func retry(ctx context.Context, f func() error) error {
	var last error
	err := wait.ExponentialBackoffWithContext(ctx, wait.Backoff{Duration: 200 * time.Millisecond, Factor: 2, Steps: 6, Cap: 10 * time.Second},
		func(context.Context) (bool, error) {
			last = f()
			switch {
			case last == nil || k8serrors.IsAlreadyExists(last):
				last = nil
				return true, nil
			case k8serrors.IsTooManyRequests(last) || k8serrors.IsServerTimeout(last) || k8serrors.IsTimeout(last) || k8serrors.IsInternalError(last):
				return false, nil
			default:
				return false, last
			}
		})
	if last != nil {
		return last
	}
	return err
}

func newDeleteCommand(c *clients) *cobra.Command {
	var run string
	var wait bool
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete the namespaces of a run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			kube, _, err := c.kube()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			sel := LabelRun + "=" + run
			list, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: sel})
			if err != nil {
				return err
			}
			for _, ns := range list.Items {
				if err := kube.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
					return err
				}
			}
			for wait {
				list, err := kube.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: sel})
				if err != nil {
					return err
				}
				if len(list.Items) == 0 {
					break
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "waiting for %d namespaces\n", len(list.Items))
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&run, "run", "perf", "Name of the run.")
	cmd.Flags().BoolVar(&wait, "wait", true, "Wait until the namespaces are gone.")
	return cmd
}
