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
	OutcomeWithdrawn   = "WITHDRAWN"
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
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "ONGOING") {
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
		attackForce, attackCost, attackPct, round         int
		defenseForce, defenseSpent, foughtTurn            *int
		withdrawn                                         bool
	)
	err := tx.QueryRow(ctx, `
		SELECT status::text, response::text, "matchId", "attackerEmpireId", "defenderEmpireId",
			"targetTerritoryId", "attackForce", "attackCost", "defenseForce",
			"attackPct", round, "defenseCost", "foughtTurn", withdrawn
		FROM "Battle" WHERE id = $1 FOR UPDATE`, battleID,
	).Scan(&status, &response, &matchID, &attackerID, &defenderID, &targetID, &attackForce, &attackCost, &defenseForce,
		&attackPct, &round, &defenseSpent, &foughtTurn, &withdrawn)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "OPEN" {
		return false, nil // already resolved: nothing is applied twice
	}
	// What the attacker decides comes first: a siege given up is not fought again.
	answer := "DEFEND"
	switch {
	case withdrawn:
		answer = "WITHDRAW"
	case response != nil:
		answer = *response
	case !silenceIsDefense:
		return false, nil
	}

	var config []byte
	var currentTurn int
	if err := tx.QueryRow(ctx, `SELECT config, "currentTurn" FROM "Match" WHERE id = $1`, matchID).Scan(&config, &currentTurn); err != nil {
		return false, err
	}
	matchRules, err := rules.ParseMatchRules(config)
	if err != nil {
		return false, err
	}
	// One round a turn: a siege whose round of this turn was fought waits for the next,
	// and can only be given up in the meantime.
	if answer == "DEFEND" && foughtTurn != nil && *foughtTurn >= currentTurn {
		return false, nil
	}
	spent := 0
	if defenseSpent != nil {
		spent = *defenseSpent
	}
	// What winning pays: a share of everything the two sides spent, round after round.
	spoilsOf := func(attackSpent, defenseSpent int) int {
		return int(math.Floor(float64(attackSpent+defenseSpent) * matchRules.BattleSpoilsPct / 100))
	}

	var (
		outcome      string
		result       *Result
		defenseCost  *int
		spoils       *int
		share        *float64
		attackerWins bool
	)
	switch answer {
	case "SURRENDER":
		// The territory is given up with what is on it.
		outcome, attackerWins = OutcomeSurrender, true
		if round == 1 {
			// Nothing was fought: the attacker gets back part of the cost.
			if _, err := tx.Exec(ctx, `
				UPDATE "Territory" t SET resources = t.resources + floor(src.cost * $2::numeric / 100)::int
				FROM "BattleSource" src WHERE src."battleId" = $1 AND t.id = src."territoryId"`,
				battleID, matchRules.SurrenderRefundPct); err != nil {
				return false, err
			}
		} else {
			// A siege that ends in a surrender is won: it pays like one.
			won := spoilsOf(attackCost, spent)
			spoils = &won
			if err := returnSpoils(ctx, tx, battleID, won); err != nil {
				return false, err
			}
		}
	case "WITHDRAW":
		// The attacker leaves: the defender keeps the territory, and is paid for the siege.
		outcome = OutcomeWithdrawn
		won := spoilsOf(attackCost, spent)
		spoils = &won
		if _, err := tx.Exec(ctx,
			`UPDATE "Territory" SET resources = resources + $2 WHERE id = $1`, targetID, won); err != nil {
			return false, err
		}
	default:
		// In the first round strength was fixed at the declaration, before any cost, and
		// the attacker paid then. A later round is fought with what is on the territories
		// when it comes, and the attacker pays for it now: the same share as the first time.
		force := 0
		if defenseForce != nil {
			force = *defenseForce
		}
		roundAttackCost := attackCost
		if round > 1 {
			paid, strength, err := payRound(ctx, tx, battleID, attackPct)
			if err != nil {
				return false, err
			}
			roundAttackCost, attackForce = paid, strength
			attackCost += paid
			if err := tx.QueryRow(ctx,
				`SELECT resources FROM "Territory" WHERE id = $1 FOR UPDATE`, targetID).Scan(&force); err != nil {
				return false, err
			}
		}
		// Defending costs a share of what is on the attacked territory, whoever wins.
		cost := int(math.Ceil(float64(force) * matchRules.DefensePct / 100))
		spent += cost
		defenseCost = &spent
		fought := s.Resolver.Resolve(attackForce, force, s.Roll)
		result = &fought

		// A capital is harder to take in one go.
		margin := matchRules.SiegeMarginPct
		var capital bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM "Empire" WHERE id = $1 AND "capitalTerritoryId" = $2)`,
			defenderID, targetID).Scan(&capital); err != nil {
			return false, err
		}
		if capital {
			margin = matchRules.SiegeCapitalMarginPct
		}
		decided, won, part := Decide(attackForce, force, fought, margin)
		share = &part
		verdict := "DRAW"
		if decided && won {
			verdict = "ATTACKER"
		} else if decided {
			verdict = "DEFENDER"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO "BattleRound" ("battleId", round, turn, "attackForce", "attackCost", "defenseForce",
				"defenseCost", "rollAttack", "rollDefense", share, result)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			battleID, round, currentTurn, attackForce, roundAttackCost, force, cost,
			fought.RollAttack, fought.RollDefense, part, verdict); err != nil {
			return false, err
		}

		if !decided {
			// Nobody prevailed: both paid, the territories stay locked, and the battle
			// waits for the next turn, when the defender answers again.
			if _, err := tx.Exec(ctx,
				`UPDATE "Territory" SET resources = GREATEST(resources - $2, 0) WHERE id = $1`, targetID, cost); err != nil {
				return false, err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE "Battle" SET round = round + 1, "foughtTurn" = $2, response = NULL, "respondedAt" = NULL,
					resolver = $3, "rollAttack" = $4, "rollDefense" = $5, "attackForce" = $6, "attackCost" = $7,
					"defenseForce" = $8, "defenseCost" = $9, share = $10
				WHERE id = $1`, battleID, currentTurn, s.Resolver.Name(), fought.RollAttack, fought.RollDefense,
				attackForce, attackCost, force, spent, part); err != nil {
				return false, err
			}
			if err := s.record(ctx, tx, "BATTLE_ROUND", matchID, attackerID, defenderID, targetID, currentTurn,
				map[string]any{"round": round, "share": part}); err != nil {
				return false, err
			}
			s.Log.Info("battle round drawn", "battleId", battleID, "matchId", matchID, "round", round)
			return true, nil
		}

		// Part of what the two sides spent goes to the winner: the defender receives it
		// on the territory that was defended, the attacker on the territories the attack
		// started from.
		prize := spoilsOf(attackCost, spent)
		spoils = &prize
		attackerWins = won
		outcome = OutcomeDefenderWon
		kept := prize
		if attackerWins {
			outcome, kept = OutcomeAttackerWon, 0
		}
		defenseForce = &force
		// Paid and received in one write: a second one on the same row would check its
		// owner again, and lock it.
		if _, err := tx.Exec(ctx,
			`UPDATE "Territory" SET resources = GREATEST(resources - $2, 0) + $3 WHERE id = $1`, targetID, cost, kept); err != nil {
			return false, err
		}
		if attackerWins {
			if err := returnSpoils(ctx, tx, battleID, prize); err != nil {
				return false, err
			}
		}
	}

	if attackerWins {
		// The winner takes the territory with whatever is left on it. Just acquired, it
		// can receive resources at once and cannot be attacked until the protection ends.
		// In the proportional split of the income it weighs like the average of the
		// territories the winner already has, so that it gets a share like theirs.
		if _, err := tx.Exec(ctx, `
			UPDATE "Territory" t SET "ownerEmpireId" = $2, "protectedUntilTurn" = $3,
				weight = coalesce((SELECT greatest(1, round(avg(o.weight)))::int FROM "Territory" o
					WHERE o."ownerEmpireId" = $2 AND o.id <> t.id), 1),
				"aboveAlertAtLastClose" = false, alert = false
			WHERE id = $1`, targetID, attackerID, protectedUntilTurn); err != nil {
			return false, err
		}
	}

	if !attackerWins {
		// Attacked once, a territory is left alone until the turn the battle ended in ends:
		// nobody can attack the same one twice in a turn, whoever they are. A battle
		// fought while its turn closes leaves the territory open in the turn that begins.
		if _, err := tx.Exec(ctx, `
			UPDATE "Territory" t SET "protectedUntilTurn" = $2
			WHERE t.id = $1 AND coalesce(t."protectedUntilTurn", 0) < $2`, targetID, currentTurn); err != nil {
			return false, err
		}
	}

	if attackerWins {
		// What stood on a conquered territory is lost with it (a second capital, for one).
		// A port or an airport is not: it is part of the place, with the level it was
		// brought to, and serves whoever holds it.
		if _, err := tx.Exec(ctx, `
			UPDATE "Structure" SET active = false
			WHERE "territoryId" = $1 AND active AND NOT jsonb_exists_any(data->'effects', ARRAY['SEA_REACH', 'AIR_REACH'])`, targetID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE "Structure" SET "empireId" = $2
			WHERE "territoryId" = $1 AND active AND jsonb_exists_any(data->'effects', ARRAY['SEA_REACH', 'AIR_REACH'])`, targetID, attackerID); err != nil {
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
			"rollAttack" = coalesce($4, "rollAttack"), "rollDefense" = coalesce($5, "rollDefense"),
			"defenseCost" = coalesce($6, "defenseCost"), spoils = $7,
			"attackForce" = $8, "attackCost" = $9, "defenseForce" = $10, share = coalesce($11, share),
			"foughtTurn" = CASE WHEN $4::float8 IS NULL THEN "foughtTurn" ELSE $12 END,
			"resolvedAt" = (now() AT TIME ZONE 'UTC')
		WHERE id = $1`, battleID, outcome, s.Resolver.Name(), rollAttack, rollDefense, defenseCost, spoils,
		attackForce, attackCost, defenseForce, share, currentTurn); err != nil {
		return false, err
	}

	// The public record of the battle: names, outcome and by how much, never an amount.
	extra := map[string]any{"outcome": outcome, "rounds": round}
	if share != nil {
		extra["share"] = *share
	}
	if err := s.record(ctx, tx, "BATTLE_RESOLVED", matchID, attackerID, defenderID, targetID, currentTurn, extra); err != nil {
		return false, err
	}
	s.Log.Info("battle resolved", "battleId", battleID, "matchId", matchID, "outcome", outcome)

	// Last, with this battle closed and its territories unlocked: if the
	// conquered territory was the capital, the empire that lost it falls.
	if attackerWins {
		ended, err := endgame.CapitalLost(ctx, tx, matchID, defenderID, attackerID, targetID, currentTurn)
		if err != nil {
			return false, fmt.Errorf("capital lost: %w", err)
		}
		if ended {
			s.Log.Info("match finished: one empire left", "matchId", matchID, "winner", attackerID)
		}
	}
	return true, nil
}

// record writes the public event of a battle, or of one of its rounds, and tells who
// follows the match that something changed.
func (s *Service) record(ctx context.Context, tx pgx.Tx, kind, matchID, attackerID, defenderID, targetID string, turn int, extra map[string]any) error {
	var attackerName, defenderName, territoryName string
	if err := tx.QueryRow(ctx, `
		SELECT a.name, d.name, t.name FROM "Empire" a, "Empire" d, "Territory" t
		WHERE a.id = $1 AND d.id = $2 AND t.id = $3`,
		attackerID, defenderID, targetID).Scan(&attackerName, &defenderName, &territoryName); err != nil {
		return err
	}
	fields := map[string]any{
		"attackerName": attackerName, "defenderName": defenderName,
		"territoryName": territoryName, "defenderEmpireId": defenderID,
	}
	for key, value := range extra {
		fields[key] = value
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO "GameEvent" (id, "matchId", turn, type, "empireId", "territoryId", data)
		VALUES ('ev_' || gen_random_uuid(), $1, $2, $3, $4, $5, $6::jsonb)`,
		matchID, turn, kind, attackerID, targetID, string(data)); err != nil {
		return err
	}
	return queue.Notify(ctx, tx, queue.Notification{MatchID: &matchID, Kind: "battle.resolved"})
}

// payRound makes the attacker pay for a round of a siege: every territory the attack
// started from pays the share it paid the first time, of what it holds now. It returns
// what was paid and the strength in the field: what was there before paying.
func payRound(ctx context.Context, tx pgx.Tx, battleID string, pct int) (paid, strength int, err error) {
	type source struct {
		ID        string
		Resources int
	}
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.resources FROM "BattleSource" src JOIN "Territory" t ON t.id = src."territoryId"
		WHERE src."battleId" = $1 ORDER BY t.id FOR UPDATE OF t`, battleID)
	if err != nil {
		return 0, 0, err
	}
	sources, err := pgx.CollectRows(rows, pgx.RowToStructByPos[source])
	if err != nil {
		return 0, 0, err
	}
	for _, src := range sources {
		cost := int(math.Ceil(float64(src.Resources) * float64(pct) / 100))
		strength += src.Resources
		paid += cost
		if _, err := tx.Exec(ctx,
			`UPDATE "Territory" SET resources = resources - $2 WHERE id = $1`, src.ID, cost); err != nil {
			return 0, 0, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE "BattleSource" SET cost = cost + $3 WHERE "battleId" = $1 AND "territoryId" = $2`,
			battleID, src.ID, cost); err != nil {
			return 0, 0, err
		}
	}
	return paid, strength, nil
}

// returnSpoils gives what an attack won to the territories it started from, each
// in proportion to what it paid; what the rounding leaves goes to the ones that
// paid the most, one each.
func returnSpoils(ctx context.Context, tx pgx.Tx, battleID string, spoils int) error {
	type source struct {
		ID   string
		Cost int
	}
	rows, err := tx.Query(ctx,
		`SELECT "territoryId", cost FROM "BattleSource" WHERE "battleId" = $1 ORDER BY cost DESC, "territoryId"`, battleID)
	if err != nil {
		return err
	}
	sources, err := pgx.CollectRows(rows, pgx.RowToStructByPos[source])
	if err != nil {
		return err
	}
	total := 0
	for _, src := range sources {
		total += src.Cost
	}
	if total == 0 {
		return nil
	}
	shares, given := make([]int, len(sources)), 0
	for i, src := range sources {
		shares[i] = spoils * src.Cost / total
		given += shares[i]
	}
	for i := 0; given < spoils; i, given = (i+1)%len(shares), given+1 {
		shares[i]++
	}
	for i, src := range sources {
		if _, err := tx.Exec(ctx,
			`UPDATE "Territory" SET resources = resources + $2 WHERE id = $1`, src.ID, shares[i]); err != nil {
			return err
		}
	}
	return nil
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
