package game

import (
	"fmt"
	"strconv"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

// event is one accepted entry in the game log.
type event struct {
	id       int
	at       time.Time // when it happened: for buzzes and marks, the judged press time
	player   string
	msg      any // a protocol message, or one of the internal events below
	reverted bool

	// Buzzes only. Decided when the buzz is handled and logged, so replay
	// never depends on clock estimates that have since changed.
	arrived    time.Time
	correction float64 // ms; at = arrived - correction
	clamped    bool
	window     time.Duration // collection window, if this buzz opens one
	rtt        float64       // ms; the buzzer's RTT estimate, 0 if none
}

// Internal events, logged when they change state so replay is deterministic.
type (
	playerJoined struct{}
	windowClosed struct{}
)

// roster events are never reverted by undo: players, teams, and protests are
// not part of the tossup flow.
func roster(msg any) bool {
	switch msg.(type) {
	case playerJoined, protocol.CreateTeam, protocol.AssignTeam, protocol.ChooseTeam, protocol.Protest:
		return true
	}
	return false
}

func undoable(msg any) bool {
	switch msg.(type) {
	case protocol.StartTossup,
		protocol.Rule, protocol.RuleBonus, protocol.Next:
		return true
	}
	return false
}

type ruleError struct {
	code protocol.ErrCode
	msg  string
}

func (e *ruleError) Error() string { return e.msg }

func notAllowed(format string, args ...any) error {
	return &ruleError{protocol.ErrCodeNotAllowed, fmt.Sprintf(format, args...)}
}

func stale(format string, args ...any) error {
	return &ruleError{protocol.ErrCodeStale, fmt.Sprintf(format, args...)}
}

func (s *state) requirePhase(want ...protocol.Phase) error {
	for _, p := range want {
		if s.phase == p {
			return nil
		}
	}
	return notAllowed("not allowed while %s", s.phase)
}

func (s *state) requireTossup(id string) error {
	if id != s.tossupID() {
		return stale("tossup %s is not current (current is %s)", id, s.tossupID())
	}
	return nil
}

// apply is the transition function. It mutates s and returns effects; on
// error the caller discards s. When replaying, roster events skip checks that
// depend on the tossup flow, since undo may have rewound the flow around them.
func apply(r Ruleset, s *state, e event, replaying bool) ([]protocol.Effect, error) {
	switch m := e.msg.(type) {
	case playerJoined:
		if !s.hasPlayer(e.player) {
			s.players = append(s.players, e.player)
		}
		return nil, nil

	case protocol.CreateTeam:
		if r.Individual {
			return nil, notAllowed("individual play has no teams")
		}
		if err := s.requireRosterPhase(replaying); err != nil {
			return nil, err
		}
		s.nextTeam++
		name, _ := protocol.NormalizeName(m.Name)
		s.teams = append(s.teams, team{id: "T" + strconv.Itoa(s.nextTeam), name: name})
		return nil, nil

	case protocol.AssignTeam:
		return nil, s.assign(r, m.PlayerID, m.TeamID, replaying)

	case protocol.ChooseTeam:
		return nil, s.assign(r, e.player, m.TeamID, replaying)

	case protocol.StartTossup:
		if err := s.requirePhase(protocol.PhaseIdle); err != nil {
			return nil, err
		}
		if err := s.requireTossup(m.TossupID); err != nil {
			return nil, err
		}
		s.phase = protocol.PhaseReading
		s.startedAt = e.at
		return nil, nil

	case protocol.Buzz:
		return s.buzz(r, e, m)

	case windowClosed:
		return s.closeWindow(), nil

	case protocol.Rule:
		return s.rule(r, m)

	case protocol.RuleBonus:
		return s.ruleBonus(r, m)

	case protocol.Next:
		if err := s.requirePhase(protocol.PhaseReading, protocol.PhaseBonus, protocol.PhaseTossupDead); err != nil {
			return nil, err
		}
		if err := s.requireTossup(m.TossupID); err != nil {
			return nil, err
		}
		s.endTossup()
		return nil, nil

	case protocol.Protest:
		n, err := strconv.Atoi(m.TossupID[1:])
		if !replaying && (m.TossupID[0] != 't' || err != nil || n < 1 || n > s.tossupNum) {
			return nil, stale("no tossup %s to protest", m.TossupID)
		}
		s.protests = append(s.protests, protocol.ProtestView{TossupID: m.TossupID, PlayerID: e.player, Note: m.Note})
		return nil, nil
	}
	return nil, &ruleError{protocol.ErrCodeBadMessage, fmt.Sprintf("unsupported message %T", e.msg)}
}

func (s *state) requireRosterPhase(replaying bool) error {
	if replaying {
		return nil
	}
	return s.requirePhase(protocol.PhaseIdle, protocol.PhaseBonus, protocol.PhaseTossupDead)
}

func (s *state) assign(r Ruleset, player, teamID string, replaying bool) error {
	if r.Individual {
		return notAllowed("individual play has no teams")
	}
	if err := s.requireRosterPhase(replaying); err != nil {
		return err
	}
	if !s.hasPlayer(player) {
		return stale("no player %s", player)
	}
	if teamID == "" {
		delete(s.playerTeam, player)
		return nil
	}
	if !s.hasTeam(teamID) {
		return stale("no team %s", teamID)
	}
	s.playerTeam[player] = teamID
	return nil
}

func (s *state) buzz(r Ruleset, e event, m protocol.Buzz) ([]protocol.Effect, error) {
	if err := s.requirePhase(protocol.PhaseReading, protocol.PhaseBuzzWindow); err != nil {
		return nil, err
	}
	if err := s.requireTossup(m.TossupID); err != nil {
		return nil, err
	}
	if e.at.Before(s.startedAt) {
		return nil, notAllowed("buzz stamped before the tossup started")
	}
	t := s.teamOf(r, e.player)
	if t == "" {
		return nil, notAllowed("join a team to buzz")
	}
	b := buzz{player: e.player, team: t, at: e.at, arrived: e.arrived, correction: e.correction, clamped: e.clamped, rtt: e.rtt, status: protocol.BuzzStatusPending}
	if s.locked[lockKey(r, b)] {
		return nil, notAllowed("locked out")
	}
	if s.phase == protocol.PhaseBuzzWindow {
		if e.arrived.After(s.windowEnds) {
			return nil, notAllowed("buzz window closed")
		}
		for _, x := range s.buzzes {
			if x.player == e.player && x.status == protocol.BuzzStatusPending {
				return nil, nil // already in this window
			}
		}
	}
	s.nextBuzz++
	b.id = "b" + strconv.Itoa(s.nextBuzz)
	s.buzzes = append(s.buzzes, b)
	if s.phase == protocol.PhaseReading {
		s.phase = protocol.PhaseBuzzWindow
		s.windowEnds = e.arrived.Add(e.window)
	}
	if r.MaxBuzzWindow == 0 {
		return s.closeWindow(), nil
	}
	return nil, nil
}

// closeWindow gives the floor to the earliest judged press among pending
// buzzes. Equal times go to whichever was processed first.
func (s *state) closeWindow() []protocol.Effect {
	win := -1
	for i, b := range s.buzzes {
		if b.status != protocol.BuzzStatusPending {
			continue
		}
		if win < 0 || b.at.Before(s.buzzes[win].at) {
			win = i
		}
	}
	for i := range s.buzzes {
		if s.buzzes[i].status == protocol.BuzzStatusPending {
			s.buzzes[i].status = protocol.BuzzStatusLost
		}
	}
	s.buzzes[win].status = protocol.BuzzStatusAwaiting
	s.phase = protocol.PhaseAwaitingRuling
	// Display order is adjudicated order.
	sortBuzzesByTime(s.buzzes)
	w := s.awaiting()
	return []protocol.Effect{protocol.BuzzLocked{TossupID: s.tossupID(), BuzzID: w.id, PlayerID: w.player}}
}

func sortBuzzesByTime(bs []buzz) {
	// Stable insertion sort: tiny slices, and stability keeps processing
	// order for equal timestamps.
	for i := 1; i < len(bs); i++ {
		for j := i; j > 0 && bs[j].at.Before(bs[j-1].at); j-- {
			bs[j], bs[j-1] = bs[j-1], bs[j]
		}
	}
}

func (s *state) rule(r Ruleset, m protocol.Rule) ([]protocol.Effect, error) {
	if err := s.requirePhase(protocol.PhaseAwaitingRuling); err != nil {
		return nil, err
	}
	b := s.awaiting()
	if m.BuzzID != b.id {
		return nil, stale("buzz %s is not awaiting a ruling (%s is)", m.BuzzID, b.id)
	}
	switch m.Result {
	case protocol.RulingPower:
		if r.PowerPoints == 0 {
			return nil, notAllowed("these rules have no powers")
		}
		b.status, b.power, b.points = protocol.BuzzStatusCorrect, true, r.PowerPoints
	case protocol.RulingCorrect:
		b.status, b.points = protocol.BuzzStatusCorrect, r.TossupPoints
	case protocol.RulingNeg:
		if r.NegPoints == 0 {
			return nil, notAllowed("these rules have no negs")
		}
		b.status, b.points = protocol.BuzzStatusIncorrect, r.NegPoints
	case protocol.RulingNoPenalty:
		b.status = protocol.BuzzStatusNoPenalty
	}
	s.scores[b.team] += b.points
	effects := []protocol.Effect{protocol.Ruled{BuzzID: b.id, PlayerID: b.player, Result: m.Result, Points: b.points}}
	if b.points != 0 {
		effects = append(effects, protocol.ScoreChanged{TeamID: b.team, Delta: b.points})
	}

	if b.status == protocol.BuzzStatusCorrect {
		if r.BonusParts == 0 {
			s.endTossup()
		} else {
			s.phase = protocol.PhaseBonus
			s.bonus = &bonus{id: "B" + strconv.Itoa(s.tossupNum), team: b.team, parts: make([]protocol.BonusPartView, r.BonusParts)}
		}
		return effects, nil
	}
	s.locked[lockKey(r, *b)] = true
	s.phase = protocol.PhaseReading
	if s.allLockedOut(r) {
		s.phase = protocol.PhaseTossupDead
	}
	return effects, nil
}

func (s *state) ruleBonus(r Ruleset, m protocol.RuleBonus) ([]protocol.Effect, error) {
	if err := s.requirePhase(protocol.PhaseBonus); err != nil {
		return nil, err
	}
	if m.BonusID != s.bonus.id {
		return nil, stale("bonus %s is not current (%s is)", m.BonusID, s.bonus.id)
	}
	if m.Part >= len(s.bonus.parts) {
		return nil, notAllowed("bonus has %d parts", len(s.bonus.parts))
	}
	p := &s.bonus.parts[m.Part]
	if p.Ruled {
		return nil, notAllowed("part %d already ruled; undo to change it", m.Part)
	}
	to := ""
	switch {
	case m.Correct:
		to = s.bonus.team
	case m.BouncebackTeamID != "":
		if !r.Bounceback {
			return nil, notAllowed("bouncebacks are off")
		}
		if m.BouncebackTeamID == s.bonus.team || !s.hasTeam(m.BouncebackTeamID) {
			return nil, stale("no other team %s", m.BouncebackTeamID)
		}
		to = m.BouncebackTeamID
		p.BouncebackTeamID = to
	}
	p.Ruled, p.Correct = true, m.Correct
	if to == "" {
		return nil, nil
	}
	s.scores[to] += r.BonusPartPoints
	return []protocol.Effect{protocol.ScoreChanged{TeamID: to, Delta: r.BonusPartPoints}}, nil
}
