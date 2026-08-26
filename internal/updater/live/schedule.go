// Package live — cron-джобы ingest'а live-статусов.
//
// Их два, и это принципиально. Дешёвый (live-schedule) выясняет, когда играют
// наши игроки; адаптивный (live-poll, Phase 7) тратит запросы только в эти
// часы. Обоснование: тик cron'а бесплатен, запрос к источнику — нет, а на
// free-тарифе их всего 100 в сутки.
package live

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/livesource"
	"github.com/StVl/tennis-backend/internal/storage"
)

// SourceFactory создаёт источник со счётчиком запросов, привязанным к текущему
// прогону. Фабрика, а не готовый Source: счётчик должен писать в строку
// live_ingest_runs ЭТОГО цикла, а id прогона известен только внутри Update.
type SourceFactory func(onRequest func()) livesource.Source

// ScheduleUpdater — Job A. Отвечает на вопрос «когда играют наши игроки»
// и складывает ответ в live_schedule, откуда Job B читает его бесплатно.
type ScheduleUpdater struct {
	pool   *pgxpool.Pool
	newSrc SourceFactory
	cfg    config.LiveConfig
}

func NewSchedule(pool *pgxpool.Pool, newSrc SourceFactory, cfg config.LiveConfig) *ScheduleUpdater {
	return &ScheduleUpdater{pool: pool, newSrc: newSrc, cfg: cfg}
}

func (u *ScheduleUpdater) Name() string { return "live-schedule" }

func (u *ScheduleUpdater) Update(ctx context.Context) error {
	if !u.cfg.Enabled {
		return nil
	}

	// Свой ключ блокировки: медленное обновление расписания не должно
	// блокировать опрос.
	lock, acquired, err := storage.AcquireLiveLock(ctx, u.pool, storage.LiveLockSchedule)
	if err != nil {
		return fmt.Errorf("acquire schedule lock: %w", err)
	}
	if !acquired {
		slog.Info("live-schedule: another instance holds the lock, skipping")
		return nil
	}
	defer lock.Release(ctx)

	keys, err := storage.TrackedExternalKeys(ctx, u.pool, livesource.SourceName)
	if err != nil {
		return fmt.Errorf("tracked external keys: %w", err)
	}
	if len(keys) == 0 {
		slog.Warn("live-schedule: no tracked players are mapped to the source; " +
			"seed db/live_external_ids.sql")
		return nil
	}

	runID, startedAt, err := storage.StartRun(ctx, u.pool, u.Name(), livesource.SourceName)
	if err != nil {
		return fmt.Errorf("start run: %w", err)
	}

	src := u.newSrc(func() { storage.IncRunRequests(ctx, u.pool, runID) })

	page, fetchErr := src.Fixtures(ctx, keys)

	// Даже при ошибке пишем то, что успели набрать: лишняя фикстура только
	// расширяет окно наблюдения, а пустое расписание уложило бы Job B спать
	// на восемь часов без единой ошибки в логах. Данные и статус прогона
	// расходятся намеренно — не «исправлять».
	rows := make([]storage.ScheduleRow, 0, len(page.Fixtures))
	for _, f := range page.Fixtures {
		rows = append(rows, storage.ScheduleRow{
			ExternalKey:   f.ExternalKey,
			TournamentKey: f.TournamentKey,
			RoundCode:     f.RoundCode,
			Tournament:    f.Tournament,
			ScheduledAt:   f.ScheduledAt,
			PlayerKeys:    f.PlayerKeys[:],
		})
	}

	// Чистим только заведомо прошедшее. Не «всё, чего не было в этом прогоне»:
	// выдача upcoming теряет матч в момент выхода на корт, и такая чистка
	// стёрла бы окно ровно у идущего матча.
	keepBefore := startedAt.Add(-2 * u.cfg.WatchTail)
	upserted, pruned, err := storage.UpsertSchedule(
		ctx, u.pool, livesource.SourceName, rows, startedAt, keepBefore)

	result := storage.RunResult{
		RowsParsed:  intPtr(page.RowsParsed),
		RowsInScope: intPtr(len(page.Fixtures)),
	}
	switch {
	case fetchErr != nil:
		result.Error = fetchErr.Error()
	case err != nil:
		result.Error = err.Error()
	}
	if finishErr := storage.FinishRun(ctx, u.pool, runID, result); finishErr != nil {
		slog.Error("live-schedule: failed to close the run row", "error", finishErr)
	}

	if err != nil {
		return fmt.Errorf("upsert schedule: %w", err)
	}
	if fetchErr != nil {
		return fmt.Errorf("fetch fixtures: %w", fetchErr)
	}

	withoutTime := 0
	for _, f := range page.Fixtures {
		if f.ScheduledAt == nil {
			withoutTime++
		}
	}
	slog.Info("live-schedule: updated",
		"players", len(keys), "rows_parsed", page.RowsParsed,
		"upserted", upserted, "pruned", pruned,
		"doubles", page.RowsDoubles, "cancelled", page.RowsCancelled,
		"unusable", page.RowsUnusable,
		// Фикстура без времени окна наблюдения не открывает — это видно здесь,
		// а не выясняется потом по отсутствию карточек.
		"without_start_time", withoutTime)
	return nil
}

func intPtr(v int) *int { return &v }

// staleAfter — через сколько отсутствие успешного прогона Job A считается
// протухшим расписанием. Используется Job B в Phase 7.
const staleAfter = 12 * time.Hour
