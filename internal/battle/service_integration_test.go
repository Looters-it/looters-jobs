package battle_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"looters/jobs/internal/battle"
	"looters/jobs/internal/turnclose"
)

// These tests need a database migrated by the api: set TEST_DATABASE_URL (see .env.example).
type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	battles *battle.Service
	rolls   []float64
}

var turnEnd = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)

// setup creates a running match at turn 3. Anna attacks from A1 (100 resources at the
// declaration, 50 reserved, 50 left) and A2 (20, 10 reserved, 10 left) at 60%... the
// numbers are written directly: what matters here is what resolution does with them.
func setup(t *testing.T) *fixture {
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
	f := &fixture{t: t, pool: pool}
	f.battles = battle.NewService(pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	i := 0
	f.battles.Roll = func() float64 {
		if len(f.rolls) == 0 {
			return battle.SecureRoll()
		}
		v := f.rolls[i%len(f.rolls)]
		i++
		return v
	}
	for _, table := range []string{"Job", "Match", "GeoArea", "User"} {
		f.exec(`DELETE FROM "` + table + `"`)
	}
	config, err := os.ReadFile("../rules/testdata/default-config.json")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(`INSERT INTO "User" (id, username, email, "passwordHash") VALUES ('u1','anna','a@example.test','x'), ('u2','bruno','b@example.test','x'), ('u3','carla','c@example.test','x')`)
	f.exec(`INSERT INTO "Match" (id, name, "ownerId", status, "startAt", "turnHours", "currentTurn", "turnEndsAt", config)
		VALUES ('m1', 'Prova', 'u1', 'ONGOING', $1, 12, 3, $2, $3::jsonb)`, turnEnd.Add(-36*time.Hour), turnEnd, string(config))
	f.exec(`INSERT INTO "Empire" (id, "matchId", "userId", name, color, emblem, "capitalTerritoryId") VALUES
		('anna', 'm1', 'u1', 'Anna', '#111111', 'torre', 'A1'), ('bruno', 'm1', 'u2', 'Bruno', '#222222', 'luna', 'B1'),
		('carla', 'm1', 'u3', 'Carla', '#333333', 'luna', 'C1')`)
	// After the declaration: A1 holds 50 of its 100, A2 10 of its 20. The target B2 still holds 80.
	f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId") VALUES
		('A1', 'm1', 'Abbiategrasso', 50, 'anna'), ('A2', 'm1', 'Albairate', 10, 'anna'),
		('B1', 'm1', 'Binasco', 300, 'bruno'), ('B2', 'm1', 'Bollate', 80, 'bruno'),
		('C1', 'm1', 'Corsico', 70, 'carla'), ('C2', 'm1', 'Cusago', 40, 'carla')`)
	f.declare("b1", "B2", 120, 80, map[string][2]int{"A1": {50, 100}, "A2": {10, 20}})
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// declare stores an open battle as the api would: sources map territory -> {cost, resources at declaration}.
func (f *fixture) declare(id, target string, attackForce, defenseForce int, sources map[string][2]int) {
	f.t.Helper()
	total := 0
	for _, source := range sources {
		total += source[0]
	}
	f.exec(`INSERT INTO "Battle" (id, "matchId", turn, "attackerEmpireId", "defenderEmpireId", "targetTerritoryId",
			"attackPct", "attackCost", "attackForce", "defenseForce")
		VALUES ($1, 'm1', 3, 'anna', 'bruno', $2, 60, $3, $4, $5)`, id, target, total, attackForce, defenseForce)
	f.exec(`INSERT INTO "BattleLock" ("territoryId", "battleId") VALUES ($1, $2)`, target, id)
	for territory, source := range sources {
		f.exec(`INSERT INTO "BattleSource" ("battleId", "territoryId", cost, "resourcesAtDeclaration") VALUES ($1, $2, $3, $4)`,
			id, territory, source[0], source[1])
		f.exec(`INSERT INTO "BattleLock" ("territoryId", "battleId") VALUES ($1, $2)`, territory, id)
	}
}

func (f *fixture) answer(battleID, response string) {
	f.exec(`UPDATE "Battle" SET response = $2::"BattleResponse", "respondedAt" = now() WHERE id = $1`, battleID, response)
}

func (f *fixture) resolve(battleID string) bool {
	f.t.Helper()
	resolved, err := f.battles.Resolve(context.Background(), battleID, "m1")
	if err != nil {
		f.t.Fatal(err)
	}
	return resolved
}

type territory struct {
	owner     string
	resources int
	protected *int
}

func (f *fixture) territory(id string) territory {
	f.t.Helper()
	var got territory
	var owner *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT "ownerEmpireId", resources, "protectedUntilTurn" FROM "Territory" WHERE id = $1`, id,
	).Scan(&owner, &got.resources, &got.protected); err != nil {
		f.t.Fatal(err)
	}
	if owner != nil {
		got.owner = *owner
	}
	return got
}

func (f *fixture) expect(id, owner string, resources int) {
	f.t.Helper()
	if got := f.territory(id); got.owner != owner || got.resources != resources {
		f.t.Errorf("%s belongs to %q with %d resources, want %q with %d", id, got.owner, got.resources, owner, resources)
	}
}

func (f *fixture) scan(query string, dest ...any) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), query).Scan(dest...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) count(query string) (n int) {
	f.scan(query, &n)
	return
}

func TestSurrender(t *testing.T) {
	f := setup(t)
	f.answer("b1", "SURRENDER")
	if !f.resolve("b1") {
		t.Fatal("not resolved")
	}

	// The territory passes to the attacker empty: what was on it is taken away to the
	// territory of the defender bordering it. No defence was paid.
	f.expect("B2", "anna", 0)
	f.expect("B1", "bruno", 380)
	// Half of the cost comes back to each territory that paid it: 25 of 50, 5 of 10.
	f.expect("A1", "anna", 75)
	f.expect("A2", "anna", 15)
	if got := f.territory("B2").protected; got == nil || *got != 3 {
		t.Errorf("the conquered territory is protected until %v, want turn 3", got)
	}

	var status, outcome, resolver string
	var rollAttack, rollDefense *float64
	var defenseCost *int
	f.scan(`SELECT status::text, outcome::text, resolver, "rollAttack", "rollDefense", "defenseCost" FROM "Battle" WHERE id = 'b1'`,
		&status, &outcome, &resolver, &rollAttack, &rollDefense, &defenseCost)
	if status != "RESOLVED" || outcome != battle.OutcomeSurrender || rollAttack != nil || rollDefense != nil || defenseCost != nil {
		t.Errorf("battle: %s %s rolls=%v/%v defenseCost=%v", status, outcome, rollAttack, rollDefense, defenseCost)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
}

func TestDefenceWonByTheAttacker(t *testing.T) {
	f := setup(t)
	f.rolls = []float64{0.9, 0.2} // 120 x 0.9 = 108 against 80 x 0.2 = 16
	f.answer("b1", "DEFEND")
	f.resolve("b1")

	// The defender paid half of what was on the territory; the rest goes to the winner.
	f.expect("B2", "anna", 40)
	// 70% of what the two sides spent, 60 to attack and 40 to defend, goes back where
	// the attack started from, in proportion to what each paid: 50 and 10 of 60.
	f.expect("A1", "anna", 109)
	f.expect("A2", "anna", 21)
	f.expect("B1", "bruno", 300)

	var outcome, resolver string
	var rollAttack, rollDefense float64
	var defenseCost, spoils int
	f.scan(`SELECT outcome::text, resolver, "rollAttack", "rollDefense", "defenseCost", spoils FROM "Battle" WHERE id = 'b1'`,
		&outcome, &resolver, &rollAttack, &rollDefense, &defenseCost, &spoils)
	// Resources, numbers drawn and outcome are all on record.
	if outcome != battle.OutcomeAttackerWon || resolver != "uniform-product-v1" || rollAttack != 0.9 || rollDefense != 0.2 || defenseCost != 40 || spoils != 70 {
		t.Errorf("battle: %s by %s, rolls %v/%v, defence cost %d", outcome, resolver, rollAttack, rollDefense, defenseCost)
	}
}

func TestDefenceWonByTheDefender(t *testing.T) {
	f := setup(t)
	f.rolls = []float64{0.1, 0.9} // 12 against 72
	f.answer("b1", "DEFEND")
	f.resolve("b1")

	// The territory stays. Defending cost half of what was on it, and winning pays 70%
	// of what the two sides spent: the defender ends up with more than before.
	f.expect("B2", "bruno", 110)
	f.expect("A1", "anna", 50)
	// Defended, it cannot be attacked again in the same turn.
	if got := f.territory("B2").protected; got == nil {
		t.Error("a territory that was defended can be attacked again in the same turn")
	}
	var outcome string
	f.scan(`SELECT outcome::text FROM "Battle" WHERE id = 'b1'`, &outcome)
	if outcome != battle.OutcomeDefenderWon {
		t.Errorf("outcome = %s", outcome)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
}

func TestARetreatCanLeaveSomethingBehind(t *testing.T) {
	f := setup(t)
	f.exec(`UPDATE "Match" SET config = config || '{"retreatPct": 25}'::jsonb`)
	f.answer("b1", "SURRENDER")
	f.resolve("b1")
	// A quarter of the 80 goes with Bruno, the rest stays for Anna.
	f.expect("B2", "anna", 60)
	f.expect("B1", "bruno", 320)
}

func TestAnEmptyTerritoryIsDefendedByItsGarrison(t *testing.T) {
	f := setup(t)
	f.exec(`UPDATE "Territory" SET resources = 0 WHERE id = 'B2'`)
	f.exec(`UPDATE "Battle" SET "defenseForce" = 0 WHERE id = 'b1'`)
	// A territory is one municipality at least: here it is drawn without any.
	units := 1
	f.rolls = []float64{0.01, 0.9} // 120 x 0.01 = 1.2 against the garrison x 0.9
	f.answer("b1", "DEFEND")
	f.resolve("b1")

	// Nothing on it, nothing paid, and still it holds.
	// The spoils are 70% of the 60 the attack cost.
	f.expect("B2", "bruno", 42)
	var force, cost int
	f.scan(`SELECT "defenseForce", "defenseCost" FROM "BattleRound" WHERE "battleId" = 'b1'`, &force, &cost)
	if force != units*10 || cost != 0 {
		t.Errorf("round fought with %d at a cost of %d, want %d for nothing", force, cost, units*10)
	}
}

func TestTheEventNamesTheSidesAndNeverAnAmount(t *testing.T) {
	f := setup(t)
	f.rolls = []float64{0.9, 0.2}
	f.answer("b1", "DEFEND")
	f.resolve("b1")

	var data string
	var empire, target string
	f.scan(`SELECT data::text, "empireId", "territoryId" FROM "GameEvent" WHERE type = 'BATTLE_RESOLVED'`, &data, &empire, &target)
	for _, want := range []string{`"attackerName": "Anna"`, `"defenderName": "Bruno"`, `"territoryName": "Bollate"`, `"outcome": "ATTACKER_WON"`} {
		if !strings.Contains(data, want) {
			t.Errorf("event data %s lacks %s", data, want)
		}
	}
	// By how much it went and in how many rounds are told; forces, costs and rolls are not.
	var fields map[string]any
	if err := json.Unmarshal([]byte(data), &fields); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		if _, number := value.(float64); number && key != "share" && key != "rounds" {
			t.Errorf("event data tells an amount in %s: %s", key, data)
		}
	}
	if empire != "anna" || target != "B2" {
		t.Errorf("event about %s on %s", empire, target)
	}
}

func TestResolvingTwiceAppliesTheOutcomeOnce(t *testing.T) {
	f := setup(t)
	f.answer("b1", "SURRENDER")
	f.resolve("b1")
	// The queue delivers the same request again.
	if f.resolve("b1") {
		t.Fatal("resolved a second time")
	}
	f.expect("A1", "anna", 75) // refunded once
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'BATTLE_RESOLVED'`); got != 1 {
		t.Errorf("%d events", got)
	}
}

func TestAnUnansweredBattleWaitsForTheTurnToClose(t *testing.T) {
	f := setup(t)
	// The queue only carries answered battles; a stray request for an unanswered one does nothing.
	if f.resolve("b1") {
		t.Fatal("resolved a battle the defender had not answered")
	}
	f.expect("B2", "bruno", 80)
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 3 {
		t.Errorf("%d locks, want 3", got)
	}
}

func TestTurnCloseFightsUnansweredBattlesAsADefence(t *testing.T) {
	f := setup(t)
	// A second battle whose surrender never reached the queue.
	f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId") VALUES ('A3', 'm1', 'Arese', 30, 'anna')`)
	f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId") VALUES ('B3', 'm1', 'Bresso', 300, 'bruno')`)
	f.declare("b2", "B3", 60, 300, map[string][2]int{"A3": {30, 60}})
	f.answer("b2", "SURRENDER")
	f.rolls = []float64{0.9, 0.2} // for b1, fought because nobody answered

	closer := &turnclose.Closer{
		Pool:    f.pool,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return turnEnd.Add(time.Second) },
		Battles: f.battles,
	}
	if outcome, err := closer.Close(context.Background(), "m1", 3); err != nil || outcome != turnclose.Closed {
		t.Fatalf("close: %s, %v", outcome, err)
	}

	var first, firstResponse, second string
	f.scan(`SELECT outcome::text, coalesce(response::text, 'none') FROM "Battle" WHERE id = 'b1'`, &first, &firstResponse)
	f.scan(`SELECT outcome::text FROM "Battle" WHERE id = 'b2'`, &second)
	// Silence is never a surrender: b1 was fought, and the defender paid the defence.
	if first != battle.OutcomeAttackerWon || firstResponse != "none" {
		t.Errorf("unanswered battle: %s (response %s)", first, firstResponse)
	}
	// A recorded answer is honoured even if it never reached the queue.
	if second != battle.OutcomeSurrender {
		t.Errorf("battle with a lost surrender: %s", second)
	}
	if got := f.count(`SELECT count(*) FROM "Battle" WHERE status = 'OPEN'`); got != 0 {
		t.Errorf("%d battles still open after the close", got)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
	// Conquered while the turn was closing: protected through the turn that begins.
	for _, id := range []string{"B3", "B2"} {
		if got := f.territory(id); got.owner != "anna" || got.protected == nil || *got.protected != 4 {
			t.Errorf("%s: owner %s, protected until %v, want anna until 4", id, got.owner, got.protected)
		}
	}
	// Income came after the battles: Anna now holds five territories of one municipality... none
	// has municipalities in this fixture, so everyone earns the base, 50, and nothing is negative.
	if got := f.count(`SELECT count(*) FROM "Territory" WHERE resources < 0`); got != 0 {
		t.Errorf("%d negative balances", got)
	}
	if got := f.count(`SELECT "currentTurn" FROM "Match" WHERE id = 'm1'`); got != 4 {
		t.Errorf("turn = %d", got)
	}
}

func TestManyBattlesResolvedAtTheSameTime(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	f.exec(`DELETE FROM "Territory" WHERE id LIKE 'A%' OR id = 'B2'`)
	// Every battle ends in its round: what is checked here is that nothing is applied twice.
	f.exec(`UPDATE "Match" SET config = config || '{"siegeMarginPct": 0}'::jsonb`)
	const n = 30
	for i := range n {
		source, target, id := fmt.Sprintf("S%02d", i), fmt.Sprintf("T%02d", i), fmt.Sprintf("x%02d", i)
		f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId") VALUES ($1, 'm1', $1, 50, 'anna'), ($2, 'm1', $2, 81, 'bruno')`, source, target)
		f.declare(id, target, 100, 81, map[string][2]int{source: {50, 100}})
		f.answer(id, "DEFEND")
	}

	// Each request is delivered twice, all at once, as a rebalancing consumer group might do.
	var wg sync.WaitGroup
	resolved := make(chan bool, 2*n)
	for i := range 2 * n {
		wg.Go(func() {
			ok, err := f.battles.Resolve(context.Background(), fmt.Sprintf("x%02d", i%n), "m1")
			if err != nil {
				t.Error(err)
			}
			resolved <- ok
		})
	}
	wg.Wait()
	close(resolved)
	applied := 0
	for ok := range resolved {
		if ok {
			applied++
		}
	}
	if applied != n {
		t.Fatalf("%d resolutions applied, want exactly %d", applied, n)
	}
	if got := f.count(`SELECT count(*) FROM "Battle" WHERE status = 'RESOLVED'`); got != n {
		t.Errorf("%d battles resolved", got)
	}
	// Every defender paid ceil(81 x 50%) = 41 exactly once, and the winner received 70% of
	// the 50 + 41 spent, 63, exactly once: on the territory if it was defended, where the
	// attack started from if it was conquered.
	if got := f.count(`SELECT count(*) FROM "Territory" s JOIN "Territory" t ON t.id = 'T' || substr(s.id, 2)
		WHERE s.id LIKE 'S%' AND NOT ((t."ownerEmpireId" = 'bruno' AND t.resources = 103 AND s.resources = 50)
			OR (t."ownerEmpireId" = 'anna' AND t.resources = 40 AND s.resources = 113))`); got != 0 {
		t.Errorf("%d attacked territories with a wrong balance", got)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'BATTLE_RESOLVED'`); got != n {
		t.Errorf("%d events, want %d", got, n)
	}
}

// declareBy stores an open battle between any two empires.
func (f *fixture) declareBy(id, attacker, defender, target, source string, cost, resourcesAtDeclaration int) {
	f.t.Helper()
	f.exec(`INSERT INTO "Battle" (id, "matchId", turn, "attackerEmpireId", "defenderEmpireId", "targetTerritoryId",
			"attackPct", "attackCost", "attackForce", "defenseForce")
		VALUES ($1, 'm1', 3, $2, $3, $4, 50, $5, $6, 1)`, id, attacker, defender, target, cost, resourcesAtDeclaration)
	f.exec(`INSERT INTO "BattleSource" ("battleId", "territoryId", cost, "resourcesAtDeclaration") VALUES ($1, $2, $3, $4)`,
		id, source, cost, resourcesAtDeclaration)
	f.exec(`INSERT INTO "BattleLock" ("territoryId", "battleId") VALUES ($1, $3), ($2, $3)`, target, source, id)
}

func TestConqueringTheCapitalMakesTheEmpireFall(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	// Anna attacks Bruno's capital, B1. Meanwhile Bruno attacks Carla's C1 from B2 (20 of
	// 40 reserved) and Carla attacks a third territory of Bruno, B3, from C2 (20 of 40).
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'A1'`)
	f.exec(`UPDATE "Territory" SET resources = 20 WHERE id IN ('B2', 'C2')`)
	f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId", "protectedUntilTurn") VALUES ('B3', 'm1', 'Bresso', 55, 'bruno', 3)`)
	f.declare("capital", "B1", 200, 300, map[string][2]int{"A1": {100, 200}})
	f.declareBy("bruno-attacks", "bruno", "carla", "C1", "B2", 20, 40)
	f.declareBy("carla-attacks", "carla", "bruno", "B3", "C2", 20, 40)
	f.answer("capital", "SURRENDER")

	if !f.resolve("capital") {
		t.Fatal("not resolved")
	}

	// The conqueror gets the capital with its resources, and with it everything else
	// Bruno had, which keeps what was on it: the cost of the cancelled attack is back
	// on B2. Just acquired, it is protected until the turn ends.
	f.expect("B1", "anna", 300)
	f.expect("B2", "anna", 40)
	f.expect("B3", "anna", 55)
	if got := f.territory("B3").protected; got == nil || *got != 3 {
		t.Errorf("an inherited territory is protected until %v, want turn 3", got)
	}
	// Carla's attack on a territory that changed hands is cancelled too, and refunded.
	f.expect("C2", "carla", 40)
	f.expect("C1", "carla", 70)

	var status string
	var fallenTurn int
	var capital *string
	f.scan(`SELECT status::text, "fallenTurn", "capitalTerritoryId" FROM "Empire" WHERE id = 'bruno'`, &status, &fallenTurn, &capital)
	if status != "FALLEN" || fallenTurn != 3 || capital != nil {
		t.Errorf("Bruno: %s at turn %d, capital %v", status, fallenTurn, capital)
	}
	if got := f.count(`SELECT count(*) FROM "Battle" WHERE outcome = 'CANCELLED' AND status = 'RESOLVED'`); got != 2 {
		t.Errorf("%d battles cancelled, want 2", got)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
	var data string
	f.scan(`SELECT data::text FROM "GameEvent" WHERE type = 'EMPIRE_FALLEN'`, &data)
	if !strings.Contains(data, `"empireName": "Bruno"`) || !strings.Contains(data, `"byEmpireName": "Anna"`) {
		t.Errorf("event data: %s", data)
	}
	// Two empires are still standing: the match goes on.
	f.scan(`SELECT status::text FROM "Match" WHERE id = 'm1'`, &status)
	if status != "ONGOING" {
		t.Errorf("match is %s", status)
	}
}

func TestLosingAnotherTerritoryDoesNotMakeAnEmpireFall(t *testing.T) {
	f := setup(t)
	f.answer("b1", "SURRENDER") // B2 is not Bruno's capital
	f.resolve("b1")
	var status string
	f.scan(`SELECT status::text FROM "Empire" WHERE id = 'bruno'`, &status)
	if status != "ACTIVE" {
		t.Errorf("Bruno is %s", status)
	}
	// With what B2 had, taken away in the retreat.
	f.expect("B1", "bruno", 380)
}

func TestTheLastEmpireStandingWins(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	// Carla has already fallen: when Bruno falls too, Anna is alone.
	f.exec(`UPDATE "Empire" SET status = 'FALLEN', "capitalTerritoryId" = NULL WHERE id = 'carla'`)
	f.exec(`UPDATE "Territory" SET "ownerEmpireId" = NULL WHERE id IN ('C1', 'C2')`)
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'A1'`)
	f.declare("last", "B1", 200, 300, map[string][2]int{"A1": {100, 200}})
	f.answer("last", "SURRENDER")
	f.resolve("last")

	var status string
	var winner *string
	f.scan(`SELECT status::text, "winnerEmpireId" FROM "Match" WHERE id = 'm1'`, &status, &winner)
	if status != "FINISHED" || winner == nil || *winner != "anna" {
		t.Fatalf("match %s, winner %v", status, winner)
	}
	var data string
	f.scan(`SELECT data::text FROM "GameEvent" WHERE type = 'MATCH_FINISHED'`, &data)
	if !strings.Contains(data, `"winnerName": "Anna"`) {
		t.Errorf("event data: %s", data)
	}
	// The last turn never closes: its picture is taken when the match ends.
	if got := f.count(`SELECT count(*) FROM "TurnSnapshot" s JOIN "Match" m ON m.id = s."matchId" AND m."currentTurn" = s.turn`); got != 1 {
		t.Errorf("snapshots of the last turn = %d, want 1", got)
	}
	// Nothing more is resolved in a finished match.
	f.declareBy("late", "anna", "bruno", "B2", "A2", 5, 10)
	f.answer("late", "DEFEND")
	if f.resolve("late") {
		t.Error("a battle was resolved after the match ended")
	}
}

// build stores an active structure with the given effects, as the api would.
func (f *fixture) build(id, empire, territory string, effects string) {
	f.t.Helper()
	f.exec(`INSERT INTO "Structure" (id, "matchId", "territoryId", "empireId", type, "builtTurn", data)
		VALUES ($1, 'm1', $2, $3, 'ANY_NAME', 1, jsonb_build_object('effects', jsonb_build_array($4::text)))`,
		id, territory, empire, effects)
}

func TestAPortStaysWithTheTerritoryItStandsOn(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'A1'`)
	// On Bruno's second territory: a port, and a block that does not stop this attack.
	f.build("port", "bruno", "B2", "SEA_REACH")
	f.build("airport", "bruno", "B2", "AIR_REACH")
	f.build("other", "bruno", "B2", "SOMETHING_ELSE")
	f.declare("harbour", "B2", 200, 300, map[string][2]int{"A1": {100, 200}})
	f.answer("harbour", "SURRENDER")
	f.resolve("harbour")

	// The port is Anna's now, with the territory; what else stood there is lost.
	var owner string
	var active bool
	f.scan(`SELECT "empireId", active FROM "Structure" WHERE id = 'port'`, &owner, &active)
	if owner != "anna" || !active {
		t.Fatalf("the port is of %s, active %v", owner, active)
	}
	// So is an airport.
	if got := f.count(`SELECT count(*) FROM "Structure" WHERE id = 'airport' AND active AND "empireId" = 'anna'`); got != 1 {
		t.Error("the airport did not pass to who conquered the territory")
	}
	if got := f.count(`SELECT count(*) FROM "Structure" WHERE id = 'other' AND active`); got != 0 {
		t.Error("the other structure survived the conquest")
	}
}

func TestASecondCapitalSavesTheEmpireOnce(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'A1'`)
	// The type is unknown to jobs on purpose: only the effect counts.
	f.build("backup", "bruno", "B2", "BACKUP_CAPITAL")
	f.declare("capital", "B1", 200, 300, map[string][2]int{"A1": {100, 200}})
	f.answer("capital", "SURRENDER")
	f.resolve("capital")

	// Bruno lost the municipality and nothing else: B2 is his capital now.
	f.expect("B1", "anna", 300)
	f.expect("B2", "bruno", 80)
	var status string
	var capital string
	f.scan(`SELECT status::text, "capitalTerritoryId" FROM "Empire" WHERE id = 'bruno'`, &status, &capital)
	if status != "ACTIVE" || capital != "B2" {
		t.Fatalf("Bruno is %s with capital %s", status, capital)
	}
	// The reserve is used up: to have another he must buy it again.
	if got := f.count(`SELECT count(*) FROM "Structure" WHERE active`); got != 0 {
		t.Errorf("%d structures still active", got)
	}
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'CAPITAL_PROMOTED'`); got != 1 {
		t.Errorf("%d promotion events", got)
	}

	// Without a reserve, losing the new capital is the end.
	f.exec(`UPDATE "Territory" SET resources = 100, "protectedUntilTurn" = NULL WHERE id = 'A1'`)
	f.declare("again", "B2", 200, 80, map[string][2]int{"A1": {100, 200}})
	f.answer("again", "SURRENDER")
	f.resolve("again")
	f.scan(`SELECT status::text FROM "Empire" WHERE id = 'bruno'`, &status)
	if status != "FALLEN" {
		t.Errorf("Bruno is %s after losing his only capital", status)
	}
}

func TestConqueringTheSecondCapitalOnlyTakesThatTerritory(t *testing.T) {
	f := setup(t)
	f.build("backup", "bruno", "B2", "BACKUP_CAPITAL")
	f.answer("b1", "SURRENDER") // b1 targets B2
	f.resolve("b1")

	f.expect("B2", "anna", 0)
	var status, capital string
	f.scan(`SELECT status::text, "capitalTerritoryId" FROM "Empire" WHERE id = 'bruno'`, &status, &capital)
	if status != "ACTIVE" || capital != "B1" {
		t.Errorf("Bruno is %s with capital %s", status, capital)
	}
	// The structure is lost with the territory, and does not pass to the conqueror.
	if got := f.count(`SELECT count(*) FROM "Structure" WHERE active`); got != 0 {
		t.Errorf("%d structures still active", got)
	}
}

func TestTheAlliancesOfAFallenEmpireEndWithIt(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'A1'`)
	// Bruno is allied with Carla; Anna and Carla have a pact of their own.
	f.exec(`INSERT INTO "Alliance" (id, "matchId", "createdTurn", "aEmpireId", "bEmpireId") VALUES
		('al1', 'm1', 1, 'bruno', 'carla'), ('al2', 'm1', 1, 'anna', 'carla')`)
	f.declare("capital", "B1", 200, 300, map[string][2]int{"A1": {100, 200}})
	f.answer("capital", "SURRENDER")
	f.resolve("capital")

	// Bruno fell: his pact is gone, the one he had no part in stays.
	var left string
	f.scan(`SELECT string_agg(id, ',') FROM "Alliance"`, &left)
	if left != "al2" {
		t.Errorf("alliances left: %q, want al2", left)
	}
}

// A round nobody wins clearly leaves the battle open: both paid, the territories stay
// locked, and the next round is fought in a later turn with what is there then.
func TestARoundNobodyWinsClearlyOpensASiege(t *testing.T) {
	f := setup(t)
	// 120 x 0.5 = 60 against 80 x 0.7 = 56: ahead, by less than the 30% needed.
	f.rolls = []float64{0.5, 0.7}
	f.answer("b1", "DEFEND")
	if !f.resolve("b1") {
		t.Fatal("round not fought")
	}
	var status, result string
	var round, fought int
	var response *string
	f.scan(`SELECT status::text, round, "foughtTurn", response::text FROM "Battle" WHERE id = 'b1'`, &status, &round, &fought, &response)
	if status != "OPEN" || round != 2 || fought != 3 || response != nil {
		t.Fatalf("battle %s at round %d, fought in turn %d, answer %v", status, round, fought, response)
	}
	f.scan(`SELECT result FROM "BattleRound" WHERE "battleId" = 'b1' AND round = 1`, &result)
	if result != "DRAW" {
		t.Errorf("round 1 = %s", result)
	}
	// The defender paid ceil(80 x 50%) = 40 and keeps the territory; nothing is unlocked.
	if got := f.territory("B2"); got.owner != "bruno" || got.resources != 40 {
		t.Errorf("B2 = %+v", got)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 3 {
		t.Errorf("%d territories locked, want 3", got)
	}
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'BATTLE_ROUND'`); got != 1 {
		t.Errorf("%d round events", got)
	}

	// One round a turn: a second answer in the same turn waits.
	f.answer("b1", "DEFEND")
	if f.resolve("b1") {
		t.Fatal("a second round was fought in the same turn")
	}

	// The next turn: Anna fights with the 50 + 10 left and pays 60% of them again, 30 + 6;
	// Bruno with the 40 left, paying 20. 60 x 0.9 = 54 against 40 x 0.2 = 8: conquered.
	f.exec(`UPDATE "Match" SET "currentTurn" = 4`)
	f.rolls = []float64{0.9, 0.2}
	if !f.resolve("b1") {
		t.Fatal("second round not fought")
	}
	var outcome string
	var attackCost, defenseCost, spoils int
	f.scan(`SELECT outcome::text, "attackCost", "defenseCost", spoils FROM "Battle" WHERE id = 'b1'`, &outcome, &attackCost, &defenseCost, &spoils)
	// 70% of everything spent in the two rounds: 60 + 36 + 40 + 20 = 156.
	if outcome != battle.OutcomeAttackerWon || attackCost != 96 || defenseCost != 60 || spoils != 109 {
		t.Errorf("outcome %s, attack cost %d, defense cost %d, spoils %d", outcome, attackCost, defenseCost, spoils)
	}
	if got := f.territory("B2"); got.owner != "anna" || got.resources != 20 {
		t.Errorf("B2 = %+v", got)
	}
	a1, a2 := f.territory("A1"), f.territory("A2")
	if a1.resources+a2.resources != 50-30+10-6+109 {
		t.Errorf("A1 %d + A2 %d", a1.resources, a2.resources)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
}

// A siege lasts as long as the two sides keep it: no round is the last one.
func TestASiegeHasNoLastRound(t *testing.T) {
	f := setup(t)
	f.rolls = []float64{0.5, 0.5}
	for turn := 3; turn < 9; turn++ {
		f.exec(`UPDATE "Match" SET "currentTurn" = $1`, turn)
		// Kept even: the same on both sides before every round.
		f.exec(`UPDATE "Territory" SET resources = 60 WHERE id IN ('A1', 'B2')`)
		f.exec(`UPDATE "Territory" SET resources = 0 WHERE id = 'A2'`)
		f.exec(`UPDATE "Battle" SET "attackForce" = 60, "defenseForce" = 60 WHERE round = 1`)
		f.answer("b1", "DEFEND")
		if !f.resolve("b1") {
			t.Fatalf("turn %d: round not fought", turn)
		}
	}
	if got := f.count(`SELECT round FROM "Battle" WHERE id = 'b1' AND status = 'OPEN'`); got != 7 {
		t.Errorf("round = %d, want 7", got)
	}
}

// The attacker can give a siege up: the defender keeps the territory and is paid.
func TestTheAttackerWithdrawsFromASiege(t *testing.T) {
	f := setup(t)
	f.rolls = []float64{0.5, 0.7}
	f.answer("b1", "DEFEND")
	f.resolve("b1")
	f.exec(`UPDATE "Battle" SET withdrawn = true WHERE id = 'b1'`)
	// At once, in the turn of the round just fought.
	if !f.resolve("b1") {
		t.Fatal("withdrawal not applied")
	}
	var outcome string
	f.scan(`SELECT outcome::text FROM "Battle" WHERE id = 'b1' AND status = 'RESOLVED'`, &outcome)
	if outcome != battle.OutcomeWithdrawn {
		t.Errorf("outcome = %s", outcome)
	}
	// 40 left after the round, plus 70% of the 60 + 40 spent.
	if got := f.territory("B2"); got.owner != "bruno" || got.resources != 110 {
		t.Errorf("B2 = %+v", got)
	}
	if got := f.count(`SELECT count(*) FROM "BattleLock"`); got != 0 {
		t.Errorf("%d territories still locked", got)
	}
}

// A capital asks for a wider margin: what takes a territory at once besieges a capital.
func TestACapitalIsHarderToTakeInOneRound(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	f.declare("c1", "B1", 600, 300, map[string][2]int{"A1": {50, 100}})
	// 600 x 0.5 = 300 against 300 x 0.7 = 210: 43% ahead, enough for a territory (30%)
	// and not for a capital (60%).
	f.rolls = []float64{0.5, 0.7}
	f.answer("c1", "DEFEND")
	f.resolve("c1")
	if got := f.count(`SELECT count(*) FROM "Battle" WHERE id = 'c1' AND status = 'OPEN' AND round = 2`); got != 1 {
		t.Error("the capital fell in one round")
	}
	if got := f.territory("B1"); got.owner != "bruno" {
		t.Errorf("B1 = %+v", got)
	}
}
