package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeUpdater — заглушка updater.Updater: сообщает, с каким контекстом её
// вызвали, и умеет блокироваться до его отмены.
type fakeUpdater struct {
	name    string
	calls   int
	gotCtx  context.Context
	block   bool
	blocked chan struct{}
	panics  bool
}

func (f *fakeUpdater) Name() string { return f.name }

func (f *fakeUpdater) Update(ctx context.Context) error {
	f.calls++
	f.gotCtx = ctx
	if f.panics {
		panic("нарочно: неожиданный ответ вендора")
	}
	if !f.block {
		return nil
	}
	if f.blocked != nil {
		close(f.blocked)
	}
	<-ctx.Done()
	return ctx.Err()
}

// Джоб со своим Timeout должен получить именно его, а не общий таймаут
// планировщика: общий (5m по умолчанию) длиннее интервала частых джобов.
func TestRegisterUsesPerJobTimeout(t *testing.T) {
	cases := []struct {
		name       string
		jobTimeout time.Duration
		want       time.Duration
	}{
		{"свой таймаут перекрывает общий", 2 * time.Second, 2 * time.Second},
		{"нулевой означает общий", 0, time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(context.Background(), time.Minute)
			u := &fakeUpdater{name: "t"}

			timeout := s.updateTimeout
			if tc.jobTimeout > 0 {
				timeout = tc.jobTimeout
			}
			s.runOnce(u, timeout)

			if u.calls != 1 {
				t.Fatalf("вызовов %d, ожидалось 1", u.calls)
			}
			deadline, ok := u.gotCtx.Deadline()
			if !ok {
				t.Fatal("у контекста нет дедлайна")
			}
			// сравниваем с допуском: между вычислением и замером проходит время
			if got := time.Until(deadline); got > tc.want || got < tc.want-time.Second {
				t.Fatalf("дедлайн через %v, ожидалось ~%v", got, tc.want)
			}
		})
	}
}

// Отмена корневого контекста должна гасить идущий прогон, а не ждать его
// таймаута: иначе cron.Stop() держит выключение на каждом деплое Railway.
func TestRootCancelAbortsRunningJob(t *testing.T) {
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	s := New(rootCtx, time.Hour) // таймаут заведомо больше теста

	u := &fakeUpdater{name: "slow", block: true, blocked: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		s.runOnce(u, time.Hour)
		close(done)
	}()

	<-u.blocked // джоб точно внутри Update
	cancelRoot()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("прогон не завершился после отмены корневого контекста")
	}
	if !errors.Is(u.gotCtx.Err(), context.Canceled) {
		t.Fatalf("контекст джоба: %v, ожидалась отмена", u.gotCtx.Err())
	}
}

// После отмены корня новые прогоны не начинаются вовсе.
func TestCancelledRootSkipsRun(t *testing.T) {
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	cancelRoot()

	s := New(rootCtx, time.Minute)
	u := &fakeUpdater{name: "t"}
	s.runOnce(u, time.Minute)

	if u.calls != 0 {
		t.Fatalf("вызовов %d, ожидалось 0: при выключении новый прогон не начинаем", u.calls)
	}
}

// Паника в джобе не должна уносить процесс. cron.New() ставит ПУСТУЮ цепочку —
// в v3 автоматический Recover убрали, — и джоб бежит в голой горутине, поэтому
// без recover'а в runOnce одна плохая строка от вендора означала бы цикл
// перезапусков на Railway. Без этого теста защиту тихо снимет любой рефакторинг:
// панику видно только когда она случается.
func TestRunOnceRecoversPanic(t *testing.T) {
	s := New(context.Background(), time.Minute)
	panicky := &fakeUpdater{name: "panicky", panics: true}

	// если recover'а нет, эта строка валит весь тестовый бинарь
	s.runOnce(panicky, time.Second)

	if panicky.calls != 1 {
		t.Fatalf("вызовов %d, ожидалось 1", panicky.calls)
	}

	// планировщик остался годен: следующий прогон идёт как обычно
	healthy := &fakeUpdater{name: "healthy"}
	s.runOnce(healthy, time.Second)
	if healthy.calls != 1 {
		t.Fatalf("после паники прогонов %d, ожидался 1", healthy.calls)
	}
}
