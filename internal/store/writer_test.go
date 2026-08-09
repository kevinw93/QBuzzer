package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
	"github.com/kevinw93/QBuzzer/internal/store/db"
)

var t0 = time.Date(2026, 11, 15, 19, 0, 0, 0, time.UTC)

func TestMigrationsGoDownAndUpCleanly(t *testing.T) {
	pool := testPool(t)
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, mustSub(migrations, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

func startWriter(t *testing.T, pool *pgxpool.Pool) *Writer {
	t.Helper()
	w := NewWriter(pool, slog.New(slog.DiscardHandler), DefaultWriterConfig())
	go w.Run()
	return w
}

func closeWriter(t *testing.T, w *Writer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func evs(from int, kinds ...string) []room.GameEvent {
	var out []room.GameEvent
	for i, k := range kinds {
		out = append(out, room.GameEvent{Seq: from + i, Kind: k, At: t0.Add(time.Duration(from+i) * time.Second), Data: json.RawMessage(`{}`)})
	}
	return out
}

func TestWriterPersistsEverything(t *testing.T) {
	pool := testPool(t)
	w := startWriter(t, pool)
	const g = "game1"
	ops := []room.Op{
		room.OpCreateGame{GameID: g, Code: "ABC234", Epoch: "e1", PasswordHash: "$argon2id$x", Rules: json.RawMessage(`{"tossupPoints":10}`), At: t0},
		room.OpEvents{GameID: g, Events: evs(1, "player_joined", "player_joined"), At: t0},
		room.OpSeat{GameID: g, Seat: room.SeatRecord{PlayerID: "p1", Role: protocol.RolePlayer, Name: "Al", TokenHash: "h1", LastSeq: 1}},
		room.OpSeat{GameID: g, Seat: room.SeatRecord{PlayerID: "p1", Role: protocol.RolePlayer, Name: "Alice", TokenHash: "h1", LastSeq: 7}}, // supersedes
		room.OpSeat{GameID: g, Seat: room.SeatRecord{PlayerID: "p2", Role: protocol.RolePlayer, Name: "Bob", TokenHash: "h2"}},
		room.OpSeat{GameID: g, Seat: room.SeatRecord{PlayerID: "mod", Role: protocol.RoleModerator, Name: "Moderator"}},
		room.OpEvents{GameID: g, Events: evs(3, "start_tossup", "buzz", "window_closed", "rule"), At: t0.Add(time.Minute)},
		room.OpProjection{GameID: g, View: room.Projection{
			Teams:       []room.ProjectedTeam{{ID: "T1", Name: "Alpha"}, {ID: "T2", Name: "Beta"}},
			PlayerTeams: map[string]string{"p1": "T1", "p2": "T2"},
			History: []protocol.TossupRecord{{TossupID: "t1", Number: 1, Buzzes: []protocol.BuzzView{
				{ID: "b1", PlayerID: "p2", TeamID: "T2", OffsetMs: 900, CorrectionMs: 30, Status: protocol.BuzzStatusIncorrect, Points: -5, RTTMs: 41},
				{ID: "b2", PlayerID: "p1", TeamID: "T1", OffsetMs: 1500, CorrectionMs: 12, Status: protocol.BuzzStatusCorrect, Power: true, Points: 15},
				{ID: "b3", PlayerID: "p2", TeamID: "T2", OffsetMs: 1510, Status: protocol.BuzzStatusLost},
			}, Bonus: &protocol.BonusView{ID: "B1", TeamID: "T1", Parts: []protocol.BonusPartView{{Ruled: true, Correct: true}, {Ruled: true, BouncebackTeamID: "T2"}, {Ruled: true}}}}},
			Protests:        []protocol.ProtestView{{TossupID: "t1", PlayerID: "p2", Note: "pronunciation"}},
			BonusPartPoints: 10,
		}},
	}
	for _, op := range ops {
		if !w.Enqueue(op) {
			t.Fatal("enqueue refused")
		}
	}
	closeWriter(t, w)

	ctx := context.Background()
	q := db.New(pool)
	events, err := q.GameEvents(ctx, g)
	if err != nil || len(events) != 6 || events[5].Kind != "rule" {
		t.Fatalf("events = %+v, %v", events, err)
	}
	players, err := q.GamePlayers(ctx, g)
	if err != nil || len(players) != 3 || players[1].Name != "Alice" || players[1].LastSeq != 7 || players[0].TokenHash.Valid {
		t.Fatalf("players = %+v, %v", players, err)
	}
	stats, err := q.PlayerStats(ctx, g)
	if err != nil || len(stats) != 2 {
		t.Fatalf("stats = %+v, %v", stats, err)
	}
	if s := stats[0]; s.PlayerID != "p1" || s.Powers != 1 || s.Points != 15 || s.CorrectBuzzMsTotal != 1500 {
		t.Errorf("alice stats = %+v", s)
	}
	if s := stats[1]; s.PlayerID != "p2" || s.Negs != 1 || s.Points != -5 {
		t.Errorf("bob stats = %+v (lost buzzes have no ruling)", s)
	}
	var bonusPoints, arrived float64
	var rtt *float64
	if err := pool.QueryRow(ctx, `SELECT sum(points) FROM bonus_parts WHERE game_id = $1`, g).Scan(&bonusPoints); err != nil || bonusPoints != 20 {
		t.Errorf("bonus points = %v, %v", bonusPoints, err)
	}
	if err := pool.QueryRow(ctx, `SELECT arrived_ms, rtt_ms FROM buzzes WHERE buzz_id = 'b1'`).Scan(&arrived, &rtt); err != nil || arrived != 930 || rtt == nil || *rtt != 41 {
		t.Errorf("b1 arrived %v rtt %v, %v", arrived, rtt, err)
	}

	// A later projection replaces the earlier one entirely.
	w = startWriter(t, pool)
	w.Enqueue(room.OpProjection{GameID: g, View: room.Projection{Teams: []room.ProjectedTeam{{ID: "T1", Name: "Alpha"}}, PlayerTeams: map[string]string{}}})
	w.Enqueue(room.OpFinish{GameID: g, At: t0.Add(time.Hour)})
	closeWriter(t, w)
	var tossups, teams int
	var finished bool
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tossups), (SELECT count(*) FROM teams), (SELECT finished_at IS NOT NULL FROM games)`).Scan(&tossups, &teams, &finished)
	if tossups != 0 || teams != 1 || !finished {
		t.Errorf("after replacement: tossups %d teams %d finished %v", tossups, teams, finished)
	}
}

func TestEnqueueNeverBlocks(t *testing.T) {
	cfg := DefaultWriterConfig()
	cfg.Capacity = 2
	w := NewWriter(nil, slog.New(slog.DiscardHandler), cfg) // not running: nothing drains
	op := room.OpFinish{GameID: "g"}
	for i := range cfg.Capacity {
		if !w.Enqueue(op) {
			t.Fatalf("enqueue %d within capacity refused", i)
		}
	}
	done := make(chan bool)
	go func() { done <- w.Enqueue(op) }()
	select {
	case ok := <-done:
		if ok {
			t.Error("enqueue past capacity accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("Enqueue blocked")
	}
}

func TestUnreachableDatabaseDropsBatchAndMarksGameBroken(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	cfg := DefaultWriterConfig()
	cfg.MaxAttempts = 2
	w := NewWriter(pool, slog.New(slog.DiscardHandler), cfg)
	w.flush([]room.Op{room.OpFinish{GameID: "g1"}})
	if !w.broken["g1"] {
		t.Fatal("game not marked broken after failed attempts")
	}
	w.flush([]room.Op{room.OpFinish{GameID: "g1"}}) // skipped without touching the database
}
