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

// Package analysis summarizes stored decisions.
package analysis

import (
	"cmp"
	"iter"
	"math"
	"slices"
	"time"

	"github.com/fedosin/dryrun-hpa/internal/store"
)

// FlapWindow is the window in which a scale in the opposite direction of the
// previous one counts as a flap.
const FlapWindow = 5 * time.Minute

// Summary describes the decisions of one DryrunHPA configuration.
type Summary struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	SpecHash  string    `json:"specHash"`
	Target    string    `json:"target"`
	First     time.Time `json:"first"`
	Last      time.Time `json:"last"`
	Decisions int       `json:"decisions"`
	Errors    int       `json:"errors"`
	ScaleUps  int       `json:"scaleUps"`
	// ScaleDowns counts decisions that lowered the replica count.
	ScaleDowns int `json:"scaleDowns"`
	// Flaps counts rescales that reversed the previous one within FlapWindow.
	Flaps       int     `json:"flaps"`
	MinDesired  int32   `json:"minDesired"`
	MaxDesired  int32   `json:"maxDesired"`
	MeanDesired float64 `json:"meanDesired"`
	// ReplicaHours integrates the virtual replica count over time: the
	// capacity this configuration would have used.
	ReplicaHours float64 `json:"replicaHours"`
	// SourceReplicaHours integrates the real replica count over the same time.
	SourceReplicaHours float64 `json:"sourceReplicaHours"`
	// AtMaxRatio is the fraction of decisions limited by maxReplicas.
	AtMaxRatio float64 `json:"atMaxRatio"`
	// MeanReconcileSeconds is the mean duration of a reconcile.
	MeanReconcileSeconds float64 `json:"meanReconcileSeconds"`
}

type summaryAcc struct {
	Summary
	sumDesired  float64
	atMax       int
	sumDuration time.Duration
	prev        *store.Record
	lastDir     int
	lastScaleAt time.Time
}

func key(r *store.Record) string { return r.Namespace + "/" + r.Name + "/" + r.SpecHash }

// Summarize aggregates records per DryrunHPA and spec. Records must be in time order per object.
func Summarize(records iter.Seq2[store.Record, error]) ([]Summary, error) {
	accs := map[string]*summaryAcc{}
	for r, err := range records {
		if err != nil {
			return nil, err
		}
		a, ok := accs[key(&r)]
		if !ok {
			a = &summaryAcc{Summary: Summary{
				Namespace: r.Namespace, Name: r.Name, SpecHash: r.SpecHash, Target: r.TargetKind + "/" + r.TargetName,
				First: r.Time, MinDesired: math.MaxInt32,
			}}
			accs[key(&r)] = a
		}
		a.Decisions++
		a.Last = r.Time
		a.sumDuration += time.Duration(r.Duration)
		if r.Error != "" {
			a.Errors++
		}
		a.MinDesired = min(a.MinDesired, r.DesiredReplicas)
		a.MaxDesired = max(a.MaxDesired, r.DesiredReplicas)
		a.sumDesired += float64(r.DesiredReplicas)
		if r.DesiredReplicas >= r.MaxReplicas && r.MaxReplicas > 0 {
			a.atMax++
		}
		if r.Rescale {
			dir := 0
			switch {
			case r.DesiredReplicas > r.CurrentReplicas:
				a.ScaleUps++
				dir = 1
			case r.DesiredReplicas < r.CurrentReplicas:
				a.ScaleDowns++
				dir = -1
			}
			if dir != 0 && a.lastDir == -dir && r.Time.Sub(a.lastScaleAt) <= FlapWindow {
				a.Flaps++
			}
			if dir != 0 {
				a.lastDir, a.lastScaleAt = dir, r.Time
			}
		}
		if a.prev != nil {
			hours := r.Time.Sub(a.prev.Time).Hours()
			a.ReplicaHours += float64(a.prev.DesiredReplicas) * hours
			if a.prev.SourceReplicas != nil {
				a.SourceReplicaHours += float64(*a.prev.SourceReplicas) * hours
			}
		}
		prev := r
		a.prev = &prev
	}
	out := make([]Summary, 0, len(accs))
	for _, a := range accs {
		s := a.Summary
		if s.Decisions > 0 {
			s.MeanDesired = a.sumDesired / float64(s.Decisions)
			s.AtMaxRatio = float64(a.atMax) / float64(s.Decisions)
			s.MeanReconcileSeconds = (a.sumDuration / time.Duration(s.Decisions)).Seconds()
		}
		if s.MinDesired == math.MaxInt32 {
			s.MinDesired = 0
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Summary) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name), a.First.Compare(b.First))
	})
	return out, nil
}

// Comparison compares a DryrunHPA with the real HPA and the real replica
// count of its workload.
type Comparison struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	SpecHash  string `json:"specHash"`
	Target    string `json:"target"`
	// Samples is the number of decisions with a real HPA value.
	Samples int `json:"samples"`
	// Matches is how many of them had the same desired replicas.
	Matches     int     `json:"matches"`
	MatchRatio  float64 `json:"matchRatio"`
	MeanAbsDiff float64 `json:"meanAbsDiff"`
	MaxAbsDiff  int32   `json:"maxAbsDiff"`
	// MeanDiff is the mean of dry-run minus real: positive means the dry-run
	// configuration would run more replicas.
	MeanDiff float64 `json:"meanDiff"`
	// SourceSamples counts decisions with a known real replica count.
	SourceSamples int `json:"sourceSamples"`
	// MeanSourceDiff is the mean of dry-run desired minus real replicas.
	MeanSourceDiff float64 `json:"meanSourceDiff"`
}

// Compare compares dry-run decisions with real HPAs and real replica counts.
func Compare(records iter.Seq2[store.Record, error]) ([]Comparison, error) {
	type acc struct {
		Comparison
		sumAbs, sum, sumSource float64
	}
	accs := map[string]*acc{}
	for r, err := range records {
		if err != nil {
			return nil, err
		}
		if r.Error != "" {
			continue
		}
		a, ok := accs[key(&r)]
		if !ok {
			a = &acc{Comparison: Comparison{Namespace: r.Namespace, Name: r.Name, SpecHash: r.SpecHash, Target: r.TargetKind + "/" + r.TargetName}}
			accs[key(&r)] = a
		}
		if r.RealHPADesired != nil {
			a.Samples++
			d := r.DesiredReplicas - *r.RealHPADesired
			if d == 0 {
				a.Matches++
			}
			abs := d
			if abs < 0 {
				abs = -abs
			}
			a.MaxAbsDiff = max(a.MaxAbsDiff, abs)
			a.sumAbs += float64(abs)
			a.sum += float64(d)
		}
		if r.SourceReplicas != nil {
			a.SourceSamples++
			a.sumSource += float64(r.DesiredReplicas - *r.SourceReplicas)
		}
	}
	out := make([]Comparison, 0, len(accs))
	for _, a := range accs {
		c := a.Comparison
		if c.Samples > 0 {
			c.MatchRatio = float64(c.Matches) / float64(c.Samples)
			c.MeanAbsDiff = a.sumAbs / float64(c.Samples)
			c.MeanDiff = a.sum / float64(c.Samples)
		}
		if c.SourceSamples > 0 {
			c.MeanSourceDiff = a.sumSource / float64(c.SourceSamples)
		}
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Comparison) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name), cmp.Compare(a.SpecHash, b.SpecHash))
	})
	return out, nil
}
