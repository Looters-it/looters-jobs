package turnclose_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gioco/jobs/internal/turnclose"
)

// These tests need a database migrated by the api: set TEST_DATABASE_URL (see .env.example).
type fixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	closer *turnclose.Closer
	now    time.Time
}

var turnEnd = time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)

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
	f := &fixture{t: t, pool: pool, now: turnEnd.Add(2 * time.Second)}
	f.closer = &turnclose.Closer{
		Pool: pool,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:  func() time.Time { return f.now },
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

	// Geography: five municipalities, needed because territories point to them.
	f.exec(`INSERT INTO "GeoArea" (id, name) VALUES ('a1', 'Area')`)
	f.exec(`INSERT INTO "GeoUpload" (id, "areaId", filename, format, "storagePath") VALUES ('up1', 'a1', 'f', 'GEOJSON', 'f')`)
	for i := 1; i <= 5; i++ {
		f.exec(`INSERT INTO "GeoUnit" (id, "areaId", "uploadId", name, "centroidLon", "centroidLat", "areaKm2", geom, "geomSimple")
			VALUES ($1, 'a1', 'up1', 'Comune', 9, 45, 1,
				ST_Multi(ST_GeomFromText('POLYGON((0 0,1 0,1 1,0 1,0 0))', 4326)),
				ST_Multi(ST_GeomFromText('POLYGON((0 0,1 0,1 1,0 1,0 0))', 4326)))`, 1000+i)
	}

	// Anna: capital A1 (weight 3) and A2 (weight 1), equal split, one annexation used.
	// Bruno: capital B1 only. Carla has fallen and still holds nothing. F is free.
	f.exec(`INSERT INTO "Empire" (id, "matchId", "userId", name, color, emblem, "annexedThisTurn", status, "capitalTerritoryId") VALUES
		('anna', 'm1', 'u1', 'Anna', '#111111', 'torre', 1, 'ACTIVE', 'A1'),
		('bruno', 'm1', 'u2', 'Bruno', '#222222', 'luna', 0, 'ACTIVE', 'B1'),
		('carla', 'm1', 'u3', 'Carla', '#333333', 'luna', 0, 'FALLEN', NULL)`)
	f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, weight, "ownerEmpireId", "protectedUntilTurn") VALUES
		('A1', 'm1', 'A1', 100, 3, 'anna', NULL),
		('A2', 'm1', 'A2', 20, 1, 'anna', 3),
		('B1', 'm1', 'B1', 5, 1, 'bruno', NULL),
		('F', 'm1', 'F', 77, 1, NULL, NULL)`)
	f.exec(`INSERT INTO "TerritoryUnit" ("matchId", "territoryId", "geoUnitId") VALUES
		('m1', 'A1', 1001), ('m1', 'A2', 1002), ('m1', 'A2', 1003), ('m1', 'B1', 1004), ('m1', 'F', 1005)`)
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) close(turn int) turnclose.Outcome {
	f.t.Helper()
	outcome, err := f.closer.Close(context.Background(), "m1", turn)
	if err != nil {
		f.t.Fatal(err)
	}
	return outcome
}

func (f *fixture) resources(territoryID string) (n int) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT resources FROM "Territory" WHERE id = $1`, territoryID).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) count(query string, args ...any) (n int) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) expectResources(want map[string]int) {
	f.t.Helper()
	for id, amount := range want {
		if got := f.resources(id); got != amount {
			f.t.Errorf("territory %s holds %d, want %d", id, got, amount)
		}
	}
}

func TestCloseCreditsIncomeAndOpensTheNextTurn(t *testing.T) {
	f := setup(t)
	if got := f.close(3); got != turnclose.Closed {
		t.Fatalf("outcome = %s", got)
	}

	// Anna controls three municipalities: 50 + 10 x 3 = 80, in equal parts between her two
	// territories. Bruno controls one: 60. The free territory keeps its hidden resources.
	f.expectResources(map[string]int{"A1": 140, "A2": 60, "B1": 65, "F": 77})

	var turn int
	var endsAt time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT "currentTurn", "turnEndsAt" FROM "Match" WHERE id = 'm1'`).Scan(&turn, &endsAt); err != nil {
		t.Fatal(err)
	}
	// The next turn ends twelve hours after this one was due, although the job ran late.
	if turn != 4 || endsAt.Format("2006-01-02 15:04:05") != "2026-10-10 21:00:00" {
		t.Errorf("turn %d ends at %s", turn, endsAt)
	}
	if got := f.count(`SELECT "annexedThisTurn" FROM "Empire" WHERE id = 'anna'`); got != 0 {
		t.Errorf("annexations of the turn not reset: %d", got)
	}
	// Protection stored for turn 3 no longer covers turn 4.
	if got := f.count(`SELECT count(*) FROM "Territory" t JOIN "Match" m ON m.id = t."matchId" WHERE t."protectedUntilTurn" >= m."currentTurn"`); got != 0 {
		t.Errorf("%d territories still protected in the new turn", got)
	}

	// Ownership at the end of the turn, by municipality; the free one is absent.
	var ownership string
	if err := f.pool.QueryRow(context.Background(), `SELECT ownership::text FROM "TurnSnapshot" WHERE "matchId" = 'm1' AND turn = 3`).Scan(&ownership); err != nil {
		t.Fatal(err)
	}
	if ownership != `{"1001": "anna", "1002": "anna", "1003": "anna", "1004": "bruno"}` {
		t.Errorf("snapshot = %s", ownership)
	}

	// The closing of the next turn is queued for when it ends.
	if got := f.count(`SELECT count(*) FROM "Job" WHERE type = 'turn.close' AND status = 'PENDING'
		AND payload = '{"matchId": "m1", "turn": 4}'::jsonb AND "runAt" = '2026-10-10 21:00:00'`); got != 1 {
		t.Errorf("%d jobs queued for turn 4, want 1", got)
	}
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'TURN_CLOSED' AND turn = 3`); got != 1 {
		t.Errorf("%d close events", got)
	}
}

func TestClosingTheSameTurnTwiceAppliesItOnce(t *testing.T) {
	f := setup(t)
	f.close(3)
	// The job is delivered again (a crash before it was marked done, a retry).
	if got := f.close(3); got != turnclose.Stale {
		t.Fatalf("second run outcome = %s", got)
	}
	f.expectResources(map[string]int{"A1": 140, "A2": 60, "B1": 65})
	if got := f.count(`SELECT count(*) FROM "Job" WHERE type = 'turn.close'`); got != 1 {
		t.Errorf("%d jobs queued after a second run", got)
	}
	if got := f.count(`SELECT "currentTurn" FROM "Match" WHERE id = 'm1'`); got != 4 {
		t.Errorf("turn is %d after a second run", got)
	}
}

func TestSeveralTurnsInARow(t *testing.T) {
	f := setup(t)
	for turn := 3; turn <= 6; turn++ {
		if got := f.close(turn); got != turnclose.Closed {
			t.Fatalf("turn %d: outcome = %s", turn, got)
		}
		f.now = f.now.Add(12 * time.Hour)
	}
	// Four closings: Anna earned 80 each time, Bruno 60.
	f.expectResources(map[string]int{"A1": 260, "A2": 180, "B1": 245})
	if got := f.count(`SELECT count(*) FROM "TurnSnapshot" WHERE "matchId" = 'm1'`); got != 4 {
		t.Errorf("%d snapshots, want 4", got)
	}
}

func TestWeightedSplitAndFallenEmpires(t *testing.T) {
	f := setup(t)
	f.exec(`UPDATE "Empire" SET "allocationMode" = 'WEIGHTED' WHERE id = 'anna'`)
	// A fallen empire earns nothing, even if a territory were still in its name.
	f.exec(`UPDATE "Territory" SET "ownerEmpireId" = 'carla' WHERE id = 'F'`)

	f.close(3)
	// 80 split 3 to 1: 60 and 20.
	f.expectResources(map[string]int{"A1": 160, "A2": 40, "F": 77})
}

func TestDoesNotCloseWhatIsNotTheCurrentTurn(t *testing.T) {
	f := setup(t)
	for _, turn := range []int{2, 4} {
		if got := f.close(turn); got != turnclose.Stale {
			t.Fatalf("turn %d: outcome = %s", turn, got)
		}
	}
	f.exec(`UPDATE "Match" SET status = 'FINISHED' WHERE id = 'm1'`)
	if got := f.close(3); got != turnclose.Stale {
		t.Fatalf("finished match: outcome = %s", got)
	}
	f.expectResources(map[string]int{"A1": 100, "A2": 20, "B1": 5})
}

func TestRefusesToCloseATurnBeforeItEnds(t *testing.T) {
	f := setup(t)
	f.now = turnEnd.Add(-time.Minute)
	if _, err := f.closer.Close(context.Background(), "m1", 3); err == nil {
		t.Fatal("expected an error for a turn that has not ended")
	}
	f.expectResources(map[string]int{"A1": 100})
}
