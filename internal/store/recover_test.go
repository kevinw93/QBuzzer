package store

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinw93/QBuzzer/internal/game"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
	"github.com/kevinw93/QBuzzer/internal/secret"
)

const password = "hunter22"

type server struct {
	hub    *room.Hub
	w      *Writer
	cancel context.CancelFunc
}

func startServer(t *testing.T, pool *pgxpool.Pool) *server {
	t.Helper()
	w := startWriter(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	cfg := room.DefaultConfig()
	cfg.PasswordParams = secret.Fast
	cfg.Sink = w
	cfg.NewGame = func() room.Game { return game.New(game.Standard()) }
	hub := room.NewHub(ctx, cfg, slog.New(slog.DiscardHandler))
	return &server{hub: hub, w: w, cancel: cancel}
}

// stop is a graceful shutdown: rooms exit (still recoverable), then the
// writer flushes.
func (s *server) stop(t *testing.T) {
	t.Helper()
	s.cancel()
	s.hub.Wait()
	closeWriter(t, s.w)
}

type client struct {
	t   *testing.T
	r   *room.Room
	c   *room.Client
	w   protocol.Welcome
	seq uint64
}

func join(t *testing.T, r *room.Room, j protocol.Join) *client {
	t.Helper()
	j.ProtocolVersion = protocol.ProtocolVersion
	c := r.NewClient()
	if err := r.Join(c, j); err != nil {
		t.Fatalf("join %+v: %v", j, err)
	}
	cl := &client{t: t, r: r, c: c}
	cl.w = waitFor[protocol.Welcome](cl, protocol.MsgWelcome, func(protocol.Welcome) bool { return true })
	cl.seq = cl.w.LastSeq
	return cl
}

func (c *client) send(msg any) {
	c.t.Helper()
	c.seq++
	if err := c.r.Deliver(c.c, room.Incoming{Seq: c.seq, Msg: msg, ReceivedAt: time.Now()}); err != nil {
		c.t.Fatal(err)
	}
}

func waitFor[T any](c *client, typ protocol.MsgType, ok func(T) bool) T {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		b, err := c.c.Next(ctx)
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", typ, err)
		}
		var env protocol.Envelope
		_ = json.Unmarshal(b, &env)
		if env.Type == protocol.MsgError {
			c.t.Fatalf("error: %s", env.Payload)
		}
		if env.Type != typ {
			continue
		}
		var v T
		_ = json.Unmarshal(env.Payload, &v)
		if ok(v) {
			return v
		}
	}
}

func (c *client) state(ok func(protocol.RoomState) bool) protocol.RoomState {
	c.t.Helper()
	return waitFor(c, protocol.MsgState, ok)
}

func phase(p protocol.Phase) func(protocol.RoomState) bool {
	return func(s protocol.RoomState) bool { return s.Game.Phase == p }
}

func restoreGame(rules []byte, log []room.GameEvent) (room.Game, error) {
	return game.Restore(rules, log)
}

// A game is played, the server shuts down mid-tossup, and a new server
// recovers it from Postgres: same code, same scores, same phase; players
// rejoin their seats with their tokens and the game continues.
func TestRecoverInProgressGame(t *testing.T) {
	pool := testPool(t)
	s1 := startServer(t, pool)
	r, err := s1.hub.Create(password)
	if err != nil {
		t.Fatal(err)
	}
	code := r.Code()

	mod := join(t, r, protocol.Join{Role: protocol.RoleModerator, Password: password})
	alice := join(t, r, protocol.Join{Role: protocol.RolePlayer, Name: "Alice"})
	bob := join(t, r, protocol.Join{Role: protocol.RolePlayer, Name: "Bob"})
	mod.send(protocol.CreateTeam{Name: "Alpha"})
	mod.send(protocol.CreateTeam{Name: "Beta"})
	mod.send(protocol.AssignTeam{PlayerID: alice.w.PlayerID, TeamID: "T1"})
	bob.send(protocol.ChooseTeam{TeamID: "T2"})
	mod.state(func(s protocol.RoomState) bool { return len(s.Game.Teams) == 2 && len(s.Game.Teams[1].PlayerIDs) == 1 })

	mod.send(protocol.StartTossup{TossupID: "t1"})
	mod.state(phase(protocol.PhaseReading))
	alice.send(protocol.Buzz{TossupID: "t1"})
	st := mod.state(phase(protocol.PhaseAwaitingRuling))
	mod.send(protocol.Rule{BuzzID: st.Game.Buzzes[0].ID, Result: protocol.RulingPower})
	st = mod.state(phase(protocol.PhaseBonus))
	mod.send(protocol.RuleBonus{BonusID: st.Game.Bonus.ID, Part: 0, Correct: true})
	mod.send(protocol.Next{TossupID: "t1"})
	mod.send(protocol.StartTossup{TossupID: "t2"})
	mod.state(phase(protocol.PhaseReading))
	bob.send(protocol.Buzz{TossupID: "t2"})
	before := mod.state(phase(protocol.PhaseAwaitingRuling))

	s1.stop(t)

	s2 := startServer(t, pool)
	defer s2.stop(t)
	n, err := Recover(context.Background(), pool, s2.hub, restoreGame, time.Now().Add(-2*time.Hour), slog.New(slog.DiscardHandler))
	if err != nil || n != 1 {
		t.Fatalf("recovered %d, err %v", n, err)
	}
	r2, ok := s2.hub.Get(code)
	if !ok {
		t.Fatal("room not restored under its code")
	}

	alice2 := join(t, r2, protocol.Join{Role: protocol.RolePlayer, Token: alice.w.SessionToken})
	if alice2.w.PlayerID != alice.w.PlayerID || alice2.w.LastSeq != alice.seq {
		t.Errorf("alice rejoined as %+v; was %s with last seq %d", alice2.w, alice.w.PlayerID, alice.seq)
	}
	after := alice2.state(func(protocol.RoomState) bool { return true })
	if after.Epoch == before.Epoch {
		t.Error("restored room kept the old epoch")
	}
	if after.Game.Phase != protocol.PhaseAwaitingRuling || after.Game.TossupID != "t2" ||
		after.Game.Teams[0].Score != 25 || len(after.Game.History) != 1 || after.Game.Buzzes[0].PlayerID != bob.w.PlayerID {
		t.Fatalf("restored game = %+v", after.Game)
	}

	// The game continues, and its log keeps growing without seq conflicts.
	mod2 := join(t, r2, protocol.Join{Role: protocol.RoleModerator, Password: password})
	mod2.send(protocol.Rule{BuzzID: after.Game.Buzzes[0].ID, Result: protocol.RulingNeg})
	mod2.state(func(s protocol.RoomState) bool {
		return s.Game.Phase == protocol.PhaseReading && s.Game.Teams[1].Score == -5
	})
	if err := r2.Join(r2.NewClient(), protocol.Join{ProtocolVersion: 1, Role: protocol.RoleModerator, Password: "wrong-one"}); !errors.Is(err, room.ErrUnauthorized) {
		t.Errorf("wrong password after restore: %v", err)
	}
}

func TestStaleAndCorruptGamesAreFinishedNotRestored(t *testing.T) {
	pool := testPool(t)
	w := startWriter(t, pool)
	rules := game.New(game.Standard()).RulesJSON()
	now := time.Now()
	w.Enqueue(room.OpCreateGame{GameID: "old", Code: "OLD234", Epoch: "e", PasswordHash: "h", Rules: rules, At: now.Add(-5 * time.Hour)})
	w.Enqueue(room.OpCreateGame{GameID: "gap", Code: "GAP234", Epoch: "e", PasswordHash: "h", Rules: rules, At: now})
	w.Enqueue(room.OpEvents{GameID: "gap", At: now, Events: []room.GameEvent{
		{Seq: 1, Kind: "player_joined", At: now, Data: json.RawMessage(`{"id":1,"player":"p1"}`)},
		{Seq: 3, Kind: "player_joined", At: now, Data: json.RawMessage(`{"id":2,"player":"p2"}`)},
	}})
	closeWriter(t, w)

	s := startServer(t, pool)
	defer s.stop(t)
	n, err := Recover(context.Background(), pool, s.hub, restoreGame, now.Add(-2*time.Hour), slog.New(slog.DiscardHandler))
	if err != nil || n != 0 {
		t.Fatalf("recovered %d, err %v", n, err)
	}
	var unfinished int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM games WHERE finished_at IS NULL`).Scan(&unfinished)
	if unfinished != 0 {
		t.Errorf("%d games left unfinished", unfinished)
	}
	// Finished codes can be reused by a new game.
	if _, err := s.hub.Create(password); err != nil {
		t.Fatal(err)
	}
}
