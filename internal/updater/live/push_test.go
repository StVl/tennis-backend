package live

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StVl/tennis-backend/internal/apns"
	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/storage"
)

type fakeSender struct {
	sent []apns.Notification
	err  error
}

func (f *fakeSender) Send(ctx context.Context, n apns.Notification) error {
	f.sent = append(f.sent, n)
	return f.err
}

func (f *fakeSender) byType(t apns.PushType) []apns.Notification {
	var out []apns.Notification
	for _, n := range f.sent {
		if n.Type == t {
			out = append(out, n)
		}
	}
	return out
}

func pushCfg() config.PushConfig {
	return config.PushConfig{
		Enabled: true, BatchSize: 50, MaxAttempts: 5,
		DismissAfter: 30 * time.Second, MaxSessionAge: 6 * time.Hour,
	}
}

// pushEnv заводит пользователя, подписанного на обоих игроков матча, с токеном.
func pushEnv(t *testing.T, pool *pgxpool.Pool, matchID int64) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx,
		`insert into profiles (device_id) values ('push-test') returning user_id::text`).
		Scan(&userID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from profiles where user_id = $1`, userID)
	})

	if _, err := pool.Exec(ctx, `
		insert into follows (user_id, player_id)
		select $1, mp.player_id from match_participants mp where mp.match_id = $2`,
		userID, matchID); err != nil {
		t.Fatal(err)
	}
	if err := storage.UpsertPushToken(ctx, pool, userID, "tok-"+userID, "sandbox",
		time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return userID
}

func emitEvent(t *testing.T, pool *pgxpool.Pool, matchID int64, event string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		insert into live_events (match_id, event, created_at) values ($1, $2, now())`,
		matchID, event); err != nil {
		t.Fatal(err)
	}
}

// Подписка на обоих игроков — ОДНА карточка, а не две.
func TestPushStartIsOnePerUserPerMatch(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	userID := pushEnv(t, pool, fm.id)

	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	sender := &fakeSender{}
	u := NewPush(pool, sender, pushCfg())
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := len(sender.byType(apns.PushStart)); got != 1 {
		t.Fatalf("отправлено %d push-to-start, ожидался 1: подписка на обоих игроков "+
			"не должна давать две карточки", got)
	}

	var sessions int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from live_activity_sessions where user_id = $1 and match_id = $2`,
		userID, fm.id).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("сессий %d, ожидалась 1", sessions)
	}

	// повторное событие по тому же матчу не должно слать второй старт
	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	if err := u.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(sender.byType(apns.PushStart)); got != 1 {
		t.Fatalf("после второго события стартов %d — открытая сессия должна исключать "+
			"пользователя из аудитории", got)
	}
}

// Без update-токена погасить карточку нечем, но сессию всё равно надо закрыть:
// открытая сессия навсегда блокирует новый старт по этому матчу.
func TestPushEndClosesSessionWithoutUpdateToken(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	pushEnv(t, pool, fm.id)

	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	sender := &fakeSender{}
	u := NewPush(pool, sender, pushCfg())
	if err := u.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	emitEvent(t, pool, fm.id, storage.LiveEventFinished)
	if err := u.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := len(sender.byType(apns.PushEnd)); got != 0 {
		t.Errorf("отправлено %d end-пушей без update-токена: слать нечем", got)
	}
	var open int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from live_activity_sessions where match_id = $1 and ended_at is null`,
		fm.id).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatal("сессия осталась открытой: она навсегда заблокирует новый старт")
	}
}

// С update-токеном карточка гасится явным end-пушем.
func TestPushEndSendsWithUpdateToken(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	userID := pushEnv(t, pool, fm.id)
	ctx := context.Background()

	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	sender := &fakeSender{}
	u := NewPush(pool, sender, pushCfg())
	if err := u.Update(ctx); err != nil {
		t.Fatal(err)
	}

	ok, err := storage.SetSessionUpdateToken(ctx, pool, userID, fm.id, "update-token-1")
	if err != nil || !ok {
		t.Fatalf("SetSessionUpdateToken: ok=%v err=%v", ok, err)
	}

	emitEvent(t, pool, fm.id, storage.LiveEventFinished)
	if err := u.Update(ctx); err != nil {
		t.Fatal(err)
	}

	ends := sender.byType(apns.PushEnd)
	if len(ends) != 1 {
		t.Fatalf("end-пушей %d, ожидался 1", len(ends))
	}
	if ends[0].Token != "update-token-1" {
		t.Errorf("end ушёл на %q: гасить надо токеном активности, а не push-to-start",
			ends[0].Token)
	}
	if ends[0].DismissAt.IsZero() {
		t.Error("dismissal-date не задан — карточка не уберётся с локскрина сама")
	}
}

// Мёртвый токен удаляется: Apple считает повторные отправки на него
// злоупотреблением.
func TestPushDropsUnregisteredToken(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	userID := pushEnv(t, pool, fm.id)

	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	u := NewPush(pool, &fakeSender{err: apns.ErrUnregistered}, pushCfg())
	if err := u.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	var tokens int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from device_push_tokens where user_id = $1`, userID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatal("токен, объявленный мёртвым, не удалён")
	}
	// и сессия не открыта: карточки нет
	var sessions int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from live_activity_sessions where match_id = $1`, fm.id).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatal("сессия открыта, хотя пуш не доставлен")
	}
}

// Сбой ленты посреди матча не даёт события об окончании вовсе. Без уборки
// карточка висит, пока iOS не снимет её сама.
func TestPushSweepsStaleSessions(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	userID := pushEnv(t, pool, fm.id)
	ctx := context.Background()

	if err := storage.OpenSession(ctx, pool, userID, fm.id,
		time.Now().UTC().Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}

	u := NewPush(pool, &fakeSender{}, pushCfg())
	if err := u.Update(ctx); err != nil {
		t.Fatal(err)
	}

	var open int
	if err := pool.QueryRow(ctx,
		`select count(*) from live_activity_sessions where match_id = $1 and ended_at is null`,
		fm.id).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatal("просроченная сессия не убрана")
	}
}

// Уборка обязана работать при выключенном рубильнике: его дёргают во время
// инцидента, и именно тогда карточки нельзя оставить висеть.
func TestPushSweepRunsWhenDisabled(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	userID := pushEnv(t, pool, fm.id)
	ctx := context.Background()

	if err := storage.OpenSession(ctx, pool, userID, fm.id,
		time.Now().UTC().Add(-8*time.Hour)); err != nil {
		t.Fatal(err)
	}

	cfg := pushCfg()
	cfg.Enabled = false
	if err := NewPush(pool, &fakeSender{}, cfg).Update(ctx); err != nil {
		t.Fatal(err)
	}

	var open int
	if err := pool.QueryRow(ctx,
		`select count(*) from live_activity_sessions where ended_at is null`).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Fatal("при выключенном рубильнике уборка не сработала")
	}
}

// Событие, которое падает всегда, не должно разбираться вечно.
func TestPushGivesUpAfterMaxAttempts(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	ctx := context.Background()

	// событие про матч, которого нет -> StartAudience вернёт пусто, но событие
	// само по себе валидно; заставим падать саму выборку невозможным матчем
	emitEvent(t, pool, fm.id, storage.LiveEventLive)

	cfg := pushCfg()
	cfg.MaxAttempts = 2
	u := NewPush(pool, &fakeSender{}, cfg)

	for i := 0; i < 4; i++ {
		if err := u.Update(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var attempts int
	if err := pool.QueryRow(ctx,
		`select max(attempts) from live_events where match_id = $1`, fm.id).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts > cfg.MaxAttempts {
		t.Fatalf("попыток %d при потолке %d: событие разбирается вечно",
			attempts, cfg.MaxAttempts)
	}
}
