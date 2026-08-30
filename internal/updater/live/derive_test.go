package live

import (
	"testing"
	"time"

	"github.com/StVl/tennis-backend/internal/livesource"
)

const noMaxAge = 0

func seen(s livesource.State) Signal { return Signal{Seen: true, State: s} }

var absent = Signal{Seen: false}

func TestDerive(t *testing.T) {
	held := FlagState{Held: true, State: "on_court"}
	suspended := FlagState{Held: true, State: "suspended"}

	cases := []struct {
		name       string
		cur        FlagState
		sig        Signal
		wantAction Action
		wantHeld   bool
		wantState  string
	}{
		{
			name: "не держим + на корте -> поднимаем карточку",
			cur:  FlagState{}, sig: seen(livesource.StateOnCourt),
			wantAction: ActionEnterLive, wantHeld: true, wantState: "on_court",
		},
		{
			name: "не держим + матч кончился -> гасить нечего",
			cur:  FlagState{}, sig: seen(livesource.StateFinished),
			wantAction: ActionNone,
		},
		{
			// приостановленный матч НЕ поднимает карточку: её ещё не было
			name: "не держим + приостановлен -> ничего",
			cur:  FlagState{}, sig: seen(livesource.StateSuspended),
			wantAction: ActionNone,
		},
		{
			name: "не держим + матча нет в борте -> ничего",
			cur:  FlagState{}, sig: absent,
			wantAction: ActionNone,
		},
		{
			name: "держим + всё ещё на корте -> ничего",
			cur:  held, sig: seen(livesource.StateOnCourt),
			wantAction: ActionNone, wantHeld: true, wantState: "on_court",
		},
		{
			name: "держим + явное окончание -> гасим сразу",
			cur:  held, sig: seen(livesource.StateFinished),
			wantAction: ActionLeaveLive,
		},
		{
			name: "держим + дождь -> остаёмся живыми, одно событие",
			cur:  held, sig: seen(livesource.StateSuspended),
			wantAction: ActionSuspend, wantHeld: true, wantState: "suspended",
		},
		{
			// иначе двухчасовой дождь дал бы пуш на каждый цикл
			name: "уже приостановлен + снова приостановлен -> второго события нет",
			cur:  suspended, sig: seen(livesource.StateSuspended),
			wantAction: ActionNone, wantHeld: true, wantState: "suspended",
		},
		{
			name: "приостановлен + снова на корте -> возобновление",
			cur:  suspended, sig: seen(livesource.StateOnCourt),
			wantAction: ActionResume, wantHeld: true, wantState: "on_court",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next, action := Derive(tc.cur, tc.sig, 0, noMaxAge)
			if action != tc.wantAction {
				t.Fatalf("действие %s, ожидалось %s", action, tc.wantAction)
			}
			if next.Held != tc.wantHeld {
				t.Errorf("Held = %v, ожидалось %v", next.Held, tc.wantHeld)
			}
			if tc.wantHeld && next.State != tc.wantState {
				t.Errorf("State = %q, ожидалось %q", next.State, tc.wantState)
			}
		})
	}
}

// Выход дорогой: один сбойный цикл не должен гасить карточку посреди матча.
func TestDeriveNeedsThreeConsecutiveMisses(t *testing.T) {
	cur := FlagState{Held: true, State: "on_court"}

	for i := 1; i < missThreshold; i++ {
		next, action := Derive(cur, absent, 0, noMaxAge)
		if action != ActionNone {
			t.Fatalf("пропуск %d: действие %s, карточка не должна гаснуть раньше %d пропусков",
				i, action, missThreshold)
		}
		if next.Misses != i {
			t.Fatalf("пропуск %d: счётчик %d", i, next.Misses)
		}
		cur = next
	}

	_, action := Derive(cur, absent, 0, noMaxAge)
	if action != ActionLeaveLive {
		t.Fatalf("после %d пропусков действие %s, ожидалось leave_live", missThreshold, action)
	}
}

// Появление матча обнуляет счётчик: on / нет / on остаётся живым.
func TestDeriveMissCounterResetsOnReappearance(t *testing.T) {
	cur := FlagState{Held: true, State: "on_court"}

	cur, _ = Derive(cur, absent, 0, noMaxAge)
	cur, _ = Derive(cur, absent, 0, noMaxAge)
	if cur.Misses != 2 {
		t.Fatalf("счётчик %d, ожидалось 2", cur.Misses)
	}

	cur, action := Derive(cur, seen(livesource.StateOnCourt), 0, noMaxAge)
	if action != ActionNone || !cur.Held {
		t.Fatalf("матч снова в борте: действие %s, held=%v", action, cur.Held)
	}
	if cur.Misses != 0 {
		t.Fatalf("счётчик %d, после появления он обязан обнулиться, иначе "+
			"мигающий борт погасит карточку идущего матча", cur.Misses)
	}

	// и снова три пропуска нужны с нуля
	for i := 0; i < missThreshold-1; i++ {
		cur, action = Derive(cur, absent, 0, noMaxAge)
		if action != ActionNone {
			t.Fatalf("после обнуления карточка погасла на %d-м пропуске", i+1)
		}
	}
}

// Аварийный выход работает, даже когда опрос вообще не идёт: кончилась квота,
// лёг источник, дёрнули рубильник. Без него ложная карточка висит у всех
// подписчиков до правки БД руками.
func TestDeriveForceExitOnMaxAge(t *testing.T) {
	cur := FlagState{Held: true, State: "on_court"}
	maxAge := 6 * time.Hour

	// матч идёт и виден — но слишком долго
	next, action := Derive(cur, seen(livesource.StateOnCourt), maxAge+time.Minute, maxAge)
	if action != ActionForceExit {
		t.Fatalf("действие %s, ожидался аварийный выход", action)
	}
	if next.Held {
		t.Error("после аварийного выхода матч не должен считаться живым")
	}

	// в пределах возраста ничего не происходит
	if _, action := Derive(cur, seen(livesource.StateOnCourt), maxAge-time.Minute, maxAge); action != ActionNone {
		t.Fatalf("действие %s до истечения возраста", action)
	}

	// нулевой maxAge выключает проверку
	if _, action := Derive(cur, seen(livesource.StateOnCourt), 100*time.Hour, 0); action != ActionNone {
		t.Fatalf("действие %s при выключенной проверке возраста", action)
	}
}

// Полный жизненный цикл: расписание -> корт -> дождь -> корт -> конец.
func TestDeriveFullLifecycle(t *testing.T) {
	var cur FlagState
	steps := []struct {
		sig        Signal
		wantAction Action
	}{
		{seen(livesource.StateOnCourt), ActionEnterLive},
		{seen(livesource.StateOnCourt), ActionNone},
		{seen(livesource.StateSuspended), ActionSuspend},
		{seen(livesource.StateSuspended), ActionNone},
		{seen(livesource.StateOnCourt), ActionResume},
		{seen(livesource.StateFinished), ActionLeaveLive},
	}
	for i, step := range steps {
		next, action := Derive(cur, step.sig, 0, noMaxAge)
		if action != step.wantAction {
			t.Fatalf("шаг %d: действие %s, ожидалось %s", i, action, step.wantAction)
		}
		cur = next
	}
	if cur.Held {
		t.Error("после окончания матч не должен считаться живым")
	}
}
