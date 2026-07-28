package game

import (
	"errors"
	"fmt"
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/fair"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

// tickGrace delays window closing past its deadline so buzzes stamped just
// before the deadline, still on their way through the room inbox, count.
const tickGrace = 5 * time.Millisecond

// Game implements room.Game. Like the room, it is used from one goroutine.
type Game struct {
	rules  Ruleset
	log    []event
	s      state
	nextID int

	// Latest correction cap per player. Not logged: it only feeds the window
	// length, which is logged on the buzz that opens the window.
	clocks map[string]playerClock
}

type playerClock struct {
	capMs float64
	at    time.Time
}

// clockFreshness excludes players whose estimate has gone quiet (likely
// disconnected) from sizing the window.
const clockFreshness = 10 * time.Second

var _ room.Game = (*Game)(nil)

// New panics on an invalid ruleset; validate user-supplied rules first.
func New(r Ruleset) *Game {
	if err := r.Validate(); err != nil {
		panic(fmt.Sprintf("game: %v", err))
	}
	return &Game{rules: r, s: newState(), clocks: map[string]playerClock{}}
}

func (g *Game) View() protocol.GameView { return g.s.view(g.rules) }

func (g *Game) PlayerJoined(playerID string) {
	if _, err := g.record(event{player: playerID, msg: playerJoined{}}); err != nil {
		panic(fmt.Sprintf("game: player joined: %v", err))
	}
}

func (g *Game) Handle(cmd room.Command) room.Result {
	// Deadlines that passed before this command take effect first, so a buzz
	// stamped after the window closed cannot join it.
	effects, changed := g.advance(cmd.ReceivedAt)

	if u, ok := cmd.Msg.(protocol.Undo); ok {
		if err := g.undo(u.ActionID); err != nil {
			return g.result(changed, effects, err)
		}
		return g.result(true, effects, nil)
	}
	e := event{at: cmd.ReceivedAt, player: cmd.PlayerID, msg: cmd.Msg}
	switch m := cmd.Msg.(type) {
	case protocol.Buzz:
		p := g.judge(cmd, m.ClientTimeMs)
		e.arrived, e.correction, e.clamped = cmd.ReceivedAt, p.CorrectionMs, p.Clamped
		e.at = msBefore(cmd.ReceivedAt, p.CorrectionMs)
		if g.s.phase == protocol.PhaseReading {
			e.window = g.window(cmd.ReceivedAt)
		}
	case protocol.MarkPower:
		e.at = msBefore(cmd.ReceivedAt, g.judge(cmd, m.ClientTimeMs).CorrectionMs)
	case protocol.ReadingFinished:
		e.at = msBefore(cmd.ReceivedAt, g.judge(cmd, m.ClientTimeMs).CorrectionMs)
	}
	eff, err := g.record(e)
	return g.result(changed || err == nil, append(effects, eff...), err)
}

func (g *Game) PlayerClock(playerID string, est clock.Estimate, at time.Time) {
	g.clocks[playerID] = playerClock{capMs: fair.Cap(est, durMs(g.rules.CorrectionMargin)), at: at}
}

func (g *Game) judge(cmd room.Command, clientMs float64) fair.Press {
	return fair.PressTime(cmd.ServerMs, clientMs, cmd.Clock, cmd.ClockOK, durMs(g.rules.CorrectionMargin))
}

// window sizes the collection window opened at now: the largest correction
// cap among players who could still buzz.
func (g *Game) window(now time.Time) time.Duration {
	var caps []float64
	for _, p := range g.s.players {
		if !g.s.eligible(g.rules, p) {
			continue
		}
		if c, ok := g.clocks[p]; ok && now.Sub(c.at) <= clockFreshness {
			caps = append(caps, c.capMs)
		}
	}
	return time.Duration(fair.Window(caps, durMs(g.rules.MaxBuzzWindow)) * float64(time.Millisecond))
}

func msBefore(t time.Time, ms float64) time.Time {
	return t.Add(-time.Duration(ms * float64(time.Millisecond)))
}

func (g *Game) Tick(now time.Time) room.Result {
	effects, changed := g.advance(now)
	return g.result(changed, effects, nil)
}

func (g *Game) result(changed bool, effects []protocol.Effect, err error) room.Result {
	res := room.Result{Changed: changed, Effects: effects, WakeAt: g.wakeAt()}
	if err != nil {
		var re *ruleError
		if !errors.As(err, &re) {
			panic(err)
		}
		res.Reject = &protocol.ErrorMessage{Code: re.code, Message: re.msg}
	}
	return res
}

// record applies e to a copy of the state and, if it succeeds, logs it.
func (g *Game) record(e event) ([]protocol.Effect, error) {
	next := g.s.clone()
	e.id = g.nextID + 1
	effects, err := step(g.rules, &next, e, false)
	if err != nil {
		return nil, err
	}
	g.nextID = e.id
	g.log = append(g.log, e)
	g.s = next
	return effects, nil
}

func step(r Ruleset, s *state, e event, replaying bool) ([]protocol.Effect, error) {
	effects, err := apply(r, s, e, replaying)
	if err == nil && undoable(e.msg) {
		s.lastAction = e.id
	}
	return effects, err
}

// advance applies deadlines due at or before now, stamped with their deadline
// rather than now so replay doesn't depend on when the timer fired.
func (g *Game) advance(now time.Time) ([]protocol.Effect, bool) {
	var effects []protocol.Effect
	changed := false
	for {
		var e event
		switch {
		case g.s.phase == protocol.PhaseBuzzWindow && !now.Before(g.s.windowEnds.Add(tickGrace)):
			e = event{at: g.s.windowEnds, msg: windowClosed{}}
		case g.deadlineAt() != (time.Time{}) && !now.Before(g.deadlineAt()):
			e = event{at: g.deadlineAt(), msg: tossupExpired{}}
		default:
			return effects, changed
		}
		eff, err := g.record(e)
		if err != nil {
			panic(fmt.Sprintf("game: deadline: %v", err))
		}
		effects, changed = append(effects, eff...), true
	}
}

func (g *Game) deadlineAt() time.Time {
	if g.s.phase != protocol.PhaseReading || g.s.finishedAt.IsZero() || g.rules.DeadAfter == 0 {
		return time.Time{}
	}
	return g.s.finishedAt.Add(g.rules.DeadAfter)
}

func (g *Game) wakeAt() time.Time {
	if g.s.phase == protocol.PhaseBuzzWindow {
		return g.s.windowEnds.Add(tickGrace)
	}
	return g.deadlineAt()
}

// undo reverts the latest undoable action and every later non-roster event,
// then rebuilds the state from the remaining log.
func (g *Game) undo(id string) error {
	if g.s.lastAction == 0 || id != actionID(g.s.lastAction) {
		return stale("action %s is not the latest undoable action", id)
	}
	for i := range g.log {
		if g.log[i].id >= g.s.lastAction && !roster(g.log[i].msg) {
			g.log[i].reverted = true
		}
	}
	g.s = replay(g.rules, g.log)
	return nil
}

// replay folds the non-reverted log. Every logged event succeeded when first
// applied and apply is deterministic, so failure is a bug.
func replay(r Ruleset, log []event) state {
	s := newState()
	for _, e := range log {
		if e.reverted {
			continue
		}
		if _, err := step(r, &s, e, true); err != nil {
			panic(fmt.Sprintf("game: replaying event %d (%T): %v", e.id, e.msg, err))
		}
	}
	return s
}
