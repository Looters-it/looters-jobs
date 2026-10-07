package queue

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// GameEventsChannel is the Postgres channel the api LISTENs on.
const GameEventsChannel = "game_events"

// utcNow is the current time as Prisma stores it: DateTime columns are
// "timestamp without time zone" holding UTC, so times are compared and written
// in UTC explicitly instead of relying on the session time zone.
const utcNow = `(now() AT TIME ZONE 'UTC')`

// PgStore implements Store on the "Job" table (schema owned by the api).
type PgStore struct {
	Pool *pgxpool.Pool
}

// Claim uses SKIP LOCKED so that concurrent workers never take the same job.
func (s PgStore) Claim(ctx context.Context) (*Job, error) {
	const query = `
		UPDATE "Job" SET status = 'RUNNING', "lockedAt" = ` + utcNow + `, attempts = attempts + 1
		WHERE id = (
			SELECT id FROM "Job"
			WHERE status = 'PENDING' AND "runAt" <= ` + utcNow + `
			ORDER BY "runAt"
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, type, payload, attempts`
	var job Job
	err := s.Pool.QueryRow(ctx, query).Scan(&job.ID, &job.Type, &job.Payload, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (s PgStore) Complete(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx,
		`UPDATE "Job" SET status = 'DONE', "lockedAt" = NULL, "lastError" = NULL, "finishedAt" = `+utcNow+` WHERE id = $1`, id)
	return err
}

func (s PgStore) Fail(ctx context.Context, id string, reason string, retryAt *time.Time) error {
	if retryAt == nil {
		_, err := s.Pool.Exec(ctx,
			`UPDATE "Job" SET status = 'FAILED', "lockedAt" = NULL, "lastError" = $2, "finishedAt" = `+utcNow+` WHERE id = $1`, id, reason)
		return err
	}
	_, err := s.Pool.Exec(ctx,
		`UPDATE "Job" SET status = 'PENDING', "lockedAt" = NULL, "lastError" = $2, "runAt" = $3 WHERE id = $1`, id, reason, retryAt.UTC())
	return err
}

func (s PgStore) ReleaseStale(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE "Job" SET status = 'PENDING', "lockedAt" = NULL WHERE status = 'RUNNING' AND "lockedAt" < $1`, cutoff.UTC())
	return tag.RowsAffected(), err
}

// Notification is what the api receives on GameEventsChannel.
type Notification struct {
	MatchID *string `json:"matchId"`
	Kind    string  `json:"kind"`
}

// Querier is satisfied by a pool and by a transaction: notifying inside a
// transaction delivers the message only if it commits.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Notify tells the api that something it should fan out has been committed.
func Notify(ctx context.Context, db Querier, n Notification) error {
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `SELECT pg_notify($1, $2)`, GameEventsChannel, string(body))
	return err
}
