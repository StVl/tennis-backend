package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnauthorized = errors.New("unknown or missing token")

// NewUser — результат анонимной регистрации. Токен показывается один раз,
// в базе хранится только его sha256.
type NewUser struct {
	UserID string `json:"user_id"`
	Token  string `json:"token"`
}

// Profile — ответ GET /users/me.
type Profile struct {
	UserID       string          `json:"user_id"`
	Email        *string         `json:"email"`
	Settings     json.RawMessage `json:"settings"`
	CreatedAt    time.Time       `json:"created_at"`
	FollowsCount int             `json:"follows_count"`
}

// Follow — подписка со сводкой игрока.
type Follow struct {
	Slug       string    `json:"slug"`
	Name       string    `json:"name"`
	PhotoURL   *string   `json:"photo_url"`
	FollowedAt time.Time `json:"followed_at"`
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// CreateUser создаёт анонимный профиль и bearer-токен.
func CreateUser(ctx context.Context, pool *pgxpool.Pool, deviceID string) (*NewUser, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := "tt_" + hex.EncodeToString(raw)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var userID string
	if err := tx.QueryRow(ctx,
		`insert into profiles (device_id) values (nullif($1, '')) returning user_id::text`,
		deviceID).Scan(&userID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`insert into auth_tokens (token_hash, user_id) values ($1, $2)`,
		hashToken(token), userID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &NewUser{UserID: userID, Token: token}, nil
}

// AuthUser проверяет токен, обновляет last_seen_at и возвращает user_id.
func AuthUser(ctx context.Context, pool *pgxpool.Pool, token string) (string, error) {
	var userID string
	err := pool.QueryRow(ctx,
		`update auth_tokens set last_seen_at = now()
		 where token_hash = $1 returning user_id::text`,
		hashToken(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return userID, err
}

func GetProfile(ctx context.Context, pool *pgxpool.Pool, userID string) (*Profile, error) {
	var (
		p        Profile
		settings string
	)
	err := pool.QueryRow(ctx, `
		select p.user_id::text, p.email, p.settings::text, p.created_at,
		       (select count(*) from follows f where f.user_id = p.user_id)
		from profiles p where p.user_id = $1`,
		userID).Scan(&p.UserID, &p.Email, &settings, &p.CreatedAt, &p.FollowsCount)
	if err != nil {
		return nil, err
	}
	p.Settings = json.RawMessage(settings)
	return &p, nil
}

// ListFollows — подписки пользователя в порядке добавления.
func ListFollows(ctx context.Context, pool *pgxpool.Pool, userID string) ([]Follow, error) {
	rows, err := pool.Query(ctx, `
		select p.slug, p.display_name, p.photo_url, f.created_at
		from follows f
		join players p on p.id = f.player_id
		where f.user_id = $1
		order by f.created_at, p.slug`,
		userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Follow, error) {
		var f Follow
		err := row.Scan(&f.Slug, &f.Name, &f.PhotoURL, &f.FollowedAt)
		return f, err
	})
}

// FollowSlugs — только слаги (для сборки home/widget), в порядке добавления.
func FollowSlugs(ctx context.Context, pool *pgxpool.Pool, userID string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		select p.slug from follows f
		join players p on p.id = f.player_id
		where f.user_id = $1
		order by f.created_at, p.slug`,
		userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var s string
		err := row.Scan(&s)
		return s, err
	})
}

// AddFollow — идемпотентная подписка. false = игрок не найден.
func AddFollow(ctx context.Context, pool *pgxpool.Pool, userID, playerSlug string) (bool, error) {
	tag, err := pool.Exec(ctx, `
		insert into follows (user_id, player_id)
		select $1, id from players where slug = $2
		on conflict do nothing`,
		userID, playerSlug)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}
	// 0 строк: либо уже подписан (ок), либо игрока нет
	var exists bool
	err = pool.QueryRow(ctx,
		`select exists (select 1 from players where slug = $1)`, playerSlug).Scan(&exists)
	return exists, err
}

// RemoveFollow — идемпотентная отписка.
func RemoveFollow(ctx context.Context, pool *pgxpool.Pool, userID, playerSlug string) error {
	_, err := pool.Exec(ctx, `
		delete from follows
		where user_id = $1
		  and player_id = (select id from players where slug = $2)`,
		userID, playerSlug)
	return err
}

// ReplaceFollows заменяет весь список (онбординг). Возвращает слаги,
// которых нет в базе (они молча не подписываются).
func ReplaceFollows(ctx context.Context, pool *pgxpool.Pool, userID string, slugs []string) ([]string, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `delete from follows where user_id = $1`, userID); err != nil {
		return nil, err
	}
	if len(slugs) > 0 {
		// created_at со смещением сохраняет порядок выбора пользователя
		if _, err := tx.Exec(ctx, `
			insert into follows (user_id, player_id, created_at)
			select $1, p.id, now() + make_interval(secs => ord * 0.001)
			from unnest($2::text[]) with ordinality as u(slug, ord)
			join players p on p.slug = u.slug
			on conflict do nothing`,
			userID, slugs); err != nil {
			return nil, err
		}
	}

	rows, err := tx.Query(ctx, `
		select u.slug from unnest($1::text[]) as u(slug)
		where not exists (select 1 from players p where p.slug = u.slug)`,
		slugs)
	if err != nil {
		return nil, err
	}
	unknown, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var s string
		err := row.Scan(&s)
		return s, err
	})
	if err != nil {
		return nil, err
	}
	return unknown, tx.Commit(ctx)
}
