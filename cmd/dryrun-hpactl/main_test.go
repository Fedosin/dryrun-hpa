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
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/fedosin/dryrun-hpa/internal/analysis"
	"github.com/fedosin/dryrun-hpa/internal/exportapi"
	"github.com/fedosin/dryrun-hpa/internal/store"
)

func seedStore(t *testing.T) (string, *store.SQLite) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "decisions.db")
	st, err := store.OpenSQLite(context.Background(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	t0 := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	real := int32(3)
	var records []store.Record
	for i, desired := range []int32{3, 4, 4, 2} {
		current := int32(3)
		if i > 0 {
			current = records[i-1].DesiredReplicas
		}
		records = append(records, store.Record{
			Time: t0.Add(time.Duration(i) * time.Minute), Namespace: "prod", Name: "web", SpecHash: "abc", TargetKind: "Deployment",
			TargetName: "web", MaxReplicas: 10, CurrentReplicas: current, DesiredReplicas: desired, Rescale: current != desired,
			RealHPADesired: &real, SourceReplicas: &real, Spec: json.RawMessage(`{"maxReplicas":10}`),
		})
	}
	require.NoError(t, st.Write(context.Background(), records))
	return path, st
}

func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	require.NoError(t, cmd.ExecuteContext(context.Background()), out.String())
	return out.String()
}

func TestExportOverHTTPMatchesDB(t *testing.T) {
	path, st := seedStore(t)
	srv := httptest.NewServer(exportapi.NewHandler(st))
	defer srv.Close()

	viaHTTP := run(t, "--server", srv.URL, "export", "--since", "2h")
	viaDB := run(t, "--db", path, "export", "--since", "2h")
	assert.Equal(t, viaDB, viaHTTP)
	assert.Equal(t, 4, strings.Count(viaHTTP, "\n"))

	csv := run(t, "--server", srv.URL, "export", "-o", "csv", "--only-rescales")
	assert.Equal(t, 3, strings.Count(csv, "\n"), "header and two rescales")
}

func TestReportAndCompare(t *testing.T) {
	_, st := seedStore(t)
	srv := httptest.NewServer(exportapi.NewHandler(st))
	defer srv.Close()

	var summaries []analysis.Summary
	require.NoError(t, json.Unmarshal([]byte(run(t, "--server", srv.URL, "report", "-o", "json")), &summaries))
	require.Len(t, summaries, 1)
	assert.Equal(t, 4, summaries[0].Decisions)
	assert.Equal(t, 1, summaries[0].ScaleUps)
	assert.Equal(t, 1, summaries[0].ScaleDowns)

	table := run(t, "--server", srv.URL, "report")
	assert.Contains(t, table, "REPLICA-H")
	assert.Contains(t, table, "abc")

	var comparisons []analysis.Comparison
	require.NoError(t, json.Unmarshal([]byte(run(t, "--server", srv.URL, "compare", "-o", "json")), &comparisons))
	require.Len(t, comparisons, 1)
	assert.Equal(t, 4, comparisons[0].Samples)
	assert.Equal(t, 1, comparisons[0].Matches)

	var specs []store.SpecVersion
	require.NoError(t, json.Unmarshal([]byte(run(t, "--server", srv.URL, "specs", "-n", "prod")), &specs))
	require.Len(t, specs, 1)
	assert.Equal(t, "abc", specs[0].SpecHash)
}

func TestExportAPIRejectsBadInput(t *testing.T) {
	_, st := seedStore(t)
	srv := httptest.NewServer(exportapi.NewHandler(st))
	defer srv.Close()
	_, err := httpGet(context.Background(), srv.URL+"/api/v1/decisions?format=xml")
	assert.ErrorContains(t, err, "400")
	_, err = httpGet(context.Background(), srv.URL+"/api/v1/decisions?since=yesterday")
	assert.ErrorContains(t, err, "400")
}
