// Package matchstart moves a match from open registrations to running when
// its start time comes.
package matchstart

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/queue"
	"gioco/jobs/internal/rules"
)

// Outcome says what a start job found.
type Outcome string

const (
	Started Outcome = "started"
	// NotOpen: the match is a draft, already running, finished or deleted.
	NotOpen Outcome = "not open"
	// Rescheduled: the owner moved the start; another job will come at the new time.
	Rescheduled Outcome = "rescheduled"
	// TooFewPlayers: the match stays open until the owner sets a new date.
	TooFewPlayers Outcome = "too few players"
)

// Starter handles `match.start` jobs.
type Starter struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	Now  func() time.Time
}

func (s *Starter) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Start starts the match if its time has come. It is idempotent: every
// change happens in one transaction that also moves the status, so running
// the job twice starts the match once.
func (s *Starter) Start(ctx context.Context, matchID string) (Outcome, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var (
		status    string
		startAt   time.Time
		turnHours float64
		config    []byte
	)
	err = tx.QueryRow(ctx,
		`SELECT status::text, "startAt", "turnHours", config FROM "Match" WHERE id = $1 FOR UPDATE`, matchID,
	).Scan(&status, &startAt, &turnHours, &config)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "OPEN") {
		return NotOpen, nil
	}
	if err != nil {
		return "", err
	}
	// Prisma stores UTC in a column without time zone: read it back as UTC.
	startAt = time.Date(startAt.Year(), startAt.Month(), startAt.Day(),
		startAt.Hour(), startAt.Minute(), startAt.Second(), startAt.Nanosecond(), time.UTC)
	if startAt.After(s.now().Add(time.Second)) {
		return Rescheduled, nil
	}

	matchRules, err := rules.ParseMatchRules(config)
	if err != nil {
		return "", err
	}
	var players int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM "Empire" WHERE "matchId" = $1 AND status = 'ACTIVE'`, matchID).Scan(&players); err != nil {
		return "", err
	}
	if players < matchRules.MinPlayersToStart {
		s.Log.Info("match not started: too few players", "matchId", matchID, "players", players, "needed", matchRules.MinPlayersToStart)
		return TooFewPlayers, nil
	}

	// Turns are counted from the scheduled start, not from when this job ran,
	// so they close at the hour the owner chose.
	turnEndsAt := startAt.Add(time.Duration(turnHours * float64(time.Hour)))
	if _, err := tx.Exec(ctx,
		`UPDATE "Match" SET status = 'RUNNING', "currentTurn" = 1, "turnEndsAt" = $2 WHERE id = $1`,
		matchID, turnEndsAt); err != nil {
		return "", err
	}
	// Capitals chosen before the start were acquired "in turn one": protected until it ends.
	if _, err := tx.Exec(ctx,
		`UPDATE "Territory" SET "protectedUntilTurn" = 1 WHERE "matchId" = $1 AND "ownerEmpireId" IS NOT NULL`,
		matchID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO "GameEvent" (id, "matchId", turn, type, data) VALUES ('ev_' || gen_random_uuid(), $1, 1, 'MATCH_STARTED', '{}')`,
		matchID); err != nil {
		return "", err
	}
	// Sent only if the transaction commits.
	if err := queue.Notify(ctx, tx, queue.Notification{MatchID: &matchID, Kind: "match.started"}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	s.Log.Info("match started", "matchId", matchID, "players", players, "turnEndsAt", turnEndsAt)
	return Started, nil
}
