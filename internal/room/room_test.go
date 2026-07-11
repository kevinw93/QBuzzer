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

func playerJoin(name, token string) protocol.Join {
	return protocol.Join{ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RolePlayer, Name: name, Token: token}
}

// joinAs joins with j and consumes the Welcome.
func joinAs(t *testing.T, r *Room, j protocol.Join) (*Client, protocol.Welcome) {
	t.Helper()
	c := r.NewClient()
	if err := r.Join(c, j); err != nil {
		t.Fatalf("join %+v: %v", j, err)
	}
	return c, recvAs[protocol.Welcome](t, c, protocol.MsgWelcome)
}

func join(t *testing.T, r *Room, name string) (*Client, protocol.Welcome) {
	t.Helper()
	return joinAs(t, r, playerJoin(name, ""))
}

func joinModerator(t *testing.T, r *Room) *Client {
	t.Helper()
	c, w := joinAs(t, r, protocol.Join{
		ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RoleModerator, Token: r.ModeratorToken(),
	})
	if w.Role != protocol.RoleModerator {
		t.Fatalf("welcome role = %q", w.Role)
	}
	return c
}

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

func recvAs[T any](t *testing.T, c *Client, typ protocol.MsgType) T {
	t.Helper()
	env := recv(t, c)
	if env.Type != typ {
		t.Fatalf("got message type %q, want %q (payload %s)", env.Type, typ, env.Payload)
	}
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
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
		if err := json.Unmarshal(env.Payload, &s); err != nil {
			t.Fatal(err)
		}
		if ok(s) {
			return s
		}
	}
}

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

func send(t *testing.T, r *Room, c *Client, seq uint64, msg any) {
	t.Helper()
	if err := r.Deliver(c, Incoming{Seq: seq, Msg: msg, ReceivedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func names(s protocol.RoomState) string {
	var ns []string
	for _, p := range s.Players {
		n := p.Name
		if !p.Connected {
			n += "(away)"
		}
		ns = append(ns, n)
	}
	return strings.Join(ns, ",")
}

func namesAre(want string) func(protocol.RoomState) bool {
	return func(s protocol.RoomState) bool { return names(s) == want }
}

func TestJoinSendsWelcomeThenState(t *testing.T) {
	r := newTestRoom(t, nil)

	a, wa := join(t, r, "  Alice ")
	if wa.PlayerID != "p1" || wa.Role != protocol.RolePlayer || wa.SessionToken == "" || wa.LastSeq != 0 {
		t.Errorf("welcome = %+v", wa)
	}
	s := recvAs[protocol.RoomState](t, a, protocol.MsgState)
	if names(s) != "Alice" || s.Code != r.Code() || s.Epoch == "" || s.ModeratorConnected {
		t.Errorf("state = %+v", s)
	}

	b, wb := join(t, r, "Bob")
	if wb.PlayerID != "p2" || wb.SessionToken == wa.SessionToken {
		t.Errorf("bob welcome = %+v", wb)
	}
	sb := recvAs[protocol.RoomState](t, b, protocol.MsgState)
	sa := recvAs[protocol.RoomState](t, a, protocol.MsgState)
	if names(sa) != "Alice,Bob" || sa.Version != sb.Version || sa.Epoch != s.Epoch {
		t.Errorf("alice saw %+v, bob saw %+v", sa, sb)
	}
	if sa.Version <= s.Version {
		t.Errorf("version did not increase: %d then %d", s.Version, sa.Version)
	}
}

func TestEpochsDifferBetweenRooms(t *testing.T) {
	h, _ := newTestHub(t, nil)
	r1, _ := h.Create()
	r2, _ := h.Create()
	if r1.epoch == "" || r1.epoch == r2.epoch {
		t.Errorf("epochs %q and %q", r1.epoch, r2.epoch)
	}
}

func TestSetNameBroadcastsAndVersionsIncrease(t *testing.T) {
	r := newTestRoom(t, nil)
	a, _ := join(t, r, "Alice")
	last := recvAs[protocol.RoomState](t, a, protocol.MsgState).Version

	for i := range 5 {
		send(t, r, a, uint64(i+1), protocol.SetName{Name: fmt.Sprint("A", i)})
		s := recvAs[protocol.RoomState](t, a, protocol.MsgState)
		if s.Version <= last || names(s) != fmt.Sprint("A", i) {
			t.Fatalf("after rename %d: %+v (last version %d)", i, s, last)
		}
		last = s.Version
	}
}

func TestDuplicateSeqIsIgnored(t *testing.T) {
	r := newTestRoom(t, nil)
	a, _ := join(t, r, "Alice")
	recv(t, a) // state

	send(t, r, a, 1, protocol.SetName{Name: "First"})
	stateUntil(t, a, namesAre("First"))
	send(t, r, a, 1, protocol.SetName{Name: "Duplicate"})
	send(t, r, a, 0, protocol.SetName{Name: "Zero"})
	send(t, r, a, 2, protocol.SetName{Name: "Second"})

	// Events are processed in order, so if either ignored message had been
	// applied, its state would arrive before this one.
	if s := recvAs[protocol.RoomState](t, a, protocol.MsgState); names(s) != "Second" {
		t.Errorf("names = %q, want Second", names(s))
	}
}

func TestSeqGapsAreAllowed(t *testing.T) {
	r := newTestRoom(t, nil)
	a, _ := join(t, r, "Alice")
	recv(t, a)
	send(t, r, a, 5, protocol.SetName{Name: "Five"})
	stateUntil(t, a, namesAre("Five"))
	send(t, r, a, 3, protocol.SetName{Name: "Three"}) // below lastSeq: duplicate
	send(t, r, a, 9, protocol.SetName{Name: "Nine"})
	if s := recvAs[protocol.RoomState](t, a, protocol.MsgState); names(s) != "Nine" {
		t.Errorf("names = %q, want Nine", names(s))
	}
}

func TestReconnectResumesSeatAndSeq(t *testing.T) {
	r := newTestRoom(t, nil)
	a, wa := join(t, r, "Alice")
	b, _ := join(t, r, "Bob")
	stateUntil(t, b, namesAre("Alice,Bob"))

	send(t, r, a, 3, protocol.SetName{Name: "Alicia"})
	stateUntil(t, b, namesAre("Alicia,Bob"))

	r.Leave(a)
	if got := waitClosed(t, a); got != ReasonLeft {
		t.Errorf("closed with %v", got)
	}
	stateUntil(t, b, namesAre("Alicia(away),Bob"))

	a2, wa2 := joinAs(t, r, playerJoin("", wa.SessionToken))
	if wa2.PlayerID != wa.PlayerID || wa2.SessionToken != wa.SessionToken || wa2.LastSeq != 3 {
		t.Errorf("resumed welcome = %+v, original %+v", wa2, wa)
	}
	stateUntil(t, b, namesAre("Alicia,Bob"))

	// The client resends seq 3 because it never saw it acknowledged; it must
	// not be applied twice.
	send(t, r, a2, 3, protocol.SetName{Name: "Resent"})
	send(t, r, a2, 4, protocol.SetName{Name: "Al"})
	if s := stateUntil(t, b, func(s protocol.RoomState) bool { return names(s) != "Alicia,Bob" }); names(s) != "Al,Bob" {
		t.Errorf("names = %q, want Al,Bob", names(s))
	}
}

func TestJoinReplacesExistingConnection(t *testing.T) {
	r := newTestRoom(t, nil)
	a1, w := join(t, r, "Alice")
	a2, w2 := joinAs(t, r, playerJoin("Alice", w.SessionToken))
	if w2.PlayerID != w.PlayerID {
		t.Fatalf("replacement got a new seat: %+v", w2)
	}
	if got := waitClosed(t, a1); got != ReasonReplaced {
		t.Errorf("old connection closed with %v, want %v", got, ReasonReplaced)
	}
	r.Leave(a1) // the old connection's reader leaving must not disconnect the new one
	send(t, r, a2, 1, protocol.SetName{Name: "Still here"})
	stateUntil(t, a2, namesAre("Still here"))
}

func TestJoinRejections(t *testing.T) {
	r := newTestRoom(t, func(c *Config) { c.MaxPlayersPerRoom = 1 })
	_, w := join(t, r, "Alice")

	tests := []struct {
		name string
		join protocol.Join
		want error
	}{
		{"seats full", playerJoin("Bob", ""), ErrFull},
		{"unknown session", playerJoin("", "not-a-token"), ErrUnknownSession},
		{"wrong moderator token", protocol.Join{ProtocolVersion: 1, Role: protocol.RoleModerator, Token: w.SessionToken}, ErrUnauthorized},
		{"bad name", playerJoin("\x00", w.SessionToken), protocol.ErrInvalidName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Join(r.NewClient(), tt.join); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	// A full room still lets existing seats reconnect.
	joinAs(t, r, playerJoin("", w.SessionToken))
}

func TestModeratorPermissions(t *testing.T) {
	r := newTestRoom(t, nil)
	p, _ := join(t, r, "Alice")
	m := joinModerator(t, r)
	s := stateUntil(t, p, func(s protocol.RoomState) bool { return s.ModeratorConnected })
	if names(s) != "Alice" {
		t.Errorf("moderator should not be listed as a player: %q", names(s))
	}
	recv(t, m) // state

	tests := []struct {
		name     string
		from     *Client
		msg      any
		wantCode protocol.ErrCode
	}{
		{"player cannot rule", p, protocol.Rule{BuzzID: "b1", Result: protocol.RulingCorrect}, protocol.ErrCodeForbidden},
		{"player cannot advance", p, protocol.Next{TossupID: "t1"}, protocol.ErrCodeForbidden},
		{"moderator cannot buzz", m, protocol.Buzz{TossupID: "t1"}, protocol.ErrCodeForbidden},
		{"moderator action reaches game", m, protocol.Next{TossupID: "t1"}, protocol.ErrCodeNotImplemented},
		{"player buzz reaches game", p, protocol.Buzz{TossupID: "t1"}, protocol.ErrCodeNotImplemented},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seq := uint64(i + 1)
			send(t, r, tt.from, seq, tt.msg)
			e := recvAs[protocol.ErrorMessage](t, tt.from, protocol.MsgError)
			if e.Code != tt.wantCode || e.Seq != seq {
				t.Errorf("error = %+v, want code %q seq %d", e, tt.wantCode, seq)
			}
		})
	}

	r.Leave(m)
	stateUntil(t, p, func(s protocol.RoomState) bool { return !s.ModeratorConnected })
}

func TestErrorsGoOnlyToSender(t *testing.T) {
	r := newTestRoom(t, nil)
	a, _ := join(t, r, "Alice")
	b, _ := join(t, r, "Bob")
	stateUntil(t, a, namesAre("Alice,Bob"))
	recv(t, b) // state

	if err := r.Deliver(a, Incoming{Seq: 1, Err: errors.New("invalid buzz payload")}); err != nil {
		t.Fatal(err)
	}
	if e := recvAs[protocol.ErrorMessage](t, a, protocol.MsgError); e.Code != protocol.ErrCodeBadMessage || e.Seq != 1 {
		t.Errorf("error = %+v", e)
	}
	send(t, r, a, 2, protocol.SetName{Name: "   "})
	if e := recvAs[protocol.ErrorMessage](t, a, protocol.MsgError); e.Code != protocol.ErrCodeInvalidName {
		t.Errorf("error = %+v", e)
	}
	send(t, r, a, 3, playerJoin("Again", ""))
	if e := recvAs[protocol.ErrorMessage](t, a, protocol.MsgError); e.Code != protocol.ErrCodeBadMessage {
		t.Errorf("error = %+v", e)
	}

	// Events are processed in order, so if the errors leaked to Bob he would
	// see them before this state.
	send(t, r, a, 4, protocol.SetName{Name: "Alicia"})
	if s := recvAs[protocol.RoomState](t, b, protocol.MsgState); names(s) != "Alicia,Bob" {
		t.Errorf("bob saw %q", names(s))
	}
}

func TestSlowClientIsDroppedWithoutBlockingOthers(t *testing.T) {
	r := newTestRoom(t, func(c *Config) { c.SendBuffer = 4 })
	slow, _ := join(t, r, "Slow") // never reads
	fast, _ := join(t, r, "Fast")

	for i := range 10 {
		want := fmt.Sprint("Fast", i)
		send(t, r, fast, uint64(i+1), protocol.SetName{Name: want})
		stateUntil(t, fast, func(s protocol.RoomState) bool {
			return len(s.Players) == 2 && s.Players[1].Name == want
		})
	}

	if got := waitClosed(t, slow); got != ReasonSlow {
		t.Errorf("slow closed with %v, want %v", got, ReasonSlow)
	}
	r.Leave(slow) // must not double-close
	send(t, r, fast, 11, protocol.SetName{Name: "Done"})
	stateUntil(t, fast, namesAre("Slow(away),Done"))
}

func TestLeaveKeepsSeatAndIsIdempotent(t *testing.T) {
	r := newTestRoom(t, nil)
	a, _ := join(t, r, "Alice")
	b, _ := join(t, r, "Bob")
	stateUntil(t, a, namesAre("Alice,Bob"))

	r.Leave(b)
	r.Leave(b)
	if got := waitClosed(t, b); got != ReasonLeft {
		t.Errorf("bob closed with %v, want %v", got, ReasonLeft)
	}
	stateUntil(t, a, namesAre("Alice,Bob(away)"))
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
	if err := r.Join(r.NewClient(), playerJoin("Late", "")); !errors.Is(err, ErrClosed) {
		t.Errorf("join after close: err = %v, want ErrClosed", err)
	}
}

func TestRoomIdlesOnlyWithoutConnections(t *testing.T) {
	h, _ := newTestHub(t, func(c *Config) { c.IdleTimeout = 30 * time.Millisecond })
	r, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := join(t, r, "Alice")

	select {
	case <-r.Done():
		t.Fatal("room closed while occupied")
	case <-time.After(150 * time.Millisecond):
	}

	r.Leave(a) // the seat remains, but nobody is connected
	select {
	case <-r.Done():
	case <-time.After(timeout):
		t.Fatal("room did not close after its last connection left")
	}
}

func TestShutdownClosesClientsAndStopsCreation(t *testing.T) {
	h, cancel := newTestHub(t, nil)
	r, err := h.Create()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := join(t, r, "Alice")

	cancel()
	if got := waitClosed(t, a); got != ReasonRoomClosed {
		t.Errorf("closed with %v, want %v", got, ReasonRoomClosed)
	}
	h.Wait()
	if _, err := h.Create(); !errors.Is(err, ErrClosed) {
		t.Errorf("create after shutdown: err = %v", err)
	}
}

func TestHubLimitsCodesAndTokens(t *testing.T) {
	h, _ := newTestHub(t, func(c *Config) { c.MaxRooms = 50 })
	codes, tokens := map[string]bool{}, map[string]bool{}
	for range 50 {
		r, err := h.Create()
		if err != nil {
			t.Fatal(err)
		}
		code, tok := r.Code(), r.ModeratorToken()
		if len(code) != codeLen || strings.Trim(code, codeAlphabet) != "" || codes[code] {
			t.Errorf("bad or duplicate code %q", code)
		}
		if len(tok) < 26 || tokens[tok] {
			t.Errorf("bad or duplicate moderator token %q", tok)
		}
		codes[code], tokens[tok] = true, true
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
			if err := r.Join(c, playerJoin(fmt.Sprint("P", i), "")); err != nil {
				t.Error(err)
				return
			}
			for j := range 5 {
				msg := protocol.SetName{Name: fmt.Sprintf("P%d-%d", i, j)}
				if err := r.Deliver(c, Incoming{Seq: uint64(j + 1), Msg: msg}); err != nil {
					t.Error(err)
				}
			}
			r.Leave(c)
			<-drained
		})
	}
	wg.Wait()

	last, _ := join(t, r, "Last")
	s := recvAs[protocol.RoomState](t, last, protocol.MsgState)
	if len(s.Players) != 21 {
		t.Fatalf("players = %d, want 21", len(s.Players))
	}
	for _, p := range s.Players {
		if p.Connected != (p.Name == "Last") {
			t.Errorf("player %+v has wrong connected state", p)
		}
	}
}
