package turnclose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/battle"
	"gioco/jobs/internal/endgame"
	"gioco/jobs/internal/queue"
	"gioco/jobs/internal/rules"
)

// Outcome says what a turn.close job found.
type Outcome string

const (
	Closed Outcome = "closed"
	// Stale: that turn is already closed, or the match is not being played.
	// This is what makes the job repeatable: a second run changes nothing.
	Stale Outcome = "stale"
	// Finished: the turn was closed and it was the last one of the match.
	Finished Outcome = "finished"
)

// Closer handles `turn.close` jobs.
type Closer struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	Now  func() time.Time
	// Battles resolves what is still open when the turn ends.
	Battles *battle.Service
}

func (c *Closer) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// asUTC reads back a Prisma DateTime, stored as UTC in a column without time zone.
func asUTC(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// Schedule queues the closing of a turn at the moment it ends. The dedupe key
// makes it safe to call more than once for the same turn. It runs inside the
// caller's transaction, so the job exists only if that turn was really opened.
func Schedule(ctx context.Context, tx pgx.Tx, matchID string, turn int, endsAt time.Time) error {
	payload, err := json.Marshal(map[string]any{"matchId": matchID, "turn": turn})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO "Job" (id, type, payload, "runAt", "dedupeKey")
		VALUES ('job_' || gen_random_uuid(), 'turn.close', $1::jsonb, $2, $3)
		ON CONFLICT ("dedupeKey") DO NOTHING`,
		string(payload), endsAt.UTC(), fmt.Sprintf("turn.close:%s:%d", matchID, turn))
	return err
}

// Close closes the given turn of a match. Everything happens in one
// transaction that also moves the match to the next turn, and the match row
// is locked first: actions of the players (which hold it shared) finish
// before the turn closes, and none of them straddles two turns.
func (c *Closer) Close(ctx context.Context, matchID string, turn int) (Outcome, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	var (
		status     string
		startAt    time.Time
		maxDays    *int
		current    int
		turnEndsAt *time.Time
		turnHours  float64
		config     []byte
	)
	err = tx.QueryRow(ctx,
		`SELECT status::text, "startAt", "maxDays", "currentTurn", "turnEndsAt", "turnHours", config FROM "Match" WHERE id = $1 FOR UPDATE`, matchID,
	).Scan(&status, &startAt, &maxDays, &current, &turnEndsAt, &turnHours, &config)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stale, nil
	}
	if err != nil {
		return "", err
	}
	if status != "RUNNING" || current != turn || turnEndsAt == nil {
		return Stale, nil
	}
	endsAt := asUTC(*turnEndsAt)
	if endsAt.After(c.now().Add(time.Second)) {
		// Should not happen: the job is scheduled at this very moment. Retried later.
		return "", fmt.Errorf("turn %d of match %s ends at %s: too early to close it", turn, matchID, endsAt)
	}
	matchRules, err := rules.ParseMatchRules(config)
	if err != nil {
		return "", err
	}

	// Battles the defender did not answer are fought now, as a voluntary defence; a
	// battle whose answer never reached the queue is resolved with that answer.
	// A territory conquered here is protected through the turn that is about to
	// begin: protection until the end of the turn that just ended would protect nothing.
	if c.Battles != nil {
		if _, err := c.Battles.ResolveOpenInTx(ctx, tx, matchID, turn+1); err != nil {
			return "", fmt.Errorf("battles: %w", err)
		}
	}

	// A battle may have conquered the last capital but one: the match is then over,
	// and there is no next turn to open.
	if err := tx.QueryRow(ctx, `SELECT status::text FROM "Match" WHERE id = $1`, matchID).Scan(&status); err != nil {
		return "", err
	}
	if status == "FINISHED" {
		if err := snapshot(ctx, tx, matchID, turn); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		return Finished, nil
	}

	// Income comes after the battles, so it goes to whoever holds each territory now.
	if err := creditIncome(ctx, tx, matchID, matchRules); err != nil {
		return "", fmt.Errorf("income: %w", err)
	}

	// The limit of annexations is per turn.
	if _, err := tx.Exec(ctx, `UPDATE "Empire" SET "annexedThisTurn" = 0 WHERE "matchId" = $1`, matchID); err != nil {
		return "", err
	}
	// Protection of a just-acquired territory lasts "until the end of the turn it was
	// acquired in": it is stored as that turn's number, so moving to the next turn ends it.

	if err := endgame.ExpireStructures(ctx, tx, matchID, turn); err != nil {
		return "", fmt.Errorf("structures: %w", err)
	}
	if err := endgame.UpdateAccumulationAlerts(ctx, tx, matchID, matchRules.AccumAlertPct, matchRules.AccumAlertMinTerritories); err != nil {
		return "", fmt.Errorf("accumulation alerts: %w", err)
	}
	if err := snapshot(ctx, tx, matchID, turn); err != nil {
		return "", err
	}

	// A match with a maximum duration ends at the first turn close on or after its deadline.
	if deadline := endgame.Deadline(asUTC(startAt), maxDays); deadline != nil && !endsAt.Before(*deadline) {
		winner, err := endgame.WinnerAtDeadline(ctx, tx, matchID)
		if err != nil {
			return "", err
		}
		if err := endgame.Finish(ctx, tx, matchID, winner, turn); err != nil {
			return "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return "", err
		}
		c.Log.Info("match finished: deadline reached", "matchId", matchID, "turn", turn)
		return Finished, nil
	}

	// The next turn ends one turn length after this one was due, not after
	// this job ran: turns keep closing at the hour the owner chose.
	next := turn + 1
	nextEndsAt := endsAt.Add(time.Duration(turnHours * float64(time.Hour)))
	if _, err := tx.Exec(ctx,
		`UPDATE "Match" SET "currentTurn" = $2, "turnEndsAt" = $3 WHERE id = $1`, matchID, next, nextEndsAt); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "GameEvent" (id, "matchId", turn, type, data)
		VALUES ('ev_' || gen_random_uuid(), $1, $2, 'TURN_CLOSED', '{}')`, matchID, turn); err != nil {
		return "", err
	}
	if err := Schedule(ctx, tx, matchID, next, nextEndsAt); err != nil {
		return "", err
	}
	// Sent only if the transaction commits.
	if err := queue.Notify(ctx, tx, queue.Notification{MatchID: &matchID, Kind: "turn.closed"}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	c.Log.Info("turn closed", "matchId", matchID, "turn", turn, "nextEndsAt", nextEndsAt)
	return Closed, nil
}

// creditIncome gives every active empire its income, split among its
// territories by the empire's rule.
func creditIncome(ctx context.Context, tx pgx.Tx, matchID string, matchRules rules.MatchRules) error {
	type empire struct {
		mode, capital string
		units         int
		shares        []Share
	}
	empires := map[string]*empire{}
	var order []string // deterministic order of updates

	rows, err := tx.Query(ctx, `
		SELECT e.id, e."allocationMode"::text, coalesce(e."capitalTerritoryId", ''), t.id, t.weight,
			(SELECT count(*) FROM "TerritoryUnit" tu WHERE tu."territoryId" = t.id)
		FROM "Empire" e JOIN "Territory" t ON t."ownerEmpireId" = e.id
		WHERE e."matchId" = $1 AND e.status = 'ACTIVE'
		ORDER BY e.id, t.id`, matchID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, mode, capital, territoryID string
		var weight, units int
		if err := rows.Scan(&id, &mode, &capital, &territoryID, &weight, &units); err != nil {
			rows.Close()
			return err
		}
		e, ok := empires[id]
		if !ok {
			e = &empire{mode: mode, capital: capital}
			empires[id] = e
			order = append(order, id)
		}
		e.units += units
		e.shares = append(e.shares, Share{TerritoryID: territoryID, Weight: weight})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	batch := &pgx.Batch{}
	for _, id := range order {
		e := empires[id]
		income := Income(matchRules.IncomeBase, matchRules.IncomePerUnit, e.units)
		amounts := Distribute(income, e.mode, e.shares, e.capital)
		for _, share := range e.shares {
			if amount := amounts[share.TerritoryID]; amount > 0 {
				batch.Queue(`UPDATE "Territory" SET resources = resources + $2 WHERE id = $1`, share.TerritoryID, amount)
			}
		}
	}
	if batch.Len() == 0 {
		return nil
	}
	return tx.SendBatch(ctx, batch).Close()
}

// snapshot saves who owns each municipality at the end of a turn, for the history.
func snapshot(ctx context.Context, tx pgx.Tx, matchID string, turn int) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO "TurnSnapshot" ("matchId", turn, ownership)
		SELECT $1, $2, coalesce(jsonb_object_agg(tu."geoUnitId"::text, t."ownerEmpireId"), '{}'::jsonb)
		FROM "Territory" t JOIN "TerritoryUnit" tu ON tu."territoryId" = t.id
		WHERE t."matchId" = $1 AND t."ownerEmpireId" IS NOT NULL
		ON CONFLICT DO NOTHING`, matchID, turn)
	return err
}
