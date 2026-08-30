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

	"github.com/StVl/tennis-backend/api"
	"github.com/StVl/tennis-backend/internal/apns"
	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/livesource"
	"github.com/StVl/tennis-backend/internal/scheduler"
	"github.com/StVl/tennis-backend/internal/storage"
	"github.com/StVl/tennis-backend/internal/updater/live"
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

	// корневой контекст: его отмена гасит идущие прогоны планировщика,
	// иначе cron.Stop() ждёт их полного таймаута на каждом деплое
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	pool, err := storage.NewPool(rootCtx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	tournamentsUpdater := tournaments.New(pool)
	playersUpdater := players.New(pool)

	// Фабрика, а не готовый клиент: счётчик запросов должен писать в строку
	// прогона ЭТОГО цикла, а её id известен только внутри Update.
	newLiveSource := func(onRequest func()) livesource.Source {
		return livesource.NewClient(cfg.Live.BaseURL, cfg.Live.APIKey,
			livesource.WithOnRequest(onRequest))
	}
	liveScheduleUpdater := live.NewSchedule(pool, newLiveSource, cfg.Live)
	livePollUpdater := live.NewPoll(pool, newLiveSource, cfg.Live)

	pushSender, err := newPushSender(cfg.Push)
	if err != nil {
		return err
	}
	livePushUpdater := live.NewPush(pool, pushSender, cfg.Push)

	jobs := []scheduler.Job{
		{Schedule: cfg.TournamentsCron, Updater: tournamentsUpdater},
		{Schedule: cfg.PlayersCron, Updater: playersUpdater},
		{
			Schedule: cfg.Live.ScheduleCron,
			Updater:  liveScheduleUpdater,
			Timeout:  cfg.Live.UpdateTimeout,
		},
		{
			Schedule: cfg.Live.Cron,
			Updater:  livePollUpdater,
			Timeout:  cfg.Live.UpdateTimeout,
		},
		{
			Schedule: cfg.Push.Cron,
			Updater:  livePushUpdater,
			Timeout:  cfg.Push.UpdateTimeout,
		},
	}

	// RUN_ONCE=<job> прогоняет один джоб и выходит. Ни нового бинаря, ни
	// изменений в сборке Railway — и заодно это боевой рычаг «обновить сейчас».
	if only := os.Getenv("RUN_ONCE"); only != "" {
		return runOnce(rootCtx, jobs, only, cfg.UpdateTimeout)
	}

	jobScheduler := scheduler.New(rootCtx, cfg.UpdateTimeout)
	if err := jobScheduler.Register(jobs); err != nil {
		return err
	}

	jobScheduler.Start()
	defer func() {
		// сначала отменяем текущие прогоны, только потом ждём их завершения
		cancelRoot()
		stopCtx := jobScheduler.Stop()
		<-stopCtx.Done()
	}()

	server := &http.Server{
		Addr: ":" + cfg.HTTPPort,
		Handler: api.NewRouter(pool, api.HandlerConfig{
			DevEndpoints:     cfg.DevEndpoints,
			LiveMatchesLimit: cfg.LiveMatchesLimit,
			LiveMatchWindow:  cfg.Live.MatchWindow,
			LiveMaxLiveAge:   cfg.Live.MaxLiveAge,
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

// newPushSender собирает клиент APNs. При выключенном рубильнике возвращает
// заглушку: ключа тогда нет, и требовать его от выключенной фичи незачем.
func newPushSender(cfg config.PushConfig) (live.Sender, error) {
	if !cfg.Enabled {
		return disabledSender{}, nil
	}
	key, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("read APNS_KEY_PATH: %w", err)
	}
	return apns.New(apns.Config{
		KeyID: cfg.KeyID, TeamID: cfg.TeamID, BundleID: cfg.BundleID,
		PrivateKeyPEM: key, Host: cfg.Host,
	})
}

type disabledSender struct{}

func (disabledSender) Send(context.Context, apns.Notification) error {
	return errors.New("push delivery is disabled (PUSH_ENABLED=false)")
}

// runOnce прогоняет один зарегистрированный джоб и возвращается.
func runOnce(ctx context.Context, jobs []scheduler.Job, name string,
	defaultTimeout time.Duration) error {

	for _, job := range jobs {
		if job.Updater.Name() != name {
			continue
		}
		timeout := defaultTimeout
		if job.Timeout > 0 {
			timeout = job.Timeout
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		slog.Info("RUN_ONCE", "updater", name, "timeout", timeout)
		if err := job.Updater.Update(runCtx); err != nil {
			return fmt.Errorf("run once %s: %w", name, err)
		}
		slog.Info("RUN_ONCE finished", "updater", name)
		return nil
	}

	available := make([]string, 0, len(jobs))
	for _, job := range jobs {
		available = append(available, job.Updater.Name())
	}
	return fmt.Errorf("RUN_ONCE=%q: unknown job; available: %v", name, available)
}
