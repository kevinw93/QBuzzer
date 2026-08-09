package game

import (
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

type buzz struct {
	id, player, team string
	at               time.Time // judged press time
	arrived          time.Time
	correction       float64 // ms
	rtt              float64 // ms
	clamped          bool
	status           protocol.BuzzStatus
	power            bool // ruled a power
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
	windowEnds time.Time
	buzzes     []buzz
	locked     map[string]bool // lockout keys: team IDs, or player IDs under LockoutPlayer
	bonus      *bonus

	players    []string          // join order
	playerTeam map[string]string // team mode only
	teams      []team
	scores     map[string]int // by team ID (player ID in individual play)
	protests   []protocol.ProtestView
	history    []protocol.TossupRecord // append-only; records are never modified

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
	c.history = slices.Clone(s.history)
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

// endTossup records the tossup, if it was read, and moves to Idle with the
// next tossup ready.
func (s *state) endTossup() {
	if !s.startedAt.IsZero() {
		rec := protocol.TossupRecord{TossupID: s.tossupID(), Number: s.tossupNum, Buzzes: s.buzzViews()}
		if s.bonus != nil {
			rec.Bonus = s.bonusView()
		}
		s.history = append(s.history, rec)
	}
	s.phase = protocol.PhaseIdle
	s.tossupNum++
	s.startedAt, s.windowEnds = time.Time{}, time.Time{}
	s.buzzes = nil
	s.locked = map[string]bool{}
	s.bonus = nil
}

func (s *state) view(r Ruleset) protocol.GameView {
	v := protocol.GameView{
		Phase:        s.phase,
		TossupID:     s.tossupID(),
		TossupNumber: s.tossupNum,
		LockedOut:    slices.Sorted(maps.Keys(s.locked)),
		Teams:        []protocol.TeamView{},
		Protests:     slices.Clone(s.protests),
		Buzzes:       s.buzzViews(),
		History:      slices.Clone(s.history),
		Rules: protocol.RulesView{
			Individual: r.Individual, TossupPoints: r.TossupPoints, PowerPoints: r.PowerPoints, NegPoints: r.NegPoints,
			BonusParts: r.BonusParts, BonusPartPoints: r.BonusPartPoints, Bounceback: r.Bounceback,
		},
	}
	if v.LockedOut == nil {
		v.LockedOut = []string{}
	}
	if v.Protests == nil {
		v.Protests = []protocol.ProtestView{}
	}
	if v.History == nil {
		v.History = []protocol.TossupRecord{}
	}
	if s.bonus != nil {
		v.Bonus = s.bonusView()
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

func durMs(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (s *state) buzzViews() []protocol.BuzzView {
	views := make([]protocol.BuzzView, 0, len(s.buzzes))
	for _, b := range s.buzzes {
		views = append(views, protocol.BuzzView{
			ID: b.id, PlayerID: b.player, TeamID: b.team,
			OffsetMs: msBetween(s.startedAt, b.at), CorrectionMs: b.correction, Clamped: b.clamped,
			Status: b.status, Power: b.power, Points: b.points, RTTMs: b.rtt,
		})
	}
	return views
}

func (s *state) bonusView() *protocol.BonusView {
	return &protocol.BonusView{ID: s.bonus.id, TeamID: s.bonus.team, Parts: slices.Clone(s.bonus.parts)}
}
