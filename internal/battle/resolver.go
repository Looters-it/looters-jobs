// Package battle resolves battles between empires. It is the only place
// where an outcome is decided: the api records declarations and answers,
// and this service turns them into results.
package battle

import (
	"crypto/rand"
	"encoding/binary"
)

// Roller returns a uniform random number in [0, 1).
type Roller func() float64

// SecureRoll draws from the operating system's secure generator.
func SecureRoll() float64 {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on supported systems; if it does there is no safe fallback.
		panic("battle: secure random source unavailable: " + err.Error())
	}
	// 53 random bits, the precision of a float64 between 0 and 1.
	return float64(binary.BigEndian.Uint64(buf[:])>>11) / (1 << 53)
}

// Result is the outcome of one battle with the numbers that produced it.
type Result struct {
	RollAttack   float64
	RollDefense  float64
	AttackerWins bool
}

// Resolver decides a battle from the strengths of the two sides. It is an
// interface because the formula is expected to change (PROMPT §2.5): the
// name of the resolver that decided each battle is stored with it.
type Resolver interface {
	Name() string
	Resolve(attackForce, defenseForce int, roll Roller) Result
}

// ProductResolver is the first formula: each side multiplies its resources
// by an independent random number between 0 and 1, and the higher product
// wins. A tie goes to the defender.
//
// With attacker resources A and defender resources D the attacker wins with
// probability A/(2D) when A <= D, and 1 - D/(2A) otherwise.
type ProductResolver struct{}

func (ProductResolver) Name() string { return "uniform-product-v1" }

func (ProductResolver) Resolve(attackForce, defenseForce int, roll Roller) Result {
	result := Result{RollAttack: roll(), RollDefense: roll()}
	result.AttackerWins = float64(attackForce)*result.RollAttack > float64(defenseForce)*result.RollDefense
	return result
}

// WinProbability is the chance that the attacker wins under ProductResolver.
func WinProbability(attackForce, defenseForce float64) float64 {
	switch {
	case attackForce <= 0:
		return 0
	case defenseForce <= 0:
		return 1
	case attackForce <= defenseForce:
		return attackForce / (2 * defenseForce)
	default:
		return 1 - defenseForce/(2*attackForce)
	}
}

// Decide reads a round fought by ProductResolver against a margin: a side wins the
// battle only if its score beats the other's by that much. Below the margin nobody
// wins and the battle goes on. Share is the attacker's part of the two scores: by how
// much the round went, without any amount. At margin zero it decides like the
// resolver: every round has a winner, and a tie goes to the defender.
func Decide(attackForce, defenseForce int, result Result, marginPct float64) (decided, attackerWins bool, share float64) {
	attack := float64(attackForce) * result.RollAttack
	defense := float64(defenseForce) * result.RollDefense
	if attack+defense > 0 {
		share = attack / (attack + defense)
	}
	factor := 1 + marginPct/100
	switch {
	case attack > defense*factor:
		return true, true, share
	case defense >= attack*factor:
		return true, false, share
	default:
		return false, false, share
	}
}
