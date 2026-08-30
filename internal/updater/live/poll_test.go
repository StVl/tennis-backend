package live

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/StVl/tennis-backend/internal/livesource"
	"github.com/StVl/tennis-backend/internal/storage"
)

// fixtureMatch — синтетический scheduled-матч из db/dev_fixtures.sql вместе с
// внешними ключами его игроков. Тесты этого файла требуют, чтобы фикстуры были
// применены: в базе нет ни одной scheduled-строки без них.
type fixtureMatch struct {
	id       int64
	keys     [2]string
	schedule time.Time
}

func loadFixtureMatch(t *testing.T, pool *pgxpool.Pool) fixtureMatch {
	t.Helper()
	ctx := context.Background()

	var fm fixtureMatch
	err := pool.QueryRow(ctx, `
		select m.id, m.scheduled_at,
		       (array_agg(e.external_key order by mp.side))[1],
		       (array_agg(e.external_key order by mp.side))[2]
		from matches m
		join match_participants mp on mp.match_id = m.id
		join external_ids e on e.entity_id = mp.player_id
		     and e.source = $1 and e.entity_type = 'player'
		where m.import_key = 'devfix_uso_r128_a'
		group by m.id, m.scheduled_at`,
		livesource.SourceName).Scan(&fm.id, &fm.schedule, &fm.keys[0], &fm.keys[1])
	if err != nil {
		t.Skipf("нет фикстуры devfix_uso_r128_a с отображёнными игроками: %v; "+
			"примените db/dev_fixtures.sql и db/live_external_ids.sql", err)
	}
	return fm
}

func resetLiveState(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		for _, q := range []string{
			`delete from live_events`, `delete from live_observations`,
			`delete from live_activity_sessions`, `delete from device_push_tokens`,
			`delete from live_flags`, `delete from live_ingest_runs`,
			`delete from live_unmatched`,
			`update matches set status = 'scheduled' where import_key like 'devfix\_%'`,
		} {
			_, _ = pool.Exec(ctx, q)
		}
	}
	clean()
	t.Cleanup(clean)
}

func boardWith(fm fixtureMatch, state livesource.State, eventStatus string) livesource.Board {
	at := fm.schedule
	return livesource.Board{
		RowsParsed: 1,
		Observations: []livesource.Observation{{
			ExternalKey: "900100",
			PlayerKeys:  fm.keys,
			RoundCode:   "R128",
			ScheduledAt: &at,
			State:       state,
			EventStatus: eventStatus,
		}},
	}
}

// боард без нашего матча: строки есть (защита от пустого борта не срабатывает),
// но наш матч в них не упомянут
func boardWithout() livesource.Board {
	return livesource.Board{
		RowsParsed: 1,
		Observations: []livesource.Observation{{
			ExternalKey: "900999",
			PlayerKeys:  [2]string{"888881", "888882"},
			State:       livesource.StateOnCourt,
		}},
	}
}

func runIngest(t *testing.T, pool *pgxpool.Pool, board livesource.Board) IngestResult {
	t.Helper()
	ctx := context.Background()
	runID, now, err := storage.StartRun(ctx, pool, "live", livesource.SourceName)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	res, err := Ingest(ctx, pool, board, IngestParams{
		RunID: runID, Now: now, MatchWindow: 36 * time.Hour, MaxLiveAge: 6 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	result := storage.RunResult{Mode: "test"}
	if res.GuardTripped != "" {
		result.Error = res.GuardTripped
	}
	if err := storage.FinishRun(ctx, pool, runID, result); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	return res
}

func matchStatus(t *testing.T, pool *pgxpool.Pool, id int64) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`select status::text from matches where id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// РЕГРЕССИЯ. Пропуск засчитывается, только если матча в борте действительно не
// было. Первая версия отмечала флаг увиденным на ActionNone независимо от
// сигнала — а ActionNone приходит и из «матч на месте», и из «копим пропуск».
// В результате счётчик обнулялся каждый цикл, и карточка не гасла НИКОГДА.
// Найдено ручным прогоном, здесь закреплено.
func TestIngestAbsenceExitTakesExactlyThreeMisses(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)

	if res := runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, "")); res.Entered != 1 {
		t.Fatalf("подъём: entered=%d, ожидалось 1", res.Entered)
	}
	if got := matchStatus(t, pool, fm.id); got != "live" {
		t.Fatalf("после подъёма статус %q", got)
	}

	for i := 1; i < missThreshold; i++ {
		res := runIngest(t, pool, boardWithout())
		if res.Left != 0 {
			t.Fatalf("пропуск %d: карточка погасла раньше %d пропусков", i, missThreshold)
		}
		if got := matchStatus(t, pool, fm.id); got != "live" {
			t.Fatalf("пропуск %d: статус %q, ожидался live", i, got)
		}
	}

	res := runIngest(t, pool, boardWithout())
	if res.Left != 1 {
		t.Fatalf("третий пропуск: left=%d, ожидалось 1", res.Left)
	}
	if got := matchStatus(t, pool, fm.id); got != "scheduled" {
		t.Fatalf("после выхода статус %q, ожидался scheduled", got)
	}

	var events []string
	rows, err := pool.Query(context.Background(),
		`select event from live_events order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if len(events) != 2 || events[0] != "live" || events[1] != "finished" {
		t.Errorf("события %v, ожидалось [live finished] — по одному на переход", events)
	}
}

// Явное окончание гасит карточку сразу: три цикла нужны только когда матч
// просто пропал из борта.
func TestIngestExplicitFinishExitsImmediately(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)

	runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))
	res := runIngest(t, pool, boardWith(fm, livesource.StateFinished, "Finished"))
	if res.Left != 1 {
		t.Fatalf("left=%d, ожидалось 1", res.Left)
	}
	if got := matchStatus(t, pool, fm.id); got != "scheduled" {
		t.Fatalf("статус %q, ожидался scheduled", got)
	}
}

// Пустой борт — отказ, а не «все матчи кончились». Без этого сбой источника
// погасил бы карточки у всех пользователей разом.
func TestIngestZeroRowsGuardDoesNotSweep(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)

	runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))

	res := runIngest(t, pool, livesource.Board{RowsParsed: 0})
	if res.GuardTripped == "" {
		t.Fatal("защита не сработала: ноль строк должен быть отказом")
	}
	if res.Left != 0 {
		t.Fatalf("left=%d: по пустому борту гасить нельзя", res.Left)
	}
	if got := matchStatus(t, pool, fm.id); got != "live" {
		t.Fatalf("статус %q, матч должен остаться live", got)
	}

	// и такой прогон не должен считаться пропуском в следующих циклах
	for i := 0; i < missThreshold; i++ {
		runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))
	}
	if got := matchStatus(t, pool, fm.id); got != "live" {
		t.Fatalf("статус %q после успешных циклов", got)
	}
}

// Дождь: матч остаётся живым, событие ровно одно.
func TestIngestSuspendEmitsOnce(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)

	runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))
	runIngest(t, pool, boardWith(fm, livesource.StateSuspended, "Interrupted"))
	runIngest(t, pool, boardWith(fm, livesource.StateSuspended, "Interrupted"))
	runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))

	if got := matchStatus(t, pool, fm.id); got != "live" {
		t.Fatalf("статус %q: приостановка не гасит карточку", got)
	}

	var suspends, resumes int
	if err := pool.QueryRow(context.Background(), `
		select count(*) filter (where event = 'suspended'),
		       count(*) filter (where event = 'resumed')
		from live_events`).Scan(&suspends, &resumes); err != nil {
		t.Fatal(err)
	}
	if suspends != 1 {
		t.Errorf("событий suspended %d, ожидалось 1: двухчасовой дождь не должен "+
			"давать пуш на каждый цикл", suspends)
	}
	if resumes != 1 {
		t.Errorf("событий resumed %d, ожидалось 1", resumes)
	}
}

// Ручные флипы исключены из прохода по пропускам: синтетического матча в борте
// источника нет и быть не может, он копил бы пропуски и гас посреди теста iOS.
func TestIngestDevFlagsAreExemptFromSweep(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	ctx := context.Background()

	if _, err := storage.FlipLive(ctx, pool, fm.id, storage.LiveSourceDev, "", nil,
		time.Now().UTC()); err != nil {
		t.Fatalf("FlipLive: %v", err)
	}

	for i := 0; i < missThreshold+2; i++ {
		runIngest(t, pool, boardWithout())
	}
	if got := matchStatus(t, pool, fm.id); got != "live" {
		t.Fatalf("статус %q: ручной флип не должен откатываться проходом по пропускам", got)
	}
}

// Обвал: борт полон, но НАШИХ матчей в нём разом не стало. По симптомам это
// неотличимо от «все матчи кончились одновременно», а цена ошибки разная —
// поэтому сравниваем с предыдущим успешным циклом и не гасим.
func TestIngestCollapseGuardStopsTheSweep(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	ctx := context.Background()

	runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))

	// предыдущий цикл видел много наших матчей
	prev := 10
	runID, now, err := storage.StartRun(ctx, pool, "live", livesource.SourceName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Ingest(ctx, pool, boardWithout(), IngestParams{
		RunID: runID, Now: now, MatchWindow: 36 * time.Hour,
		MaxLiveAge: 6 * time.Hour, PrevInScope: &prev,
	})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.GuardTripped == "" {
		t.Fatal("защита от обвала не сработала: 10 -> 0 при поднятой карточке")
	}
	if res.Left != 0 {
		t.Fatalf("left=%d: при обвале гасить нельзя", res.Left)
	}
	if got := matchStatus(t, pool, fm.id); got != "live" {
		t.Fatalf("статус %q, матч должен остаться live", got)
	}
}

// Честная граница защиты: с одним матчем 1 -> 0 неотличимо от окончания, и
// защита обязана НЕ мешать нормальному выходу. Для единичного матча страховкой
// служит дебаунс в три пропуска, а не эта проверка.
func TestIngestCollapseGuardDoesNotBlockNormalExit(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	ctx := context.Background()

	runIngest(t, pool, boardWith(fm, livesource.StateOnCourt, ""))

	prev := 1 // ниже collapseMinRows
	for i := 0; i < missThreshold; i++ {
		runID, now, err := storage.StartRun(ctx, pool, "live", livesource.SourceName)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Ingest(ctx, pool, boardWithout(), IngestParams{
			RunID: runID, Now: now, MatchWindow: 36 * time.Hour,
			MaxLiveAge: 6 * time.Hour, PrevInScope: &prev,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.GuardTripped != "" {
			t.Fatalf("цикл %d: защита сработала на единичном матче и заблокировала "+
				"нормальный выход: %s", i, res.GuardTripped)
		}
		if err := storage.FinishRun(ctx, pool, runID, storage.RunResult{Mode: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := matchStatus(t, pool, fm.id); got != "scheduled" {
		t.Fatalf("статус %q: обычный выход по пропускам должен работать", got)
	}
}

// Матч live без нашего флага: все прочие страховки читают live_flags и этого
// случая не видят. Присваивать строку нельзя (prior_status пришлось бы
// выдумать), но и молчать нельзя — это ложная карточка у всех подписчиков.
func TestUnflaggedLiveMatchIsDetected(t *testing.T) {
	pool := testPool(t)
	resetLiveState(t, pool)
	fm := loadFixtureMatch(t, pool)
	ctx := context.Background()

	// кто-то написал статус мимо нас
	if _, err := pool.Exec(ctx,
		`update matches set status = 'live' where id = $1`, fm.id); err != nil {
		t.Fatal(err)
	}

	ids, err := storage.UnflaggedLiveMatches(ctx, pool, livesource.SourceName)
	if err != nil {
		t.Fatalf("UnflaggedLiveMatches: %v", err)
	}
	if len(ids) != 1 || ids[0] != fm.id {
		t.Fatalf("найдено %v, ожидался [%d]: без этого запроса ложная карточка "+
			"висит бессрочно и правится только руками в проде", ids, fm.id)
	}

	// а флага у нас действительно нет — значит остальные страховки слепы
	held, err := storage.HeldLiveFlags(ctx, pool, livesource.SourceName)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Fatalf("флагов %d, ожидалось 0", len(held))
	}
}
