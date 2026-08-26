package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Тесты этого файла требуют живой Postgres и потому включаются переменной
// TEST_DATABASE_URL. Без неё `go test ./...` остаётся зелёным и быстрым:
// как только набор начинает требовать Docker, его перестают запускать.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL не задан — пропускаем тесты, которым нужна БД")
	}
	pool, err := NewPool(context.Background(), dsn, 5)
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Блокировка живёт в СЕССИИ, а берётся через пул. Если её взять и отпустить
// разными соединениями, unlock молча не сработает и джоб перестанет работать
// до перезапуска процесса — снаружи это выглядит как «иногда не запускается».
//
// Проверять ОБЯЗАТЕЛЬНО из второго пула. Advisory-блокировки реентерабельны в
// пределах сессии, а пул охотно отдаёт то же соединение обратно — поэтому
// повторный захват из того же пула удаётся даже с протёкшей блокировкой, и
// тест ничего не проверяет. Проверено мутацией: если Release просто вернёт
// соединение в пул, вариант «один пул» остаётся зелёным, «два пула» краснеет.
//
// Попутно мутация показала, что запасной путь в Release работает: если убрать
// сам pg_advisory_unlock, тест ВСЁ РАВНО зелёный — потому что released=false
// уводит нас в Hijack+Close, а закрытие сессии снимает блокировку. То есть
// уничтожение соединения — не украшение, а рабочая страховка.
func TestLiveLockRoundTrip(t *testing.T) {
	holder := testPool(t)
	observer := testPool(t) // отдельные сессии
	ctx := context.Background()

	// произвольный ключ, чтобы не пересечься с боевыми
	const key = 9901

	lock, acquired, err := AcquireLiveLock(ctx, holder, key)
	if err != nil {
		t.Fatalf("первый захват: %v", err)
	}
	if !acquired {
		t.Fatal("первый захват должен удаться")
	}

	// пока держим — другая сессия того же ключа взять не может
	second, acquired2, err := AcquireLiveLock(ctx, observer, key)
	if err != nil {
		t.Fatalf("второй захват: %v", err)
	}
	if acquired2 {
		second.Release(ctx)
		t.Fatal("второй захват удался — блокировка не работает, два инстанса опросят источник одновременно")
	}

	// другой ключ не должен конфликтовать: у джобов свои блокировки
	other, acquiredOther, err := AcquireLiveLock(ctx, observer, key+1)
	if err != nil {
		t.Fatalf("захват другого ключа: %v", err)
	}
	if !acquiredOther {
		t.Fatal("другой ключ не должен конфликтовать")
	}
	other.Release(ctx)

	lock.Release(ctx)

	// ключ снова свободен ДЛЯ ДРУГОЙ СЕССИИ — только это доказывает, что
	// unlock реально выполнился, а не что мы попали в то же соединение
	again, acquired3, err := AcquireLiveLock(ctx, observer, key)
	if err != nil {
		t.Fatalf("захват после освобождения: %v", err)
	}
	if !acquired3 {
		t.Fatal("ключ не освободился: unlock ушёл в другое соединение, блокировка протекла")
	}
	again.Release(ctx)
}

// Отменённый контекст цикла не должен мешать снять блокировку: Release ходит
// по отвязанному контексту именно ради этого случая.
func TestLiveLockReleaseSurvivesCancelledContext(t *testing.T) {
	holder := testPool(t)
	observer := testPool(t)
	const key = 9903

	ctx, cancel := context.WithCancel(context.Background())
	lock, acquired, err := AcquireLiveLock(ctx, holder, key)
	if err != nil || !acquired {
		t.Fatalf("захват: acquired=%v err=%v", acquired, err)
	}

	cancel() // как будто сработал таймаут цикла
	lock.Release(ctx)

	again, acquired2, err := AcquireLiveLock(context.Background(), observer, key)
	if err != nil {
		t.Fatalf("повторный захват: %v", err)
	}
	if !acquired2 {
		t.Fatal("после отменённого контекста блокировка осталась висеть — " +
			"джоб замолчал бы до перезапуска процесса")
	}
	again.Release(context.Background())
}

// requests_made инкрементируется по ходу, а не на закрытии: цикл, убитый
// редеплоем, иначе унёс бы потраченные запросы, а квота — связывающее
// ограничение всей фичи.
func TestRunRequestCounting(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	runID, startedAt, err := StartRun(ctx, pool, "test-job", "test-source")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from live_ingest_runs where id = $1`, runID)
	})
	if startedAt.IsZero() {
		t.Error("started_at пустой, а это единственное обращение к часам за цикл")
	}

	for i := 0; i < 3; i++ {
		IncRunRequests(ctx, pool, runID)
	}

	var made int
	if err := pool.QueryRow(ctx,
		`select requests_made from live_ingest_runs where id = $1`, runID).Scan(&made); err != nil {
		t.Fatal(err)
	}
	if made != 3 {
		t.Fatalf("requests_made = %d, ожидалось 3", made)
	}

	// незакрытый прогон должен подбираться дворником
	swept, err := SweepAbandonedRuns(ctx, pool, "test-job", startedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("SweepAbandonedRuns: %v", err)
	}
	if swept != 1 {
		t.Fatalf("подобрано %d прогонов, ожидался 1: иначе finished_at пуст навсегда "+
			"и «последний успешный прогон» отравлен", swept)
	}
}

// Расписание обновляется, а не пересоздаётся: пустое расписание уложило бы
// Job B спать на восемь часов без единой ошибки.
func TestUpsertScheduleIsIncremental(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const source = "test-source"

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from live_schedule where source = $1`, source)
	})

	now := time.Now().UTC()
	soon := now.Add(2 * time.Hour)
	old := now.Add(-100 * time.Hour)

	rows := []ScheduleRow{
		{ExternalKey: "a", ScheduledAt: &soon, PlayerKeys: []string{"1", "2"}, RoundCode: "R16"},
		{ExternalKey: "b", ScheduledAt: &old, PlayerKeys: []string{"3", "4"}},
	}
	if _, _, err := UpsertSchedule(ctx, pool, source, rows, now, now.Add(-50*time.Hour)); err != nil {
		t.Fatalf("UpsertSchedule: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`select count(*) from live_schedule where source = $1`, source).Scan(&count); err != nil {
		t.Fatal(err)
	}
	// "b" вставилась и тут же попала под чистку прошлого
	if count != 1 {
		t.Fatalf("строк %d, ожидалась 1 (давняя фикстура должна быть подчищена)", count)
	}

	// повторный upsert той же строки с новым временем — обновление, не дубль
	later := now.Add(5 * time.Hour)
	rows[0].ScheduledAt = &later
	rows[0].RoundCode = "QF"
	if _, _, err := UpsertSchedule(ctx, pool, source, rows[:1], now, now.Add(-50*time.Hour)); err != nil {
		t.Fatalf("повторный UpsertSchedule: %v", err)
	}

	var (
		gotRound string
		gotAt    time.Time
	)
	if err := pool.QueryRow(ctx,
		`select round_code, scheduled_at from live_schedule where source = $1 and external_key = 'a'`,
		source).Scan(&gotRound, &gotAt); err != nil {
		t.Fatal(err)
	}
	if gotRound != "QF" || !gotAt.Equal(later) {
		t.Errorf("строка не обновилась: round=%q at=%v", gotRound, gotAt)
	}
}
