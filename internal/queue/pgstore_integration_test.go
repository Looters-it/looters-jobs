package queue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/handlers"
	"gioco/jobs/internal/queue"
)

// These tests need a database migrated by the api: set TEST_DATABASE_URL (see .env.example).
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DELETE FROM "Job"`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func insertJob(t *testing.T, pool *pgxpool.Pool, id, jobType string, runAt time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO "Job" (id, type, payload, "runAt") VALUES ($1, $2, '{}', $3)`, id, jobType, runAt.UTC())
	if err != nil {
		t.Fatal(err)
	}
}

func jobStatus(t *testing.T, pool *pgxpool.Pool, id string) (status string, attempts int) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT status::text, attempts FROM "Job" WHERE id = $1`, id).Scan(&status, &attempts)
	if err != nil {
		t.Fatal(err)
	}
	return status, attempts
}

func TestClaimIsExclusiveAcrossWorkers(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const total = 40
	for i := range total {
		insertJob(t, pool, fmt.Sprintf("job-%02d", i), "x", time.Now().Add(-time.Minute))
	}

	store := queue.PgStore{Pool: pool}
	var mu sync.Mutex
	claims := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				job, err := store.Claim(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				claims[job.ID]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	if len(claims) != total {
		t.Fatalf("claimed %d distinct jobs, want %d", len(claims), total)
	}
	for id, n := range claims {
		if n != 1 {
			t.Errorf("job %s claimed %d times", id, n)
		}
	}
}

func TestJobLifecycle(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	store := queue.PgStore{Pool: pool}

	insertJob(t, pool, "future", "x", time.Now().Add(time.Hour))
	if job, err := store.Claim(ctx); err != nil || job != nil {
		t.Fatalf("a job scheduled in the future was claimed: %+v, %v", job, err)
	}

	insertJob(t, pool, "due", "x", time.Now().Add(-time.Minute))
	job, err := store.Claim(ctx)
	if err != nil || job == nil || job.ID != "due" || job.Attempts != 1 {
		t.Fatalf("claim: %+v, %v", job, err)
	}

	// A retry puts it back, due later.
	retryAt := time.Now().Add(time.Hour)
	if err := store.Fail(ctx, "due", "boom", &retryAt); err != nil {
		t.Fatal(err)
	}
	if status, _ := jobStatus(t, pool, "due"); status != "PENDING" {
		t.Fatalf("status after retry = %s", status)
	}
	if job, _ := store.Claim(ctx); job != nil {
		t.Fatal("job claimed before its retry time")
	}

	// A crashed worker's job is released and can be claimed again.
	if _, err := pool.Exec(ctx, `UPDATE "Job" SET status = 'RUNNING', "lockedAt" = (now() AT TIME ZONE 'UTC') - interval '1 hour', "runAt" = (now() AT TIME ZONE 'UTC') WHERE id = 'due'`); err != nil {
		t.Fatal(err)
	}
	if released, err := store.ReleaseStale(ctx, time.Now().Add(-time.Minute)); err != nil || released != 1 {
		t.Fatalf("released %d, %v", released, err)
	}
	job, err = store.Claim(ctx)
	if err != nil || job == nil || job.Attempts != 2 {
		t.Fatalf("second claim: %+v, %v", job, err)
	}

	if err := store.Fail(ctx, "due", "gave up", nil); err != nil {
		t.Fatal(err)
	}
	if status, _ := jobStatus(t, pool, "due"); status != "FAILED" {
		t.Fatalf("status after giving up = %s", status)
	}
}

func TestPingRepliesOnGameEventsChannel(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	listener, err := pgx.Connect(ctx, os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	if _, err := listener.Exec(ctx, "LISTEN "+queue.GameEventsChannel); err != nil {
		t.Fatal(err)
	}

	insertJob(t, pool, "ping", handlers.TypePing, time.Now().Add(-time.Second))
	worker := &queue.Worker{Store: queue.PgStore{Pool: pool}, Handlers: handlers.All(pool), MaxAttempts: 1, Log: discardLog()}
	if worked, err := worker.ProcessNext(ctx); !worked || err != nil {
		t.Fatalf("worked=%v err=%v", worked, err)
	}

	message, err := listener.WaitForNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got queue.Notification
	if err := json.Unmarshal([]byte(message.Payload), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != handlers.KindPong || got.MatchID != nil {
		t.Fatalf("unexpected notification: %s", message.Payload)
	}
	if status, _ := jobStatus(t, pool, "ping"); status != "DONE" {
		t.Fatalf("ping job status = %s", status)
	}
}
