// Command server runs the QBuzzer server: API, WebSockets, and frontend in
// one binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kevinw93/QBuzzer/internal/game"
	"github.com/kevinw93/QBuzzer/internal/httpapi"
	"github.com/kevinw93/QBuzzer/internal/limit"
	"github.com/kevinw93/QBuzzer/internal/metrics"
	"github.com/kevinw93/QBuzzer/internal/room"
	"github.com/kevinw93/QBuzzer/internal/store"
	"github.com/kevinw93/QBuzzer/web"
)

// recoverWindow is how recently a game must have been active to be restored
// after a restart; older unfinished games are finished instead.
const recoverWindow = 2 * time.Hour

func main() {
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if os.Getenv("QBUZZER_LOG_FORMAT") == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)
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
		metrics.WriterQueue = writer.QueueLen
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

	httpOpts := httpapi.Options{ClientIPHeader: os.Getenv("QBUZZER_CLIENT_IP_HEADER")}
	if os.Getenv("QBUZZER_RATE_LIMITS") != "off" {
		// Per client IP. Joins must absorb a whole school network reconnecting
		// at once; each create costs an argon2id hash.
		httpOpts.CreateLimit = limit.New(10, 10)
		httpOpts.JoinLimit = limit.New(120, 60)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(hub, web.Assets(), log, httpOpts),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		// No ReadTimeout/WriteTimeout: they would cap WebSocket lifetime.
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()

	// pprof: loopback only.
	var debugSrv *http.Server
	if daddr := os.Getenv("QBUZZER_DEBUG_ADDR"); daddr != "" {
		if err := httpapi.CheckLoopback(daddr); err != nil {
			return fmt.Errorf("QBUZZER_DEBUG_ADDR=%s: %w", daddr, err)
		}
		debugSrv = &http.Server{Addr: daddr, Handler: httpapi.NewDebug(hub), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Info("debug listening", "addr", daddr)
			if err := debugSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("debug listener", "err", err)
			}
		}()
	}

	// May bind a private network address, never a publicly routed one.
	var metricsSrv *http.Server
	if maddr := os.Getenv("QBUZZER_METRICS_ADDR"); maddr != "" {
		metricsSrv = &http.Server{Addr: maddr, Handler: httpapi.NewMetrics(hub), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			log.Info("metrics listening", "addr", maddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics listener", "err", err)
			}
		}()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down")
	// The writer closes last: rooms enqueue until they have stopped.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if debugSrv != nil {
		_ = debugSrv.Shutdown(shutdownCtx)
	}
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	stopRooms()
	hub.Wait()
	if writer != nil {
		if werr := writer.Close(shutdownCtx); werr != nil {
			log.Error("writer did not flush before the shutdown deadline", "err", werr)
		}
	}
	return err
}
