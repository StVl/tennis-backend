package storage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Источники флипа. Ручные флипы помечаются отдельно: проход по пропускам их
// исключает, иначе матч, поднятый руками для теста iOS, молча вернулся бы
// назад через три цикла опроса — синтетических фикстур в борте источника нет.
const (
	LiveSourceAPI = "livetennisapi"
	LiveSourceDev = "dev"
)

// События outbox'а. Домен закрыт check-констрейнтом в db/live_ingest.sql.
const (
	LiveEventLive      = "live"
	LiveEventFinished  = "finished"
	LiveEventSuspended = "suspended"
	LiveEventResumed   = "resumed"
)

// FlipResult различает три исхода, которые вызывающему нужно различать:
// «сделали», «уже было» и «guard не пропустил». Голого bool здесь не хватает —
// 204 на «уже live» и 409 на «этот матч флипать нельзя» это разные ответы.
type FlipResult int

const (
	FlipDone FlipResult = iota
	FlipAlreadyLive
	FlipRefused
)

// LiveFlag — наша отметка о том, что матч держим live мы.
type LiveFlag struct {
	Source        string    `json:"source"`
	ExternalKey   *string   `json:"external_key"`
	State         string    `json:"state"`
	PriorStatus   string    `json:"prior_status"`
	FlippedAt     time.Time `json:"flipped_at"`
	LastSeenRunID *int64    `json:"last_seen_run_id"`
}

// LiveMatchState — текущий статус матча плюс флаг, если он есть.
// Flag = null означает «матч нами не флипнут»; отдельным объектом, а не
// плоскими полями, чтобы клиент не получал нулевые значения Go вместо
// честного «флага нет» (flipped_at первого года нашей эры и пустые строки).
type LiveMatchState struct {
	MatchID     int64      `json:"match_id"`
	Status      string     `json:"status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	Flag        *LiveFlag  `json:"flag"`
}

// LiveMatch — нейтральная форма живого матча БЕЗ полей счёта.
//
// sets, score_text и live отсутствуют намеренно, и поэтому это отдельный тип, а
// не переиспользование Match: у Match эти поля без omitempty, то есть обнулить
// их нельзя — получится "sets": null, а поле, которое существует, однажды
// отрендерят. Правило 1 из docs/live-status-ingest.md: карточка показывает факт
// присутствия на корте, а не счёт.
type LiveMatch struct {
	ID             int64       `json:"id"`
	Edition        string      `json:"edition"`
	TournamentName string      `json:"tournament_name"`
	Round          string      `json:"round"`
	ScheduledAt    *time.Time  `json:"scheduled_at"`
	Court          *string     `json:"court"`
	Status         string      `json:"status"`
	Surface        string      `json:"surface"`
	Sides          []MatchSide `json:"sides"`
	// Когда мы пометили матч живым. Не время начала игры: источник сообщает
	// факт «на корте», а не первый разыгранный мяч.
	StartedAt *time.Time `json:"started_at"`
}

// UserLiveMatches — живые матчи среди подписок пользователя.
//
// Матч, в котором пользователь подписан на ОБОИХ игроков, возвращается один
// раз: условие — exists-подзапрос, а не join, иначе такая строка задвоилась бы.
// Одиночки: парный разряд из триггера исключён.
func UserLiveMatches(ctx context.Context, pool *pgxpool.Pool,
	userID string, limit int) ([]LiveMatch, error) {

	rows, err := pool.Query(ctx, `
		select m.id, te.slug, t.name, m.round_code, m.scheduled_at, m.court,
		       m.status::text, te.surface::text, f.flipped_at,
		       coalesce(parts.j::text, '[]')
		from matches m
		join tournament_editions te on te.id = m.edition_id
		join tournaments t on t.id = te.tournament_id
		left join live_flags f on f.match_id = m.id
		left join lateral (
			select json_agg(json_build_object(
			         'side', mp.side, 'slot', mp.slot, 'slug', p.slug,
			         'name', p.display_name, 'last_name', p.last_name,
			         'photo_url', p.photo_url, 'rank', r.rank)
			       order by mp.side, mp.slot) as j
			from match_participants mp
			join players p on p.id = mp.player_id
			left join v_current_rankings r on r.player_id = p.id and r.tour_code = 'atp'
			where mp.match_id = m.id
		) parts on true
		where m.status = 'live'
		  and m.discipline = 'singles'
		  and exists (select 1 from match_participants mp2
		              join follows fo on fo.player_id = mp2.player_id
		              where mp2.match_id = m.id and fo.user_id = $1)
		order by m.scheduled_at asc nulls last, m.id
		limit $2`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanLiveMatch)
}

func scanLiveMatch(row pgx.CollectableRow) (LiveMatch, error) {
	var (
		m         LiveMatch
		partsJSON string
	)
	if err := row.Scan(&m.ID, &m.Edition, &m.TournamentName, &m.Round,
		&m.ScheduledAt, &m.Court, &m.Status, &m.Surface, &m.StartedAt,
		&partsJSON); err != nil {
		return m, err
	}
	sides, err := sidesFromJSON(partsJSON)
	if err != nil {
		return m, err
	}
	m.Sides = sides
	return m, nil
}

// FlipLive переводит матч в live и кладёт событие в outbox.
//
// Guard одинаков для всех источников, включая ручные флипы: только из
// 'scheduled' и только пока нет победителя.
//
//   - Источник отдаёт протухшие live-строки (status=live вместе с
//     event_status=Finished). Без guard'а они воскрешали бы завершённые матчи,
//     затирая результат, который принадлежит пайплайну tennis-data-storage.
//   - Ручные флипы guard'у подчиняются тоже, иначе они создают состояние,
//     которого у боевого пути не бывает: завершённый матч со счётом и
//     winner_side, помеченный live. Именно на такой ответ iOS собирала бы
//     Live Activity до конца Phase 8. Флипать руками нужно scheduled-строки
//     из db/dev_fixtures.sql.
//
// Идемпотентна: матч уже live -> FlipAlreadyLive, ничего не пишется. Без этого
// второй вызов записал бы prior_status='live', и вернуть матч назад стало бы
// нечем, кроме правки БД руками.
//
// runID — прогон, в котором матч увиден; для ручного флипа nil.
func FlipLive(ctx context.Context, pool *pgxpool.Pool, matchID int64,
	source, externalKey string, runID *int64, at time.Time) (FlipResult, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return FlipRefused, err
	}
	defer tx.Rollback(ctx)

	// Блокировка строки matches берётся первой И здесь, и в FlipOut: одинаковый
	// порядок захвата — единственное, что защищает от взаимной блокировки, когда
	// в Phase 7 появится третий писатель (бамп last_seen_run_id).
	var (
		status     string
		winnerSide *int
	)
	if err := tx.QueryRow(ctx,
		`select status::text, winner_side from matches where id = $1 for update`,
		matchID).Scan(&status, &winnerSide); err != nil {
		return FlipRefused, err
	}

	if status == "live" {
		return FlipAlreadyLive, nil
	}
	if status != "scheduled" || winnerSide != nil {
		return FlipRefused, nil
	}

	if _, err := tx.Exec(ctx,
		`update matches set status = 'live' where id = $1`, matchID); err != nil {
		return FlipRefused, err
	}

	// Досюда мы дошли только если матч НЕ был live. Флаг в этот момент
	// существовать не должен; если существует — кто-то менял статус мимо нас,
	// и это надо увидеть в логах, а не проглотить через do nothing.
	var staleSource string
	switch err := tx.QueryRow(ctx,
		`select source from live_flags where match_id = $1`, matchID).Scan(&staleSource); {
	case err == nil:
		slog.Warn("stale live flag overwritten", "match_id", matchID,
			"stale_source", staleSource, "match_status", status)
	case !errors.Is(err, pgx.ErrNoRows):
		return FlipRefused, err
	}

	var key *string
	if externalKey != "" {
		key = &externalKey
	}
	if _, err := tx.Exec(ctx, `
		insert into live_flags (match_id, source, external_key, state,
		                        prior_status, flipped_at, last_seen_run_id)
		values ($1, $2, $3, 'on_court', $4::match_status_t, $5, $6)
		on conflict (match_id) do update set
			source = excluded.source, external_key = excluded.external_key,
			state = excluded.state, prior_status = excluded.prior_status,
			flipped_at = excluded.flipped_at, last_seen_run_id = excluded.last_seen_run_id`,
		matchID, source, key, status, at, runID); err != nil {
		return FlipRefused, err
	}

	if err := insertLiveEvent(ctx, tx, matchID, LiveEventLive, "", at); err != nil {
		return FlipRefused, err
	}
	return FlipDone, tx.Commit(ctx)
}

// FlipOut снимает флаг и восстанавливает статус, который был до флипа.
//
// Асимметрия, которая здесь принципиальна: статус восстанавливается ТОЛЬКО если
// матч всё ещё live (то есть всё ещё наш). Если пайплайн успел забрать строку и
// записать completed со счётом — мы её не трогаем. Но флаг снимаем и событие
// пишем в любом случае: иначе карточка на локскрине не погаснет никогда.
//
// Возвращает статус, в который вернули (пустая строка, если флага не было).
func FlipOut(ctx context.Context, pool *pgxpool.Pool, matchID int64,
	event, reason string, at time.Time) (bool, string, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback(ctx)

	// Тот же порядок захвата, что и в FlipLive: сначала matches, потом live_flags.
	var status string
	if err := tx.QueryRow(ctx,
		`select status::text from matches where id = $1 for update`,
		matchID).Scan(&status); err != nil {
		return false, "", err
	}

	var priorStatus string
	err = tx.QueryRow(ctx,
		`select prior_status::text from live_flags where match_id = $1 for update`,
		matchID).Scan(&priorStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}

	if _, err := tx.Exec(ctx, `
		update matches set status = $2::match_status_t
		where id = $1 and status = 'live'`,
		matchID, priorStatus); err != nil {
		return false, "", err
	}

	if _, err := tx.Exec(ctx,
		`delete from live_flags where match_id = $1`, matchID); err != nil {
		return false, "", err
	}

	if err := insertLiveEvent(ctx, tx, matchID, event, reason, at); err != nil {
		return false, "", err
	}
	return true, priorStatus, tx.Commit(ctx)
}

// GetLiveMatchState — статус матча и наш флаг, если он есть.
// pgx.ErrNoRows, если матча нет.
func GetLiveMatchState(ctx context.Context, pool *pgxpool.Pool, matchID int64) (*LiveMatchState, error) {
	var (
		s      LiveMatchState
		f      LiveFlag
		source *string
		state  *string
		prior  *string
		at     *time.Time
	)
	err := pool.QueryRow(ctx, `
		select m.id, m.status::text, m.scheduled_at,
		       f.source, f.state, f.prior_status::text, f.flipped_at,
		       f.external_key, f.last_seen_run_id
		from matches m
		left join live_flags f on f.match_id = m.id
		where m.id = $1`,
		matchID).Scan(&s.MatchID, &s.Status, &s.ScheduledAt,
		&source, &state, &prior, &at, &f.ExternalKey, &f.LastSeenRunID)
	if err != nil {
		return nil, err
	}
	if source != nil {
		f.Source, f.State, f.PriorStatus = *source, deref(state), deref(prior)
		if at != nil {
			f.FlippedAt = *at
		}
		s.Flag = &f
	}
	return &s, nil
}

// insertLiveEvent — строка outbox'а. Пишется в той же транзакции, что и смена
// статуса: иначе падение между ними либо теряет пуш, либо отправляет пуш о
// переходе, которого не было.
func insertLiveEvent(ctx context.Context, tx pgx.Tx, matchID int64,
	event, reason string, at time.Time) error {

	var r *string
	if reason != "" {
		r = &reason
	}
	_, err := tx.Exec(ctx, `
		insert into live_events (match_id, event, reason, created_at)
		values ($1, $2, $3, $4)`,
		matchID, event, r, at)
	return err
}
