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

// Package store persists the decisions of DryrunHPAs.
package store

import (
	"context"
	"encoding/json"
	"iter"
	"time"
)

// Record is one reconcile of a DryrunHPA.
type Record struct {
	ID        int64     `json:"id"`
	Time      time.Time `json:"time"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       string    `json:"uid"`
	// SpecHash identifies the effective spec and simulation settings.
	SpecHash   string `json:"specHash"`
	TargetKind string `json:"targetKind"`
	TargetName string `json:"targetName"`

	MinReplicas int32 `json:"minReplicas"`
	MaxReplicas int32 `json:"maxReplicas"`
	// CurrentReplicas is the virtual replica count before the decision.
	CurrentReplicas int32 `json:"currentReplicas"`
	// MetricReplicas is the raw recommendation of the metrics.
	MetricReplicas *int32 `json:"metricReplicas,omitempty"`
	// StabilizedReplicas is the recommendation after stabilization.
	StabilizedReplicas *int32 `json:"stabilizedReplicas,omitempty"`
	// DesiredReplicas is the final decision after behavior and min/max.
	DesiredReplicas int32  `json:"desiredReplicas"`
	MetricName      string `json:"metricName,omitempty"`
	Rescale         bool   `json:"rescale"`
	RescaleReason   string `json:"rescaleReason,omitempty"`

	// SourceReplicas is spec.replicas of the real workload.
	SourceReplicas *int32 `json:"sourceReplicas,omitempty"`
	// RealHPADesired is status.desiredReplicas of a real HPA on the same workload.
	RealHPADesired *int32 `json:"realHPADesired,omitempty"`

	Metrics    []Metric    `json:"metrics,omitempty"`
	Conditions []Condition `json:"conditions,omitempty"`
	Error      string      `json:"error,omitempty"`
	Duration   Duration    `json:"duration"`

	// Spec is the effective spec as JSON; it is stored once per SpecHash
	// and not returned by queries.
	Spec json.RawMessage `json:"-"`
}

// Metric is the evaluation of one entry of spec.metrics.
type Metric struct {
	Index     int    `json:"index"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Container string `json:"container,omitempty"`
	// TargetType is Utilization, AverageValue or Value.
	TargetType  string `json:"targetType"`
	TargetValue string `json:"targetValue"`
	// Current is the observed value in the unit of the target.
	Current          string `json:"current,omitempty"`
	ProposedReplicas *int32 `json:"proposedReplicas,omitempty"`
	Error            string `json:"error,omitempty"`
}

// Condition is an HPA status condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// Duration marshals as seconds.
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).Seconds())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s float64
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*d = Duration(s * float64(time.Second))
	return nil
}

// Filter selects records. Zero fields don't filter.
type Filter struct {
	Namespace string
	Name      string
	SpecHash  string
	Since     time.Time
	Until     time.Time
	// OnlyRescales returns only decisions that changed the replica count.
	OnlyRescales bool
	// AfterID returns records with a larger ID, for incremental export.
	AfterID int64
	Limit   int
}

// SpecVersion is a spec a DryrunHPA had while making decisions.
type SpecVersion struct {
	SpecHash  string          `json:"specHash"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	FirstSeen time.Time       `json:"firstSeen"`
	Spec      json.RawMessage `json:"spec"`
}

// Store persists decisions.
type Store interface {
	// Write stores a batch of records atomically.
	Write(ctx context.Context, records []Record) error
	// Query returns records in ID order.
	Query(ctx context.Context, f Filter) iter.Seq2[Record, error]
	// Specs returns the spec versions of the matching DryrunHPAs.
	Specs(ctx context.Context, namespace, name string) ([]SpecVersion, error)
	// DeleteBefore removes records older than t and returns how many.
	DeleteBefore(ctx context.Context, t time.Time) (int64, error)
	// SizeBytes returns the storage size.
	SizeBytes() (int64, error)
	// Name is the store type, used as a metric label.
	Name() string
	Close() error
}

// None discards everything.
type None struct{}

var _ Store = None{}

// Write implements Store.
func (None) Write(context.Context, []Record) error { return nil }

// Query implements Store.
func (None) Query(context.Context, Filter) iter.Seq2[Record, error] {
	return func(func(Record, error) bool) {}
}

// Specs implements Store.
func (None) Specs(context.Context, string, string) ([]SpecVersion, error) {
	return []SpecVersion{}, nil
}

// DeleteBefore implements Store.
func (None) DeleteBefore(context.Context, time.Time) (int64, error) { return 0, nil }

// SizeBytes implements Store.
func (None) SizeBytes() (int64, error) { return 0, nil }

// Name implements Store.
func (None) Name() string { return "none" }

// Close implements Store.
func (None) Close() error { return nil }
