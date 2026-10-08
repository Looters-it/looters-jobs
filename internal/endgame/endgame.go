// Package endgame holds what ends things: the fall of an empire whose
// capital is conquered, and the end of a match (PROMPT §2.9).
package endgame

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"looters/jobs/internal/queue"
)

const utcNow = `(now() AT TIME ZONE 'UTC')`

func event(ctx context.Context, tx pgx.Tx, matchID string, turn int, kind string, empireID *string, data map[string]string) error {
	body, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO "GameEvent" (id, "matchId", turn, type, "empireId", data)
		VALUES ('ev_' || gen_random_uuid(), $1, $2, $3, $4, $5::jsonb)`,
		matchID, turn, kind, empireID, string(body))
	return err
}

// CapitalLost is called when a territory has just been conquered. If it was
// the capital of the empire that lost it, that empire falls. It reports
// whether the match ended as a consequence (one empire left).
//
// When an empire falls (PROMPT §2.9):
//   - the conqueror keeps only the capital, with its resources;
//   - every other territory of the fallen empire becomes free and keeps its
//     resources, which stay hidden until someone annexes it;
//   - its player stays in the match as a spectator;
//   - the battles it was still part of are cancelled: nothing is fought, the
//     territories are unlocked and the cost of the attack goes back where it came from.
func CapitalLost(ctx context.Context, tx pgx.Tx, matchID, loserID, conquerorID, territoryID string, turn int) (matchEnded bool, err error) {
	var capital *string
	var loserName string
	if err := tx.QueryRow(ctx,
		`SELECT "capitalTerritoryId", name FROM "Empire" WHERE id = $1 FOR UPDATE`, loserID).Scan(&capital, &loserName); err != nil {
		return false, err
	}
	if capital == nil || *capital != territoryID {
		return false, nil
	}

	// A structure with the BACKUP_CAPITAL effect (the second capital) on a territory
	// the empire still holds saves it: that territory becomes the capital, the
	// structure is used up, and only the conquered municipality is lost.
	var backupStructure, backupTerritory, backupName string
	err = tx.QueryRow(ctx, `
		SELECT s.id, t.id, t.name FROM "Structure" s JOIN "Territory" t ON t.id = s."territoryId"
		WHERE s."empireId" = $1 AND s.active AND s.data->'effects' ? 'BACKUP_CAPITAL'
			AND t."ownerEmpireId" = $1
		ORDER BY s."builtTurn", s.id LIMIT 1`, loserID).Scan(&backupStructure, &backupTerritory, &backupName)
	if err == nil {
		if _, err := tx.Exec(ctx, `UPDATE "Structure" SET active = false WHERE id = $1`, backupStructure); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE "Empire" SET "capitalTerritoryId" = $2 WHERE id = $1`, loserID, backupTerritory); err != nil {
			return false, err
		}
		return false, event(ctx, tx, matchID, turn, "CAPITAL_PROMOTED", &loserID,
			map[string]string{"empireName": loserName, "territoryName": backupName})
	}
	if err != pgx.ErrNoRows {
		return false, err
	}

	const openBattlesOfLoser = `
		SELECT id FROM "Battle"
		WHERE "matchId" = $1 AND status = 'OPEN' AND ("attackerEmpireId" = $2 OR "defenderEmpireId" = $2)`
	if _, err := tx.Exec(ctx, `
		UPDATE "Territory" t SET resources = t.resources + src.cost
		FROM "BattleSource" src WHERE t.id = src."territoryId" AND src."battleId" IN (`+openBattlesOfLoser+`)`,
		matchID, loserID); err != nil {
		return false, fmt.Errorf("refund cancelled battles: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM "BattleLock" WHERE "battleId" IN (`+openBattlesOfLoser+`)`, matchID, loserID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Battle" SET status = 'RESOLVED', outcome = 'CANCELLED', "resolvedAt" = `+utcNow+`
		WHERE "matchId" = $1 AND status = 'OPEN' AND ("attackerEmpireId" = $2 OR "defenderEmpireId" = $2)`,
		matchID, loserID); err != nil {
		return false, err
	}

	// A fallen empire is in no alliance.
	if _, err := tx.Exec(ctx, `DELETE FROM "AllianceMember" WHERE "empireId" = $1`, loserID); err != nil {
		return false, err
	}
	if err := dissolveSmallAlliances(ctx, tx, matchID); err != nil {
		return false, err
	}
	// Nothing the empire built outlives it.
	if _, err := tx.Exec(ctx, `UPDATE "Structure" SET active = false WHERE "empireId" = $1 AND active`, loserID); err != nil {
		return false, err
	}
	// The rest of the empire becomes no man's land, resources included.
	if _, err := tx.Exec(ctx, `
		UPDATE "Territory" SET "ownerEmpireId" = NULL, "protectedUntilTurn" = NULL, weight = 1,
			alert = false, "aboveAlertAtLastClose" = false
		WHERE "matchId" = $1 AND "ownerEmpireId" = $2`, matchID, loserID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Empire" SET status = 'FALLEN', "fallenTurn" = $2, "capitalTerritoryId" = NULL WHERE id = $1`,
		loserID, turn); err != nil {
		return false, err
	}

	var conquerorName string
	if err := tx.QueryRow(ctx, `SELECT name FROM "Empire" WHERE id = $1`, conquerorID).Scan(&conquerorName); err != nil {
		return false, err
	}
	if err := event(ctx, tx, matchID, turn, "EMPIRE_FALLEN", &loserID,
		map[string]string{"empireName": loserName, "byEmpireName": conquerorName}); err != nil {
		return false, err
	}
	return finishIfOneLeft(ctx, tx, matchID, turn)
}

// finishIfOneLeft ends the match when a single empire is still standing: it wins.
func finishIfOneLeft(ctx context.Context, tx pgx.Tx, matchID string, turn int) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM "Empire" WHERE "matchId" = $1 AND status = 'ACTIVE'`, matchID)
	if err != nil {
		return false, err
	}
	active, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return false, err
	}
	if len(active) != 1 {
		return false, nil
	}
	return true, Finish(ctx, tx, matchID, active, turn)
}

// Deadline is the moment a match with a maximum duration ends, or nil.
func Deadline(startAt time.Time, maxDays *int) *time.Time {
	if maxDays == nil {
		return nil
	}
	deadline := startAt.Add(time.Duration(*maxDays) * 24 * time.Hour)
	return &deadline
}

// WinnersAtDeadline picks who wins a match that reached its maximum
// duration: the active empire with the most resources; with equal resources
// the one that controls more municipalities. Empires level on both win
// together: there is nothing left to tell them apart.
func WinnersAtDeadline(ctx context.Context, tx pgx.Tx, matchID string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		WITH standing AS (
			SELECT e.id, e."joinedAt", coalesce(sum(t.resources), 0) AS resources, coalesce(sum(units.n), 0) AS units
			FROM "Empire" e
			LEFT JOIN "Territory" t ON t."ownerEmpireId" = e.id
			LEFT JOIN LATERAL (SELECT count(*) AS n FROM "TerritoryUnit" tu WHERE tu."territoryId" = t.id) units ON true
			WHERE e."matchId" = $1 AND e.status = 'ACTIVE'
			GROUP BY e.id, e."joinedAt"
		)
		SELECT id FROM standing
		WHERE (resources, units) = (SELECT resources, units FROM standing ORDER BY resources DESC, units DESC LIMIT 1)
		ORDER BY "joinedAt", id`, matchID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Finish ends a match. Battles still open are cancelled like those of a
// fallen empire, so that nothing stays locked in a finished match.
func Finish(ctx context.Context, tx pgx.Tx, matchID string, winnerIDs []string, turn int) error {
	var winnerID *string
	if len(winnerIDs) > 0 {
		winnerID = &winnerIDs[0]
	}
	if winnerIDs == nil {
		winnerIDs = []string{}
	}
	const open = `SELECT id FROM "Battle" WHERE "matchId" = $1 AND status = 'OPEN'`
	if _, err := tx.Exec(ctx, `
		UPDATE "Territory" t SET resources = t.resources + src.cost
		FROM "BattleSource" src WHERE t.id = src."territoryId" AND src."battleId" IN (`+open+`)`, matchID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM "BattleLock" WHERE "battleId" IN (`+open+`)`, matchID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Battle" SET status = 'RESOLVED', outcome = 'CANCELLED', "resolvedAt" = `+utcNow+`
		WHERE "matchId" = $1 AND status = 'OPEN'`, matchID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Match" SET status = 'FINISHED', "finishedAt" = `+utcNow+`, "winnerEmpireId" = $2, "winnerEmpireIds" = $3 WHERE id = $1`,
		matchID, winnerID, winnerIDs); err != nil {
		return err
	}
	// The last turn never closes: its picture is taken here, so that the history
	// shows how the match ended whenever that happened.
	if _, err := tx.Exec(ctx, `
		INSERT INTO "TurnSnapshot" ("matchId", turn, ownership)
		SELECT $1, $2, coalesce(jsonb_object_agg(tu."geoUnitId"::text, t."ownerEmpireId"), '{}'::jsonb)
		FROM "Territory" t JOIN "TerritoryUnit" tu ON tu."territoryId" = t.id
		WHERE t."matchId" = $1 AND t."ownerEmpireId" IS NOT NULL
		ON CONFLICT DO NOTHING`, matchID, turn); err != nil {
		return err
	}
	data := map[string]string{}
	if len(winnerIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT name FROM "Empire" WHERE id = ANY($1) ORDER BY "joinedAt", id`, winnerIDs)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		data["winnerName"] = strings.Join(names, ", ")
		if len(names) > 1 {
			// Told apart by the interface, which writes the sentence.
			data["result"] = "TIE"
		}
	}
	if err := event(ctx, tx, matchID, turn, "MATCH_FINISHED", winnerID, data); err != nil {
		return err
	}
	return queue.Notify(ctx, tx, queue.Notification{MatchID: &matchID, Kind: "match.finished"})
}

// UpdateAccumulationAlerts marks the territories that hold more than pct% of
// their empire's resources at two consecutive turn closes (PROMPT §2.8): a
// sign, shown to everyone, that an attack is probably being prepared there.
// Only empires with at least minTerritories territories are considered.
func UpdateAccumulationAlerts(ctx context.Context, tx pgx.Tx, matchID string, pct float64, minTerritories int) error {
	_, err := tx.Exec(ctx, `
		WITH totals AS (
			SELECT t."ownerEmpireId" AS empire, sum(t.resources) AS total, count(*) AS territories
			FROM "Territory" t JOIN "Empire" e ON e.id = t."ownerEmpireId" AND e.status = 'ACTIVE'
			WHERE t."matchId" = $1
			GROUP BY t."ownerEmpireId"
		), now_above AS (
			SELECT t.id, coalesce(totals.territories >= $3 AND totals.total > 0
				AND t.resources * 100.0 > $2 * totals.total, false) AS above
			FROM "Territory" t LEFT JOIN totals ON totals.empire = t."ownerEmpireId"
			WHERE t."matchId" = $1
		)
		UPDATE "Territory" t SET alert = now_above.above AND t."aboveAlertAtLastClose",
			"aboveAlertAtLastClose" = now_above.above
		FROM now_above WHERE t.id = now_above.id
			AND (t.alert IS DISTINCT FROM (now_above.above AND t."aboveAlertAtLastClose")
				OR t."aboveAlertAtLastClose" IS DISTINCT FROM now_above.above)`,
		matchID, pct, minTerritories)
	return err
}

// ExpireStructures ends the structures whose last turn is the one closing.
// It reads only what every structure has, so a new kind of structure needs
// nothing here.
func ExpireStructures(ctx context.Context, tx pgx.Tx, matchID string, turn int) error {
	_, err := tx.Exec(ctx, `
		UPDATE "Structure" SET active = false
		WHERE "matchId" = $1 AND active AND "expiresTurn" IS NOT NULL AND "expiresTurn" <= $2`, matchID, turn)
	return err
}

// ApplyAllianceExits makes effective the exits announced for the turn that
// is closing or earlier (PROMPT §2.6): until this moment the empire that is
// leaving was still an ally in every respect.
func ApplyAllianceExits(ctx context.Context, tx pgx.Tx, matchID string, turn int) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM "AllianceMember" m USING "Alliance" a
		WHERE a.id = m."allianceId" AND a."matchId" = $1 AND m."leavesAtTurn" IS NOT NULL AND m."leavesAtTurn" <= $2`,
		matchID, turn); err != nil {
		return err
	}
	return dissolveSmallAlliances(ctx, tx, matchID)
}

// dissolveSmallAlliances ends the alliances left with fewer than two members:
// an alliance of one is no alliance.
func dissolveSmallAlliances(ctx context.Context, tx pgx.Tx, matchID string) error {
	const small = `
		SELECT a.id FROM "Alliance" a
		WHERE a."matchId" = $1 AND a."dissolvedAt" IS NULL
			AND (SELECT count(*) FROM "AllianceMember" m WHERE m."allianceId" = a.id) < 2`
	if _, err := tx.Exec(ctx, `DELETE FROM "AllianceMember" WHERE "allianceId" IN (`+small+`)`, matchID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE "Alliance" a SET "dissolvedAt" = `+utcNow+`
		WHERE a."matchId" = $1 AND a."dissolvedAt" IS NULL
			AND NOT EXISTS (SELECT 1 FROM "AllianceMember" m WHERE m."allianceId" = a.id)`, matchID)
	return err
}
