package game_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/kevinw93/QBuzzer/internal/game"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

type conn struct {
	t   *testing.T
	r   *room.Room
	c   *room.Client
	seq uint64
	id  string
	tok string
}

func connect(t *testing.T, r *room.Room, j protocol.Join) *conn {
	t.Helper()
	j.ProtocolVersion = protocol.ProtocolVersion
	c := r.NewClient()
	if err := r.Join(c, j); err != nil {
		t.Fatalf("join: %v", err)
	}
	x := &conn{t: t, r: r, c: c}
	for _, e := range x.drain() {
		if e.Type == protocol.MsgWelcome {
			var w protocol.Welcome
			_ = json.Unmarshal(e.Payload, &w)
			x.id, x.tok, x.seq = w.PlayerID, w.SessionToken, w.LastSeq
		}
	}
	return x
}

func (x *conn) send(msg any) {
	x.t.Helper()
	x.seq++
	if err := x.r.Deliver(x.c, room.Incoming{Seq: x.seq, Msg: msg, ReceivedAt: time.Now()}); err != nil {
		x.t.Fatal(err)
	}
}

// drain returns everything delivered to x once the room is idle.
func (x *conn) drain() []protocol.Envelope {
	x.t.Helper()
	synctest.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Next still returns pending messages before checking ctx
	var out []protocol.Envelope
	for {
		b, err := x.c.Next(ctx)
		if err != nil {
			return out
		}
		var e protocol.Envelope
		if err := json.Unmarshal(b, &e); err != nil {
			x.t.Fatal(err)
		}
		out = append(out, e)
	}
}

func (x *conn) state() protocol.RoomState {
	x.t.Helper()
	var s protocol.RoomState
	envs := x.drain()
	for _, e := range envs {
		if e.Type == protocol.MsgError {
			x.t.Fatalf("unexpected error: %s", e.Payload)
		}
		if e.Type == protocol.MsgState {
			_ = json.Unmarshal(e.Payload, &s)
		}
	}
	if s.Version == 0 {
		x.t.Fatalf("no state delivered (got %d messages)", len(envs))
	}
	return s
}

func TestGameThroughRoom(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rules := game.Standard()
		ctx, cancel := context.WithCancel(context.Background())
		cfg := room.DefaultConfig()
		cfg.NewGame = func() room.Game { return game.New(rules) }
		hub := room.NewHub(ctx, cfg, slog.New(slog.DiscardHandler))
		t.Cleanup(func() { cancel(); hub.Wait() })
		r, err := hub.Create()
		if err != nil {
			t.Fatal(err)
		}

		mod := connect(t, r, protocol.Join{Role: protocol.RoleModerator, Token: r.ModeratorToken()})
		alice := connect(t, r, protocol.Join{Role: protocol.RolePlayer, Name: "Alice"})
		bob := connect(t, r, protocol.Join{Role: protocol.RolePlayer, Name: "Bob"})
		mod.send(protocol.CreateTeam{Name: "Alpha"})
		mod.send(protocol.CreateTeam{Name: "Beta"})
		mod.send(protocol.AssignTeam{PlayerID: alice.id, TeamID: "T1"})
		bob.send(protocol.ChooseTeam{TeamID: "T2"})
		if s := mod.state(); len(s.Game.Teams) != 2 || s.Game.Teams[1].PlayerIDs[0] != bob.id {
			t.Fatalf("teams = %+v", s.Game.Teams)
		}
		alice.drain()
		bob.drain()

		mod.send(protocol.StartTossup{TossupID: "t1"})
		time.Sleep(time.Second)
		alice.send(protocol.Buzz{TossupID: "t1"})
		if s := mod.state(); s.Game.Phase != protocol.PhaseBuzzWindow {
			t.Fatalf("phase = %s", s.Game.Phase)
		}

		// The room's wake timer closes the window with no further commands.
		time.Sleep(rules.MaxBuzzWindow + 10*time.Millisecond)
		envs := mod.drain()
		if len(envs) != 2 || envs[0].Type != protocol.MsgBuzzLocked || envs[1].Type != protocol.MsgState {
			t.Fatalf("after window: %d messages %+v", len(envs), envs)
		}
		var locked protocol.BuzzLocked
		var s protocol.RoomState
		_ = json.Unmarshal(envs[0].Payload, &locked)
		_ = json.Unmarshal(envs[1].Payload, &s)
		if locked.PlayerID != alice.id || locked.Version != s.Version || s.Game.Phase != protocol.PhaseAwaitingRuling {
			t.Fatalf("locked %+v, state %+v", locked, s.Game)
		}
		buzz := s.Game.Buzzes[0]
		if buzz.OffsetMs != 1000 || !buzz.Power {
			t.Errorf("buzz = %+v", buzz)
		}

		// Alice drops while holding the buzz. Nothing is ruled automatically:
		// she answered over voice chat, which QBuzzer can't see.
		r.Leave(alice.c)
		s = mod.state()
		if s.Game.Phase != protocol.PhaseAwaitingRuling || s.Game.Buzzes[0].PlayerID != alice.id || s.Players[0].Connected {
			t.Fatalf("after disconnect: game %+v, players %+v", s.Game, s.Players)
		}
		time.Sleep(time.Minute)
		if s := mod.drain(); len(s) != 0 {
			t.Fatalf("game changed on its own: %+v", s)
		}

		mod.send(protocol.Rule{BuzzID: buzz.ID, Result: protocol.RulingCorrect})
		s = mod.state()
		if s.Game.Phase != protocol.PhaseBonus || s.Game.Teams[0].Score != 15 {
			t.Fatalf("after ruling: %+v", s.Game)
		}

		back := connect(t, r, protocol.Join{Role: protocol.RolePlayer, Token: alice.tok})
		if back.id != alice.id {
			t.Fatalf("rejoined as %s, want %s", back.id, alice.id)
		}
		if s := mod.state(); !s.Players[0].Connected || s.Game.Teams[0].Score != 15 {
			t.Fatalf("after rejoin: %+v", s)
		}

		// Undo through the room: the latest action is the correct ruling.
		mod.send(protocol.Undo{ActionID: s.Game.LastActionID})
		if s := mod.state(); s.Game.Phase != protocol.PhaseAwaitingRuling || s.Game.Teams[0].Score != 0 {
			t.Fatalf("after undo: %+v", s.Game)
		}
	})
}
