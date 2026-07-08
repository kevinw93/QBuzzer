package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

const timeout = 2 * time.Second

type env struct {
	ts     *httptest.Server
	hub    *room.Hub
	cancel context.CancelFunc
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.DiscardHandler)
	hub := room.NewHub(ctx, room.DefaultConfig(), log)
	assets := fstest.MapFS{"index.html": {Data: []byte("<h1>QBuzzer</h1>")}}
	ts := httptest.NewServer(New(hub, assets, log))
	t.Cleanup(func() {
		cancel()
		hub.Wait()
		ts.Close()
	})
	return &env{ts: ts, hub: hub, cancel: cancel}
}

func (e *env) createRoom(t *testing.T) string {
	t.Helper()
	resp, err := http.Post(e.ts.URL+"/api/rooms", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body protocol.CreateRoomResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Code
}

func (e *env) wsURL(code, name string) string {
	return "ws" + strings.TrimPrefix(e.ts.URL, "http") +
		"/api/rooms/" + code + "/ws?name=" + url.QueryEscape(name)
}

func (e *env) dial(t *testing.T, code, name string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, e.wsURL(code, name), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
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

func stateUntil(t *testing.T, conn *websocket.Conn, ok func(protocol.RoomState) bool) protocol.RoomState {
	t.Helper()
	for {
		m, err := read(t, conn)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Type != protocol.MsgState {
			continue
		}
		var s protocol.RoomState
		if err := json.Unmarshal(m.Data, &s); err != nil {
			t.Fatal(err)
		}
		if ok(s) {
			return s
		}
	}
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

func playerCount(n int) func(protocol.RoomState) bool {
	return func(s protocol.RoomState) bool { return len(s.Players) == n }
}

func send(t *testing.T, conn *websocket.Conn, typ string, v any) {
	t.Helper()
	b, err := protocol.Encode(typ, v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRoomReturnsCode(t *testing.T) {
	e := newEnv(t)
	code := e.createRoom(t)
	if len(code) != 6 {
		t.Errorf("code = %q", code)
	}
	if _, ok := e.hub.Get(code); !ok {
		t.Error("room not registered")
	}
}

func TestJoinBroadcastAndLeave(t *testing.T) {
	e := newEnv(t)
	code := e.createRoom(t)
	alice := e.dial(t, code, "Alice")

	m, err := read(t, alice)
	if err != nil || m.Type != protocol.MsgWelcome {
		t.Fatalf("first message = %+v, %v; want welcome", m, err)
	}

	bob := e.dial(t, strings.ToLower(code), "Bob") // codes are case-insensitive
	stateUntil(t, alice, playerCount(2))
	stateUntil(t, bob, playerCount(2))

	send(t, alice, protocol.MsgSetName, protocol.SetName{Name: "Alicia"})
	stateUntil(t, bob, func(s protocol.RoomState) bool {
		return len(s.Players) == 2 && s.Players[0].Name == "Alicia"
	})

	if err := bob.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatal(err)
	}
	s := stateUntil(t, alice, playerCount(1))
	if s.Players[0].Name != "Alicia" {
		t.Errorf("remaining player = %+v", s.Players[0])
	}
}

func TestJoinRejectedBeforeUpgrade(t *testing.T) {
	e := newEnv(t)
	code := e.createRoom(t)
	tests := []struct {
		name, url string
		want      int
	}{
		{"unknown room", e.wsURL("ZZZZZZ", "Alice"), http.StatusNotFound},
		{"missing name", e.wsURL(code, ""), http.StatusBadRequest},
		{"blank name", e.wsURL(code, "   "), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			conn, resp, err := websocket.Dial(ctx, tt.url, nil)
			if err == nil {
				conn.CloseNow()
				t.Fatal("dial succeeded")
			}
			if resp == nil || resp.StatusCode != tt.want {
				t.Errorf("resp = %v, want status %d", resp, tt.want)
			}
		})
	}
}

func TestCrossOriginWebSocketRejected(t *testing.T) {
	e := newEnv(t)
	code := e.createRoom(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, e.wsURL(code, "Mallory"), &websocket.DialOptions{
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
		{"oversized", websocket.MessageText, []byte(`{"type":"set_name","data":"` + strings.Repeat("x", 2*maxMessageBytes) + `"}`), websocket.StatusMessageTooBig},
		{"not json", websocket.MessageText, []byte("hello"), websocket.StatusInvalidFramePayloadData},
		{"missing type", websocket.MessageText, []byte(`{"data":{}}`), websocket.StatusInvalidFramePayloadData},
		{"binary", websocket.MessageBinary, []byte{1, 2, 3}, websocket.StatusUnsupportedData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			conn := e.dial(t, e.createRoom(t), "Alice")
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			if err := conn.Write(ctx, tt.typ, tt.msg); err != nil {
				t.Fatal(err)
			}
			if got := closeStatusOf(t, conn); got != tt.want {
				t.Errorf("close status = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoomErrorDoesNotDisconnect(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t, e.createRoom(t), "Alice")
	send(t, conn, "no_such_type", struct{}{})
	for {
		m, err := read(t, conn)
		if err != nil {
			t.Fatalf("connection dropped: %v", err)
		}
		if m.Type == protocol.MsgError {
			break
		}
	}
	send(t, conn, protocol.MsgSetName, protocol.SetName{Name: "Still here"})
	stateUntil(t, conn, func(s protocol.RoomState) bool {
		return len(s.Players) == 1 && s.Players[0].Name == "Still here"
	})
}

func TestShutdownSendsGoingAway(t *testing.T) {
	e := newEnv(t)
	conn := e.dial(t, e.createRoom(t), "Alice")
	e.cancel()
	if got := closeStatusOf(t, conn); got != websocket.StatusGoingAway {
		t.Errorf("close status = %v, want going away", got)
	}
}

func TestServesFrontendAndHealth(t *testing.T) {
	e := newEnv(t)
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
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.DiscardHandler)
	cfg := room.DefaultConfig()
	cfg.MaxRooms = 1
	hub := room.NewHub(ctx, cfg, log)
	ts := httptest.NewServer(New(hub, fstest.MapFS{}, log))
	t.Cleanup(func() { cancel(); hub.Wait(); ts.Close() })

	for i, want := range []int{http.StatusCreated, http.StatusServiceUnavailable} {
		resp, err := http.Post(ts.URL+"/api/rooms", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("request %d: status %d, want %d", i, resp.StatusCode, want)
		}
	}
}
