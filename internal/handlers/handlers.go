// Package handlers maps job types to the code that runs them.
// Job types and payloads are the contract with the api (docs/ARCHITECTURE.md).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"looters/jobs/internal/battle"
	"looters/jobs/internal/battlequeue"
	"looters/jobs/internal/endgame"
	"looters/jobs/internal/geoprocess"
	"looters/jobs/internal/matchstart"
	"looters/jobs/internal/queue"
	"looters/jobs/internal/turnclose"
)

// Job types.
const (
	TypePing          = "system.ping"
	TypeGeoProcess    = "geo.process"
	TypeMatchStart    = "match.start"
	TypeTurnClose     = "turn.close"
	TypeEmpireAbandon = "empire.abandon"
)

// KindPong is the notification sent in reply to a ping.
const KindPong = "system.pong"

// All returns the handlers for every job type this service knows.
func All(db Database, geo *geoprocess.Processor, starter *matchstart.Starter, closer *turnclose.Closer) map[string]queue.Handler {
	return map[string]queue.Handler{
		TypePing:          Ping(db),
		TypeGeoProcess:    GeoProcess(geo),
		TypeMatchStart:    MatchStart(starter),
		TypeTurnClose:     TurnClose(closer),
		TypeEmpireAbandon: EmpireAbandon(db),
	}
}

// Database is what the handlers need of the pool: to notify, and to open a transaction.
type Database interface {
	queue.Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// EmpireAbandon takes out of a running match the empire of a player who leaves it.
func EmpireAbandon(db Database) queue.Handler {
	return func(ctx context.Context, job queue.Job) error {
		var payload struct {
			MatchID  string `json:"matchId"`
			EmpireID string `json:"empireId"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.MatchID == "" || payload.EmpireID == "" {
			return fmt.Errorf("%w: invalid empire.abandon payload %s", queue.ErrPermanent, job.Payload)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(context.WithoutCancel(ctx))
		if _, err := endgame.Abandon(ctx, tx, payload.MatchID, payload.EmpireID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
}

// TurnClose closes a turn that has reached its end.
func TurnClose(closer *turnclose.Closer) queue.Handler {
	return func(ctx context.Context, job queue.Job) error {
		var payload struct {
			MatchID string `json:"matchId"`
			Turn    int    `json:"turn"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.MatchID == "" || payload.Turn < 1 {
			return fmt.Errorf("%w: invalid turn.close payload %s", queue.ErrPermanent, job.Payload)
		}
		_, err := closer.Close(ctx, payload.MatchID, payload.Turn)
		return err
	}
}

// MatchStart starts a match whose start time has come.
func MatchStart(starter *matchstart.Starter) queue.Handler {
	return func(ctx context.Context, job queue.Job) error {
		var payload struct {
			MatchID string `json:"matchId"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.MatchID == "" {
			return fmt.Errorf("%w: invalid match.start payload %s", queue.ErrPermanent, job.Payload)
		}
		_, err := starter.Start(ctx, payload.MatchID)
		return err
	}
}

// GeoProcess validates an uploaded file and stores its municipalities,
// their display geometry and their adjacency.
func GeoProcess(processor *geoprocess.Processor) queue.Handler {
	return func(ctx context.Context, job queue.Job) error {
		var payload struct {
			UploadID string `json:"uploadId"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.UploadID == "" {
			return fmt.Errorf("%w: invalid geo.process payload %s", queue.ErrPermanent, job.Payload)
		}
		return processor.Process(ctx, payload.UploadID)
	}
}

// Ping answers the api's startup ping, proving the queue works end to end.
func Ping(db queue.Querier) queue.Handler {
	return func(ctx context.Context, _ queue.Job) error {
		return queue.Notify(ctx, db, queue.Notification{Kind: KindPong})
	}
}

// ResolveBattle handles a request from the battle queue (Kafka): the defender
// has answered, and the battle is resolved at once.
func ResolveBattle(battles *battle.Service) battlequeue.Resolver {
	return func(ctx context.Context, message battlequeue.Message) error {
		_, err := battles.Resolve(ctx, message.BattleID, message.MatchID)
		return err
	}
}
