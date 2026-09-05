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

	events, err := storage.ClaimLiveEvents(ctx, u.pool, u.cfg.BatchSize,
		u.cfg.MaxAttempts, now, u.cfg.RetryAfter)
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

	// Матч мог кончиться, пока событие лежало в очереди. Само по себе событие —
	// факт прошлого («тогда матч начался»), а push-to-start — утверждение о
	// настоящем («карточку показать СЕЙЧАС»), и вот это надо перепроверять.
	//
	// Иначе включение PUSH_ENABLED после любого простоя рассылает карточки по
	// всему накопленному хвосту: матчи давно сыграны, а погасить карточку
	// нечем — update_token приходит только от уже запущенной активности.
	// Событие при этом считается разобранным: повторять его смысла нет.
	live, err := storage.MatchStillLive(ctx, u.pool, e.MatchID, e.CreatedAt)
	if err != nil {
		return sent, fmt.Errorf("re-check match state: %w", err)
	}
	if !live {
		slog.Info("live-push: skipping a start push, the match is not live for this event",
			"match_id", e.MatchID, "event_id", e.ID, "event_age", now.Sub(e.CreatedAt))
		return sent, nil
	}

	targets, err := storage.StartAudience(ctx, u.pool, e.MatchID)
	if err != nil {
		return sent, err
	}
	if len(targets) == 0 {
		return sent, nil
	}
	card, err := storage.PushCardFor(ctx, u.pool, e.MatchID)
	if err != nil {
		return sent, fmt.Errorf("push card: %w", err)
	}

	var failed int
	for _, t := range targets {
		sessionID, err := storage.OpenSession(ctx, u.pool, t.UserID, e.MatchID, now)
		if err != nil {
			failed++
			slog.Error("live-push: opening a session failed", "user_id", t.UserID, "error", err)
			continue
		}
		if sessionID == 0 {
			// Слот уже занят: кто-то отправил старт раньше нас.
			continue
		}
		if err := u.send(ctx, t, apns.Notification{
			Token: t.Token, Type: apns.PushStart,
			Payload: startPayload(now, card, u.cfg.AttributesType),
		}); err != nil {
			if delErr := storage.DeleteSession(ctx, u.pool, sessionID); delErr != nil {
				slog.Error("live-push: releasing the session slot failed",
					"session_id", sessionID, "error", delErr)
			}
			// Мёртвый токен уже удалён, повторять его нечем; всё остальное
			// (429, 5xx, отвергнутый JWT) обязано вернуться следующей попыткой.
			if !errors.Is(err, apns.ErrUnregistered) {
				failed++
			}
			slog.Warn("live-push: start push failed", "user_id", t.UserID, "error", err)
			continue
		}
		sent++
	}
	// Событие не считается разобранным, пока хоть одна отправка может удаться:
	// иначе перебой у Apple молча лишает пользователя карточки, а очередь
	// говорит, что всё доставлено.
	if failed > 0 {
		return sent, fmt.Errorf("%d of %d start pushes failed", failed, len(targets))
	}
	return sent, nil
}

func (u *PushUpdater) pushEnd(ctx context.Context, e storage.LiveEvent,
	now time.Time, ended int) (int, error) {

	sessions, err := storage.OpenSessionsForMatch(ctx, u.pool, e.MatchID)
	if err != nil {
		return ended, err
	}
	var failed int
	for _, s := range sessions {
		if err := u.endSession(ctx, s, now); err != nil {
			failed++
			slog.Warn("live-push: end push failed", "session_id", s.ID, "error", err)
			continue
		}
		ended++
	}
	if failed > 0 {
		return ended, fmt.Errorf("%d of %d end pushes failed", failed, len(sessions))
	}
	return ended, nil
}

// endSession гасит карточку и закрывает сессию, если гасить удалось или гасить
// было нечем. При отказе, который может пройти со второй попытки, сессия
// остаётся открытой: только по ней и можно повторить end-пуш. Навсегда она так
// не залипнет — по возрасту её принудительно закроет sweepStale.
func (u *PushUpdater) endSession(ctx context.Context, s storage.Session, now time.Time) error {
	if s.UpdateToken == nil {
		// Клиент не прислал update-токен: гасить нечем, карточку снимет сама iOS.
		slog.Info("live-push: session has no update token; closing without a push",
			"session_id", s.ID, "user_id", s.UserID)
		return u.closeSession(ctx, s.ID, now)
	}
	err := u.send(ctx, storage.PushTarget{UserID: s.UserID, Token: *s.UpdateToken},
		apns.Notification{
			Token: *s.UpdateToken, Type: apns.PushEnd,
			Payload: endPayload(now, now.Add(u.cfg.DismissAfter)),
		})
	switch {
	case err == nil:
		return u.closeSession(ctx, s.ID, now)
	case errors.Is(err, apns.ErrUnregistered):
		// Токен мёртв: повторять нечем, карточку снимет iOS.
		return u.closeSession(ctx, s.ID, now)
	}
	return err
}

func (u *PushUpdater) closeSession(ctx context.Context, id int64, now time.Time) error {
	if err := storage.EndSession(ctx, u.pool, id, now); err != nil {
		return fmt.Errorf("close session %d: %w", id, err)
	}
	return nil
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
		if err := u.endSession(ctx, s, now); err != nil {
			// Возраст вышел: закрываем, даже если погасить не удалось, —
			// открытая сессия блокирует новый старт по этому матчу.
			slog.Warn("live-push: closing a stale session without extinguishing it",
				"session_id", s.ID, "error", err)
			if err := u.closeSession(ctx, s.ID, now); err != nil {
				slog.Error("live-push: closing a stale session failed", "error", err)
			}
		}
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
//
// attributes — статическая часть, из которой iOS СОЗДАЁТ активность: без
// личности матча карточку нечем нарисовать, а клиенту нечем ответить на
// PUT /v1/users/me/live-activities/{match_id}. Форма зафиксирована в README;
// attributes-type задаётся APNS_ATTRIBUTES_TYPE, потому что имя Swift-типа
// знает только клиент.
func startPayload(now time.Time, card storage.PushCard, attributesType string) map[string]any {
	return map[string]any{"aps": map[string]any{
		"timestamp":       now.Unix(),
		"event":           "start",
		"attributes-type": attributesType,
		"attributes": map[string]any{
			"match_id":        card.MatchID,
			"edition":         card.Edition,
			"tournament_name": card.TournamentName,
			"round":           card.Round,
			"players":         card.Players,
		},
		"content-state": map[string]any{"phase": "on_court"},
		"alert":         map[string]any{"title": "", "body": ""},
	}}
}

func endPayload(now, dismissAt time.Time) map[string]any {
	return map[string]any{"aps": map[string]any{
		"timestamp":      now.Unix(),
		"event":          "end",
		"dismissal-date": dismissAt.Unix(),
		"content-state":  map[string]any{"phase": "ended"},
	}}
}
