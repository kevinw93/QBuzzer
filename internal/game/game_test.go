package game

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

var t0 = time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// harness drives a Game with explicit times and checks, after every call,
// that replaying the log reproduces the live state exactly.
type harness struct {
	t       *testing.T
	g       *Game
	entries []room.GameEvent // everything drained so far, as storage would hold it
}

// testRTT gives every test player a 100 ms correction cap (RTT + 10 ms
// margin), so the collection window is testWindow.
const (
	testRTT    = 90
	testWindow = 100 * time.Millisecond
)

func newHarness(t *testing.T, r Ruleset, players ...string) *harness {
	t.Helper()
	h := &harness{t: t, g: New(r)}
	for _, p := range players {
		h.g.PlayerJoined(p)
		h.g.PlayerClock(p, clock.Estimate{RTT: testRTT}, t0)
	}
	h.checkReplay()
	return h
}

// teams creates T1 = {p1, p2} and T2 = {p3} with the standard rules.
func teams(t *testing.T, r Ruleset) *harness {
	t.Helper()
	h := newHarness(t, r, "p1", "p2", "p3")
	h.ok("mod", 0, protocol.CreateTeam{Name: "Alpha"})
	h.ok("mod", 0, protocol.CreateTeam{Name: "Beta"})
	h.ok("mod", 0, protocol.AssignTeam{PlayerID: "p1", TeamID: "T1"})
	h.ok("mod", 0, protocol.AssignTeam{PlayerID: "p2", TeamID: "T1"})
	h.ok("p3", 0, protocol.ChooseTeam{TeamID: "T2"})
	return h
}

func (h *harness) do(from string, at time.Duration, msg any) room.Result {
	h.t.Helper()
	role := protocol.RolePlayer
	if from == "mod" {
		role = protocol.RoleModerator
	}
	res := h.g.Handle(room.Command{PlayerID: from, Role: role, Msg: msg, ReceivedAt: t0.Add(at)})
	h.checkReplay()
	return res
}

func (h *harness) ok(from string, at time.Duration, msg any) room.Result {
	h.t.Helper()
	res := h.do(from, at, msg)
	if res.Reject != nil {
		h.t.Fatalf("%s %T%+v rejected: %+v", from, msg, msg, res.Reject)
	}
	if !res.Changed {
		h.t.Fatalf("%s %T%+v: accepted but not reported as changed", from, msg, msg)
	}
	return res
}

func (h *harness) reject(code protocol.ErrCode, from string, at time.Duration, msg any) {
	h.t.Helper()
	before := h.view()
	res := h.do(from, at, msg)
	if res.Reject == nil || res.Reject.Code != code {
		h.t.Fatalf("%s %T%+v: got %+v, want reject %q", from, msg, msg, res.Reject, code)
	}
	if !res.Changed && !reflect.DeepEqual(before, h.view()) {
		h.t.Fatalf("rejected command changed state")
	}
}

func (h *harness) tick(at time.Duration) room.Result {
	h.t.Helper()
	res := h.g.Tick(t0.Add(at))
	h.checkReplay()
	return res
}

func (h *harness) view() protocol.GameView { return h.g.View() }

func (h *harness) phase(want protocol.Phase) {
	h.t.Helper()
	if got := h.view().Phase; got != want {
		h.t.Fatalf("phase = %s, want %s", got, want)
	}
}

func (h *harness) scores() string {
	var parts []string
	for _, t := range h.view().Teams {
		parts = append(parts, fmt.Sprintf("%s=%d", t.ID, t.Score))
	}
	return strings.Join(parts, " ")
}

func (h *harness) wantScores(want string) {
	h.t.Helper()
	if got := h.scores(); got != want {
		h.t.Fatalf("scores = %q, want %q", got, want)
	}
}

func (h *harness) checkReplay() {
	h.t.Helper()
	live := h.g.View()
	replayed := replay(h.g.rules, h.g.log)
	if got := replayed.view(h.g.rules); !reflect.DeepEqual(got, live) {
		h.t.Fatalf("replay diverged:\nlive     %+v\nreplayed %+v", live, got)
	}
	h.entries = append(h.entries, h.g.Drain()...)
	restored, err := Restore(h.g.RulesJSON(), h.entries)
	if err != nil {
		h.t.Fatalf("restore: %v", err)
	}
	if got := restored.View(); !reflect.DeepEqual(got, live) {
		h.t.Fatalf("restore diverged:\nlive     %+v\nrestored %+v", live, got)
	}
}

func (h *harness) tossup() string     { return h.view().TossupID }
func (h *harness) lastAction() string { return h.view().LastActionID }

func (h *harness) awaitingBuzz() string {
	for _, b := range h.view().Buzzes {
		if b.Status == protocol.BuzzStatusAwaiting {
			return b.ID
		}
	}
	return "b0"
}

func (h *harness) bonusID() string {
	if b := h.view().Bonus; b != nil {
		return b.ID
	}
	return "B0"
}

// buzzAndLock buzzes as player at time at and closes the window.
func (h *harness) buzzAndLock(player string, at time.Duration) {
	h.t.Helper()
	h.ok(player, at, protocol.Buzz{TossupID: h.tossup()})
	h.tick(at + testWindow + tickGrace)
	h.phase(protocol.PhaseAwaitingRuling)
}

// reach drives a fresh standard team game into phase.
func reach(t *testing.T, phase protocol.Phase) *harness {
	t.Helper()
	h := teams(t, Standard())
	if phase == protocol.PhaseIdle {
		return h
	}
	h.ok("mod", ms(0), protocol.StartTossup{TossupID: "t1"})
	switch phase {
	case protocol.PhaseReading:
	case protocol.PhaseBuzzWindow:
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
	case protocol.PhaseAwaitingRuling:
		h.buzzAndLock("p1", ms(1000))
	case protocol.PhaseBonus:
		h.buzzAndLock("p1", ms(1000))
		h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingCorrect})
	case protocol.PhaseTossupDead:
		h.buzzAndLock("p1", ms(1000))
		h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
		h.buzzAndLock("p3", ms(3000))
		h.ok("mod", ms(4000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
	}
	h.phase(phase)
	return h
}

// Every client message in every phase: either the documented transition or
// not_allowed.
func TestTransitionMatrix(t *testing.T) {
	phases := []protocol.Phase{
		protocol.PhaseIdle, protocol.PhaseReading, protocol.PhaseBuzzWindow,
		protocol.PhaseAwaitingRuling, protocol.PhaseBonus, protocol.PhaseTossupDead,
	}
	type msgFn func(h *harness) (from string, msg any)
	msgs := []struct {
		name string
		make msgFn
	}{
		{"start_tossup", func(h *harness) (string, any) { return "mod", protocol.StartTossup{TossupID: h.tossup()} }},
		{"buzz", func(h *harness) (string, any) { return "p3", protocol.Buzz{TossupID: h.tossup()} }},
		{"rule", func(h *harness) (string, any) {
			return "mod", protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingCorrect}
		}},
		{"rule_bonus", func(h *harness) (string, any) {
			return "mod", protocol.RuleBonus{BonusID: h.bonusID(), Part: 0, Correct: true}
		}},
		{"next", func(h *harness) (string, any) { return "mod", protocol.Next{TossupID: h.tossup()} }},
		{"create_team", func(*harness) (string, any) { return "mod", protocol.CreateTeam{Name: "Gamma"} }},
	}
	// The phase each accepted message leads to; absent means not_allowed.
	// In TossupDead every team is locked out, so even a buzz from p3 is
	// rejected; in BuzzWindow p3 joins the window.
	accept := map[protocol.Phase]map[string]protocol.Phase{
		protocol.PhaseIdle: {
			"start_tossup": protocol.PhaseReading,
			"create_team":  protocol.PhaseIdle,
		},
		protocol.PhaseReading: {
			"buzz": protocol.PhaseBuzzWindow,
			"next": protocol.PhaseIdle,
		},
		protocol.PhaseBuzzWindow: {
			"buzz": protocol.PhaseBuzzWindow,
		},
		protocol.PhaseAwaitingRuling: {
			"rule": protocol.PhaseBonus,
		},
		protocol.PhaseBonus: {
			"rule_bonus":  protocol.PhaseBonus,
			"next":        protocol.PhaseIdle,
			"create_team": protocol.PhaseBonus,
		},
		protocol.PhaseTossupDead: {
			"next":        protocol.PhaseIdle,
			"create_team": protocol.PhaseTossupDead,
		},
	}
	for _, p := range phases {
		for _, m := range msgs {
			t.Run(string(p)+"/"+m.name, func(t *testing.T) {
				h := reach(t, p)
				from, msg := m.make(h)
				to, ok := accept[p][m.name]
				if !ok {
					h.reject(protocol.ErrCodeNotAllowed, from, ms(1050), msg)
					h.phase(p)
					return
				}
				h.ok(from, ms(1050), msg)
				h.phase(to)
			})
		}
	}
}

func TestStaleIDsAreRejected(t *testing.T) {
	tests := []struct {
		phase protocol.Phase
		msg   any
	}{
		{protocol.PhaseIdle, protocol.StartTossup{TossupID: "t2"}},
		{protocol.PhaseReading, protocol.Buzz{TossupID: "t0"}},
		{protocol.PhaseReading, protocol.Next{TossupID: "t2"}},
		{protocol.PhaseAwaitingRuling, protocol.Rule{BuzzID: "b9", Result: protocol.RulingCorrect}},
		{protocol.PhaseBonus, protocol.RuleBonus{BonusID: "B9", Part: 0, Correct: true}},
		{protocol.PhaseBonus, protocol.Undo{ActionID: "a1"}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%T", tt.phase, tt.msg), func(t *testing.T) {
			h := reach(t, tt.phase)
			h.reject(protocol.ErrCodeStale, "mod", ms(1050), tt.msg)
		})
	}
}

func TestLockoutAndDeadTossup(t *testing.T) {
	h := teams(t, Standard())
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.buzzAndLock("p1", ms(1000))
	h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
	h.phase(protocol.PhaseReading)

	h.reject(protocol.ErrCodeNotAllowed, "p1", ms(2100), protocol.Buzz{TossupID: "t1"})
	h.reject(protocol.ErrCodeNotAllowed, "p2", ms(2100), protocol.Buzz{TossupID: "t1"}) // teammate
	if got := h.view().LockedOut; !reflect.DeepEqual(got, []string{"T1"}) {
		t.Errorf("locked out = %v", got)
	}
	h.buzzAndLock("p3", ms(3000))
	h.ok("mod", ms(4000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
	h.phase(protocol.PhaseTossupDead)
	h.wantScores("T1=-5 T2=-5")

	h.ok("mod", ms(5000), protocol.Next{TossupID: "t1"})
	h.phase(protocol.PhaseIdle)
	if v := h.view(); v.TossupID != "t2" || len(v.Buzzes) != 0 || len(v.LockedOut) != 0 {
		t.Errorf("next tossup not reset: %+v", v)
	}
}

func TestPlayerLockoutLetsTeammatesBuzz(t *testing.T) {
	r := Standard()
	r.Lockout = LockoutPlayer
	h := teams(t, r)
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.buzzAndLock("p1", ms(1000))
	h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
	h.reject(protocol.ErrCodeNotAllowed, "p1", ms(2100), protocol.Buzz{TossupID: "t1"})
	h.buzzAndLock("p2", ms(3000))
}

func TestNearSimultaneousBuzzes(t *testing.T) {
	t.Run("earliest stamp wins regardless of processing order", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1040), protocol.Buzz{TossupID: "t1"}) // processed first
		h.ok("p3", ms(1010), protocol.Buzz{TossupID: "t1"}) // stamped earlier
		res := h.tick(ms(1040) + testWindow + tickGrace)
		bs := h.view().Buzzes
		if bs[0].PlayerID != "p3" || bs[0].Status != protocol.BuzzStatusAwaiting || bs[1].Status != protocol.BuzzStatusLost {
			t.Errorf("buzzes = %+v", bs)
		}
		if len(res.Effects) != 1 || res.Effects[0].(protocol.BuzzLocked).PlayerID != "p3" {
			t.Errorf("effects = %+v", res.Effects)
		}
	})
	t.Run("equal stamps go to the first processed", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p3", ms(1000), protocol.Buzz{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		h.tick(ms(1200))
		if bs := h.view().Buzzes; bs[0].PlayerID != "p3" || bs[0].Status != protocol.BuzzStatusAwaiting {
			t.Errorf("buzzes = %+v", bs)
		}
	})
	t.Run("window is measured from the first buzz", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		h.ok("p3", ms(1100), protocol.Buzz{TossupID: "t1"}) // exactly at the window end: counts
		h.reject(protocol.ErrCodeNotAllowed, "p2", ms(1101), protocol.Buzz{TossupID: "t1"})
	})
	t.Run("a command after the deadline closes the window first", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		// No tick arrived, but this buzz is stamped past window end + grace.
		res := h.do("p3", ms(1200), protocol.Buzz{TossupID: "t1"})
		if res.Reject == nil || !res.Changed || len(res.Effects) != 1 {
			t.Fatalf("result = %+v; want reject, plus the window closing", res)
		}
		h.phase(protocol.PhaseAwaitingRuling)
	})
	t.Run("duplicate buzz in the window is ignored", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		if res := h.do("p1", ms(1050), protocol.Buzz{TossupID: "t1"}); res.Reject != nil {
			t.Fatalf("duplicate rejected: %+v", res.Reject)
		}
		if n := len(h.view().Buzzes); n != 1 {
			t.Errorf("buzzes = %d, want 1", n)
		}
	})
	t.Run("lost buzzers must buzz again after a neg", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		h.ok("p3", ms(1020), protocol.Buzz{TossupID: "t1"})
		h.tick(ms(1200))
		h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
		h.phase(protocol.PhaseReading) // p3's lost buzz does not take the floor
		h.buzzAndLock("p3", ms(3000))
	})
	t.Run("buzz stamped before the tossup started", func(t *testing.T) {
		h := teams(t, Standard())
		h.ok("mod", ms(1000), protocol.StartTossup{TossupID: "t1"})
		h.reject(protocol.ErrCodeNotAllowed, "p1", ms(999), protocol.Buzz{TossupID: "t1"})
	})
	t.Run("zero window locks immediately", func(t *testing.T) {
		r := Standard()
		r.MaxBuzzWindow = 0
		h := teams(t, r)
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		res := h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		h.phase(protocol.PhaseAwaitingRuling)
		if !res.WakeAt.IsZero() || len(res.Effects) != 1 {
			t.Errorf("result = %+v", res)
		}
	})
}

func TestWakeAt(t *testing.T) {
	h := teams(t, Standard())
	if res := h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"}); !res.WakeAt.IsZero() {
		t.Errorf("reading: WakeAt = %v", res.WakeAt)
	}
	if res := h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"}); !res.WakeAt.Equal(t0.Add(ms(1000) + testWindow + tickGrace)) {
		t.Errorf("window: WakeAt = %v", res.WakeAt)
	}
	if res := h.tick(ms(1000) + testWindow); res.Changed || h.view().Phase != protocol.PhaseBuzzWindow {
		t.Errorf("window closed before its grace period: %+v", res)
	}
	if res := h.tick(ms(1000) + testWindow + tickGrace); !res.Changed || !res.WakeAt.IsZero() {
		t.Errorf("window close: %+v", res)
	}
	if res := h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg}); !res.WakeAt.IsZero() {
		t.Errorf("after ruling: WakeAt = %v", res.WakeAt)
	}
}

func TestBonus(t *testing.T) {
	r := Standard()
	r.Bounceback = true
	h := teams(t, r)
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.buzzAndLock("p1", ms(1000))
	h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingPower})
	b := h.view().Bonus
	if b == nil || b.TeamID != "T1" || len(b.Parts) != 3 {
		t.Fatalf("bonus = %+v", b)
	}
	h.ok("mod", ms(3000), protocol.RuleBonus{BonusID: b.ID, Part: 0, Correct: true})
	h.ok("mod", ms(3000), protocol.RuleBonus{BonusID: b.ID, Part: 1, BouncebackTeamID: "T2"})
	h.reject(protocol.ErrCodeNotAllowed, "mod", ms(3000), protocol.RuleBonus{BonusID: b.ID, Part: 1, Correct: true})
	h.reject(protocol.ErrCodeNotAllowed, "mod", ms(3000), protocol.RuleBonus{BonusID: b.ID, Part: 3, Correct: true})
	h.reject(protocol.ErrCodeStale, "mod", ms(3000), protocol.RuleBonus{BonusID: b.ID, Part: 2, BouncebackTeamID: "T1"})
	h.wantScores("T1=25 T2=10")
	h.ok("mod", ms(4000), protocol.Next{TossupID: "t1"}) // part 2 never ruled: zero
	h.wantScores("T1=25 T2=10")
	h.phase(protocol.PhaseIdle)
}

func TestBouncebackOffByDefault(t *testing.T) {
	h := reach(t, protocol.PhaseBonus)
	h.reject(protocol.ErrCodeNotAllowed, "mod", ms(3000), protocol.RuleBonus{BonusID: h.bonusID(), Part: 0, BouncebackTeamID: "T2"})
}

func TestNoBonusesEndsTossupOnCorrect(t *testing.T) {
	r := Standard()
	r.BonusParts = 0
	h := teams(t, r)
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.buzzAndLock("p1", ms(1000))
	h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingCorrect})
	h.phase(protocol.PhaseIdle)
	if h.tossup() != "t2" {
		t.Errorf("tossup = %s", h.tossup())
	}
}

func TestUndo(t *testing.T) {
	t.Run("undo a correct ruling restores the buzz and score", func(t *testing.T) {
		h := reach(t, protocol.PhaseBonus)
		h.ok("mod", ms(3000), protocol.Undo{ActionID: h.lastAction()})
		h.phase(protocol.PhaseAwaitingRuling)
		h.wantScores("T1=0 T2=0")
		h.ok("mod", ms(3100), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
		h.wantScores("T1=-5 T2=0")
	})
	t.Run("undo a neg also removes buzzes made after it", func(t *testing.T) {
		h := reach(t, protocol.PhaseAwaitingRuling)
		h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNeg})
		neg := h.lastAction()
		h.buzzAndLock("p3", ms(3000)) // other team buzzes after the neg
		h.ok("mod", ms(4000), protocol.Undo{ActionID: neg})
		h.phase(protocol.PhaseAwaitingRuling)
		if bs := h.view().Buzzes; len(bs) != 1 || bs[0].PlayerID != "p1" || bs[0].Status != protocol.BuzzStatusAwaiting {
			t.Errorf("buzzes = %+v", bs)
		}
		h.wantScores("T1=0 T2=0")
	})
	t.Run("repeated undo walks back one action at a time", func(t *testing.T) {
		h := reach(t, protocol.PhaseBonus)
		want := []protocol.Phase{protocol.PhaseAwaitingRuling, protocol.PhaseIdle}
		for _, p := range want {
			h.ok("mod", ms(3000), protocol.Undo{ActionID: h.lastAction()})
			h.phase(p)
		}
		if h.lastAction() != "" {
			t.Errorf("lastAction = %q after undoing everything", h.lastAction())
		}
		h.reject(protocol.ErrCodeStale, "mod", ms(3000), protocol.Undo{ActionID: "a1"})
	})
	t.Run("double-tapped undo is stale the second time", func(t *testing.T) {
		h := reach(t, protocol.PhaseBonus)
		id := h.lastAction()
		h.ok("mod", ms(3000), protocol.Undo{ActionID: id})
		h.reject(protocol.ErrCodeStale, "mod", ms(3000), protocol.Undo{ActionID: id})
	})
	t.Run("undo keeps roster changes made after the action", func(t *testing.T) {
		h := reach(t, protocol.PhaseBonus)
		h.g.PlayerJoined("p4")
		h.ok("mod", ms(3000), protocol.CreateTeam{Name: "Gamma"})
		h.ok("mod", ms(3000), protocol.AssignTeam{PlayerID: "p4", TeamID: "T3"})
		h.ok("mod", ms(3000), protocol.Protest{TossupID: "t1", Note: "check answer"})
		h.ok("mod", ms(3100), protocol.Undo{ActionID: h.lastAction()}) // back to AwaitingRuling
		v := h.view()
		if len(v.Teams) != 3 || !reflect.DeepEqual(v.Teams[2].PlayerIDs, []string{"p4"}) || len(v.Protests) != 1 {
			t.Errorf("roster lost on undo: %+v", v)
		}
	})
}

func TestTeams(t *testing.T) {
	h := newHarness(t, Standard(), "p1", "p2")
	h.ok("mod", 0, protocol.CreateTeam{Name: "  Alpha "})
	h.reject(protocol.ErrCodeStale, "mod", 0, protocol.AssignTeam{PlayerID: "p9", TeamID: "T1"})
	h.reject(protocol.ErrCodeStale, "mod", 0, protocol.AssignTeam{PlayerID: "p1", TeamID: "T9"})
	h.reject(protocol.ErrCodeStale, "p2", 0, protocol.ChooseTeam{TeamID: "T9"})
	h.ok("p1", 0, protocol.ChooseTeam{TeamID: "T1"})
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.reject(protocol.ErrCodeNotAllowed, "p2", ms(100), protocol.Buzz{TossupID: "t1"}) // no team
	h.reject(protocol.ErrCodeNotAllowed, "p2", ms(100), protocol.ChooseTeam{TeamID: "T1"})
	h.reject(protocol.ErrCodeNotAllowed, "mod", ms(100), protocol.CreateTeam{Name: "Beta"})
	h.ok("mod", ms(200), protocol.Next{TossupID: "t1"})
	h.ok("mod", ms(300), protocol.AssignTeam{PlayerID: "p1"}) // unassign
	if teams := h.view().Teams; teams[0].Name != "Alpha" || len(teams[0].PlayerIDs) != 0 {
		t.Errorf("teams = %+v", teams)
	}
}

func TestIndividualPlay(t *testing.T) {
	h := newHarness(t, Practice(), "p1", "p2")
	h.reject(protocol.ErrCodeNotAllowed, "mod", 0, protocol.CreateTeam{Name: "Alpha"})
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.buzzAndLock("p1", ms(1000))
	h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingNoPenalty})
	h.wantScores("p1=0 p2=0") // practice: no negs
	h.buzzAndLock("p2", ms(3000))
	h.ok("mod", ms(4000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingPower})
	h.wantScores("p1=0 p2=15") // no bonuses: straight to the next tossup
	h.phase(protocol.PhaseIdle)
}

func TestProtests(t *testing.T) {
	h := reach(t, protocol.PhaseReading)
	h.ok("p3", ms(100), protocol.Protest{TossupID: "t1", Note: "pronunciation"})
	h.reject(protocol.ErrCodeStale, "p3", ms(100), protocol.Protest{TossupID: "t2", Note: "future"})
	if p := h.view().Protests; len(p) != 1 || p[0].PlayerID != "p3" {
		t.Errorf("protests = %+v", p)
	}
}

func TestRulesetValidation(t *testing.T) {
	for _, r := range []Ruleset{Standard(), NoPowers(), Practice()} {
		if err := r.Validate(); err != nil {
			t.Errorf("preset invalid: %v", err)
		}
	}
	bad := []func(*Ruleset){
		func(r *Ruleset) { r.TossupPoints = 0 },
		func(r *Ruleset) { r.PowerPoints = 10 },
		func(r *Ruleset) { r.NegPoints = 5 },
		func(r *Ruleset) { r.BonusParts = protocol.MaxBonusParts + 1 },
		func(r *Ruleset) { r.BonusPartPoints = 0 },
		func(r *Ruleset) { r.Individual, r.Bounceback = true, true },
		func(r *Ruleset) { r.MaxBuzzWindow = 3 * time.Second },
		func(r *Ruleset) { r.CorrectionMargin = time.Second },
	}
	for i, mutate := range bad {
		r := Standard()
		mutate(&r)
		if r.Validate() == nil {
			t.Errorf("case %d: invalid ruleset accepted: %+v", i, r)
		}
	}
}

func TestCloneIsDeep(t *testing.T) {
	h := reach(t, protocol.PhaseBonus)
	c := h.g.s.clone()
	c.buzzes[0].points = 999
	c.locked["X"] = true
	c.bonus.parts[0].Ruled = true
	c.playerTeam["p1"] = "T2"
	c.scores["T1"] = 999
	c.players[0] = "zz"
	if v := h.view(); v.Buzzes[0].Points == 999 || v.Bonus.Parts[0].Ruled || v.Teams[0].Score == 999 ||
		len(v.LockedOut) != 0 || v.Teams[0].PlayerIDs[0] != "p1" {
		t.Errorf("clone shares memory with the original: %+v", v)
	}
}

func TestHistoryRecordsFinishedTossups(t *testing.T) {
	h := teams(t, Standard())
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.buzzAndLock("p1", ms(1000))
	h.ok("mod", ms(2000), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingPower})
	h.ok("mod", ms(2500), protocol.RuleBonus{BonusID: h.bonusID(), Part: 0, Correct: true})
	h.ok("mod", ms(3000), protocol.Next{TossupID: "t1"})
	h.ok("mod", ms(3000), protocol.StartTossup{TossupID: "t2"})
	h.ok("mod", ms(4000), protocol.Next{TossupID: "t2"}) // dead: nobody buzzed

	hist := h.view().History
	if len(hist) != 2 {
		t.Fatalf("history = %+v", hist)
	}
	if r := hist[0]; r.TossupID != "t1" || len(r.Buzzes) != 1 || r.Buzzes[0].Points != 15 || r.Bonus == nil || !r.Bonus.Parts[0].Correct {
		t.Errorf("t1 record = %+v", r)
	}
	if r := hist[1]; r.TossupID != "t2" || len(r.Buzzes) != 0 || r.Bonus != nil {
		t.Errorf("t2 record = %+v", r)
	}

	h.ok("mod", ms(5000), protocol.Undo{ActionID: h.lastAction()}) // un-next t2
	if n := len(h.view().History); n != 1 {
		t.Errorf("history after undoing next = %d records, want 1", n)
	}
	if r := h.view().Rules; r.PowerPoints != 15 || r.BonusParts != 3 || r.Individual {
		t.Errorf("rules view = %+v", r)
	}
}
