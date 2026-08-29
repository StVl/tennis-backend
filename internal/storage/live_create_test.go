package storage

import (
	"context"
	"testing"
	"time"
)

func createFixtureEnv(t *testing.T) (editionID int64, p1, p2 int64) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()

	if err := pool.QueryRow(ctx,
		`select id from tournament_editions where slug = 'us_open_2026'`).Scan(&editionID); err != nil {
		t.Skipf("нет розыгрыша us_open_2026: %v", err)
	}
	// Игроки, у которых В ЭТОМ розыгрыше ещё нет матча: иначе тест наткнётся
	// на собственный дедуп (фикстуры из db/dev_fixtures.sql — это как раз
	// sinner vs alcaraz в us_open_2026).
	rows, err := pool.Query(ctx, `
		select p.id from players p
		join external_ids e on e.entity_id = p.id
		  and e.source = 'livetennisapi' and e.entity_type = 'player'
		where p.is_tracked
		  and not exists (
		    select 1 from match_participants mp
		    join matches m on m.id = mp.match_id
		    where mp.player_id = p.id and m.edition_id = $1)
		order by p.id limit 2`, editionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if len(ids) < 2 {
		t.Skip("нужны минимум два отображённых игрока; примените db/live_external_ids.sql")
	}
	return editionID, ids[0], ids[1]
}

func cleanupCreated(t *testing.T) {
	t.Helper()
	pool := testPool(t)
	clean := func() {
		_, _ = pool.Exec(context.Background(),
			`delete from matches where import_key like $1`, ImportKeyPrefix+"%")
	}
	clean()
	t.Cleanup(clean)
}

func TestCreateMatchFromFixture(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	at := time.Now().UTC().Add(24 * time.Hour)
	draft := MatchDraft{
		EditionID: editionID, RoundCode: "R128", ScheduledAt: at,
		PlayerIDs: [2]int64{p1, p2}, ExternalKey: "test-900001",
	}

	id, outcome, err := CreateMatchFromFixture(ctx, pool, draft)
	if err != nil {
		t.Fatalf("CreateMatchFromFixture: %v", err)
	}
	if outcome != CreateDone {
		t.Fatalf("outcome %v, ожидалось CreateDone", outcome)
	}

	// матч и ОБА участника должны существовать: матч без участников невидим
	// для поиска по игрокам и для всех экранов
	var (
		status, importKey, src string
		participants           int
	)
	if err := pool.QueryRow(ctx, `
		select m.status::text, m.import_key, m.metadata->>'source',
		       (select count(*) from match_participants mp where mp.match_id = m.id)
		from matches m where m.id = $1`, id).Scan(&status, &importKey, &src, &participants); err != nil {
		t.Fatal(err)
	}
	if status != "scheduled" {
		t.Errorf("статус %q, ожидался scheduled", status)
	}
	if importKey != ImportKeyPrefix+"test-900001" {
		t.Errorf("import_key %q", importKey)
	}
	if src != "livetennisapi" {
		t.Errorf("provenance %q: по метаданным должно быть видно, что строка наша", src)
	}
	if participants != 2 {
		t.Fatalf("участников %d, ожидалось 2", participants)
	}

	// повтор той же фикстуры — не дубль
	id2, outcome2, err := CreateMatchFromFixture(ctx, pool, draft)
	if err != nil {
		t.Fatalf("повторный вызов: %v", err)
	}
	if outcome2 != CreateExists || id2 != id {
		t.Fatalf("повтор дал outcome=%v id=%d, ожидалось CreateExists и тот же id", outcome2, id2)
	}
}

// Дедуп идёт по паре игроков и НЕ по раунду: наши словари раундов несогласованы
// между розыгрышами, поэтому ключ с раундом промахнулся бы мимо строки
// пайплайна и создал дубль.
func TestCreateMatchDedupesIgnoringRound(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	at := time.Now().UTC().Add(24 * time.Hour)
	if _, _, err := CreateMatchFromFixture(ctx, pool, MatchDraft{
		EditionID: editionID, RoundCode: "R128", ScheduledAt: at,
		PlayerIDs: [2]int64{p1, p2}, ExternalKey: "test-900010",
	}); err != nil {
		t.Fatal(err)
	}

	// та же пара, другой раунд и другой внешний ключ
	_, outcome, err := CreateMatchFromFixture(ctx, pool, MatchDraft{
		EditionID: editionID, RoundCode: "R64", ScheduledAt: at,
		PlayerIDs: [2]int64{p2, p1}, ExternalKey: "test-900011",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != CreateExists {
		t.Fatalf("outcome %v: та же пара в том же розыгрыше — это тот же матч, "+
			"даже если источник назвал раунд иначе", outcome)
	}
}

// round_code — внешний ключ в rounds, а словарь источника нам не подмножество
// (BR, голый Q, Q4, ER). Непроверенная вставка роняла бы оператор целиком.
func TestCreateMatchRejectsUnknownRound(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	for _, code := range []string{"BR", "Q", "Q4", "ER"} {
		t.Run(code, func(t *testing.T) {
			_, outcome, err := CreateMatchFromFixture(ctx, pool, MatchDraft{
				EditionID: editionID, RoundCode: code,
				ScheduledAt: time.Now().UTC().Add(time.Hour),
				PlayerIDs:   [2]int64{p1, p2}, ExternalKey: "test-round-" + code,
			})
			if err != nil {
				t.Fatalf("неизвестный раунд должен отсекаться проверкой, а не падать: %v", err)
			}
			if outcome != CreateUnknownRound {
				t.Fatalf("outcome %v, ожидалось CreateUnknownRound", outcome)
			}
		})
	}
}

// Наши созданные строки, которые так и не сыграли, надо убирать: иначе они
// навсегда становятся у обоих игроков «следующим матчем».
func TestRetireCreatedMatches(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	old := time.Now().UTC().Add(-72 * time.Hour)
	id, _, err := CreateMatchFromFixture(ctx, pool, MatchDraft{
		EditionID: editionID, RoundCode: "R128", ScheduledAt: old,
		PlayerIDs: [2]int64{p1, p2}, ExternalKey: "test-900020",
	})
	if err != nil {
		t.Fatal(err)
	}

	retired, err := RetireCreatedMatches(ctx, pool, time.Now().UTC().Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("RetireCreatedMatches: %v", err)
	}
	if retired != 1 {
		t.Fatalf("убрано %d, ожидалась 1", retired)
	}

	var status string
	if err := pool.QueryRow(ctx,
		`select status::text from matches where id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "scheduled" {
		t.Error("строка осталась scheduled и будет вечно висеть как «следующий матч»")
	}

	// чужие строки не трогаем ни при каких условиях
	var pipelineTouched int
	if err := pool.QueryRow(ctx, `
		select count(*) from matches
		where status = 'cancelled' and (import_key is null or import_key not like $1)`,
		ImportKeyPrefix+"%").Scan(&pipelineTouched); err != nil {
		t.Fatal(err)
	}
	if pipelineTouched != 0 {
		t.Fatalf("тронуто %d чужих строк — уборка обязана касаться только наших",
			pipelineTouched)
	}
}

// Созданная нами строка после окончания матча обязана получить ТЕРМИНАЛЬНЫЙ
// статус, а не вернуться в scheduled.
//
// Иначе уже сыгранный матч показывается обоим игрокам как предстоящий: на
// главной и в виджете он остаётся «следующим матчем» до тех пор, пока его не
// спрячет отсечка в 12 часов, и ещё сутки с лишним висит невидимо до уборщика.
// Уборщик закрывает хвост, но не это окно.
func TestCreatedMatchEndsTerminal(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	id, _, err := CreateMatchFromFixture(ctx, pool, MatchDraft{
		EditionID: editionID, RoundCode: "R128",
		ScheduledAt: time.Now().UTC().Add(time.Hour),
		PlayerIDs:   [2]int64{p1, p2}, ExternalKey: "test-900030",
	})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	if _, err := FlipLive(ctx, pool, id, LiveSourceAPI, "test-900030", nil, now); err != nil {
		t.Fatalf("FlipLive: %v", err)
	}

	// прежний статус запоминается на входе: для нашей строки он терминальный
	var prior string
	if err := pool.QueryRow(ctx,
		`select prior_status::text from live_flags where match_id = $1`, id).Scan(&prior); err != nil {
		t.Fatal(err)
	}
	if prior == "scheduled" {
		t.Fatal("prior_status = scheduled: сыгранный матч вернётся в «предстоящие» " +
			"и станет у обоих игроков «следующим матчем»")
	}
	// но и не completed: это означало бы, что мы знаем результат
	if prior == "completed" {
		t.Fatal("prior_status = completed: результат придумывать нельзя, он " +
			"принадлежит пайплайну")
	}

	if _, _, err := FlipOut(ctx, pool, id, LiveEventFinished, "test", now); err != nil {
		t.Fatalf("FlipOut: %v", err)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`select status::text from matches where id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status == "scheduled" {
		t.Fatalf("статус после окончания %q — сыгранный матч показывается как предстоящий", status)
	}

	// чужие строки ведут себя по-прежнему: возвращаются в scheduled
	var pipelineID int64
	if err := pool.QueryRow(ctx,
		`select id from matches where import_key not like $1 and status = 'scheduled' limit 1`,
		ImportKeyPrefix+"%").Scan(&pipelineID); err == nil {
		if _, err := FlipLive(ctx, pool, pipelineID, LiveSourceAPI, "", nil, now); err != nil {
			t.Fatal(err)
		}
		var p string
		if err := pool.QueryRow(ctx,
			`select prior_status::text from live_flags where match_id = $1`, pipelineID).Scan(&p); err != nil {
			t.Fatal(err)
		}
		if p != "scheduled" {
			t.Errorf("prior_status чужой строки = %q, ожидался scheduled: правило "+
				"касается только наших строк", p)
		}
		_, _, _ = FlipOut(ctx, pool, pipelineID, LiveEventFinished, "test", now)
	}
}

// Источник переносит фикстуры на часы и на сутки. Писатель insert-only, поэтому
// без отдельной правки времени у созданного матча навсегда осталось бы время из
// первого снимка — и на главной он показывался бы не тогда, когда играется.
func TestCreateMatchUpdatesScheduledAtOnReschedule(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	first := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	draft := MatchDraft{
		EditionID: editionID, RoundCode: "R128", ScheduledAt: first,
		PlayerIDs: [2]int64{p1, p2}, ExternalKey: "test-900050",
	}
	id, outcome, err := CreateMatchFromFixture(ctx, pool, draft)
	if err != nil || outcome != CreateDone {
		t.Fatalf("создание: outcome=%v err=%v", outcome, err)
	}

	// тот же матч, время уехало на сутки
	moved := first.Add(24 * time.Hour)
	draft.ScheduledAt = moved
	sameID, outcome, err := CreateMatchFromFixture(ctx, pool, draft)
	if err != nil {
		t.Fatalf("перенос: %v", err)
	}
	if sameID != id {
		t.Fatalf("создалась вторая строка (%d вместо %d): дедуп по паре игроков не сработал", sameID, id)
	}
	if outcome != CreateRescheduled {
		t.Fatalf("outcome %v, ожидалось CreateRescheduled", outcome)
	}
	var got time.Time
	if err := pool.QueryRow(ctx, `select scheduled_at from matches where id = $1`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.UTC().Equal(moved) {
		t.Errorf("scheduled_at = %s, ожидалось %s: время осталось из первого снимка", got.UTC(), moved)
	}

	// повтор без изменений — не перенос
	if _, outcome, err = CreateMatchFromFixture(ctx, pool, draft); err != nil {
		t.Fatal(err)
	}
	if outcome != CreateExists {
		t.Errorf("outcome %v, ожидалось CreateExists: время не менялось", outcome)
	}
}

// Строку пайплайна не трогаем даже по времени: он сам её и поправит.
func TestCreateMatchLeavesPipelineRowAlone(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	editionID, p1, p2 := createFixtureEnv(t)
	cleanupCreated(t)

	pipelineAt := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	var pipelineID int64
	if err := pool.QueryRow(ctx, `
		insert into matches (edition_id, round_code, scheduled_at, status, discipline, import_key)
		values ($1, 'R128', $2, 'scheduled', 'singles', 'pipeline_test_900051')
		returning id`, editionID, pipelineAt).Scan(&pipelineID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from matches where id = $1`, pipelineID)
	})
	for side, pid := range []int64{p1, p2} {
		if _, err := pool.Exec(ctx, `
			insert into match_participants (match_id, side, slot, player_id) values ($1, $2, 1, $3)`,
			pipelineID, side+1, pid); err != nil {
			t.Fatal(err)
		}
	}

	draft := MatchDraft{
		EditionID: editionID, RoundCode: "R128",
		ScheduledAt: pipelineAt.Add(-6 * time.Hour),
		PlayerIDs:   [2]int64{p1, p2}, ExternalKey: "test-900051",
	}
	id, outcome, err := CreateMatchFromFixture(ctx, pool, draft)
	if err != nil {
		t.Fatal(err)
	}
	if id != pipelineID || outcome != CreateExists {
		t.Fatalf("id=%d outcome=%v: ожидалось, что найдём строку пайплайна и ничего не сделаем", id, outcome)
	}
	var got time.Time
	if err := pool.QueryRow(ctx, `select scheduled_at from matches where id = $1`, pipelineID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.UTC().Equal(pipelineAt) {
		t.Errorf("время строки пайплайна изменилось на %s: этот сервис её не владелец", got.UTC())
	}
}
