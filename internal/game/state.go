package game

import (
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/kevinw93/QBuzzer/internal/fair"
	"github.com/kevinw93/QBuzzer/internal/protocol"
)

type buzz struct {
	id, player, team string
	at               time.Time // judged press time
	arrived          time.Time
	correction       float64 // ms
	clamped          bool
	status           protocol.BuzzStatus
	ruled            bool
	power, interrupt bool // decided at ruling
	points           int
}

type team struct {
	id, name string
}

type bonus struct {
	id, team string
	parts    []protocol.BonusPartView
}

// state is plain data. clone must deep-copy every slice and map, because a
// command is applied to a clone and discarded if it fails.
type state struct {
	phase      protocol.Phase
	tossupNum  int // the current tossup, or the next to start when idle
	startedAt  time.Time
	powerAt    time.Time // zero until marked
	finishedAt time.Time // zero until reading finished
	windowEnds time.Time
	buzzes     []buzz
	locked     map[string]bool // lockout keys: team IDs, or player IDs under LockoutPlayer
	bonus      *bonus

	players    []string          // join order
	playerTeam map[string]string // team mode only
	teams      []team
	scores     map[string]int // by team ID (player ID in individual play)
	protests   []protocol.ProtestView

	nextBuzz, nextTeam int
	lastAction         int // event ID of the latest undoable action; 0 if none
}

func newState() state {
	return state{
		phase:      protocol.PhaseIdle,
		tossupNum:  1,
		locked:     map[string]bool{},
		playerTeam: map[string]string{},
		scores:     map[string]int{},
	}
}

func (s state) clone() state {
	c := s
	c.buzzes = slices.Clone(s.buzzes)
	c.locked = maps.Clone(s.locked)
	if s.bonus != nil {
		b := *s.bonus
		b.parts = slices.Clone(s.bonus.parts)
		c.bonus = &b
	}
	c.players = slices.Clone(s.players)
	c.playerTeam = maps.Clone(s.playerTeam)
	c.teams = slices.Clone(s.teams)
	c.scores = maps.Clone(s.scores)
	c.protests = slices.Clone(s.protests)
	return c
}

func tossupID(n int) string { return "t" + strconv.Itoa(n) }

func (s *state) tossupID() string { return tossupID(s.tossupNum) }

func (s *state) hasPlayer(id string) bool { return slices.Contains(s.players, id) }

func (s *state) hasTeam(id string) bool {
	return slices.ContainsFunc(s.teams, func(t team) bool { return t.id == id })
}

// teamOf returns the scoring team for a player, or "" if they have none.
func (s *state) teamOf(r Ruleset, player string) string {
	if r.Individual {
		return player
	}
	return s.playerTeam[player]
}

func lockKey(r Ruleset, b buzz) string {
	if r.Lockout == LockoutPlayer && !r.Individual {
		return b.player
	}
	return b.team
}

// eligible reports whether player could buzz now: on a team and not locked out.
func (s *state) eligible(r Ruleset, player string) bool {
	t := s.teamOf(r, player)
	if t == "" {
		return false
	}
	return !s.locked[lockKey(r, buzz{player: player, team: t})]
}

// allLockedOut reports whether nobody eligible is left to buzz.
func (s *state) allLockedOut(r Ruleset) bool {
	for _, p := range s.players {
		t := s.teamOf(r, p)
		if t == "" {
			continue
		}
		key := t
		if r.Lockout == LockoutPlayer && !r.Individual {
			key = p
		}
		if !s.locked[key] {
			return false
		}
	}
	return true
}

func (s *state) awaiting() *buzz {
	for i := range s.buzzes {
		if s.buzzes[i].status == protocol.BuzzStatusAwaiting {
			return &s.buzzes[i]
		}
	}
	return nil
}

// endTossup moves to Idle with the next tossup ready.
func (s *state) endTossup() {
	s.phase = protocol.PhaseIdle
	s.tossupNum++
	s.startedAt, s.powerAt, s.finishedAt, s.windowEnds = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	s.buzzes = nil
	s.locked = map[string]bool{}
	s.bonus = nil
}

func (s *state) view(r Ruleset) protocol.GameView {
	v := protocol.GameView{
		Phase:           s.phase,
		TossupID:        s.tossupID(),
		TossupNumber:    s.tossupNum,
		PowerMarked:     !s.powerAt.IsZero(),
		ReadingFinished: !s.finishedAt.IsZero(),
		Buzzes:          make([]protocol.BuzzView, 0, len(s.buzzes)),
		LockedOut:       slices.Sorted(maps.Keys(s.locked)),
		Teams:           []protocol.TeamView{},
		Protests:        slices.Clone(s.protests),
		Individual:      r.Individual,
	}
	if v.LockedOut == nil {
		v.LockedOut = []string{}
	}
	if v.Protests == nil {
		v.Protests = []protocol.ProtestView{}
	}
	for _, b := range s.buzzes {
		bv := protocol.BuzzView{
			ID: b.id, PlayerID: b.player, TeamID: b.team,
			OffsetMs: msBetween(s.startedAt, b.at), CorrectionMs: b.correction, Clamped: b.clamped,
			Status: b.status, PowerHint: s.powerHint(r, b), InterruptHint: s.interruptHint(r, b),
			Power: b.power, Interrupt: b.interrupt, Points: b.points,
		}
		if !b.ruled {
			bv.Power, bv.Interrupt = bv.PowerHint == protocol.HintYes, bv.InterruptHint == protocol.HintYes
		}
		if !s.powerAt.IsZero() {
			gap := msBetween(s.powerAt, b.at)
			bv.PowerGapMs = &gap
		}
		if !s.finishedAt.IsZero() {
			gap := msBetween(s.finishedAt, b.at)
			bv.InterruptGapMs = &gap
		}
		v.Buzzes = append(v.Buzzes, bv)
	}
	if s.bonus != nil {
		v.Bonus = &protocol.BonusView{ID: s.bonus.id, TeamID: s.bonus.team, Parts: slices.Clone(s.bonus.parts)}
	}
	if r.Individual {
		for _, p := range s.players {
			v.Teams = append(v.Teams, protocol.TeamView{ID: p, Score: s.scores[p], PlayerIDs: []string{p}})
		}
	} else {
		for _, t := range s.teams {
			members := []string{}
			for _, p := range s.players {
				if s.playerTeam[p] == t.id {
					members = append(members, p)
				}
			}
			v.Teams = append(v.Teams, protocol.TeamView{ID: t.id, Name: t.name, Score: s.scores[t.id], PlayerIDs: members})
		}
	}
	if s.lastAction != 0 {
		v.LastActionID = actionID(s.lastAction)
	}
	return v
}

func actionID(eventID int) string { return "a" + strconv.Itoa(eventID) }

func msBetween(from, to time.Time) float64 { return float64(to.Sub(from).Microseconds()) / 1000 }

// powerHint is empty when the rules have no powers. An unmarked power means
// reading hasn't reached it, so every buzz so far is a power.
func (s *state) powerHint(r Ruleset, b buzz) protocol.Hint {
	if r.PowerPoints == 0 {
		return ""
	}
	return markHint(r, b, s.powerAt)
}

func (s *state) interruptHint(r Ruleset, b buzz) protocol.Hint {
	return markHint(r, b, s.finishedAt)
}

func markHint(r Ruleset, b buzz, mark time.Time) protocol.Hint {
	if mark.IsZero() {
		return protocol.HintYes
	}
	return fair.Classify(msBetween(mark, b.at), 0, durMs(r.AudioDelayMax))
}

func durMs(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
