package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Когда переход случился. Нужен не для отправки, а чтобы отказ от
	// протухшего события было видно в логе с его возрастом.
	CreatedAt time.Time
}

// MatchStillLive — идёт ли матч прямо сейчас И держим ли его live мы.
//
// Проверяется перед стартовым пушем, а не только в момент записи события.
// Между FlipLive и разбором очереди проходит до минуты, а при выключенном
// пушере — сколько угодно: retention удаляет только ПОТРЕБЛЁННЫЕ события
// (см. PruneLiveTables), поэтому непрочитанные копятся вечно. Без этой
// проверки включение PUSH_ENABLED поднимало бы карточки по всему накопленному
// хвосту — для матчей, которые кончились часы назад, — и погасить их было бы
// нечем: update_token у такой карточки ещё не существует.
//
// Флаг проверяется вместе со статусом намеренно: status='live' без нашего
// флага ставит пайплайн контента, и поднимать по нему карточку мы не вправе.
func MatchStillLive(ctx context.Context, pool *pgxpool.Pool, matchID int64) (bool, error) {
	var live bool
	err := pool.QueryRow(ctx, `
		select exists (
			select 1 from matches m
			join live_flags f on f.match_id = m.id
			where m.id = $1 and m.status = 'live')`,
		matchID).Scan(&live)
	return live, err
}

// ClaimLiveEvents забирает пачку необработанных событий.
//
// for update skip locked, а не обычный select: два инстанса пушера иначе
// разберут одно событие оба, и пользователь получит две карточки.
// retryAfter задаёт паузу между попытками: без неё пушер с крона раз в минуту
// сжигает весь лимит попыток за минуты, и перебой у Apple длиннее этого
// исчерпывает событие насовсем.
func ClaimLiveEvents(ctx context.Context, pool *pgxpool.Pool, limit, maxAttempts int,
	at time.Time, retryAfter time.Duration) ([]LiveEvent, error) {

	rows, err := pool.Query(ctx, `
		update live_events set claimed_at = $3, attempts = attempts + 1
		where id in (
			select id from live_events
			where consumed_at is null and attempts < $2
			  and (claimed_at is null or claimed_at < $4)
			order by id
			limit $1
			for update skip locked
		)
		returning id, match_id, event, attempts, created_at`,
		limit, maxAttempts, at, at.Add(-retryAfter))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (LiveEvent, error) {
		var e LiveEvent
		err := row.Scan(&e.ID, &e.MatchID, &e.Event, &e.Attempts, &e.CreatedAt)
		return e, err
	})
}

func ConsumeLiveEvent(ctx context.Context, pool *pgxpool.Pool, id int64, at time.Time) error {
	_, err := pool.Exec(ctx,
		`update live_events set consumed_at = $2, last_error = null where id = $1`, id, at)
	return err
}

// FailLiveEvent помечает попытку неудачной. claimed_at НЕ сбрасывается: он же
// служит отметкой «когда пробовали в последний раз», по которой ClaimLiveEvents
// выдерживает паузу. Со сбросом пауза не действовала бы вовсе.
func FailLiveEvent(ctx context.Context, pool *pgxpool.Pool, id int64, reason string) error {
	_, err := pool.Exec(ctx,
		`update live_events set last_error = $2 where id = $1`, id, reason)
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

// OpenSession занимает слот карточки и возвращает id строки; 0 означает, что
// открытая сессия уже была.
//
// Слот занимается ДО отправки пуша. Уникальный индекс по (user_id, match_id)
// при ended_at is null — единственное, что не даёт отправить второй старт на
// тот же матч, а если сначала отправлять, то падение между отправкой и
// вставкой оставит карточку без сессии: гасить её потом будет нечем.
func OpenSession(ctx context.Context, pool *pgxpool.Pool, userID string, matchID int64,
	at time.Time) (int64, error) {

	var id int64
	err := pool.QueryRow(ctx, `
		insert into live_activity_sessions (user_id, match_id, phase, started_at)
		values ($1, $2, 'starting', $3)
		on conflict do nothing
		returning id`,
		userID, matchID, at).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// DeleteSession снимает слот под пуш, который отправить не удалось: иначе
// повторная попытка пропустит этого пользователя как «уже с карточкой».
func DeleteSession(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx, `delete from live_activity_sessions where id = $1`, id)
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

// PushCard — то, чей это матч и где. Всё, что нужно клиенту, чтобы построить
// карточку, и ничего сверх: ни счёта, ни сетов, ни победителя. Правило 1 из
// docs/live-status-ingest.md действует и в payload'е пуша — это последний шаг,
// на котором его проще всего нарушить.
type PushCard struct {
	MatchID        int64        `json:"match_id"`
	Edition        string       `json:"edition"`
	TournamentName string       `json:"tournament_name"`
	Round          string       `json:"round"`
	Players        []CardPlayer `json:"players"`
}

type CardPlayer struct {
	Side int    `json:"side"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func PushCardFor(ctx context.Context, pool *pgxpool.Pool, matchID int64) (PushCard, error) {
	var (
		card        PushCard
		playersJSON string
	)
	err := pool.QueryRow(ctx, `
		select m.id, te.slug, t.name, m.round_code,
		       coalesce((
		         select json_agg(json_build_object(
		                  'side', mp.side, 'slug', p.slug, 'name', p.display_name)
		                order by mp.side, mp.slot)
		         from match_participants mp
		         join players p on p.id = mp.player_id
		         where mp.match_id = m.id), '[]')::text
		from matches m
		join tournament_editions te on te.id = m.edition_id
		join tournaments t on t.id = te.tournament_id
		where m.id = $1`,
		matchID).Scan(&card.MatchID, &card.Edition, &card.TournamentName,
		&card.Round, &playersJSON)
	if err != nil {
		return card, err
	}
	if err := json.Unmarshal([]byte(playersJSON), &card.Players); err != nil {
		return card, fmt.Errorf("unmarshal card players: %w", err)
	}
	return card, nil
}
