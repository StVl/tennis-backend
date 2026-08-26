package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/StVl/tennis-backend/api"
	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/scheduler"
	"github.com/StVl/tennis-backend/internal/storage"
	"github.com/StVl/tennis-backend/internal/updater/players"
	"github.com/StVl/tennis-backend/internal/updater/tournaments"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()

	pool, err := storage.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	tournamentsUpdater := tournaments.New(pool)
	playersUpdater := players.New(pool)

	jobScheduler := scheduler.New(cfg.UpdateTimeout)
	if err := jobScheduler.Register([]scheduler.Job{
		{Schedule: cfg.TournamentsCron, Updater: tournamentsUpdater},
		{Schedule: cfg.PlayersCron, Updater: playersUpdater},
	}); err != nil {
		return err
	}

	jobScheduler.Start()
	defer func() {
		stopCtx := jobScheduler.Stop()
		<-stopCtx.Done()
	}()

	server := &http.Server{
		Addr: ":" + cfg.HTTPPort,
		Handler: api.NewRouter(pool, api.HandlerConfig{
			DevEndpoints:     cfg.DevEndpoints,
			LiveMatchesLimit: cfg.LiveMatchesLimit,
		}),
	}

	serverErr := make(chan error, 1)
	go func() {
		slog.Info("starting http server", "port", cfg.HTTPPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	shutdownSignal := make(chan os.Signal, 1)
	signal.Notify(shutdownSignal, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		return fmt.Errorf("http server: %w", err)
	case sig := <-shutdownSignal:
		slog.Info("shutdown signal received", "signal", sig.String())
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}

	slog.Info("application stopped")
	return nil
}
