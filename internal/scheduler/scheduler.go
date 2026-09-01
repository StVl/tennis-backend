package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/StVl/tennis-backend/internal/updater"
)

type Job struct {
	Schedule string
	Updater  updater.Updater
	// Timeout — таймаут одного прогона именно этого джоба; 0 = общий
	// updateTimeout планировщика. Нужен, потому что общий таймаут (5m по
	// умолчанию) длиннее, чем интервал частых джобов: без своего значения
	// медленный вызов внешнего API перехлёстывал бы следующий тик.
	Timeout time.Duration
}

type Scheduler struct {
	cron          *cron.Cron
	rootCtx       context.Context
	updateTimeout time.Duration
}

// New принимает корневой контекст, от которого наследуются контексты прогонов.
// Его отмена гасит уже идущие прогоны, поэтому cron.Stop() не ждёт полного
// таймаута: иначе Railway на каждом деплое либо ждал бы минуты, либо присылал
// SIGKILL посреди цикла.
func New(rootCtx context.Context, updateTimeout time.Duration) *Scheduler {
	return &Scheduler{
		cron:          cron.New(),
		rootCtx:       rootCtx,
		updateTimeout: updateTimeout,
	}
}

func (s *Scheduler) Register(jobs []Job) error {
	for _, job := range jobs {
		currentJob := job
		timeout := s.updateTimeout
		if currentJob.Timeout > 0 {
			timeout = currentJob.Timeout
		}

		_, err := s.cron.AddFunc(currentJob.Schedule, func() {
			s.runOnce(currentJob.Updater, timeout)
		})
		if err != nil {
			return fmt.Errorf("register job %s: %w", currentJob.Updater.Name(), err)
		}
	}

	return nil
}

// runOnce — один прогон. Вынесен из замыкания cron'а, чтобы его можно было
// проверить тестом: cron без WithSeconds() умеет только минутную гранулярность,
// то есть через планировщик такой тест ждал бы минуту.
func (s *Scheduler) runOnce(u updater.Updater, timeout time.Duration) {
	// cron.New() ставит ПУСТУЮ цепочку: в v3 автоматический Recover убрали, а
	// джоб запускается в голой горутине, у которой единственный defer — учёт
	// в jobWaiter. Без этого паника в разборе чужого JSON уносит весь процесс,
	// а на Railway это цикл перезапусков, причём каждый заново применяет схему.
	// Плохой ответ вендора должен стоить один прогон, а не сервис.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduled update panicked", "updater", u.Name(),
				"panic", r, "stack", string(debug.Stack()))
		}
	}()

	// уже выключаемся — новый прогон не начинаем, чтобы не шуметь в логах
	// ошибками отменённого контекста
	if s.rootCtx.Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.rootCtx, timeout)
	defer cancel()

	slog.Info("starting scheduled update", "updater", u.Name())
	if err := u.Update(ctx); err != nil {
		slog.Error("scheduled update failed", "updater", u.Name(), "error", err)
		return
	}
	slog.Info("scheduled update finished", "updater", u.Name())
}

func (s *Scheduler) Start() {
	s.cron.Start()
}

func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}
