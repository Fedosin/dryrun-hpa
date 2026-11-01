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

package observability

import (
	"context"
	"runtime/pprof"
)

// Components used as the "component" pprof label. Goroutines inherit the
// labels of the goroutine that started them, so CPU and allocation profiles
// can be split by component with `go tool pprof -tagfocus component=...`.
const (
	// ComponentHPALoop is the copied upstream HPA loop, including its use of
	// informers, the scale client and the metrics client.
	ComponentHPALoop = "hpa-loop"
	// ComponentProjection is the virtual pod lister and metric projection
	// called from inside the HPA loop.
	ComponentProjection = "projection"
	// ComponentRecorder converts decisions for the store, inside the HPA loop.
	ComponentRecorder = "recorder"
	// ComponentStore is the decision store writer and retention job.
	ComponentStore = "store"
	// ComponentProvisioner creates and updates VirtualWorkloads.
	ComponentProvisioner = "provisioner"
	// ComponentVirtualWorkload simulates virtual pods and writes VirtualWorkload status.
	ComponentVirtualWorkload = "vw-controller"
	// ComponentInformers runs the shared informers.
	ComponentInformers = "informers"
	// ComponentHTTP serves metrics, health and the export API.
	ComponentHTTP = "http"
)

var labelSets = map[string]pprof.LabelSet{}

func init() {
	for _, c := range []string{ComponentHPALoop, ComponentProjection, ComponentRecorder, ComponentStore,
		ComponentProvisioner, ComponentVirtualWorkload, ComponentInformers, ComponentHTTP} {
		labelSets[c] = pprof.Labels("component", c)
	}
}

// Do runs f with the component pprof label.
func Do(ctx context.Context, component string, f func(context.Context)) {
	labels, ok := labelSets[component]
	if !ok {
		labels = pprof.Labels("component", component)
	}
	pprof.Do(ctx, labels, f)
}

// Go runs f in a new goroutine with the component pprof label.
func Go(ctx context.Context, component string, f func(context.Context)) {
	go Do(ctx, component, f)
}
