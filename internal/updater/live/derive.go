package live

import (
	"time"

	"github.com/StVl/tennis-backend/internal/livesource"
)

// FlagState — наше текущее знание о матче: держим ли мы его живым, в каком
// состоянии и сколько подходящих прогонов прошло с последнего появления в борте.
type FlagState struct {
	Held   bool
	State  string // storage: 'on_court' | 'suspended'
	Misses int
}

// Signal — что борт сказал про матч в этом цикле. Seen=false означает, что
// матча в борте НЕ БЫЛО, и это осмысленно только для успешного опроса.
type Signal struct {
	Seen  bool
	State livesource.State
}

// Action — что делать с матчем.
type Action int

const (
	ActionNone Action = iota
	// Поднять карточку.
	ActionEnterLive
	// Погасить карточку и вернуть прежний статус.
	ActionLeaveLive
	// Остаётся живым, но приостановлен: событие ровно одно, а не на каждый цикл.
	ActionSuspend
	// Возобновление после приостановки.
	ActionResume
	// Аварийный выход по максимальному возрасту.
	ActionForceExit
)

func (a Action) String() string {
	switch a {
	case ActionEnterLive:
		return "enter_live"
	case ActionLeaveLive:
		return "leave_live"
	case ActionSuspend:
		return "suspend"
	case ActionResume:
		return "resume"
	case ActionForceExit:
		return "force_exit"
	}
	return "none"
}

// missThreshold — сколько подходящих прогонов подряд без матча в борте
// означают, что он кончился.
const missThreshold = 3

// Derive — чистая функция дебаунса: состояние + сигнал -> новое состояние и
// действие. Ни часов, ни БД, ни сети.
//
// Асимметрия входа и выхода намеренная и она главная в этой фиче.
//
// ВХОД — одно наблюдение on_court. Дешёвый вход возможен потому, что парсер уже
// отказывается угадывать: незнакомое значение event_status отбрасывает строку
// целиком, поэтому «мы видим on_court» — это утверждение источника, а не наша
// догадка.
//
// ВЫХОД — либо явное окончание, либо три пропуска подряд. Дорогой выход нужен
// потому, что один сбойный цикл иначе погасил бы карточку посреди матча.
// Карточка, повисевшая лишние полчаса, не стоит ничего; ложный LIVE — это пуш,
// который нельзя отозвать.
func Derive(cur FlagState, sig Signal, age time.Duration, maxAge time.Duration) (FlagState, Action) {
	// Аварийный выход проверяется ПЕРВЫМ и работает, даже когда опрос вообще
	// не идёт: кончилась квота, лёг источник, дёрнули рубильник. Без него
	// ложная карточка висит у всех подписчиков до правки БД руками.
	if cur.Held && maxAge > 0 && age >= maxAge {
		return FlagState{}, ActionForceExit
	}

	if !cur.Held {
		// Поднять карточку может только on_court. Ни finished, ни suspended:
		// на приостановленном матче карточки ещё не было, поднимать нечего.
		if sig.Seen && sig.State == livesource.StateOnCourt {
			return FlagState{Held: true, State: "on_court"}, ActionEnterLive
		}
		return cur, ActionNone
	}

	if !sig.Seen {
		// Пропуск считается только для успешного опроса — за это отвечает
		// вызывающий: при ошибке цикла Derive для пропусков не вызывается вовсе.
		cur.Misses++
		if cur.Misses >= missThreshold {
			return FlagState{}, ActionLeaveLive
		}
		return cur, ActionNone
	}

	// Матч в борте есть — счётчик пропусков обнуляется.
	cur.Misses = 0

	switch sig.State {
	case livesource.StateFinished:
		return FlagState{}, ActionLeaveLive
	case livesource.StateSuspended:
		if cur.State == "suspended" {
			// уже приостановлен: второго события не нужно, иначе дождь
			// длиной в два часа даст пуш на каждый цикл
			return cur, ActionNone
		}
		cur.State = "suspended"
		return cur, ActionSuspend
	case livesource.StateOnCourt:
		if cur.State == "suspended" {
			cur.State = "on_court"
			return cur, ActionResume
		}
		return cur, ActionNone
	}
	return cur, ActionNone
}
