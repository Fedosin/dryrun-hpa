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

// Command fake-metrics-server serves the resource metrics API
// (metrics.k8s.io/v1beta1) with synthetic usage, for performance tests with
// simulated (kwok) pods. It is not secure: it doesn't authenticate requests.
//
// Usage comes from annotations on the pods (set them on the pod template):
//
//	perf.dryrun-hpa.io/total-cpu      total millicores of all ready pods with the same "app" label
//	perf.dryrun-hpa.io/memory         bytes per pod
//	perf.dryrun-hpa.io/period         period of a sine wave on the total CPU (e.g. 10m)
//	perf.dryrun-hpa.io/amplitude      relative amplitude of the wave, 0..1
//	perf.dryrun-hpa.io/phase          phase offset in seconds
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	metricsapi "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

const (
	annotationPrefix    = "perf.dryrun-hpa.io/"
	annotationTotalCPU  = annotationPrefix + "total-cpu"
	annotationMemory    = annotationPrefix + "memory"
	annotationPeriod    = annotationPrefix + "period"
	annotationAmplitude = annotationPrefix + "amplitude"
	annotationPhase     = annotationPrefix + "phase"
	indexByApp          = "byApp"
	window              = 15 * time.Second
)

func main() {
	klog.InitFlags(nil)
	addr := flag.String("secure-addr", ":6443", "HTTPS listen address.")
	kubeconfig := flag.String("kubeconfig", "", "Path to a kubeconfig; in-cluster config if empty.")
	flag.Parse()

	var cfg *rest.Config
	var err error
	if *kubeconfig != "" {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		cfg, err = rest.InClusterConfig()
	}
	if err != nil {
		klog.Fatal(err)
	}
	cfg.QPS, cfg.Burst = 50, 100
	client := kubernetes.NewForConfigOrDie(cfg)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	factory := informers.NewSharedInformerFactoryWithOptions(client, 0, informers.WithTransform(func(obj interface{}) (interface{}, error) {
		if a, err := meta.Accessor(obj); err == nil {
			a.SetManagedFields(nil)
		}
		return obj, nil
	}))
	pods := factory.Core().V1().Pods()
	if err := pods.Informer().AddIndexers(cache.Indexers{indexByApp: func(obj interface{}) ([]string, error) {
		p := obj.(*corev1.Pod)
		return []string{p.Namespace + "/" + p.Labels["app"]}, nil
	}}); err != nil {
		klog.Fatal(err)
	}
	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	s := &server{pods: pods.Lister(), indexer: pods.Informer().GetIndexer(), start: time.Now()}
	cert, err := selfSignedCert()
	if err != nil {
		klog.Fatal(err)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.routes(),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	klog.InfoS("Serving fake resource metrics", "address", *addr)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		klog.Fatal(err)
	}
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fake-metrics-server"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"fake-metrics-server", "fake-metrics-server.kube-system.svc"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

type server struct {
	pods    corelisters.PodLister
	indexer cache.Indexer
	start   time.Time
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }
	mux.HandleFunc("/healthz", ok)
	mux.HandleFunc("/livez", ok)
	mux.HandleFunc("/readyz", ok)
	mux.HandleFunc("GET /apis/metrics.k8s.io", func(w http.ResponseWriter, _ *http.Request) {
		gv := metav1.GroupVersionForDiscovery{GroupVersion: metricsapi.SchemeGroupVersion.String(), Version: "v1beta1"}
		writeJSON(w, &metav1.APIGroup{TypeMeta: metav1.TypeMeta{Kind: "APIGroup", APIVersion: "v1"},
			Name: metricsapi.GroupName, Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv})
	})
	mux.HandleFunc("GET /apis/metrics.k8s.io/v1beta1", func(w http.ResponseWriter, _ *http.Request) {
		verbs := metav1.Verbs{"get", "list"}
		writeJSON(w, &metav1.APIResourceList{
			TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
			GroupVersion: metricsapi.SchemeGroupVersion.String(),
			APIResources: []metav1.APIResource{
				{Name: "pods", Kind: "PodMetrics", Namespaced: true, Verbs: verbs},
				{Name: "nodes", Kind: "NodeMetrics", Verbs: verbs},
			},
		})
	})
	mux.HandleFunc("GET /apis/metrics.k8s.io/v1beta1/pods", s.listPods)
	mux.HandleFunc("GET /apis/metrics.k8s.io/v1beta1/namespaces/{namespace}/pods", s.listPods)
	mux.HandleFunc("GET /apis/metrics.k8s.io/v1beta1/namespaces/{namespace}/pods/{name}", s.getPod)
	mux.HandleFunc("GET /apis/metrics.k8s.io/v1beta1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, &metricsapi.NodeMetricsList{TypeMeta: metav1.TypeMeta{Kind: "NodeMetricsList", APIVersion: metricsapi.SchemeGroupVersion.String()}})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status: metav1.StatusFailure, Code: int32(code), Reason: reason, Message: msg})
}

func ready(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || p.DeletionTimestamp != nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (s *server) listPods(w http.ResponseWriter, r *http.Request) {
	selector, err := labels.Parse(r.URL.Query().Get("labelSelector"))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}
	var pods []*corev1.Pod
	if ns := r.PathValue("namespace"); ns != "" {
		pods, err = s.pods.Pods(ns).List(selector)
	} else {
		pods, err = s.pods.List(selector)
	}
	if err != nil {
		writeStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, err.Error())
		return
	}
	now := time.Now()
	list := &metricsapi.PodMetricsList{TypeMeta: metav1.TypeMeta{Kind: "PodMetricsList", APIVersion: metricsapi.SchemeGroupVersion.String()}}
	for _, p := range pods {
		if m, ok := s.metrics(p, now); ok {
			list.Items = append(list.Items, m)
		}
	}
	writeJSON(w, list)
}

func (s *server) getPod(w http.ResponseWriter, r *http.Request) {
	p, err := s.pods.Pods(r.PathValue("namespace")).Get(r.PathValue("name"))
	if err != nil {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, err.Error())
		return
	}
	m, ok := s.metrics(p, time.Now())
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "no metrics for pod "+p.Name)
		return
	}
	writeJSON(w, &m)
}

func floatAnnotation(p *corev1.Pod, key string, def float64) float64 {
	if v, err := strconv.ParseFloat(p.Annotations[key], 64); err == nil {
		return v
	}
	return def
}

// metrics returns the synthetic usage of a ready pod.
func (s *server) metrics(p *corev1.Pod, now time.Time) (metricsapi.PodMetrics, bool) {
	if !ready(p) || len(p.Spec.Containers) == 0 {
		return metricsapi.PodMetrics{}, false
	}
	totalCPU := floatAnnotation(p, annotationTotalCPU, 0)
	if totalCPU > 0 {
		if period, err := time.ParseDuration(p.Annotations[annotationPeriod]); err == nil && period > 0 {
			phase := floatAnnotation(p, annotationPhase, 0)
			amplitude := math.Min(math.Max(floatAnnotation(p, annotationAmplitude, 0), 0), 1)
			t := now.Sub(s.start).Seconds() + phase
			totalCPU *= 1 + amplitude*math.Sin(2*math.Pi*t/period.Seconds())
		}
	}
	peers := 1
	if objs, err := s.indexer.ByIndex(indexByApp, p.Namespace+"/"+p.Labels["app"]); err == nil {
		peers = 0
		for _, o := range objs {
			if ready(o.(*corev1.Pod)) {
				peers++
			}
		}
		peers = max(peers, 1)
	}
	perPod := totalCPU / float64(peers)
	memory := floatAnnotation(p, annotationMemory, 64<<20)

	m := metricsapi.PodMetrics{
		TypeMeta:   metav1.TypeMeta{Kind: "PodMetrics", APIVersion: metricsapi.SchemeGroupVersion.String()},
		ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace, Labels: p.Labels},
		Timestamp:  metav1.Time{Time: now.Add(-window / 2).Truncate(time.Second)},
		Window:     metav1.Duration{Duration: window},
	}
	n := float64(len(p.Spec.Containers))
	for _, c := range p.Spec.Containers {
		m.Containers = append(m.Containers, metricsapi.ContainerMetrics{
			Name: c.Name,
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(perPod/n), resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(int64(memory/n), resource.BinarySI),
			},
		})
	}
	return m, true
}
