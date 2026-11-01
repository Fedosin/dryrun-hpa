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

// Package lifecycle decides how virtual pods appear and disappear when the
// replica count of a VirtualWorkload changes.
package lifecycle

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/fedosin/dryrun-hpa/api/dryrunhpa/v1alpha1"
)

// Pod is the lifecycle state of one virtual pod.
type Pod struct {
	// Ordinal is unique within a VirtualWorkload and never reused.
	Ordinal int64
	// Created is when the pod was created (its start time).
	Created time.Time
	// ReadyAt is when the pod becomes ready.
	ReadyAt time.Time
}

// Ready reports whether the pod is ready at now.
func (p Pod) Ready(now time.Time) bool {
	return !now.Before(p.ReadyAt)
}

// Profile changes the set of virtual pods.
type Profile interface {
	// Type is the profile's VirtualWorkload lifecycleProfile value.
	Type() v1alpha1.LifecycleProfileType
	// ScaleUp returns add new pods. nextOrdinal is the first free ordinal.
	ScaleUp(existing []Pod, add int, nextOrdinal int64, now time.Time, readinessDelay time.Duration) []Pod
	// ScaleDown returns the pods that remain after removing remove pods.
	ScaleDown(existing []Pod, remove int, now time.Time) []Pod
}

// Get returns the profile for a lifecycleProfile value.
func Get(t v1alpha1.LifecycleProfileType) (Profile, error) {
	switch t {
	case "", v1alpha1.LifecycleProfileParallel:
		return Parallel{}, nil
	default:
		return nil, fmt.Errorf("unsupported lifecycle profile %q", t)
	}
}

// Parallel behaves like a ReplicaSet: all new pods start at once, and
// scale-down removes unready pods first, then the newest pods.
type Parallel struct{}

func (Parallel) Type() v1alpha1.LifecycleProfileType {
	return v1alpha1.LifecycleProfileParallel
}

func (Parallel) ScaleUp(_ []Pod, add int, nextOrdinal int64, now time.Time, readinessDelay time.Duration) []Pod {
	out := make([]Pod, add)
	for i := range out {
		out[i] = Pod{Ordinal: nextOrdinal + int64(i), Created: now, ReadyAt: now.Add(readinessDelay)}
	}
	return out
}

func (Parallel) ScaleDown(existing []Pod, remove int, now time.Time) []Pod {
	if remove >= len(existing) {
		return nil
	}
	ranked := slices.Clone(existing)
	// Most deletable first, matching the ReplicaSet controller's ranking for
	// the states virtual pods can be in.
	slices.SortStableFunc(ranked, func(a, b Pod) int {
		if ar, br := a.Ready(now), b.Ready(now); ar != br {
			if !ar {
				return -1
			}
			return 1
		}
		if c := b.Created.Compare(a.Created); c != 0 {
			return c
		}
		return cmp.Compare(b.Ordinal, a.Ordinal)
	})
	keep := ranked[remove:]
	slices.SortFunc(keep, func(a, b Pod) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	return keep
}
