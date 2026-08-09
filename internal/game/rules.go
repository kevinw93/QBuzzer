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
	Individual      bool    `json:"individual"` // every player is their own team
	TossupPoints    int     `json:"tossupPoints"`
	PowerPoints     int     `json:"powerPoints"` // 0 disables powers
	NegPoints       int     `json:"negPoints"`   // 0 disables negs
	Lockout         Lockout `json:"lockout"`
	BonusParts      int     `json:"bonusParts"` // 0 disables bonuses
	BonusPartPoints int     `json:"bonusPartPoints"`
	Bounceback      bool    `json:"bounceback"` // missed bonus parts may be awarded to another team

	// MaxBuzzWindow caps the collection window, which otherwise covers the
	// largest possible correction among eligible players. 0 locks the first
	// buzz immediately.
	MaxBuzzWindow time.Duration `json:"maxBuzzWindow"`
	// CorrectionMargin is added to a connection's RTT to cap how much
	// earlier than its arrival a message may be judged.
	CorrectionMargin time.Duration `json:"correctionMargin"`
}

func defaultTiming(r Ruleset) Ruleset {
	r.MaxBuzzWindow = 100 * time.Millisecond
	r.CorrectionMargin = 10 * time.Millisecond
	return r
}

// Standard is 15/10/-5 with three 10-point bonus parts.
func Standard() Ruleset {
	return defaultTiming(Ruleset{
		TossupPoints: 10, PowerPoints: 15, NegPoints: -5,
		BonusParts: 3, BonusPartPoints: 10,
	})
}

// NoPowers is 10/-5 with three 10-point bonus parts.
func NoPowers() Ruleset {
	r := Standard()
	r.PowerPoints = 0
	return r
}

// Practice is individual play with no negs and no bonuses.
func Practice() Ruleset {
	return defaultTiming(Ruleset{Individual: true, TossupPoints: 10, PowerPoints: 15})
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
	case r.MaxBuzzWindow < 0 || r.MaxBuzzWindow > 2*time.Second:
		return errors.New("max buzz window must be 0-2s")
	case r.CorrectionMargin < 0 || r.CorrectionMargin > 100*time.Millisecond:
		return errors.New("correction margin must be 0-100ms")
	}
	return nil
}
