package live

import (
	"testing"
	"time"

	"github.com/StVl/tennis-backend/internal/config"
)

func decideCfg() config.LiveConfig {
	return config.LiveConfig{
		DailyQuota:    100,
		Reserve:       20,
		MinInterval:   5 * time.Minute,
		MaxInterval:   20 * time.Minute,
		StaleInterval: 20 * time.Minute,
		StaleDailyCap: 12,
		WatchTail:     6 * time.Hour,
	}
}

// полдень UTC: подальше от границы суток, чтобы горизонт её не обрезал
var noon = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

func ptr(t time.Time) *time.Time { return &t }

func TestDecideModes(t *testing.T) {
	cfg := decideCfg()
	fresh := ptr(noon.Add(-time.Hour))

	cases := []struct {
		name     string
		snap     Snapshot
		wantMode Mode
		wantPoll bool
	}{
		{
			name:     "нет окон, ничего не живо, расписание свежее -> спим",
			snap:     Snapshot{LastScheduleRunAt: fresh},
			wantMode: ModeAsleep,
		},
		{
			name: "матч живой -> опрашиваем независимо от расписания",
			snap: Snapshot{ActiveMatches: 1, LastScheduleRunAt: fresh},
			// окон нет, но узнать об окончании больше неоткуда
			wantMode: ModeActive, wantPoll: true,
		},
		{
			name: "окно накрывает сейчас -> наблюдаем",
			snap: Snapshot{
				Windows:           []Window{{From: noon.Add(-time.Minute), To: noon.Add(time.Hour)}},
				LastScheduleRunAt: fresh,
			},
			wantMode: ModeWatching, wantPoll: true,
		},
		{
			name: "окно начинается ровно сейчас -> включительно",
			snap: Snapshot{
				Windows:           []Window{{From: noon, To: noon.Add(time.Hour)}},
				LastScheduleRunAt: fresh,
			},
			wantMode: ModeWatching, wantPoll: true,
		},
		{
			name: "окно кончилось ровно сейчас -> исключительно",
			snap: Snapshot{
				Windows:           []Window{{From: noon.Add(-time.Hour), To: noon}},
				LastScheduleRunAt: fresh,
			},
			wantMode: ModeAsleep,
		},
		{
			name:     "расписание протухло -> отказ наружу, а не тишина",
			snap:     Snapshot{LastScheduleRunAt: ptr(noon.Add(-13 * time.Hour))},
			wantMode: ModeStaleSafe, wantPoll: true,
		},
		{
			name:     "прогонов Job A не было ни одного -> тоже протухло",
			snap:     Snapshot{LastScheduleRunAt: nil},
			wantMode: ModeStaleSafe, wantPoll: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.snap, cfg, noon)
			if got.Mode != tc.wantMode || got.Poll != tc.wantPoll {
				t.Fatalf("%s; ожидалось mode=%s poll=%v", got, tc.wantMode, tc.wantPoll)
			}
		})
	}
}

// Регулятор ОБЯЗАН уметь сказать «не опрашиваю». max(left,1) плюс потолок
// означали бы, что при исчерпанной квоте он вернёт «раз в 20 минут» и
// продолжит тратить — то есть заявленная гарантия не выполняется.
func TestDecideRefusesWhenQuotaExhausted(t *testing.T) {
	cfg := decideCfg()
	snap := Snapshot{
		ActiveMatches:     1,
		LastScheduleRunAt: ptr(noon.Add(-time.Hour)),
		RequestsToday:     cfg.DailyQuota - cfg.Reserve, // left = 0
	}
	got := Decide(snap, cfg, noon)
	if got.Poll {
		t.Fatalf("%s: при исчерпанной квоте опрашивать нельзя", got)
	}
	if got.Reason != "quota_exhausted" {
		t.Errorf("reason = %q, ожидалось quota_exhausted", got.Reason)
	}
}

// Цикл может стоить больше одного запроса (пагинация), поэтому резервируем
// худший случай, а не единицу.
func TestDecideReservesWorstCaseCycle(t *testing.T) {
	cfg := decideCfg()
	snap := Snapshot{
		ActiveMatches:     1,
		LastScheduleRunAt: ptr(noon.Add(-time.Hour)),
		RequestsToday:     cfg.DailyQuota - cfg.Reserve - 1, // left = 1 < maxPagesPerRun
	}
	if got := Decide(snap, cfg, noon); got.Poll {
		t.Fatalf("%s: с остатком в один запрос цикл начинать нельзя", got)
	}
}

// STALE-SAFE получает СВОЙ интервал. На минимальном он стоил бы 288 запросов
// в сутки против квоты в 100: отказ «наружу» превратился бы в полную тишину.
func TestDecideStaleSafeUsesOwnIntervalAndCap(t *testing.T) {
	cfg := decideCfg()

	got := Decide(Snapshot{LastScheduleRunAt: nil}, cfg, noon)
	if got.Interval != cfg.StaleInterval {
		t.Errorf("интервал %s, ожидался StaleInterval %s", got.Interval, cfg.StaleInterval)
	}

	capped := Decide(Snapshot{LastScheduleRunAt: nil, StaleRequestsToday: cfg.StaleDailyCap}, cfg, noon)
	if capped.Poll {
		t.Fatalf("%s: суточный потолок STALE-SAFE должен останавливать опрос", capped)
	}
	if capped.Reason != "stale_cap_reached" {
		t.Errorf("reason = %q", capped.Reason)
	}
}

// Пол интервала не должен молча удваиваться из-за дискретности тика.
func TestDecideTickSlack(t *testing.T) {
	cfg := decideCfg()
	snap := Snapshot{
		ActiveMatches:     1,
		LastScheduleRunAt: ptr(noon.Add(-time.Hour)),
		// прошло 4м57с при поле 5м: без запаса тик был бы пропущен,
		// и реальный интервал стал бы десятью минутами
		LastPollAt: ptr(noon.Add(-4*time.Minute - 57*time.Second)),
	}
	if got := Decide(snap, cfg, noon); !got.Poll {
		t.Fatalf("%s: тик пропущен из-за трёх секунд — пол интервала удвоился", got)
	}
}

func TestWatchMinutesUnionsRatherThanSums(t *testing.T) {
	// день турнира Большого шлема: десять фикстур в один и тот же час
	var windows []Window
	for i := 0; i < 10; i++ {
		windows = append(windows, Window{From: noon, To: noon.Add(6 * time.Hour)})
	}
	got := WatchMinutes(windows, noon, noon.Add(12*time.Hour))
	if got != 6*time.Hour {
		t.Fatalf("объединение = %s, ожидалось 6h; сумма дала бы 60h и прижала бы "+
			"интервал к потолку на весь день", got)
	}

	// разрывы сохраняются
	gap := []Window{
		{From: noon, To: noon.Add(time.Hour)},
		{From: noon.Add(3 * time.Hour), To: noon.Add(4 * time.Hour)},
	}
	if got := WatchMinutes(gap, noon, noon.Add(12*time.Hour)); got != 2*time.Hour {
		t.Fatalf("с разрывом = %s, ожидалось 2h", got)
	}

	// частично перекрывающиеся сливаются
	overlap := []Window{
		{From: noon, To: noon.Add(2 * time.Hour)},
		{From: noon.Add(time.Hour), To: noon.Add(3 * time.Hour)},
	}
	if got := WatchMinutes(overlap, noon, noon.Add(12*time.Hour)); got != 3*time.Hour {
		t.Fatalf("перекрытие = %s, ожидалось 3h", got)
	}
}

// Горизонт обрезается концом суток квоты: иначе тик в 23:50 делит завтрашние
// минуты на сегодняшние запросы.
func TestDecideHorizonStopsAtQuotaDay(t *testing.T) {
	cfg := decideCfg()
	late := time.Date(2026, 8, 31, 23, 50, 0, 0, time.UTC)
	// вечерняя сессия тянется до 04:00 следующего дня
	snap := Snapshot{
		Windows:           []Window{{From: late.Add(-2 * time.Hour), To: late.Add(4 * time.Hour)}},
		LastScheduleRunAt: ptr(late.Add(-time.Hour)),
		RequestsToday:     60,
	}
	got := Decide(snap, cfg, late)
	// осталось 10 минут суток и left = 100-60-20 = 20 -> need прижимается к полу
	if got.Interval != cfg.MinInterval {
		t.Fatalf("интервал %s, ожидался пол %s: горизонт должен обрезаться концом суток",
			got.Interval, cfg.MinInterval)
	}
}

// Интервал монотонен: больше времени наблюдать — реже опрашиваем;
// больше запросов в остатке — чаще. Свойство ловит больше, чем таблица.
func TestDecideIntervalIsMonotonic(t *testing.T) {
	cfg := decideCfg()
	fresh := ptr(noon.Add(-time.Hour))

	prev := time.Duration(0)
	for hours := 1; hours <= 6; hours++ {
		snap := Snapshot{
			Windows:           []Window{{From: noon, To: noon.Add(time.Duration(hours) * time.Hour)}},
			LastScheduleRunAt: fresh,
			RequestsToday:     70,
		}
		got := Decide(snap, cfg, noon).Interval
		if got < prev {
			t.Fatalf("при %dh наблюдения интервал %s меньше предыдущего %s", hours, got, prev)
		}
		prev = got
	}

	prev = cfg.MaxInterval
	for spent := 78; spent >= 20; spent -= 10 {
		snap := Snapshot{
			Windows:           []Window{{From: noon, To: noon.Add(6 * time.Hour)}},
			LastScheduleRunAt: fresh,
			RequestsToday:     spent,
		}
		d := Decide(snap, cfg, noon)
		// При отказе интервал не вычисляется и равен нулю — сравнивать его с
		// вычисленными нельзя, это разные величины.
		if d.Reason == "quota_exhausted" {
			continue
		}
		if d.Interval > prev {
			t.Fatalf("при потраченных %d интервал %s больше предыдущего %s",
				spent, d.Interval, prev)
		}
		prev = d.Interval
	}
}

// ГЛАВНЫЙ тест регулятора: сутки шагами по 5 минут с обратной связью.
// Только он доказывает заявленное «квоту нельзя перерасходовать» — таблица
// проверяет отдельные точки, а перерасход возникает из накопления.
func TestGovernorNeverExceedsDailyQuota(t *testing.T) {
	cfg := decideCfg()
	dayStart := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)

	layouts := map[string][]Window{
		"тихий день: два матча вечером": {
			{From: dayStart.Add(18 * time.Hour), To: dayStart.Add(23 * time.Hour)},
		},
		"день Большого шлема: две сессии, 16 часов": {
			{From: dayStart.Add(11 * time.Hour), To: dayStart.Add(19 * time.Hour)},
			{From: dayStart.Add(17 * time.Hour), To: dayStart.Add(27 * time.Hour)},
		},
		"наблюдение круглые сутки": {
			{From: dayStart, To: dayStart.Add(24 * time.Hour)},
		},
		"десять фикстур в один час": func() []Window {
			var w []Window
			for i := 0; i < 10; i++ {
				w = append(w, Window{From: dayStart.Add(15 * time.Hour), To: dayStart.Add(21 * time.Hour)})
			}
			return w
		}(),
		"расписания нет вовсе (STALE-SAFE весь день)": nil,
	}

	for name, windows := range layouts {
		t.Run(name, func(t *testing.T) {
			snap := Snapshot{Windows: windows}
			if windows != nil {
				snap.LastScheduleRunAt = ptr(dayStart)
			}
			polls := 0

			for tick := 0; tick < 24*12; tick++ { // тик раз в 5 минут
				now := dayStart.Add(time.Duration(tick) * 5 * time.Minute)
				if windows != nil {
					// расписание освежается, пока окна есть
					snap.LastScheduleRunAt = ptr(now.Add(-time.Hour))
				}
				d := Decide(snap, cfg, now)
				if !d.Poll {
					continue
				}
				polls++
				// обратная связь: цикл может стоить больше одного запроса
				snap.RequestsToday += maxPagesPerRun
				if d.Mode == ModeStaleSafe {
					snap.StaleRequestsToday += maxPagesPerRun
				}
				snap.LastPollAt = ptr(now)
			}

			if snap.RequestsToday > cfg.DailyQuota {
				t.Fatalf("потрачено %d запросов при квоте %d (%d опросов)",
					snap.RequestsToday, cfg.DailyQuota, polls)
			}
			// и не должен быть настолько осторожным, что не опрашивает вовсе
			if polls == 0 {
				t.Fatal("ни одного опроса за сутки — регулятор слишком осторожен")
			}
			t.Logf("опросов %d, потрачено %d из %d", polls, snap.RequestsToday, cfg.DailyQuota)
		})
	}
}
