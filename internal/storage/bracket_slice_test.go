package storage

import (
	"testing"
	"time"
)

// Эталонные конфигурации — таблица «Reference configurations» спецификации главной, §5.4.

var testNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func pl(slug, first, last string, rank int) *TreePlayer {
	r := rank
	return &TreePlayer{Slug: slug, FirstName: first, LastName: last, DisplayName: first + " " + last, Rank: &r}
}

var (
	faa  = pl("auger_aliassime", "Felix", "Auger-Aliassime", 5)
	kha  = pl("khachanov", "Karen", "Khachanov", 18)
	men  = pl("mensik", "Jakub", "Mensik", 15)
	bub  = pl("bublik", "Alexander", "Bublik", 30)
	alc  = pl("alcaraz", "Carlos", "Alcaraz", 1)
	fri  = pl("fritz", "Taylor", "Fritz", 4)
	hur  = pl("hurkacz", "Hubert", "Hurkacz", 20)
	sha  = pl("shapovalov", "Denis", "Shapovalov", 25)
	bas  = pl("basilashvili", "Nikoloz", "Basilashvili", 90)
	adf  = pl("davidovich_fokina", "Alejandro", "Davidovich Fokina", 17)
	side = func(n int) *int { return &n }
)

func at(h int) *time.Time {
	t := testNow.Add(time.Duration(h) * time.Hour)
	return &t
}

func tree(nodes ...*TreeMatch) *DrawTree {
	t := &DrawTree{Edition: "x_2026", Start: testNow.AddDate(0, 0, -5), End: testNow.AddDate(0, 0, 1),
		Nodes: map[[2]int]*TreeMatch{}}
	for i, n := range nodes {
		n.ID = int64(100 + i)
		n.Round = roundCode(n.Depth)
		if n.Status == "" {
			n.Status = "scheduled"
		}
		t.Nodes[[2]int{n.Depth, n.Pos}] = n
	}
	return t
}

func follows(slugs ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range slugs {
		m[s] = true
	}
	return m
}

func box(t *testing.T, s *BracketSlice, key string) BracketMatch {
	t.Helper()
	for _, m := range s.Matches {
		if m.Key == key {
			return m
		}
	}
	t.Fatalf("нет бокса %s среди %v", key, keys(s))
	return BracketMatch{}
}

func keys(s *BracketSlice) []string {
	var out []string
	for _, m := range s.Matches {
		out = append(out, m.Key)
	}
	return out
}

func ph(b BracketMatch, i int) string {
	if b.Sides[i].Placeholder == nil {
		return b.Sides[i].Player.Slug
	}
	return *b.Sides[i].Placeholder
}

// Chengdu: подписка взяла титул — SF×2 → F → чемпион.
func TestSliceChampion(t *testing.T) {
	tr := tree(
		&TreeMatch{Depth: 1, Pos: 1, Sides: [2]*TreePlayer{hur, sha}, Status: "completed", WinnerSide: side(1), ScheduledAt: at(-26)},
		&TreeMatch{Depth: 1, Pos: 2, Sides: [2]*TreePlayer{bas, adf}, Status: "completed", WinnerSide: side(2), ScheduledAt: at(-25)},
		&TreeMatch{Depth: 0, Pos: 1, Sides: [2]*TreePlayer{hur, adf}, Status: "completed", WinnerSide: side(2), ScheduledAt: at(-2)},
	)
	s := BuildBracketSlice(tr, follows("davidovich_fokina"), testNow, time.UTC)
	if s == nil {
		t.Fatal("ожидался срез")
	}
	if got := s.Columns; len(got) != 3 || got[0] != "SF" || got[1] != "F" || got[2] != "W" {
		t.Fatalf("колонки %v", got)
	}
	if len(s.Groups) != 1 || len(s.Groups[0].Keys) != 2 || s.Groups[0].NextKey != "F-1" {
		t.Fatalf("группы %+v", s.Groups)
	}
	if s.EndNode.Kind != "champion" || s.EndNode.Player.Slug != "davidovich_fokina" {
		t.Fatalf("конечный узел %+v", s.EndNode)
	}
	if s.Note.Kind != "champion" || s.DefaultKey != "F-1" {
		t.Fatalf("заметка %+v, выбор %s", s.Note, s.DefaultKey)
	}
}

// Tokyo: две подписки могут встретиться в полуфинале.
func TestSliceCouldMeet(t *testing.T) {
	tr := tree(
		&TreeMatch{Depth: 2, Pos: 1, Sides: [2]*TreePlayer{faa, kha}, ScheduledAt: at(17)},
		&TreeMatch{Depth: 2, Pos: 2, Sides: [2]*TreePlayer{men, bub}, ScheduledAt: at(20)},
		&TreeMatch{Depth: 2, Pos: 3, Sides: [2]*TreePlayer{alc, fri}, ScheduledAt: at(19)},
	)
	s := BuildBracketSlice(tr, follows("auger_aliassime", "mensik"), testNow, time.UTC)
	if got := s.Columns; got[0] != "QF" || got[1] != "SF" || got[2] != "F" {
		t.Fatalf("колонки %v", got)
	}
	sf := box(t, s, "SF-1")
	if ph(sf, 0) != "FAA / KHA" || ph(sf, 1) != "MEN / BUB" {
		t.Fatalf("плейсхолдеры %q / %q", ph(sf, 0), ph(sf, 1))
	}
	if s.EndNode.Kind != "pending" || s.EndNode.Round != "F" || *s.EndNode.Date != "2026-10-05" {
		t.Fatalf("конечный узел %+v", s.EndNode)
	}
	if s.Note.Kind != "could_meet" || s.Note.Round != "SF" {
		t.Fatalf("заметка %+v", s.Note)
	}
	// по умолчанию — ближайший матч подписки с самым высоким рейтингом (FAA #5)
	if s.DefaultKey != "QF-1" {
		t.Fatalf("выбор %s", s.DefaultKey)
	}
	if len(s.Matches) != 3 { // QF-1, QF-2, SF-1 — третьего QF в срезе нет
		t.Fatalf("боксы %v", keys(s))
	}
}

// Три подписки в трёх разных QF: две группы (2 QF + 1 QF) → SF×2 → пустой узел финала.
func TestSliceThreeQuarterfinals(t *testing.T) {
	tr := tree(
		&TreeMatch{Depth: 2, Pos: 1, Sides: [2]*TreePlayer{faa, kha}, ScheduledAt: at(17)},
		&TreeMatch{Depth: 2, Pos: 2, Sides: [2]*TreePlayer{men, bub}, ScheduledAt: at(20)},
		&TreeMatch{Depth: 2, Pos: 3, Sides: [2]*TreePlayer{alc, fri}, ScheduledAt: at(19)},
	)
	s := BuildBracketSlice(tr, follows("auger_aliassime", "mensik", "alcaraz"), testNow, time.UTC)
	if len(s.Groups) != 2 || len(s.Groups[0].Keys) != 2 || len(s.Groups[1].Keys) != 1 {
		t.Fatalf("группы %+v", s.Groups)
	}
	sf2 := box(t, s, "SF-2")
	if ph(sf2, 0) != "ALC / FRI" || ph(sf2, 1) != "TBD" {
		t.Fatalf("SF-2: %q / %q", ph(sf2, 0), ph(sf2, 1))
	}
	if s.Note.Kind != "in_round" || s.Note.Count != 3 || s.Note.Round != "QF" {
		t.Fatalf("заметка %+v", s.Note)
	}
	if s.DefaultKey != "QF-3" { // Alcaraz #1
		t.Fatalf("выбор %s", s.DefaultKey)
	}
}

// Две группы, чьи матчи колонки 2 кормят разные узлы, в один срез не сводятся.
func TestSliceDropsGroupFromOtherHalf(t *testing.T) {
	tr := tree(
		&TreeMatch{Depth: 2, Pos: 1, Sides: [2]*TreePlayer{faa, kha}, ScheduledAt: at(17)},
		&TreeMatch{Depth: 3, Pos: 1, Sides: [2]*TreePlayer{men, bub}, ScheduledAt: at(3)},
		&TreeMatch{Depth: 3, Pos: 8, Sides: [2]*TreePlayer{alc, fri}, ScheduledAt: at(5)},
	)
	s := BuildBracketSlice(tr, follows("mensik", "alcaraz"), testNow, time.UTC)
	if len(s.Groups) != 1 {
		t.Fatalf("группы %+v", s.Groups)
	}
	if s.Note.More != 1 {
		t.Fatalf("ожидался «+1 more», заметка %+v", s.Note)
	}
}

// Вылетел больше суток назад — сетка не нужна.
func TestSliceEliminatedLongAgo(t *testing.T) {
	tr := tree(&TreeMatch{Depth: 3, Pos: 1, Sides: [2]*TreePlayer{men, bub}, Status: "completed",
		WinnerSide: side(2), ScheduledAt: at(-30)})
	if s := BuildBracketSlice(tr, follows("mensik"), testNow, time.UTC); s != nil {
		t.Fatalf("ожидался nil, получили %+v", s)
	}
	// а вчерашний вылет ещё держит сетку
	tr.Nodes[[2]int{3, 1}].ScheduledAt = at(-5)
	s := BuildBracketSlice(tr, follows("mensik"), testNow, time.UTC)
	if s == nil || s.Note.Kind != "played" {
		t.Fatalf("ожидалась заметка played, %+v", s)
	}
}

func TestPlayerCodeAndShortName(t *testing.T) {
	cases := map[string]string{
		playerCode(faa): "FAA",
		playerCode(kha): "KHA",
		playerCode(pl("de_minaur", "Alex", "de Minaur", 9)): "MIN",
		playerCode(pl("x", "Tomás", "Etcheverry", 9)):       "ETC",
		playerCode(nil): "TBD",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("код %q, ожидали %q", got, want)
		}
	}
	if got := shortLastName("Auger-Aliassime"); got != "Auger-A." {
		t.Errorf("short %q", got)
	}
	if got := shortLastName("Davidovich Fokina"); got != "Davidovich F." {
		t.Errorf("short %q", got)
	}
	if got := shortLastName("Mensik"); got != "Mensik" {
		t.Errorf("short %q", got)
	}
}

// Подписка сегодня выиграла QF: исход виден по самой сетке (ALC в полуфинале, срез сдвинулся
// на SF), поэтому срез несёт этот матч в recent_results — клиент закроет карточку целиком.
func TestSliceRecentResults(t *testing.T) {
	qf := func() *TreeMatch {
		return &TreeMatch{Depth: 2, Pos: 1, Status: "completed", ScheduledAt: at(-3), WinnerSide: side(1),
			Sets: [][]*int{{side(6), side(4), nil}, {side(7), side(6), side(5)}}, Sides: [2]*TreePlayer{alc, kha}}
	}
	qf2 := func() *TreeMatch {
		return &TreeMatch{Depth: 2, Pos: 2, ScheduledAt: at(4), Sides: [2]*TreePlayer{fri, hur}}
	}

	// строки полуфинала ещё нет — QF в срезе, в полуфинале плейсхолдер ALC
	s := BuildBracketSlice(tree(qf(), qf2()), follows("alcaraz"), testNow, time.UTC)
	if len(s.RecentResults) != 1 || s.RecentResults[0].MatchID != *box(t, s, "QF-1").MatchID {
		t.Fatalf("recent_results: %+v", s.RecentResults)
	}
	if got := s.RecentResults[0].Sets; len(got) != 2 || *got[1][2] != 5 {
		t.Fatalf("счёт: %+v", got)
	}

	// полуфинал записан — QF выпал из среза, но в recent_results остался
	s = BuildBracketSlice(tree(qf(), qf2(), &TreeMatch{Depth: 1, Pos: 1, ScheduledAt: at(24),
		Sides: [2]*TreePlayer{alc, nil}}), follows("alcaraz"), testNow, time.UTC)
	if s.Columns[0] != "SF" || len(s.RecentResults) != 1 || s.RecentResults[0].MatchID != 100 {
		t.Fatalf("сдвинутый срез: columns=%v recent=%+v", s.Columns, s.RecentResults)
	}

	// результат старше окна — не свежий; у подписки без сыгранных матчей список пуст, не null
	old := qf()
	old.ScheduledAt = at(-30)
	s = BuildBracketSlice(tree(old, qf2(), &TreeMatch{Depth: 1, Pos: 1, ScheduledAt: at(24),
		Sides: [2]*TreePlayer{alc, nil}}), follows("alcaraz"), testNow, time.UTC)
	if s.RecentResults == nil || len(s.RecentResults) != 0 {
		t.Fatalf("старый результат: %+v", s.RecentResults)
	}
}
