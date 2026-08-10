package metrics

import (
	"math"
	"sync"
	"testing"
)

func TestSummaryQuantiles(t *testing.T) {
	h := NewHistogram(0.01, 10_000, 1.1)
	for i := 1; i <= 1000; i++ {
		h.Observe(float64(i))
	}
	s := h.Summary()
	if s.Count != 1000 || s.Max != 1000 || math.Abs(s.Mean-500.5) > 1e-9 {
		t.Fatalf("summary = %+v", s)
	}
	for _, c := range []struct{ got, want float64 }{{s.P50, 500}, {s.P90, 900}, {s.P99, 990}} {
		if math.Abs(c.got-c.want)/c.want > 0.1 { // within one 10% bucket
			t.Errorf("quantile %v, want ≈ %v", c.got, c.want)
		}
	}
}

func TestConcurrentObserve(t *testing.T) {
	h := NewHistogram(0.01, 100, 1.25)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 1000 {
				h.Observe(float64(i % 50))
			}
		})
	}
	wg.Wait()
	if s := h.Summary(); s.Count != 8000 || s.Max != 49 {
		t.Errorf("summary = %+v", s)
	}
}

func TestEmptyAndOverflow(t *testing.T) {
	h := NewHistogram(1, 10, 2)
	if s := h.Summary(); s.Count != 0 || s.P99 != 0 {
		t.Errorf("empty = %+v", s)
	}
	h.Observe(1e6)
	if s := h.Summary(); s.P50 != 1e6 || s.Max != 1e6 {
		t.Errorf("overflow = %+v", s)
	}
}
