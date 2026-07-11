package room

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/kevinw93/QBuzzer/internal/protocol"
)

var (
	ErrClosed         = errors.New("room closed")
	ErrFull           = errors.New("room full")
	ErrUnauthorized   = errors.New("moderator token rejected")
	ErrUnknownSession = errors.New("unknown session")
)

// Incoming is one decoded client message. Exactly one of Msg and Err is set.
type Incoming struct {
	Seq        uint64
	Msg        any   // a protocol payload by value, e.g. protocol.Buzz
	Err        error // decode or validation failure
	ReceivedAt time.Time
}

type (
	joinEvent struct {
		c     *Client
		join  protocol.Join
		reply chan error // buffered so the room never blocks replying
	}
	leaveEvent struct {
		c *Client
	}
	messageEvent struct {
		c  *Client
		in Incoming
	}
)

// seat is a session: it outlives connections so a player can reconnect with
// the same identity and seq history.
type seat struct {
	id      string
	role    protocol.Role
	name    string
	token   string
	lastSeq uint64  // highest seq processed; lower or equal seqs are duplicates
	client  *Client // nil while disconnected
}

// Room is one live game. All fields below done are owned by the run
// goroutine; other goroutines interact only by sending events to inbox, which
// gives every event in the room a single total order.
type Room struct {
	code           string
	epoch          string
	moderatorToken string
	cfg            Config
	inbox          chan any
	done           chan struct{} // closed when run returns

	players   []*seat // join order
	moderator *seat   // nil until a moderator first joins
	byToken   map[string]*seat
	byClient  map[*Client]*seat
	nextID    int
	version   uint64
}

func newRoom(code, moderatorToken string, cfg Config) *Room {
	return &Room{
		code:           code,
		epoch:          rand.Text()[:8],
		moderatorToken: moderatorToken,
		cfg:            cfg,
		inbox:          make(chan any, 64),
		done:           make(chan struct{}),
		byToken:        make(map[string]*seat),
		byClient:       make(map[*Client]*seat),
	}
}

func (r *Room) Code() string { return r.code }

// ModeratorToken grants moderator rights in this room.
func (r *Room) ModeratorToken() string { return r.moderatorToken }

// Done is closed once the room has shut down.
func (r *Room) Done() <-chan struct{} { return r.done }

func (r *Room) NewClient() *Client {
	return &Client{send: make(chan []byte, r.cfg.SendBuffer)}
}

// Join attaches c to a new or existing seat. On success a Welcome followed
// by a RoomState is already queued on c.Outbound(). If the seat was connected
// elsewhere, that connection is closed with ReasonReplaced.
func (r *Room) Join(c *Client, j protocol.Join) error {
	reply := make(chan error, 1)
	if err := r.submit(joinEvent{c: c, join: j, reply: reply}); err != nil {
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

// Leave disconnects c, keeping its seat. It is idempotent, and safe after the
// room has dropped c or shut down.
func (r *Room) Leave(c *Client) { _ = r.submit(leaveEvent{c: c}) }

func (r *Room) Deliver(c *Client, in Incoming) error {
	return r.submit(messageEvent{c: c, in: in})
}

func (r *Room) submit(ev any) error {
	select {
	case r.inbox <- ev:
		return nil
	case <-r.done:
		return ErrClosed
	}
}

// run returns when ctx is cancelled or the room has had no connections for
// cfg.IdleTimeout. onExit runs before done is closed.
func (r *Room) run(ctx context.Context, onExit func()) {
	idle := time.NewTimer(r.cfg.IdleTimeout) // armed only while nobody is connected
	defer idle.Stop()

	defer func() {
		for c := range r.byClient {
			closeClient(c, ReasonRoomClosed)
		}
		r.byClient, r.byToken = nil, nil
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
			wasEmpty := len(r.byClient) == 0
			r.handle(ev)
			switch isEmpty := len(r.byClient) == 0; {
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
		// Clients already dropped (slow, replaced) are not in byClient, so
		// their channel is never closed twice.
		if s, ok := r.byClient[ev.c]; ok {
			r.disconnect(s, ReasonLeft)
			r.broadcastState()
		}
	case messageEvent:
		if s, ok := r.byClient[ev.c]; ok {
			r.handleMessage(s, ev.in)
		}
	default:
		panic(fmt.Sprintf("room: unknown event %T", ev))
	}
}

func (r *Room) handleJoin(ev joinEvent) {
	s, err := r.seatFor(ev.join)
	if err != nil {
		ev.reply <- err
		return
	}
	if s.client != nil {
		r.disconnect(s, ReasonReplaced)
	}
	s.client = ev.c
	r.byClient[ev.c] = s
	ev.reply <- nil

	r.sendTo(s, encode(protocol.MsgWelcome, protocol.Welcome{
		PlayerID: s.id, Role: s.role, SessionToken: s.token, LastSeq: s.lastSeq,
	}))
	r.broadcastState()
}

// seatFor finds or creates the seat for j. j has passed Join.Validate.
func (r *Room) seatFor(j protocol.Join) (*seat, error) {
	var name string
	if j.Name != "" {
		var err error
		if name, err = protocol.NormalizeName(j.Name); err != nil {
			return nil, err
		}
	}

	switch j.Role {
	case protocol.RoleModerator:
		if subtle.ConstantTimeCompare([]byte(j.Token), []byte(r.moderatorToken)) != 1 {
			return nil, ErrUnauthorized
		}
		if r.moderator == nil {
			r.moderator = &seat{id: "mod", role: protocol.RoleModerator, name: "Moderator", token: r.moderatorToken}
		}
		if name != "" {
			r.moderator.name = name
		}
		return r.moderator, nil

	case protocol.RolePlayer:
		if j.Token != "" {
			s, ok := r.byToken[j.Token]
			if !ok {
				return nil, ErrUnknownSession
			}
			if name != "" {
				s.name = name
			}
			return s, nil
		}
		if len(r.players) >= r.cfg.MaxPlayersPerRoom {
			return nil, ErrFull
		}
		r.nextID++
		s := &seat{id: "p" + strconv.Itoa(r.nextID), role: protocol.RolePlayer, name: name, token: rand.Text()}
		r.players = append(r.players, s)
		r.byToken[s.token] = s
		return s, nil
	}
	return nil, fmt.Errorf("unknown role %q", j.Role)
}

func (r *Room) handleMessage(s *seat, in Incoming) {
	if in.Err != nil {
		s.lastSeq = max(s.lastSeq, in.Seq)
		r.sendError(s, in.Seq, protocol.ErrCodeBadMessage, in.Err.Error())
		return
	}
	if _, ok := in.Msg.(protocol.Join); ok {
		r.sendError(s, 0, protocol.ErrCodeBadMessage, "already joined")
		return
	}
	if in.Seq <= s.lastSeq {
		return // already processed; resent after a reconnect
	}
	s.lastSeq = in.Seq

	switch m := in.Msg.(type) {
	case protocol.SetName:
		name, err := protocol.NormalizeName(m.Name)
		if err != nil {
			r.sendError(s, in.Seq, protocol.ErrCodeInvalidName, err.Error())
			return
		}
		if name != s.name {
			s.name = name
			r.broadcastState()
		}
	case protocol.Pong:
		// Clock sync (B5).
	case protocol.Buzz:
		if s.role != protocol.RolePlayer {
			r.sendError(s, in.Seq, protocol.ErrCodeForbidden, "only players can buzz")
			return
		}
		r.sendError(s, in.Seq, protocol.ErrCodeNotImplemented, "buzzing arrives with the game state machine")
	case protocol.StartTossup, protocol.MarkPower, protocol.ReadingFinished,
		protocol.Rule, protocol.Undo, protocol.Next, protocol.Protest:
		if s.role != protocol.RoleModerator {
			r.sendError(s, in.Seq, protocol.ErrCodeForbidden, "moderators only")
			return
		}
		r.sendError(s, in.Seq, protocol.ErrCodeNotImplemented, "game actions arrive with the game state machine")
	default:
		r.sendError(s, in.Seq, protocol.ErrCodeBadMessage, fmt.Sprintf("unexpected message %T", m))
	}
}

// broadcastState never blocks. Connections whose buffers are full are
// dropped and the resulting state is broadcast again; each extra pass drops
// at least one connection, so the loop terminates.
func (r *Room) broadcastState() {
	for {
		r.version++
		msg := encode(protocol.MsgState, r.snapshot())
		var slow []*seat
		for c, s := range r.byClient {
			select {
			case c.send <- msg:
			default:
				slow = append(slow, s)
			}
		}
		if len(slow) == 0 {
			return
		}
		for _, s := range slow {
			r.disconnect(s, ReasonSlow)
		}
	}
}

func (r *Room) sendError(s *seat, seq uint64, code protocol.ErrCode, message string) {
	msg := encode(protocol.MsgError, protocol.ErrorMessage{Code: code, Message: message, Seq: seq})
	if !r.sendTo(s, msg) {
		r.broadcastState()
	}
}

// sendTo drops s's connection if its buffer is full. On false the caller must
// broadcast.
func (r *Room) sendTo(s *seat, msg []byte) bool {
	select {
	case s.client.send <- msg:
		return true
	default:
		r.disconnect(s, ReasonSlow)
		return false
	}
}

// disconnect closes s's connection and keeps the seat.
func (r *Room) disconnect(s *seat, reason CloseReason) {
	delete(r.byClient, s.client)
	closeClient(s.client, reason)
	s.client = nil
}

func (r *Room) snapshot() protocol.RoomState {
	players := make([]protocol.PlayerView, 0, len(r.players)) // non-nil: encodes as []
	for _, s := range r.players {
		players = append(players, protocol.PlayerView{ID: s.id, Name: s.name, Connected: s.client != nil})
	}
	return protocol.RoomState{
		Epoch:              r.epoch,
		Version:            r.version,
		Code:               r.code,
		ModeratorConnected: r.moderator != nil && r.moderator.client != nil,
		Players:            players,
	}
}

// closeClient writes reason before close; Client.CloseReason depends on it.
func closeClient(c *Client, reason CloseReason) {
	c.reason = reason
	close(c.send)
}

// encode panics because marshaling registered protocol types cannot fail.
func encode(typ protocol.MsgType, v any) []byte {
	b, err := protocol.Encode(typ, v)
	if err != nil {
		panic(fmt.Sprintf("room: encoding %s: %v", typ, err))
	}
	return b
}
