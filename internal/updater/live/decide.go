package live

import (
	"fmt"
	"sort"
	"time"

	"github.com/StVl/tennis-backend/internal/config"
)

// Режимы Job B. Порядок проверки — сверху вниз, побеждает первый подошедший.
type Mode string

const (
	// Мы держим матч живым: опрашиваем независимо от расписания, иначе не
	// узнаем, что он кончился.
	ModeActive Mode = "active"
	// Кто-то из наших вот-вот выйдет на корт или уже должен быть там.
	ModeWatching Mode = "watching"
	// Расписание протухло. Отказ «наружу»: опрашиваем всё равно, но реже и с
	// отдельным суточным потолком.
	ModeStaleSafe Mode = "stale_safe"
	// Смотреть некого. Возврат без единого запроса — это большинство тиков.
	ModeAsleep Mode = "asleep"
)

// tickSlack компенсирует дискретность тика.
//
// Без него интервал молча удваивается: cron срабатывает в :00, опрос
// заканчивается в :00:03, и следующий тик в :05:00 видит 4м57с < 5м и
// пропускает. Реальный пол становится десятью минутами вместо пяти, и заметить
// это можно только по счётчику запросов.
const tickSlack = 30 * time.Second

// maxPagesPerRun — сколько запросов может стоить один цикл в худшем случае.
// Резервируем именно столько, а не один: иначе цикл начинается с остатком в
// один запрос и обрывается на середине пагинации.
const maxPagesPerRun = 2

// Window — отрезок времени, который нужно наблюдать.
type Window struct{ From, To time.Time }

// Snapshot — всё, что решение читает из БД. Один запрос, одна структура,
// дальше чистая функция: так решение тестируется без БД, без сети и без часов.
type Snapshot struct {
	// Сколько матчей мы сейчас держим live (или считаем им пропуски).
	ActiveMatches int
	// Окна наблюдения из live_schedule, уже с учётом lead/tail.
	Windows []Window
	// Запросов потрачено сегодня, ОБОИМИ джобами: квота у них общая.
	RequestsToday int
	// Запросов потрачено сегодня в режиме STALE-SAFE.
	StaleRequestsToday int
	LastPollAt         *time.Time
	// Последний успешный прогон Job A. nil означает «ни одного» и по
	// определению считается протухшим: иначе на первом запуске отказа
	// «наружу» не было бы вовсе.
	LastScheduleRunAt *time.Time
}

// Decision — что делать на этом тике.
type Decision struct {
	Mode Mode
	Poll bool
	// Требуемый интервал между опросами по мнению регулятора.
	Interval time.Duration
	// Причина пропуска: попадает в live_ingest_runs.skipped_reason.
	Reason string
}

// Decide — чистая функция: тот же Snapshot и тот же now дают тот же ответ.
// now передаётся параметром, а не берётся из часов, именно ради тестируемости.
func Decide(s Snapshot, cfg config.LiveConfig, now time.Time) Decision {
	mode := pickMode(s, cfg, now)
	if mode == ModeAsleep {
		return Decision{Mode: mode, Reason: "asleep"}
	}

	// Жёсткий отказ ДО арифметики. Без него max(left,1) в знаменателе плюс
	// потолок означают, что регулятор не умеет сказать «не опрашиваю»: при
	// исчерпанной квоте он вернул бы «раз в 20 минут» и продолжил тратить.
	left := cfg.DailyQuota - s.RequestsToday - cfg.Reserve
	if left < maxPagesPerRun {
		return Decision{Mode: mode, Reason: "quota_exhausted"}
	}

	if mode == ModeStaleSafe && s.StaleRequestsToday >= cfg.StaleDailyCap {
		return Decision{Mode: mode, Reason: "stale_cap_reached"}
	}

	interval := requiredInterval(s, cfg, now, mode, left)
	if s.LastPollAt != nil && now.Sub(*s.LastPollAt) < interval-tickSlack {
		return Decision{Mode: mode, Interval: interval, Reason: "too_soon"}
	}
	return Decision{Mode: mode, Poll: true, Interval: interval}
}

func pickMode(s Snapshot, cfg config.LiveConfig, now time.Time) Mode {
	// Первым делом ACTIVE: пока мы держим карточку поднятой, расписание не
	// имеет значения — узнать об окончании больше неоткуда.
	if s.ActiveMatches > 0 {
		return ModeActive
	}
	for _, w := range s.Windows {
		if !now.Before(w.From) && now.Before(w.To) {
			return ModeWatching
		}
	}
	if s.LastScheduleRunAt == nil || now.Sub(*s.LastScheduleRunAt) > staleAfter {
		return ModeStaleSafe
	}
	return ModeAsleep
}

// requiredInterval — сколько ждать между опросами.
//
//	need = watch_minutes / requests_left, зажатое между полом и потолком.
//
// Это обратная связь: чем меньше осталось запросов на оставшееся время
// наблюдения, тем реже опрашиваем. STALE-SAFE в ней не участвует — у него свой
// интервал, потому что при протухшем расписании watch_minutes равно нулю, и
// формула вернула бы пол, то есть 288 запросов в сутки против квоты в 100.
func requiredInterval(s Snapshot, cfg config.LiveConfig, now time.Time,
	mode Mode, left int) time.Duration {

	if mode == ModeStaleSafe {
		return cfg.StaleInterval
	}

	// Горизонт обрезается концом суток квоты: иначе тик в 23:50 при вечерней
	// сессии до 04:00 делит завтрашние минуты на сегодняшние запросы.
	horizon := endOfUTCDay(now)
	if tail := now.Add(cfg.WatchTail); tail.Before(horizon) {
		horizon = tail
	}

	watch := WatchMinutes(s.Windows, now, horizon)
	if mode == ModeActive && watch <= 0 {
		// Матч идёт, но окон уже нет: расписание про него забыло. Смотреть
		// всё равно надо — до конца горизонта.
		watch = horizon.Sub(now)
	}
	if watch <= 0 {
		return cfg.MaxInterval
	}

	need := watch / time.Duration(left)
	if need < cfg.MinInterval {
		return cfg.MinInterval
	}
	if need > cfg.MaxInterval {
		return cfg.MaxInterval
	}
	return need
}

// WatchMinutes — сколько времени осталось наблюдать в промежутке [from, to).
//
// ОБЪЕДИНЕНИЕ пересекающихся окон, а не сумма. На турнире Большого шлема
// десять фикстур в 15:00 дают в сумме около 3900 минут и в объединении около
// 400: с суммой интервал прижимался бы к потолку весь день — ровно в тот день,
// когда важнее всего опрашивать часто, — и десятки запросов остались бы
// неистраченными.
func WatchMinutes(windows []Window, from, to time.Time) time.Duration {
	if !from.Before(to) {
		return 0
	}
	clipped := make([]Window, 0, len(windows))
	for _, w := range windows {
		if w.From.Before(from) {
			w.From = from
		}
		if w.To.After(to) {
			w.To = to
		}
		if w.From.Before(w.To) {
			clipped = append(clipped, w)
		}
	}
	if len(clipped) == 0 {
		return 0
	}
	sort.Slice(clipped, func(i, j int) bool { return clipped[i].From.Before(clipped[j].From) })

	var total time.Duration
	cur := clipped[0]
	for _, w := range clipped[1:] {
		if w.From.After(cur.To) {
			total += cur.To.Sub(cur.From)
			cur = w
			continue
		}
		if w.To.After(cur.To) {
			cur.To = w.To
		}
	}
	return total + cur.To.Sub(cur.From)
}

func endOfUTCDay(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).
		AddDate(0, 0, 1)
}

func (d Decision) String() string {
	return fmt.Sprintf("mode=%s poll=%v interval=%s reason=%s",
		d.Mode, d.Poll, d.Interval, d.Reason)
}
