package battlequeue

import "testing"

func TestDecode(t *testing.T) {
	message, err := Decode([]byte(`{"battleId":"b1","matchId":"m1"}`))
	if err != nil || message != (Message{BattleID: "b1", MatchID: "m1"}) {
		t.Fatalf("got %+v, %v", message, err)
	}
	for _, bad := range []string{``, `not json`, `{}`, `{"battleId":"b1"}`, `{"matchId":"m1"}`} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("Decode(%q): expected an error", bad)
		}
	}
}
