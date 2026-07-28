package game

import (
	"testing"
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

// timed sends msg as from, arriving at arrival, with a client clock offset
// ahead of the server and a press pressedBefore arrival. The server clock in
// these tests is milliseconds since t0.
func (h *harness) timed(from string, arrival, pressedBefore time.Duration, msg any) {
	h.t.Helper()
	const offset = 7000.0
	est := clock.Estimate{Offset: offset, RTT: testRTT, Bound: testRTT / 2}
	arrivalMs := durMs(arrival)
	clientMs := arrivalMs - durMs(pressedBefore) + offset
	switch m := msg.(type) {
	case protocol.Buzz:
		m.ClientTimeMs = clientMs
		msg = m
	case protocol.MarkPower:
		m.ClientTimeMs = clientMs
		msg = m
	case protocol.ReadingFinished:
		m.ClientTimeMs = clientMs
		msg = m
	}
	role := protocol.RolePlayer
	if from == "mod" {
		role = protocol.RoleModerator
	}
	res := h.g.Handle(room.Command{
		PlayerID: from, Role: role, Msg: msg, ReceivedAt: t0.Add(arrival),
		Clock: est, ClockOK: true, ServerMs: arrivalMs,
	})
	h.checkReplay()
	if res.Reject != nil {
		h.t.Fatalf("%s %T rejected: %+v", from, msg, res.Reject)
	}
}

func buzzOf(v protocol.GameView, player string) protocol.BuzzView {
	for _, b := range v.Buzzes {
		if b.PlayerID == player {
			return b
		}
	}
	return protocol.BuzzView{}
}

func TestCorrectedPressBeatsEarlierArrival(t *testing.T) {
	h := teams(t, Standard())
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.timed("p1", ms(1000), ms(10), protocol.Buzz{TossupID: "t1"}) // pressed at 990
	h.timed("p3", ms(1060), ms(90), protocol.Buzz{TossupID: "t1"}) // pressed at 970, slow uplink
	h.tick(ms(1000) + testWindow + tickGrace)
	v := h.view()
	if w := buzzOf(v, "p3"); w.Status != protocol.BuzzStatusAwaiting || w.OffsetMs != 970 || w.CorrectionMs != 90 {
		t.Errorf("p3 = %+v, want the floor at 970 ms", w)
	}
	if l := buzzOf(v, "p1"); l.Status != protocol.BuzzStatusLost {
		t.Errorf("p1 = %+v", l)
	}
}

func TestImplausibleTimestampsAreClamped(t *testing.T) {
	h := teams(t, Standard())
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.timed("p3", ms(1000), ms(500), protocol.Buzz{TossupID: "t1"}) // claims 500 ms of uplink
	b := buzzOf(h.view(), "p3")
	if !b.Clamped || b.CorrectionMs != testRTT+testRTT/2+10 || b.OffsetMs != 855 {
		t.Errorf("liar = %+v, want correction clamped to RTT + bound + margin", b)
	}
	h.timed("p1", ms(1010), ms(-50), protocol.Buzz{TossupID: "t1"}) // claims a press after arrival
	if b := buzzOf(h.view(), "p1"); !b.Clamped || b.CorrectionMs != 0 || b.OffsetMs != 1010 {
		t.Errorf("future press = %+v, want judged at arrival", b)
	}
}

func TestWindowCoversLargestEligibleCap(t *testing.T) {
	wide := func() Ruleset { // a ceiling above every cap in these tests
		r := Standard()
		r.MaxBuzzWindow = 250 * time.Millisecond
		return r
	}
	setClocks := func(h *harness, rtts map[string]float64, at time.Duration) {
		for p, rtt := range rtts {
			h.g.PlayerClock(p, clock.Estimate{RTT: rtt}, t0.Add(at))
		}
	}
	wake := func(res room.Result, firstArrival time.Duration) time.Duration {
		return res.WakeAt.Sub(t0) - firstArrival - tickGrace
	}

	t.Run("largest cap among eligible players", func(t *testing.T) {
		h := teams(t, wide())
		setClocks(h, map[string]float64{"p1": 20, "p2": 60, "p3": 120}, 0)
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		if got := wake(h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"}), ms(1000)); got != ms(130) {
			t.Errorf("window = %v, want p3's 120 + 10 ms", got)
		}
	})
	t.Run("ceiling", func(t *testing.T) {
		h := teams(t, Standard())
		setClocks(h, map[string]float64{"p3": 900}, 0)
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		if got := wake(h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"}), ms(1000)); got != Standard().MaxBuzzWindow {
			t.Errorf("window = %v, want the ceiling", got)
		}
	})
	t.Run("locked-out and stale players don't count", func(t *testing.T) {
		h := teams(t, Standard())
		setClocks(h, map[string]float64{"p1": 200, "p2": 200, "p3": 30}, 0)
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		h.tick(ms(1000) + Standard().MaxBuzzWindow + tickGrace)                                          // p1 and p2 caps exceed the ceiling
		h.ok("mod", ms(1500), protocol.Rule{BuzzID: h.awaitingBuzz(), Result: protocol.RulingIncorrect}) // T1 locked
		if got := wake(h.ok("p3", ms(2000), protocol.Buzz{TossupID: "t1"}), ms(2000)); got != ms(40) {
			t.Errorf("window = %v, want only p3's 30 + 10 ms", got)
		}

		h = teams(t, Standard())
		setClocks(h, map[string]float64{"p1": 20, "p2": 20, "p3": 200}, 0)
		setClocks(h, map[string]float64{"p1": 20, "p2": 20}, 12*time.Second) // p3 went quiet
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		if got := wake(h.ok("p1", 12*time.Second, protocol.Buzz{TossupID: "t1"}), 12*time.Second); got != ms(30) {
			t.Errorf("window = %v, stale p3 should be ignored", got)
		}
	})
	t.Run("window length is logged, not recomputed on replay", func(t *testing.T) {
		h := teams(t, wide())
		setClocks(h, map[string]float64{"p3": 170}, 0)
		h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
		h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
		setClocks(h, map[string]float64{"p3": 10}, ms(1001))
		h.ok("p3", ms(1150), protocol.Buzz{TossupID: "t1"}) // inside the 180 ms window opened earlier
		h.checkReplay()
	})
}

func TestModeratorMarksAreClockCorrected(t *testing.T) {
	h := teams(t, Standard())
	h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
	h.timed("mod", ms(1000), ms(60), protocol.MarkPower{TossupID: "t1"}) // marked at 940
	h.timed("p1", ms(1000), ms(50), protocol.Buzz{TossupID: "t1"})       // pressed at 950
	b := buzzOf(h.view(), "p1")
	if b.PowerGapMs == nil || *b.PowerGapMs != 10 || b.PowerHint != protocol.HintClose {
		t.Errorf("buzz = %+v, want a 10 ms close call", b)
	}
}

func TestHintsAndOverrides(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name       string
		rules      func() Ruleset
		mark       time.Duration // power mark arrival; 0 = never marked
		finish     time.Duration // reading_finished arrival; 0 = never
		result     protocol.Ruling
		power, neg *bool
		wantReject bool
		wantPoints int
	}{
		{"unmarked power is a power", Standard, 0, 0, protocol.RulingCorrect, nil, nil, false, 15},
		{"well before the mark", Standard, ms(1500), 0, protocol.RulingCorrect, nil, nil, false, 15},
		{"well after the mark", Standard, ms(500), 0, protocol.RulingCorrect, nil, nil, false, 10},
		{"close call needs a decision", Standard, ms(900), 0, protocol.RulingCorrect, nil, nil, true, 0},
		{"close call ruled power", Standard, ms(900), 0, protocol.RulingCorrect, &yes, nil, false, 15},
		{"close call ruled not power", Standard, ms(900), 0, protocol.RulingCorrect, &no, nil, false, 10},
		{"override beats a clear hint", Standard, ms(500), 0, protocol.RulingCorrect, &yes, nil, false, 15},
		{"no powers: no decision needed", NoPowers, ms(900), 0, protocol.RulingCorrect, nil, nil, false, 10},
		{"interrupt negs", Standard, 0, 0, protocol.RulingIncorrect, nil, nil, false, -5},
		{"close end of reading needs a decision", Standard, 0, ms(900), protocol.RulingIncorrect, nil, nil, true, 0},
		{"close end ruled neg", Standard, 0, ms(900), protocol.RulingIncorrect, nil, &yes, false, -5},
		{"close end ruled no neg", Standard, 0, ms(900), protocol.RulingIncorrect, nil, &no, false, 0},
		{"no_penalty never needs a decision", Standard, 0, ms(900), protocol.RulingNoPenalty, nil, nil, false, 0},
		{"practice has no negs to decide", Practice, 0, ms(900), protocol.RulingIncorrect, nil, nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var h *harness
			if tt.rules().Individual {
				h = newHarness(t, tt.rules(), "p1", "p2")
			} else {
				h = teams(t, tt.rules())
			}
			h.ok("mod", 0, protocol.StartTossup{TossupID: "t1"})
			// Buzz first, then marks stamped around it: marks may arrive
			// after a buzz yet be judged earlier.
			h.ok("p1", ms(1000), protocol.Buzz{TossupID: "t1"})
			h.tick(ms(1000) + testWindow + tickGrace)
			if tt.mark != 0 {
				h.g.s.powerAt = t0.Add(tt.mark) // marks are only accepted while reading; set directly
			}
			if tt.finish != 0 {
				h.g.s.finishedAt = t0.Add(tt.finish)
			}
			rule := protocol.Rule{BuzzID: h.awaitingBuzz(), Result: tt.result, Power: tt.power, Neg: tt.neg}
			res := h.g.Handle(room.Command{PlayerID: "mod", Role: protocol.RoleModerator, Msg: rule, ReceivedAt: t0.Add(ms(2000))})
			if tt.wantReject {
				if res.Reject == nil || res.Reject.Code != protocol.ErrCodeNotAllowed {
					t.Fatalf("got %+v, want a close-call rejection", res.Reject)
				}
				return
			}
			if res.Reject != nil {
				t.Fatalf("rejected: %+v", res.Reject)
			}
			if b := buzzOf(h.view(), "p1"); b.Points != tt.wantPoints {
				t.Errorf("points = %d, want %d (buzz %+v)", b.Points, tt.wantPoints, b)
			}
		})
	}
}
