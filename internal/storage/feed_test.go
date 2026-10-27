package storage

import (
	"context"
	"testing"
	"time"
)

// Полдень UTC сегодня: матч фикстуры (now+2h) гарантированно попадает в сегодняшний день,
// во сколько бы ни шёл тест.
func todayNoonUTC() time.Time {
	return time.Now().UTC().Truncate(24 * time.Hour).Add(12 * time.Hour)
}

// Ingest вернул сыгранный матч в 'scheduled' (результат за пайплайном) — он не должен остаться
// «ближайшим» ни на главной, ни на обложке турнира, ни в карточке игрока.
func TestMatchFinishedByIngestIsNotNext(t *testing.T) {
	now := todayNoonUTC()
	homeTourFixture(t, now)
	pool := testPool(t)
	ctx := context.Background()

	next := func() (card, cover *int64) {
		t.Helper()
		feed, err := GetHomeFeed(ctx, pool, "en", []string{"zzt_atp_man"}, 0, time.UTC, now, true, TourATP)
		if err != nil {
			t.Fatal(err)
		}
		if len(feed.YourSeason) == 1 && feed.YourSeason[0].NextMatch != nil {
			card = &feed.YourSeason[0].NextMatch.MatchID
		}
		for _, h := range feed.Tournaments {
			if h.Edition == "zzt_slam_2026" && h.NextMatch != nil {
				cover = &h.NextMatch.MatchID
			}
		}
		return card, cover
	}
	if card, cover := next(); card == nil || cover == nil {
		t.Fatalf("до события матч должен быть ближайшим: card %v, cover %v", card, cover)
	}

	if _, err := pool.Exec(ctx, `
		insert into live_events (match_id, event, reason, created_at)
		select id, 'finished', 'test', now() from matches where import_key = 'zzt_zzt_slam_2026'`); err != nil {
		t.Fatal(err)
	}
	if card, cover := next(); card != nil || cover != nil {
		t.Errorf("сыгранный матч остался ближайшим: card %v, cover %v", card, cover)
	}
	if m, err := NextMatch(ctx, pool, "zzt_atp_man"); err != nil || m != nil {
		t.Errorf("NextMatch = %v, %v; want nil", m, err)
	}

	// Подняли снова — это опять живой матч, прошлое событие его не прячет.
	if _, err := pool.Exec(ctx,
		`update matches set status = 'live' where import_key = 'zzt_zzt_slam_2026'`); err != nil {
		t.Fatal(err)
	}
	if card, cover := next(); card == nil || cover == nil {
		t.Errorf("live-матч пропал: card %v, cover %v", card, cover)
	}
}

func TestHeadToHeadSkipsWalkovers(t *testing.T) {
	homeTourFixture(t, todayNoonUTC())
	pool := testPool(t)
	ctx := context.Background()
	for _, q := range []string{
		`insert into matches (edition_id, round_code, scheduled_at, status, winner_side, outcome, import_key)
		 select te.id, v.round, now() - interval '30 days', 'completed', 1, v.outcome::match_outcome_t, v.key
		 from tournament_editions te,
		      (values ('R16', 'normal', 'zzt_h2h_played'), ('QF', 'walkover', 'zzt_h2h_wo')) v(round, outcome, key)
		 where te.slug = 'zzt_slam_2026'`,
		`insert into match_participants (match_id, side, slot, player_id)
		 select m.id, v.side, 1, p.id
		 from matches m, (values (1, 'zzt_atp_man'), (2, 'zzt_atp_opp')) v(side, slug)
		 join players p on p.slug = v.slug
		 where m.import_key in ('zzt_h2h_played', 'zzt_h2h_wo')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	h, err := HeadToHead(ctx, pool, "zzt_atp_man", "zzt_atp_opp")
	if err != nil {
		t.Fatal(err)
	}
	if h.Wins != 1 || h.Losses != 0 || len(h.Matches) != 2 {
		t.Errorf("h2h = %d-%d, %d матчей; want 1-0 и оба матча в списке", h.Wins, h.Losses, len(h.Matches))
	}
}

// Отменённый сегодняшний матч подписки — не «подписка играет сегодня»: виджет должен уйти в split
// с колонкой TODAY, а сам отменённый матч в колонку не попадает.
func TestWidgetIgnoresCancelledMatchesToday(t *testing.T) {
	now := todayNoonUTC()
	homeTourFixture(t, now)
	pool := testPool(t)
	ctx := context.Background()
	for _, q := range []string{
		`update matches set status = 'cancelled' where import_key = 'zzt_zzt_slam_wta_2026'`,
		// у второй подписки — будущий турнир, чтобы виджету было что показать строкой
		`insert into tournament_editions (tournament_id, year, slug, start_date, end_date, surface, gender)
		 select t.id, 2027, 'zzt_slam_wta_2027', current_date + 30, current_date + 37, 'hard', 'f'
		 from tournaments t where t.slug = 'zzt_slam'`,
		`insert into tournament_entries (edition_id, player_id)
		 select te.id, p.id from tournament_editions te, players p
		 where te.slug = 'zzt_slam_wta_2027' and p.slug = 'zzt_wta_new'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	feed, err := GetWidgetFeed(ctx, pool, []string{"zzt_wta_woman", "zzt_wta_new"}, time.UTC, now)
	if err != nil {
		t.Fatal(err)
	}
	if feed.State != "split" {
		t.Errorf("state = %s, want split: отменённый матч засчитан как игра подписки", feed.State)
	}
	for _, m := range feed.TodayColumn {
		if m.P1LastName == "Woman" || m.P2LastName == "Woman" {
			t.Errorf("отменённый матч в колонке TODAY: %+v", m)
		}
	}
}
