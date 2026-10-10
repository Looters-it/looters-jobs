package endgame_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"looters/jobs/internal/endgame"
)

// These tests need a database migrated by the api: set TEST_DATABASE_URL (see .env.example).
type fixture struct {
	t    *testing.T
	pool *pgxpool.Pool
}

// setup creates a running match with one empire for each user, each on one territory.
func setup(t *testing.T, users ...string) *fixture {
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
	for _, table := range []string{"Match", "User"} {
		f.exec(`DELETE FROM "` + table + `"`)
	}
	config, err := os.ReadFile("../rules/testdata/default-config.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range users {
		f.exec(`INSERT INTO "User" (id, username, email, "passwordHash", "isBot") VALUES ($1, $1, $1 || '@example.test', 'x', $2)`,
			user, user == "bot")
	}
	f.exec(`INSERT INTO "Match" (id, name, "ownerId", status, "startAt", "turnHours", "currentTurn", config)
		VALUES ('m1', 'Prova', $1, 'ONGOING', now(), 12, 3, $2::jsonb)`, users[0], string(config))
	for _, user := range users {
		f.exec(`INSERT INTO "Empire" (id, "matchId", "userId", name, color, emblem) VALUES ('e' || $1, 'm1', $1, $1, '#' || md5($1)::varchar(6), 'torre')`, user)
		f.exec(`INSERT INTO "Territory" (id, "matchId", name, resources, "ownerEmpireId") VALUES ('t' || $1, 'm1', $1, 250, 'e' || $1)`, user)
		f.exec(`UPDATE "Empire" SET "capitalTerritoryId" = 't' || $1 WHERE id = 'e' || $1`, user)
	}
	return f
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) text(query string, args ...any) string {
	f.t.Helper()
	var value *string
	if err := f.pool.QueryRow(context.Background(), query, args...).Scan(&value); err != nil {
		f.t.Fatal(err)
	}
	if value == nil {
		return ""
	}
	return *value
}

func (f *fixture) abandon(empireID string) bool {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	ended, err := endgame.Abandon(ctx, tx, "m1", empireID)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
	return ended
}

func TestAbandonFreesEverythingAndTheMatchGoesOn(t *testing.T) {
	f := setup(t, "anna", "bruno", "carla")
	if f.abandon("eanna") {
		t.Fatal("the match ended with two players left")
	}
	if got := f.text(`SELECT status::text FROM "Empire" WHERE id = 'eanna'`); got != "FALLEN" {
		t.Errorf("empire status = %s, want FALLEN", got)
	}
	// The capital too is nobody's, and keeps what it held.
	if got := f.text(`SELECT coalesce("ownerEmpireId", 'free') || ':' || resources FROM "Territory" WHERE id = 'tanna'`); got != "free:250" {
		t.Errorf("territory = %s, want free:250", got)
	}
	if got := f.text(`SELECT status::text FROM "Match" WHERE id = 'm1'`); got != "ONGOING" {
		t.Errorf("match status = %s, want ONGOING", got)
	}
	if got := f.text(`SELECT count(*)::text FROM "GameEvent" WHERE type = 'EMPIRE_ABANDONED' AND "empireId" = 'eanna'`); got != "1" {
		t.Errorf("EMPIRE_ABANDONED events = %s, want 1", got)
	}
	// Delivered twice: nothing more happens.
	f.abandon("eanna")
	if got := f.text(`SELECT count(*)::text FROM "GameEvent" WHERE type = 'EMPIRE_ABANDONED'`); got != "1" {
		t.Errorf("EMPIRE_ABANDONED events after a second delivery = %s, want 1", got)
	}
}

func TestAbandonLeavingOneEmpireEndsTheMatch(t *testing.T) {
	f := setup(t, "anna", "bruno")
	if !f.abandon("eanna") {
		t.Fatal("the match did not end with one empire left")
	}
	if got := f.text(`SELECT status::text || ':' || "winnerEmpireId" FROM "Match" WHERE id = 'm1'`); got != "FINISHED:ebruno" {
		t.Errorf("match = %s, want FINISHED:ebruno", got)
	}
}

func TestAbandonLeavingOnlyBotsEndsTheMatch(t *testing.T) {
	f := setup(t, "anna", "bot", "bruno")
	f.exec(`UPDATE "User" SET "isBot" = true WHERE id = 'bruno'`)
	if !f.abandon("eanna") {
		t.Fatal("the match went on among bots")
	}
	if got := f.text(`SELECT status::text FROM "Match" WHERE id = 'm1'`); got != "FINISHED" {
		t.Errorf("match status = %s, want FINISHED", got)
	}
}
