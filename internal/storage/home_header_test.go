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
