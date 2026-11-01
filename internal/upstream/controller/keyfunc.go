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

// Package controller provides the subset of k8s.io/kubernetes/pkg/controller
// that the copied HPA controller uses.
package controller

import (
	"time"

	"k8s.io/client-go/tools/cache"
)

// KeyFunc is the same key function kube-controller-manager uses.
var KeyFunc = cache.DeletionHandlingMetaNamespaceKeyFunc

// NoResyncPeriodFunc returns 0 for resyncPeriod in case resyncing is not needed.
func NoResyncPeriodFunc() time.Duration {
	return 0
}
