package storage

import (
	"fmt"
	"sort"
	"time"
)

// Срез сетки для карточки турнира на главной: только та часть дерева, где играют подписки,
// не больше трёх колонок — раунд R, раунд R+1 и следующий узел (чемпион или пустой узел
// следующего раунда). Правила — спецификация главной, §5.4; вся логика здесь, клиент рисует.

// relevanceWindow — сколько вылетевший (или взявший титул) игрок ещё держит турнир
// карточкой-сеткой.
const relevanceWindow = 24 * time.Hour

// BracketSlice — срез для карточки.
type BracketSlice struct {
	Note BracketNote `json:"note"`
	// Коды раундов трёх колонок; последняя — раунд конечного узла, «W» для чемпиона.
	Columns []string       `json:"columns"`
	Groups  []BracketGroup `json:"groups"`
	EndNode BracketEndNode `json:"end_node"`
	// Все боксы колонок 1 и 2; группы ссылаются на них по key.
	Matches []BracketMatch `json:"matches"`
	// Бокс, выбранный при открытии (§5.5).
	DefaultKey string `json:"default_key"`
	// Свежие результаты подписок в этом розыгрыше (§10.1), в том числе вне среза: исход виден
	// и по самой сетке — победитель уже стоит в следующем раунде, срез сдвинулся. Пока любой из
	// них не раскрыт, клиент закрывает карточку целиком.
	RecentResults []RecentResult `json:"recent_results"`
}

// RecentResult — сыгранный матч, по которому клиент проверяет раскрытие спойлера.
type RecentResult struct {
	MatchID int64    `json:"match_id"`
	Sets    [][]*int `json:"sets"`
}

// BracketGroup — матчи колонки 1, кормящие один матч колонки 2.
type BracketGroup struct {
	Keys    []string `json:"keys"`
	NextKey string   `json:"next_key"`
}

// BracketNote — почему показан этот кусок сетки. Копию собирает клиент (EN/RU, спойлеры).
type BracketNote struct {
	// champion | could_meet | in_round | plays | played
	Kind        string         `json:"kind"`
	Round       string         `json:"round,omitempty"`
	Count       int            `json:"count,omitempty"`
	Player      *BracketPlayer `json:"player,omitempty"`
	ScheduledAt *time.Time     `json:"scheduled_at,omitempty"`
	// Подписок в сетке, не попавших в срез: «+N more in the draw».
	More int `json:"more,omitempty"`
}

// BracketEndNode — третья колонка.
type BracketEndNode struct {
	Kind   string         `json:"kind"` // champion | pending
	Round  string         `json:"round"`
	Player *BracketPlayer `json:"player,omitempty"`
	// Локальная дата матча следующего раунда (pending): по ней клиент пишет «SUN».
	Date *string `json:"date,omitempty"`
}

// BracketMatch — бокс сетки. key стабилен для узла («QF-3»), а у строки БД есть ещё match_id —
// по нему клиент помнит раскрытые спойлеры.
type BracketMatch struct {
	Key         string         `json:"key"`
	MatchID     *int64         `json:"match_id"`
	Round       string         `json:"round"`
	Status      string         `json:"status"` // scheduled | live | completed
	ScheduledAt *time.Time     `json:"scheduled_at"`
	Court       *string        `json:"court"`
	WinnerSide  *int           `json:"winner_side"`
	Sets        [][]*int       `json:"sets"`
	Sides       [2]BracketSide `json:"sides"`
}

// BracketSide — игрок или плейсхолдер «FAA / KHA» / «TBD».
type BracketSide struct {
	Player      *BracketPlayer `json:"player"`
	Placeholder *string        `json:"placeholder"`
}

type BracketPlayer struct {
	Slug        string  `json:"slug"`
	FirstName   string  `json:"first_name"`
	LastName    string  `json:"last_name"`
	ShortName   string  `json:"short_name"`
	DisplayName string  `json:"display_name"`
	PhotoURL    *string `json:"photo_url"`
	Rank        *int    `json:"rank"`
	Followed    bool    `json:"followed"`
}

func nodeKey(depth, pos int) string { return fmt.Sprintf("%s-%d", roundCode(depth), pos) }

func parentPos(pos int) int  { return (pos + 1) / 2 }
func siblingPos(pos int) int { return pos + 1 - 2*((pos+1)%2) } // 1↔2, 3↔4

// followedFocus — подписка в сетке и её «матч в фокусе».
type followedFocus struct {
	slug  string
	rank  int // 0 — без рейтинга
	focus *TreeMatch
	// eliminated — проиграл; champion — выиграл финал
	eliminated, champion bool
}

// relevantFollowed — подписки, ради которых турнир показывается сеткой (§5): ещё в сетке
// или вылетели/взяли титул не раньше relevanceWindow назад. Фокус — самый поздний матч
// игрока: live или ближайший, а у выбывшего — последний сыгранный.
func relevantFollowed(t *DrawTree, followed map[string]bool, now time.Time) []followedFocus {
	latest := map[string]*TreeMatch{}
	for _, m := range t.Nodes {
		for _, s := range m.Sides {
			if s == nil || !followed[s.Slug] {
				continue
			}
			if cur := latest[s.Slug]; cur == nil || m.Depth < cur.Depth {
				latest[s.Slug] = m
			}
		}
	}
	var out []followedFocus
	for slug, m := range latest {
		f := followedFocus{slug: slug, focus: m}
		for _, s := range m.Sides {
			if s != nil && s.Slug == slug && s.Rank != nil {
				f.rank = *s.Rank
			}
		}
		if w := m.winner(); w != nil {
			f.eliminated = w.Slug != slug
			f.champion = w.Slug == slug && m.Depth == 0
		}
		if f.eliminated || f.champion {
			// время исхода — время матча; без времени считаем исход свежим, пока идёт турнир
			if m.ScheduledAt != nil && now.Sub(*m.ScheduledAt) > relevanceWindow {
				continue
			}
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return lessRank(out[i].rank, out[j].rank, out[i].slug, out[j].slug) })
	return out
}

func lessRank(a, b int, sa, sb string) bool {
	switch {
	case a == 0 && b == 0:
		return sa < sb
	case a == 0:
		return false
	case b == 0:
		return true
	case a != b:
		return a < b
	}
	return sa < sb
}

// BuildBracketSlice — срез сетки; nil, если подписок в турнире не осталось (тогда обложка).
//
// loc нужна только для даты пустого узла: «SUN» — день следующего раунда у пользователя.
func BuildBracketSlice(t *DrawTree, followed map[string]bool, now time.Time, loc *time.Location) *BracketSlice {
	focus := relevantFollowed(t, followed, now)
	if len(focus) == 0 {
		return nil
	}

	// R — самый ранний раунд среди фокусов. Финал колонкой 1 не бывает: тогда показываем
	// полуфиналы → финал → чемпион (Chengdu в спецификации).
	d := 0
	for _, f := range focus {
		if f.focus.Depth > d {
			d = f.focus.Depth
		}
	}
	if d == 0 {
		d = 1
	}

	// Колонка 1: фокусные матчи раунда R; при R = финалу-1 — матчи этого раунда с подписками.
	col1 := map[int]bool{}
	for _, f := range focus {
		switch {
		case f.focus.Depth == d:
			col1[f.focus.Pos] = true
		case d == 1 && f.focus.Depth == 0:
			// R сдвинут с финала на полуфинал: колонка 1 — полуфинал этого игрока
			for pos := 1; pos <= 2; pos++ {
				if m := t.Node(1, pos); m != nil && m.hasPlayer(f.slug) {
					col1[pos] = true
				}
			}
		}
	}
	if len(col1) == 0 {
		return nil
	}
	// Сосед по вилке — если в нём подписка или следующая колонка — финал.
	for pos := range col1 {
		sib := siblingPos(pos)
		if d-1 == 0 || nodeHasFollowed(t.Node(d, sib), followed) {
			col1[sib] = true
		}
	}

	// Группы: матчи колонки 1, кормящие один матч колонки 2.
	groups := map[int][]int{}
	for pos := range col1 {
		p := parentPos(pos)
		groups[p] = append(groups[p], pos)
	}
	parents := make([]int, 0, len(groups))
	for p := range groups {
		sort.Ints(groups[p])
		parents = append(parents, p)
	}
	score := func(p int) (int, time.Time) {
		n, first := 0, time.Time{}
		for _, pos := range groups[p] {
			m := t.Node(d, pos)
			n += countFollowed(m, followed)
			if m != nil && m.ScheduledAt != nil && (first.IsZero() || m.ScheduledAt.Before(first)) {
				first = *m.ScheduledAt
			}
		}
		return n, first
	}
	sort.Slice(parents, func(i, j int) bool {
		ni, ti := score(parents[i])
		nj, tj := score(parents[j])
		if ni != nj {
			return ni > nj
		}
		if !ti.Equal(tj) {
			if ti.IsZero() || tj.IsZero() {
				return tj.IsZero()
			}
			return ti.Before(tj)
		}
		return parents[i] < parents[j]
	})
	// Не больше двух групп, и две — только если их матчи колонки 2 кормят один узел: иначе
	// скобка в конечный узел соврала бы о сетке.
	chosen := []int{parents[0]}
	for _, p := range parents[1:] {
		if parentPos(p) == parentPos(parents[0]) {
			chosen = append(chosen, p)
			break
		}
	}
	sort.Ints(chosen)

	slice := &BracketSlice{Columns: []string{roundCode(d), roundCode(d - 1)}}
	shown := map[string]bool{}
	addBox := func(depth, pos int) string {
		key := nodeKey(depth, pos)
		box := bracketBox(t, depth, pos, followed)
		for _, s := range box.Sides {
			if s.Player != nil {
				shown[s.Player.Slug] = true
			}
		}
		slice.Matches = append(slice.Matches, box)
		return key
	}
	for _, p := range chosen {
		g := BracketGroup{}
		for _, pos := range groups[p] {
			g.Keys = append(g.Keys, addBox(d, pos))
		}
		g.NextKey = addBox(d-1, p)
		slice.Groups = append(slice.Groups, g)
	}

	// Колонка 3.
	if d-1 == 0 {
		final := t.Node(0, 1)
		if w := final.winner(); w != nil {
			slice.Columns = append(slice.Columns, "W")
			slice.EndNode = BracketEndNode{Kind: "champion", Round: "W", Player: bracketPlayer(w, followed)}
		} else {
			slice.Columns = append(slice.Columns, "W")
			slice.EndNode = BracketEndNode{Kind: "pending", Round: "W", Date: roundDate(t, 0, 1, loc)}
		}
	} else {
		next := parentPos(chosen[0])
		slice.Columns = append(slice.Columns, roundCode(d-2))
		slice.EndNode = BracketEndNode{Kind: "pending", Round: roundCode(d - 2), Date: roundDate(t, d-2, next, loc)}
	}

	slice.Note = bracketNote(t, focus, d, chosen, followed)
	for _, f := range focus {
		if !shown[f.slug] {
			slice.Note.More++
		}
	}
	slice.DefaultKey = defaultKey(t, slice, focus)
	slice.RecentResults = recentResults(t, followed, now)
	return slice
}

// recentResults — последний сыгранный матч каждой подписки, если он не старше relevanceWindow.
// Без времени матч считается свежим, как в relevantFollowed.
func recentResults(t *DrawTree, followed map[string]bool, now time.Time) []RecentResult {
	latest := map[string]*TreeMatch{}
	for _, m := range t.Nodes {
		if m.Status != "completed" {
			continue
		}
		for _, s := range m.Sides {
			if s == nil || !followed[s.Slug] {
				continue
			}
			if cur := latest[s.Slug]; cur == nil || m.Depth < cur.Depth {
				latest[s.Slug] = m
			}
		}
	}
	seen := map[int64]bool{}
	out := []RecentResult{}
	for _, m := range latest {
		if seen[m.ID] || (m.ScheduledAt != nil && now.Sub(*m.ScheduledAt) > relevanceWindow) {
			continue
		}
		seen[m.ID] = true
		sets := m.Sets
		if sets == nil {
			sets = [][]*int{}
		}
		out = append(out, RecentResult{MatchID: m.ID, Sets: sets})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MatchID < out[j].MatchID })
	return out
}

func nodeHasFollowed(m *TreeMatch, followed map[string]bool) bool {
	return countFollowed(m, followed) > 0
}

func countFollowed(m *TreeMatch, followed map[string]bool) int {
	if m == nil {
		return 0
	}
	n := 0
	for _, s := range m.Sides {
		if s != nil && followed[s.Slug] {
			n++
		}
	}
	return n
}

func bracketPlayer(p *TreePlayer, followed map[string]bool) *BracketPlayer {
	if p == nil {
		return nil
	}
	return &BracketPlayer{
		Slug: p.Slug, FirstName: p.FirstName, LastName: p.LastName, ShortName: shortLastName(p.LastName),
		DisplayName: p.DisplayName, PhotoURL: p.PhotoURL, Rank: p.Rank, Followed: followed[p.Slug],
	}
}

// bracketBox — бокс узла; у неизвестной стороны — плейсхолдер из кормящего матча.
func bracketBox(t *DrawTree, depth, pos int, followed map[string]bool) BracketMatch {
	box := BracketMatch{Key: nodeKey(depth, pos), Round: roundCode(depth), Status: "scheduled", Sets: [][]*int{}}
	m := t.Node(depth, pos)
	if m != nil {
		id := m.ID
		box.MatchID, box.Status, box.ScheduledAt, box.Court = &id, m.Status, m.ScheduledAt, m.Court
		box.WinnerSide = m.WinnerSide
		if m.Sets != nil {
			box.Sets = m.Sets
		}
	}
	for i := 0; i < 2; i++ {
		var p *TreePlayer
		if m != nil {
			p = m.Sides[i]
		}
		if p != nil {
			box.Sides[i].Player = bracketPlayer(p, followed)
			continue
		}
		ph := placeholder(t, depth+1, 2*pos-1+i)
		box.Sides[i].Placeholder = &ph
	}
	return box
}

// placeholder — «ABC / XYZ» для победителя ещё не сыгранного матча, иначе «TBD».
func placeholder(t *DrawTree, depth, pos int) string {
	feeder := t.Node(depth, pos)
	if feeder == nil || (feeder.Sides[0] == nil && feeder.Sides[1] == nil) {
		return "TBD"
	}
	if w := feeder.winner(); w != nil {
		return playerCode(w)
	}
	return playerCode(feeder.Sides[0]) + " / " + playerCode(feeder.Sides[1])
}

// roundDate — локальная дата узла: время матча, если оно есть, иначе оценка от конца
// турнира (финал — последний день, полуфинал — накануне …).
func roundDate(t *DrawTree, depth, pos int, loc *time.Location) *string {
	var day time.Time
	if m := t.Node(depth, pos); m != nil && m.ScheduledAt != nil {
		day = m.ScheduledAt.In(loc)
	} else {
		day = t.End.AddDate(0, 0, -depth)
	}
	s := day.Format("2006-01-02")
	return &s
}

func bracketNote(t *DrawTree, focus []followedFocus, d int, chosen []int, followed map[string]bool) BracketNote {
	for _, f := range focus {
		if f.champion {
			p := playerOf(f.focus, f.slug)
			return BracketNote{Kind: "champion", Player: bracketPlayer(p, followed)}
		}
	}
	inRound := 0
	for _, f := range focus {
		if f.focus.Depth == d && !f.eliminated {
			inRound++
		}
	}
	// Три и больше в одном раунде — это и есть новость, даже если двое могут встретиться.
	if inRound >= 3 {
		return BracketNote{Kind: "in_round", Round: roundCode(d), Count: inRound}
	}
	// Двое подписок могут встретиться в матче колонки 2: по подписке в каждой половине.
	for _, p := range chosen {
		top, bottom := subtreeHasActive(t, d, 2*p-1, focus), subtreeHasActive(t, d, 2*p, focus)
		if top && bottom {
			return BracketNote{Kind: "could_meet", Round: roundCode(d - 1), Count: 2}
		}
	}
	if inRound >= 2 {
		return BracketNote{Kind: "in_round", Round: roundCode(d), Count: inRound}
	}
	f := focus[0] // самый высокий рейтинг
	p := bracketPlayer(playerOf(f.focus, f.slug), followed)
	if f.focus.Status == "completed" {
		return BracketNote{Kind: "played", Round: f.focus.Round, Player: p}
	}
	return BracketNote{Kind: "plays", Round: f.focus.Round, Player: p, ScheduledAt: f.focus.ScheduledAt}
}

// subtreeHasActive — в матче (depth, pos) есть подписка, ещё не вылетевшая.
func subtreeHasActive(t *DrawTree, depth, pos int, focus []followedFocus) bool {
	m := t.Node(depth, pos)
	if m == nil {
		return false
	}
	for _, f := range focus {
		if !f.eliminated && m.hasPlayer(f.slug) {
			if w := m.winner(); w == nil || w.Slug == f.slug {
				return true
			}
		}
	}
	return false
}

func playerOf(m *TreeMatch, slug string) *TreePlayer {
	for _, s := range m.Sides {
		if s != nil && s.Slug == slug {
			return s
		}
	}
	return nil
}

// defaultKey — §5.5: финал, если турнир сыгран и в финале подписка; иначе ближайший
// фокусный матч подписки с самым высоким рейтингом; иначе первый бокс.
func defaultKey(t *DrawTree, s *BracketSlice, focus []followedFocus) string {
	has := map[string]bool{}
	for _, m := range s.Matches {
		has[m.Key] = true
	}
	if final := t.Node(0, 1); final != nil && final.Status == "completed" && has[nodeKey(0, 1)] {
		for _, f := range focus {
			if final.hasPlayer(f.slug) {
				return nodeKey(0, 1)
			}
		}
	}
	for _, f := range focus {
		key := nodeKey(f.focus.Depth, f.focus.Pos)
		if f.focus.Status != "completed" && has[key] {
			return key
		}
	}
	for _, f := range focus {
		if key := nodeKey(f.focus.Depth, f.focus.Pos); has[key] {
			return key
		}
	}
	return s.Matches[0].Key
}
