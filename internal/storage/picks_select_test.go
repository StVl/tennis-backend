package storage

import (
	"strconv"
	"testing"
	"time"
)

func pkRank(n int) *int { return &n }

func pkCand(id int64, edition, brand, cat string, at time.Time, rankA, rankB int, followed bool) pickCandidate {
	c := cat
	return pickCandidate{
		MatchID: id, Edition: edition, Brand: brand, Name: brand, Round: "R16", Category: &c,
		ScheduledAt: at, Status: "scheduled", Outcome: "normal",
		A:        MatchPlayer{Slug: "a" + itoa64(id), Rank: pkRank(rankA)},
		B:        MatchPlayer{Slug: "b" + itoa64(id), Rank: pkRank(rankB)},
		Followed: followed,
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

var pkDay = time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)

func pkHours(h int) time.Time { return pkDay.Add(time.Duration(h) * time.Hour) }

func ids(cs []pickCandidate) []int64 {
	out := make([]int64, len(cs))
	for i, c := range cs {
		out[i] = c.MatchID
	}
	return out
}

func eqIDs(t *testing.T, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func TestSelectGameDay_FollowedTournamentWins(t *testing.T) {
	var cs []pickCandidate
	for i := int64(1); i <= 4; i++ {
		cs = append(cs, pkCand(i, "paris_2026", "paris_masters", "ATP 1000", pkHours(int(i)), 5, 20, false))
	}
	for i := int64(11); i <= 13; i++ {
		cs = append(cs, pkCand(i, "metz_2026", "metz", "ATP 250", pkHours(int(i-10)), 30, 40, i == 12))
	}
	ed, chosen, ok := selectGameDay(cs)
	if !ok || ed != "metz_2026" {
		t.Fatalf("edition = %q ok=%v, want metz_2026", ed, ok)
	}
	eqIDs(t, ids(chosen), []int64{11, 12, 13})
}

func TestSelectGameDay_CategoryThenCount(t *testing.T) {
	var cs []pickCandidate
	for i := int64(1); i <= 3; i++ {
		cs = append(cs, pkCand(i, "basel_2026", "basel", "ATP 500", pkHours(int(i)), 5, 20, false))
	}
	for i := int64(11); i <= 16; i++ {
		cs = append(cs, pkCand(i, "metz_2026", "metz", "ATP 250", pkHours(int(i-10)), 30, 40, false))
	}
	if ed, _, _ := selectGameDay(cs); ed != "basel_2026" {
		t.Fatalf("category: edition = %q, want basel_2026", ed)
	}
	// Равный уровень: больше матчей — выше.
	for i := range cs[:3] {
		cs[i].Category = cs[3].Category
	}
	if ed, _, _ := selectGameDay(cs); ed != "metz_2026" {
		t.Fatalf("count: edition = %q, want metz_2026", ed)
	}
}

func TestSelectGameDay_FewerThanThree(t *testing.T) {
	cs := []pickCandidate{
		pkCand(1, "metz_2026", "metz", "ATP 250", pkHours(1), 1, 2, true),
		pkCand(2, "metz_2026", "metz", "ATP 250", pkHours(2), 1, 2, false),
	}
	if _, _, ok := selectGameDay(cs); ok {
		t.Fatal("2 matches must not make a game day")
	}
}

func TestSelectGameDay_TakesSixFollowedFirstThenRank(t *testing.T) {
	var cs []pickCandidate
	// 8 матчей: лучший ранг пары = 10*i; подписка у матча 8 (худший ранг).
	for i := int64(1); i <= 8; i++ {
		cs = append(cs, pkCand(i, "paris_2026", "paris_masters", "ATP 1000", pkHours(int(9-i)), int(10*i), 200, i == 8))
	}
	cs[2].A.Rank = nil // у матча 3 ранг только у B (200) — он уходит вниз
	_, chosen, ok := selectGameDay(cs)
	if !ok || len(chosen) != 6 {
		t.Fatalf("chosen %d ok=%v, want 6", len(chosen), ok)
	}
	got := map[int64]bool{}
	for _, c := range chosen {
		got[c.MatchID] = true
	}
	for _, want := range []int64{8, 1, 2, 4, 5, 6} {
		if !got[want] {
			t.Fatalf("chosen %v, missing %d", ids(chosen), want)
		}
	}
}

func TestSelectGameDay_DisplayOrderByTime(t *testing.T) {
	cs := []pickCandidate{
		pkCand(3, "metz_2026", "metz", "ATP 250", pkHours(5), 1, 2, false),
		pkCand(1, "metz_2026", "metz", "ATP 250", pkHours(2), 3, 4, false),
		pkCand(2, "metz_2026", "metz", "ATP 250", pkHours(2), 5, 6, false),
	}
	_, chosen, _ := selectGameDay(cs)
	eqIDs(t, ids(chosen), []int64{1, 2, 3})
}

func TestGameDayDate_TournamentZone(t *testing.T) {
	c := pickCandidate{ScheduledAt: time.Date(2026, 1, 20, 23, 30, 0, 0, time.UTC)}
	if got := gameDayDate(c, tournamentZone("australian_open", "AU")).String(); got != "2026-01-21" {
		t.Fatalf("Melbourne date = %s, want 2026-01-21", got)
	}
	if got := gameDayDate(c, tournamentZone("zzz", "")).String(); got != "2026-01-20" {
		t.Fatalf("unknown zone date = %s, want 2026-01-20 (UTC)", got)
	}
	if z := tournamentZone("stockholm", "SE"); z.String() != "Europe/Stockholm" {
		t.Fatalf("SE zone = %s", z)
	}
}

func TestPickStatus(t *testing.T) {
	zone := tournamentZone("stockholm", "SE") // UTC+1 в ноябре
	at := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	nextDay0359 := time.Date(2026, 11, 11, 2, 59, 0, 0, time.UTC) // 03:59 по Стокгольму
	nextDay0400 := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)  // 04:00 по Стокгольму
	cases := []struct {
		name            string
		status, outcome string
		now             time.Time
		want, reason    string
	}{
		{"walkover", "completed", "walkover", at, "void", "walkover"},
		{"cancelled", "cancelled", "normal", at, "void", "cancelled"},
		{"retirement counts", "completed", "retirement", at, "finished", ""},
		{"default counts", "completed", "default", at, "finished", ""},
		{"normal finish", "completed", "normal", at, "finished", ""},
		{"live", "live", "normal", at, "live", ""},
		{"scheduled 03:59 next day", "scheduled", "normal", nextDay0359, "scheduled", ""},
		{"scheduled 04:00 next day", "scheduled", "normal", nextDay0400, "void", "unplayed"},
		{"live stuck past 04:00", "live", "normal", nextDay0400, "void", "unplayed"},
	}
	for _, tc := range cases {
		c := pickCandidate{ScheduledAt: at, Status: tc.status, Outcome: tc.outcome}
		got, reason := pickStatus(c, tc.now, zone)
		if got != tc.want || reason != tc.reason {
			t.Errorf("%s: got %s/%q, want %s/%q", tc.name, got, reason, tc.want, tc.reason)
		}
	}
}
