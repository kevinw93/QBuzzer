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
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// Room tests run inside synctest bubbles: time is virtual, so timeouts are
// exact and instant, and synctest.Wait() means "the room has processed
// everything sent so far".
const timeout = 2 * time.Second

func newTestHub(t *testing.T, mod func(*Config)) (*Hub, context.CancelFunc) {
	t.Helper()
	cfg := DefaultConfig()
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	b, err := c.Next(ctx)
	if errors.Is(err, ErrClientClosed) {
		t.Fatalf("client closed unexpectedly (reason %v)", c.CloseReason())
	}
	if err != nil {
		t.Fatal("timed out waiting for message")
	}
	var env protocol.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("bad envelope %s: %v", b, err)
	}
	return env
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		_, err := c.Next(ctx)
		if errors.Is(err, ErrClientClosed) {
			return c.CloseReason()
		}
		if err != nil {
			t.Fatal("timed out waiting for client to be closed")
		}
	}
}

// expectNothing asserts that, once the room is idle, nothing is pending for c.
func expectNothing(t *testing.T, c *Client) {
	t.Helper()
	synctest.Wait()
	if hasPending(c) {
		t.Errorf("unexpected pending message: %+v", recv(t, c))
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

// recordingGame records every Command and answers with respond (or a plain
// state change).
type recordingGame struct {
	mu      sync.Mutex
	cmds    []Command
	respond func(Command) Result
}

func (g *recordingGame) Handle(cmd Command) Result {
	g.mu.Lock()
	g.cmds = append(g.cmds, cmd)
	g.mu.Unlock()
	if g.respond != nil {
		return g.respond(cmd)
	}
	return Result{Changed: true}
}

func (g *recordingGame) commands() []Command {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Command(nil), g.cmds...)
}

func withGame(g Game) func(*Config) {
	return func(c *Config) { c.NewGame = func() Game { return g } }
}

func TestJoinSendsWelcomeThenState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		if names(sa) != "Alice,Bob" || sa.Version != sb.Version || sa.Epoch != s.Epoch || sa.Version <= s.Version {
			t.Errorf("first %+v, alice saw %+v, bob saw %+v", s, sa, sb)
		}
	})
}

func TestEpochsDifferBetweenRooms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := newTestHub(t, nil)
		r1, _ := h.Create()
		r2, _ := h.Create()
		if r1.epoch == "" || r1.epoch == r2.epoch {
			t.Errorf("epochs %q and %q", r1.epoch, r2.epoch)
		}
	})
}

func TestSetNameBroadcastsAndVersionsIncrease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestDuplicateAndOldSeqsAreIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRoom(t, nil)
		a, _ := join(t, r, "Alice")
		recv(t, a) // state

		send(t, r, a, 5, protocol.SetName{Name: "Five"})
		stateUntil(t, a, namesAre("Five"))
		send(t, r, a, 5, protocol.SetName{Name: "Duplicate"})
		send(t, r, a, 3, protocol.SetName{Name: "Old"})
		send(t, r, a, 0, protocol.SetName{Name: "Zero"})
		expectNothing(t, a)

		send(t, r, a, 9, protocol.SetName{Name: "Nine"}) // gaps are allowed
		if s := recvAs[protocol.RoomState](t, a, protocol.MsgState); names(s) != "Nine" {
			t.Errorf("names = %q, want Nine", names(s))
		}
	})
}

func TestReconnectResumesSeatAndSeq(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		send(t, r, a2, 3, protocol.SetName{Name: "Resent"}) // never acknowledged, so resent
		expectNothing(t, b)
		send(t, r, a2, 4, protocol.SetName{Name: "Al"})
		stateUntil(t, b, namesAre("Al,Bob"))
	})
}

func TestJoinReplacesExistingConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestJoinRejections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRoom(t, func(c *Config) { c.MaxPlayersPerRoom = 1 })
		_, w := join(t, r, "Alice")

		for _, tt := range []struct {
			name string
			join protocol.Join
			want error
		}{
			{"seats full", playerJoin("Bob", ""), ErrFull},
			{"unknown session", playerJoin("", "not-a-token"), ErrUnknownSession},
			{"wrong moderator token", protocol.Join{ProtocolVersion: 1, Role: protocol.RoleModerator, Token: w.SessionToken}, ErrUnauthorized},
			{"bad name", playerJoin("\x00", w.SessionToken), protocol.ErrInvalidName},
		} {
			if err := r.Join(r.NewClient(), tt.join); !errors.Is(err, tt.want) {
				t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
			}
		}
		joinAs(t, r, playerJoin("", w.SessionToken)) // a full room still lets seats reconnect
	})
}

func TestRolesGateGameMessages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := &recordingGame{}
		r := newTestRoom(t, withGame(g))
		p, _ := join(t, r, "Alice")
		m := joinModerator(t, r)
		s := stateUntil(t, p, func(s protocol.RoomState) bool { return s.ModeratorConnected })
		if names(s) != "Alice" {
			t.Errorf("moderator should not be listed as a player: %q", names(s))
		}
		recv(t, m) // state

		for i, tt := range []struct {
			name string
			from *Client
			msg  any
		}{
			{"player cannot rule", p, protocol.Rule{BuzzID: "b1", Result: protocol.RulingCorrect}},
			{"player cannot advance", p, protocol.Next{TossupID: "t1"}},
			{"moderator cannot buzz", m, protocol.Buzz{TossupID: "t1"}},
		} {
			seq := uint64(i + 1)
			send(t, r, tt.from, seq, tt.msg)
			if e := recvAs[protocol.ErrorMessage](t, tt.from, protocol.MsgError); e.Code != protocol.ErrCodeForbidden || e.Seq != seq {
				t.Errorf("%s: error = %+v", tt.name, e)
			}
		}
		synctest.Wait()
		if cmds := g.commands(); len(cmds) != 0 {
			t.Errorf("forbidden messages reached the game: %+v", cmds)
		}

		send(t, r, p, 10, protocol.Buzz{TossupID: "t1"})
		send(t, r, m, 10, protocol.Next{TossupID: "t1"})
		synctest.Wait()
		cmds := g.commands()
		if len(cmds) != 2 || cmds[0].Role != protocol.RolePlayer || cmds[1].Role != protocol.RoleModerator || cmds[1].PlayerID != "mod" {
			t.Errorf("game saw %+v", cmds)
		}
	})
}

func TestDefaultGameRejectsAsNotImplemented(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRoom(t, nil)
		p, _ := join(t, r, "Alice")
		recv(t, p)
		send(t, r, p, 1, protocol.Buzz{TossupID: "t1"})
		if e := recvAs[protocol.ErrorMessage](t, p, protocol.MsgError); e.Code != protocol.ErrCodeNotImplemented || e.Seq != 1 {
			t.Errorf("error = %+v", e)
		}
	})
}

func TestGameResultIsApplied(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := &recordingGame{respond: func(cmd Command) Result {
			b := cmd.Msg.(protocol.Buzz)
			if b.TossupID == "stale" {
				return Result{Reject: &protocol.ErrorMessage{Code: protocol.ErrCodeStale, Message: "not current"}}
			}
			return Result{Changed: true, Effects: []protocol.Effect{
				protocol.BuzzLocked{TossupID: b.TossupID, BuzzID: "b1", PlayerID: cmd.PlayerID},
			}}
		}}
		r := newTestRoom(t, withGame(g))
		a, wa := join(t, r, "Alice")
		b, _ := join(t, r, "Bob")
		stateUntil(t, a, namesAre("Alice,Bob"))
		stateUntil(t, b, namesAre("Alice,Bob"))

		send(t, r, a, 1, protocol.Buzz{TossupID: "stale"})
		if e := recvAs[protocol.ErrorMessage](t, a, protocol.MsgError); e.Code != protocol.ErrCodeStale || e.Seq != 1 {
			t.Errorf("reject = %+v", e)
		}
		expectNothing(t, b)

		send(t, r, a, 2, protocol.Buzz{TossupID: "t1"})
		for _, c := range []*Client{a, b} {
			e := recvAs[protocol.BuzzLocked](t, c, protocol.MsgBuzzLocked)
			s := recvAs[protocol.RoomState](t, c, protocol.MsgState)
			if e.PlayerID != wa.PlayerID || e.Version != s.Version || e.Version == 0 {
				t.Errorf("effect %+v, state version %d", e, s.Version)
			}
		}
	})
}

func TestErrorsGoOnlyToSender(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		expectNothing(t, b)
	})
}

func TestSlowReaderGetsOnlyTheLatestState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRoom(t, func(c *Config) { c.SendBuffer = 4 })
		slow := r.NewClient() // never reads until the end
		if err := r.Join(slow, playerJoin("Slow", "")); err != nil {
			t.Fatal(err)
		}
		fast, _ := join(t, r, "Fast")

		for i := range 100 {
			send(t, r, fast, uint64(i+1), protocol.SetName{Name: fmt.Sprint("Fast", i)})
		}
		synctest.Wait()

		recvAs[protocol.Welcome](t, slow, protocol.MsgWelcome)
		if s := recvAs[protocol.RoomState](t, slow, protocol.MsgState); names(s) != "Slow,Fast99" {
			t.Errorf("slow reader got %q, want the latest state", names(s))
		}
		expectNothing(t, slow)
	})
}

func TestQueueOverflowDropsClientWithoutBlockingOthers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newTestRoom(t, func(c *Config) { c.SendBuffer = 4 })
		slow := r.NewClient()
		if err := r.Join(slow, playerJoin("Slow", "")); err != nil {
			t.Fatal(err)
		}
		fast, _ := join(t, r, "Fast")
		stateUntil(t, fast, namesAre("Slow,Fast"))

		// Each bad message queues an error for its sender; Slow never reads.
		for i := range 10 {
			if err := r.Deliver(slow, Incoming{Seq: uint64(i + 1), Err: errors.New("bad")}); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait() // reading earlier would drain the queue it is overflowing
		if got := waitClosed(t, slow); got != ReasonSlow {
			t.Errorf("slow closed with %v, want %v", got, ReasonSlow)
		}
		r.Leave(slow) // must not double-close
		stateUntil(t, fast, namesAre("Slow(away),Fast"))
	})
}

func TestLeaveKeepsSeatAndIsIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestIdleTimeoutIsExact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := newTestHub(t, nil)
		idle := h.cfg.IdleTimeout
		r, err := h.Create()
		if err != nil {
			t.Fatal(err)
		}
		a, _ := join(t, r, "Alice")

		time.Sleep(3 * idle) // occupied rooms never idle out
		synctest.Wait()
		if isDone(r) {
			t.Fatal("room closed while occupied")
		}

		r.Leave(a) // the seat remains, but nobody is connected
		synctest.Wait()
		time.Sleep(idle - time.Nanosecond)
		synctest.Wait()
		if isDone(r) {
			t.Fatal("room closed before the idle timeout")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if !isDone(r) {
			t.Fatal("room still open at the idle timeout")
		}
		if _, ok := h.Get(r.Code()); ok {
			t.Error("closed room still registered in hub")
		}
		if err := r.Join(r.NewClient(), playerJoin("Late", "")); !errors.Is(err, ErrClosed) {
			t.Errorf("join after close: err = %v, want ErrClosed", err)
		}
	})
}

func TestNeverJoinedRoomIdlesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := newTestHub(t, nil)
		r, _ := h.Create()
		time.Sleep(h.cfg.IdleTimeout)
		synctest.Wait()
		if !isDone(r) {
			t.Fatal("unused room never shut down")
		}
	})
}

func isDone(r *Room) bool {
	select {
	case <-r.Done():
		return true
	default:
		return false
	}
}

func TestShutdownClosesClientsAndStopsCreation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

func TestHubLimitsCodesAndTokens(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

// 100 clients buzz concurrently, each sending every buzz twice (a resend).
// The room must hand each buzz to the game exactly once, keep each client's
// buzzes in send order with their ReceivedAt intact, never drop a client, and
// give every observer strictly increasing states that end on the same one.
func TestHundredConcurrentBuzzers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const clients, buzzes = 100, 10
		g := &recordingGame{}
		r := newTestRoom(t, func(c *Config) {
			c.MaxPlayersPerRoom = clients
			withGame(g)(c)
		})
		base := time.Now()
		stamp := func(i, j int) time.Time { return base.Add(time.Duration(i*buzzes+j) * time.Microsecond) }

		type observer struct {
			id       string
			versions []uint64
			last     []byte
			closed   bool
		}
		obs := make([]*observer, clients)
		var wg sync.WaitGroup
		for i := range clients {
			obs[i] = &observer{}
			wg.Go(func() {
				o := obs[i]
				c := r.NewClient()
				if err := r.Join(c, playerJoin(fmt.Sprint("P", i), "")); err != nil {
					t.Error(err)
					return
				}
				go func() { // drains c like a connection's writer would
					for {
						b, err := c.Next(context.Background())
						if err != nil {
							o.closed = true
							return
						}
						var env protocol.Envelope
						_ = json.Unmarshal(b, &env)
						switch env.Type {
						case protocol.MsgWelcome:
							var w protocol.Welcome
							_ = json.Unmarshal(env.Payload, &w)
							o.id = w.PlayerID
						case protocol.MsgState:
							var s protocol.RoomState
							_ = json.Unmarshal(env.Payload, &s)
							o.versions = append(o.versions, s.Version)
							o.last = b
						}
					}
				}()
				for j := range buzzes {
					in := Incoming{Seq: uint64(j + 1), Msg: protocol.Buzz{TossupID: fmt.Sprint("t", j)}, ReceivedAt: stamp(i, j)}
					for range 2 {
						if err := r.Deliver(c, in); err != nil {
							t.Error(err)
						}
					}
				}
			})
		}
		wg.Wait()
		synctest.Wait() // the room and every drainer are idle: all processed and delivered

		cmds := g.commands()
		if len(cmds) != clients*buzzes {
			t.Fatalf("game saw %d buzzes, want %d", len(cmds), clients*buzzes)
		}
		index := map[string]int{}
		for i, o := range obs {
			index[o.id] = i
		}
		nextJ := make([]int, clients)
		for _, cmd := range cmds {
			i := index[cmd.PlayerID]
			j := nextJ[i]
			if b := cmd.Msg.(protocol.Buzz); b.TossupID != fmt.Sprint("t", j) || !cmd.ReceivedAt.Equal(stamp(i, j)) {
				t.Fatalf("player %d: got %s at %v, want t%d at %v", i, b.TossupID, cmd.ReceivedAt, j, stamp(i, j))
			}
			nextJ[i]++
		}

		final := obs[0].last
		for i, o := range obs {
			if o.closed {
				t.Errorf("observer %d was dropped", i)
			}
			for k := 1; k < len(o.versions); k++ {
				if o.versions[k] <= o.versions[k-1] {
					t.Fatalf("observer %d: version %d after %d", i, o.versions[k], o.versions[k-1])
				}
			}
			if string(o.last) != string(final) {
				t.Errorf("observer %d ended on a different state", i)
			}
		}
	})
}
