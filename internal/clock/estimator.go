package clock

import (
	"errors"
	"math"
	"time"
)

// Server is the server's millisecond clock. It reads Go's monotonic clock,
// so wall-clock adjustments never move it.
type Server struct{ epoch time.Time }

func NewServer() Server { return Server{epoch: time.Now()} }

// Now returns milliseconds since the clock was created.
func (s Server) Now() float64 { return s.At(time.Now()) }

// At converts a time.Time (with a monotonic reading) to this clock.
func (s Server) At(t time.Time) float64 {
	return float64(t.Sub(s.epoch).Nanoseconds()) / 1e6
}

// Time converts milliseconds on this clock back to a time.Time.
func (s Server) Time(ms float64) time.Time {
	return s.epoch.Add(time.Duration(ms * 1e6))
}

// Sample is one four-timestamp exchange.
type Sample struct {
	T1, T2, T3, T4 float64
}

func (s Sample) RTT() float64    { return (s.T4 - s.T1) - (s.T3 - s.T2) }
func (s Sample) Offset() float64 { return ((s.T2 - s.T1) + (s.T3 - s.T4)) / 2 }

// Estimate is the current best offset. For an honest client whose clock
// drifts no faster than Config.DriftPPM, the true offset at the time the
// estimate was taken is within Offset ± Bound.
type Estimate struct {
	Offset  float64 // client minus server
	RTT     float64 // of the sample the estimate is based on
	Bound   float64
	Samples int // samples in the window
}

// ToServer converts a client timestamp to server time.
func (e Estimate) ToServer(clientMs float64) float64 { return clientMs - e.Offset }

type Config struct {
	Window   float64 // ms; older samples are discarded
	DriftPPM float64 // assumed worst-case relative drift between the clocks
	MaxRTT   float64 // ms; slower samples are discarded as uninformative
}

func DefaultConfig() Config {
	return Config{Window: 30_000, DriftPPM: 100, MaxRTT: 5_000}
}

var (
	ErrNegativeRTT = errors.New("clock: sample has negative round-trip time")
	ErrSlowSample  = errors.New("clock: sample round-trip time exceeds MaxRTT")
)

// Estimator keeps a sliding window of samples. Not safe for concurrent use.
type Estimator struct {
	cfg     Config
	samples []Sample // ascending T4
	steps   int
}

func NewEstimator(cfg Config) *Estimator { return &Estimator{cfg: cfg} }

// Add records s. It reports a step when s is inconsistent with the current
// estimate: both claim to contain the true offset, but their intervals don't
// overlap, so one clock jumped (sleep, a frozen tab) or the client lied. On a
// step the window restarts from s.
func (e *Estimator) Add(s Sample) (step bool, err error) {
	rtt := s.RTT()
	if rtt < 0 {
		return false, ErrNegativeRTT
	}
	if rtt > e.cfg.MaxRTT {
		return false, ErrSlowSample
	}
	e.prune(s.T4)
	if cur, ok := e.Estimate(s.T4); ok && math.Abs(s.Offset()-cur.Offset) > cur.Bound+rtt/2 {
		e.samples = e.samples[:0]
		e.steps++
		step = true
	}
	e.samples = append(e.samples, s)
	return step, nil
}

// Estimate returns the estimate at server time now: the sample whose bound,
// RTT/2 widened by worst-case drift since it was taken, is smallest. That is
// usually the lowest-RTT sample, unless it is old enough that a fresher one
// is tighter.
func (e *Estimator) Estimate(now float64) (Estimate, bool) {
	e.prune(now)
	best, bestBound := -1, math.Inf(1)
	for i, s := range e.samples {
		if b := e.bound(s, now); b < bestBound {
			best, bestBound = i, b
		}
	}
	if best < 0 {
		return Estimate{}, false
	}
	s := e.samples[best]
	return Estimate{Offset: s.Offset(), RTT: s.RTT(), Bound: bestBound, Samples: len(e.samples)}, true
}

// Steps returns how many clock steps have been detected.
func (e *Estimator) Steps() int { return e.steps }

// bound widens RTT/2 by worst-case drift during the exchange and since it.
func (e *Estimator) bound(s Sample, now float64) float64 {
	return s.RTT()/2 + e.cfg.DriftPPM*1e-6*(s.RTT()+max(0, now-s.T4))
}

func (e *Estimator) prune(now float64) {
	cut := 0
	for cut < len(e.samples) && now-e.samples[cut].T4 > e.cfg.Window {
		cut++
	}
	e.samples = e.samples[cut:]
}
