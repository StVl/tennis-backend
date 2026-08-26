package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ключи advisory-блокировок. Пространство своё, чтобы не столкнуться с
// пайплайном в той же базе; у каждого джоба свой ключ, иначе медленное
// обновление расписания блокировало бы опрос.
const (
	liveLockClass    = 0x4C56 // "LV"
	LiveLockSchedule = 1
	LiveLockPoll     = 2
)

// LiveLock — захваченная advisory-блокировка вместе с соединением, на котором
// она живёт.
//
// Метод Release — сознательное исключение из «никаких методов на структурах»:
// это хендл ресурса, а не сервис. Блокировка живёт В СЕССИИ, поэтому её нельзя
// брать через пул: pool.Exec возвращает соединение в пул сразу, и unlock уйдёт
// в другую сессию, вернёт false, а блокировка протечёт до перезапуска процесса.
// Внешне это выглядит как «джоб иногда молча перестаёт работать на час».
type LiveLock struct {
	conn *pgxpool.Conn
	key  int
}

// AcquireLiveLock берёт выделенное соединение и на нём — try-блокировку.
// Именно try, а не блокирующая: второй инстанс должен уйти, а не встать в
// очередь и опросить источник сразу после первого.
func AcquireLiveLock(ctx context.Context, pool *pgxpool.Pool, key int) (*LiveLock, bool, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx,
		`select pg_try_advisory_lock($1, $2)`, liveLockClass, key).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	return &LiveLock{conn: conn, key: key}, true, nil
}

// Release снимает блокировку и возвращает соединение.
//
// Контекст ОТВЯЗАН от контекста цикла намеренно. При срабатывании таймаута
// отложенный unlock иначе выполнялся бы на уже отменённом контексте: pgx вернёт
// ошибку, не тронув сеть, а Release() уничтожает соединение только закрытое,
// занятое или в транзакции — сессия с висящей блокировкой ни то, ни другое.
// Соединение вернулось бы в пул всё ещё заблокированным, и следующий тик
// получил бы «занято» — неотличимо от честного «работает другой инстанс».
func (l *LiveLock) Release(ctx context.Context) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	var released bool
	err := l.conn.QueryRow(unlockCtx,
		`select pg_advisory_unlock($1, $2)`, liveLockClass, l.key).Scan(&released)
	if err != nil || !released {
		slog.Warn("advisory unlock failed, destroying connection so the lock dies with the session",
			"key", l.key, "error", err, "released", released)
		// В пул такое соединение возвращать нельзя: блокировка переживёт нас.
		if raw := l.conn.Hijack(); raw != nil {
			_ = raw.Close(unlockCtx)
		}
		return
	}
	l.conn.Release()
}

// RunResult — итоги прогона. Ноль полей обязательных: прогон, который упал на
// половине, всё равно должен оставить строку.
type RunResult struct {
	RowsParsed            *int
	RowsInScope           *int
	RowsMatched           *int
	RowsDroppedUnresolved *int
	Mode                  string
	SkippedReason         string
	Error                 string
}

// StartRun открывает строку прогона в автокоммите и возвращает её id и время
// старта.
//
// started_at — ЕДИНСТВЕННОЕ обращение к часам за цикл: все окна и границы
// считаются от него, и ни один SQL этой фичи не вызывает now(). Так цикл
// становится воспроизводимым от сохранённого борта и метки времени, а
// расхождение часов Go и Postgres перестаёт что-либо значить.
func StartRun(ctx context.Context, pool *pgxpool.Pool, job, source string) (int64, time.Time, error) {
	var (
		id        int64
		startedAt time.Time
	)
	err := pool.QueryRow(ctx, `
		insert into live_ingest_runs (job, source, started_at)
		values ($1, $2, now())
		returning id, started_at`,
		job, source).Scan(&id, &startedAt)
	return id, startedAt, err
}

// IncRunRequests увеличивает счётчик запросов на единицу.
//
// Инкремент по ходу, а не на закрытии прогона: Railway редеплоится на каждый
// push, и цикл, убитый посередине, унёс бы с собой уже потраченные запросы.
// Квота — связывающее ограничение всей фичи, недосчитывать её нельзя.
// Контекст отвязан: запрос вендору уже сделан, и посчитать его надо, даже если
// цикл в этот момент отменяют.
func IncRunRequests(ctx context.Context, pool *pgxpool.Pool, runID int64) {
	incCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(incCtx,
		`update live_ingest_runs set requests_made = requests_made + 1 where id = $1`,
		runID); err != nil {
		slog.Error("failed to count a spent request; quota accounting is now low",
			"run_id", runID, "error", err)
	}
}

// FinishRun закрывает строку прогона.
//
// Контекст ОТВЯЗАН, как и у IncRunRequests, и по той же причине. Сработавший
// таймаут цикла иначе валит именно эту запись, и строка остаётся с пустым
// finished_at навсегда — а на «последний успешный прогон» опирается режим
// STALE-SAFE. Терять то самое единственное окно в причины отказа нельзя.
func FinishRun(ctx context.Context, pool *pgxpool.Pool, runID int64, r RunResult) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(finishCtx, `
		update live_ingest_runs set
			finished_at = now(),
			rows_parsed = $2, rows_in_scope = $3, rows_matched = $4,
			rows_dropped_unresolved = $5,
			mode = nullif($6, ''), skipped_reason = nullif($7, ''), error = nullif($8, '')
		where id = $1`,
		runID, r.RowsParsed, r.RowsInScope, r.RowsMatched, r.RowsDroppedUnresolved,
		r.Mode, r.SkippedReason, r.Error)
	return err
}

// SweepAbandonedRuns закрывает прогоны, которые никто не закрыл: SIGKILL
// посреди цикла оставляет finished_at пустым навсегда, а на «последний
// успешный прогон» опирается режим STALE-SAFE.
func SweepAbandonedRuns(ctx context.Context, pool *pgxpool.Pool, job string,
	olderThan time.Time) (int64, error) {
	tag, err := pool.Exec(ctx, `
		update live_ingest_runs set finished_at = now(), error = 'abandoned'
		where job = $1 and finished_at is null and started_at < $2`,
		job, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// TrackedExternalKeys — id отслеживаемых игроков у источника. Основа Job A:
// 101 ключ укладывается в 3 запроса по 50.
func TrackedExternalKeys(ctx context.Context, pool *pgxpool.Pool, source string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		select e.external_key
		from external_ids e
		join players p on p.id = e.entity_id
		where e.source = $1 and e.entity_type = 'player' and p.is_tracked
		order by e.external_key`,
		source)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var s string
		err := row.Scan(&s)
		return s, err
	})
}

// ScheduleRow — строка расписания для upsert'а.
type ScheduleRow struct {
	ExternalKey   string
	TournamentKey string
	RoundCode     string
	Tournament    string
	ScheduledAt   *time.Time
	PlayerKeys    []string
}

// UpsertSchedule обновляет расписание и подчищает прошлое в ОДНОЙ транзакции.
//
// Никакого delete-then-insert: если часть батчей не доехала, расписание
// оказалось бы пустым, и Job B ушёл бы спать на восемь часов — без единой
// ошибки в логах.
//
// Чистка только по времени и НЕ «всё, чего не было в этом прогоне». Выдача
// upcoming теряет матч в момент, когда он выходит на корт, поэтому чистка по
// поколению стёрла бы окно наблюдения ровно у идущего матча.
func UpsertSchedule(ctx context.Context, pool *pgxpool.Pool, source string,
	rows []ScheduleRow, at time.Time, keepBefore, staleRefresh time.Time) (int, int64, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)

	batch := &pgx.Batch{}
	for _, r := range rows {
		batch.Queue(`
			insert into live_schedule (source, external_key, tournament_key, scheduled_at,
			                           player_keys, round_code, tournament, refreshed_at)
			values ($1, $2, nullif($3,''), $4, $5, nullif($6,''), nullif($7,''), $8)
			on conflict (source, external_key) do update set
				tournament_key = excluded.tournament_key,
				scheduled_at   = excluded.scheduled_at,
				player_keys    = excluded.player_keys,
				round_code     = excluded.round_code,
				tournament     = excluded.tournament,
				refreshed_at   = excluded.refreshed_at`,
			source, r.ExternalKey, r.TournamentKey, r.ScheduledAt,
			r.PlayerKeys, r.RoundCode, r.Tournament, at)
	}
	if batch.Len() > 0 {
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return 0, 0, err
		}
	}

	// Две ветки чистки, потому что нельзя удалить одну конкретную строку:
	// матч, который вышел на корт, ИСЧЕЗАЕТ из выдачи upcoming, и у него есть
	// scheduled_at. Поэтому по времени матча чистим только заведомо прошедшее.
	// Фикстуры без scheduled_at этим случаем быть не могут (окна они не
	// открывают вовсе), поэтому их чистим по времени обновления — иначе они
	// копились бы вечно.
	tag, err := tx.Exec(ctx, `
		delete from live_schedule
		where source = $1
		  and ((scheduled_at is not null and scheduled_at < $2)
		    or (scheduled_at is null and refreshed_at < $3))`,
		source, keepBefore, staleRefresh)
	if err != nil {
		return 0, 0, err
	}
	return len(rows), tag.RowsAffected(), tx.Commit(ctx)
}

// LastSuccessfulRun — время закрытия последнего успешного прогона джоба.
// nil означает «ни одного»: для STALE-SAFE это по определению «протухло»,
// а не «свежо», иначе на первом запуске отказа «наружу» не было бы вовсе.
func LastSuccessfulRun(ctx context.Context, pool *pgxpool.Pool, job string) (*time.Time, error) {
	var at *time.Time
	err := pool.QueryRow(ctx, `
		select max(finished_at) from live_ingest_runs
		where job = $1 and error is null and skipped_reason is null
		  and finished_at is not null and requests_made > 0`,
		job).Scan(&at)
	return at, err
}
