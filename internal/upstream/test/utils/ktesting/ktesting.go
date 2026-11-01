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

// Package ktesting provides the subset of k8s.io/kubernetes/test/utils/ktesting
// that the copied HPA controller tests use.
package ktesting

import (
	"context"
	"testing"
	"time"

	"k8s.io/klog/v2"
	klogktesting "k8s.io/klog/v2/ktesting"
)

// TContext is a context bound to a test.
type TContext struct {
	context.Context
	testing.TB
}

// Deadline resolves the ambiguity between context.Context and testing.TB.
func (t TContext) Deadline() (time.Time, bool) {
	return t.Context.Deadline()
}

// Init returns a context with a per-test logger that is canceled when the test ends.
func Init(tb testing.TB) TContext {
	_, ctx := klogktesting.NewTestContext(tb)
	ctx, cancel := context.WithCancel(ctx)
	tb.Cleanup(cancel)
	return TContext{Context: ctx, TB: tb}
}

// NewTestContext returns a per-test logger and a context that carries it.
func NewTestContext(tl klogktesting.TL) (klog.Logger, context.Context) {
	return klogktesting.NewTestContext(tl)
}
