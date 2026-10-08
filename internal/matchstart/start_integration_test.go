package matchstart_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"looters/jobs/internal/matchstart"
)

// These tests need a database migrated by the api: set TEST_DATABASE_URL (see .env.example).
type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	starter *matchstart.Starter
	now     time.Time
}

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
	f := &fixture{t: t, pool: pool, now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	f.starter = &matchstart.Starter{
		Pool: pool,
		Log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:  func() time.Time { return f.now },
	}
	for _, table := range []string{"Match", "User"} {
		f.exec(`DELETE FROM "` + table + `"`)
	}
	f.exec(`INSERT INTO "User" (id, username, email, "passwordHash") VALUES ('u1','anna','a@example.test','x'), ('u2','bruno','b@example.test','x')`)
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// match creates a match starting at f.now with the given number of empires, each on one territory.
func (f *fixture) match(status string, players int) {
	config, err := os.ReadFile("../rules/testdata/default-config.json")
	if err != nil {
		f.t.Fatal(err)
	}
	f.exec(`INSERT INTO "Match" (id, name, "ownerId", status, "startAt", "turnHours", config)
		VALUES ('m1', 'Prova', 'u1', $1::"MatchStatus", $2, 12, $3::jsonb)`, status, f.now, string(config))
	f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources) VALUES ('free', 'm1', 'Libero', 40)`)
	for i, user := range []string{"u1", "u2"}[:players] {
		empire := "e" + user
		territory := "t" + user
		f.exec(`INSERT INTO "Empire" (id, "matchId", "userId", name, color, emblem) VALUES ($1, 'm1', $2, $1, $3, 'torre')`,
			empire, user, []string{"#111111", "#222222"}[i])
		f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId", "protectedUntilTurn") VALUES ($1, 'm1', $1, 250, $2, 0)`,
			territory, empire)
	}
}

func (f *fixture) start() matchstart.Outcome {
	f.t.Helper()
	outcome, err := f.starter.Start(context.Background(), "m1")
	if err != nil {
		f.t.Fatal(err)
	}
	return outcome
}

func (f *fixture) matchState() (status string, turn int, turnEndsAt *time.Time) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status::text, "currentTurn", "turnEndsAt" FROM "Match" WHERE id = 'm1'`).Scan(&status, &turn, &turnEndsAt); err != nil {
		f.t.Fatal(err)
	}
	return
}

func (f *fixture) count(query string) (n int) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), query).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return
}

func TestStartsAnOpenMatchAtItsTime(t *testing.T) {
	f := setup(t)
	f.match("OPEN", 2)
	// The job runs a little late: turns are still counted from the scheduled start.
	scheduled := f.now
	f.now = f.now.Add(7 * time.Second)

	if got := f.start(); got != matchstart.Started {
		t.Fatalf("outcome = %s", got)
	}
	status, turn, turnEndsAt := f.matchState()
	if status != "RUNNING" || turn != 1 {
		t.Fatalf("status=%s turn=%d", status, turn)
	}
	if want := scheduled.Add(12 * time.Hour); turnEndsAt == nil || !turnEndsAt.Equal(time.Date(want.Year(), want.Month(), want.Day(), want.Hour(), 0, 0, 0, turnEndsAt.Location())) {
		t.Fatalf("turnEndsAt = %v, want %v", turnEndsAt, want)
	}
	// Capitals are protected through the first turn; free territories are untouched.
	if got := f.count(`SELECT count(*) FROM "Territory" WHERE "protectedUntilTurn" = 1`); got != 2 {
		t.Errorf("%d protected territories, want 2", got)
	}
	if got := f.count(`SELECT count(*) FROM "Territory" WHERE id = 'free' AND "protectedUntilTurn" IS NULL AND resources = 40`); got != 1 {
		t.Error("the free territory was changed")
	}
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'MATCH_STARTED' AND turn = 1`); got != 1 {
		t.Errorf("%d start events, want 1", got)
	}

	// Running the job again changes nothing.
	if got := f.start(); got != matchstart.NotOpen {
		t.Fatalf("second run outcome = %s", got)
	}
	if got := f.count(`SELECT count(*) FROM "GameEvent" WHERE type = 'MATCH_STARTED'`); got != 1 {
		t.Errorf("%d start events after a second run", got)
	}
}

func TestDoesNotStartWhatIsNotDue(t *testing.T) {
	cases := map[string]struct {
		status  string
		players int
		early   bool
		want    matchstart.Outcome
	}{
		"draft":             {"DRAFT", 2, false, matchstart.NotOpen},
		"already running":   {"RUNNING", 2, false, matchstart.NotOpen},
		"start moved later": {"OPEN", 2, true, matchstart.Rescheduled},
		"one player":        {"OPEN", 1, false, matchstart.TooFewPlayers},
		"nobody":            {"OPEN", 0, false, matchstart.TooFewPlayers},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.match(tc.status, tc.players)
			if tc.early {
				f.now = f.now.Add(-time.Hour) // the job fires an hour before the new start
			}
			if got := f.start(); got != tc.want {
				t.Fatalf("outcome = %s, want %s", got, tc.want)
			}
			if status, _, _ := f.matchState(); status != tc.status {
				t.Fatalf("status changed to %s", status)
			}
		})
	}
}

func TestMissingMatchIsNotAnError(t *testing.T) {
	f := setup(t)
	if got := f.start(); got != matchstart.NotOpen {
		t.Fatalf("outcome = %s", got)
	}
}
