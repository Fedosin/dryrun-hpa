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

// Command dryrun-hpa-controller runs the Kubernetes HPA algorithm against
// DryrunHorizontalPodAutoscalers without scaling real workloads.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register" // enables --logging-format=json
	"k8s.io/klog/v2"

	"github.com/fedosin/dryrun-hpa/internal/app"
	_ "github.com/fedosin/dryrun-hpa/internal/upstream/features" // registers the HPA feature gates
	"github.com/fedosin/dryrun-hpa/internal/version"
)

func main() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
	o := app.NewOptions()
	fs := pflag.NewFlagSet("dryrun-hpa-controller", pflag.ExitOnError)
	o.AddFlags(fs)
	utilfeature.DefaultMutableFeatureGate.AddFlag(fs)
	showVersion := fs.Bool("version", false, "Print the version and exit.")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("dryrun-hpa-controller %s (Kubernetes %s)\n", version.Version, version.UpstreamKubernetes)
		return
	}
	if err := logsapi.ValidateAndApply(o.Logs, utilfeature.DefaultFeatureGate); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := o.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := klog.Background().WithName("dryrun-hpa-controller")
	ctx = klog.NewContext(ctx, logger)
	if err := app.Run(ctx, o); err != nil {
		logger.Error(err, "Controller failed")
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
	klog.Flush()
}
