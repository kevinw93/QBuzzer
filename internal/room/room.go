package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

var (
	ErrClosed = errors.New("room closed")
	ErrFull   = errors.New("room full")
)

type (
	joinEvent struct {
		c     *Client
		name  string
		reply chan error // buffered so the room never blocks replying
	}
	leaveEvent struct {
		c *Client
	}
	messageEvent struct {
		c   *Client
		env protocol.Envelope
		// receivedAt is taken by the connection reader, before the event waits
		// in the inbox.
		receivedAt time.Time
	}
)

type member struct {
	client *Client
	id     string
	name   string
}

// Room is one live game. All fields below inbox/done are owned by the run
// goroutine; other goroutines interact only by sending events to inbox, which
// gives every event in the room a single total order.
type Room struct {
	code  string
	cfg   Config
	inbox chan any
	done  chan struct{} // closed when run returns

	members  []*member // join order
	byClient map[*Client]*member
	nextID   int
	version  uint64
}

func newRoom(code string, cfg Config) *Room {
	return &Room{
		code:     code,
		cfg:      cfg,
		inbox:    make(chan any, 64),
		done:     make(chan struct{}),
		byClient: make(map[*Client]*member),
	}
}

func (r *Room) Code() string { return r.code }

// Done is closed once the room has shut down.
func (r *Room) Done() <-chan struct{} { return r.done }

func (r *Room) NewClient() *Client {
	return &Client{send: make(chan []byte, r.cfg.SendBuffer)}
}

// Join adds c to the room. On success a Welcome followed by a RoomState is
// already queued on c.Outbound().
func (r *Room) Join(c *Client, name string) error {
	reply := make(chan error, 1)
	if err := r.submit(joinEvent{c: c, name: name, reply: reply}); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-r.done:
		// If the room added c before exiting, it also closed c's channel.
		return ErrClosed
	}
}

// Leave is idempotent, and safe after the room has dropped c or shut down.
func (r *Room) Leave(c *Client) { _ = r.submit(leaveEvent{c: c}) }

func (r *Room) Deliver(c *Client, env protocol.Envelope, receivedAt time.Time) error {
	return r.submit(messageEvent{c: c, env: env, receivedAt: receivedAt})
}

func (r *Room) submit(ev any) error {
	select {
	case r.inbox <- ev:
		return nil
	case <-r.done:
		return ErrClosed
	}
}

// run returns when ctx is cancelled or the room has been empty for
// cfg.IdleTimeout. onExit runs before done is closed.
func (r *Room) run(ctx context.Context, onExit func()) {
	idle := time.NewTimer(r.cfg.IdleTimeout) // armed only while the room is empty
	defer idle.Stop()

	defer func() {
		for _, m := range r.members {
			closeClient(m.client, ReasonRoomClosed)
		}
		r.members, r.byClient = nil, nil
		onExit()
		close(r.done)
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			return
		case ev := <-r.inbox:
			wasEmpty := len(r.members) == 0
			r.handle(ev)
			switch isEmpty := len(r.members) == 0; {
			case wasEmpty && !isEmpty:
				idle.Stop() // relies on Go 1.23+: no stale tick is delivered after Stop
			case !wasEmpty && isEmpty:
				idle.Reset(r.cfg.IdleTimeout)
			}
		}
	}
}

func (r *Room) handle(ev any) {
	switch ev := ev.(type) {
	case joinEvent:
		r.handleJoin(ev)
	case leaveEvent:
		// Clients already dropped as slow are not in byClient, so their
		// channel is never closed twice.
		if m, ok := r.byClient[ev.c]; ok {
			r.remove(m, ReasonLeft)
			r.broadcastState()
		}
	case messageEvent:
		if m, ok := r.byClient[ev.c]; ok {
			r.handleMessage(m, ev)
		}
	default:
		panic(fmt.Sprintf("room: unknown event %T", ev))
	}
}

func (r *Room) handleJoin(ev joinEvent) {
	if len(r.members) >= r.cfg.MaxClientsPerRoom {
		ev.reply <- ErrFull
		return
	}
	name, err := protocol.NormalizeName(ev.name)
	if err != nil {
		ev.reply <- err
		return
	}
	r.nextID++
	m := &member{client: ev.c, id: "p" + strconv.Itoa(r.nextID), name: name}
	r.members = append(r.members, m)
	r.byClient[ev.c] = m
	ev.reply <- nil

	r.sendTo(m, encode(protocol.MsgWelcome, protocol.Welcome{PlayerID: m.id}))
	r.broadcastState()
}

func (r *Room) handleMessage(m *member, ev messageEvent) {
	switch ev.env.Type {
	case protocol.MsgSetName:
		var p protocol.SetName
		if err := json.Unmarshal(ev.env.Data, &p); err != nil {
			r.sendError(m, protocol.ErrCodeBadMessage, "invalid set_name payload")
			return
		}
		name, err := protocol.NormalizeName(p.Name)
		if err != nil {
			r.sendError(m, protocol.ErrCodeInvalidName, err.Error())
			return
		}
		if name != m.name {
			m.name = name
			r.broadcastState()
		}
	default:
		r.sendError(m, protocol.ErrCodeUnknownType, "unknown message type "+strconv.Quote(ev.env.Type))
	}
}

// broadcastState never blocks. Members whose buffers are full are dropped and
// the resulting state is broadcast again; each extra pass removes at least one
// member, so the loop terminates.
func (r *Room) broadcastState() {
	for {
		r.version++
		msg := encode(protocol.MsgState, r.snapshot())
		var slow []*member
		for _, m := range r.members {
			select {
			case m.client.send <- msg:
			default:
				slow = append(slow, m)
			}
		}
		if len(slow) == 0 {
			return
		}
		for _, m := range slow {
			r.remove(m, ReasonSlow)
		}
	}
}

func (r *Room) sendError(m *member, code, message string) {
	if !r.sendTo(m, encode(protocol.MsgError, protocol.ErrorMessage{Code: code, Message: message})) {
		r.broadcastState()
	}
}

// sendTo drops m if its buffer is full. On false the caller must broadcast.
func (r *Room) sendTo(m *member, msg []byte) bool {
	select {
	case m.client.send <- msg:
		return true
	default:
		r.remove(m, ReasonSlow)
		return false
	}
}

func (r *Room) remove(m *member, reason CloseReason) {
	delete(r.byClient, m.client)
	for i, x := range r.members {
		if x == m {
			r.members = append(r.members[:i], r.members[i+1:]...)
			break
		}
	}
	closeClient(m.client, reason)
}

func (r *Room) snapshot() protocol.RoomState {
	players := make([]protocol.PlayerView, 0, len(r.members)) // non-nil: encodes as []
	for _, m := range r.members {
		players = append(players, protocol.PlayerView{ID: m.id, Name: m.name})
	}
	return protocol.RoomState{Version: r.version, Code: r.code, Players: players}
}

// closeClient writes reason before close; Client.CloseReason depends on it.
func closeClient(c *Client, reason CloseReason) {
	c.reason = reason
	close(c.send)
}

// encode panics because marshaling protocol types cannot fail.
func encode(typ string, v any) []byte {
	b, err := protocol.Encode(typ, v)
	if err != nil {
		panic(fmt.Sprintf("room: encoding %s: %v", typ, err))
	}
	return b
}
