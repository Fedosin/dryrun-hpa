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

// Package exportapi serves stored decisions over HTTP.
//
//	GET /api/v1/decisions?namespace=&name=&specHash=&since=&until=&afterId=&limit=&onlyRescales=&format=ndjson|json|csv&metrics=N
//	GET /api/v1/specs?namespace=&name=
//	GET /api/v1/summary?<filter>
//	GET /api/v1/compare?<filter>
package exportapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"k8s.io/klog/v2"

	"github.com/fedosin/dryrun-hpa/internal/analysis"
	"github.com/fedosin/dryrun-hpa/internal/store"
)

// DefaultCSVMetrics is the number of metric column groups in CSV exports.
const DefaultCSVMetrics = 4

// NewHandler returns the export API handler.
func NewHandler(st store.Store) http.Handler {
	s := &server{store: st, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/decisions", s.decisions)
	mux.HandleFunc("GET /api/v1/specs", s.specs)
	mux.HandleFunc("GET /api/v1/summary", s.summary)
	mux.HandleFunc("GET /api/v1/compare", s.compare)
	return mux
}

type server struct {
	store store.Store
	now   func() time.Time
}

func (s *server) decisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := analysis.FilterFromQuery(q, s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	format, err := analysis.ParseFormat(q.Get("format"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	metrics := DefaultCSVMetrics
	if v := q.Get("metrics"); v != "" {
		if metrics, err = strconv.Atoi(v); err != nil || metrics < 0 {
			http.Error(w, "invalid metrics", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", format.ContentType())
	n, err := analysis.WriteRecords(w, format, s.store.Query(r.Context(), f), metrics)
	if err != nil {
		// Headers are sent; the client sees a truncated body.
		klog.FromContext(r.Context()).Error(err, "Exporting decisions failed", "written", n)
	}
}

func (s *server) specs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	specs, err := s.store.Specs(r.Context(), q.Get("namespace"), q.Get("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, specs)
}

func (s *server) summary(w http.ResponseWriter, r *http.Request) {
	f, err := analysis.FilterFromQuery(r.URL.Query(), s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := analysis.Summarize(s.store.Query(r.Context(), f))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

func (s *server) compare(w http.ResponseWriter, r *http.Request) {
	f, err := analysis.FilterFromQuery(r.URL.Query(), s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := analysis.Compare(s.store.Query(r.Context(), f))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
