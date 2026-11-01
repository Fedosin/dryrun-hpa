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
	"time"

	"k8s.io/klog/v2"

	"github.com/fedosin/dryrun-hpa/internal/observability"
)

// WriterOptions configures a Writer.
type WriterOptions struct {
	// QueueSize is how many records can wait; more are dropped so the HPA
	// loop never blocks on storage.
	QueueSize int
	// BatchSize is the maximum number of records per transaction.
	BatchSize int
	// FlushInterval is the maximum time a record waits for a batch.
	FlushInterval time.Duration
	// Retention is how long records are kept; 0 keeps them forever.
	Retention time.Duration
	// RetentionInterval is how often old records are deleted.
	RetentionInterval time.Duration
}

// Writer writes records to a Store asynchronously in batches.
type Writer struct {
	store Store
	opts  WriterOptions
	ch    chan Record
	done  chan struct{}
}

// NewWriter returns a writer; call Run to start it.
func NewWriter(s Store, o WriterOptions) *Writer {
	if o.QueueSize <= 0 {
		o.QueueSize = 10000
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	if o.FlushInterval <= 0 {
		o.FlushInterval = time.Second
	}
	if o.RetentionInterval <= 0 {
		o.RetentionInterval = 10 * time.Minute
	}
	return &Writer{store: s, opts: o, ch: make(chan Record, o.QueueSize), done: make(chan struct{})}
}

// Store returns the underlying store.
func (w *Writer) Store() Store { return w.store }

// Enqueue queues a record without blocking. It returns false when the queue
// is full and the record was dropped.
func (w *Writer) Enqueue(r Record) bool {
	select {
	case w.ch <- r:
		observability.StoreQueueLength.Set(float64(len(w.ch)))
		return true
	default:
		observability.StoreErrors.WithLabelValues("dropped").Inc()
		return false
	}
}

// Run writes batches until ctx is done, then flushes what is queued.
func (w *Writer) Run(ctx context.Context) {
	defer close(w.done)
	logger := klog.FromContext(ctx).WithName("store")
	ticker := time.NewTicker(w.opts.FlushInterval)
	defer ticker.Stop()
	retention := time.NewTicker(w.opts.RetentionInterval)
	defer retention.Stop()
	w.applyRetention(ctx, logger)

	batch := make([]Record, 0, w.opts.BatchSize)
	flush := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		start := time.Now()
		err := w.store.Write(ctx, batch)
		observability.StoreWriteDuration.Observe(time.Since(start).Seconds())
		observability.StoreBatchSize.Observe(float64(len(batch)))
		if err != nil {
			observability.StoreErrors.WithLabelValues("write").Inc()
			logger.Error(err, "Writing decisions failed", "records", len(batch))
		} else {
			observability.StoreWritten.Add(float64(len(batch)))
		}
		batch = batch[:0]
		observability.StoreQueueLength.Set(float64(len(w.ch)))
	}
	for {
		select {
		case r := <-w.ch:
			batch = append(batch, r)
			if len(batch) >= w.opts.BatchSize {
				flush(ctx)
			}
		case <-ticker.C:
			flush(ctx)
		case <-retention.C:
			w.applyRetention(ctx, logger)
		case <-ctx.Done():
			// Drain with a fresh context so shutdown doesn't lose decisions.
			drainCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for {
				select {
				case r := <-w.ch:
					batch = append(batch, r)
					if len(batch) >= w.opts.BatchSize {
						flush(drainCtx)
					}
				default:
					flush(drainCtx)
					return
				}
			}
		}
	}
}

// Wait blocks until Run has returned.
func (w *Writer) Wait() { <-w.done }

func (w *Writer) applyRetention(ctx context.Context, logger klog.Logger) {
	if w.opts.Retention > 0 {
		n, err := w.store.DeleteBefore(ctx, time.Now().Add(-w.opts.Retention))
		if err != nil {
			observability.StoreErrors.WithLabelValues("retention").Inc()
			logger.Error(err, "Deleting old decisions failed")
		} else if n > 0 {
			observability.StoreRetentionDeleted.Add(float64(n))
			logger.V(2).Info("Deleted old decisions", "records", n)
		}
	}
	if size, err := w.store.SizeBytes(); err == nil {
		observability.StoreSizeBytes.Set(float64(size))
	}
}
