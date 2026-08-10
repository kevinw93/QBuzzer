package httpapi

import (
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"

	"github.com/kevinw93/QBuzzer/internal/metrics"
	"github.com/kevinw93/QBuzzer/internal/room"
)

// Stats is the body of GET /debug/stats.
type Stats struct {
	Rooms          int                        `json:"rooms"`
	Goroutines     int                        `json:"goroutines"`
	HeapInuseBytes uint64                     `json:"heapInuseBytes"`
	HeapAllocBytes uint64                     `json:"heapAllocBytes"`
	NumGC          uint32                     `json:"numGC"`
	Histograms     map[string]metrics.Summary `json:"histograms"`
	Outbound       map[string]uint64          `json:"outbound"` // messages written, by type
}

var ErrNotLoopback = errors.New("debug listener must bind a loopback address")

// CheckLoopback rejects debug addresses reachable from other machines:
// profiles expose memory contents and internals.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return ErrNotLoopback
	}
	return nil
}

// NewDebug serves pprof and runtime stats. Bind it to loopback only.
func NewDebug(hub *room.Hub) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("GET /debug/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("gc") {
			runtime.GC() // settles the heap for per-room memory measurements
		}
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		writeJSON(w, http.StatusOK, Stats{
			Rooms: hub.Len(), Goroutines: runtime.NumGoroutine(),
			HeapInuseBytes: m.HeapInuse, HeapAllocBytes: m.HeapAlloc, NumGC: m.NumGC,
			Histograms: metrics.All(), Outbound: metrics.OutboundCounts(),
		})
		if r.URL.Query().Has("reset") {
			metrics.ResetAll() // after reporting, to start a new measurement phase
		}
	})
	return mux
}
