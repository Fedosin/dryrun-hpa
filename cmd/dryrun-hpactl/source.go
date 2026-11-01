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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/fedosin/dryrun-hpa/internal/analysis"
	"github.com/fedosin/dryrun-hpa/internal/store"
)

// decisionSource is where decisions are read from.
type decisionSource interface {
	Records(ctx context.Context, f store.Filter) iter.Seq2[store.Record, error]
	Specs(ctx context.Context, namespace, name string) ([]store.SpecVersion, error)
	Close() error
}

type sourceFlags struct {
	db         string
	server     string
	service    string
	kubeconfig string
}

func (s *sourceFlags) open(ctx context.Context) (decisionSource, error) {
	n := 0
	for _, v := range []string{s.db, s.server, s.service} {
		if v != "" {
			n++
		}
	}
	if n > 1 {
		return nil, errors.New("use only one of --db, --server and --service")
	}
	switch {
	case s.db != "":
		st, err := store.OpenSQLite(ctx, s.db)
		if err != nil {
			return nil, err
		}
		return &dbSource{st}, nil
	case s.server != "":
		return &httpSource{get: func(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
			return httpGet(ctx, strings.TrimSuffix(s.server, "/")+path+"?"+q.Encode())
		}}, nil
	default:
		return s.proxySource()
	}
}

// proxySource reaches the export API through the API server's service proxy.
func (s *sourceFlags) proxySource() (decisionSource, error) {
	svc := s.service
	if svc == "" {
		svc = "dryrun-hpa-system/dryrun-hpa-controller:export"
	}
	ns, rest, ok := strings.Cut(svc, "/")
	if !ok {
		return nil, fmt.Errorf("--service must be namespace/name[:port], got %q", svc)
	}
	name, port, _ := strings.Cut(rest, ":")
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if s.kubeconfig != "" {
		rules.ExplicitPath = s.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &httpSource{get: func(ctx context.Context, path string, q url.Values) (io.ReadCloser, error) {
		params := map[string]string{}
		for k := range q {
			params[k] = q.Get(k)
		}
		return client.CoreV1().Services(ns).ProxyGet("http", name, port, path, params).Stream(ctx)
	}}, nil
}

func httpGet(ctx context.Context, u string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return resp.Body, nil
}

type dbSource struct{ st *store.SQLite }

func (s *dbSource) Records(ctx context.Context, f store.Filter) iter.Seq2[store.Record, error] {
	return s.st.Query(ctx, f)
}

func (s *dbSource) Specs(ctx context.Context, namespace, name string) ([]store.SpecVersion, error) {
	return s.st.Specs(ctx, namespace, name)
}

func (s *dbSource) Close() error { return s.st.Close() }

type httpSource struct {
	get func(ctx context.Context, path string, q url.Values) (io.ReadCloser, error)
}

func (s *httpSource) Records(ctx context.Context, f store.Filter) iter.Seq2[store.Record, error] {
	return func(yield func(store.Record, error) bool) {
		q := analysis.Query(f)
		q.Set("format", string(analysis.FormatNDJSON))
		body, err := s.get(ctx, "/api/v1/decisions", q)
		if err != nil {
			yield(store.Record{}, err)
			return
		}
		defer body.Close()
		dec := json.NewDecoder(bufio.NewReader(body))
		for {
			var r store.Record
			if err := dec.Decode(&r); err != nil {
				if !errors.Is(err, io.EOF) {
					yield(store.Record{}, err)
				}
				return
			}
			if !yield(r, nil) {
				return
			}
		}
	}
}

func (s *httpSource) Specs(ctx context.Context, namespace, name string) ([]store.SpecVersion, error) {
	q := url.Values{}
	if namespace != "" {
		q.Set("namespace", namespace)
	}
	if name != "" {
		q.Set("name", name)
	}
	body, err := s.get(ctx, "/api/v1/specs", q)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []store.SpecVersion
	return out, json.NewDecoder(body).Decode(&out)
}

func (s *httpSource) Close() error { return nil }
