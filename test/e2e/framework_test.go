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

// Package e2e tests the controller in a real cluster. See README.md.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
	"github.com/fedosin/dryrun-hpa/internal/store"
	dryrunclientset "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned"
)

var (
	kube   kubernetes.Interface
	dryrun dryrunclientset.Interface

	controllerNamespace = env("E2E_CONTROLLER_NAMESPACE", "dryrun-hpa-system")
	controllerService   = env("E2E_CONTROLLER_SERVICE", "dryrun-hpa-controller")
	busyImage           = env("E2E_BUSY_IMAGE", "docker.io/library/busybox:1.37")
	phpImage            = env("E2E_PHP_IMAGE", "registry.k8s.io/hpa-example")
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading kubeconfig:", err)
		os.Exit(1)
	}
	kube = kubernetes.NewForConfigOrDie(cfg)
	dryrun = dryrunclientset.NewForConfigOrDie(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err = wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		d, err := kube.AppsV1().Deployments(controllerNamespace).Get(ctx, "dryrun-hpa-controller", metav1.GetOptions{})
		return err == nil && d.Status.AvailableReplicas > 0, nil
	})
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "the controller is not running; deploy it first (make kind-up kind-load deploy-e2e)")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// namespace creates a namespace that is deleted after the test.
func namespace(t *testing.T) string {
	t.Helper()
	name := "e2e-" + rand.String(6)
	_, err := kube.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() && os.Getenv("E2E_KEEP_FAILED") != "" {
			t.Logf("keeping namespace %s", name)
			return
		}
		_ = kube.CoreV1().Namespaces().Delete(context.Background(), name, metav1.DeleteOptions{})
	})
	return name
}

func ctxFor(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// eventually polls cond until it succeeds or the timeout expires; the last
// condition error is reported.
func eventually(t *testing.T, timeout time.Duration, what string, cond func(ctx context.Context) error) {
	t.Helper()
	var last error
	err := wait.PollUntilContextTimeout(context.Background(), 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		last = cond(ctx)
		return last == nil, nil
	})
	if err != nil {
		t.Fatalf("timed out after %s waiting for %s: %v", timeout, what, last)
	}
}

func deployment(namespace, name string, replicas int32, cpu string, container corev1.Container) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	container.Name = "app"
	container.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)},
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To[int64](1),
					Containers:                    []corev1.Container{container},
				},
			},
		},
	}
}

func cpuSpec(target string, minReplicas, maxReplicas, utilization int32) autoscalingv2.HorizontalPodAutoscalerSpec {
	return autoscalingv2.HorizontalPodAutoscalerSpec{
		ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: target},
		MinReplicas:    ptr.To(minReplicas),
		MaxReplicas:    maxReplicas,
		Metrics: []autoscalingv2.MetricSpec{{
			Type: autoscalingv2.ResourceMetricSourceType,
			Resource: &autoscalingv2.ResourceMetricSource{
				Name:   corev1.ResourceCPU,
				Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To(utilization)},
			},
		}},
	}
}

func dryrunHPA(namespace, name string, spec autoscalingv2.HorizontalPodAutoscalerSpec) *v1alpha1.DryrunHorizontalPodAutoscaler {
	return &v1alpha1.DryrunHorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Spec: spec}
}

func waitReady(t *testing.T, namespace, name string) {
	t.Helper()
	eventually(t, 3*time.Minute, "deployment "+name+" to be available", func(ctx context.Context) error {
		d, err := kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if d.Status.AvailableReplicas != *d.Spec.Replicas {
			return fmt.Errorf("%d/%d available", d.Status.AvailableReplicas, *d.Spec.Replicas)
		}
		return nil
	})
}

func condition(conds []autoscalingv2.HorizontalPodAutoscalerCondition, t autoscalingv2.HorizontalPodAutoscalerConditionType) *autoscalingv2.HorizontalPodAutoscalerCondition {
	for i := range conds {
		if conds[i].Type == t {
			return &conds[i]
		}
	}
	return nil
}

// proxyGet fetches a path from the controller service through the API server.
func proxyGet(ctx context.Context, port, path string, params map[string]string) (io.ReadCloser, error) {
	return kube.CoreV1().Services(controllerNamespace).ProxyGet("http", controllerService, port, path, params).Stream(ctx)
}

// decisions exports the stored decisions of a DryrunHPA.
func decisions(ctx context.Context, namespace, name string) ([]store.Record, error) {
	body, err := proxyGet(ctx, "export", "/api/v1/decisions", map[string]string{"namespace": namespace, "name": name})
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []store.Record
	dec := json.NewDecoder(bufio.NewReader(body))
	for {
		var r store.Record
		if err := dec.Decode(&r); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return out, err
		}
		out = append(out, r)
	}
}

// metricLines returns the lines of /metrics that contain all substrings.
func metricLines(ctx context.Context, substrings ...string) ([]string, error) {
	body, err := proxyGet(ctx, "metrics", "/metrics", nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []string
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
next:
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		for _, s := range substrings {
			if !strings.Contains(line, s) {
				continue next
			}
		}
		out = append(out, line)
	}
	return out, sc.Err()
}
