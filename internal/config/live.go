package config

import (
	"fmt"
	"os"
	"time"

	"github.com/robfig/cron/v3"
)

// LiveConfig — настройки ingest'а live-статусов. Отдельной структурой, чтобы
// не раздувать Config: их полтора десятка, и почти все — тюнинг регулятора квоты.
type LiveConfig struct {
	// Рубильник. Проверяется на каждом прогоне, а не при регистрации.
	Enabled bool

	BaseURL string
	APIKey  string

	// Частота ТИКОВ Job B, а не частота запросов: большинство тиков не тратят
	// ничего. Реальный интервал выбирает регулятор квоты.
	Cron string
	// Job A: обновление расписания наших игроков.
	ScheduleCron  string
	UpdateTimeout time.Duration

	// Суточная квота вендора и резерв под повторы. Регулятор считает
	// left = DailyQuota - потрачено - Reserve и при left <= 0 ОТКАЗЫВАЕТСЯ
	// опрашивать, а не продолжает с максимальным интервалом.
	DailyQuota int
	Reserve    int

	MinInterval time.Duration
	MaxInterval time.Duration

	// Режим STALE-SAFE (расписание протухло) получает СВОЙ интервал и свой
	// суточный потолок. На минимальном интервале он стоил бы 288 запросов в
	// сутки против квоты в 100 — то есть отказ «наружу» превращался бы в
	// полную тишину к обеду.
	StaleInterval time.Duration
	StaleDailyCap int

	// Насколько раньше назначенного времени открывать окно наблюдения и
	// насколько долго держать его открытым после. Хвост длинный не для запаса:
	// матч начинается, когда закончится предыдущий на том же корте, и в снятом
	// срезе фикстуры висели upcoming спустя 4 часа 44 минуты после слота.
	WatchLead time.Duration
	WatchTail time.Duration

	// Окно поиска нашего матча вокруг времени фикстуры.
	MatchWindow time.Duration
	// Аварийный выход: матч не может идти дольше. Работает, даже когда опрос
	// вообще не идёт (кончилась квота, лёг источник) — иначе ложная карточка
	// висит у всех подписчиков до вмешательства руками.
	MaxLiveAge time.Duration

	// Потолок обращений ленивого резолвера за цикл: он тратит ту же квоту.
	ResolveMaxPerCycle int

	// Создавать ли строки matches из фикстур (Phase 8). По умолчанию выключено:
	// это ослабление правила 3 из docs/live-status-ingest.md, и решение за
	// iOS-стороной.
	CreateMatches bool
}

// PushConfig — доставка Live Activity (Task 2).
type PushConfig struct {
	Enabled  bool
	Cron     string
	KeyID    string
	TeamID   string
	BundleID string
	// .p8 задаётся ОДНИМ из двух способов, и KeyBase64 имеет приоритет.
	//
	// KeyPath — для локальной работы: файл лежит рядом, в репозиторий не
	// попадает. На Railway положить файл некуда: единственное место, которое
	// попадает в образ, — сам репозиторий, а приватный ключ туда нельзя.
	// Поэтому KeyBase64 — содержимое .p8 в base64. Именно base64, а не PEM
	// как есть: перевод строки в переменной окружения переживает не всякий
	// деплой, а внутри base64 переносов нет вовсе.
	KeyPath   string
	KeyBase64 string
	// Хост Apple. Токен sandbox на боевом хосте даёт BadDeviceToken, причём
	// сообщение об этом ни на что не указывает.
	Host string
	// Имя Swift-типа ActivityAttributes: iOS создаёт активность из push-to-start
	// по нему, и знает его только клиент.
	AttributesType string
	BatchSize      int
	MaxAttempts    int
	// Пауза перед повторной попыткой. Без неё крон раз в минуту сжигает
	// MaxAttempts за минуты, и перебой у Apple длиннее этого теряет событие.
	RetryAfter    time.Duration
	DismissAfter  time.Duration
	MaxSessionAge time.Duration
	UpdateTimeout time.Duration
}

func loadPush() (PushConfig, error) {
	c := PushConfig{
		Enabled:        envBool("PUSH_ENABLED", false),
		Cron:           envOrDefault("PUSH_CRON", "* * * * *"),
		KeyID:          os.Getenv("APNS_KEY_ID"),
		TeamID:         os.Getenv("APNS_TEAM_ID"),
		BundleID:       os.Getenv("APNS_BUNDLE_ID"),
		KeyPath:        os.Getenv("APNS_KEY_PATH"),
		KeyBase64:      os.Getenv("APNS_KEY_BASE64"),
		Host:           envOrDefault("APNS_HOST", "https://api.sandbox.push.apple.com"),
		AttributesType: envOrDefault("APNS_ATTRIBUTES_TYPE", "MatchActivityAttributes"),
		BatchSize:      envInt("PUSH_BATCH_SIZE", 50),
		MaxAttempts:    envInt("PUSH_MAX_ATTEMPTS", 5),
		RetryAfter:     envDuration("PUSH_RETRY_AFTER", 2*time.Minute),
		DismissAfter:   envDuration("PUSH_DISMISS_AFTER", 30*time.Second),
		MaxSessionAge:  envDuration("PUSH_MAX_SESSION_AGE", 6*time.Hour),
		UpdateTimeout:  envDuration("PUSH_UPDATE_TIMEOUT", 45*time.Second),
	}
	if !c.Enabled {
		return c, nil
	}
	for name, v := range map[string]string{
		"APNS_KEY_ID": c.KeyID, "APNS_TEAM_ID": c.TeamID,
		"APNS_BUNDLE_ID": c.BundleID,
	} {
		if v == "" {
			return c, fmt.Errorf("%s is required when PUSH_ENABLED is true", name)
		}
	}
	if c.KeyBase64 == "" && c.KeyPath == "" {
		return c, fmt.Errorf(
			"either APNS_KEY_BASE64 or APNS_KEY_PATH is required when PUSH_ENABLED is true")
	}
	if _, err := cron.ParseStandard(c.Cron); err != nil {
		return c, fmt.Errorf("parse PUSH_CRON: %w", err)
	}
	return c, nil
}

func loadLive() (LiveConfig, error) {
	c := LiveConfig{
		Enabled:            envBool("LIVE_POLL_ENABLED", false),
		BaseURL:            envOrDefault("LIVE_API_BASE_URL", "https://api.livetennisapi.com/api/public/v1"),
		APIKey:             os.Getenv("LIVE_API_KEY"),
		Cron:               envOrDefault("LIVE_CRON", "*/5 * * * *"),
		ScheduleCron:       envOrDefault("LIVE_SCHEDULE_CRON", "0 */8 * * *"),
		UpdateTimeout:      envDuration("LIVE_UPDATE_TIMEOUT", 90*time.Second),
		DailyQuota:         envInt("LIVE_DAILY_QUOTA", 100),
		Reserve:            envInt("LIVE_RESERVE", 20),
		MinInterval:        envDuration("LIVE_MIN_INTERVAL", 5*time.Minute),
		MaxInterval:        envDuration("LIVE_MAX_INTERVAL", 20*time.Minute),
		StaleInterval:      envDuration("LIVE_STALE_INTERVAL", 20*time.Minute),
		StaleDailyCap:      envInt("LIVE_STALE_DAILY_CAP", 12),
		WatchLead:          envDuration("LIVE_WATCH_LEAD", 30*time.Minute),
		WatchTail:          envDuration("LIVE_WATCH_TAIL", 6*time.Hour),
		MatchWindow:        envDuration("LIVE_MATCH_WINDOW", 36*time.Hour),
		MaxLiveAge:         envDuration("LIVE_MAX_LIVE_AGE", 6*time.Hour),
		ResolveMaxPerCycle: envInt("LIVE_RESOLVE_MAX_PER_CYCLE", 2),
		CreateMatches:      envBool("LIVE_CREATE_MATCHES", false),
	}

	// Ключ обязателен ТОЛЬКО когда опрос включён. Иначе прод спокойно
	// поднимается без ключа и падает в тот момент, когда кто-то дёрнет
	// рубильник — то есть ночью посреди турнира.
	if c.Enabled && c.APIKey == "" {
		return c, fmt.Errorf("LIVE_API_KEY is required when LIVE_POLL_ENABLED is true")
	}

	for name, spec := range map[string]string{
		"LIVE_CRON":          c.Cron,
		"LIVE_SCHEDULE_CRON": c.ScheduleCron,
	} {
		if _, err := cron.ParseStandard(spec); err != nil {
			return c, fmt.Errorf("parse %s: %w", name, err)
		}
	}

	// Таймаут прогона должен быть строго меньше периода тиков: robfig/cron
	// перехлёстывающие прогоны не пропускает, и опечатка в конфиге дала бы
	// бесконечную очередь прогонов вместо ошибки.
	if period, ok := cronPeriod(c.Cron); ok && c.UpdateTimeout >= period {
		return c, fmt.Errorf(
			"LIVE_UPDATE_TIMEOUT (%s) must be shorter than the LIVE_CRON period (%s)",
			c.UpdateTimeout, period)
	}
	if c.MinInterval > c.MaxInterval {
		return c, fmt.Errorf("LIVE_MIN_INTERVAL (%s) must not exceed LIVE_MAX_INTERVAL (%s)",
			c.MinInterval, c.MaxInterval)
	}
	return c, nil
}

// cronPeriod — расстояние между двумя ближайшими срабатываниями. Для
// нерегулярных расписаний это оценка, поэтому используется только для проверки.
func cronPeriod(spec string) (time.Duration, bool) {
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		return 0, false
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first := sched.Next(base)
	second := sched.Next(first)
	if second.Before(first) {
		return 0, false
	}
	return second.Sub(first), true
}
