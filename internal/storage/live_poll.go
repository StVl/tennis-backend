package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAmbiguousMatch — под условие подошёл не один матч. Догадка здесь
// запрещена: не тот матч, помеченный live, это ложная карточка и пуш, который
// нельзя отозвать. Та же упорядоченная пара игроков реально встречается в базе
// до трёх раз.
var ErrAmbiguousMatch = errors.New("more than one match fits")

// ResolvePlayerKeys — id игроков источника -> наши id. Один индексный проход
// по первичному ключу external_ids; таблица players при этом не трогается.
// Имена в разрешении не участвуют вообще.
func ResolvePlayerKeys(ctx context.Context, pool *pgxpool.Pool, source string,
	keys []string) (map[string]int64, error) {

	rows, err := pool.Query(ctx, `
		select external_key, entity_id from external_ids
		where source = $1 and entity_type = 'player' and external_key = any($2)`,
		source, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64, len(keys))
	for rows.Next() {
		var (
			key string
			id  int64
		)
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

// FindMatchByPlayers ищет НАШ матч по двум игрокам в окне вокруг времени фикстуры.
//
// Предикат выписан целиком, потому что его широта — это размен ложного LIVE на
// отсутствие карточки:
//   - только scheduled/live: без этого completed-матч можно пометить живым,
//     затерев результат, который принадлежит пайплайну;
//   - только одиночки, плюс ровно два участника — вторая линия защиты от пар;
//   - окно считается от времени ФИКСТУРЫ, а не от now: матч начинается, когда
//     закончится предыдущий на корте, и «сейчас» тут ни при чём.
//
// Берём две строки, чтобы отличить «ровно одна» от «несколько», и на
// нескольких возвращаем ErrAmbiguousMatch вместо догадки.
func FindMatchByPlayers(ctx context.Context, pool *pgxpool.Pool, p1, p2 int64,
	from, to time.Time) (int64, error) {

	rows, err := pool.Query(ctx, `
		select m.id from matches m
		where m.status in ('scheduled', 'live')
		  and m.discipline = 'singles'
		  and m.scheduled_at between $3 and $4
		  and exists (select 1 from match_participants mp
		              where mp.match_id = m.id and mp.player_id = $1)
		  and exists (select 1 from match_participants mp
		              where mp.match_id = m.id and mp.player_id = $2)
		  and (select count(*) from match_participants mp where mp.match_id = m.id) = 2
		limit 2`,
		p1, p2, from, to)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (int64, error) {
		var id int64
		err := row.Scan(&id)
		return id, err
	})
	if err != nil {
		return 0, err
	}
	switch len(ids) {
	case 0:
		return 0, pgx.ErrNoRows
	case 1:
		return ids[0], nil
	default:
		return 0, ErrAmbiguousMatch
	}
}

// AppendObservation — append-only запись факта. Статус из неё не пишется:
// он выводится из истории отдельно.
func AppendObservation(ctx context.Context, pool *pgxpool.Pool, runID, matchID int64,
	source, state, eventStatus string, at time.Time) error {

	_, err := pool.Exec(ctx, `
		insert into live_observations (run_id, match_id, source, state, event_status, observed_at)
		values ($1, $2, $3, $4, nullif($5, ''), $6)`,
		runID, matchID, source, state, eventStatus, at)
	return err
}

// UnmatchedRow — ОЧИЩЕННАЯ проекция строки борта для очереди ревью.
//
// Тип, а не []byte, намеренно: сигнатура с сырыми байтами приглашает передать
// json.RawMessage исходной строки, а она несёт сеты, геймы, очки и вероятности
// победы — поверхность, которую правило 1 называет поимённо. Здесь пронести
// счёт нечем.
type UnmatchedRow struct {
	ExternalKey string   `json:"external_key"`
	PlayerKeys  []string `json:"player_keys"`
	RoundCode   string   `json:"round_code,omitempty"`
	ScheduledAt string   `json:"scheduled_at,omitempty"`
	EventStatus string   `json:"event_status,omitempty"`
}

func RecordUnmatched(ctx context.Context, pool *pgxpool.Pool, source string,
	row UnmatchedRow, reason string, at time.Time) error {

	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `
		insert into live_unmatched (source, payload, reason, observed_at)
		values ($1, $2, $3, $4)`,
		source, payload, reason, at)
	return err
}

// LiveFlagRow — матч, который мы держим живым, вместе с числом пропусков.
type LiveFlagRow struct {
	MatchID     int64
	Source      string
	ExternalKey *string
	State       string
	FlippedAt   time.Time
	Misses      int
}

// HeldLiveFlags — все матчи, которые мы держим, с посчитанными пропусками.
//
// Пропуск = успешный прогон Job B, в котором матча в борте не было. Считаем
// ПРОГОНЫ, а не строки-«отсутствия»: прогоны строго упорядочены, поэтому «три
// подряд» — точное утверждение, а запись строк на каждый цикл при любом
// повторном выполнении считалась бы дважды и гасила карточку раньше срока.
//
// Каждое условие в подсчёте нужно:
//   - error is null    — сбойный прогон отсутствием не является. Здесь же
//     бесплатно подключается защита от пустого борта: она пишет error, поэтому
//     пустой борт в активном окне не может погасить карточку;
//   - skipped_reason is null — тик, который ничего не потратил, ничего и не видел;
//   - finished_at is not null — прогон, убитый редеплоем, не считается;
//   - job = 'live'     — прогоны Job A тратят ту же квоту, но отсутствием матча
//     в live-борте не являются.
//
// Условия requests_made > 0 здесь СОЗНАТЕЛЬНО нет, хотя оно напрашивается.
// Три условия выше уже означают «цикл дошёл до конца и разобрал борт», а
// requests_made отделяло бы не сбойные прогоны, а бесплатные: прогон
// dev-эндпоинта повтора разбирает настоящий борт, но к вендору не ходит. С
// этим условием повтор борта не считался бы пропуском, и dev-путь перестал бы
// быть точной репетицией боевого — а он существует именно ради неё.
func HeldLiveFlags(ctx context.Context, pool *pgxpool.Pool, source string) ([]LiveFlagRow, error) {
	rows, err := pool.Query(ctx, `
		select f.match_id, f.source, f.external_key, f.state, f.flipped_at,
		       coalesce((
		         select count(*) from live_ingest_runs r
		         where r.job = 'live'
		           and r.id > coalesce(f.last_seen_run_id, 0)
		           and r.finished_at is not null
		           and r.error is null
		           and r.skipped_reason is null
		       ), 0)
		from live_flags f
		where f.source = $1
		order by f.match_id`,
		source)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveFlagRow, error) {
		var f LiveFlagRow
		err := row.Scan(&f.MatchID, &f.Source, &f.ExternalKey, &f.State, &f.FlippedAt, &f.Misses)
		return f, err
	})
}

// MarkFlagSeen сдвигает «последний прогон, в котором мы видели матч».
func MarkFlagSeen(ctx context.Context, pool *pgxpool.Pool, matchID, runID int64,
	state string) error {

	_, err := pool.Exec(ctx, `
		update live_flags set last_seen_run_id = $2, state = $3 where match_id = $1`,
		matchID, runID, state)
	return err
}

// StaleFlags — матчи, которых мы держим дольше допустимого. Читается ДО
// решения об опросе: аварийный выход должен работать и тогда, когда опроса
// нет вовсе (кончилась квота, лёг источник, выключен рубильник).
func StaleFlags(ctx context.Context, pool *pgxpool.Pool, source string,
	olderThan time.Time) ([]int64, error) {

	rows, err := pool.Query(ctx, `
		select match_id from live_flags where source = $1 and flipped_at < $2`,
		source, olderThan)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (int64, error) {
		var id int64
		err := row.Scan(&id)
		return id, err
	})
}

// OrphanFlags — флаги, чей матч уже НЕ live: пайплайн забрал строку себе, либо
// статус поменяли мимо нас. Карточку надо погасить, даже если matches трогать
// уже нельзя.
func OrphanFlags(ctx context.Context, pool *pgxpool.Pool, source string) ([]int64, error) {
	rows, err := pool.Query(ctx, `
		select f.match_id from live_flags f
		join matches m on m.id = f.match_id
		where f.source = $1 and m.status <> 'live'`,
		source)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (int64, error) {
		var id int64
		err := row.Scan(&id)
		return id, err
	})
}

// LiveWindow — отрезок наблюдения. Дублирует форму из updater/live намеренно:
// storage не должен зависеть от слоя джобов.
type LiveWindow struct{ From, To time.Time }

// LiveSnapshot — всё, что решение об опросе читает из БД, одним запросом.
type LiveSnapshot struct {
	ActiveMatches      int
	Windows            []LiveWindow
	RequestsToday      int
	StaleRequestsToday int
	LastPollAt         *time.Time
	LastScheduleRunAt  *time.Time
}

// LoadLiveSnapshot собирает снимок на момент now.
//
// now передаётся параметром, а не берётся из now() в SQL: единственное
// обращение к часам за цикл — live_ingest_runs.started_at, и от него считаются
// все границы. Так цикл воспроизводим от сохранённого борта и метки времени.
func LoadLiveSnapshot(ctx context.Context, pool *pgxpool.Pool, source string,
	now time.Time, watchLead, watchTail time.Duration) (LiveSnapshot, error) {

	var s LiveSnapshot
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(),
		0, 0, 0, 0, time.UTC)

	err := pool.QueryRow(ctx, `
		select
		  (select count(*) from live_flags where source = $1),
		  (select coalesce(sum(requests_made), 0) from live_ingest_runs
		    where started_at >= $2),
		  (select coalesce(sum(requests_made), 0) from live_ingest_runs
		    where started_at >= $2 and mode = 'stale_safe'),
		  (select max(started_at) from live_ingest_runs
		    where job = 'live' and requests_made > 0),
		  (select max(finished_at) from live_ingest_runs
		    where job = 'live-schedule' and error is null and skipped_reason is null
		      and finished_at is not null and requests_made > 0)`,
		source, dayStart).Scan(
		&s.ActiveMatches, &s.RequestsToday, &s.StaleRequestsToday,
		&s.LastPollAt, &s.LastScheduleRunAt)
	if err != nil {
		return s, err
	}

	rows, err := pool.Query(ctx, `
		select scheduled_at - $2::interval, scheduled_at + $3::interval
		from live_schedule
		where source = $1 and scheduled_at is not null
		  and scheduled_at + $3::interval > $4`,
		source, watchLead, watchTail, now)
	if err != nil {
		return s, err
	}
	s.Windows, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveWindow, error) {
		var w LiveWindow
		err := row.Scan(&w.From, &w.To)
		return w, err
	})
	return s, err
}
