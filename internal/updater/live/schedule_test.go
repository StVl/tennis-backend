package live

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StVl/tennis-backend/internal/config"
	"github.com/StVl/tennis-backend/internal/livesource"
	"github.com/StVl/tennis-backend/internal/storage"
)

// fakeSource — Source с заранее заданным ответом. Именно для этого
// SourceFactory и существует.
type fakeSource struct {
	page      livesource.FixturePage
	err       error
	gotKeys   []string
	onRequest func()
}

func (f *fakeSource) Name() string { return livesource.SourceName }

func (f *fakeSource) PollLive(ctx context.Context) (livesource.Board, error) {
	return livesource.Board{}, nil
}

func (f *fakeSource) Fixtures(ctx context.Context, playerKeys []string) (livesource.FixturePage, error) {
	f.gotKeys = playerKeys
	if f.onRequest != nil {
		f.onRequest()
	}
	return f.page, f.err
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL не задан — пропускаем тесты, которым нужна БД")
	}
	pool, err := storage.NewPool(context.Background(), dsn, 5)
	if err != nil {
		t.Fatalf("подключение: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testCfg() config.LiveConfig {
	return config.LiveConfig{
		Enabled:       true,
		UpdateTimeout: 90 * time.Second,
		WatchTail:     6 * time.Hour,
	}
}

// trackedKeys берёт настоящие ключи из external_ids: тест должен работать на той
// же выборке, что и боевой джоб.
func trackedKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	keys, err := storage.TrackedExternalKeys(context.Background(), pool, livesource.SourceName)
	if err != nil {
		t.Fatalf("TrackedExternalKeys: %v", err)
	}
	if len(keys) < 2 {
		t.Skipf("в external_ids %d ключей — мало для теста; примените db/live_external_ids.sql", len(keys))
	}
	return keys
}

// Чистим и ДО, и после: иначе строки, оставшиеся от ручного прогона джоба,
// попадают в тест и ломают счётчики. Отловлено ровно так.
func cleanupSchedule(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		_, _ = pool.Exec(ctx, `delete from live_schedule where source = $1`, livesource.SourceName)
		_, _ = pool.Exec(ctx, `delete from live_ingest_runs where job = 'live-schedule'`)
		_, _ = pool.Exec(ctx, `delete from live_unmatched where source = $1`, livesource.SourceName)
	}
	clean()
	t.Cleanup(clean)
}

func at(d time.Duration) *time.Time {
	v := time.Now().UTC().Add(d)
	return &v
}

// ГЛАВНЫЙ тест этого файла: частичная ошибка обязана СОХРАНИТЬ то, что успели
// набрать, и одновременно пометить прогон ошибкой. Данные и статус расходятся
// намеренно, и следующий читатель наверняка захочет это «исправить».
func TestSchedulePartialFailureKeepsRowsAndMarksError(t *testing.T) {
	pool := testPool(t)
	cleanupSchedule(t, pool)
	keys := trackedKeys(t, pool)

	fetchErr := errors.New("вторая страница не доехала")
	src := &fakeSource{
		err: fetchErr,
		page: livesource.FixturePage{
			RowsParsed: 2,
			Fixtures: []livesource.Fixture{{
				ExternalKey: "partial-1",
				PlayerKeys:  [2]string{keys[0], keys[1]},
				ScheduledAt: at(2 * time.Hour),
				RoundCode:   "R16",
			}},
		},
	}

	u := NewSchedule(pool, func(func()) livesource.Source { return src }, testCfg())
	err := u.Update(context.Background())
	if err == nil {
		t.Fatal("частичная ошибка должна возвращаться наверх")
	}

	// набранное — сохранено
	var rows int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from live_schedule where source = $1`,
		livesource.SourceName).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("строк расписания %d, ожидалась 1: набранное нельзя выбрасывать, "+
			"иначе пустое расписание уложит Job B спать на восемь часов", rows)
	}

	// прогон — помечен ошибкой
	var runErr *string
	if err := pool.QueryRow(context.Background(),
		`select error from live_ingest_runs where job = 'live-schedule'
		 order by id desc limit 1`).Scan(&runErr); err != nil {
		t.Fatal(err)
	}
	if runErr == nil {
		t.Error("прогон должен быть помечен ошибкой, даже когда данные записаны")
	}
}

// Фикстуры, в которых нет ни одного нашего игрока, до расписания доходить не
// должны. Проверка избыточна, пока вендор honours player=, и ровно поэтому
// нужна: иначе отказ фильтра означает вечный WATCHING и выеденную квоту без
// единой ошибки.
func TestScheduleDropsFixturesWithoutTrackedPlayer(t *testing.T) {
	pool := testPool(t)
	cleanupSchedule(t, pool)
	keys := trackedKeys(t, pool)

	src := &fakeSource{page: livesource.FixturePage{
		RowsParsed: 3,
		Fixtures: []livesource.Fixture{
			{ExternalKey: "ours", PlayerKeys: [2]string{keys[0], "999999"}, ScheduledAt: at(time.Hour)},
			{ExternalKey: "theirs-1", PlayerKeys: [2]string{"888881", "888882"}, ScheduledAt: at(time.Hour)},
			{ExternalKey: "theirs-2", PlayerKeys: [2]string{"888883", "888884"}, ScheduledAt: at(time.Hour)},
		},
	}}

	u := NewSchedule(pool, func(func()) livesource.Source { return src }, testCfg())
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var got []string
	rows, err := pool.Query(context.Background(),
		`select external_key from live_schedule where source = $1 order by external_key`,
		livesource.SourceName)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		got = append(got, k)
	}
	if len(got) != 1 || got[0] != "ours" {
		t.Fatalf("в расписании %v, ожидалось только [ours]", got)
	}

	var dropped *int
	if err := pool.QueryRow(context.Background(),
		`select rows_dropped_unresolved from live_ingest_runs where job = 'live-schedule'
		 order by id desc limit 1`).Scan(&dropped); err != nil {
		t.Fatal(err)
	}
	if dropped == nil || *dropped != 2 {
		t.Errorf("rows_dropped_unresolved = %v, ожидалось 2: отброс должен быть виден в прогоне", dropped)
	}
}

// Выключенный рубильник не должен делать ни запросов, ни записей.
func TestScheduleDisabledDoesNothing(t *testing.T) {
	pool := testPool(t)
	cleanupSchedule(t, pool)

	src := &fakeSource{}
	cfg := testCfg()
	cfg.Enabled = false

	u := NewSchedule(pool, func(func()) livesource.Source { return src }, cfg)
	if err := u.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if src.gotKeys != nil {
		t.Error("при выключенном рубильнике источник не должен опрашиваться")
	}

	var runs int
	if err := pool.QueryRow(context.Background(),
		`select count(*) from live_ingest_runs where job = 'live-schedule'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("прогонов %d, ожидалось 0", runs)
	}
}
