package storage

import "testing"

func hp(slug, first string) HeaderPlayer { return HeaderPlayer{Slug: slug, FirstName: first} }

func TestHeaderStatusPriority(t *testing.T) {
	carlos := headerMatch{Player: hp("alcaraz", "Carlos"), Round: "QF", At: at(-3), Status: "scheduled"}
	jannik := headerMatch{Player: hp("sinner", "Jannik"), Round: "QF", At: at(-2), Status: "scheduled"}
	final := headerMatch{Player: hp("davidovich_fokina", "Alejandro"), Round: "F", At: at(1), City: "Chengdu"}
	tokyo := headerTournament{City: "Tokyo", Day: 3}

	cases := []struct {
		name  string
		facts headerFacts
		kind  string
	}{
		{"финал подписки сегодня", headerFacts{Today: []headerMatch{carlos, final}}, "title_day"},
		{"одна подписка сегодня", headerFacts{Today: []headerMatch{carlos}, Live: []headerTournament{tokyo}}, "plays_today"},
		{"две подписки сегодня", headerFacts{Today: []headerMatch{carlos, jannik}}, "on_court"},
		{"день полуфиналов", headerFacts{Live: []headerTournament{tokyo, {City: "Beijing", SemisToday: true}}}, "semis_day"},
		{"день четвертьфиналов", headerFacts{Live: []headerTournament{{City: "Tokyo", QuartersToday: true}}}, "quarters_day"},
		{"полуфиналы важнее вчерашнего титула", headerFacts{
			Yesterday: []headerMatch{{Player: hp("x", "X"), Round: "F", Status: "completed", Won: true}},
			Live:      []headerTournament{{City: "Tokyo", SemisToday: true}}}, "semis_day"},
		{"вчера титул", headerFacts{Yesterday: []headerMatch{{Player: hp("x", "X"), Round: "F", Status: "completed", Won: true}}}, "champion_yesterday"},
		{"вчера поражение", headerFacts{Yesterday: []headerMatch{{Player: hp("x", "X"), Round: "R16", Status: "completed"}}}, "lost_yesterday"},
		{"обычный день турнира", headerFacts{Live: []headerTournament{tokyo}}, "tournament_day"},
		{"финал без подписок", headerFacts{Live: []headerTournament{{City: "Chengdu", FinalToday: true}}}, "final_day"},
		{"турниров нет", headerFacts{}, "season"},
	}
	for _, c := range cases {
		if got := pickHeaderStatus(c.facts); got.Kind != c.kind {
			t.Errorf("%s: %s, ожидали %s", c.name, got.Kind, c.kind)
		}
	}

	s := pickHeaderStatus(headerFacts{Today: []headerMatch{carlos, {Player: hp("alcaraz", "Carlos"), Round: "QF", At: at(-6)}}})
	if s.Kind != "plays_today" || s.Player.FirstName != "Carlos" || !s.At.Equal(*at(-6)) {
		t.Fatalf("два матча одного игрока — это один игрок, время — раннее: %+v", s)
	}
	if s := pickHeaderStatus(headerFacts{Today: []headerMatch{final}}); s.City != "Chengdu" {
		t.Fatalf("title_day несёт город для нейтральной копии: %+v", s)
	}
}

// Несколько подходящих подписок — называем ту, у кого выше рейтинг, а не первую по подписке.
func TestHeaderStatusNamesTheHighestRanked(t *testing.T) {
	mensik := headerMatch{Player: hp("mensik", "Jakub"), Round: "QF", Rank: 15, Order: 0}
	alcaraz := headerMatch{Player: hp("alcaraz", "Carlos"), Round: "QF", Rank: 1, Order: 1}
	unranked := headerMatch{Player: hp("x", "X"), Round: "QF", Rank: 0, Order: 2}

	s := pickHeaderStatus(headerFacts{Today: []headerMatch{unranked, mensik, alcaraz}})
	if s.Kind != "on_court" || s.Count != 3 || s.Player == nil || s.Player.Slug != "alcaraz" {
		t.Fatalf("on_court должен назвать Алькараса: %+v", s)
	}

	finalM := mensik
	finalM.Round, finalM.City = "F", "Tokyo"
	finalA := alcaraz
	finalA.Round, finalA.City = "F", "Beijing"
	if s := pickHeaderStatus(headerFacts{Today: []headerMatch{finalM, finalA}}); s.Player.Slug != "alcaraz" || s.City != "Beijing" {
		t.Fatalf("title_day: %+v", s)
	}

	lostM, lostA := mensik, alcaraz
	lostM.Status, lostA.Status = "completed", "completed"
	if s := pickHeaderStatus(headerFacts{Yesterday: []headerMatch{lostM, lostA}}); s.Kind != "lost_yesterday" || s.Player.Slug != "alcaraz" {
		t.Fatalf("lost_yesterday: %+v", s)
	}

	// Без рейтингов решает порядок подписок.
	a, b := unranked, unranked
	a.Player, b.Player, b.Order = hp("a", "A"), hp("b", "B"), 3
	if s := pickHeaderStatus(headerFacts{Today: []headerMatch{b, a}}); s.Player.Slug != "a" {
		t.Fatalf("порядок подписок: %+v", s)
	}
}
