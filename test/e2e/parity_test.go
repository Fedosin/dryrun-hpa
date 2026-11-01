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
	"os"
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// TestParityWithRealHPA runs the Kubernetes HPA walkthrough: php-apache
// under a fixed request load. A real HPA and a DryrunHPA with the same spec
// target the same Deployment. The total CPU the load needs doesn't depend on
// the replica count, so both should settle on about the same replicas.
func TestParityWithRealHPA(t *testing.T) {
	if os.Getenv("E2E_SKIP_PARITY") != "" {
		t.Skip("E2E_SKIP_PARITY is set")
	}
	ns := namespace(t)
	ctx := ctxFor(t, 20*time.Minute)

	php := deployment(ns, "php-apache", 1, "200m", corev1.Container{
		Image: phpImage,
		Ports: []corev1.ContainerPort{{ContainerPort: 80}},
		ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt32(80)},
		}},
	})
	if _, err := kube.AppsV1().Deployments(ns).Create(ctx, php, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "php-apache", Namespace: ns},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "php-apache"},
			Ports:    []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(80)}},
		},
	}
	if _, err := kube.CoreV1().Services(ns).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitReady(t, ns, "php-apache")

	spec := cpuSpec("php-apache", 1, 8, 50)
	if _, err := kube.AutoscalingV2().HorizontalPodAutoscalers(ns).Create(ctx,
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "php-apache", Namespace: ns}, Spec: spec},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Create(ctx, dryrunHPA(ns, "php-apache", spec), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	load := deployment(ns, "load", 2, "100m", corev1.Container{
		Image:   busyImage,
		Command: []string{"sh", "-c", "while true; do wget -q -O- http://php-apache >/dev/null; done"},
	})
	if _, err := kube.AppsV1().Deployments(ns).Create(ctx, load, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	desired := func(ctx context.Context) (real, dry int32, err error) {
		h, err := kube.AutoscalingV2().HorizontalPodAutoscalers(ns).Get(ctx, "php-apache", metav1.GetOptions{})
		if err != nil {
			return 0, 0, err
		}
		d, err := dryrun.DryrunV1alpha1().DryrunHorizontalPodAutoscalers(ns).Get(ctx, "php-apache", metav1.GetOptions{})
		if err != nil {
			return 0, 0, err
		}
		return h.Status.DesiredReplicas, d.Status.DesiredReplicas, nil
	}

	eventually(t, 8*time.Minute, "both autoscalers to scale up", func(ctx context.Context) error {
		real, dry, err := desired(ctx)
		if err != nil {
			return err
		}
		if real < 2 || dry < 2 {
			return fmt.Errorf("real HPA desired %d, dry-run desired %d", real, dry)
		}
		return nil
	})

	// Let both settle, then require agreement within one replica for most samples.
	time.Sleep(2 * time.Minute)
	var samples, agree int
	for range 12 {
		real, dry, err := desired(ctx)
		if err != nil {
			t.Fatal(err)
		}
		diff := real - dry
		if diff < 0 {
			diff = -diff
		}
		samples++
		if diff <= 1 {
			agree++
		}
		t.Logf("real HPA desired %d, dry-run desired %d", real, dry)
		time.Sleep(10 * time.Second)
	}
	if agree*4 < samples*3 {
		t.Fatalf("the autoscalers agreed within one replica in only %d of %d samples", agree, samples)
	}

	records, err := decisions(ctx, ns, "php-apache")
	if err != nil {
		t.Fatal(err)
	}
	withReal := 0
	for _, r := range records {
		if r.RealHPADesired != nil {
			withReal++
		}
	}
	if withReal == 0 {
		t.Fatalf("none of %d decisions recorded the real HPA (is --compare-real-hpa set?)", len(records))
	}
}
