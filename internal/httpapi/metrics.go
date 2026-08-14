package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/kevinw93/QBuzzer/internal/metrics"
	"github.com/kevinw93/QBuzzer/internal/room"
)

// NewMetrics serves Prometheus text-format metrics. It exposes no game or
// player data, but bind it to a private address anyway.
func NewMetrics(hub *room.Hub) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		gauge(w, "qbuzzer_rooms", "Live rooms.", float64(hub.Len()))
		gauge(w, "qbuzzer_connections", "Open WebSocket connections.", float64(metrics.Connections.Load()))
		gauge(w, "qbuzzer_goroutines", "Goroutines.", float64(runtime.NumGoroutine()))
		gauge(w, "qbuzzer_heap_inuse_bytes", "Heap in use.", float64(m.HeapInuse))
		if metrics.WriterQueue != nil {
			gauge(w, "qbuzzer_writer_queue_ops", "Ops waiting to be persisted.", float64(metrics.WriterQueue()))
		}
		counter(w, "qbuzzer_writer_flushes_total", "Committed persistence batches.", metrics.WriterFlushes.Load())
		counter(w, "qbuzzer_writer_failures_total", "Failed persistence flush attempts.", metrics.WriterFailures.Load())
		counter(w, "qbuzzer_writer_dropped_batches_total", "Persistence batches given up on.", metrics.WriterDropped.Load())

		summary(w, "qbuzzer_lock_lateness_ms", "How late a buzz lock fired after its window deadline.", metrics.LockLateness.Summary())
		summary(w, "qbuzzer_inbox_delay_ms", "Time from reading a message to its room handling it.", metrics.InboxDelay.Summary())
		summary(w, "qbuzzer_broadcast_ms", "Time to build, encode, and queue one state broadcast.", metrics.Broadcast.Summary())
		summary(w, "qbuzzer_state_bytes", "Encoded state size.", metrics.StateBytes.Summary())

		labeled(w, "qbuzzer_messages_written_total", "WebSocket messages written, by type.", "type", &metrics.Outbound)
		labeled(w, "qbuzzer_rate_limited_total", "Requests refused by per-IP rate limits.", "kind", &metrics.RateLimited)
	})
}

func gauge(w io.Writer, name, help string, v float64) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

func counter(w io.Writer, name, help string, v uint64) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

func summary(w io.Writer, name, help string, s metrics.Summary) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s summary\n", name, help, name)
	for _, q := range []struct {
		q string
		v float64
	}{{"0.5", s.P50}, {"0.9", s.P90}, {"0.99", s.P99}} {
		_, _ = fmt.Fprintf(w, "%s{quantile=%q} %g\n", name, q.q, q.v)
	}
	_, _ = fmt.Fprintf(w, "%s_sum %g\n%s_count %d\n", name, s.Mean*float64(s.Count), name, s.Count)
}

func labeled(w io.Writer, name, help, label string, m *sync.Map) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	var keys []string
	m.Range(func(k, _ any) bool { keys = append(keys, k.(string)); return true }) //nolint:forcetypeassert // keys are strings
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := m.Load(k)
		_, _ = fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, v.(*atomic.Uint64).Load()) //nolint:forcetypeassert // values are *atomic.Uint64
	}
}
