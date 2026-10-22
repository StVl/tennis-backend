package storage

import (
	"context"
	"testing"
)

func TestParseTour(t *testing.T) {
	cases := []struct {
		in   string
		want Tour
		ok   bool
	}{
		{"", TourATP, true},
		{"atp", TourATP, true},
		{"wta", TourWTA, true},
		{"WTA", "", false},
		{"itf", "", false},
	}
	for _, c := range cases {
		got, ok := ParseTour(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("ParseTour(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if TourWTA.Gender() != "f" || TourATP.Gender() != "m" {
		t.Error("Gender: atp → m, wta → f")
	}
}

// tourFixture — два игрока-мужчины и две женщины с рейтингом в разных турах; удаляется после теста.
//
//	zzt_atp_man:   ATP #7
//	zzt_wta_woman: WTA #3 (и ATP-снапшот, которого быть не должно, — проверяем, что он не берётся)
//	zzt_wta_new:   только ATP-снапшот — WTA-рейтинга нет, ждём null
func tourFixture(t *testing.T) {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	cleanup := func() {
		_, _ = pool.Exec(ctx, `delete from players where slug like 'zzt\_%'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	_, err := pool.Exec(ctx, `
		insert into players (slug, first_name, last_name, display_name, gender, is_tracked) values
		  ('zzt_atp_man', 'Atp', 'Man', 'Zzt Atp Man', 'm', true),
		  ('zzt_wta_woman', 'Wta', 'Woman', 'Zzt Wta Woman', 'f', true),
		  ('zzt_wta_new', 'Wta', 'New', 'Zzt Wta New', 'f', true);
		insert into ranking_snapshots (player_id, tour_code, snapshot_date, rank, points)
		select p.id, v.tour, current_date, v.rank, 100
		from (values ('zzt_atp_man', 'atp', 7), ('zzt_wta_woman', 'wta', 3),
		             ('zzt_wta_woman', 'atp', 1), ('zzt_wta_new', 'atp', 2)) v(slug, tour, rank)
		join players p on p.slug = v.slug;`)
	if err != nil {
		t.Fatalf("фикстура: %v", err)
	}
}

func TestListPlayersRankUsesPlayerTour(t *testing.T) {
	tourFixture(t)
	pool := testPool(t)
	items, err := ListPlayers(context.Background(), pool, "en", true, "Zzt ", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]PlayerListItem{}
	for _, it := range items {
		got[it.Slug] = it
	}
	check := func(slug string, rank *int, tour Tour) {
		t.Helper()
		it, ok := got[slug]
		if !ok {
			t.Fatalf("%s нет в списке", slug)
		}
		if (it.Rank == nil) != (rank == nil) || (rank != nil && *it.Rank != *rank) {
			t.Errorf("%s: rank = %v, want %v", slug, intOrNil(it.Rank), intOrNil(rank))
		}
		if it.Tour != tour {
			t.Errorf("%s: tour = %q, want %q", slug, it.Tour, tour)
		}
	}
	seven, three := 7, 3
	check("zzt_atp_man", &seven, TourATP)
	check("zzt_wta_woman", &three, TourWTA)
	check("zzt_wta_new", nil, TourWTA)

	wta := TourWTA
	women, err := ListPlayers(context.Background(), pool, "en", true, "Zzt ", &wta)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range women {
		if it.Tour != TourWTA {
			t.Errorf("tour=wta вернул %s (%s)", it.Slug, it.Tour)
		}
	}
	if len(women) != 2 {
		t.Errorf("tour=wta: %d игроков, want 2", len(women))
	}
}

func TestGetPlayerRankUsesPlayerTour(t *testing.T) {
	tourFixture(t)
	p, err := GetPlayer(context.Background(), testPool(t), "en", "zzt_wta_woman")
	if err != nil {
		t.Fatal(err)
	}
	if p.Rank == nil || *p.Rank != 3 || p.Tour != TourWTA {
		t.Errorf("rank = %v, tour = %q; want 3, wta", intOrNil(p.Rank), p.Tour)
	}
}

func intOrNil(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
