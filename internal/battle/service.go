package battle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"looters/jobs/internal/endgame"
	"looters/jobs/internal/queue"
	"looters/jobs/internal/rules"
)

// Outcomes, as stored in Battle.outcome.
const (
	OutcomeAttackerWon = "ATTACKER_WON"
	OutcomeDefenderWon = "DEFENDER_WON"
	OutcomeSurrender   = "SURRENDER"
)

// Service resolves battles.
type Service struct {
	Pool     *pgxpool.Pool
	Log      *slog.Logger
	Resolver Resolver
	Roll     Roller
}

// NewService returns a Service with the current formula and the secure generator.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Log: log, Resolver: ProductResolver{}, Roll: SecureRoll}
}

// Resolve resolves a battle the defender has answered. It is what the battle
// queue calls. A battle without an answer is left alone: it waits for the
// turn to close. Delivery is at-least-once, so a battle already resolved is
// simply skipped.
func (s *Service) Resolve(ctx context.Context, battleID, matchID string) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	// Held shared, like the actions of the players: the turn cannot close halfway through.
	var status string
	var turn int
	err = tx.QueryRow(ctx,
		`SELECT status::text, "currentTurn" FROM "Match" WHERE id = $1 FOR SHARE`, matchID).Scan(&status, &turn)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "RUNNING") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Acquired now: protected until the end of the current turn.
	resolved, err := s.ResolveInTx(ctx, tx, battleID, false, turn)
	if err != nil || !resolved {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ResolveInTx resolves one battle inside the caller's transaction.
//
// silenceIsDefense is true when the turn is closing: a battle the defender
// never answered is then fought as a voluntary defence, never as a surrender
// (PROMPT §2.5). A recorded answer is always honoured.
// protectedUntilTurn is the turn through which a conquered territory is protected.
func (s *Service) ResolveInTx(ctx context.Context, tx pgx.Tx, battleID string, silenceIsDefense bool, protectedUntilTurn int) (bool, error) {
	var (
		status, matchID, attackerID, defenderID, targetID string
		response                                          *string
		attackForce                                       int
		defenseForce                                      *int
	)
	err := tx.QueryRow(ctx, `
		SELECT status::text, response::text, "matchId", "attackerEmpireId", "defenderEmpireId",
			"targetTerritoryId", "attackForce", "defenseForce"
		FROM "Battle" WHERE id = $1 FOR UPDATE`, battleID,
	).Scan(&status, &response, &matchID, &attackerID, &defenderID, &targetID, &attackForce, &defenseForce)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "OPEN" {
		return false, nil // already resolved: nothing is applied twice
	}
	answer := "DEFEND"
	if response != nil {
		answer = *response
	} else if !silenceIsDefense {
		return false, nil
	}

	var config []byte
	if err := tx.QueryRow(ctx, `SELECT config FROM "Match" WHERE id = $1`, matchID).Scan(&config); err != nil {
		return false, err
	}
	matchRules, err := rules.ParseMatchRules(config)
	if err != nil {
		return false, err
	}

	var (
		outcome      string
		result       *Result
		defenseCost  *int
		attackerWins bool
	)
	if answer == "SURRENDER" {
		// The territory is given up with what is on it; the attacker gets back part of the cost.
		outcome, attackerWins = OutcomeSurrender, true
		if _, err := tx.Exec(ctx, `
			UPDATE "Territory" t SET resources = t.resources + floor(src.cost * $2::numeric / 100)::int
			FROM "BattleSource" src WHERE src."battleId" = $1 AND t.id = src."territoryId"`,
			battleID, matchRules.SurrenderRefundPct); err != nil {
			return false, err
		}
	} else {
		// Strength was fixed at the declaration, before any cost. Defending costs a share
		// of what is on the attacked territory, whoever wins.
		force := 0
		if defenseForce != nil {
			force = *defenseForce
		}
		cost := int(math.Ceil(float64(force) * matchRules.DefensePct / 100))
		defenseCost = &cost
		if _, err := tx.Exec(ctx,
			`UPDATE "Territory" SET resources = GREATEST(resources - $2, 0) WHERE id = $1`, targetID, cost); err != nil {
			return false, err
		}
		fought := s.Resolver.Resolve(attackForce, force, s.Roll)
		result = &fought
		attackerWins = fought.AttackerWins
		outcome = OutcomeDefenderWon
		if attackerWins {
			outcome = OutcomeAttackerWon
		}
	}

	if attackerWins {
		// The winner takes the territory with whatever is left on it. Just acquired, it
		// can receive resources at once and cannot be attacked until the protection ends.
		if _, err := tx.Exec(ctx, `
			UPDATE "Territory" SET "ownerEmpireId" = $2, "protectedUntilTurn" = $3, weight = 1,
				"aboveAlertAtLastClose" = false, alert = false
			WHERE id = $1`, targetID, attackerID, protectedUntilTurn); err != nil {
			return false, err
		}
	}

	if attackerWins {
		// What stood on a conquered territory is lost with it (a second capital, for one).
		if _, err := tx.Exec(ctx, `UPDATE "Structure" SET active = false WHERE "territoryId" = $1 AND active`, targetID); err != nil {
			return false, err
		}
	}

	// The territories involved are free again.
	if _, err := tx.Exec(ctx, `DELETE FROM "BattleLock" WHERE "battleId" = $1`, battleID); err != nil {
		return false, err
	}
	var rollAttack, rollDefense *float64
	if result != nil {
		rollAttack, rollDefense = &result.RollAttack, &result.RollDefense
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Battle" SET status = 'RESOLVED', outcome = $2::"BattleOutcome", resolver = $3,
			"rollAttack" = $4, "rollDefense" = $5, "defenseCost" = $6,
			"resolvedAt" = (now() AT TIME ZONE 'UTC')
		WHERE id = $1`, battleID, outcome, s.Resolver.Name(), rollAttack, rollDefense, defenseCost); err != nil {
		return false, err
	}

	// The public record of the battle: names and outcome, never an amount.
	var attackerName, defenderName, territoryName string
	var turn int
	if err := tx.QueryRow(ctx, `
		SELECT a.name, d.name, t.name, m."currentTurn"
		FROM "Empire" a, "Empire" d, "Territory" t, "Match" m
		WHERE a.id = $1 AND d.id = $2 AND t.id = $3 AND m.id = $4`,
		attackerID, defenderID, targetID, matchID).Scan(&attackerName, &defenderName, &territoryName, &turn); err != nil {
		return false, err
	}
	data, err := json.Marshal(map[string]string{
		"attackerName": attackerName, "defenderName": defenderName,
		"territoryName": territoryName, "outcome": outcome, "defenderEmpireId": defenderID,
	})
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "GameEvent" (id, "matchId", turn, type, "empireId", "territoryId", data)
		VALUES ('ev_' || gen_random_uuid(), $1, $2, 'BATTLE_RESOLVED', $3, $4, $5::jsonb)`,
		matchID, turn, attackerID, targetID, string(data)); err != nil {
		return false, err
	}
	if err := queue.Notify(ctx, tx, queue.Notification{MatchID: &matchID, Kind: "battle.resolved"}); err != nil {
		return false, err
	}
	s.Log.Info("battle resolved", "battleId", battleID, "matchId", matchID, "outcome", outcome)

	// Last, with this battle closed and its territories unlocked: if the
	// conquered territory was the capital, the empire that lost it falls.
	if attackerWins {
		ended, err := endgame.CapitalLost(ctx, tx, matchID, defenderID, attackerID, targetID, turn)
		if err != nil {
			return false, fmt.Errorf("capital lost: %w", err)
		}
		if ended {
			s.Log.Info("match finished: one empire left", "matchId", matchID, "winner", attackerID)
		}
	}
	return true, nil
}

// ResolveOpenInTx resolves every battle of a match that is still open, in
// the order they were declared. It is called when the turn closes.
func (s *Service) ResolveOpenInTx(ctx context.Context, tx pgx.Tx, matchID string, protectedUntilTurn int) (int, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM "Battle" WHERE "matchId" = $1 AND status = 'OPEN' ORDER BY "createdAt", id`, matchID)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, id := range ids {
		ok, err := s.ResolveInTx(ctx, tx, id, true, protectedUntilTurn)
		if err != nil {
			return resolved, err
		}
		if ok {
			resolved++
		}
	}
	return resolved, nil
}
