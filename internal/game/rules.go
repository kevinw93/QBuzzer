package game

import (
	"errors"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

type Lockout int

const (
	// LockoutTeam locks the buzzer's whole team out after a wrong answer.
	LockoutTeam Lockout = iota
	// LockoutPlayer locks out only the buzzer; teammates may still buzz.
	LockoutPlayer
)

// Ruleset configures scoring and timing. Validate before use.
type Ruleset struct {
	Individual      bool // every player is their own team
	TossupPoints    int
	PowerPoints     int // 0 disables powers
	NegPoints       int // applied to interrupts only; 0 disables negs
	Lockout         Lockout
	BonusParts      int // 0 disables bonuses
	BonusPartPoints int
	Bounceback      bool          // missed bonus parts may be awarded to another team
	BuzzWindow      time.Duration // collection window for near-simultaneous buzzes
	DeadAfter       time.Duration // tossup dies this long after reading finishes; 0 = moderator decides
}

// Standard is 15/10/-5 with three 10-point bonus parts.
func Standard() Ruleset {
	return Ruleset{
		TossupPoints: 10, PowerPoints: 15, NegPoints: -5,
		BonusParts: 3, BonusPartPoints: 10,
		BuzzWindow: 100 * time.Millisecond,
	}
}

// NoPowers is 10/-5 with three 10-point bonus parts.
func NoPowers() Ruleset {
	r := Standard()
	r.PowerPoints = 0
	return r
}

// Practice is individual play with no negs and no bonuses.
func Practice() Ruleset {
	return Ruleset{
		Individual: true, TossupPoints: 10, PowerPoints: 15,
		BuzzWindow: 100 * time.Millisecond,
	}
}

func (r Ruleset) Validate() error {
	switch {
	case r.TossupPoints <= 0:
		return errors.New("tossup points must be positive")
	case r.PowerPoints != 0 && r.PowerPoints <= r.TossupPoints:
		return errors.New("power points must exceed tossup points")
	case r.NegPoints > 0:
		return errors.New("neg points must be zero or negative")
	case r.BonusParts < 0 || r.BonusParts > protocol.MaxBonusParts:
		return errors.New("bonus parts out of range")
	case r.BonusParts > 0 && r.BonusPartPoints <= 0:
		return errors.New("bonus part points must be positive")
	case r.Bounceback && (r.Individual || r.BonusParts == 0):
		return errors.New("bouncebacks need teams and bonuses")
	case r.BuzzWindow < 0 || r.BuzzWindow > 2*time.Second:
		return errors.New("buzz window must be 0-2s")
	case r.DeadAfter < 0:
		return errors.New("dead-after must be non-negative")
	}
	return nil
}
