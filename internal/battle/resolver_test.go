package battle

import (
	"math"
	"testing"
)

// rolls returns a Roller that yields the given numbers in order.
func rolls(values ...float64) Roller {
	i := 0
	return func() float64 {
		v := values[i%len(values)]
		i++
		return v
	}
}

func TestHigherProductWins(t *testing.T) {
	resolver := ProductResolver{}
	cases := []struct {
		name            string
		attack, defense int
		rAttack, rDef   float64
		attackerWins    bool
	}{
		{"stronger and luckier attacker", 100, 50, 0.8, 0.5, true},
		{"weaker attacker can still win", 50, 100, 0.9, 0.2, true},
		{"stronger attacker can still lose", 100, 50, 0.1, 0.9, false},
		{"an exact tie goes to the defender", 100, 100, 0.5, 0.5, false},
		{"another tie: 80 x 0.5 = 100 x 0.4", 80, 100, 0.5, 0.4, false},
		{"an empty territory falls to any positive roll", 10, 0, 0.01, 0.99, true},
		{"nobody wins by rolling zero against nothing", 10, 0, 0, 0.5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolver.Resolve(tc.attack, tc.defense, rolls(tc.rAttack, tc.rDef))
			if got.AttackerWins != tc.attackerWins || got.RollAttack != tc.rAttack || got.RollDefense != tc.rDef {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestWinProbability(t *testing.T) {
	cases := []struct{ a, d, want float64 }{
		{100, 100, 0.5},     // equal resources
		{200, 100, 0.75},    // twice the resources
		{50, 100, 0.25},     // half the resources
		{300, 100, 5.0 / 6}, // three times
		{10, 0, 1},
		{0, 10, 0},
	}
	for _, tc := range cases {
		if got := WinProbability(tc.a, tc.d); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("WinProbability(%v, %v) = %v, want %v", tc.a, tc.d, got, tc.want)
		}
	}
}

func TestSecureRollIsUniformInTheUnitInterval(t *testing.T) {
	const n = 200_000
	sum := 0.0
	var buckets [10]int
	for range n {
		r := SecureRoll()
		if r < 0 || r >= 1 {
			t.Fatalf("roll %v outside [0, 1)", r)
		}
		sum += r
		buckets[int(r*10)]++
	}
	if mean := sum / n; math.Abs(mean-0.5) > 0.005 {
		t.Errorf("mean of the rolls = %v, want about 0.5", mean)
	}
	for i, count := range buckets {
		if share := float64(count) / n; math.Abs(share-0.1) > 0.005 {
			t.Errorf("bucket %d holds %.4f of the rolls, want about 0.1", i, share)
		}
	}
}

// The formula, drawn with the real generator, wins as often as the theory says.
func TestOutcomesMatchTheExpectedProbabilities(t *testing.T) {
	const n = 200_000
	resolver := ProductResolver{}
	for _, tc := range []struct{ attack, defense int }{
		{100, 100}, {200, 100}, {100, 200}, {300, 100}, {70, 100}, {1, 100}, {1000, 30},
	} {
		wins := 0
		for range n {
			if resolver.Resolve(tc.attack, tc.defense, SecureRoll).AttackerWins {
				wins++
			}
		}
		got := float64(wins) / n
		want := WinProbability(float64(tc.attack), float64(tc.defense))
		// Four standard deviations of a proportion over n trials, at most 0.0045.
		if math.Abs(got-want) > 0.0045 {
			t.Errorf("attack %d against %d: attacker won %.4f of the battles, expected %.4f", tc.attack, tc.defense, got, want)
		}
	}
}
