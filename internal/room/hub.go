package room

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type Config struct {
	MaxRooms          int
	MaxClientsPerRoom int
	IdleTimeout       time.Duration // an empty room shuts down after this long
	SendBuffer        int           // per-client outbound buffer, in messages
}

func DefaultConfig() Config {
	return Config{
		MaxRooms:          1000,
		MaxClientsPerRoom: 64,
		IdleTimeout:       10 * time.Minute,
		SendBuffer:        16,
	}
}

var ErrTooManyRooms = errors.New("too many rooms")

// Hub is the registry of live rooms. Its mutex guards only the map; game state
// lives in each room's goroutine.
type Hub struct {
	ctx context.Context
	cfg Config
	log *slog.Logger
	wg  sync.WaitGroup

	mu     sync.Mutex
	rooms  map[string]*Room
	closed bool // set by Wait so no wg.Add can race with wg.Wait
}

// NewHub returns a hub whose rooms all shut down when ctx is cancelled.
func NewHub(ctx context.Context, cfg Config, log *slog.Logger) *Hub {
	return &Hub{ctx: ctx, cfg: cfg, log: log, rooms: make(map[string]*Room)}
}

func (h *Hub) Create() (*Room, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if len(h.rooms) >= h.cfg.MaxRooms {
		return nil, ErrTooManyRooms
	}
	code, err := h.unusedCode()
	if err != nil {
		return nil, err
	}
	r := newRoom(code, h.cfg)
	h.rooms[code] = r
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		r.run(h.ctx, func() { h.remove(code) })
		h.log.Info("room closed", "code", code)
	}()
	h.log.Info("room created", "code", code)
	return r, nil
}

// Get is case-insensitive.
func (h *Hub) Get(code string) (*Room, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[strings.ToUpper(code)]
	return r, ok
}

// Wait blocks until every room has exited. Call it after cancelling the hub's
// context; Create fails afterwards.
func (h *Hub) Wait() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
	h.wg.Wait()
}

func (h *Hub) remove(code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.rooms, code)
}

const (
	codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O or 1/I
	codeLen      = 6
)

func (h *Hub) unusedCode() (string, error) {
	for range 10 {
		if c := newCode(); h.rooms[c] == nil {
			return c, nil
		}
	}
	return "", errors.New("could not find an unused room code")
}

func newCode() string {
	var b [codeLen]byte
	rand.Read(b[:]) // cannot fail since Go 1.24
	for i := range b {
		b[i] = codeAlphabet[b[i]&31] // len(codeAlphabet) == 32, so this is unbiased
	}
	return string(b[:])
}
