package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

const timeout = 2 * time.Second

type env struct {
	ts     *httptest.Server
	hub    *room.Hub
	cancel context.CancelFunc
}

func newEnv(t *testing.T, mod func(*room.Config)) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.DiscardHandler)
	cfg := room.DefaultConfig()
	if mod != nil {
		mod(&cfg)
	}
	hub := room.NewHub(ctx, cfg, log)
	s := &Server{hub: hub, log: log, joinTimeout: 200 * time.Millisecond}
	ts := httptest.NewServer(s.handler(fstest.MapFS{"index.html": {Data: []byte("<h1>QBuzzer</h1>")}}))
	t.Cleanup(func() {
		cancel()
		hub.Wait()
		ts.Close()
	})
	return &env{ts: ts, hub: hub, cancel: cancel}
}

func (e *env) createRoom(t *testing.T) protocol.CreateRoomResponse {
	t.Helper()
	resp, err := http.Post(e.ts.URL+"/api/rooms", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	var body protocol.CreateRoomResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func (e *env) wsURL(code string) string {
	return "ws" + strings.TrimPrefix(e.ts.URL, "http") + "/api/rooms/" + code + "/ws"
}

func (e *env) dial(t *testing.T, code string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, e.wsURL(code), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

// join dials, sends j, and returns the connection and its Welcome.
func (e *env) join(t *testing.T, code string, j protocol.Join) (*websocket.Conn, protocol.Welcome) {
	t.Helper()
	conn := e.dial(t, code)
	send(t, conn, protocol.MsgJoin, 0, j)
	m, err := read(t, conn)
	if err != nil {
		t.Fatalf("join %+v: %v", j, err)
	}
	if m.Type != protocol.MsgWelcome {
		t.Fatalf("first message = %+v, want welcome", m)
	}
	var w protocol.Welcome
	if err := json.Unmarshal(m.Payload, &w); err != nil {
		t.Fatal(err)
	}
	return conn, w
}

func player(name, token string) protocol.Join {
	return protocol.Join{ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RolePlayer, Name: name, Token: token}
}

func read(t *testing.T, conn *websocket.Conn) (protocol.Envelope, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var m protocol.Envelope
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("bad envelope %s: %v", data, err)
	}
	return m, nil
}

func readUntil[T any](t *testing.T, conn *websocket.Conn, typ protocol.MsgType, ok func(T) bool) T {
	t.Helper()
	for {
		m, err := read(t, conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type != typ {
			continue
		}
		var v T
		if err := json.Unmarshal(m.Payload, &v); err != nil {
			t.Fatal(err)
		}
		if ok(v) {
			return v
		}
	}
}

func stateUntil(t *testing.T, conn *websocket.Conn, ok func(protocol.RoomState) bool) {
	t.Helper()
	readUntil(t, conn, protocol.MsgState, ok)
}

// closeStatusOf reads until the connection fails and returns the close code.
func closeStatusOf(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	for {
		if _, err := read(t, conn); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func send(t *testing.T, conn *websocket.Conn, typ protocol.MsgType, seq uint64, v any) {
	t.Helper()
	b, err := protocol.EncodeClient(typ, seq, v)
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, conn, websocket.MessageText, b)
}

func writeRaw(t *testing.T, conn *websocket.Conn, typ websocket.MessageType, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := conn.Write(ctx, typ, b); err != nil {
		t.Fatal(err)
	}
}

func players(s protocol.RoomState) string {
	var ps []string
	for _, p := range s.Players {
		n := p.Name
		if !p.Connected {
			n += "(away)"
		}
		ps = append(ps, n)
	}
	return strings.Join(ps, ",")
}

func playersAre(want string) func(protocol.RoomState) bool {
	return func(s protocol.RoomState) bool { return players(s) == want }
}

func TestCreateRoomReturnsCodeAndModeratorToken(t *testing.T) {
	e := newEnv(t, nil)
	r := e.createRoom(t)
	if len(r.Code) != 6 || len(r.ModeratorToken) < 26 {
		t.Errorf("response = %+v", r)
	}
	if rm, ok := e.hub.Get(r.Code); !ok || rm.ModeratorToken() != r.ModeratorToken {
		t.Error("room not registered with the returned token")
	}
}

func TestJoinBroadcastAndLeave(t *testing.T) {
	e := newEnv(t, nil)
	code := e.createRoom(t).Code
	alice, _ := e.join(t, code, player("Alice", ""))
	bob, _ := e.join(t, strings.ToLower(code), player("Bob", "")) // codes are case-insensitive
	stateUntil(t, alice, playersAre("Alice,Bob"))

	send(t, alice, protocol.MsgSetName, 1, protocol.SetName{Name: "Alicia"})
	stateUntil(t, bob, playersAre("Alicia,Bob"))

	if err := bob.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatal(err)
	}
	stateUntil(t, alice, playersAre("Alicia,Bob(away)"))
}

func TestResumeAcrossConnections(t *testing.T) {
	e := newEnv(t, nil)
	code := e.createRoom(t).Code
	watcher, _ := e.join(t, code, player("Watcher", ""))
	a1, w1 := e.join(t, code, player("Alice", ""))
	send(t, a1, protocol.MsgSetName, 1, protocol.SetName{Name: "Alicia"})
	stateUntil(t, watcher, playersAre("Watcher,Alicia"))

	a1.CloseNow()
	stateUntil(t, watcher, playersAre("Watcher,Alicia(away)"))

	_, w2 := e.join(t, code, player("", w1.SessionToken))
	if w2.PlayerID != w1.PlayerID || w2.LastSeq != 1 {
		t.Errorf("resumed welcome = %+v, original %+v", w2, w1)
	}
	stateUntil(t, watcher, playersAre("Watcher,Alicia"))
}

func TestSecondConnectionReplacesFirst(t *testing.T) {
	e := newEnv(t, nil)
	code := e.createRoom(t).Code
	first, w := e.join(t, code, player("Alice", ""))
	second, _ := e.join(t, code, player("", w.SessionToken))

	if got := closeStatusOf(t, first); got != websocket.StatusCode(protocol.CloseCodeReplaced) {
		t.Errorf("first connection closed with %v, want replaced", got)
	}
	send(t, second, protocol.MsgSetName, 1, protocol.SetName{Name: "Still here"})
	stateUntil(t, second, playersAre("Still here"))
}

func TestModeratorJoinAndPermissions(t *testing.T) {
	e := newEnv(t, nil)
	r := e.createRoom(t)
	p, _ := e.join(t, r.Code, player("Alice", ""))
	m, w := e.join(t, r.Code, protocol.Join{
		ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RoleModerator, Token: r.ModeratorToken,
	})
	if w.Role != protocol.RoleModerator {
		t.Errorf("welcome = %+v", w)
	}
	stateUntil(t, p, func(s protocol.RoomState) bool { return s.ModeratorConnected })

	send(t, p, protocol.MsgRule, 1, protocol.Rule{BuzzID: "b1", Result: protocol.RulingCorrect})
	if e := readUntil(t, p, protocol.MsgError, func(protocol.ErrorMessage) bool { return true }); e.Code != protocol.ErrCodeForbidden || e.Seq != 1 {
		t.Errorf("player rule: error = %+v", e)
	}
	send(t, m, protocol.MsgNext, 1, protocol.Next{TossupID: "t1"})
	if e := readUntil(t, m, protocol.MsgError, func(protocol.ErrorMessage) bool { return true }); e.Code != protocol.ErrCodeNotImplemented {
		t.Errorf("moderator next: error = %+v", e)
	}
}

func TestJoinHandshakeFailures(t *testing.T) {
	e := newEnv(t, nil)
	r := e.createRoom(t)
	_, w := e.join(t, r.Code, player("Alice", ""))

	join := func(j protocol.Join) []byte {
		b, err := protocol.EncodeClient(protocol.MsgJoin, 0, j)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name  string
		typ   websocket.MessageType
		first []byte // nil: send nothing
		want  protocol.CloseCode
	}{
		{"silence", 0, nil, protocol.CloseCodeJoinRequired},
		{"other message first", websocket.MessageText, []byte(`{"type":"set_name","seq":1,"payload":{"name":"A"}}`), protocol.CloseCodeJoinRequired},
		{"binary first", websocket.MessageBinary, []byte{1}, protocol.CloseCodeJoinRequired},
		{"not json", websocket.MessageText, []byte("hello"), protocol.CloseCodeJoinRequired},
		{"old protocol version", websocket.MessageText, join(protocol.Join{ProtocolVersion: 0, Role: protocol.RolePlayer, Name: "A"}), protocol.CloseCodeReloadRequired},
		{"stale join shape", websocket.MessageText, []byte(`{"type":"join","payload":{"protocolVersion":0,"name":["A"]}}`), protocol.CloseCodeReloadRequired},
		{"invalid join", websocket.MessageText, join(protocol.Join{ProtocolVersion: protocol.ProtocolVersion, Role: "admin", Name: "A"}), protocol.CloseCodeJoinRequired},
		{"wrong moderator token", websocket.MessageText, join(protocol.Join{ProtocolVersion: protocol.ProtocolVersion, Role: protocol.RoleModerator, Token: w.SessionToken}), protocol.CloseCodeUnauthorized},
		{"unknown session", websocket.MessageText, join(player("", "nope")), protocol.CloseCodeUnknownSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := e.dial(t, r.Code)
			if tt.first != nil {
				writeRaw(t, conn, tt.typ, tt.first)
			}
			if got := closeStatusOf(t, conn); got != websocket.StatusCode(tt.want) {
				t.Errorf("close status = %v, want %d", got, tt.want)
			}
		})
	}
}

func TestRoomFullClosesWithTryAgainLater(t *testing.T) {
	e := newEnv(t, func(c *room.Config) { c.MaxPlayersPerRoom = 1 })
	code := e.createRoom(t).Code
	e.join(t, code, player("Alice", ""))
	conn := e.dial(t, code)
	send(t, conn, protocol.MsgJoin, 0, player("Bob", ""))
	if got := closeStatusOf(t, conn); got != websocket.StatusTryAgainLater {
		t.Errorf("close status = %v, want try again later", got)
	}
}

func TestJoinUnknownRoomIs404(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, e.wsURL("ZZZZZZ"), nil)
	if err == nil {
		conn.CloseNow()
		t.Fatal("dial succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("resp = %v, want 404", resp)
	}
}

func TestCrossOriginWebSocketRejected(t *testing.T) {
	e := newEnv(t, nil)
	code := e.createRoom(t).Code
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, e.wsURL(code), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		conn.CloseNow()
		t.Fatal("cross-origin dial succeeded")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("resp = %v, want 403", resp)
	}
}

func TestProtocolViolationsCloseConnection(t *testing.T) {
	tests := []struct {
		name string
		typ  websocket.MessageType
		msg  []byte
		want websocket.StatusCode
	}{
		{"oversized", websocket.MessageText, []byte(`{"type":"set_name","seq":1,"payload":"` + strings.Repeat("x", 2*maxMessageBytes) + `"}`), websocket.StatusMessageTooBig},
		{"not json", websocket.MessageText, []byte("hello"), websocket.StatusInvalidFramePayloadData},
		{"missing type", websocket.MessageText, []byte(`{"seq":1,"payload":{}}`), websocket.StatusInvalidFramePayloadData},
		{"binary", websocket.MessageBinary, []byte{1, 2, 3}, websocket.StatusUnsupportedData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			conn, _ := e.join(t, e.createRoom(t).Code, player("Alice", ""))
			writeRaw(t, conn, tt.typ, tt.msg)
			if got := closeStatusOf(t, conn); got != tt.want {
				t.Errorf("close status = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInvalidPayloadGetsErrorAndStaysConnected(t *testing.T) {
	e := newEnv(t, nil)
	conn, _ := e.join(t, e.createRoom(t).Code, player("Alice", ""))
	writeRaw(t, conn, websocket.MessageText, []byte(`{"type":"buzz","seq":1,"payload":{"tossupId":""}}`))
	if got := readUntil(t, conn, protocol.MsgError, func(protocol.ErrorMessage) bool { return true }); got.Code != protocol.ErrCodeBadMessage || got.Seq != 1 {
		t.Errorf("error = %+v", got)
	}
	writeRaw(t, conn, websocket.MessageText, []byte(`{"type":"launch","seq":2,"payload":{}}`))
	if got := readUntil(t, conn, protocol.MsgError, func(protocol.ErrorMessage) bool { return true }); got.Seq != 2 {
		t.Errorf("error = %+v", got)
	}
	send(t, conn, protocol.MsgSetName, 3, protocol.SetName{Name: "Still here"})
	stateUntil(t, conn, playersAre("Still here"))
}

func TestShutdownSendsGoingAway(t *testing.T) {
	e := newEnv(t, nil)
	conn, _ := e.join(t, e.createRoom(t).Code, player("Alice", ""))
	e.cancel()
	if got := closeStatusOf(t, conn); got != websocket.StatusGoingAway {
		t.Errorf("close status = %v, want going away", got)
	}
}

func TestServesFrontendAndHealth(t *testing.T) {
	e := newEnv(t, nil)
	for path, want := range map[string]string{"/": "<h1>QBuzzer</h1>", "/healthz": "ok"} {
		resp, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != want {
			t.Errorf("GET %s = %d %q", path, resp.StatusCode, body)
		}
	}
}

func TestCreateRoomAtLimit(t *testing.T) {
	e := newEnv(t, func(c *room.Config) { c.MaxRooms = 1 })
	for i, want := range []int{http.StatusCreated, http.StatusServiceUnavailable} {
		resp, err := http.Post(e.ts.URL+"/api/rooms", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: status %d, want %d", i, resp.StatusCode, want)
		}
	}
}
