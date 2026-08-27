package live

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StVl/tennis-backend/internal/apns"
	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/storage"
)

// Sender — то, что умеет отправить пуш. Интерфейс, а не *apns.Client: пушер
// тестируется без Apple, а подмена транспорта внутри клиента этого не даёт —
// проверять надо решения пушера, а не HTTP.
type Sender interface {
	Send(ctx context.Context, n apns.Notification) error
}

// PushUpdater разбирает outbox live_events.
//
// Это единственный потребитель очереди. Он не опрашивает поллер и ничего не
// знает про источник: событие уже записано в той же транзакции, что и смена
// статуса, поэтому пуш не может уйти о переходе, которого не было.
type PushUpdater struct {
	pool   *pgxpool.Pool
	sender Sender
	cfg    config.PushConfig
}

func NewPush(pool *pgxpool.Pool, sender Sender, cfg config.PushConfig) *PushUpdater {
	return &PushUpdater{pool: pool, sender: sender, cfg: cfg}
}

func (u *PushUpdater) Name() string { return "live-push" }

func (u *PushUpdater) Update(ctx context.Context) error {
	now, err := storage.DBNow(ctx, u.pool)
	if err != nil {
		return fmt.Errorf("read db clock: %w", err)
	}

	// Уборка идёт до проверки рубильника и до разбора очереди: сбой ленты
	// посреди матча не даёт события об окончании вовсе, и без неё карточка
	// висит, пока iOS не снимет её сама.
	u.sweepStale(ctx, now)

	if !u.cfg.Enabled {
		return nil
	}

	lock, acquired, err := storage.AcquireLiveLock(ctx, u.pool, storage.LiveLockPush)
	if err != nil {
		return fmt.Errorf("acquire push lock: %w", err)
	}
	if !acquired {
		return nil
	}
	defer lock.Release(ctx)

	events, err := storage.ClaimLiveEvents(ctx, u.pool, u.cfg.BatchSize, u.cfg.MaxAttempts, now)
	if err != nil {
		return fmt.Errorf("claim events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	var sent, ended, failed int
	for _, e := range events {
		var err error
		switch e.Event {
		case storage.LiveEventLive:
			sent, err = u.pushStart(ctx, e, now, sent)
		case storage.LiveEventFinished:
			ended, err = u.pushEnd(ctx, e, now, ended)
		default:
			// suspended/resumed пишутся ingest'ом, но продуктового решения о
			// поведении карточки нет. Помечаем разобранными, чтобы очередь не
			// росла, и не выдумываем поведение.
			err = nil
		}
		if err != nil {
			failed++
			_ = storage.FailLiveEvent(ctx, u.pool, e.ID, err.Error())
			slog.Error("live-push: event failed", "event_id", e.ID, "event", e.Event,
				"attempts", e.Attempts, "error", err)
			continue
		}
		if err := storage.ConsumeLiveEvent(ctx, u.pool, e.ID, now); err != nil {
			slog.Error("live-push: marking event consumed failed", "event_id", e.ID, "error", err)
		}
	}
	slog.Info("live-push: batch done",
		"events", len(events), "started", sent, "ended", ended, "failed", failed)
	return nil
}

func (u *PushUpdater) pushStart(ctx context.Context, e storage.LiveEvent,
	now time.Time, sent int) (int, error) {

	targets, err := storage.StartAudience(ctx, u.pool, e.MatchID)
	if err != nil {
		return sent, err
	}
	for _, t := range targets {
		payload := startPayload(now)
		if err := u.send(ctx, t, apns.Notification{
			Token: t.Token, Type: apns.PushStart, Payload: payload,
		}); err != nil {
			// Одна мёртвая подписка не должна ронять рассылку остальным.
			slog.Warn("live-push: start push failed", "user_id", t.UserID, "error", err)
			continue
		}
		if err := storage.OpenSession(ctx, u.pool, t.UserID, e.MatchID, now); err != nil {
			slog.Error("live-push: opening a session failed", "user_id", t.UserID, "error", err)
			continue
		}
		sent++
	}
	return sent, nil
}

func (u *PushUpdater) pushEnd(ctx context.Context, e storage.LiveEvent,
	now time.Time, ended int) (int, error) {

	sessions, err := storage.OpenSessionsForMatch(ctx, u.pool, e.MatchID)
	if err != nil {
		return ended, err
	}
	for _, s := range sessions {
		u.endSession(ctx, s, now)
		ended++
	}
	return ended, nil
}

// endSession гасит карточку и всегда закрывает сессию.
//
// Закрывает даже когда отправить нечем или отправка не удалась: незакрытая
// сессия блокирует будущий push-to-start по этому же матчу уникальным индексом,
// то есть одна неудача лишила бы пользователя карточек на этот матч навсегда.
func (u *PushUpdater) endSession(ctx context.Context, s storage.Session, now time.Time) {
	if s.UpdateToken != nil {
		payload := endPayload(now.Add(u.cfg.DismissAfter))
		err := u.send(ctx, storage.PushTarget{UserID: s.UserID, Token: *s.UpdateToken},
			apns.Notification{
				Token: *s.UpdateToken, Type: apns.PushEnd, Payload: payload,
				DismissAt: now.Add(u.cfg.DismissAfter),
			})
		if err != nil {
			slog.Warn("live-push: end push failed; closing the session anyway",
				"session_id", s.ID, "error", err)
		}
	} else {
		// Клиент не прислал update-токен: гасить нечем, карточку снимет сама
		// iOS. Сессию закрываем, иначе она навсегда блокирует новый старт.
		slog.Info("live-push: session has no update token; closing without a push",
			"session_id", s.ID, "user_id", s.UserID)
	}
	if err := storage.EndSession(ctx, u.pool, s.ID, now); err != nil {
		slog.Error("live-push: closing a session failed", "session_id", s.ID, "error", err)
	}
}

func (u *PushUpdater) sweepStale(ctx context.Context, now time.Time) {
	if u.cfg.MaxSessionAge <= 0 {
		return
	}
	stale, err := storage.StaleSessions(ctx, u.pool, now.Add(-u.cfg.MaxSessionAge))
	if err != nil {
		slog.Error("live-push: reading stale sessions failed", "error", err)
		return
	}
	for _, s := range stale {
		slog.Warn("live-push: force-ending a session past max age",
			"session_id", s.ID, "match_id", s.MatchID, "age", now.Sub(s.StartedAt))
		u.endSession(ctx, s, now)
	}
}

// send отправляет и удаляет токен, который Apple объявила мёртвым.
func (u *PushUpdater) send(ctx context.Context, t storage.PushTarget, n apns.Notification) error {
	err := u.sender.Send(ctx, n)
	switch {
	case errors.Is(err, apns.ErrUnregistered):
		if delErr := storage.DeletePushToken(ctx, u.pool, t.Token); delErr != nil {
			slog.Error("live-push: dropping an unregistered token failed", "error", delErr)
		}
		return err
	case errors.Is(err, apns.ErrBadDeviceToken):
		// Чаще всего это несовпадение окружения, а не битый токен: удалять
		// нельзя, иначе неверно настроенный хост вычистит живые подписки.
		slog.Error("live-push: BadDeviceToken; check APNS_HOST against the token's env",
			"user_id", t.UserID, "token_env", t.Env)
		return err
	}
	return err
}

// Полезная нагрузка не несёт счёта — карточка показывает присутствие на корте.
// Правило 1 действует и здесь, на последнем шаге, где нарушить его проще всего.
func startPayload(now time.Time) map[string]any {
	return map[string]any{"aps": map[string]any{
		"timestamp":       now.Unix(),
		"event":           "start",
		"content-state":   map[string]any{},
		"attributes-type": "MatchActivityAttributes",
		"attributes":      map[string]any{},
		"alert":           map[string]any{"title": "", "body": ""},
	}}
}

func endPayload(dismissAt time.Time) map[string]any {
	return map[string]any{"aps": map[string]any{
		"timestamp":      time.Now().Unix(),
		"event":          "end",
		"dismissal-date": dismissAt.Unix(),
		"content-state":  map[string]any{},
	}}
}
