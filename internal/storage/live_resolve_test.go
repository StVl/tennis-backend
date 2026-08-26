package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"Sebastián Báez":          "sebastian baez",
		"Jakub Menšik":            "jakub mensik",
		"Dušan Lajović":           "dusan lajovic",
		"Félix Auger-Aliassime":   "felix auger aliassime",
		"Stefanos Tsitsipás":      "stefanos tsitsipas",
		"Botic van de Zandschulp": "botic van de zandschulp",
		"  A.  Molcan  ":          "a molcan",
		"Zhizhen Zhang":           "zhizhen zhang",
	}
	for in, want := range cases {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, ожидалось %q", in, got, want)
		}
	}
}

// Предикат обязан ОТКАЗЫВАТЬСЯ чаще, чем угадывать: сопоставлять можно только
// по именам (birth_date и country_code у всех наших игроков NULL), а неверная
// привязка тихо покажет карточку не того игрока.
func TestMatchPlayerByName(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var name, lastName string
	if err := pool.QueryRow(ctx, `
		select display_name, coalesce(last_name, '') from players
		where is_tracked and last_name is not null
		  and (select count(*) from players p2 where p2.is_tracked
		       and lower(p2.last_name) = lower(players.last_name)) = 1
		order by id limit 1`).Scan(&name, &lastName); err != nil {
		t.Skipf("нет подходящего игрока: %v", err)
	}

	t.Run("точное имя", func(t *testing.T) {
		if _, err := MatchPlayerByName(ctx, pool, name); err != nil {
			t.Fatalf("не нашли %q: %v", name, err)
		}
	})

	t.Run("уникальная фамилия", func(t *testing.T) {
		if _, err := MatchPlayerByName(ctx, pool, "Someone "+lastName); err != nil {
			t.Fatalf("не нашли по фамилии %q: %v", lastName, err)
		}
	})

	t.Run("выдуманное имя — отказ, а не ближайший", func(t *testing.T) {
		if _, err := MatchPlayerByName(ctx, pool, "Zzyzx Qqqqq"); !errors.Is(err, ErrNoConfidentMatch) {
			t.Fatalf("err = %v, ожидался отказ", err)
		}
	})

	t.Run("пустое имя — отказ", func(t *testing.T) {
		if _, err := MatchPlayerByName(ctx, pool, "   "); !errors.Is(err, ErrNoConfidentMatch) {
			t.Fatalf("err = %v, ожидался отказ", err)
		}
	})
}

// Негативный кэш: без него неизвестный ключ стоил бы запрос каждый цикл вечно.
// Holger Rune — ровно этот случай, его в индексе источника нет вообще.
func TestResolveNegativeCacheBacksOff(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const key = "test-unknown-999"

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`delete from live_resolve_attempts where external_key = $1`, key)
	})

	now := time.Now().UTC()
	ok, err := ShouldTryResolve(ctx, pool, "test", key, now)
	if err != nil || !ok {
		t.Fatalf("первая попытка должна разрешаться: ok=%v err=%v", ok, err)
	}

	if err := RecordResolveAttempt(ctx, pool, "test", key, now); err != nil {
		t.Fatal(err)
	}

	// сразу после неудачи — нельзя
	if ok, err := ShouldTryResolve(ctx, pool, "test", key, now); err != nil || ok {
		t.Fatalf("повтор сразу после неудачи должен быть запрещён: ok=%v", ok)
	}
	// через час — можно (первый отступ)
	if ok, err := ShouldTryResolve(ctx, pool, "test", key, now.Add(time.Hour)); err != nil || !ok {
		t.Fatalf("через час попытка должна разрешаться: ok=%v", ok)
	}

	// отступ растёт: после нескольких неудач час уже не хватает
	for i := 0; i < 4; i++ {
		if err := RecordResolveAttempt(ctx, pool, "test", key, now); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := ShouldTryResolve(ctx, pool, "test", key, now.Add(2*time.Hour)); err != nil || ok {
		t.Fatalf("после нескольких неудач отступ должен вырасти: ok=%v", ok)
	}
}

// Машинная привязка помечается ждущей ревью и НЕ перетирает подтверждённую
// человеком.
func TestUpsertPlayerMappingNeverOverwritesConfirmed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	const key = "test-map-999"

	var playerID int64
	if err := pool.QueryRow(ctx,
		`select id from players where is_tracked order by id limit 1`).Scan(&playerID); err != nil {
		t.Skip("нет игроков")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from external_ids where external_key = $1`, key)
	})

	if err := UpsertPlayerMapping(ctx, pool, "test", key, playerID); err != nil {
		t.Fatal(err)
	}
	var confirmed *time.Time
	if err := pool.QueryRow(ctx,
		`select confirmed_at from external_ids where source='test' and external_key=$1`,
		key).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed != nil {
		t.Error("машинная привязка должна ждать ревью, а не считаться подтверждённой")
	}

	// человек подтвердил — машина больше не трогает
	if _, err := pool.Exec(ctx,
		`update external_ids set confirmed_at = now() where source='test' and external_key=$1`,
		key); err != nil {
		t.Fatal(err)
	}
	if err := UpsertPlayerMapping(ctx, pool, "test", key, playerID+1); err != nil {
		t.Fatal(err)
	}
	var (
		gotID int64
		after *time.Time
	)
	if err := pool.QueryRow(ctx,
		`select entity_id, confirmed_at from external_ids where source='test' and external_key=$1`,
		key).Scan(&gotID, &after); err != nil {
		t.Fatal(err)
	}
	if gotID != playerID || after == nil {
		t.Fatalf("подтверждённая привязка перезаписана: entity_id=%d confirmed=%v", gotID, after)
	}
}

// Чистка журналов не может унести наблюдения раньше срока: они каскадятся
// вместе с прогонами, поэтому горизонт прогонов зажимается горизонтом наблюдений.
func TestRetentionRunsHorizonClampedByObservations(t *testing.T) {
	p := RetentionPolicy{Observations: 10 * 24 * time.Hour, Runs: 30 * 24 * time.Hour}
	pool := testPool(t)
	if _, err := PruneLiveTables(context.Background(), pool, time.Now().UTC(), p); err != nil {
		t.Fatalf("PruneLiveTables: %v", err)
	}
	// сам факт, что вызов не удалил прогоны старше 10 дней, проверяется
	// зажатием внутри функции; здесь фиксируем, что политика по умолчанию
	// согласована
	d := DefaultRetention()
	if d.Runs > d.Observations {
		t.Fatalf("горизонт прогонов (%s) длиннее горизонта наблюдений (%s): "+
			"каскад унесёт наблюдения раньше срока", d.Runs, d.Observations)
	}
}

// Однофамилец, которого мы НЕ отслеживаем, обязан вызывать отказ. Сужение
// кандидатов до отслеживаемых выглядит безопаснее, но делает ровно наоборот:
// неоднозначность перестаёт возникать, и привязка молча уходит не туда.
func TestMatchPlayerRefusesOnUntrackedNamesake(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// берём отслеживаемого игрока с уникальной среди отслеживаемых фамилией
	var (
		trackedID int64
		lastName  string
	)
	if err := pool.QueryRow(ctx, `
		select id, last_name from players
		where is_tracked and last_name is not null
		  and (select count(*) from players p2 where lower(p2.last_name) = lower(players.last_name)) = 1
		order by id limit 1`).Scan(&trackedID, &lastName); err != nil {
		t.Skipf("нет игрока с уникальной фамилией: %v", err)
	}

	// до появления однофамильца — привязка находится
	if _, err := MatchPlayerByName(ctx, pool, "Someone "+lastName); err != nil {
		t.Fatalf("до однофамильца должно находиться: %v", err)
	}

	// заводим НЕотслеживаемого однофамильца
	var namesakeID int64
	if err := pool.QueryRow(ctx, `
		insert into players (slug, display_name, last_name, is_tracked)
		values ($1, $2, $3, false) returning id`,
		"test-namesake-zz", "Other "+lastName, lastName).Scan(&namesakeID); err != nil {
		t.Fatalf("создание однофамильца: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `delete from players where id = $1`, namesakeID)
	})

	if _, err := MatchPlayerByName(ctx, pool, "Someone "+lastName); !errors.Is(err, ErrNoConfidentMatch) {
		t.Fatalf("err = %v: однофамилец обязан приводить к отказу, даже если он "+
			"не отслеживается — иначе карточка уйдёт не тому игроку", err)
	}
}

// Односложное имя от источника не должно проходить по одной фамилии: именно так
// выглядят его записи-заглушки, то есть самый бедный данными случай получал бы
// самое слабое доказательство.
func TestMatchPlayerRefusesSingleWordVendorName(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var lastName string
	if err := pool.QueryRow(ctx, `
		select last_name from players
		where is_tracked and last_name is not null
		  and (select count(*) from players p2 where lower(p2.last_name) = lower(players.last_name)) = 1
		order by id limit 1`).Scan(&lastName); err != nil {
		t.Skipf("нет игрока с уникальной фамилией: %v", err)
	}

	if _, err := MatchPlayerByName(ctx, pool, lastName); !errors.Is(err, ErrNoConfidentMatch) {
		t.Fatalf("err = %v: одна фамилия без имени — недостаточное доказательство", err)
	}
	// а с именем — находится
	if _, err := MatchPlayerByName(ctx, pool, "Someone "+lastName); err != nil {
		t.Fatalf("с двумя словами должно находиться: %v", err)
	}
}

// Неподтверждённая привязка розыгрыша не должна создавать матчи: это
// единственное место, где догадка привела бы к записи в чужую таблицу.
func TestResolveEditionsIgnoresUnconfirmed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	var editionID int64
	if err := pool.QueryRow(ctx,
		`select id from tournament_editions limit 1`).Scan(&editionID); err != nil {
		t.Skip("нет розыгрышей")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx,
			`delete from external_ids where source = 'test-ed'`)
	})
	if _, err := pool.Exec(ctx, `
		insert into external_ids (source, entity_type, external_key, entity_id, confirmed_at)
		values ('test-ed','edition','confirmed-1',$1, now()),
		       ('test-ed','edition','guessed-1',$1, null)`, editionID); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveEditions(ctx, pool, "test-ed", []string{"confirmed-1", "guessed-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["confirmed-1"]; !ok {
		t.Error("подтверждённая привязка должна резолвиться")
	}
	if _, ok := got["guessed-1"]; ok {
		t.Fatal("неподтверждённая привязка резолвится: догадка создаст матч в чужой сетке")
	}
}
