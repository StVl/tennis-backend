package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/livesource"
	"github.com/StVl/tennis-backend/internal/storage"
)

// errFlipRefused — guard не пропустил подъём. Не ошибка цикла: строка просто
// не в том состоянии, и цикл продолжается.
var errFlipRefused = errors.New("flip refused by guard")

// PollUpdater — Job B. Тикает часто, тратит редко.
type PollUpdater struct {
	pool   *pgxpool.Pool
	newSrc SourceFactory
	cfg    config.LiveConfig
}

func NewPoll(pool *pgxpool.Pool, newSrc SourceFactory, cfg config.LiveConfig) *PollUpdater {
	return &PollUpdater{pool: pool, newSrc: newSrc, cfg: cfg}
}

func (u *PollUpdater) Name() string { return "live" }

func (u *PollUpdater) Update(ctx context.Context) error {
	// Одно обращение к часам за цикл, и именно к часам Postgres. Всё, с чем
	// это время сравнивается — last_poll_at, границы суток квоты, flipped_at —
	// пришло из БД, а Postgres на Railway отдельный сервис: смешение часов
	// перекашивает и интервал опроса, и границу суток.
	now, err := storage.DBNow(ctx, u.pool)
	if err != nil {
		return fmt.Errorf("read db clock: %w", err)
	}

	// Шаги 1 и 2 идут ДО проверки рубильника и до решения об опросе.
	// Это не стилистика: и то и другое гасит карточки, и оба обязаны работать,
	// когда опроса нет вовсе — кончилась квота, лёг источник, выключили
	// рубильник. Иначе ложная карточка висит у всех подписчиков до правки БД.
	if err := u.reconcile(ctx, now); err != nil {
		slog.Error("live: reconcile failed", "error", err)
	}

	if !u.cfg.Enabled {
		return nil
	}

	if swept, err := storage.SweepAbandonedRuns(ctx, u.pool, u.Name(),
		now.Add(-2*u.cfg.UpdateTimeout)); err != nil {
		slog.Error("live: sweeping abandoned runs failed", "error", err)
	} else if swept > 0 {
		slog.Warn("live: closed abandoned runs", "count", swept)
	}

	lock, acquired, err := storage.AcquireLiveLock(ctx, u.pool, storage.LiveLockPoll)
	if err != nil {
		return fmt.Errorf("acquire poll lock: %w", err)
	}
	if !acquired {
		u.recordSkip(ctx, "", "lock_held")
		return nil
	}
	defer lock.Release(ctx)

	snap, err := storage.LoadLiveSnapshot(ctx, u.pool, livesource.SourceName,
		now, u.cfg.WatchLead, u.cfg.WatchTail)
	if err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}

	decision := Decide(fromStorageSnapshot(snap), u.cfg, now)
	if !decision.Poll {
		// Режим пишется на КАЖДОМ тике, включая пропуск: live_ingest_runs —
		// единственное окно в то, почему карточка не появилась.
		u.recordSkip(ctx, string(decision.Mode), decision.Reason)
		return nil
	}

	runID, _, err := storage.StartRun(ctx, u.pool, u.Name(), livesource.SourceName)
	if err != nil {
		return fmt.Errorf("start run: %w", err)
	}
	prevInScope, err := storage.PrevSuccessfulInScope(ctx, u.pool)
	if err != nil {
		slog.Error("live: cannot read the previous cycle's in-scope count; "+
			"the collapse guard is blind this cycle", "error", err)
	}
	src := u.newSrc(func() { storage.IncRunRequests(ctx, u.pool, runID) })

	board, fetchErr := src.PollLive(ctx)
	if fetchErr != nil {
		// Ни наблюдений, ни прохода по пропускам: сбой сети не должен
		// выглядеть как «все матчи кончились».
		_ = storage.FinishRun(ctx, u.pool, runID, storage.RunResult{
			Mode: string(decision.Mode), Error: fetchErr.Error(),
		})
		return fmt.Errorf("poll live: %w", fetchErr)
	}

	res, err := Ingest(ctx, u.pool, board, IngestParams{
		RunID:       runID,
		Now:         now,
		MatchWindow: u.cfg.MatchWindow,
		MaxLiveAge:  u.cfg.MaxLiveAge,
		PrevInScope: prevInScope,
	})

	result := storage.RunResult{
		Mode:                  string(decision.Mode),
		RowsParsed:            intPtr(board.RowsParsed),
		RowsInScope:           intPtr(res.RowsInScope),
		RowsMatched:           intPtr(res.RowsMatched),
		RowsDroppedUnresolved: intPtr(res.RowsDropped),
	}
	if err != nil {
		result.Error = err.Error()
	} else if res.GuardTripped != "" {
		result.Error = res.GuardTripped
	}
	if finishErr := storage.FinishRun(ctx, u.pool, runID, result); finishErr != nil {
		slog.Error("live: failed to close the run row", "error", finishErr)
	}
	if err != nil {
		return err
	}

	slog.Info("live: cycle done", "mode", decision.Mode, "interval", decision.Interval,
		"rows_parsed", board.RowsParsed, "in_scope", res.RowsInScope,
		"matched", res.RowsMatched, "dropped", res.RowsDropped,
		"entered", res.Entered, "left", res.Left, "guard", res.GuardTripped)
	if len(board.UnknownEventStatuses) > 0 {
		slog.Warn("live: vendor sent event_status values we do not map; rows were dropped",
			"values", board.UnknownEventStatuses)
	}
	return nil
}

// reconcile гасит карточки без единого запроса к источнику.
//
//  1. флаг есть, а матч уже не live — пайплайн забрал строку себе;
//  2. матч висит живым дольше допустимого — аварийный выход.
//
// Обе ветки работают при выключенном рубильнике и при исчерпанной квоте,
// потому что стоят до них.
func (u *PollUpdater) reconcile(ctx context.Context, now time.Time) error {
	orphans, err := storage.OrphanFlags(ctx, u.pool, livesource.SourceName)
	if err != nil {
		return err
	}
	for _, id := range orphans {
		if _, _, err := storage.FlipOut(ctx, u.pool, id,
			storage.LiveEventFinished, "reconciled", now); err != nil {
			slog.Error("live: reconciling an orphan flag failed", "match_id", id, "error", err)
			continue
		}
		slog.Warn("live: flag cleared for a match that is no longer live", "match_id", id)
	}

	// Зеркальный случай: матч помечен live, а флага у нас нет. Все остальные
	// страховки читают live_flags и этого не видят вовсе.
	unflagged, err := storage.UnflaggedLiveMatches(ctx, u.pool, livesource.SourceName)
	if err != nil {
		return err
	}
	if len(unflagged) > 0 {
		// Не присваиваем: prior_status пришлось бы выдумать, а неверное
		// восстановление затрёт результат пайплайна. Но и молчать нельзя —
		// это ложная карточка у всех подписчиков обоих игроков.
		slog.Error("live: matches are live with no flag of ours; something wrote "+
			"matches.status outside this service. They will not be swept: restoring "+
			"a status we never recorded would be a guess",
			"match_ids", unflagged, "count", len(unflagged))
	}

	if u.cfg.MaxLiveAge <= 0 {
		return nil
	}
	stale, err := storage.StaleFlags(ctx, u.pool, livesource.SourceName,
		now.Add(-u.cfg.MaxLiveAge))
	if err != nil {
		return err
	}
	for _, id := range stale {
		if _, _, err := storage.FlipOut(ctx, u.pool, id,
			storage.LiveEventFinished, "max_live_age", now); err != nil {
			slog.Error("live: force-exit failed", "match_id", id, "error", err)
			continue
		}
		slog.Warn("live: forced a match out of live on max age",
			"match_id", id, "max_age", u.cfg.MaxLiveAge)
	}
	return nil
}

func (u *PollUpdater) recordSkip(ctx context.Context, mode, reason string) {
	runID, _, err := storage.StartRun(ctx, u.pool, u.Name(), livesource.SourceName)
	if err != nil {
		slog.Error("live: failed to record a skipped run", "error", err)
		return
	}
	if err := storage.FinishRun(ctx, u.pool, runID,
		storage.RunResult{Mode: mode, SkippedReason: reason}); err != nil {
		slog.Error("live: failed to close a skipped run", "error", err)
	}
}

func fromStorageSnapshot(s storage.LiveSnapshot) Snapshot {
	windows := make([]Window, 0, len(s.Windows))
	for _, w := range s.Windows {
		windows = append(windows, Window{From: w.From, To: w.To})
	}
	return Snapshot{
		ActiveMatches:      s.ActiveMatches,
		Windows:            windows,
		RequestsToday:      s.RequestsToday,
		StaleRequestsToday: s.StaleRequestsToday,
		LastPollAt:         s.LastPollAt,
		LastScheduleRunAt:  s.LastScheduleRunAt,
	}
}

// IngestParams — всё, что нужно проходу разбора борта. Отдельно от
// config.LiveConfig, чтобы этот проход мог вызывать и dev-эндпоинт повтора.
type IngestParams struct {
	RunID       int64
	Now         time.Time
	MatchWindow time.Duration
	MaxLiveAge  time.Duration
	// rows_in_scope предыдущего успешного цикла; nil — сравнивать не с чем.
	PrevInScope *int
}

// collapseMinRows — с какого числа наших матчей в предыдущем цикле обвал
// вообще имеет смысл проверять.
//
// Ниже этого порога защита принципиально невозможна, и об этом лучше сказать
// прямо. Когда мы держим один матч, переход in_scope 1 -> 0 выглядит ОДИНАКОВО
// и когда матч честно кончился, и когда источник перестал его показывать.
// Различить их нечем, и именно поэтому выход из live стоит трёх пропусков:
// для единичного матча защитой служит дебаунс, а не эта проверка. Обвал же
// ловится там, где он статистически виден — когда разом исчезает много матчей.
const collapseMinRows = 3

// IngestResult — счётчики цикла.
type IngestResult struct {
	RowsInScope int
	RowsMatched int
	RowsDropped int
	Entered     int
	Left        int
	// Источник считает матч идущим, но наш guard флип не пропустил.
	Refused int
	// Непустое значение означает, что сработала защита: наблюдения не
	// записаны, проход по пропускам не выполнялся.
	GuardTripped string
}

// Ingest — проход по разобранному борту. Вынесен из джоба, чтобы тот же путь
// можно было прогнать из сохранённого борта без единого запроса к источнику.
func Ingest(ctx context.Context, pool *pgxpool.Pool, board livesource.Board,
	p IngestParams) (IngestResult, error) {

	var res IngestResult

	// Защита от пустого борта. Ноль разобранных строк при живом источнике —
	// это отказ, а не результат: гасить по нему карточки нельзя.
	if board.RowsParsed == 0 {
		res.GuardTripped = "zero rows parsed: treating as a failure, not an empty board"
		return res, nil
	}

	held, err := storage.HeldLiveFlags(ctx, pool, livesource.SourceName)
	if err != nil {
		return res, fmt.Errorf("held flags: %w", err)
	}
	byMatch := make(map[int64]storage.LiveFlagRow, len(held))
	for _, f := range held {
		byMatch[f.MatchID] = f
	}

	// Собираем ключи всех участников одним запросом, а не по строке.
	keys := make([]string, 0, len(board.Observations)*2)
	for _, o := range board.Observations {
		keys = append(keys, o.PlayerKeys[0], o.PlayerKeys[1])
	}
	resolved, err := storage.ResolvePlayerKeys(ctx, pool, livesource.SourceName, keys)
	if err != nil {
		return res, fmt.Errorf("resolve players: %w", err)
	}

	seen := map[int64]bool{}

	for _, o := range board.Observations {
		p1, ok1 := resolved[o.PlayerKeys[0]]
		p2, ok2 := resolved[o.PlayerKeys[1]]

		// Ни один игрок не наш — молча отбрасываем. Борт источника это весь
		// мир; складывать такие строки в очередь ревью означало бы сотни строк
		// за цикл и очередь, которую никто не читает.
		if !ok1 && !ok2 {
			res.RowsDropped++
			continue
		}
		res.RowsInScope++

		if !ok1 || !ok2 {
			// Один известен, второй нет: кандидат для ленивого резолвера
			// (Phase 9). Пока — в очередь ревью.
			_ = storage.RecordUnmatched(ctx, pool, livesource.SourceName,
				unmatchedRow(o), "one_side_unresolved", p.Now)
			continue
		}

		anchor := p.Now
		if o.ScheduledAt != nil {
			anchor = *o.ScheduledAt
		}
		matchID, err := storage.FindMatchByPlayers(ctx, pool, p1, p2,
			anchor.Add(-p.MatchWindow), anchor.Add(p.MatchWindow))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_ = storage.RecordUnmatched(ctx, pool, livesource.SourceName,
				unmatchedRow(o), "no_match_row", p.Now)
			continue
		case errors.Is(err, storage.ErrAmbiguousMatch):
			_ = storage.RecordUnmatched(ctx, pool, livesource.SourceName,
				unmatchedRow(o), "ambiguous", p.Now)
			continue
		case err != nil:
			return res, fmt.Errorf("find match: %w", err)
		}

		res.RowsMatched++
		seen[matchID] = true

		if err := storage.AppendObservation(ctx, pool, p.RunID, matchID,
			livesource.SourceName, string(o.State), o.EventStatus, p.Now); err != nil {
			return res, fmt.Errorf("append observation: %w", err)
		}

		flag := byMatch[matchID]
		cur := FlagState{Held: flag.MatchID != 0, State: flag.State, Misses: flag.Misses}
		age := time.Duration(0)
		if cur.Held {
			age = p.Now.Sub(flag.FlippedAt)
		}
		next, action := Derive(cur, Signal{Seen: true, State: o.State}, age, p.MaxLiveAge)
		err = applyAction(ctx, pool, matchID, o.ExternalKey, p.RunID, next, action, true, p.Now)
		if errors.Is(err, errFlipRefused) {
			res.Refused++
			continue
		}
		if err != nil {
			return res, err
		}
		switch action {
		case ActionEnterLive:
			res.Entered++
		case ActionLeaveLive, ActionForceExit:
			res.Left++
		}
	}

	// Защита от обвала. Ноль разобранных строк — это сломанный источник, но
	// борт может остаться полным и при этом разом лишиться ВСЕХ наших матчей:
	// поехавший шард, регрессия пагинации, изменившийся фильтр. По симптомам
	// это неотличимо от «все наши матчи кончились одновременно», а цена ошибки
	// разная: во втором случае карточки и должны погаснуть, в первом — нет.
	//
	// Поэтому сравниваем с предыдущим успешным циклом и срабатываем только на
	// заметном обвале при наличии поднятых карточек. Наблюдения уже записаны
	// (они — факт), но проход по пропускам не выполняется.
	if p.PrevInScope != nil && *p.PrevInScope >= collapseMinRows &&
		len(held) > 0 && res.RowsInScope*2 < *p.PrevInScope {

		res.GuardTripped = fmt.Sprintf(
			"in-scope rows collapsed from %d to %d while holding %d card(s): "+
				"treating as a source failure, not as matches ending",
			*p.PrevInScope, res.RowsInScope, len(held))
		return res, nil
	}

	// Проход по пропускам выполняется ТОЛЬКО после успешного опроса: иначе
	// сбойный цикл посчитал бы отсутствием все живые матчи разом.
	for _, f := range held {
		if seen[f.MatchID] || f.Source == storage.LiveSourceDev {
			// Ручные флипы исключены: синтетического матча в борте источника
			// нет и быть не может, он копил бы пропуски и гас посреди теста.
			continue
		}
		cur := FlagState{Held: true, State: f.State, Misses: f.Misses}
		next, action := Derive(cur, Signal{Seen: false}, p.Now.Sub(f.FlippedAt), p.MaxLiveAge)
		if err := applyAction(ctx, pool, f.MatchID, "", p.RunID, next, action, false, p.Now); err != nil {
			return res, err
		}
		if action == ActionLeaveLive || action == ActionForceExit {
			res.Left++
		}
	}
	return res, nil
}

// applyAction переносит решение в БД.
//
// seen обязателен и отделён от action намеренно. ActionNone означает «ничего не
// меняем», но приходит из ДВУХ разных мест: «матч на месте, всё идёт» и «матча
// в борте нет, копим пропуск». Отметить флаг увиденным во втором случае —
// значит обнулять счётчик пропусков каждый цикл, и карточка не погаснет никогда.
func applyAction(ctx context.Context, pool *pgxpool.Pool, matchID int64,
	externalKey string, runID int64, next FlagState, action Action,
	seen bool, now time.Time) error {

	switch action {
	case ActionEnterLive:
		flip, err := storage.FlipLive(ctx, pool, matchID,
			livesource.SourceName, externalKey, &runID, now)
		if err != nil {
			return fmt.Errorf("flip live: %w", err)
		}
		// Результат разбирается, а не выбрасывается: иначе отказ guard'а
		// (например, у строки уже есть winner_side) выглядит как успешный
		// подъём, Derive запрашивает его снова каждый цикл, и в счётчике
		// entered стоит матч, который на самом деле не поднят.
		switch flip {
		case storage.FlipRefused:
			slog.Warn("live: refused to flip a match the source calls on court; "+
				"it is not scheduled or already has a winner",
				"match_id", matchID, "external_key", externalKey)
			return errFlipRefused
		case storage.FlipAlreadyLive:
			slog.Warn("live: match was already live without a flag of ours",
				"match_id", matchID)
		}
	case ActionLeaveLive:
		if _, _, err := storage.FlipOut(ctx, pool, matchID,
			storage.LiveEventFinished, "derived", now); err != nil {
			return fmt.Errorf("flip out: %w", err)
		}
	case ActionForceExit:
		if _, _, err := storage.FlipOut(ctx, pool, matchID,
			storage.LiveEventFinished, "max_live_age", now); err != nil {
			return fmt.Errorf("force exit: %w", err)
		}
	case ActionSuspend, ActionResume:
		event := storage.LiveEventSuspended
		if action == ActionResume {
			event = storage.LiveEventResumed
		}
		if err := storage.MarkFlagSeen(ctx, pool, matchID, runID, next.State); err != nil {
			return err
		}
		if err := storage.AppendLiveEvent(ctx, pool, matchID, event, action.String(), now); err != nil {
			return err
		}
	case ActionNone:
		if next.Held && seen {
			if err := storage.MarkFlagSeen(ctx, pool, matchID, runID, next.State); err != nil {
				return err
			}
		}
	}
	return nil
}

func unmatchedRow(o livesource.Observation) storage.UnmatchedRow {
	row := storage.UnmatchedRow{
		ExternalKey: o.ExternalKey,
		PlayerKeys:  []string{o.PlayerKeys[0], o.PlayerKeys[1]},
		RoundCode:   o.RoundCode,
		EventStatus: o.EventStatus,
	}
	if o.ScheduledAt != nil {
		row.ScheduledAt = o.ScheduledAt.Format(time.RFC3339)
	}
	return row
}
