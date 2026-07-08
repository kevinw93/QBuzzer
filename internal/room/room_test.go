package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

const timeout = 2 * time.Second

func newTestHub(t *testing.T, mod func(*Config)) (*Hub, context.CancelFunc) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.IdleTimeout = time.Minute
	if mod != nil {
		mod(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := NewHub(ctx, cfg, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { cancel(); h.Wait() })
	return h, cancel
}

func newTestRoom(t *testing.T, mod func(*Config)) *Room {
	t.Helper()
	h, _ := newTestHub(t, mod)
	r, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func join(t *testing.T, r *Room, name string) *Client {
	t.Helper()
	c := r.NewClient()
	if err := r.Join(c, name); err != nil {
		t.Fatalf("join %q: %v", name, err)
	}
	return c
}

// recv returns the next message for c, failing if none arrives or c is closed.
func recv(t *testing.T, c *Client) protocol.Envelope {
	t.Helper()
	select {
	case b, ok := <-c.Outbound():
		if !ok {
			t.Fatalf("client closed unexpectedly (reason %v)", c.CloseReason())
		}
		var env protocol.Envelope
		if err := json.Unmarshal(b, &env); err != nil {
			t.Fatalf("bad envelope %s: %v", b, err)
		}
		return env
	case <-time.After(timeout):
		t.Fatal("timed out waiting for message")
	}
	panic("unreachable")
}

func recvAs[T any](t *testing.T, c *Client, typ string) T {
	t.Helper()
	env := recv(t, c)
	if env.Type != typ {
		t.Fatalf("got message type %q, want %q (data %s)", env.Type, typ, env.Data)
	}
	var v T
	if err := json.Unmarshal(env.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// stateUntil reads messages for c, skipping non-state ones, until a state
// satisfies ok.
func stateUntil(t *testing.T, c *Client, ok func(protocol.RoomState) bool) protocol.RoomState {
	t.Helper()
	for {
		env := recv(t, c)
		if env.Type != protocol.MsgState {
			continue
		}
		var s protocol.RoomState
		if err := json.Unmarshal(env.Data, &s); err != nil {
			t.Fatal(err)
		}
		if ok(s) {
			return s
		}
	}
}

// waitClosed drains c and returns why the room closed it.
func waitClosed(t *testing.T, c *Client) CloseReason {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-c.Outbound():
			if !ok {
				return c.CloseReason()
			}
		case <-deadline:
			t.Fatal("timed out waiting for client to be closed")
		}
	}
}

func setName(t *testing.T, r *Room, c *Client, name string) {
	t.Helper()
	data, err := json.Marshal(protocol.SetName{Name: name})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Deliver(c, protocol.Envelope{Type: protocol.MsgSetName, Data: data}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func names(s protocol.RoomState) string {
	var ns []string
	for _, p := range s.Players {
		ns = append(ns, p.Name)
	}
	return strings.Join(ns, ",")
}

func TestJoinSendsWelcomeThenState(t *testing.T) {
	r := newTestRoom(t, nil)

	a := join(t, r, "  Alice ")
	if w := recvAs[protocol.Welcome](t, a, protocol.MsgWelcome); w.PlayerID != "p1" {
		t.Errorf("alice id = %q, want p1", w.PlayerID)
	}
	s := recvAs[protocol.RoomState](t, a, protocol.MsgState)
	if names(s) != "Alice" || s.Code != r.Code() {
		t.Errorf("state = %+v", s)
	}

	b := join(t, r, "Bob")
	if w := recvAs[protocol.Welcome](t, b, protocol.MsgWelcome); w.PlayerID != "p2" {
		t.Errorf("bob id = %q, want p2", w.PlayerID)
	}
	sb := recvAs[protocol.RoomState](t, b, protocol.MsgState)
	sa := recvAs[protocol.RoomState](t, a, protocol.MsgState)
	if names(sa) != "Alice,Bob" || sa.Version != sb.Version {
		t.Errorf("alice saw %+v, bob saw %+v", sa, sb)
	}
	if sa.Version <= s.Version {
		t.Errorf("version did not increase: %d then %d", s.Version, sa.Version)
	}
}

func TestEmptyRoomStateEncodesPlayersAsArray(t *testing.T) {
	r := newTestRoom(t, nil)
	b, err := protocol.Encode(protocol.MsgState, r.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"players":[]`) {
		t.Errorf("got %s, want players as []", b)
	}
}

func TestSetNameBroadcastsAndVersionsIncrease(t *testing.T) {
	r := newTestRoom(t, nil)
	a := join(t, r, "Alice")
	recv(t, a) // welcome
	last := recvAs[protocol.RoomState](t, a, protocol.MsgState).Version

	for i := range 5 {
		setName(t, r, a, fmt.Sprint("A", i))
		s := recvAs[protocol.RoomState](t, a, protocol.MsgState)
		if s.Version <= last {
			t.Fatalf("version %d after %d", s.Version, last)
		}
		if names(s) != fmt.Sprint("A", i) {
			t.Fatalf("names = %q", names(s))
		}
		last = s.Version
	}
}

func TestBadRequestsGetErrorsOnlyToSender(t *testing.T) {
	r := newTestRoom(t, nil)
	a := join(t, r, "Alice")
	b := join(t, r, "Bob")
	stateUntil(t, a, func(s protocol.RoomState) bool { return len(s.Players) == 2 })
	recv(t, b) // welcome
	recv(t, b) // state

	tests := []struct {
		name     string
		env      protocol.Envelope
		wantCode string
	}{
		{"invalid name", protocol.Envelope{Type: protocol.MsgSetName, Data: json.RawMessage(`{"name":"   "}`)}, protocol.ErrCodeInvalidName},
		{"bad payload", protocol.Envelope{Type: protocol.MsgSetName, Data: json.RawMessage(`[1,2]`)}, protocol.ErrCodeBadMessage},
		{"missing payload", protocol.Envelope{Type: protocol.MsgSetName}, protocol.ErrCodeBadMessage},
		{"unknown type", protocol.Envelope{Type: "launch_missiles"}, protocol.ErrCodeUnknownType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Deliver(a, tt.env, time.Now()); err != nil {
				t.Fatal(err)
			}
			if e := recvAs[protocol.ErrorMessage](t, a, protocol.MsgError); e.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", e.Code, tt.wantCode)
			}
		})
	}

	// Events are processed in order, so if the errors leaked to Bob he would
	// see them before this state.
	setName(t, r, a, "Alicia")
	if s := recvAs[protocol.RoomState](t, b, protocol.MsgState); names(s) != "Alicia,Bob" {
		t.Errorf("bob saw %q", names(s))
	}
}

func TestSlowClientIsDroppedWithoutBlockingOthers(t *testing.T) {
	r := newTestRoom(t, func(c *Config) { c.SendBuffer = 4 })
	slow := join(t, r, "Slow") // never reads
	fast := join(t, r, "Fast")

	for i := range 10 {
		want := fmt.Sprint("Fast", i)
		setName(t, r, fast, want)
		stateUntil(t, fast, func(s protocol.RoomState) bool {
			return len(s.Players) > 0 && s.Players[len(s.Players)-1].Name == want
		})
	}

	if got := waitClosed(t, slow); got != ReasonSlow {
		t.Errorf("slow closed with %v, want %v", got, ReasonSlow)
	}
	r.Leave(slow) // must not double-close
	setName(t, r, fast, "Done")
	if s := stateUntil(t, fast, func(s protocol.RoomState) bool { return names(s) == "Done" }); len(s.Players) != 1 {
		t.Errorf("players = %q, want only Done", names(s))
	}
}

func TestLeaveRemovesPlayerAndIsIdempotent(t *testing.T) {
	r := newTestRoom(t, nil)
	a := join(t, r, "Alice")
	b := join(t, r, "Bob")
	stateUntil(t, a, func(s protocol.RoomState) bool { return len(s.Players) == 2 })

	r.Leave(b)
	r.Leave(b)
	if got := waitClosed(t, b); got != ReasonLeft {
		t.Errorf("bob closed with %v, want %v", got, ReasonLeft)
	}
	stateUntil(t, a, func(s protocol.RoomState) bool { return names(s) == "Alice" })
}

func TestJoinRejections(t *testing.T) {
	r := newTestRoom(t, func(c *Config) { c.MaxClientsPerRoom = 1 })
	if err := r.Join(r.NewClient(), "\x00bad"); !errors.Is(err, protocol.ErrInvalidName) {
		t.Errorf("control char name: err = %v", err)
	}
	if err := r.Join(r.NewClient(), strings.Repeat("x", protocol.MaxNameLen+1)); !errors.Is(err, protocol.ErrInvalidName) {
		t.Errorf("long name: err = %v", err)
	}
	join(t, r, "Alice")
	if err := r.Join(r.NewClient(), "Bob"); !errors.Is(err, ErrFull) {
		t.Errorf("full room: err = %v, want ErrFull", err)
	}
}

func TestIdleRoomShutsDown(t *testing.T) {
	h, _ := newTestHub(t, func(c *Config) { c.IdleTimeout = 20 * time.Millisecond })
	r, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	case <-time.After(timeout):
		t.Fatal("idle room never shut down")
	}
	if _, ok := h.Get(r.Code()); ok {
		t.Error("closed room still registered in hub")
	}
	if err := r.Join(r.NewClient(), "Late"); !errors.Is(err, ErrClosed) {
		t.Errorf("join after close: err = %v, want ErrClosed", err)
	}
}

func TestOccupiedRoomStaysOpenThenIdlesAfterLastLeave(t *testing.T) {
	h, _ := newTestHub(t, func(c *Config) { c.IdleTimeout = 30 * time.Millisecond })
	r, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	a := join(t, r, "Alice")

	select {
	case <-r.Done():
		t.Fatal("room closed while occupied")
	case <-time.After(150 * time.Millisecond):
	}

	r.Leave(a)
	select {
	case <-r.Done():
	case <-time.After(timeout):
		t.Fatal("room did not close after becoming empty")
	}
}

func TestShutdownClosesClientsAndStopsCreation(t *testing.T) {
	h, cancel := newTestHub(t, nil)
	r, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	a := join(t, r, "Alice")

	cancel()
	if got := waitClosed(t, a); got != ReasonRoomClosed {
		t.Errorf("closed with %v, want %v", got, ReasonRoomClosed)
	}
	h.Wait()
	if _, err := h.Create(); !errors.Is(err, ErrClosed) {
		t.Errorf("create after shutdown: err = %v", err)
	}
}

func TestHubLimitsAndCodes(t *testing.T) {
	h, _ := newTestHub(t, func(c *Config) { c.MaxRooms = 50 })
	seen := map[string]bool{}
	for range 50 {
		r, err := h.Create()
		if err != nil {
			t.Fatal(err)
		}
		code := r.Code()
		if len(code) != codeLen || strings.Trim(code, codeAlphabet) != "" {
			t.Errorf("bad code %q", code)
		}
		if seen[code] {
			t.Errorf("duplicate code %q", code)
		}
		seen[code] = true
		if got, ok := h.Get(strings.ToLower(code)); !ok || got != r {
			t.Errorf("lowercase lookup of %q failed", code)
		}
	}
	if _, err := h.Create(); !errors.Is(err, ErrTooManyRooms) {
		t.Errorf("err = %v, want ErrTooManyRooms", err)
	}
}

func TestConcurrentClients(t *testing.T) {
	r := newTestRoom(t, nil)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			c := r.NewClient()
			drained := make(chan struct{})
			go func() {
				for range c.Outbound() {
				}
				close(drained)
			}()
			if err := r.Join(c, fmt.Sprint("P", i)); err != nil {
				t.Error(err)
				return
			}
			for j := range 5 {
				data, _ := json.Marshal(protocol.SetName{Name: fmt.Sprintf("P%d-%d", i, j)})
				if err := r.Deliver(c, protocol.Envelope{Type: protocol.MsgSetName, Data: data}, time.Now()); err != nil {
					t.Error(err)
				}
			}
			r.Leave(c)
			<-drained
		})
	}
	wg.Wait()

	last := join(t, r, "Last")
	recv(t, last) // welcome
	if s := recvAs[protocol.RoomState](t, last, protocol.MsgState); names(s) != "Last" {
		t.Errorf("players = %q, want only Last", names(s))
	}
}
