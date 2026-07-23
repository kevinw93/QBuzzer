package clock

import (
	"errors"
	"sync"
)

const (
	burstPings     = 5
	burstInterval  = 200  // ms
	steadyInterval = 2000 // ms
	pingTimeout    = 5000 // ms; unanswered pings are forgotten
	maxOutstanding = 8
)

var ErrUnknownPing = errors.New("clock: pong for an unknown or expired ping")

// Session runs the ping protocol for one connection: a burst of pings on
// connect (and after a detected step), then a steady cadence. It is safe for
// concurrent use by the connection's pinger and reader.
type Session struct {
	mu      sync.Mutex
	est     *Estimator
	pending map[uint64]float64 // ping ID -> T1
	nextID  uint64
	burst   int // pings left in the current burst
}

func NewSession(cfg Config) *Session {
	return &Session{est: NewEstimator(cfg), pending: map[uint64]float64{}, burst: burstPings}
}

// NextInterval is how long to wait before the next ping, in ms.
func (s *Session) NextInterval() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.burst > 0 {
		return burstInterval
	}
	return steadyInterval
}

// Ping registers a ping sent at server time t1 and returns its ID. ok is
// false if too many pings are unanswered; send nothing then.
func (s *Session) Ping(t1 float64) (id uint64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for pid, sent := range s.pending {
		if t1-sent > pingTimeout {
			delete(s.pending, pid)
		}
	}
	if len(s.pending) >= maxOutstanding {
		return 0, false
	}
	s.nextID++
	s.pending[s.nextID] = t1
	if s.burst > 0 {
		s.burst--
	}
	return s.nextID, true
}

// Pong completes ping id with the client's T2 and T3 and the server's T4.
func (s *Session) Pong(id uint64, t2, t3, t4 float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t1, ok := s.pending[id]
	if !ok || t4-t1 > pingTimeout {
		delete(s.pending, id)
		return ErrUnknownPing
	}
	delete(s.pending, id)
	step, err := s.est.Add(Sample{T1: t1, T2: t2, T3: t3, T4: t4})
	if step {
		s.burst = burstPings
	}
	return err
}

func (s *Session) Estimate(now float64) (Estimate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.est.Estimate(now)
}

func (s *Session) Steps() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.est.Steps()
}
