package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/StVl/tennis-backend/internal/updater"
)

type Job struct {
	Schedule string
	Updater  updater.Updater
}

type Scheduler struct {
	cron          *cron.Cron
	updateTimeout time.Duration
}

func New(updateTimeout time.Duration) *Scheduler {
	return &Scheduler{
		cron:          cron.New(),
		updateTimeout: updateTimeout,
	}
}

func (s *Scheduler) Register(jobs []Job) error {
	for _, job := range jobs {
		currentJob := job
		_, err := s.cron.AddFunc(currentJob.Schedule, func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.updateTimeout)
			defer cancel()

			slog.Info("starting scheduled update", "updater", currentJob.Updater.Name())
			if err := currentJob.Updater.Update(ctx); err != nil {
				slog.Error("scheduled update failed", "updater", currentJob.Updater.Name(), "error", err)
				return
			}
			slog.Info("scheduled update finished", "updater", currentJob.Updater.Name())
		})
		if err != nil {
			return fmt.Errorf("register job %s: %w", currentJob.Updater.Name(), err)
		}
	}

	return nil
}

func (s *Scheduler) Start() {
	s.cron.Start()
}

func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}
