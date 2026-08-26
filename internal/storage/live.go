package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Источники флипа. Ручные флипы помечаются отдельно: проход по пропускам их
// исключает, иначе матч, поднятый руками для теста iOS, молча вернулся бы
// назад через три цикла опроса.
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

// LiveFlag — матч, который мы держим live: что восстановить на выходе и когда
// мы его видели в последний раз.
type LiveFlag struct {
	MatchID       int64      `json:"match_id"`
	Source        string     `json:"source"`
	ExternalKey   *string    `json:"external_key"`
	State         string     `json:"state"`
	PriorStatus   string     `json:"prior_status"`
	FlippedAt     time.Time  `json:"flipped_at"`
	LastSeenRunID *int64     `json:"last_seen_run_id"`
	Status        string     `json:"status"` // текущий matches.status
	ScheduledAt   *time.Time `json:"scheduled_at"`
}

// FlipLive переводит матч в live и кладёт событие в outbox.
//
// Идемпотентна: матч уже live -> (false, nil), ничего не пишется. Это важно не
// только для dev-эндпоинта: без этого второй POST записал бы prior_status='live'
// и матч уже нельзя было бы вернуть назад ничем, кроме правки БД руками.
//
// Guard входа — из 'scheduled' и только без победителя. Источник отдаёт
// протухшие live-строки (status=live вместе с event_status=Finished), и без
// guard'а они воскрешали бы уже завершённые матчи, затирая результат, который
// принадлежит пайплайну tennis-data-storage.
//
// Ручные флипы (source=LiveSourceDev) guard обходят: в базе 481 матч и все
// completed, ни одной scheduled-строки нет, так что guard'нутый dev-флип не
// сработал бы вообще ни на одном матче.
//
// runID — прогон, в котором матч увиден; для ручного флипа nil.
func FlipLive(ctx context.Context, pool *pgxpool.Pool, matchID int64,
	source, externalKey string, runID *int64, at time.Time) (bool, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	// for update: guard и запись состояния должны быть атомарны относительно
	// параллельного цикла и пайплайна
	var (
		status     string
		winnerSide *int
	)
	if err := tx.QueryRow(ctx,
		`select status::text, winner_side from matches where id = $1 for update`,
		matchID).Scan(&status, &winnerSide); err != nil {
		return false, err
	}

	if status == "live" {
		return false, nil
	}
	if source != LiveSourceDev && (status != "scheduled" || winnerSide != nil) {
		return false, nil
	}

	if _, err := tx.Exec(ctx,
		`update matches set status = 'live' where id = $1`, matchID); err != nil {
		return false, err
	}

	var key *string
	if externalKey != "" {
		key = &externalKey
	}
	if _, err := tx.Exec(ctx, `
		insert into live_flags (match_id, source, external_key, state,
		                        prior_status, flipped_at, last_seen_run_id)
		values ($1, $2, $3, 'on_court', $4::match_status_t, $5, $6)
		on conflict (match_id) do nothing`,
		matchID, source, key, status, at, runID); err != nil {
		return false, err
	}

	if err := insertLiveEvent(ctx, tx, matchID, LiveEventLive, "", at); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// FlipOut снимает флаг и восстанавливает статус, который был до флипа.
//
// Асимметрия, которая здесь принципиальна: статус восстанавливается ТОЛЬКО если
// матч всё ещё live (то есть всё ещё наш). Если пайплайн успел забрать строку и
// записать completed со счётом — мы её не трогаем. Но флаг снимаем и событие
// пишем в любом случае: иначе карточка на локскрине не погаснет никогда.
//
// Идемпотентна: флага нет -> (false, nil).
func FlipOut(ctx context.Context, pool *pgxpool.Pool, matchID int64,
	event, reason string, at time.Time) (bool, error) {

	tx, err := pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var priorStatus string
	err = tx.QueryRow(ctx,
		`select prior_status::text from live_flags where match_id = $1 for update`,
		matchID).Scan(&priorStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.Exec(ctx, `
		update matches set status = $2::match_status_t
		where id = $1 and status = 'live'`,
		matchID, priorStatus); err != nil {
		return false, err
	}

	if _, err := tx.Exec(ctx,
		`delete from live_flags where match_id = $1`, matchID); err != nil {
		return false, err
	}

	if err := insertLiveEvent(ctx, tx, matchID, event, reason, at); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// GetLiveFlag — состояние флипа вместе с текущим статусом матча.
// pgx.ErrNoRows, если матча нет; флага может не быть (тогда поля флага пустые).
func GetLiveFlag(ctx context.Context, pool *pgxpool.Pool, matchID int64) (*LiveFlag, error) {
	var (
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
		matchID).Scan(&f.MatchID, &f.Status, &f.ScheduledAt,
		&source, &state, &prior, &at, &f.ExternalKey, &f.LastSeenRunID)
	if err != nil {
		return nil, err
	}
	f.Source, f.State, f.PriorStatus = deref(source), deref(state), deref(prior)
	if at != nil {
		f.FlippedAt = *at
	}
	return &f, nil
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
