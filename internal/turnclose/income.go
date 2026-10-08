// Package turnclose closes a turn of a match: it credits the income of every
// empire, resets what lasts one turn, saves who owns what, and opens the next turn.
package turnclose

// Share is a territory that receives part of an empire's income.
type Share struct {
	TerritoryID string
	// Weight in the split of the income; used by the weighted mode only.
	Weight int
}

// Allocation modes, as stored in Empire.allocationMode.
const (
	ModeEqual    = "EQUAL"
	ModeWeighted = "WEIGHTED"
)

// Income is what an empire earns in a turn: a fixed base plus a share for
// every municipality it controls (PROMPT §2.3). The api computes the same
// amount to show it to the player (src/rules/income.ts).
func Income(base, perUnit, units int) int {
	return base + perUnit*units
}

// Distribute splits an income among the territories of an empire, following
// its permanent rule (PROMPT §2.4): in equal parts, or in proportion to the
// weights. Amounts are whole numbers; what the division leaves over goes to
// the capital, or to the first territory if the capital is not among them.
// Nothing is lost: the amounts always add up to the income.
func Distribute(income int, mode string, shares []Share, capitalID string) map[string]int {
	amounts := make(map[string]int, len(shares))
	if len(shares) == 0 || income <= 0 {
		return amounts
	}

	totalWeight := 0
	for _, share := range shares {
		totalWeight += share.Weight
	}
	// Weights that are all zero say nothing: fall back to equal parts.
	weighted := mode == ModeWeighted && totalWeight > 0

	given := 0
	for _, share := range shares {
		amount := income / len(shares)
		if weighted {
			amount = income * share.Weight / totalWeight
		}
		amounts[share.TerritoryID] = amount
		given += amount
	}

	remainderTo := shares[0].TerritoryID
	if _, ok := amounts[capitalID]; ok {
		remainderTo = capitalID
	}
	amounts[remainderTo] += income - given
	return amounts
}
