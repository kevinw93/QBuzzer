// Command server runs the QBuzzer server: API, WebSockets, and frontend in
// one binary.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinw93/QBuzzer/internal/game"
	"github.com/kevinw93/QBuzzer/internal/httpapi"
	"github.com/kevinw93/QBuzzer/internal/room"
	"github.com/kevinw93/QBuzzer/internal/store"
	"github.com/kevinw93/QBuzzer/web"
)

// recoverWindow is how recently a game must have been active to be restored
// after a restart; older unfinished games are finished instead.
const recoverWindow = 2 * time.Hour

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	addr := os.Getenv("QBUZZER_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := room.DefaultConfig()
	cfg.NewGame = func() room.Game { return game.New(game.Standard()) }

	// Persistence is optional: without DATABASE_URL games live in memory only.
	var pool *pgxpool.Pool
	var writer *store.Writer
	if url := os.Getenv("DATABASE_URL"); url != "" {
		var err error
		if pool, err = pgxpool.New(ctx, url); err != nil {
			return err
		}
		defer pool.Close()
		if err := store.Migrate(ctx, pool); err != nil {
			return err
		}
		writer = store.NewWriter(pool, log, store.DefaultWriterConfig())
		go writer.Run()
		cfg.Sink = writer
	} else {
		log.Warn("DATABASE_URL not set: games will not survive a restart")
	}

	// Rooms get their own context so shutdown can stop them before the writer.
	roomsCtx, stopRooms := context.WithCancel(context.Background())
	defer stopRooms()
	hub := room.NewHub(roomsCtx, cfg, log)
	if pool != nil {
		restore := func(rules []byte, log []room.GameEvent) (room.Game, error) { return game.Restore(rules, log) }
		n, err := store.Recover(ctx, pool, hub, restore, time.Now().Add(-recoverWindow), log)
		if err != nil {
			return err
		}
		log.Info("recovered games", "count", n)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(hub, web.Assets(), log),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No ReadTimeout/WriteTimeout: they would cap WebSocket lifetime.
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	// Order matters: stop accepting requests, stop the rooms (which close
	// their WebSockets and leave games recoverable), then flush the writer
	// once nothing can enqueue anymore.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	stopRooms()
	hub.Wait()
	if writer != nil {
		if werr := writer.Close(shutdownCtx); werr != nil {
			log.Error("writer did not flush before the shutdown deadline", "err", werr)
		}
	}
	return err
}
