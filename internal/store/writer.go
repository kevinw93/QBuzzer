package store

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinw93/QBuzzer/internal/metrics"
	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
	"github.com/kevinw93/QBuzzer/internal/store/db"
)

// WriterConfig tunes the writer. A crash loses at most FlushEvery of writes.
type WriterConfig struct {
	Capacity    int           // ops buffered before Enqueue refuses
	FlushEvery  time.Duration // flush at least this often
	FlushEvents int           // or once this many log entries are buffered
	MaxAttempts int           // per batch before giving up on it
}

func DefaultWriterConfig() WriterConfig {
	return WriterConfig{Capacity: 10_000, FlushEvery: 100 * time.Millisecond, FlushEvents: 50, MaxAttempts: 5}
}

// Writer is the room.Sink backed by Postgres. Rooms call Enqueue on their own
// goroutines; one writer goroutine batches ops into transactions. Ops reach
// Postgres in the order each room produced them.
type Writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	cfg  WriterConfig
	ops  chan room.Op
	stop chan struct{}
	done chan struct{}

	broken map[string]bool // games whose writes were dropped; their logs have a gap
}

var _ room.Sink = (*Writer)(nil)

func NewWriter(pool *pgxpool.Pool, log *slog.Logger, cfg WriterConfig) *Writer {
	return &Writer{
		pool: pool, log: log, cfg: cfg,
		ops:    make(chan room.Op, cfg.Capacity),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		broken: map[string]bool{},
	}
}

// QueueLen is the number of ops waiting to be written.
func (w *Writer) QueueLen() int { return len(w.ops) }

// Enqueue never blocks.
func (w *Writer) Enqueue(op room.Op) bool {
	select {
	case w.ops <- op:
		return true
	default:
		return false
	}
}

// Run writes until Close is called, then flushes what's buffered and returns.
func (w *Writer) Run() {
	defer close(w.done)
	tick := time.NewTicker(w.cfg.FlushEvery)
	defer tick.Stop()
	var batch []room.Op
	events := 0
	for {
		select {
		case op := <-w.ops:
			batch = append(batch, op)
			if e, ok := op.(room.OpEvents); ok {
				events += len(e.Events)
			}
			if events >= w.cfg.FlushEvents {
				w.flush(batch)
				batch, events = nil, 0
			}
		case <-tick.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch, events = nil, 0
			}
		case <-w.stop:
			for {
				select {
				case op := <-w.ops:
					batch = append(batch, op)
					continue
				default:
				}
				break
			}
			if len(batch) > 0 {
				w.flush(batch)
			}
			return
		}
	}
}

// Close stops the writer after a final flush, or gives up when ctx is done.
// Call it only after every room has stopped enqueueing.
func (w *Writer) Close(ctx context.Context) error {
	close(w.stop)
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// flush writes batch in one transaction, retrying with backoff. If every
// attempt fails, the batch is dropped and its games are marked broken: their
// logs now have a gap, so later writes for them are pointless and recovery
// will refuse them.
func (w *Writer) flush(batch []room.Op) {
	var live []room.Op
	for _, op := range batch {
		if !w.broken[room.GameID(op)] {
			live = append(live, op)
		}
	}
	if len(live) == 0 {
		return
	}
	backoff := 50 * time.Millisecond
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := w.write(ctx, live)
		cancel()
		if err == nil {
			metrics.WriterFlushes.Add(1)
			return
		}
		metrics.WriterFailures.Add(1)
		w.log.Error("store: flush failed", "attempt", attempt, "ops", len(live), "err", err)
		if attempt >= w.cfg.MaxAttempts {
			for _, op := range live {
				w.broken[room.GameID(op)] = true
			}
			metrics.WriterDropped.Add(1)
			w.log.Error("store: dropped writes; affected games can't be recovered", "ops", len(live))
			return
		}
		select {
		case <-time.After(backoff):
		case <-w.stop:
			if attempt >= 2 {
				return // shutting down and the database is unreachable
			}
		}
		backoff = min(2*backoff, 2*time.Second)
	}
}

type seatKey struct{ game, player string }

// write applies ops in dependency order within one transaction. Seats and
// projections are latest-wins, so only the last of each is written.
func (w *Writer) write(ctx context.Context, ops []room.Op) error {
	var (
		creates  []room.OpCreateGame
		epochs   = map[string]string{}
		events   []db.InsertEventsParams
		touched  = map[string]time.Time{}
		seats    = map[seatKey]room.SeatRecord{}
		projs    = map[string]room.Projection{}
		finishes []room.OpFinish
	)
	for _, op := range ops {
		switch o := op.(type) {
		case room.OpCreateGame:
			creates = append(creates, o)
		case room.OpEpoch:
			epochs[o.GameID] = o.Epoch
		case room.OpEvents:
			for _, e := range o.Events {
				events = append(events, db.InsertEventsParams{GameID: o.GameID, Seq: int32(e.Seq), Kind: e.Kind, At: ts(e.At), Data: e.Data}) //nolint:gosec // seq is a per-game counter, far below 2^31
			}
			touched[o.GameID] = o.At
		case room.OpSeat:
			seats[seatKey{o.GameID, o.Seat.PlayerID}] = o.Seat
		case room.OpProjection:
			projs[o.GameID] = o.View
		case room.OpFinish:
			finishes = append(finishes, o)
		}
	}

	return pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		for _, c := range creates {
			if err := q.CreateGame(ctx, db.CreateGameParams{ID: c.GameID, Code: c.Code, Epoch: c.Epoch, ModeratorPasswordHash: c.PasswordHash, Rules: c.Rules, CreatedAt: ts(c.At)}); err != nil {
				return err
			}
		}
		for id, epoch := range epochs {
			if err := q.SetGameEpoch(ctx, db.SetGameEpochParams{ID: id, Epoch: epoch}); err != nil {
				return err
			}
		}
		if len(events) > 0 {
			if _, err := q.InsertEvents(ctx, events); err != nil {
				return err
			}
		}
		for id, at := range touched {
			if err := q.TouchGame(ctx, db.TouchGameParams{ID: id, LastActiveAt: ts(at)}); err != nil {
				return err
			}
		}
		for k, s := range seats {
			if err := q.UpsertPlayer(ctx, db.UpsertPlayerParams{
				GameID: k.game, PlayerID: s.PlayerID, Role: string(s.Role), Name: s.Name,
				TokenHash: pgtype.Text{String: s.TokenHash, Valid: s.TokenHash != ""}, LastSeq: int64(s.LastSeq), //nolint:gosec // seqs are far below 2^63
			}); err != nil {
				return err
			}
		}
		for id, p := range projs {
			if err := writeProjection(ctx, q, id, p); err != nil {
				return err
			}
		}
		for _, f := range finishes {
			if err := q.FinishGame(ctx, db.FinishGameParams{ID: f.GameID, FinishedAt: ts(f.At)}); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeProjection(ctx context.Context, q *db.Queries, gameID string, p room.Projection) error {
	if err := q.ClearProjection(ctx, gameID); err != nil {
		return err
	}
	if err := q.ClearPlayerTeams(ctx, gameID); err != nil {
		return err
	}
	for _, t := range p.Teams {
		if err := q.InsertTeam(ctx, db.InsertTeamParams{GameID: gameID, TeamID: t.ID, Name: t.Name}); err != nil {
			return err
		}
	}
	for pid, tid := range p.PlayerTeams {
		if err := q.SetPlayerTeam(ctx, db.SetPlayerTeamParams{GameID: gameID, PlayerID: pid, TeamID: pgtype.Text{String: tid, Valid: true}}); err != nil {
			return err
		}
	}
	for _, rec := range p.History {
		n := int32(rec.Number) //nolint:gosec // tossup numbers are small
		if err := q.InsertTossup(ctx, db.InsertTossupParams{GameID: gameID, Number: n, TossupID: rec.TossupID}); err != nil {
			return err
		}
		for _, b := range rec.Buzzes {
			if err := q.InsertBuzz(ctx, db.InsertBuzzParams{
				GameID: gameID, TossupNumber: n, BuzzID: b.ID, PlayerID: b.PlayerID, TeamID: b.TeamID,
				ArrivedMs: b.OffsetMs + b.CorrectionMs, JudgedMs: b.OffsetMs,
				RttMs: pgtype.Float8{Float64: b.RTTMs, Valid: b.RTTMs > 0}, Clamped: b.Clamped, Status: string(b.Status),
			}); err != nil {
				return err
			}
			if result, ok := rulingOf(b); ok {
				if err := q.InsertRuling(ctx, db.InsertRulingParams{GameID: gameID, TossupNumber: n, BuzzID: b.ID, Result: string(result), Points: int32(b.Points)}); err != nil { //nolint:gosec // points are small
					return err
				}
			}
		}
		if rec.Bonus != nil {
			for i, part := range rec.Bonus.Parts {
				points := 0
				if part.Correct || part.BouncebackTeamID != "" {
					points = p.BonusPartPoints
				}
				if err := q.InsertBonusPart(ctx, db.InsertBonusPartParams{
					GameID: gameID, TossupNumber: n, Part: int32(i), TeamID: rec.Bonus.TeamID, Correct: part.Correct, //nolint:gosec // at most MaxBonusParts
					BouncebackTeam: pgtype.Text{String: part.BouncebackTeamID, Valid: part.BouncebackTeamID != ""}, Points: int32(points), //nolint:gosec // points are small
				}); err != nil {
					return err
				}
			}
		}
	}
	for i, pr := range p.Protests {
		if err := q.InsertProtest(ctx, db.InsertProtestParams{GameID: gameID, Ordinal: int32(i + 1), TossupID: pr.TossupID, FiledBy: pr.PlayerID, Note: pr.Note}); err != nil { //nolint:gosec // protest counts are small
			return err
		}
	}
	return nil
}

// rulingOf maps a finished buzz to the moderator's ruling. Lost or pending
// buzzes have none.
func rulingOf(b protocol.BuzzView) (protocol.Ruling, bool) {
	switch b.Status {
	case protocol.BuzzStatusCorrect:
		if b.Power {
			return protocol.RulingPower, true
		}
		return protocol.RulingCorrect, true
	case protocol.BuzzStatusIncorrect:
		return protocol.RulingNeg, true
	case protocol.BuzzStatusNoPenalty:
		return protocol.RulingNoPenalty, true
	}
	return "", false
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
