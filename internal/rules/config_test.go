package rules

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testdata/default-config.json is the api's DEFAULT_MATCH_RULES, exported as is.
// Regenerate it when the api adds a rule (see README).
func TestParseDefaultConfigFromAPI(t *testing.T) {
	raw, err := os.ReadFile("testdata/default-config.json")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := ParseMatchRules(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rules.StartResources != 200 || rules.IncomeBase != 50 || rules.IncomePerUnit != 10 ||
		rules.AttackPct != 50 || rules.DefensePct != 50 || rules.MergeCostTurns != 0.5 || rules.BlockCost != 150 {
		t.Fatalf("unexpected defaults: %+v", rules)
	}

	// Every key the api writes must map to a field, and every field must be written.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	fields := reflect.TypeFor[MatchRules]()
	if len(keys) != fields.NumField() {
		t.Fatalf("api config has %d keys, MatchRules has %d fields", len(keys), fields.NumField())
	}
	for i := range fields.NumField() {
		tag := fields.Field(i).Tag.Get("json")
		if _, ok := keys[tag]; !ok {
			t.Errorf("field %s (%q) is missing from the api config", fields.Field(i).Name, tag)
		}
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	if _, err := ParseMatchRules([]byte(`{"annexCost":50,"newRule":1}`)); err == nil {
		t.Fatal("expected an error for an unknown key")
	}
}
