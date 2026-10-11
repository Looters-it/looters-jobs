package endgame_test

import (
	"context"
	"testing"

	"looters/jobs/internal/endgame"
)

func (f *fixture) finish(turn int, winners ...string) {
	f.t.Helper()
	ctx := context.Background()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := endgame.Finish(ctx, tx, "m1", winners, turn); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
}

func TestFinishAwardsEveryPlaceMoreTheLongerAndTheFullerTheMatch(t *testing.T) {
	f := setup(t, "anna", "bruno", "carla", "bot")
	// Carla fell first, the bot later; Bruno stands with less than Anna, who wins.
	f.exec(`UPDATE "Empire" SET status = 'FALLEN', "fallenTurn" = 4 WHERE id = 'ecarla'`)
	f.exec(`UPDATE "Empire" SET status = 'FALLEN', "fallenTurn" = 9 WHERE id = 'ebot'`)
	f.exec(`UPDATE "Territory" SET "ownerEmpireId" = NULL WHERE id IN ('tcarla', 'tbot')`)
	f.exec(`UPDATE "Territory" SET resources = 100 WHERE id = 'tbruno'`)
	f.finish(23, "eanna")

	// Four empires and 23 turns: a share is worth 4 + 23 / 5 = 8.
	const places = `SELECT string_agg(id || ':' || place || ':' || award, ' ' ORDER BY place) FROM "Empire"`
	if got, want := f.text(places), "eanna:1:32 ebruno:2:24 ebot:3:16 ecarla:4:8"; got != want {
		t.Errorf("places = %s, want %s", got, want)
	}
	const ranking = `SELECT string_agg(id || ':' || elo || ':' || "ratedMatches" || ':' || "ratedTurns", ' ' ORDER BY id) FROM "User"`
	if got, want := f.text(ranking), "anna:32:1:23 bot:0:0:0 bruno:24:1:23 carla:8:1:4"; got != want {
		t.Errorf("ranking = %s, want %s", got, want)
	}
	// Ended twice: nothing is given again.
	f.finish(23, "eanna")
	if got := f.text(`SELECT elo::text FROM "User" WHERE id = 'anna'`); got != "32" {
		t.Errorf("elo after a second end = %s, want 32", got)
	}
}

func TestWinnersLevelShareTheFirstPlace(t *testing.T) {
	f := setup(t, "anna", "bruno", "carla")
	f.exec(`UPDATE "Territory" SET resources = 10 WHERE id = 'tcarla'`)
	f.finish(0, "eanna", "ebruno")
	const places = `SELECT string_agg(id || ':' || place || ':' || award, ' ' ORDER BY id) FROM "Empire"`
	if got, want := f.text(places), "eanna:1:9 ebruno:1:9 ecarla:3:3"; got != want {
		t.Errorf("places = %s, want %s", got, want)
	}
}
