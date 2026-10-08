package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type failure struct {
	reason  string
	retryAt *time.Time
}

// fakeStore hands out the given jobs in order and records what happens to them.
type fakeStore struct {
	pending   []Job
	completed []string
	failed    map[string]failure
	mu        sync.Mutex
	touched   []string
}

func (s *fakeStore) Claim(context.Context) (*Job, error) {
	if len(s.pending) == 0 {
		return nil, nil
	}
	job := s.pending[0]
	s.pending = s.pending[1:]
	return &job, nil
}

func (s *fakeStore) Complete(_ context.Context, id string) error {
	s.completed = append(s.completed, id)
	return nil
}

func (s *fakeStore) Fail(_ context.Context, id, reason string, retryAt *time.Time) error {
	if s.failed == nil {
		s.failed = map[string]failure{}
	}
	s.failed[id] = failure{reason, retryAt}
	return nil
}

func (s *fakeStore) Touch(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, id)
	return nil
}

func (s *fakeStore) ReleaseStale(context.Context, time.Time) (int64, error) { return 0, nil }

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func newWorker(store Store, handlers map[string]Handler) *Worker {
	return &Worker{
		Store:       store,
		Handlers:    handlers,
		MaxAttempts: 3,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:         func() time.Time { return now },
	}
}

func TestBackoff(t *testing.T) {
	cases := map[int]time.Duration{0: 5 * time.Second, 1: 5 * time.Second, 2: 10 * time.Second, 3: 20 * time.Second, 7: 5 * time.Minute, 50: 5 * time.Minute}
	for attempt, want := range cases {
		if got := Backoff(attempt); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

func TestProcessNextEmptyQueue(t *testing.T) {
	worked, err := newWorker(&fakeStore{}, nil).ProcessNext(context.Background())
	if worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
}

func TestProcessNextCompletesJob(t *testing.T) {
	store := &fakeStore{pending: []Job{{ID: "j1", Type: "ok", Attempts: 1}}}
	var seen Job
	worker := newWorker(store, map[string]Handler{"ok": func(_ context.Context, job Job) error {
		seen = job
		return nil
	}})
	if worked, err := worker.ProcessNext(context.Background()); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if seen.ID != "j1" || len(store.completed) != 1 || len(store.failed) != 0 {
		t.Fatalf("job not completed: seen=%+v store=%+v", seen, store)
	}
}

func TestProcessNextRetriesWithBackoff(t *testing.T) {
	store := &fakeStore{pending: []Job{{ID: "j1", Type: "bad", Attempts: 2}}}
	worker := newWorker(store, map[string]Handler{"bad": func(context.Context, Job) error { return errors.New("boom") }})
	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := store.failed["j1"]
	if got.reason != "boom" || got.retryAt == nil || !got.retryAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("unexpected failure record: %+v", got)
	}
}

func TestProcessNextGivesUp(t *testing.T) {
	boom := func(context.Context, Job) error { return errors.New("boom") }
	panics := func(context.Context, Job) error { panic("kaboom") }
	cases := map[string]struct {
		job      Job
		handlers map[string]Handler
		retried  bool
	}{
		"last attempt":     {Job{ID: "j", Type: "bad", Attempts: 3}, map[string]Handler{"bad": boom}, false},
		"unknown type":     {Job{ID: "j", Type: "nope", Attempts: 1}, nil, false},
		"permanent error":  {Job{ID: "j", Type: "bad", Attempts: 1}, map[string]Handler{"bad": func(context.Context, Job) error { return ErrPermanent }}, false},
		"panic is retried": {Job{ID: "j", Type: "bad", Attempts: 1}, map[string]Handler{"bad": panics}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{pending: []Job{tc.job}}
			if _, err := newWorker(store, tc.handlers).ProcessNext(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, ok := store.failed["j"]
			if !ok || (got.retryAt != nil) != tc.retried {
				t.Fatalf("failure record %+v (recorded=%v), want retried=%v", got, ok, tc.retried)
			}
		})
	}
}

func TestLongJobKeepsItsClaim(t *testing.T) {
	store := &fakeStore{pending: []Job{{ID: "long", Type: "slow", Attempts: 1}}}
	worker := newWorker(store, map[string]Handler{"slow": func(context.Context, Job) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	}})
	worker.StaleAfter = 30 * time.Millisecond // renewed every 10ms
	if _, err := worker.ProcessNext(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.touched) < 3 || store.touched[0] != "long" {
		t.Fatalf("claim renewed %d times", len(store.touched))
	}
}
