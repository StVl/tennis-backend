package live

import (
	"context"
	"errors"
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
		AttributesType: "MatchActivityAttributes",
		RetryAfter:     0,
		DismissAfter:   30 * time.Second, MaxSessionAge: 6 * time.Hour,
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

// emitEvent кладёт событие в outbox.
//
// Для 'live' заодно приводит матч в состояние, которое оставляет за собой
// FlipLive: status='live' плюс наш флаг. Это не удобство теста, а требование
// боевого пути — событие 'live' физически не может существовать без них,
// потому что FlipLive пишет матч, флаг и событие в ОДНОЙ транзакции. Пушер
// это состояние перепроверяет (storage.MatchStillLive), и фикстура без него
// проверяла бы путь, которого в проде не бывает.
func emitEvent(t *testing.T, pool *pgxpool.Pool, matchID int64, event string) {
	t.Helper()
	ctx := context.Background()

	if event == storage.LiveEventLive {
		if _, err := pool.Exec(ctx,
			`update matches set status = 'live' where id = $1`, matchID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into live_flags (match_id, source, state, prior_status, flipped_at)
			values ($1, $2, 'on_court', 'scheduled', now())
			on conflict (match_id) do nothing`,
			matchID, storage.LiveSourceAPI); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pool.Exec(ctx, `
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
	if apsField(t, ends[0], "dismissal-date") == nil {
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

	if _, err := storage.OpenSession(ctx, pool, userID, fm.id,
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

	if _, err := storage.OpenSession(ctx, pool, userID, fm.id,
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

	// Аудитория обязана быть НЕПУСТОЙ, иначе pushStart возвращает nil, событие
	// потребляется с первой попытки и потолок не проверяется вовсе — тест был
	// бы зелёным даже со сломанным ограничителем.
	pushEnv(t, pool, fm.id)
	emitEvent(t, pool, fm.id, storage.LiveEventLive)

	cfg := pushCfg()
	cfg.MaxAttempts = 2
	// Отказ, который в принципе может пройти со второй попытки: событие
	// возвращается в очередь, пока не кончатся попытки.
	u := NewPush(pool, &fakeSender{err: errors.New("apns: status 503")}, cfg)

	for i := 0; i < 4; i++ {
		if err := u.Update(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var (
		attempts int
		consumed bool
	)
	if err := pool.QueryRow(ctx, `
		select attempts, consumed_at is not null from live_events
		where match_id = $1 and event = $2`,
		fm.id, storage.LiveEventLive).Scan(&attempts, &consumed); err != nil {
		t.Fatal(err)
	}
	if attempts != cfg.MaxAttempts {
		t.Fatalf("попыток %d при потолке %d: четыре прогона должны упереться в потолок "+
			"и остановиться ровно на нём", attempts, cfg.MaxAttempts)
	}
	if consumed {
		t.Error("событие помечено разобранным, хотя ни один пуш не ушёл")
	}
}

func apsField(t *testing.T, n apns.Notification, key string) any {
	t.Helper()
	payload, ok := n.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload не map: %T", n.Payload)
	}
	aps, ok := payload["aps"].(map[string]any)
	if !ok {
		t.Fatalf("в payload нет aps: %v", payload)
	}
	return aps[key]
}

// Отказ Apple, который может пройти со второй попытки, НЕ должен считаться
// доставкой: иначе перебой молча лишает пользователя карточки, а очередь
// говорит, что всё отправлено. И слот сессии должен освободиться, иначе
// повторная попытка пропустит этого пользователя как «уже с карточкой».
func TestPushRetriesWhenSendFails(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	pushEnv(t, pool, fm.id)
	ctx := context.Background()
	emitEvent(t, pool, fm.id, storage.LiveEventLive)

	sender := &fakeSender{err: errors.New("apns: status 503")}
	u := NewPush(pool, sender, pushCfg())
	if err := u.Update(ctx); err != nil {
		t.Fatal(err)
	}

	var (
		consumed bool
		attempts int
		lastErr  *string
	)
	if err := pool.QueryRow(ctx, `
		select consumed_at is not null, attempts, last_error
		from live_events where match_id = $1`, fm.id).
		Scan(&consumed, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Fatal("событие помечено разобранным, хотя пуш не ушёл: повторить его больше нечем")
	}
	if attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1", attempts)
	}
	if lastErr == nil {
		t.Error("last_error пуст: по чему тогда разбирать, почему не доставили")
	}
	if n := openSessions(t, pool, fm.id); n != 0 {
		t.Fatalf("открытых сессий %d: слот занят под пуш, которого не было", n)
	}

	// Apple вернулась в строй — повтор должен доставить.
	sender.err = nil
	if err := u.Update(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`select consumed_at is not null from live_events where match_id = $1`, fm.id).
		Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if !consumed {
		t.Error("повтор прошёл, а событие всё ещё не разобрано")
	}
	if got := len(sender.byType(apns.PushStart)); got != 2 {
		t.Errorf("start-пушей %d, ожидалось 2 (неудачный и удачный)", got)
	}
	if n := openSessions(t, pool, fm.id); n != 1 {
		t.Errorf("открытых сессий %d, ожидалась 1", n)
	}
}

// Без личности матча в attributes iOS нечем создать активность, а клиенту
// нечем ответить на PUT /v1/users/me/live-activities/{match_id}.
func TestStartPayloadCarriesMatchIdentity(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	pushEnv(t, pool, fm.id)
	ctx := context.Background()
	emitEvent(t, pool, fm.id, storage.LiveEventLive)

	sender := &fakeSender{}
	if err := NewPush(pool, sender, pushCfg()).Update(ctx); err != nil {
		t.Fatal(err)
	}
	starts := sender.byType(apns.PushStart)
	if len(starts) != 1 {
		t.Fatalf("start-пушей %d, ожидался 1", len(starts))
	}
	attrs, ok := apsField(t, starts[0], "attributes").(map[string]any)
	if !ok {
		t.Fatalf("attributes не объект: %#v", apsField(t, starts[0], "attributes"))
	}
	if attrs["match_id"] != fm.id {
		t.Errorf("match_id = %v, ожидался %d", attrs["match_id"], fm.id)
	}
	players, ok := attrs["players"].([]storage.CardPlayer)
	if !ok || len(players) != 2 {
		t.Fatalf("players = %#v, ожидались двое", attrs["players"])
	}
	for _, p := range players {
		if p.Slug == "" || p.Name == "" {
			t.Errorf("игрок без имени или слага: %#v", p)
		}
	}
	if apsField(t, starts[0], "attributes-type") != "MatchActivityAttributes" {
		t.Error("attributes-type пуст: iOS не поймёт, во что декодировать attributes")
	}
}

func openSessions(t *testing.T, pool *pgxpool.Pool, matchID int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `
		select count(*) from live_activity_sessions
		where match_id = $1 and ended_at is null`, matchID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Накопленное событие НЕ поднимает карточку, если матч уже кончился.
//
// Это ровно тот случай, ради которого проверка и добавлена: при
// PUSH_ENABLED=false события копятся неограниченно (retention удаляет только
// потреблённые), и включение рубильника рассылало бы push-to-start по всему
// хвосту — для матчей, сыгранных часы назад. Погасить такую карточку нечем:
// update_token приходит только от уже запущенной активности, а её нет.
func TestStalePushStartIsSkippedWhenMatchIsOver(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	pushEnv(t, pool, fm.id)
	ctx := context.Background()

	// матч шёл и кончился: событие в очереди осталось, флага и статуса уже нет
	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	if _, err := pool.Exec(ctx, `delete from live_flags where match_id = $1`, fm.id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`update matches set status = 'completed' where id = $1`, fm.id); err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{}
	if err := NewPush(pool, sender, pushCfg()).Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := len(sender.byType(apns.PushStart)); got != 0 {
		t.Fatalf("отправлено %d push-to-start по протухшему событию, ожидалось 0", got)
	}

	// событие разобрано, а не оставлено на повтор: матч не оживёт
	var pending int
	if err := pool.QueryRow(ctx,
		`select count(*) from live_events where match_id = $1 and consumed_at is null`,
		fm.id).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("непотреблённых событий %d, ожидалось 0: повторять протухшее событие нечем", pending)
	}

	// и слот сессии не занят — иначе настоящий старт потом не пройдёт
	var sessions int
	if err := pool.QueryRow(ctx,
		`select count(*) from live_activity_sessions where match_id = $1`, fm.id).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("сессий %d, ожидалось 0", sessions)
	}
}

// Матч, сходивший live -> finished -> live, даёт ОДНУ карточку, а не две.
//
// При выключенном пушере события копятся, и повторный вход в live — обычное
// дело: Derive пускает обратно без паузы, FlipOut возвращает 'scheduled'.
// В очереди тогда лежат live(1), finished(2), live(3), и на вопрос «идёт ли
// матч сейчас» все три отвечают «да». Без привязки к периоду пушер поднял бы
// карточку по live(1), закрыл её по finished(2) без пуша (update_token ещё не
// пришёл) и поднял вторую по live(3): две карточки на один матч, причём первую
// погасить уже нечем — её сессия закрыта, и sweepStale её не видит.
func TestReflippedMatchRaisesOnlyOneCard(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	pushEnv(t, pool, fm.id)
	ctx := context.Background()

	// первый период: поднялся и кончился
	emitEvent(t, pool, fm.id, storage.LiveEventLive)
	if _, _, err := storage.FlipOut(ctx, pool, fm.id,
		storage.LiveEventFinished, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// второй период: матч идёт ПРЯМО СЕЙЧАС
	emitEvent(t, pool, fm.id, storage.LiveEventLive)

	sender := &fakeSender{}
	if err := NewPush(pool, sender, pushCfg()).Update(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if got := len(sender.byType(apns.PushStart)); got != 1 {
		t.Fatalf("push-to-start %d, ожидался 1: событие прошлого периода не должно "+
			"поднимать вторую карточку", got)
	}

	var open, total int
	if err := pool.QueryRow(ctx, `
		select count(*) filter (where ended_at is null), count(*)
		from live_activity_sessions where match_id = $1`, fm.id).Scan(&open, &total); err != nil {
		t.Fatal(err)
	}
	if total != 1 || open != 1 {
		t.Errorf("сессий всего %d, открытых %d; ожидалось 1 и 1: закрытая сессия — это "+
			"карточка, которую уже нечем погасить", total, open)
	}
}
