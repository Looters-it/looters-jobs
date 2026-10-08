// Package handlers maps job types to the code that runs them.
// Job types and payloads are the contract with the api (docs/ARCHITECTURE.md).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"gioco/jobs/internal/battlequeue"
	"gioco/jobs/internal/geoprocess"
	"gioco/jobs/internal/matchstart"
	"gioco/jobs/internal/queue"
	"gioco/jobs/internal/turnclose"
)

// Job types.
const (
	TypePing       = "system.ping"
	TypeGeoProcess = "geo.process"
	TypeMatchStart = "match.start"
	TypeTurnClose  = "turn.close"
)

// KindPong is the notification sent in reply to a ping.
const KindPong = "system.pong"

// All returns the handlers for every job type this service knows.
func All(db queue.Querier, geo *geoprocess.Processor, starter *matchstart.Starter, closer *turnclose.Closer) map[string]queue.Handler {
	return map[string]queue.Handler{
		TypePing:       Ping(db),
		TypeGeoProcess: GeoProcess(geo),
		TypeMatchStart: MatchStart(starter),
		TypeTurnClose:  TurnClose(closer),
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

// ResolveBattle handles a request from the battle queue (Kafka).
// The battle rules arrive with milestone 7: until then no battle can exist,
// so a request is only logged.
func ResolveBattle(log *slog.Logger) battlequeue.Resolver {
	return func(_ context.Context, message battlequeue.Message) error {
		log.Warn("battle resolution is not implemented yet", "battleId", message.BattleID, "matchId", message.MatchID)
		return nil
	}
}
