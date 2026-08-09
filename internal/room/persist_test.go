package room

import (
	"encoding/json"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/secret"
)

// fakeSink records ops and refuses everything after limit.
type fakeSink struct {
	mu    sync.Mutex
	ops   []Op
	limit int
}

func (s *fakeSink) Enqueue(op Op) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limit > 0 && len(s.ops) >= s.limit {
		return false
	}
	s.ops = append(s.ops, op)
	return true
}

func (s *fakeSink) snapshot() []Op {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Op(nil), s.ops...)
}

// loggingGame is recordingGame plus a durable log of one entry per command.
type loggingGame struct {
	recordingGame
	seq     int
	pending []GameEvent
}

func (g *loggingGame) Handle(cmd Command) Result {
	g.seq++
	g.pending = append(g.pending, GameEvent{Seq: g.seq, Kind: "cmd", At: cmd.ReceivedAt, Data: json.RawMessage(`{}`)})
	return g.recordingGame.Handle(cmd)
}

func (g *loggingGame) Drain() []GameEvent {
	p := g.pending
	g.pending = nil
	return p
}

func TestRoomPersistsCreateEventsAndSeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := &fakeSink{}
		g := &loggingGame{}
		r := newTestRoom(t, func(c *Config) { c.Sink = sink; withGame(g)(c) })
		a, _ := join(t, r, "Alice")
		send(t, r, a, 1, protocol.Buzz{TossupID: "t1"})
		send(t, r, a, 2, protocol.Buzz{TossupID: "t1"})
		synctest.Wait()

		ops := sink.snapshot()
		create, ok := ops[0].(OpCreateGame)
		if !ok || create.Code != r.Code() || create.GameID == "" || create.PasswordHash == testPassword {
			t.Fatalf("first op = %+v; want a create with a hashed password", ops[0])
		}
		var seqs []int
		var lastSeat SeatRecord
		for _, op := range ops {
			if GameID(op) != create.GameID {
				t.Errorf("op for another game: %+v", op)
			}
			switch o := op.(type) {
			case OpEvents:
				for _, e := range o.Events {
					seqs = append(seqs, e.Seq)
				}
			case OpSeat:
				lastSeat = o.Seat
			}
		}
		if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
			t.Errorf("logged seqs = %v", seqs)
		}
		if lastSeat.Name != "Alice" || lastSeat.LastSeq != 2 || len(lastSeat.TokenHash) != 64 {
			t.Errorf("last seat = %+v", lastSeat)
		}
	})
}

func TestRefusedWriteMarksGameUnsavedAndPlayContinues(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sink := &fakeSink{limit: 2}
		r := newTestRoom(t, func(c *Config) { c.Sink = sink })
		a, _ := join(t, r, "Alice")
		s := stateUntil(t, a, func(s protocol.RoomState) bool { return s.Unsaved })
		if names(s) != "Alice" {
			t.Errorf("state = %+v", s)
		}
		before := len(sink.snapshot())
		send(t, r, a, 1, protocol.SetName{Name: "Alicia"})
		stateUntil(t, a, namesAre("Alicia")) // the game goes on
		if got := len(sink.snapshot()); got != before {
			t.Errorf("room kept writing after a refused op: %d -> %d ops", before, got)
		}
	})
}

func TestRestoredRoomResumesSeatsByTokenHash(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, _ := newTestHub(t, nil)
		r1, _ := h.Create(testPassword)
		_, w := join(t, r1, "Alice")

		r2, err := h.Restore(Restored{
			GameID: "g1", Code: "RST234", PasswordHash: r1.passwordHash, Game: unimplementedGame{},
			Seats: []SeatRecord{{PlayerID: "p7", Role: protocol.RolePlayer, Name: "Alice", TokenHash: hashOf(w.SessionToken), LastSeq: 9}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, w2 := joinAs(t, r2, playerJoin("", w.SessionToken))
		if w2.PlayerID != "p7" || w2.LastSeq != 9 || w2.SessionToken != w.SessionToken {
			t.Errorf("resumed welcome = %+v", w2)
		}
		_, w3 := join(t, r2, "Bob")
		if w3.PlayerID != "p8" {
			t.Errorf("new seat after restore = %s, want p8", w3.PlayerID)
		}
		if _, err := h.Restore(Restored{Code: "RST234"}); err == nil {
			t.Error("restored a second room under a live code")
		}
		joinModerator(t, r2) // the stored hash still verifies the password
	})
}

func hashOf(token string) string { return secret.HashToken(token) }
