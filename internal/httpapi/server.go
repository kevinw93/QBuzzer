// Package httpapi is the HTTP layer: REST endpoints, the WebSocket endpoint,
// and the embedded frontend.
package httpapi

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/limit"
	"github.com/kevinw93/QBuzzer/internal/metrics"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

type Server struct {
	hub         *room.Hub
	log         *slog.Logger
	clk         clock.Server
	joinTimeout time.Duration
	opts        Options
}

// Options are deployment settings; the zero value suits tests and local
// development (no rate limits, client IP from the TCP connection).
type Options struct {
	// CreateLimit and JoinLimit throttle room creation (which costs an
	// argon2id hash) and WebSocket joins per client IP. Nil disables.
	CreateLimit, JoinLimit *limit.Limiter
	// ClientIPHeader names a header set by a trusted reverse proxy (on Fly,
	// "Fly-Client-IP"). Set it only behind such a proxy: anywhere else a
	// client can forge it to dodge the limits.
	ClientIPHeader string
}

// New returns the application handler. assets is the built frontend.
func New(hub *room.Hub, assets fs.FS, log *slog.Logger, opts Options) http.Handler {
	s := &Server{hub: hub, log: log, clk: clock.NewServer(), joinTimeout: defaultJoinTimeout, opts: opts}
	return s.handler(assets)
}

func (s *Server) clientIP(r *http.Request) string {
	if s.opts.ClientIPHeader != "" {
		if ip := r.Header.Get(s.opts.ClientIPHeader); ip != "" {
			return ip
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// allow applies l to r's client IP, writing a 429 when it refuses.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, l *limit.Limiter, kind string) bool {
	if l == nil || l.Allow(s.clientIP(r)) {
		return true
	}
	metrics.CountRateLimited(kind)
	w.Header().Set("Retry-After", "60")
	http.Error(w, "too many requests; try again in a minute", http.StatusTooManyRequests)
	return false
}

func (s *Server) handler(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("GET /api/rooms/{code}", s.getRoom)
	mux.HandleFunc("GET /api/rooms/{code}/ws", s.joinRoom)
	mux.Handle("GET /", spa(assets))
	return mux
}

// createRoom is unauthenticated: anyone may open a room, and whoever knows
// the password they chose can moderate it.
func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, s.opts.CreateLimit, "create_room") {
		return
	}
	var req protocol.CreateRoomRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&req); err != nil {
		http.Error(w, "body must be JSON with moderatorPassword", http.StatusBadRequest)
		return
	}
	rm, err := s.hub.Create(req.ModeratorPassword)
	switch {
	case errors.Is(err, protocol.ErrInvalidPassword):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, room.ErrTooManyRooms):
		http.Error(w, "server is at its room limit; try again later", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.log.Error("create room", "err", err)
		http.Error(w, "could not create room", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusCreated, protocol.CreateRoomResponse{Code: rm.Code()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status line is already sent
}

// getRoom lets a client tell "room gone" from "network down", which a failed
// WebSocket handshake can't report to browser code.
func (s *Server) getRoom(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.hub.Get(r.PathValue("code"))
	if !ok {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, protocol.CreateRoomResponse{Code: rm.Code()})
}

// spa serves built assets, and index.html for extensionless paths that aren't
// files, so client-side routes like /r/ABC234 load the app while a missing
// asset is still a 404.
func spa(assets fs.FS) http.Handler {
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" {
			if _, err := fs.Stat(assets, name); err != nil && path.Ext(name) == "" {
				http.ServeFileFS(w, r, assets, "index.html")
				return
			}
		}
		files.ServeHTTP(w, r)
	})
}
