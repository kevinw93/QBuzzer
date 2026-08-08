package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

const (
	maxMessageBytes    = 4096
	writeTimeout       = 5 * time.Second
	pingInterval       = 20 * time.Second // detects half-open connections
	pingTimeout        = 10 * time.Second
	defaultJoinTimeout = 5 * time.Second
)

// joinRoom runs a reader (this goroutine), a writer, a pinger, and a
// keepalive per connection. Only these goroutines touch the socket; only the room touches
// room state.
func (s *Server) joinRoom(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.hub.Get(r.PathValue("code"))
	if !ok {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, nil) // nil options: cross-origin requests are rejected
	if err != nil {
		return // Accept has written the response
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxMessageBytes)

	join, ok := s.readJoin(conn)
	if !ok {
		return
	}
	c := rm.NewClient()
	if err := rm.Join(c, join); err != nil {
		conn.Close(joinErrStatus(err), err.Error())
		return
	}
	s.log.Info("client joined", "room", rm.Code(), "role", join.Role)

	ctx, cancel := context.WithCancel(context.Background())
	cs := clock.NewSession(clock.DefaultConfig())
	started := make(chan struct{}) // closed after the first write, which is always the Welcome
	var wg sync.WaitGroup
	wg.Go(func() { writeLoop(ctx, conn, c, started) })
	wg.Go(func() { s.pinger(ctx, conn, cs, started) })
	wg.Go(func() { keepalive(ctx, conn) })

	s.readLoop(ctx, conn, rm, c, cs)

	rm.Leave(c)
	cancel() // stops the writer if the room exited and will never close c
	wg.Wait()
}

// readJoin reads the mandatory first message. On failure it has already
// closed conn and returns false.
func (s *Server) readJoin(conn *websocket.Conn) (protocol.Join, bool) {
	fail := func(code protocol.CloseCode, reason string) (protocol.Join, bool) {
		conn.Close(websocket.StatusCode(code), reason)
		return protocol.Join{}, false
	}

	// A read-context deadline would drop the TCP connection without a close
	// frame; closing from a timer sends a proper close code instead.
	timer := time.AfterFunc(s.joinTimeout, func() {
		conn.Close(websocket.StatusCode(protocol.CloseCodeJoinRequired), "join timeout")
	})
	typ, data, err := conn.Read(context.Background())
	if !timer.Stop() || err != nil {
		return protocol.Join{}, false
	}

	var env protocol.Envelope
	if typ != websocket.MessageText || json.Unmarshal(data, &env) != nil || env.Type != protocol.MsgJoin {
		return fail(protocol.CloseCodeJoinRequired, "first message must be join")
	}
	// Checked before full decoding: a stale client's join may not match the
	// current Join shape, and it needs to be told to reload, not that it is
	// malformed.
	var v struct {
		ProtocolVersion int `json:"protocolVersion"`
	}
	if json.Unmarshal(env.Payload, &v) != nil || v.ProtocolVersion != protocol.ProtocolVersion {
		return fail(protocol.CloseCodeReloadRequired, "protocol version mismatch; reload")
	}
	msg, err := protocol.DecodeClient(env)
	if err != nil {
		return fail(protocol.CloseCodeJoinRequired, truncate(err.Error()))
	}
	return msg.(protocol.Join), true
}

func (s *Server) readLoop(ctx context.Context, conn *websocket.Conn, rm *room.Room, c *room.Client, cs *clock.Session) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// Taken before the event queues in the room inbox, so room backlog
		// does not shift buzz timestamps.
		receivedAt := time.Now()

		if typ != websocket.MessageText {
			conn.Close(websocket.StatusUnsupportedData, "text messages only")
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.Type == "" {
			conn.Close(websocket.StatusInvalidFramePayloadData, "malformed envelope")
			return
		}
		msg, err := protocol.DecodeClient(env)
		if p, ok := msg.(protocol.Pong); ok {
			now := s.clk.At(receivedAt)
			if cs.Pong(p.PingID, p.ClientRecvMs, p.ClientSendMs, now) == nil { // bad pongs are just discarded samples
				if est, ok := cs.Estimate(now); ok {
					rm.UpdateClock(c, est, receivedAt)
				}
			}
			continue
		}
		now := s.clk.At(receivedAt)
		est, estOK := cs.Estimate(now)
		in := room.Incoming{Seq: env.Seq, Msg: msg, Err: err, ReceivedAt: receivedAt, Clock: est, ClockOK: estOK, ServerMs: now}
		if err := rm.Deliver(c, in); err != nil {
			return
		}
	}
}

func writeLoop(ctx context.Context, conn *websocket.Conn, c *room.Client, started chan<- struct{}) {
	first := true
	for {
		msg, err := c.Next(ctx)
		if errors.Is(err, room.ErrClientClosed) {
			status, reason := closeStatus(c.CloseReason())
			conn.Close(status, reason)
			return
		}
		if err != nil {
			return
		}
		wctx, cancel := context.WithTimeout(ctx, writeTimeout)
		err = conn.Write(wctx, websocket.MessageText, msg)
		cancel()
		if err != nil {
			conn.CloseNow() // unblocks the reader
			return
		}
		if first {
			close(started)
			first = false
		}
	}
}

// pinger runs the clock-sync exchange. T1 is stamped immediately before the
// write; if the write waits behind another, that sample just has a larger RTT
// and loses to better ones.
func (s *Server) pinger(ctx context.Context, conn *websocket.Conn, cs *clock.Session, started <-chan struct{}) {
	select {
	case <-started:
	case <-ctx.Done():
		return
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if id, ok := cs.Ping(s.clk.Now()); ok {
			msg, err := protocol.Encode(protocol.MsgPing, protocol.Ping{ID: id})
			if err != nil {
				panic(err)
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err = conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				conn.CloseNow()
				return
			}
		}
		t.Reset(time.Duration(cs.NextInterval() * float64(time.Millisecond)))
	}
}

func keepalive(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pctx) // the pong is consumed by the concurrent Read
			cancel()
			if err != nil {
				conn.CloseNow()
				return
			}
		}
	}
}

func closeStatus(r room.CloseReason) (websocket.StatusCode, string) {
	switch r {
	case room.ReasonSlow:
		return websocket.StatusCode(protocol.CloseCodeTooSlow), "connection too slow; reconnect"
	case room.ReasonReplaced:
		return websocket.StatusCode(protocol.CloseCodeReplaced), "session opened elsewhere"
	case room.ReasonRoomClosed:
		return websocket.StatusGoingAway, "room closed"
	default:
		return websocket.StatusNormalClosure, ""
	}
}

func joinErrStatus(err error) websocket.StatusCode {
	switch {
	case errors.Is(err, room.ErrFull):
		return websocket.StatusTryAgainLater
	case errors.Is(err, room.ErrClosed):
		return websocket.StatusGoingAway
	case errors.Is(err, room.ErrUnauthorized):
		return websocket.StatusCode(protocol.CloseCodeUnauthorized)
	case errors.Is(err, room.ErrRateLimited):
		return websocket.StatusCode(protocol.CloseCodeRateLimited)
	case errors.Is(err, room.ErrUnknownSession):
		return websocket.StatusCode(protocol.CloseCodeUnknownSession)
	default:
		return websocket.StatusCode(protocol.CloseCodeJoinRequired)
	}
}

// truncate keeps a close reason within the 123-byte limit of a close frame.
func truncate(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120]
}
