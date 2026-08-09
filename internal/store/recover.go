package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinw93/QBuzzer/internal/protocol"
	"github.com/kevinw93/QBuzzer/internal/room"
	"github.com/kevinw93/QBuzzer/internal/store/db"
)

// RestoreFunc rebuilds a game from its stored rules and log.
type RestoreFunc func(rules []byte, log []room.GameEvent) (room.Game, error)

// Recover restarts every unfinished game active since cutoff and finishes
// older ones (so their codes can be reused). A game whose log can't be
// replayed is finished and skipped rather than restored wrong. It returns how
// many rooms were restored.
func Recover(ctx context.Context, pool *pgxpool.Pool, hub *room.Hub, restore RestoreFunc, cutoff time.Time, log *slog.Logger) (int, error) {
	q := db.New(pool)
	if n, err := q.FinishStaleGames(ctx, ts(cutoff)); err != nil {
		return 0, fmt.Errorf("store: finishing stale games: %w", err)
	} else if n > 0 {
		log.Info("store: finished stale games", "count", n)
	}
	games, err := q.RecoverableGames(ctx, ts(cutoff))
	if err != nil {
		return 0, fmt.Errorf("store: listing games: %w", err)
	}
	restored := 0
	for _, g := range games {
		rs, err := load(ctx, q, g, restore)
		if err == nil {
			_, err = hub.Restore(rs)
		}
		if err != nil {
			log.Error("store: can't recover game; finishing it", "code", g.Code, "game", g.ID, "err", err)
			if ferr := q.FinishGame(ctx, db.FinishGameParams{ID: g.ID, FinishedAt: ts(time.Now())}); ferr != nil {
				return restored, ferr
			}
			continue
		}
		restored++
	}
	return restored, nil
}

func load(ctx context.Context, q *db.Queries, g db.RecoverableGamesRow, restore RestoreFunc) (room.Restored, error) {
	rows, err := q.GameEvents(ctx, g.ID)
	if err != nil {
		return room.Restored{}, err
	}
	log := make([]room.GameEvent, len(rows))
	for i, r := range rows {
		log[i] = room.GameEvent{Seq: int(r.Seq), Kind: r.Kind, At: r.At.Time, Data: r.Data}
	}
	game, err := restore(g.Rules, log)
	if err != nil {
		return room.Restored{}, err
	}
	players, err := q.GamePlayers(ctx, g.ID)
	if err != nil {
		return room.Restored{}, err
	}
	seats := make([]room.SeatRecord, len(players))
	for i, p := range players {
		seats[i] = room.SeatRecord{PlayerID: p.PlayerID, Role: protocol.Role(p.Role), Name: p.Name, TokenHash: p.TokenHash.String, LastSeq: uint64(p.LastSeq)} //nolint:gosec // stored from a uint64
	}
	return room.Restored{GameID: g.ID, Code: g.Code, PasswordHash: g.ModeratorPasswordHash, Seats: seats, Game: game}, nil
}
