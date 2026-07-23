package clock

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
	"time"
)

func TestSampleMath(t *testing.T) {
	// Client clock = server + 1000 ms. Each case lists true one-way delays and
	// client processing time.
	tests := []struct {
		name                 string
		up, down, processing float64
		wantRTT, wantOffset  float64
	}{
		{"symmetric", 20, 20, 0, 40, 1000},
		{"processing time is excluded", 20, 20, 7, 40, 1000},
		{"slow uplink overestimates offset", 30, 10, 0, 40, 1010},
		{"slow downlink underestimates offset", 10, 30, 0, 40, 990},
		{"all delay one way: error is exactly RTT/2", 40, 0, 0, 40, 1020},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t1 := 5000.0
			t2 := t1 + tt.up + 1000
			t3 := t2 + tt.processing
			t4 := t3 - 1000 + tt.down
			s := Sample{t1, t2, t3, t4}
			if s.RTT() != tt.wantRTT || s.Offset() != tt.wantOffset {
				t.Errorf("RTT %v offset %v, want %v %v", s.RTT(), s.Offset(), tt.wantRTT, tt.wantOffset)
			}
			if err := math.Abs(s.Offset() - 1000); err > s.RTT()/2 {
				t.Errorf("error %v exceeds RTT/2 = %v", err, s.RTT()/2)
			}
		})
	}
}

// For honest clients the error is at most RTT/2, whatever the asymmetry.
func TestOffsetErrorNeverExceedsHalfRTT(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 100_000 {
		offset := rng.Float64()*2e6 - 1e6
		up, down, proc := rng.ExpFloat64()*50, rng.ExpFloat64()*50, rng.Float64()*20
		t1 := rng.Float64() * 1e6
		t2 := t1 + up + offset
		t3 := t2 + proc
		t4 := t3 - offset + down
		s := Sample{t1, t2, t3, t4}
		if e := math.Abs(s.Offset() - offset); e > s.RTT()/2+1e-6 {
			t.Fatalf("up %v down %v: error %v > RTT/2 %v", up, down, e, s.RTT()/2)
		}
	}
}

func TestEstimatorPicksTightestBound(t *testing.T) {
	cfg := DefaultConfig()
	e := NewEstimator(cfg)
	mustAdd := func(s Sample) {
		t.Helper()
		if _, err := e.Add(s); err != nil {
			t.Fatal(err)
		}
	}
	mustAdd(Sample{T1: 0, T2: 5, T3: 5, T4: 10})            // RTT 10, offset 0
	mustAdd(Sample{T1: 1000, T2: 1040, T3: 1040, T4: 1080}) // RTT 80, offset 0
	est, _ := e.Estimate(2000)
	if est.RTT != 10 || est.Samples != 2 {
		t.Errorf("estimate = %+v, want the 10 ms sample", est)
	}
	// Twenty-nine seconds later the old sample has widened by drift; a fresh
	// 12 ms sample is tighter.
	mustAdd(Sample{T1: 29_000, T2: 29_006, T3: 29_006, T4: 29_012})
	est, _ = e.Estimate(29_012)
	if est.RTT != 12 {
		t.Errorf("estimate = %+v, want the fresh 12 ms sample", est)
	}
	// Past the window, everything but the fresh sample is gone.
	if est, _ := e.Estimate(32_000); est.Samples != 1 {
		t.Errorf("window not pruned: %+v", est)
	}
	if _, ok := e.Estimate(100_000); ok {
		t.Error("estimate from an empty window")
	}
}

func TestEstimatorRejectsBadSamples(t *testing.T) {
	e := NewEstimator(DefaultConfig())
	if _, err := e.Add(Sample{T1: 0, T2: 50, T3: 200, T4: 100}); !errors.Is(err, ErrNegativeRTT) {
		t.Errorf("err = %v, want ErrNegativeRTT", err)
	}
	if _, err := e.Add(Sample{T1: 0, T2: 3000, T3: 3000, T4: 6000}); !errors.Is(err, ErrSlowSample) {
		t.Errorf("err = %v, want ErrSlowSample", err)
	}
}

// delay is a one-way network delay: base + exponential jitter, with
// occasional queueing spikes. Delays are never below base.
type delay struct {
	base, jitter, spikeProb, spike float64
}

func (d delay) sample(rng *rand.Rand) float64 {
	v := d.base + rng.ExpFloat64()*d.jitter
	if rng.Float64() < d.spikeProb {
		v += rng.Float64() * d.spike
	}
	return v
}

type network struct {
	name     string
	up, down delay
	skewPPM  float64 // client clock runs fast by this much
	// Converged bound ceiling, ms. The floor is half the lowest RTT seen in a
	// 30 s window (about base RTT plus the minimum of ~15 jitter draws), plus
	// headroom; asymmetry raises the error, never the bound.
	wantBound  float64
	processing float64 // max client time between receive and reply, ms
}

// sim runs a Session against a simulated client for duration ms of server
// time and checks, at every pong, that the true offset is within the
// estimate's bound.
type sim struct {
	t       *testing.T
	rng     *rand.Rand
	net     network
	offset  float64 // client minus server at server time 0
	session *Session
	now     float64
}

func (s *sim) client(serverMs float64) float64 {
	return serverMs + s.offset + s.net.skewPPM*1e-6*serverMs
}

func (s *sim) trueOffset(serverMs float64) float64 { return s.client(serverMs) - serverMs }

func (s *sim) run(duration float64) Estimate {
	s.t.Helper()
	var est Estimate
	for end := s.now + duration; s.now < end; s.now += s.session.NextInterval() {
		id, ok := s.session.Ping(s.now)
		if !ok {
			s.t.Fatal("too many outstanding pings")
		}
		atClient := s.now + s.net.up.sample(s.rng)
		replyAt := atClient + s.rng.Float64()*s.net.processing
		t4 := replyAt + s.net.down.sample(s.rng)
		if err := s.session.Pong(id, s.client(atClient), s.client(replyAt), t4); err != nil {
			s.t.Fatalf("pong: %v", err)
		}
		var ok2 bool
		if est, ok2 = s.session.Estimate(t4); !ok2 {
			s.t.Fatal("no estimate after a pong")
		}
		if err := math.Abs(est.Offset - s.trueOffset(t4)); err > est.Bound+1e-9 {
			s.t.Fatalf("t=%.0f: error %.3f ms exceeds bound %.3f ms", t4, err, est.Bound)
		}
	}
	return est
}

func TestConvergenceOnSimulatedNetworks(t *testing.T) {
	nets := []network{
		{name: "lan", up: delay{0.3, 0.2, 0, 0}, down: delay{0.3, 0.2, 0, 0}, processing: 0.5, wantBound: 0.6},
		{name: "home wifi", up: delay{8, 6, 0.05, 80}, down: delay{8, 6, 0.05, 80}, processing: 2, wantBound: 13},
		{name: "asymmetric uplink", up: delay{40, 4, 0, 0}, down: delay{8, 2, 0, 0}, processing: 2, wantBound: 27},
		{name: "mobile with spikes", up: delay{30, 25, 0.15, 300}, down: delay{25, 20, 0.15, 300}, processing: 5, wantBound: 42},
		{name: "drifting client clock (80 ppm)", up: delay{8, 6, 0.05, 80}, down: delay{8, 6, 0.05, 80}, skewPPM: 80, processing: 2, wantBound: 13},
	}
	for _, n := range nets {
		t.Run(n.name, func(t *testing.T) {
			s := &sim{t: t, rng: rand.New(rand.NewPCG(7, 11)), net: n, offset: 123_456.789, session: NewSession(DefaultConfig())}
			first := s.run(1000) // the join burst
			final := s.run(5 * 60_000)
			if final.Bound > n.wantBound {
				t.Errorf("converged bound %.2f ms, want <= %.2f", final.Bound, n.wantBound)
			}
			if s.session.Steps() != 0 {
				t.Errorf("%d spurious steps", s.session.Steps())
			}
			t.Logf("after burst: bound %.2f ms; after 5 min: bound %.2f ms, error %.3f ms, RTT %.1f ms",
				first.Bound, final.Bound, math.Abs(final.Offset-s.trueOffset(s.now)), final.RTT)
		})
	}
}

func TestStepIsDetectedAndRecovered(t *testing.T) {
	n := network{up: delay{8, 6, 0, 0}, down: delay{8, 6, 0, 0}, processing: 2}
	s := &sim{t: t, rng: rand.New(rand.NewPCG(3, 4)), net: n, offset: 5000, session: NewSession(DefaultConfig())}
	s.run(60_000)

	s.offset += 500 // laptop slept; the client's clock jumped
	id, _ := s.session.Ping(s.now)
	atClient, replyAt := s.now+9, s.now+10
	if err := s.session.Pong(id, s.client(atClient), s.client(replyAt), s.now+19); err != nil {
		t.Fatal(err)
	}
	if s.session.Steps() != 1 {
		t.Fatalf("steps = %d, want 1", s.session.Steps())
	}
	if got := s.session.NextInterval(); got != burstInterval {
		t.Errorf("after a step the next ping comes in %v ms, want a new burst", got)
	}
	s.now += s.session.NextInterval()
	est := s.run(60_000) // checks the bound is honest again throughout
	if math.Abs(est.Offset-s.trueOffset(s.now)) > est.Bound {
		t.Errorf("estimate did not recover: %+v", est)
	}
}

// A client controls T2 and T3. It can move its offset and shrink its apparent
// RTT, and nothing in a single exchange can detect it; adjudication must cap
// any correction it derives from these numbers.
func TestLyingClient(t *testing.T) {
	// True: offset 0, 20 ms each way, 10 ms processing.
	honest := Sample{T1: 0, T2: 20, T3: 30, T4: 50}
	shifted := Sample{T1: 0, T2: 220, T3: 230, T4: 50} // claims its clock is 200 ms ahead
	padded := Sample{T1: 0, T2: 20, T3: 55, T4: 50}    // claims 25 ms extra processing

	if honest.RTT() != 40 || shifted.RTT() != 40 || shifted.Offset() != 200 {
		t.Errorf("shifted clock: RTT %v offset %v", shifted.RTT(), shifted.Offset())
	}
	if padded.RTT() >= honest.RTT() {
		t.Errorf("padding processing time should shrink apparent RTT: %v vs %v", padded.RTT(), honest.RTT())
	}
	e := NewEstimator(DefaultConfig())
	if _, err := e.Add(Sample{T1: 0, T2: 20, T3: 80, T4: 50}); !errors.Is(err, ErrNegativeRTT) {
		t.Errorf("impossible claim accepted: %v", err)
	}
}

func TestSessionCadenceAndBookkeeping(t *testing.T) {
	s := NewSession(DefaultConfig())
	now := 0.0
	var intervals []float64
	for range burstPings + 2 {
		intervals = append(intervals, s.NextInterval())
		id, _ := s.Ping(now)
		if err := s.Pong(id, now+5, now+5, now+10); err != nil {
			t.Fatal(err)
		}
		now += intervals[len(intervals)-1]
	}
	want := []float64{200, 200, 200, 200, 200, 2000, 2000}
	for i := range want {
		if intervals[i] != want[i] {
			t.Fatalf("intervals = %v, want %v", intervals, want)
		}
	}

	// Unanswered pings are capped.
	s = NewSession(DefaultConfig())
	for i := range maxOutstanding {
		if _, ok := s.Ping(float64(i)); !ok {
			t.Fatalf("ping %d refused", i)
		}
	}
	if _, ok := s.Ping(10); ok {
		t.Error("ping beyond the outstanding cap was allowed")
	}
	// ...and expire.
	if _, ok := s.Ping(pingTimeout + 100); !ok {
		t.Error("expired pings still count toward the cap")
	}

	s = NewSession(DefaultConfig())
	id, _ := s.Ping(0)
	if err := s.Pong(id+1, 1, 1, 2); !errors.Is(err, ErrUnknownPing) {
		t.Errorf("unknown ID: %v", err)
	}
	if err := s.Pong(id, 1, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Pong(id, 1, 1, 2); !errors.Is(err, ErrUnknownPing) {
		t.Errorf("duplicate pong: %v", err)
	}
	id, _ = s.Ping(100)
	if err := s.Pong(id, 101, 101, 100+pingTimeout+1); !errors.Is(err, ErrUnknownPing) {
		t.Errorf("late pong: %v", err)
	}
}

func TestServerClock(t *testing.T) {
	c := NewServer()
	at := time.Now().Add(1500 * time.Millisecond)
	ms := c.At(at)
	if got := c.Time(ms); got.Sub(at).Abs() > time.Microsecond {
		t.Errorf("round trip through ms lost %v", got.Sub(at))
	}
	if c.Now() < 0 {
		t.Error("clock before its epoch")
	}
}
