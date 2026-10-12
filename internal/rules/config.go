// Package rules holds the game rules that run at turn close.
package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MatchRules is the per-match configuration stored in full in Match.config.
// Field names are shared with the api (src/rules/config.ts): the api owns
// the defaults and validation, this service only reads the stored object.
type MatchRules struct {
	MinPlayersToStart int  `json:"minPlayersToStart"`
	LateJoin          bool `json:"lateJoin"`
	// Bots are played by the api: jobs only carries their settings along.
	BotsEnabled       bool    `json:"botsEnabled"`
	BotsMax           int     `json:"botsMax"`
	BotsAtStart       int     `json:"botsAtStart"`
	BotSpawnChancePct float64 `json:"botSpawnChancePct"`

	StartResources int     `json:"startResources"`
	HiddenMin      int     `json:"hiddenMin"`
	HiddenMax      int     `json:"hiddenMax"`
	HiddenMean     float64 `json:"hiddenMean"`
	HiddenSd       float64 `json:"hiddenSd"`
	IncomeBase     int     `json:"incomeBase"`
	IncomePerUnit  int     `json:"incomePerUnit"`

	AnnexCost    int `json:"annexCost"`
	AnnexPerTurn int `json:"annexPerTurn"`

	AttacksPerTurn     int     `json:"attacksPerTurn"`
	AttackPct          float64 `json:"attackPct"`
	AttackStepPct      float64 `json:"attackStepPct"`
	AttackMaxPct       float64 `json:"attackMaxPct"`
	DefensePct         float64 `json:"defensePct"`
	SurrenderRefundPct float64 `json:"surrenderRefundPct"`
	BattleSpoilsPct    float64 `json:"battleSpoilsPct"`
	// By how much a side must beat the other for a round to end a battle; below it the
	// battle stays open as a siege. The second is for an attack on a capital.
	SiegeMarginPct        float64 `json:"siegeMarginPct"`
	SiegeCapitalMarginPct float64 `json:"siegeCapitalMarginPct"`

	CapitalMoveEveryTurns    int     `json:"capitalMoveEveryTurns"`
	AccumAlertPct            float64 `json:"accumAlertPct"`
	AccumAlertMinTerritories int     `json:"accumAlertMinTerritories"`

	MergeCostTurns         float64 `json:"mergeCostTurns"`
	MergeMaxUnits          int     `json:"mergeMaxUnits"`
	SecondCapitalCostTurns float64 `json:"secondCapitalCostTurns"`
	BlockCost              int     `json:"blockCost"`
	BlockTurns             int     `json:"blockTurns"`
	BlockCooldownTurns     int     `json:"blockCooldownTurns"`
	PortCost               int     `json:"portCost"`
	PortFoundIslandPct     int     `json:"portFoundIslandPct"`
	PortFoundCoastPct      int     `json:"portFoundCoastPct"`

	AllianceMaxAllies        int `json:"allianceMaxAllies"`
	AllianceLeaveNoticeTurns int `json:"allianceLeaveNoticeTurns"`

	BoardMaxChars           int `json:"boardMaxChars"`
	BoardMessageIntervalSec int `json:"boardMessageIntervalSec"`
}

// ParseMatchRules decodes Match.config. Unknown keys are an error: they mean
// the api has a rule this service does not know about yet.
func ParseMatchRules(raw []byte) (MatchRules, error) {
	// A match created before a rule existed plays by the default of that rule.
	rules := MatchRules{BattleSpoilsPct: 70, AttacksPerTurn: 5}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return MatchRules{}, fmt.Errorf("match config: %w", err)
	}
	return rules, nil
}
