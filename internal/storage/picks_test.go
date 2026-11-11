package storage

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Фикстура Daily picks: розыгрыш pkt_2026 (Швеция, ATP 250, хард).
// Завтра — 6 матчей R16, послезавтра нет ничего; вчера — 3 матча R32: два сыграны, один
// отказ (walkover). Подписка — pkt_p1 (играет завтра в матче с худшим рангом).
var pkNow = time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC) // 19:00 в Стокгольме

func seedPicks(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	cleanupPicks(t, pool)
	t.Cleanup(func() { cleanupPicks(t, pool) })

	exec(`insert into tournaments (slug, name, tour_code, country_code, description, conditions, metadata)
		values ('pkt', 'Picks Test Open', 'atp', 'SE', '{}', '{}', '{}')`)
	exec(`insert into tournament_editions (tournament_id, year, slug, start_date, end_date, surface,
		discipline, draw_status, gender, metadata)
		select id, 2026, 'pkt_2026', '2026-11-08', '2026-11-14', 'hard', 'singles', 'drawn', 'm',
		       '{"category":"ATP 250"}'
		from tournaments where slug = 'pkt'`)
	for i := 1; i <= 18; i++ {
		exec(`insert into players (slug, first_name, last_name, display_name, gender)
			values ($1, 'First'||$2::text, 'Last'||$2::text, 'Player '||$2::text, 'm')`,
			"pkt_p"+itoa64(int64(i)), itoa64(int64(i)))
	}
	ids := map[string]int64{}
	match := func(key, round string, at time.Time, status, outcome string, winner *int, pos int, a, b int, sets [][2]int) {
		t.Helper()
		var id int64
		err := pool.QueryRow(ctx, `insert into matches (edition_id, round_code, scheduled_at, status, outcome,
				winner_side, bracket_pos, import_key)
			select id, $1, $2, $3::match_status_t, $4::match_outcome_t, $5, $6, $7
			from tournament_editions where slug = 'pkt_2026' returning id`,
			round, at, status, outcome, winner, pos, "pkt_"+key).Scan(&id)
		if err != nil {
			t.Fatalf("seed match %s: %v", key, err)
		}
		ids[key] = id
		for side, p := range []int{a, b} {
			exec(`insert into match_participants (match_id, side, slot, player_id)
				select $1, $2, 1, id from players where slug = $3`, id, side+1, "pkt_p"+itoa64(int64(p)))
		}
		for n, s := range sets {
			exec(`insert into match_sets (match_id, set_no, side1_games, side2_games) values ($1, $2, $3, $4)`,
				id, n+1, s[0], s[1])
		}
	}
	two := 2
	tomorrow := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		match("t"+itoa64(int64(i)), "R16", tomorrow.Add(time.Duration(i)*time.Hour), "scheduled", "normal", nil,
			i, 2*i+1, 2*i+2, nil)
	}
	yesterday := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	match("y0", "R32", yesterday, "completed", "normal", &two, 0, 13, 14, [][2]int{{4, 6}, {3, 6}})
	match("y1", "R32", yesterday.Add(time.Hour), "completed", "normal", &two, 1, 15, 16, [][2]int{{6, 7}, {2, 6}})
	match("y2", "R32", yesterday.Add(2*time.Hour), "completed", "walkover", &two, 2, 17, 18, nil)
	return ids
}

func cleanupPicks(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	for _, q := range []string{
		`delete from matches where import_key like 'pkt_%'`,
		`delete from players where slug like 'pkt_p%'`,
		`delete from tournament_editions where slug = 'pkt_2026'`,
		`delete from tournaments where slug = 'pkt'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}
}

func pkDayByDate(resp *PicksResponse, date string) *PicksDay {
	for i := range resp.Days {
		if resp.Days[i].Date == date && resp.Days[i].Edition == "pkt_2026" {
			return &resp.Days[i]
		}
	}
	return nil
}

func TestPicks_BuildsTomorrowFromFixture(t *testing.T) {
	pool := testPool(t)
	ids := seedPicks(t, pool)
	resp, err := GetPicksDays(context.Background(), pool, []string{"pkt_p11"}, TourATP, pkNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := pkDayByDate(resp, "2026-11-11")
	if d == nil {
		t.Fatalf("no 2026-11-11 day in %+v", resp.Days)
	}
	if d.ID != "atp-2026-11-11" || d.League != "atp" || d.Round != "R16" || d.Category == nil || *d.Category != "ATP 250" {
		t.Fatalf("day header = %+v", d)
	}
	// Розыгрыш с 2026-11-08: 11-11 — четвёртый день.
	if d.TournamentDay != 4 {
		t.Fatalf("tournament_day = %d, want 4", d.TournamentDay)
	}
	if len(d.Matches) != 6 {
		t.Fatalf("matches = %d, want 6", len(d.Matches))
	}
	for i, m := range d.Matches {
		if m.MatchID != ids["t"+itoa64(int64(i))] || m.Order != i || m.Status != "scheduled" {
			t.Fatalf("match %d = %+v", i, m)
		}
		if !m.LockAt.Equal(time.Date(2026, 11, 11, 10+i, 0, 0, 0, time.UTC)) {
			t.Fatalf("lock_at %d = %v", i, m.LockAt)
		}
	}
	if d.Matches[0].Players[0].FirstName != "First1" || d.Matches[0].Players[1].LastName != "Last2" {
		t.Fatalf("players = %+v", d.Matches[0].Players)
	}
	if d.NextRound == nil || d.NextRound.Round != "QF" || len(d.NextRound.Pairs) != 3 {
		t.Fatalf("next round = %+v", d.NextRound)
	}
}

func TestPicks_YesterdayVoidAndFinished(t *testing.T) {
	pool := testPool(t)
	seedPicks(t, pool)
	resp, err := GetPicksDays(context.Background(), pool, nil, TourATP, pkNow, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := pkDayByDate(resp, "2026-11-09")
	if d == nil || len(d.Matches) != 3 {
		t.Fatalf("yesterday = %+v", d)
	}
	if d.Matches[0].Status != "finished" || d.Matches[0].WinnerSlug == nil || *d.Matches[0].WinnerSlug != "pkt_p14" {
		t.Fatalf("finished = %+v", d.Matches[0])
	}
	if d.Matches[0].ScoreText == nil || *d.Matches[0].ScoreText != "6-4, 6-3" {
		t.Fatalf("score from the winner side = %q", *d.Matches[0].ScoreText)
	}
	if d.Matches[2].Status != "void" || d.Matches[2].VoidReason == nil || *d.Matches[2].VoidReason != "walkover" {
		t.Fatalf("walkover = %+v", d.Matches[2])
	}
}

func TestPicks_PinnedKeepsList(t *testing.T) {
	pool := testPool(t)
	ids := seedPicks(t, pool)
	pinned := []int64{ids["t0"], ids["t2"], ids["t4"]}
	resp, err := GetPicksDays(context.Background(), pool, nil, TourATP, pkNow, pinned)
	if err != nil {
		t.Fatal(err)
	}
	d := pkDayByDate(resp, "2026-11-11")
	if d == nil || len(d.Matches) != 3 {
		t.Fatalf("pinned day = %+v", d)
	}
	for i, m := range d.Matches {
		if m.MatchID != pinned[i] {
			t.Fatalf("pinned order: got %d at %d, want %d", m.MatchID, i, pinned[i])
		}
	}
}
