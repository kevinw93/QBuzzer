package room

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/kevinw93/QBuzzer/internal/clock"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/secret"
)

var (
	ErrClosed         = errors.New("room closed")
	ErrFull           = errors.New("room full")
	ErrUnauthorized   = errors.New("wrong moderator password")
	ErrRateLimited    = errors.New("too many wrong moderator passwords; try again in a minute")
	ErrUnknownSession = errors.New("unknown session")
)

// Incoming is one decoded client message. Exactly one of Msg and Err is set.
type Incoming struct {
	Seq        uint64
	Msg        any   // a protocol payload by value, e.g. protocol.Buzz
	Err        error // decode or validation failure
	ReceivedAt time.Time
	Clock      clock.Estimate // the connection's clock estimate when the message was read
	ClockOK    bool           // false until the first ping exchange completes
	ServerMs   float64        // ReceivedAt on the clock.Server scale
}

type (
	joinEvent struct {
		c          *Client
		join       protocol.Join
		passwordOK bool       // verified by Join on the caller's goroutine
		reply      chan error // buffered so the room never blocks replying
	}
	leaveEvent struct {
		c *Client
	}
	messageEvent struct {
		c  *Client
		in Incoming
	}
	clockEvent struct {
		c   *Client
		est clock.Estimate
		at  time.Time
	}
)

// seat is a session: it outlives connections so a player can reconnect with
// the same identity and seq history.
type seat struct {
	id        string
	role      protocol.Role
	name      string
	tokenHash string  // SHA-256 of the session token; the token itself is never kept
	lastSeq   uint64  // highest seq processed; lower or equal seqs are duplicates
	client    *Client // nil while disconnected
}

// Room is one live game. All fields below lockedUntil are owned by the run
// goroutine; other goroutines interact only by sending events to inbox, which
// gives every event in the room a single total order. Messages from one
// connection are handled in the order they were read; across connections,
// inbox order need not match ReceivedAt order.
type Room struct {
	code         string
	gameID       string
	passwordHash string // argon2id; immutable, so Join verifies off the room goroutine
	cfg          Config
	inbox        chan any
	done         chan struct{} // closed when run returns
	lockedUntil  atomic.Int64  // unix nanos; moderator joins refused before this

	epoch     string
	restored  bool
	game      Game
	wake      *time.Timer // fires at the game's latest WakeAt; created stopped in run
	players   []*seat     // join order
	moderator *seat       // nil until a moderator first joins
	byToken   map[string]*seat
	byClient  map[*Client]*seat
	nextID    int
	version   uint64

	modFailures []time.Time // wrong passwords in the last moderatorLockout

	dirty    map[*seat]bool // seats changed since the last persist
	lastProj []byte         // last persisted projection
	unsaved  bool           // the sink refused an op; persistence stopped
}

func newRoom(code, passwordHash string, cfg Config) *Room {
	var g Game = unimplementedGame{}
	if cfg.NewGame != nil {
		g = cfg.NewGame()
	}
	return &Room{
		code:         code,
		gameID:       rand.Text(),
		passwordHash: passwordHash,
		cfg:          cfg,
		inbox:        make(chan any, 64),
		done:         make(chan struct{}),
		epoch:        rand.Text()[:8],
		game:         g,
		byToken:      make(map[string]*seat),
		byClient:     make(map[*Client]*seat),
		dirty:        make(map[*seat]bool),
	}
}

func restoredRoom(rs Restored, cfg Config) *Room {
	r := newRoom(rs.Code, rs.PasswordHash, cfg)
	r.gameID, r.game, r.restored = rs.GameID, rs.Game, true
	for _, sr := range rs.Seats {
		s := &seat{id: sr.PlayerID, role: sr.Role, name: sr.Name, tokenHash: sr.TokenHash, lastSeq: sr.LastSeq}
		if s.role == protocol.RoleModerator {
			r.moderator = s
			continue
		}
		r.players = append(r.players, s)
		if s.tokenHash != "" {
			r.byToken[s.tokenHash] = s
		}
		if n, err := strconv.Atoi(s.id[1:]); err == nil {
			r.nextID = max(r.nextID, n)
		}
	}
	return r
}

func (r *Room) Code() string { return r.code }

// Done is closed once the room has shut down.
func (r *Room) Done() <-chan struct{} { return r.done }

func (r *Room) NewClient() *Client { return newClient(r.cfg.SendBuffer) }

// Join attaches c to a new or existing seat. On success a Welcome followed
// by a RoomState is already pending on c. If the seat was connected
// elsewhere, that connection is closed with ReasonReplaced.
//
// A moderator's password is verified here, on the caller's goroutine: argon2id
// costs tens of milliseconds, which the room loop must never spend.
func (r *Room) Join(c *Client, j protocol.Join) error {
	ev := joinEvent{c: c, join: j, reply: make(chan error, 1)}
	if j.Role == protocol.RoleModerator {
		if time.Now().UnixNano() < r.lockedUntil.Load() {
			return ErrRateLimited // checked first so guessing can't burn CPU
		}
		ok, err := secret.VerifyPassword(j.Password, r.passwordHash)
		if err != nil {
			return err
		}
		ev.passwordOK = ok
	}
	if err := r.submit(ev); err != nil {
		return err
	}
	select {
	case err := <-ev.reply:
		return err
	case <-r.done:
		// If the room added c before exiting, it also closed c.
		return ErrClosed
	}
}

// Leave disconnects c, keeping its seat. It is idempotent, and safe after the
// room has dropped c or shut down.
func (r *Room) Leave(c *Client) { _ = r.submit(leaveEvent{c: c}) }

func (r *Room) Deliver(c *Client, in Incoming) error {
	return r.submit(messageEvent{c: c, in: in})
}

// UpdateClock reports c's latest clock estimate, taken at server time at.
func (r *Room) UpdateClock(c *Client, est clock.Estimate, at time.Time) {
	_ = r.submit(clockEvent{c: c, est: est, at: at})
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
// cfg.IdleTimeout. onExit runs before done is closed. An idle room is
// finished for good; a cancelled one stays recoverable.
func (r *Room) run(ctx context.Context, onExit func()) {
	idle := time.NewTimer(r.cfg.IdleTimeout) // armed only while nobody is connected
	defer idle.Stop()
	r.wake = time.NewTimer(time.Hour)
	r.wake.Stop()
	defer r.wake.Stop()

	defer func() {
		for c := range r.byClient {
			c.close(ReasonRoomClosed)
		}
		r.byClient, r.byToken = nil, nil
		onExit()
		close(r.done)
	}()

	if r.restored {
		r.persist(OpEpoch{GameID: r.gameID, Epoch: r.epoch})
		r.applyGame(nil, 0, r.game.Tick(time.Now())) // closes a window that expired during the restart
	} else {
		r.persist(OpCreateGame{GameID: r.gameID, Code: r.code, Epoch: r.epoch, PasswordHash: r.passwordHash, Rules: r.game.RulesJSON(), At: time.Now()})
	}
	r.persistChanges(time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			r.persist(OpFinish{GameID: r.gameID, At: time.Now()})
			return
		case now := <-r.wake.C:
			r.applyGame(nil, 0, r.game.Tick(now))
			r.persistChanges(now)
		case ev := <-r.inbox:
			wasEmpty := len(r.byClient) == 0
			r.handle(ev)
			r.persistChanges(time.Now())
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
		// they are never closed twice.
		if s, ok := r.byClient[ev.c]; ok {
			r.disconnect(s, ReasonLeft)
			r.broadcastState()
		}
	case messageEvent:
		if s, ok := r.byClient[ev.c]; ok {
			r.handleMessage(s, ev.in)
		}
	case clockEvent:
		if s, ok := r.byClient[ev.c]; ok && s.role == protocol.RolePlayer {
			r.game.PlayerClock(s.id, ev.est, ev.at)
		}
	default:
		panic(fmt.Sprintf("room: unknown event %T", ev))
	}
}

func (r *Room) handleJoin(ev joinEvent) {
	s, token, err := r.seatFor(ev)
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
		PlayerID: s.id, Role: s.role, SessionToken: token, LastSeq: s.lastSeq,
	}))
	r.broadcastState()
}

// seatFor finds or creates the seat for ev and returns the session token the
// client should keep (empty for moderators). ev.join has passed Join.Validate.
func (r *Room) seatFor(ev joinEvent) (*seat, string, error) {
	j := ev.join
	var name string
	if j.Name != "" {
		var err error
		if name, err = protocol.NormalizeName(j.Name); err != nil {
			return nil, "", err
		}
	}

	switch j.Role {
	case protocol.RoleModerator:
		if err := r.checkModerator(ev.passwordOK, time.Now()); err != nil {
			return nil, "", err
		}
		if r.moderator == nil {
			r.moderator = &seat{id: "mod", role: protocol.RoleModerator, name: "Moderator"}
			r.dirty[r.moderator] = true
		}
		if name != "" && name != r.moderator.name {
			r.moderator.name = name
			r.dirty[r.moderator] = true
		}
		return r.moderator, "", nil

	case protocol.RolePlayer:
		if j.Token != "" {
			s, ok := r.byToken[secret.HashToken(j.Token)]
			if !ok {
				return nil, "", ErrUnknownSession
			}
			if name != "" && name != s.name {
				s.name = name
				r.dirty[s] = true
			}
			return s, j.Token, nil
		}
		if len(r.players) >= r.cfg.MaxPlayersPerRoom {
			return nil, "", ErrFull
		}
		r.nextID++
		token := rand.Text()
		s := &seat{id: "p" + strconv.Itoa(r.nextID), role: protocol.RolePlayer, name: name, tokenHash: secret.HashToken(token)}
		r.players = append(r.players, s)
		r.byToken[s.tokenHash] = s
		r.dirty[s] = true
		r.game.PlayerJoined(s.id)
		return s, token, nil
	}
	return nil, "", fmt.Errorf("unknown role %q", j.Role)
}

func (r *Room) handleMessage(s *seat, in Incoming) {
	if in.Err != nil {
		if in.Seq > s.lastSeq {
			s.lastSeq = in.Seq
			r.dirty[s] = true
		}
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
	r.dirty[s] = true

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
	case protocol.Buzz, protocol.ChooseTeam:
		if s.role != protocol.RolePlayer {
			r.sendError(s, in.Seq, protocol.ErrCodeForbidden, "players only")
			return
		}
		r.toGame(s, in)
	case protocol.StartTossup,
		protocol.Rule, protocol.Undo, protocol.Next, protocol.Protest,
		protocol.RuleBonus, protocol.CreateTeam, protocol.AssignTeam:
		if s.role != protocol.RoleModerator {
			r.sendError(s, in.Seq, protocol.ErrCodeForbidden, "moderators only")
			return
		}
		r.toGame(s, in)
	default:
		r.sendError(s, in.Seq, protocol.ErrCodeBadMessage, fmt.Sprintf("unexpected message %T", m))
	}
}

func (r *Room) toGame(s *seat, in Incoming) {
	res := r.game.Handle(Command{
		PlayerID: s.id, Role: s.role, Msg: in.Msg, ReceivedAt: in.ReceivedAt, Clock: in.Clock, ClockOK: in.ClockOK, ServerMs: in.ServerMs,
	})
	r.applyGame(s, in.Seq, res)
}

// applyGame sends a game Result. from is nil for ticks, which never reject.
func (r *Room) applyGame(from *seat, seq uint64, res Result) {
	if res.WakeAt.IsZero() {
		r.wake.Stop()
	} else {
		r.wake.Reset(time.Until(res.WakeAt)) // a past WakeAt fires immediately
	}
	if res.Reject != nil && from != nil {
		rej := *res.Reject
		rej.Seq = seq
		if !r.sendTo(from, encode(protocol.MsgError, rej)) {
			res.Changed = true // the sender was dropped; others must see it
		}
	}
	// Effects are queued before the state is set, so a client never sees an
	// effect's state before the effect itself; v is the version that state
	// will get.
	v := r.version
	if res.Changed {
		v++
	}
	for _, e := range res.Effects {
		if r.broadcastEvent(encode(e.EffectType(), e.WithVersion(v))) {
			res.Changed = true
		}
	}
	if res.Changed {
		r.broadcastState()
	}
}

// persistChanges hands the sink everything that changed since the last call:
// new log entries, changed seats, and the projection if it differs. It never
// blocks; the game's log is drained even when nothing is being persisted.
func (r *Room) persistChanges(now time.Time) {
	evs := r.game.Drain()
	if r.cfg.Sink == nil || r.unsaved {
		return
	}
	if len(evs) > 0 {
		r.persist(OpEvents{GameID: r.gameID, Events: evs, At: now})
	}
	for s := range r.dirty {
		r.persist(OpSeat{GameID: r.gameID, Seat: SeatRecord{PlayerID: s.id, Role: s.role, Name: s.name, TokenHash: s.tokenHash, LastSeq: s.lastSeq}})
	}
	clear(r.dirty)
	proj := projection(r.game.View())
	if b, err := json.Marshal(proj); err == nil && !bytes.Equal(b, r.lastProj) {
		r.persist(OpProjection{GameID: r.gameID, View: proj})
		r.lastProj = b
	}
}

func (r *Room) persist(op Op) {
	if r.cfg.Sink == nil || r.unsaved {
		return
	}
	if !r.cfg.Sink.Enqueue(op) {
		r.unsaved = true // a gap in the log can't be recovered from, so stop entirely
		r.broadcastState()
	}
}

// broadcastState makes a new state pending for every connection. It cannot
// fail: states replace each other in each mailbox.
func (r *Room) broadcastState() {
	r.version++
	msg := encode(protocol.MsgState, r.snapshot())
	for c := range r.byClient {
		c.setState(msg)
	}
}

// broadcastEvent queues msg for every connection, dropping any whose queue is
// full. It reports whether anyone was dropped; the caller must then broadcast.
func (r *Room) broadcastEvent(msg []byte) bool {
	var slow []*seat
	for c, s := range r.byClient {
		if !c.enqueue(msg) {
			slow = append(slow, s)
		}
	}
	for _, s := range slow {
		r.disconnect(s, ReasonSlow)
	}
	return len(slow) > 0
}

func (r *Room) sendError(s *seat, seq uint64, code protocol.ErrCode, message string) {
	msg := encode(protocol.MsgError, protocol.ErrorMessage{Code: code, Message: message, Seq: seq})
	if !r.sendTo(s, msg) {
		r.broadcastState()
	}
}

// sendTo queues msg for s, dropping s's connection if its queue is full. On
// false the caller must broadcast.
func (r *Room) sendTo(s *seat, msg []byte) bool {
	if s.client.enqueue(msg) {
		return true
	}
	r.disconnect(s, ReasonSlow)
	return false
}

// disconnect closes s's connection and keeps the seat.
func (r *Room) disconnect(s *seat, reason CloseReason) {
	delete(r.byClient, s.client)
	s.client.close(reason)
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
		Game:               r.game.View(),
		Unsaved:            r.unsaved,
	}
}

// encode panics because marshaling registered protocol types cannot fail.
func encode(typ protocol.MsgType, v any) []byte {
	b, err := protocol.Encode(typ, v)
	if err != nil {
		panic(fmt.Sprintf("room: encoding %s: %v", typ, err))
	}
	return b
}

const (
	maxModeratorFailures = 5
	moderatorLockout     = time.Minute
)

// checkModerator refuses all moderator joins for moderatorLockout after
// maxModeratorFailures wrong passwords within moderatorLockout, so a player
// can't guess the password by trying many.
func (r *Room) checkModerator(passwordOK bool, now time.Time) error {
	if now.UnixNano() < r.lockedUntil.Load() {
		return ErrRateLimited
	}
	if passwordOK {
		return nil
	}
	recent := r.modFailures[:0]
	for _, t := range r.modFailures {
		if now.Sub(t) < moderatorLockout {
			recent = append(recent, t)
		}
	}
	r.modFailures = append(recent, now)
	if len(r.modFailures) >= maxModeratorFailures {
		r.lockedUntil.Store(now.Add(moderatorLockout).UnixNano())
		r.modFailures = nil
	}
	return ErrUnauthorized
}
