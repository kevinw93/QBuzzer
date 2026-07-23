// Package httpapi is the HTTP layer: REST endpoints, the WebSocket endpoint,
// and the embedded frontend.
package httpapi

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

type Server struct {
	hub         *room.Hub
	log         *slog.Logger
	clk         clock.Server
	joinTimeout time.Duration
}

// New returns the application handler. assets is the built frontend.
func New(hub *room.Hub, assets fs.FS, log *slog.Logger) http.Handler {
	s := &Server{hub: hub, log: log, clk: clock.NewServer(), joinTimeout: defaultJoinTimeout}
	return s.handler(assets)
}

func (s *Server) handler(assets fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /api/rooms", s.createRoom)
	mux.HandleFunc("GET /api/rooms/{code}/ws", s.joinRoom)
	mux.Handle("GET /", http.FileServerFS(assets))
	return mux
}

// createRoom is unauthenticated: anyone may open a room and becomes its
// moderator by holding the returned token.
func (s *Server) createRoom(w http.ResponseWriter, _ *http.Request) {
	rm, err := s.hub.Create()
	switch {
	case errors.Is(err, room.ErrTooManyRooms):
		http.Error(w, "server is at its room limit; try again later", http.StatusServiceUnavailable)
		return
	case err != nil:
		s.log.Error("create room", "err", err)
		http.Error(w, "could not create room", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store") // carries a credential
	writeJSON(w, http.StatusCreated, protocol.CreateRoomResponse{Code: rm.Code(), ModeratorToken: rm.ModeratorToken()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // the status line is already sent
}
