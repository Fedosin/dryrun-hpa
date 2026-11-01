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

package analysis

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"iter"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fedosin/dryrun-hpa/internal/store"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func seq(records ...store.Record) iter.Seq2[store.Record, error] {
	return func(yield func(store.Record, error) bool) {
		for _, r := range records {
			if !yield(r, nil) {
				return
			}
		}
	}
}

func rec(at time.Duration, current, desired int32) store.Record {
	return store.Record{
		Time: t0.Add(at), Namespace: "prod", Name: "web", SpecHash: "h1", TargetKind: "Deployment", TargetName: "web",
		MaxReplicas: 5, CurrentReplicas: current, DesiredReplicas: desired, Rescale: current != desired,
		Duration: store.Duration(2 * time.Millisecond),
	}
}

func TestSummarize(t *testing.T) {
	src := int32(2)
	r0 := rec(0, 2, 4)
	r0.SourceReplicas = &src
	r3 := rec(3*time.Hour, 4, 5)
	r3.Error = "boom"
	out, err := Summarize(seq(
		r0,
		rec(time.Hour, 4, 2),             // reverses, but an hour later
		rec(time.Hour+time.Minute, 2, 4), // reverses within FlapWindow
		r3,
	))
	require.NoError(t, err)
	require.Len(t, out, 1)
	s := out[0]
	assert.Equal(t, 4, s.Decisions)
	assert.Equal(t, 1, s.Errors)
	assert.Equal(t, 3, s.ScaleUps)
	assert.Equal(t, 1, s.ScaleDowns)
	assert.Equal(t, 1, s.Flaps)
	assert.Equal(t, int32(2), s.MinDesired)
	assert.Equal(t, int32(5), s.MaxDesired)
	assert.InDelta(t, 3.75, s.MeanDesired, 1e-9)
	assert.InDelta(t, 0.25, s.AtMaxRatio, 1e-9)
	// 4 replicas for 1h, 2 for 1m, 4 for 1h59m.
	assert.InDelta(t, 4+2.0/60+4*(119.0/60), s.ReplicaHours, 1e-9)
	assert.InDelta(t, 2, s.SourceReplicaHours, 1e-9)
	assert.InDelta(t, 0.002, s.MeanReconcileSeconds, 1e-9)
}

func TestSummarizeSeparatesSpecs(t *testing.T) {
	a := rec(0, 1, 1)
	b := rec(time.Minute, 1, 1)
	b.SpecHash = "h2"
	out, err := Summarize(seq(a, b))
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "h1", out[0].SpecHash)
}

func TestCompare(t *testing.T) {
	withReal := func(r store.Record, real, src int32) store.Record {
		r.RealHPADesired = &real
		r.SourceReplicas = &src
		return r
	}
	failed := rec(3*time.Minute, 1, 1)
	failed.Error = "boom"
	out, err := Compare(seq(
		withReal(rec(0, 3, 3), 3, 3),
		withReal(rec(time.Minute, 3, 5), 3, 3),
		withReal(rec(2*time.Minute, 5, 2), 3, 3),
		failed,
	))
	require.NoError(t, err)
	require.Len(t, out, 1)
	c := out[0]
	assert.Equal(t, 3, c.Samples)
	assert.Equal(t, 1, c.Matches)
	assert.InDelta(t, 1.0/3, c.MatchRatio, 1e-9)
	assert.InDelta(t, 1, c.MeanAbsDiff, 1e-9)
	assert.InDelta(t, 1.0/3, c.MeanDiff, 1e-9)
	assert.Equal(t, int32(2), c.MaxAbsDiff)
	assert.Equal(t, 3, c.SourceSamples)
}

func TestWriteRecords(t *testing.T) {
	r := rec(0, 1, 2)
	r.Metrics = []store.Metric{{Type: "Resource", Name: "cpu", TargetType: "Utilization", TargetValue: "80", Current: "120", ProposedReplicas: new(int32(2))}}

	var buf bytes.Buffer
	n, err := WriteRecords(&buf, FormatNDJSON, seq(r, r), 0)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Equal(t, 2, bytes.Count(buf.Bytes(), []byte("\n")))

	buf.Reset()
	_, err = WriteRecords(&buf, FormatJSON, seq(r, r), 0)
	require.NoError(t, err)
	var arr []store.Record
	require.NoError(t, json.Unmarshal(buf.Bytes(), &arr))
	assert.Len(t, arr, 2)

	buf.Reset()
	_, err = WriteRecords(&buf, FormatCSV, seq(r), 2)
	require.NoError(t, err)
	rows, err := csv.NewReader(&buf).ReadAll()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Len(t, rows[0], len(CSVHeader)+8)
	assert.Equal(t, len(rows[0]), len(rows[1]))
	assert.Equal(t, "Resource/cpu", rows[1][len(CSVHeader)])
	assert.Equal(t, "Utilization=80", rows[1][len(CSVHeader)+2])
}

func TestFilterFromQuery(t *testing.T) {
	now := t0
	f, err := FilterFromQuery(url.Values{"namespace": {"prod"}, "since": {"2h"}, "until": {"2026-01-01T11:00:00Z"}, "limit": {"5"}, "onlyRescales": {"true"}}, now)
	require.NoError(t, err)
	assert.Equal(t, "prod", f.Namespace)
	assert.Equal(t, now.Add(-2*time.Hour), f.Since)
	assert.Equal(t, t0.Add(-time.Hour), f.Until)
	assert.Equal(t, 5, f.Limit)
	assert.True(t, f.OnlyRescales)

	back, err := FilterFromQuery(Query(f), now)
	require.NoError(t, err)
	assert.Equal(t, f, back)

	_, err = FilterFromQuery(url.Values{"since": {"yesterday"}}, now)
	assert.Error(t, err)
}
