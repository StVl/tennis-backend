package storage

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PushToken struct {
	UserID string `json:"-"`
	Token  string `json:"token"`
	Env    string `json:"env"`
}

func UpsertPushToken(ctx context.Context, pool *pgxpool.Pool, userID, token, env string,
	at time.Time) error {

	_, err := pool.Exec(ctx, `
		insert into device_push_tokens (user_id, token, env, updated_at)
		values ($1, $2, $3, $4)
		on conflict (user_id, token) do update set env = excluded.env, updated_at = excluded.updated_at`,
		userID, token, env, at)
	return err
}

// DeletePushToken вызывается на 410 от Apple: повторные отправки на мёртвый
// токен считаются злоупотреблением.
func DeletePushToken(ctx context.Context, pool *pgxpool.Pool, token string) error {
	_, err := pool.Exec(ctx, `delete from device_push_tokens where token = $1`, token)
	return err
}

type LiveEvent struct {
	ID       int64
	MatchID  int64
	Event    string
	Attempts int
}

// ClaimLiveEvents забирает пачку необработанных событий.
//
// for update skip locked, а не обычный select: два инстанса пушера иначе
// разберут одно событие оба, и пользователь получит две карточки.
func ClaimLiveEvents(ctx context.Context, pool *pgxpool.Pool, limit, maxAttempts int,
	at time.Time) ([]LiveEvent, error) {

	rows, err := pool.Query(ctx, `
		update live_events set claimed_at = $3, attempts = attempts + 1
		where id in (
			select id from live_events
			where consumed_at is null and attempts < $2
			order by id
			limit $1
			for update skip locked
		)
		returning id, match_id, event, attempts`,
		limit, maxAttempts, at)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveEvent, error) {
		var e LiveEvent
		err := row.Scan(&e.ID, &e.MatchID, &e.Event, &e.Attempts)
		return e, err
	})
}

func ConsumeLiveEvent(ctx context.Context, pool *pgxpool.Pool, id int64, at time.Time) error {
	_, err := pool.Exec(ctx,
		`update live_events set consumed_at = $2, last_error = null where id = $1`, id, at)
	return err
}

func FailLiveEvent(ctx context.Context, pool *pgxpool.Pool, id int64, reason string) error {
	_, err := pool.Exec(ctx,
		`update live_events set claimed_at = null, last_error = $2 where id = $1`, id, reason)
	return err
}

type PushTarget struct {
	UserID string
	Token  string
	Env    string
}

// StartAudience — кому слать push-to-start по этому матчу.
//
// Подписка на ОБОИХ игроков даёт одну строку: distinct по пользователю, а не
// join, иначе человек получит две карточки на один матч. Уже открытая сессия
// исключается — второй push-to-start это дубль на локскрине.
func StartAudience(ctx context.Context, pool *pgxpool.Pool, matchID int64) ([]PushTarget, error) {
	rows, err := pool.Query(ctx, `
		select distinct t.user_id::text, t.token, t.env
		from device_push_tokens t
		where exists (
			select 1 from follows f
			join match_participants mp on mp.player_id = f.player_id
			where f.user_id = t.user_id and mp.match_id = $1
		)
		and not exists (
			select 1 from live_activity_sessions s
			where s.user_id = t.user_id and s.match_id = $1 and s.ended_at is null
		)
		order by t.user_id::text`,
		matchID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PushTarget, error) {
		var p PushTarget
		err := row.Scan(&p.UserID, &p.Token, &p.Env)
		return p, err
	})
}

func OpenSession(ctx context.Context, pool *pgxpool.Pool, userID string, matchID int64,
	at time.Time) error {

	_, err := pool.Exec(ctx, `
		insert into live_activity_sessions (user_id, match_id, phase, started_at)
		values ($1, $2, 'starting', $3)
		on conflict do nothing`,
		userID, matchID, at)
	return err
}

// SetSessionUpdateToken принимает токен, который iOS выдала уже запущенной
// активности. До него завершить карточку пушем нечем.
func SetSessionUpdateToken(ctx context.Context, pool *pgxpool.Pool, userID string,
	matchID int64, token string) (bool, error) {

	tag, err := pool.Exec(ctx, `
		update live_activity_sessions set update_token = $3, phase = 'active'
		where user_id = $1 and match_id = $2 and ended_at is null`,
		userID, matchID, token)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

type Session struct {
	ID          int64
	UserID      string
	MatchID     int64
	UpdateToken *string
	Phase       string
	StartedAt   time.Time
}

func OpenSessionsForMatch(ctx context.Context, pool *pgxpool.Pool, matchID int64) ([]Session, error) {
	rows, err := pool.Query(ctx, `
		select id, user_id::text, match_id, update_token, phase, started_at
		from live_activity_sessions
		where match_id = $1 and ended_at is null
		order by id`,
		matchID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSession)
}

func scanSession(row pgx.CollectableRow) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.MatchID, &s.UpdateToken, &s.Phase, &s.StartedAt)
	return s, err
}

func EndSession(ctx context.Context, pool *pgxpool.Pool, id int64, at time.Time) error {
	_, err := pool.Exec(ctx,
		`update live_activity_sessions set phase = 'ended', ended_at = $2 where id = $1`, id, at)
	return err
}

// StaleSessions — сессии, открытые дольше допустимого.
//
// Нужны потому, что сбой ленты посреди матча не даёт события об окончании
// вовсе: без этой уборки карточка висит, пока iOS не снимет её сама через
// несколько часов.
func StaleSessions(ctx context.Context, pool *pgxpool.Pool, olderThan time.Time) ([]Session, error) {
	rows, err := pool.Query(ctx, `
		select id, user_id::text, match_id, update_token, phase, started_at
		from live_activity_sessions
		where ended_at is null and started_at < $1
		order by id`,
		olderThan)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanSession)
}

// PushTokensForUser нужен только для end-пуша сессии, у которой update_token
// так и не пришёл: карточку тогда не погасить, но и висеть вечно она не должна.
func PushTokensForUser(ctx context.Context, pool *pgxpool.Pool, userID string) ([]PushTarget, error) {
	rows, err := pool.Query(ctx, `
		select user_id::text, token, env from device_push_tokens where user_id = $1`,
		userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PushTarget, error) {
		var p PushTarget
		err := row.Scan(&p.UserID, &p.Token, &p.Env)
		return p, err
	})
}
