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

package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func record(name string, at time.Duration, current, desired int32) Record {
	metricReplicas := desired
	return Record{
		Time: t0.Add(at), Namespace: "prod", Name: name, UID: "uid-" + name, SpecHash: "hash-" + name,
		TargetKind: "Deployment", TargetName: name, MinReplicas: 1, MaxReplicas: 10,
		CurrentReplicas: current, DesiredReplicas: desired, MetricReplicas: &metricReplicas,
		Rescale: current != desired, MetricName: "cpu resource utilization (percentage of request)",
		Metrics:    []Metric{{Index: 0, Type: "Resource", Name: "cpu", TargetType: "Utilization", TargetValue: "80", Current: "95"}},
		Conditions: []Condition{{Type: "AbleToScale", Status: "True", Reason: "SucceededRescale"}},
		Duration:   Duration(3 * time.Millisecond),
		Spec:       json.RawMessage(`{"maxReplicas":10}`),
	}
}

func openTest(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "decisions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func collect(t *testing.T, s Store, f Filter) []Record {
	t.Helper()
	var out []Record
	for r, err := range s.Query(context.Background(), f) {
		require.NoError(t, err)
		out = append(out, r)
	}
	return out
}

func TestSQLiteRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	in := record("web", 0, 2, 4)
	in.SourceReplicas = new(int32(3))
	require.NoError(t, s.Write(ctx, []Record{in}))

	out := collect(t, s, Filter{})
	require.Len(t, out, 1)
	got := out[0]
	assert.Positive(t, got.ID)
	assert.True(t, in.Time.Equal(got.Time))
	assert.Equal(t, in.Metrics, got.Metrics)
	assert.Equal(t, in.Conditions, got.Conditions)
	assert.Equal(t, int32(3), *got.SourceReplicas)
	assert.Nil(t, got.RealHPADesired)
	assert.Nil(t, got.StabilizedReplicas)
	assert.Equal(t, int32(4), *got.MetricReplicas)
	assert.Equal(t, in.Duration, got.Duration)
	assert.True(t, got.Rescale)

	specs, err := s.Specs(ctx, "prod", "web")
	require.NoError(t, err)
	require.Len(t, specs, 1)
	assert.JSONEq(t, `{"maxReplicas":10}`, string(specs[0].Spec))
}

func TestSQLiteFilters(t *testing.T) {
	s := openTest(t)
	require.NoError(t, s.Write(context.Background(), []Record{
		record("web", 0, 2, 2),
		record("web", time.Minute, 2, 3),
		record("api", time.Minute, 1, 1),
		record("web", 2*time.Minute, 3, 3),
	}))

	assert.Len(t, collect(t, s, Filter{Name: "web"}), 3)
	assert.Len(t, collect(t, s, Filter{OnlyRescales: true}), 1)
	assert.Len(t, collect(t, s, Filter{Since: t0.Add(time.Minute)}), 3)
	assert.Len(t, collect(t, s, Filter{Until: t0.Add(time.Minute)}), 1)
	assert.Len(t, collect(t, s, Filter{Limit: 2}), 2)
	all := collect(t, s, Filter{})
	assert.Len(t, collect(t, s, Filter{AfterID: all[1].ID}), 2)
	assert.Len(t, collect(t, s, Filter{SpecHash: "hash-api"}), 1)
}

func TestSQLiteRetention(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	require.NoError(t, s.Write(ctx, []Record{record("old", 0, 1, 1), record("web", time.Hour, 1, 1)}))

	n, err := s.DeleteBefore(ctx, t0.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Len(t, collect(t, s, Filter{}), 1)
	specs, err := s.Specs(ctx, "", "")
	require.NoError(t, err)
	require.Len(t, specs, 1, "spec without decisions is removed")
	assert.Equal(t, "web", specs[0].Name)

	size, err := s.SizeBytes()
	require.NoError(t, err)
	assert.Positive(t, size)
}

func TestSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.db")
	s, err := OpenSQLite(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, s.Write(context.Background(), []Record{record("web", 0, 1, 1)}))
	require.NoError(t, s.Close())

	s, err = OpenSQLite(context.Background(), path)
	require.NoError(t, err)
	defer s.Close()
	assert.Len(t, collect(t, s, Filter{}), 1)
}

func TestQueryStopsEarly(t *testing.T) {
	s := openTest(t)
	require.NoError(t, s.Write(context.Background(), []Record{record("a", 0, 1, 1), record("b", 0, 1, 1)}))
	n := 0
	for range s.Query(context.Background(), Filter{}) {
		n++
		break
	}
	assert.Equal(t, 1, n)
}

func TestWriterFlushesOnShutdown(t *testing.T) {
	s := openTest(t)
	w := NewWriter(s, WriterOptions{BatchSize: 1000, FlushInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	for i := range 25 {
		require.True(t, w.Enqueue(record("web", time.Duration(i)*time.Second, 1, 1)))
	}
	cancel()
	w.Wait()
	assert.Len(t, collect(t, s, Filter{}), 25)
}

func TestWriterDropsWhenFull(t *testing.T) {
	w := NewWriter(None{}, WriterOptions{QueueSize: 1})
	assert.True(t, w.Enqueue(Record{}))
	assert.False(t, w.Enqueue(Record{}))
}

func TestDurationJSON(t *testing.T) {
	b, err := json.Marshal(Duration(1500 * time.Millisecond))
	require.NoError(t, err)
	assert.Equal(t, "1.5", string(b))
	var d Duration
	require.NoError(t, json.Unmarshal(b, &d))
	assert.Equal(t, Duration(1500*time.Millisecond), d)
}
