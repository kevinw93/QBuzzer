package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

const (
	maxMessageBytes = 4096
	writeTimeout    = 5 * time.Second
	pingInterval    = 20 * time.Second // detects half-open connections
	pingTimeout     = 10 * time.Second
)

// joinRoom runs a reader (this goroutine), a writer, and a keepalive per
// connection. Only these goroutines touch the socket; only the room touches
// room state.
func (s *Server) joinRoom(w http.ResponseWriter, r *http.Request) {
	rm, ok := s.hub.Get(r.PathValue("code"))
	if !ok {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	name, err := protocol.NormalizeName(r.URL.Query().Get("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, nil) // nil options: cross-origin requests are rejected
	if err != nil {
		return // Accept has written the response
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxMessageBytes)

	c := rm.NewClient()
	if err := rm.Join(c, name); err != nil {
		conn.Close(joinErrStatus(err), err.Error())
		return
	}
	s.log.Info("client joined", "room", rm.Code(), "name", name)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { writeLoop(ctx, conn, c) })
	wg.Go(func() { keepalive(ctx, conn) })

	readLoop(ctx, conn, rm, c)

	rm.Leave(c)
	cancel() // stops the writer if the room exited and will never close c
	wg.Wait()
}

func readLoop(ctx context.Context, conn *websocket.Conn, rm *room.Room, c *room.Client) {
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
		if err := rm.Deliver(c, env, receivedAt); err != nil {
			return
		}
	}
}

func writeLoop(ctx context.Context, conn *websocket.Conn, c *room.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.Outbound():
			if !ok {
				status, reason := closeStatus(c.CloseReason())
				conn.Close(status, reason)
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				conn.CloseNow() // unblocks the reader
				return
			}
		}
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
		return websocket.StatusPolicyViolation, "connection too slow; reconnect"
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
	default:
		return websocket.StatusPolicyViolation
	}
}
