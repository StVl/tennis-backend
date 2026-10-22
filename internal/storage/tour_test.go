package storage

import (
	"context"
	"strings"
	"testing"
	"time"
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

// homeTourFixture — совмещённый турнир zzt_slam: мужской и женский розыгрыш идут сегодня, у
// каждого по матчу подписанного игрока.
func homeTourFixture(t *testing.T, now time.Time) {
	t.Helper()
	tourFixture(t)
	pool := testPool(t)
	ctx := context.Background()
	cleanup := func() {
		_, _ = pool.Exec(ctx, `delete from tournaments where slug like 'zzt\_%'`)
	}
	cleanup()
	t.Cleanup(cleanup)
	day, at := now.Format("2006-01-02"), now.Add(2*time.Hour)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`insert into players (slug, display_name, gender) values
		    ('zzt_atp_opp', 'Zzt Atp Opp', 'm'), ('zzt_wta_opp', 'Zzt Wta Opp', 'f')`, nil},
		{`insert into tournaments (slug, name) values ('zzt_slam', 'Zzt Slam')`, nil},
		{`insert into tournament_editions (tournament_id, year, slug, start_date, end_date, surface, gender)
		  select t.id, 2026, v.slug, $1::date - 2, $1::date + 3, 'hard', v.g::gender_t
		  from tournaments t, (values ('zzt_slam_2026', 'm'), ('zzt_slam_wta_2026', 'f')) v(slug, g)
		  where t.slug = 'zzt_slam'`, []any{day}},
		{`insert into matches (edition_id, round_code, scheduled_at, status, import_key)
		  select te.id, 'R32', $1::timestamptz, 'scheduled', 'zzt_' || te.slug
		  from tournament_editions te where te.slug in ('zzt_slam_2026', 'zzt_slam_wta_2026')`, []any{at}},
		{`insert into match_participants (match_id, side, slot, player_id)
		  select m.id, v.side, 1, p.id
		  from (values ('zzt_zzt_slam_2026', 1, 'zzt_atp_man'), ('zzt_zzt_slam_2026', 2, 'zzt_atp_opp'),
		               ('zzt_zzt_slam_wta_2026', 1, 'zzt_wta_woman'), ('zzt_zzt_slam_wta_2026', 2, 'zzt_wta_opp')
		       ) v(key, side, slug)
		  join matches m on m.import_key = v.key
		  join players p on p.slug = v.slug`, nil},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("фикстура главной: %v", err)
		}
	}
}

func TestHomeFeedFiltersByTour(t *testing.T) {
	now := time.Now().UTC()
	homeTourFixture(t, now)
	pool := testPool(t)
	followed := []string{"zzt_atp_man", "zzt_wta_woman"}

	for _, c := range []struct {
		tour             Tour
		mine, other      string
		edition, otherEd string
	}{
		{TourATP, "zzt_atp_man", "zzt_wta_woman", "zzt_slam_2026", "zzt_slam_wta_2026"},
		{TourWTA, "zzt_wta_woman", "zzt_atp_man", "zzt_slam_wta_2026", "zzt_slam_2026"},
	} {
		feed, err := GetHomeFeed(context.Background(), pool, "en", followed, 7, time.UTC, now, true, c.tour)
		if err != nil {
			t.Fatalf("%s: %v", c.tour, err)
		}
		if len(feed.YourSeason) != 1 || feed.YourSeason[0].Player.Slug != c.mine {
			var got []string
			for _, s := range feed.YourSeason {
				got = append(got, s.Player.Slug)
			}
			t.Errorf("%s: your_season = %v, want [%s]", c.tour, got, c.mine)
		}
		for _, p := range feed.AllPlayers {
			if p.Tour != c.tour {
				t.Errorf("%s: all_players содержит %s (%s)", c.tour, p.Slug, p.Tour)
			}
		}
		editions := map[string]bool{}
		for _, h := range feed.Tournaments {
			editions[h.Edition] = true
		}
		if !editions[c.edition] || editions[c.otherEd] {
			t.Errorf("%s: tournaments = %v, want %s без %s", c.tour, editions, c.edition, c.otherEd)
		}
		if f := feed.FeaturedTournament; f != nil && f.Edition == c.otherEd {
			t.Errorf("%s: featured — розыгрыш другой лиги %s", c.tour, f.Edition)
		}
	}
}

func TestFollowsFilteredByTourKeepOrder(t *testing.T) {
	tourFixture(t)
	got, err := followedInTour(context.Background(), testPool(t),
		[]string{"zzt_wta_new", "zzt_atp_man", "unknown", "zzt_wta_woman"}, TourWTA)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "zzt_wta_new,zzt_wta_woman" {
		t.Errorf("followedInTour = %v", got)
	}
}

func TestTournamentCategoryByTour(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		brand string
		tour  Tour
		meta  *string
		want  *string
	}{
		{"madrid", TourATP, nil, str("ATP 1000")},
		{"madrid", TourWTA, nil, str("WTA 1000")},
		{"wimbledon", TourWTA, nil, str("Grand Slam")},
		{"washington", TourWTA, nil, str("WTA 500")},
		{"zzt_unknown", TourWTA, str("WTA 250"), str("WTA 250")},
		{"madrid", TourATP, str("ATP 500"), str("ATP 500")},
		{"zzt_unknown", TourWTA, nil, nil},
		{"chengdu", TourWTA, nil, nil}, // мужской 250 не становится женским
	}
	for _, c := range cases {
		got := homeCategory(c.brand, c.tour, c.meta)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("homeCategory(%s, %s, %v) = %v, want %v", c.brand, c.tour, c.meta, got, c.want)
		}
	}
	if categoryWeight["WTA 1000"] != categoryWeight["ATP 1000"] || categoryWeight["WTA 250"] != categoryWeight["ATP 250"] {
		t.Error("веса WTA-категорий должны совпадать с ATP-аналогами")
	}
	if tournamentCategory("madrid", TourWTA) != "WTA 1000" || tournamentCategory("madrid", TourATP) != "Masters 1000" {
		t.Error("tournamentCategory: madrid → Masters 1000 у ATP, WTA 1000 у WTA")
	}
}

func TestWidgetFeedMixesTours(t *testing.T) {
	now := time.Now().UTC()
	homeTourFixture(t, now)
	feed, err := GetWidgetFeed(context.Background(), testPool(t), []string{"zzt_atp_man", "zzt_wta_woman"}, time.UTC, now)
	if err != nil {
		t.Fatal(err)
	}
	tours := map[string]Tour{}
	for _, r := range feed.Rows {
		tours[r.Player.Slug] = r.Player.Tour
	}
	if tours["zzt_atp_man"] != TourATP || tours["zzt_wta_woman"] != TourWTA {
		t.Errorf("rows: %v, want обе лиги с верным tour (state %s)", tours, feed.State)
	}
	cards := map[string]Tour{}
	for _, c := range feed.Season {
		cards[c.Player.Slug] = c.Player.Tour
	}
	if cards["zzt_atp_man"] != TourATP || cards["zzt_wta_woman"] != TourWTA {
		t.Errorf("season: %v, want обе лиги", cards)
	}
}
