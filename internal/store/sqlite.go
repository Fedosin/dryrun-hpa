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

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schemaVersion = 1

var schema = []string{
	`CREATE TABLE IF NOT EXISTS decisions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		uid TEXT NOT NULL,
		spec_hash TEXT NOT NULL,
		target_kind TEXT NOT NULL,
		target_name TEXT NOT NULL,
		min_replicas INTEGER NOT NULL,
		max_replicas INTEGER NOT NULL,
		current_replicas INTEGER NOT NULL,
		metric_replicas INTEGER,
		stabilized_replicas INTEGER,
		desired_replicas INTEGER NOT NULL,
		metric_name TEXT NOT NULL,
		rescale INTEGER NOT NULL,
		rescale_reason TEXT NOT NULL,
		source_replicas INTEGER,
		real_hpa_desired INTEGER,
		metrics TEXT NOT NULL,
		conditions TEXT NOT NULL,
		error TEXT NOT NULL,
		duration_ns INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS decisions_object_ts ON decisions (namespace, name, ts)`,
	`CREATE INDEX IF NOT EXISTS decisions_ts ON decisions (ts)`,
	`CREATE TABLE IF NOT EXISTS specs (
		spec_hash TEXT NOT NULL,
		namespace TEXT NOT NULL,
		name TEXT NOT NULL,
		first_seen INTEGER NOT NULL,
		spec TEXT NOT NULL,
		PRIMARY KEY (namespace, name, spec_hash)
	)`,
}

// SQLite stores decisions in an SQLite database in WAL mode.
type SQLite struct {
	path string
	db   *sql.DB
}

var _ Store = &SQLite{}

// OpenSQLite opens or creates the database at path.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	q := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(10000)", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One writer at a time; readers (export API) use the other connections.
	db.SetMaxOpenConns(4)
	s := &SQLite{path: path, db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if v > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", v, schemaVersion)
	}
	for _, stmt := range schema {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("creating schema: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
	return err
}

// Name implements Store.
func (s *SQLite) Name() string { return "sqlite" }

// Close implements Store.
func (s *SQLite) Close() error { return s.db.Close() }

// Write implements Store.
func (s *SQLite) Write(ctx context.Context, records []Record) (retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO decisions (ts, namespace, name, uid, spec_hash, target_kind, target_name,
		min_replicas, max_replicas, current_replicas, metric_replicas, stabilized_replicas, desired_replicas, metric_name,
		rescale, rescale_reason, source_replicas, real_hpa_desired, metrics, conditions, error, duration_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	spec, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO specs (spec_hash, namespace, name, first_seen, spec) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer spec.Close()

	for i := range records {
		r := &records[i]
		metrics, err := json.Marshal(nonNil(r.Metrics))
		if err != nil {
			return err
		}
		conditions, err := json.Marshal(nonNil(r.Conditions))
		if err != nil {
			return err
		}
		if _, err := ins.ExecContext(ctx, r.Time.UnixNano(), r.Namespace, r.Name, r.UID, r.SpecHash, r.TargetKind, r.TargetName,
			r.MinReplicas, r.MaxReplicas, r.CurrentReplicas, nullInt(r.MetricReplicas), nullInt(r.StabilizedReplicas), r.DesiredReplicas,
			r.MetricName, r.Rescale, r.RescaleReason, nullInt(r.SourceReplicas), nullInt(r.RealHPADesired),
			string(metrics), string(conditions), r.Error, int64(r.Duration)); err != nil {
			return err
		}
		if len(r.Spec) > 0 {
			if _, err := spec.ExecContext(ctx, r.SpecHash, r.Namespace, r.Name, r.Time.UnixNano(), string(r.Spec)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func nullInt(p *int32) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: *p, Valid: true}
}

func intPtr(n sql.NullInt32) *int32 {
	if !n.Valid {
		return nil
	}
	v := n.Int32
	return &v
}

func where(f Filter) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, arg any) {
		conds = append(conds, cond)
		args = append(args, arg)
	}
	if f.Namespace != "" {
		add("namespace = ?", f.Namespace)
	}
	if f.Name != "" {
		add("name = ?", f.Name)
	}
	if f.SpecHash != "" {
		add("spec_hash = ?", f.SpecHash)
	}
	if !f.Since.IsZero() {
		add("ts >= ?", f.Since.UnixNano())
	}
	if !f.Until.IsZero() {
		add("ts < ?", f.Until.UnixNano())
	}
	if f.AfterID > 0 {
		add("id > ?", f.AfterID)
	}
	if f.OnlyRescales {
		conds = append(conds, "rescale = 1")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Query implements Store.
func (s *SQLite) Query(ctx context.Context, f Filter) iter.Seq2[Record, error] {
	return func(yield func(Record, error) bool) {
		w, args := where(f)
		q := `SELECT id, ts, namespace, name, uid, spec_hash, target_kind, target_name, min_replicas, max_replicas,
			current_replicas, metric_replicas, stabilized_replicas, desired_replicas, metric_name, rescale, rescale_reason,
			source_replicas, real_hpa_desired, metrics, conditions, error, duration_ns FROM decisions` + w + ` ORDER BY id`
		if f.Limit > 0 {
			q += fmt.Sprintf(" LIMIT %d", f.Limit)
		}
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			yield(Record{}, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var r Record
			var ts, dur int64
			var metricReplicas, stabilized, sourceReplicas, realDesired sql.NullInt32
			var metrics, conditions string
			if err := rows.Scan(&r.ID, &ts, &r.Namespace, &r.Name, &r.UID, &r.SpecHash, &r.TargetKind, &r.TargetName,
				&r.MinReplicas, &r.MaxReplicas, &r.CurrentReplicas, &metricReplicas, &stabilized, &r.DesiredReplicas,
				&r.MetricName, &r.Rescale, &r.RescaleReason, &sourceReplicas, &realDesired, &metrics, &conditions,
				&r.Error, &dur); err != nil {
				yield(Record{}, err)
				return
			}
			r.Time = time.Unix(0, ts).UTC()
			r.Duration = Duration(dur)
			r.MetricReplicas = intPtr(metricReplicas)
			r.StabilizedReplicas = intPtr(stabilized)
			r.SourceReplicas = intPtr(sourceReplicas)
			r.RealHPADesired = intPtr(realDesired)
			if err := json.Unmarshal([]byte(metrics), &r.Metrics); err != nil {
				yield(Record{}, fmt.Errorf("record %d: %w", r.ID, err))
				return
			}
			if err := json.Unmarshal([]byte(conditions), &r.Conditions); err != nil {
				yield(Record{}, fmt.Errorf("record %d: %w", r.ID, err))
				return
			}
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(Record{}, err)
		}
	}
}

// Specs implements Store.
func (s *SQLite) Specs(ctx context.Context, namespace, name string) ([]SpecVersion, error) {
	w, args := where(Filter{Namespace: namespace, Name: name})
	rows, err := s.db.QueryContext(ctx, `SELECT spec_hash, namespace, name, first_seen, spec FROM specs`+w+` ORDER BY first_seen`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpecVersion
	for rows.Next() {
		var v SpecVersion
		var ts int64
		var spec string
		if err := rows.Scan(&v.SpecHash, &v.Namespace, &v.Name, &ts, &spec); err != nil {
			return nil, err
		}
		v.FirstSeen = time.Unix(0, ts).UTC()
		v.Spec = json.RawMessage(spec)
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteBefore implements Store. Spec versions without decisions are removed too.
func (s *SQLite) DeleteBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM decisions WHERE ts < ?`, t.UnixNano())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM specs WHERE first_seen < ? AND NOT EXISTS (
			SELECT 1 FROM decisions d WHERE d.namespace = specs.namespace AND d.name = specs.name AND d.spec_hash = specs.spec_hash)`,
			t.UnixNano()); err != nil {
			return n, err
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return n, err
		}
	}
	return n, nil
}

// SizeBytes implements Store: the database plus its WAL.
func (s *SQLite) SizeBytes() (int64, error) {
	var total int64
	for _, p := range []string{s.path, s.path + "-wal"} {
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += fi.Size()
	}
	return total, nil
}
