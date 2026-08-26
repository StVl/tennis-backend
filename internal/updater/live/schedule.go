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

	// Подбираем прогоны, которых никто не закрыл: SIGKILL посреди цикла
	// оставляет finished_at пустым навсегда, а на «последний успешный прогон»
	// опирается STALE-SAFE.
	if swept, err := storage.SweepAbandonedRuns(ctx, u.pool, u.Name(),
		time.Now().Add(-2*u.cfg.UpdateTimeout)); err != nil {
		slog.Error("live-schedule: sweeping abandoned runs failed", "error", err)
	} else if swept > 0 {
		slog.Warn("live-schedule: closed abandoned runs", "count", swept)
	}

	// Свой ключ блокировки: медленное обновление расписания не должно
	// блокировать опрос.
	lock, acquired, err := storage.AcquireLiveLock(ctx, u.pool, storage.LiveLockSchedule)
	if err != nil {
		return fmt.Errorf("acquire schedule lock: %w", err)
	}
	if !acquired {
		// Оставляем след: заклинившая блокировка иначе выглядит ровно как
		// здоровый пропуск, а live_ingest_runs — единственное окно в причины.
		u.recordSkip(ctx, "lock_held")
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
	//
	// Фильтр по своим игрокам избыточен, пока вендор honours повторяемый
	// player=, и ровно поэтому он дешёвая страховка. Если фильтр однажды
	// перестанет применяться (переименование параметра, Set вместо Add,
	// изменение на стороне вендора), без этой проверки Job A принял бы за
	// «наше расписание» весь борт upcoming — 309 строк в снятом срезе — и
	// Job B считал бы, что смотреть надо всегда: вечный WATCHING, квота
	// выедается каждый день, и ни одной ошибки нигде.
	tracked := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		tracked[k] = struct{}{}
	}

	rows := make([]storage.ScheduleRow, 0, len(page.Fixtures))
	foreign := 0
	for _, f := range page.Fixtures {
		if _, ok := tracked[f.PlayerKeys[0]]; !ok {
			if _, ok := tracked[f.PlayerKeys[1]]; !ok {
				foreign++
				continue
			}
		}
		rows = append(rows, storage.ScheduleRow{
			ExternalKey:   f.ExternalKey,
			TournamentKey: f.TournamentKey,
			RoundCode:     f.RoundCode,
			Tournament:    f.Tournament,
			ScheduledAt:   f.ScheduledAt,
			PlayerKeys:    f.PlayerKeys[:],
		})
	}
	if foreign > 0 {
		slog.Warn("live-schedule: fixtures without a tracked player were dropped; "+
			"the vendor's player= filter may have stopped applying",
			"dropped", foreign, "kept", len(rows))
	}

	// Чистим только заведомо прошедшее. Не «всё, чего не было в этом прогоне»:
	// выдача upcoming теряет матч в момент выхода на корт, и такая чистка
	// стёрла бы окно ровно у идущего матча.
	keepBefore := startedAt.Add(-2 * u.cfg.WatchTail)
	staleRefresh := startedAt.Add(-48 * time.Hour)
	upserted, pruned, err := storage.UpsertSchedule(
		ctx, u.pool, livesource.SourceName, rows, startedAt, keepBefore, staleRefresh)

	result := storage.RunResult{
		RowsParsed:            intPtr(page.RowsParsed),
		RowsInScope:           intPtr(len(rows)),
		RowsDroppedUnresolved: intPtr(foreign),
		Mode:                  "refresh",
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

	// Создание строк matches из фикстур. По умолчанию выключено: это
	// ослабление правила 3 из docs/live-status-ingest.md, и решение за
	// iOS-стороной. Ошибка здесь не валит прогон — расписание уже записано,
	// а оно и есть основная задача Job A.
	if u.cfg.CreateMatches {
		if err := u.createMatches(ctx, page.Fixtures, startedAt); err != nil {
			slog.Error("live-schedule: creating matches from fixtures failed", "error", err)
		}
	}

	// Журнал прогонов растёт быстрее всех: тик раз в пять минут это около 288
	// строк в сутки, почти все — «спим». Горизонт заведомо длиннее любого
	// разумного простоя: на «предыдущий успешный прогон» опирается защита от
	// обвала, а на порядок id — счётчик пропусков.
	if purged, err := storage.PruneRuns(ctx, u.pool,
		startedAt.Add(-runRetention)); err != nil {
		slog.Error("live-schedule: pruning the run log failed", "error", err)
	} else if purged > 0 {
		slog.Info("live-schedule: pruned old run rows", "count", purged)
	}

	withoutTime := 0
	for _, f := range rows {
		if f.ScheduledAt == nil {
			withoutTime++
		}
	}
	slog.Info("live-schedule: updated",
		"players", len(keys), "rows_parsed", page.RowsParsed,
		"upserted", upserted, "pruned", pruned, "foreign", foreign,
		"doubles", page.RowsDoubles, "cancelled", page.RowsCancelled,
		"unusable", page.RowsUnusable,
		// Фикстура без времени окна наблюдения не открывает — это видно здесь,
		// а не выясняется потом по отсутствию карточек.
		"without_start_time", withoutTime)
	return nil
}

// createMatches превращает фикстуры в строки matches со статусом scheduled.
//
// Это единственное место, где сервис ДОБАВЛЯЕТ строки в matches, а не меняет
// один столбец. Каждая проверка ниже отделяет «можно создать» от «угадали бы»,
// и ни одна не необязательна.
func (u *ScheduleUpdater) createMatches(ctx context.Context,
	fixtures []livesource.Fixture, now time.Time) error {

	editionKeys := make([]string, 0, len(fixtures))
	playerKeys := make([]string, 0, len(fixtures)*2)
	for _, f := range fixtures {
		if f.TournamentKey != "" {
			editionKeys = append(editionKeys, f.TournamentKey)
		}
		playerKeys = append(playerKeys, f.PlayerKeys[0], f.PlayerKeys[1])
	}
	editions, err := storage.ResolveEditions(ctx, u.pool, livesource.SourceName, editionKeys)
	if err != nil {
		return fmt.Errorf("resolve editions: %w", err)
	}
	players, err := storage.ResolvePlayerKeys(ctx, u.pool, livesource.SourceName, playerKeys)
	if err != nil {
		return fmt.Errorf("resolve players: %w", err)
	}

	var created, exists, unknownRound, skipped int
	for _, f := range fixtures {
		// Квалификация: наши розыгрыши — основные сетки, и строки квалификации
		// в них попадать не должны. В снятом срезе это 84 строки из 200, то
		// есть случай массовый, а не краевой.
		if f.IsQualifying {
			skipped++
			continue
		}
		// Без времени начала строка бесполезна: она не откроет окна наблюдения
		// и не найдётся поиском по окну вокруг времени фикстуры.
		if f.ScheduledAt == nil {
			skipped++
			continue
		}
		editionID, ok := editions[f.TournamentKey]
		if !ok {
			// Именно ЭТА очередь и пополняет db/live_edition_ids.sql.
			// Угадывать розыгрыш нельзя: неверная догадка создаёт матч в
			// чужой сетке.
			_ = storage.RecordUnmatched(ctx, u.pool, livesource.SourceName,
				storage.UnmatchedRow{
					ExternalKey: f.ExternalKey,
					PlayerKeys:  []string{f.PlayerKeys[0], f.PlayerKeys[1]},
					RoundCode:   f.RoundCode,
				}, "edition_unmapped", now)
			skipped++
			continue
		}
		p1, ok1 := players[f.PlayerKeys[0]]
		p2, ok2 := players[f.PlayerKeys[1]]
		if !ok1 || !ok2 {
			// Создаём только то, где известны ОБА игрока: матч с одним
			// участником невидим для поиска по игрокам.
			skipped++
			continue
		}

		_, outcome, err := storage.CreateMatchFromFixture(ctx, u.pool, storage.MatchDraft{
			EditionID:   editionID,
			RoundCode:   f.RoundCode,
			ScheduledAt: *f.ScheduledAt,
			PlayerIDs:   [2]int64{p1, p2},
			ExternalKey: f.ExternalKey,
		})
		if err != nil {
			return fmt.Errorf("create match %s: %w", f.ExternalKey, err)
		}
		switch outcome {
		case storage.CreateDone:
			created++
		case storage.CreateExists:
			exists++
		case storage.CreateUnknownRound:
			unknownRound++
			_ = storage.RecordUnmatched(ctx, u.pool, livesource.SourceName,
				storage.UnmatchedRow{
					ExternalKey: f.ExternalKey,
					PlayerKeys:  []string{f.PlayerKeys[0], f.PlayerKeys[1]},
					RoundCode:   f.RoundCode,
				}, "round_unmapped", now)
		}
	}

	slog.Info("live-schedule: matches from fixtures",
		"created", created, "already_existed", exists,
		"unknown_round", unknownRound, "skipped", skipped)
	return nil
}

// recordSkip оставляет строку прогона для тика, который ничего не потратил.
func (u *ScheduleUpdater) recordSkip(ctx context.Context, reason string) {
	runID, _, err := storage.StartRun(ctx, u.pool, u.Name(), livesource.SourceName)
	if err != nil {
		slog.Error("live-schedule: failed to record a skipped run", "error", err)
		return
	}
	if err := storage.FinishRun(ctx, u.pool, runID,
		storage.RunResult{SkippedReason: reason}); err != nil {
		slog.Error("live-schedule: failed to close a skipped run", "error", err)
	}
}

func intPtr(v int) *int { return &v }

// staleAfter — через сколько отсутствие успешного прогона Job A считается
// протухшим расписанием.
const staleAfter = 12 * time.Hour

// runRetention — сколько держим журнал прогонов.
const runRetention = 14 * 24 * time.Hour
