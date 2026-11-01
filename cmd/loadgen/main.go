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

// Command loadgen drives the Goal 1 performance tests: it creates many
// Deployments with DryrunHPAs on simulated (kwok) nodes, samples the
// controller's resource use, and reports how it scales.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	dryrunclientset "github.com/fedosin/dryrun-hpa/pkg/generated/clientset/versioned"
)

type clients struct {
	kubeconfig string
	qps        float32
	burst      int
}

func (c *clients) config() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.kubeconfig != "" {
		rules.ExplicitPath = c.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst = c.qps, c.burst
	return cfg, nil
}

func (c *clients) kube() (kubernetes.Interface, dryrunclientset.Interface, error) {
	cfg, err := c.config()
	if err != nil {
		return nil, nil, err
	}
	k, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	d, err := dryrunclientset.NewForConfig(cfg)
	return k, d, err
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	var c clients
	root := &cobra.Command{
		Use:          "loadgen",
		Short:        "Generate DryrunHPA load and measure the controller",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&c.kubeconfig, "kubeconfig", "", "Path to a kubeconfig.")
	root.PersistentFlags().Float32Var(&c.qps, "qps", 100, "Client QPS.")
	root.PersistentFlags().IntVar(&c.burst, "burst", 200, "Client burst.")
	root.AddCommand(newApplyCommand(&c), newDeleteCommand(&c), newSampleCommand(&c), newReportCommand())
	if err := root.ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}
