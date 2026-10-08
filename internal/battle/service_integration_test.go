package battle_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/battle"
	"gioco/jobs/internal/turnclose"
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
		VALUES ('m1', 'Prova', 'u1', 'RUNNING', $1, 12, 3, $2, $3::jsonb)`, turnEnd.Add(-36*time.Hour), turnEnd, string(config))
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

	// The territory passes to the attacker with everything on it: no defence was paid.
	f.expect("B2", "anna", 80)
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
	// Nothing comes back to the attacker: the cost of the attack is spent.
	f.expect("A1", "anna", 50)
	f.expect("A2", "anna", 10)
	f.expect("B1", "bruno", 300)

	var outcome, resolver string
	var rollAttack, rollDefense float64
	var defenseCost int
	f.scan(`SELECT outcome::text, resolver, "rollAttack", "rollDefense", "defenseCost" FROM "Battle" WHERE id = 'b1'`,
		&outcome, &resolver, &rollAttack, &rollDefense, &defenseCost)
	// Resources, numbers drawn and outcome are all on record.
	if outcome != battle.OutcomeAttackerWon || resolver != "uniform-product-v1" || rollAttack != 0.9 || rollDefense != 0.2 || defenseCost != 40 {
		t.Errorf("battle: %s by %s, rolls %v/%v, defence cost %d", outcome, resolver, rollAttack, rollDefense, defenseCost)
	}
}

func TestDefenceWonByTheDefender(t *testing.T) {
	f := setup(t)
	f.rolls = []float64{0.1, 0.9} // 12 against 72
	f.answer("b1", "DEFEND")
	f.resolve("b1")

	// The territory stays, but defending still cost half of what was on it.
	f.expect("B2", "bruno", 40)
	f.expect("A1", "anna", 50)
	if got := f.territory("B2").protected; got != nil {
		t.Errorf("a territory that did not change hands became protected until %d", *got)
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
	if strings.ContainsAny(data, "0123456789") {
		t.Errorf("event data contains a number: %s", data)
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
	// Every defender paid ceil(81 x 50%) = 41 exactly once: 40 is left wherever the territory is now.
	if got := f.count(`SELECT count(*) FROM "Territory" WHERE id LIKE 'T%' AND resources <> 40`); got != 0 {
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

	// The conqueror gets the capital only, with its resources.
	f.expect("B1", "anna", 300)
	// Everything else Bruno had is no man's land and keeps what was on it: the cost of
	// the cancelled attack is back on B2, and B3 is no longer protected.
	f.expect("B2", "", 40)
	f.expect("B3", "", 55)
	if got := f.territory("B3").protected; got != nil {
		t.Errorf("a freed territory is still protected until %d", *got)
	}
	// Carla's attack on a territory that is now free is cancelled too, and refunded.
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
	if status != "RUNNING" {
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
	f.expect("B1", "bruno", 300)
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

	f.expect("B2", "anna", 80)
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

func TestAFallenEmpireLeavesItsAlliance(t *testing.T) {
	f := setup(t)
	f.exec(`DELETE FROM "Battle"`)
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'A1'`)
	f.exec(`INSERT INTO "Alliance" (id, "matchId", "createdTurn") VALUES ('al1', 'm1', 1)`)
	f.exec(`INSERT INTO "AllianceMember" ("empireId", "allianceId", "joinedTurn") VALUES ('bruno', 'al1', 1), ('carla', 'al1', 1)`)
	f.declare("capital", "B1", 200, 300, map[string][2]int{"A1": {100, 200}})
	f.answer("capital", "SURRENDER")
	f.resolve("capital")

	// Bruno fell: Carla is left alone, and an alliance of one is dissolved.
	if got := f.count(`SELECT count(*) FROM "AllianceMember"`); got != 0 {
		t.Errorf("%d members left", got)
	}
	if got := f.count(`SELECT count(*) FROM "Alliance" WHERE "dissolvedAt" IS NOT NULL`); got != 1 {
		t.Error("the alliance was not dissolved")
	}
}
