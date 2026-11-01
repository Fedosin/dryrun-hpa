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
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/fedosin/dryrun-hpa/internal/store"
)

// Format is an export format.
type Format string

const (
	FormatNDJSON Format = "ndjson"
	FormatJSON   Format = "json"
	FormatCSV    Format = "csv"
)

// ParseFormat parses a format name; empty means NDJSON.
func ParseFormat(s string) (Format, error) {
	switch f := Format(strings.ToLower(s)); f {
	case "":
		return FormatNDJSON, nil
	case FormatNDJSON, FormatJSON, FormatCSV:
		return f, nil
	}
	return "", fmt.Errorf("unknown format %q: use ndjson, json or csv", s)
}

// ContentType returns the MIME type of a format.
func (f Format) ContentType() string {
	switch f {
	case FormatCSV:
		return "text/csv"
	case FormatJSON:
		return "application/json"
	default:
		return "application/x-ndjson"
	}
}

// CSVHeader are the columns of CSV exports. Metrics are flattened as
// metric<N>_<field>, for up to the largest number of metrics in the export.
var CSVHeader = []string{
	"id", "time", "namespace", "name", "uid", "spec_hash", "target_kind", "target_name",
	"min_replicas", "max_replicas", "current_replicas", "metric_replicas", "stabilized_replicas", "desired_replicas",
	"metric_name", "rescale", "rescale_reason", "source_replicas", "real_hpa_desired", "error", "duration_seconds", "conditions",
}

const csvMetricColumns = 4

func optInt(p *int32) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(int(*p))
}

func csvRow(r *store.Record, metrics int) []string {
	conds := make([]string, 0, len(r.Conditions))
	for _, c := range r.Conditions {
		conds = append(conds, c.Type+"="+c.Status+":"+c.Reason)
	}
	row := []string{
		strconv.FormatInt(r.ID, 10), r.Time.Format(time.RFC3339Nano), r.Namespace, r.Name, r.UID, r.SpecHash, r.TargetKind, r.TargetName,
		strconv.Itoa(int(r.MinReplicas)), strconv.Itoa(int(r.MaxReplicas)), strconv.Itoa(int(r.CurrentReplicas)),
		optInt(r.MetricReplicas), optInt(r.StabilizedReplicas), strconv.Itoa(int(r.DesiredReplicas)),
		r.MetricName, strconv.FormatBool(r.Rescale), r.RescaleReason, optInt(r.SourceReplicas), optInt(r.RealHPADesired),
		r.Error, strconv.FormatFloat(time.Duration(r.Duration).Seconds(), 'f', 6, 64), strings.Join(conds, ";"),
	}
	for i := range metrics {
		if i < len(r.Metrics) {
			m := r.Metrics[i]
			row = append(row, m.Type+"/"+m.Name, m.Current, m.TargetType+"="+m.TargetValue, optInt(m.ProposedReplicas))
		} else {
			row = append(row, make([]string, csvMetricColumns)...)
		}
	}
	return row
}

// WriteRecords streams records in the given format. For CSV, maxMetrics is
// the number of metric column groups (use the largest spec.metrics length).
func WriteRecords(w io.Writer, f Format, records iter.Seq2[store.Record, error], maxMetrics int) (int, error) {
	n := 0
	switch f {
	case FormatCSV:
		cw := csv.NewWriter(w)
		header := append([]string{}, CSVHeader...)
		for i := range maxMetrics {
			p := fmt.Sprintf("metric%d_", i)
			header = append(header, p+"name", p+"current", p+"target", p+"proposed_replicas")
		}
		if err := cw.Write(header); err != nil {
			return 0, err
		}
		for r, err := range records {
			if err != nil {
				cw.Flush()
				return n, err
			}
			if err := cw.Write(csvRow(&r, maxMetrics)); err != nil {
				return n, err
			}
			n++
		}
		cw.Flush()
		return n, cw.Error()
	case FormatJSON:
		if _, err := io.WriteString(w, "["); err != nil {
			return 0, err
		}
		enc := json.NewEncoder(w)
		for r, err := range records {
			if err != nil {
				return n, err
			}
			if n > 0 {
				if _, err := io.WriteString(w, ","); err != nil {
					return n, err
				}
			}
			if err := enc.Encode(r); err != nil {
				return n, err
			}
			n++
		}
		_, err := io.WriteString(w, "]\n")
		return n, err
	default:
		enc := json.NewEncoder(w)
		for r, err := range records {
			if err != nil {
				return n, err
			}
			if err := enc.Encode(r); err != nil {
				return n, err
			}
			n++
		}
		return n, nil
	}
}
