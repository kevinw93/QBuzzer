// Package metrics holds process-wide latency histograms. Observing is
// lock-free (atomic counters), so the room loop can record without
// contention.
package metrics

import (
	"math"
	"sync"
	"sync/atomic"
)

// Histogram counts observations in exponentially sized buckets.
type Histogram struct {
	bounds []float64 // upper bounds; a final overflow bucket follows
	counts []atomic.Uint64
	n      atomic.Uint64
	sum    atomic.Uint64 // float64 bits
	max    atomic.Uint64 // float64 bits
}

// NewHistogram covers [lo, hi] with buckets growing by factor.
func NewHistogram(lo, hi, factor float64) *Histogram {
	var bounds []float64
	for b := lo; b < hi*factor; b *= factor {
		bounds = append(bounds, b)
	}
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

func (h *Histogram) Observe(v float64) {
	i := 0
	for i < len(h.bounds) && v > h.bounds[i] {
		i++
	}
	h.counts[i].Add(1)
	h.n.Add(1)
	addFloat(&h.sum, v)
	for {
		old := h.max.Load()
		if v <= math.Float64frombits(old) || h.max.CompareAndSwap(old, math.Float64bits(v)) {
			return
		}
	}
}

func addFloat(u *atomic.Uint64, v float64) {
	for {
		old := u.Load()
		if u.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+v)) {
			return
		}
	}
}

// Summary is a point-in-time digest. Quantiles are interpolated within
// buckets, so they are accurate to one bucket width.
type Summary struct {
	Count uint64  `json:"count"`
	Mean  float64 `json:"mean"`
	P50   float64 `json:"p50"`
	P90   float64 `json:"p90"`
	P99   float64 `json:"p99"`
	Max   float64 `json:"max"`
}

func (h *Histogram) Summary() Summary {
	counts := make([]uint64, len(h.counts))
	var n uint64
	for i := range h.counts {
		counts[i] = h.counts[i].Load()
		n += counts[i]
	}
	s := Summary{Count: n, Max: math.Float64frombits(h.max.Load())}
	if n == 0 {
		return s
	}
	s.Mean = math.Float64frombits(h.sum.Load()) / float64(n)
	s.P50, s.P90, s.P99 = h.quantile(counts, n, 0.5), h.quantile(counts, n, 0.9), h.quantile(counts, n, 0.99)
	return s
}

func (h *Histogram) quantile(counts []uint64, n uint64, q float64) float64 {
	target := q * float64(n)
	var cum float64
	for i, c := range counts {
		if c == 0 {
			continue
		}
		if cum+float64(c) >= target {
			if i == len(h.bounds) {
				return math.Float64frombits(h.max.Load()) // the overflow bucket has no upper bound to interpolate to
			}
			lo, hi := 0.0, math.Float64frombits(h.max.Load())
			if i > 0 {
				lo = h.bounds[i-1]
			}
			if i < len(h.bounds) {
				hi = min(h.bounds[i], hi)
			}
			return lo + (hi-lo)*(target-cum)/float64(c)
		}
		cum += float64(c)
	}
	return math.Float64frombits(h.max.Load())
}

// Room loop timings, in milliseconds, and state sizes, in bytes.
var (
	InboxDelay   = NewHistogram(0.005, 10_000, 1.25) // message read → handled by its room
	LockLateness = NewHistogram(0.005, 10_000, 1.25) // window deadline + grace → lock handled
	Broadcast    = NewHistogram(0.001, 1_000, 1.25)  // building, encoding, and queueing one state
	StateBytes   = NewHistogram(64, 1<<20, 1.25)
)

// All is every histogram by name, for reporting.
func All() map[string]Summary {
	return map[string]Summary{
		"inboxDelayMs":   InboxDelay.Summary(),
		"lockLatenessMs": LockLateness.Summary(),
		"broadcastMs":    Broadcast.Summary(),
		"stateBytes":     StateBytes.Summary(),
	}
}

// Reset zeroes h. Concurrent observations may straddle it; it's for
// separating benchmark phases, not for exact accounting.
func (h *Histogram) Reset() {
	for i := range h.counts {
		h.counts[i].Store(0)
	}
	h.n.Store(0)
	h.sum.Store(0)
	h.max.Store(0)
}

func ResetAll() {
	for _, h := range []*Histogram{InboxDelay, LockLateness, Broadcast, StateBytes} {
		h.Reset()
	}
	Outbound.Range(func(k, _ any) bool { Outbound.Delete(k); return true })
}

// Outbound counts WebSocket messages written, by message type.
var Outbound sync.Map // protocol.MsgType string -> *atomic.Uint64

func CountOutbound(typ string) {
	v, ok := Outbound.Load(typ)
	if !ok {
		v, _ = Outbound.LoadOrStore(typ, new(atomic.Uint64))
	}
	v.(*atomic.Uint64).Add(1) //nolint:forcetypeassert // only *atomic.Uint64 is stored
}

func OutboundCounts() map[string]uint64 {
	out := map[string]uint64{}
	Outbound.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Uint64).Load() //nolint:forcetypeassert // see CountOutbound
		return true
	})
	return out
}

// Gauges and counters for operations, read by the Prometheus endpoint.
var (
	Connections    atomic.Int64  // open WebSocket connections
	WriterFlushes  atomic.Uint64 // committed batches
	WriterFailures atomic.Uint64 // failed flush attempts
	WriterDropped  atomic.Uint64 // batches given up on (their games become unrecoverable)
	RateLimited    sync.Map      // kind -> *atomic.Uint64
)

// WriterQueue reports ops waiting in the persistence queue; nil without one.
var WriterQueue func() int

func CountRateLimited(kind string) {
	v, ok := RateLimited.Load(kind)
	if !ok {
		v, _ = RateLimited.LoadOrStore(kind, new(atomic.Uint64))
	}
	v.(*atomic.Uint64).Add(1) //nolint:forcetypeassert // only *atomic.Uint64 is stored
}
