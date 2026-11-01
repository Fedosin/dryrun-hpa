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
	"bytes"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuantile(t *testing.T) {
	first := map[float64]float64{0.1: 0, 0.2: 0, math.Inf(1): 0}
	last := map[float64]float64{0.1: 50, 0.2: 100, math.Inf(1): 100}
	assert.InDelta(t, 0.1, quantile(0.5, first, last), 1e-9)
	assert.InDelta(t, 0.198, quantile(0.99, first, last), 1e-9)
	assert.Zero(t, quantile(0.5, last, last), "no observations")
}

func TestQuantileFromExposition(t *testing.T) {
	parse := func(count0, count1 int) scrape {
		text := fmt.Sprintf(`# TYPE reconcile_seconds histogram
reconcile_seconds_bucket{action="none",le="0.1"} %d
reconcile_seconds_bucket{action="none",le="1"} %d
reconcile_seconds_bucket{action="none",le="+Inf"} %d
reconcile_seconds_sum{action="none"} 1
reconcile_seconds_count{action="none"} %d
reconcile_seconds_bucket{action="up",le="0.1"} 0
reconcile_seconds_bucket{action="up",le="1"} %d
reconcile_seconds_bucket{action="up",le="+Inf"} %d
reconcile_seconds_sum{action="up"} 1
reconcile_seconds_count{action="up"} %d
`, count0, count0, count0, count0, count1, count1, count1)
		parser := expfmt.NewTextParser(model.UTF8Validation)
		families, err := parser.TextToMetricFamilies(strings.NewReader(text))
		require.NoError(t, err)
		return scrape{families: families}
	}
	first, last := parse(0, 0), parse(90, 10)
	b0, b1 := first.buckets("reconcile_seconds", nil), last.buckets("reconcile_seconds", nil)
	assert.Equal(t, 100.0, b1[math.Inf(1)])
	assert.InDelta(t, 0.1, quantile(0.9, b0, b1), 1e-9)
	assert.InDelta(t, 0.91, quantile(0.99, b0, b1), 1e-9)
}

func TestLinearFit(t *testing.T) {
	f, ok := linearFit([]float64{100, 200, 400}, []float64{0.15, 0.25, 0.45})
	require.True(t, ok)
	assert.InDelta(t, 0.001, f.Slope, 1e-9)
	assert.InDelta(t, 0.05, f.Intercept, 1e-9)
	assert.InDelta(t, 1, f.R2, 1e-9)
	_, ok = linearFit([]float64{1}, []float64{1})
	assert.False(t, ok)
}

func TestReport(t *testing.T) {
	in := strings.Join([]string{
		`{"labels":{"count":"100","replicas":"3"},"objects":100,"cpuCores":0.15,"rssBytesMean":104857600}`,
		`{"labels":{"count":"200","replicas":"3"},"objects":200,"cpuCores":0.25,"rssBytesMean":157286400}`,
		`{"labels":{"count":"100","replicas":"10"},"objects":100,"cpuCores":0.3,"rssBytesMean":209715200}`,
	}, "\n")
	samples, err := readSamples(strings.NewReader(in))
	require.NoError(t, err)
	var out bytes.Buffer
	writeReport(&out, samples)
	assert.Contains(t, out.String(), "## replicas=3")
	assert.Contains(t, out.String(), "## replicas=10")
	assert.Contains(t, out.String(), "CPU: 1.000 millicores per object + 50 millicores base")
	assert.Contains(t, out.String(), "Memory: 512.0 KiB per object + 50 MiB base")
}

func TestObjects(t *testing.T) {
	o := applyOptions{run: "perf", perNamespace: 10, replicas: 3, metrics: 3, behavior: true, utilization: 0.7,
		wavePeriod: 600e9, amplitude: 0.5, kwok: true, cpuRequestMilli: 100}
	d, h := o.objects(23, rand.New(rand.NewPCG(1, 0)))
	assert.Equal(t, "perf-2", d.Namespace)
	assert.Equal(t, "app-23", h.Name)
	assert.Equal(t, d.Name, h.Spec.ScaleTargetRef.Name)
	assert.Len(t, h.Spec.Metrics, 3)
	assert.NotNil(t, h.Spec.Behavior)
	assert.Equal(t, int32(12), h.Spec.MaxReplicas)
	assert.Equal(t, "kwok", d.Spec.Template.Spec.NodeSelector["type"])
	assert.NotEmpty(t, d.Spec.Template.Annotations["perf.dryrun-hpa.io/total-cpu"])
}
