// Package handlers maps job types to the code that runs them.
// Job types and payloads are the contract with the api (docs/ARCHITECTURE.md).
package handlers

import (
	"context"
	"log/slog"

	"gioco/jobs/internal/battlequeue"
	"gioco/jobs/internal/queue"
)

// Job types. geo.process, match.start and turn.close arrive with later milestones.
const (
	TypePing = "system.ping"
)

// KindPong is the notification sent in reply to a ping.
const KindPong = "system.pong"

// All returns the handlers for every job type this service knows.
func All(db queue.Querier) map[string]queue.Handler {
	return map[string]queue.Handler{
		TypePing: Ping(db),
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
