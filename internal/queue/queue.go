// Package queue consumes the Job table written by the api.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Job is one claimed row of the queue.
type Job struct {
	ID       string
	Type     string
	Payload  json.RawMessage
	Attempts int // including the current one
}

// Handler runs a job. It must be idempotent: a job can run more than once.
type Handler func(ctx context.Context, job Job) error

// Store is the persistence the worker needs.
type Store interface {
	// Claim locks and returns the next due job, or nil when none is due.
	Claim(ctx context.Context) (*Job, error)
	Complete(ctx context.Context, id string) error
	// Fail records the error; a nil retryAt marks the job as permanently failed.
	Fail(ctx context.Context, id string, reason string, retryAt *time.Time) error
	// Touch renews the claim on a job that is still running.
	Touch(ctx context.Context, id string) error
	// ReleaseStale returns to the queue jobs claimed before the cutoff and never finished.
	ReleaseStale(ctx context.Context, cutoff time.Time) (int64, error)
}

// ErrPermanent wraps errors that retrying cannot fix.
var ErrPermanent = errors.New("permanent failure")

// Worker polls the store and dispatches jobs to handlers by type.
type Worker struct {
	Store        Store
	Handlers     map[string]Handler
	Workers      int
	MaxAttempts  int
	PollInterval time.Duration
	StaleAfter   time.Duration
	Log          *slog.Logger
	Now          func() time.Time
}

// Backoff is the delay before retrying after the given failed attempt (1-based):
// 5s, 10s, 20s, ... capped at 5 minutes.
func Backoff(attempt int) time.Duration {
	const base, limit = 5 * time.Second, 5 * time.Minute
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for i := 1; i < attempt && delay < limit; i++ {
		delay *= 2
	}
	return min(delay, limit)
}

// Run processes jobs until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range max(w.Workers, 1) {
		wg.Go(func() { w.loop(ctx) })
	}
	wg.Go(func() { w.reapLoop(ctx) })
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		worked, err := w.ProcessNext(ctx)
		if err != nil && ctx.Err() == nil {
			w.Log.Error("queue error", "err", err)
		}
		if worked && err == nil {
			continue // drain the queue without waiting
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.PollInterval):
		}
	}
}

func (w *Worker) reapLoop(ctx context.Context) {
	for {
		released, err := w.Store.ReleaseStale(ctx, w.now().Add(-w.StaleAfter))
		if err != nil && ctx.Err() == nil {
			w.Log.Error("release stale jobs", "err", err)
		} else if released > 0 {
			w.Log.Warn("released stale jobs", "count", released)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.StaleAfter / 2):
		}
	}
}

// ProcessNext claims and runs at most one job. It reports whether a job was found.
func (w *Worker) ProcessNext(ctx context.Context) (bool, error) {
	job, err := w.Store.Claim(ctx)
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if job == nil {
		return false, nil
	}

	runErr := w.run(ctx, *job)
	if runErr == nil {
		w.Log.Info("job done", "id", job.ID, "type", job.Type)
		return true, w.Store.Complete(ctx, job.ID)
	}

	var retryAt *time.Time
	if !errors.Is(runErr, ErrPermanent) && job.Attempts < w.MaxAttempts {
		at := w.now().Add(Backoff(job.Attempts))
		retryAt = &at
	}
	w.Log.Error("job failed", "id", job.ID, "type", job.Type, "attempt", job.Attempts, "retry", retryAt != nil, "err", runErr)
	return true, w.Store.Fail(ctx, job.ID, runErr.Error(), retryAt)
}

func (w *Worker) run(ctx context.Context, job Job) (err error) {
	handler, ok := w.Handlers[job.Type]
	if !ok {
		return fmt.Errorf("%w: no handler for job type %q", ErrPermanent, job.Type)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("handler panic: %v", r)
		}
	}()

	// Long jobs (a whole country of boundaries) outlive StaleAfter: renew the
	// claim while the handler runs, so the job is not handed to another worker.
	if w.StaleAfter > 0 {
		done := make(chan struct{})
		defer close(done)
		go func() {
			ticker := time.NewTicker(w.StaleAfter / 3)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := w.Store.Touch(ctx, job.ID); err != nil && ctx.Err() == nil {
						w.Log.Error("renew job claim", "id", job.ID, "err", err)
					}
				}
			}
		}()
	}
	return handler(ctx, job)
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}
