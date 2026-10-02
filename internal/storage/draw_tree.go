package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Дерево розыгрыша — основная сетка как узлы (глубина, позиция).
//
// Глубина — сколько раундов до финала: F=0, SF=1, QF=2, R16=3 … R128=6. Матч (d, k) кормит
// (d-1, ceil(k/2)), его сосед по вилке — (d, k±1). Позиции пишет импорт полных сеток
// (tennis-data-storage, scripts/import_draws.py: официальный PDF ATP) и только он, поэтому
// дерево есть лишь у розыгрышей, где у матчей проставлен bracket_pos. Коды раундов там
// единые, по размеру дерева; старые R1…R4 без позиций деревом не считаются — главная
// показывает такой турнир обложкой, а не угадывает сетку.

// TreePlayer — участник матча в дереве.
type TreePlayer struct {
	Slug        string
	FirstName   string
	LastName    string
	DisplayName string
	PhotoURL    *string
	Rank        *int
}

// TreeMatch — узел дерева, у которого в БД есть строка.
type TreeMatch struct {
	ID          int64
	Depth       int
	Pos         int
	Round       string
	Status      string // scheduled | live | completed
	ScheduledAt *time.Time
	Court       *string
	WinnerSide  *int
	Sets        [][]*int
	Sides       [2]*TreePlayer // nil — участник ещё неизвестен
}

// DrawTree — сетка одного розыгрыша.
type DrawTree struct {
	Edition string
	Start   time.Time
	End     time.Time
	Nodes   map[[2]int]*TreeMatch // {глубина, позиция}
}

var depthByCode = map[string]int{"F": 0, "SF": 1, "QF": 2, "R16": 3, "R32": 4, "R64": 5, "R128": 6}

// roundDepth — глубина по коду; false для кодов без однозначной глубины (R1…R4, Q*, RR).
func roundDepth(code string) (int, bool) {
	d, ok := depthByCode[code]
	return d, ok
}

func roundCode(depth int) string {
	for code, d := range depthByCode {
		if d == depth {
			return code
		}
	}
	return ""
}

// Node — узел по глубине и позиции; nil, если строки нет (оба участника ещё неизвестны).
func (t *DrawTree) Node(depth, pos int) *TreeMatch {
	return t.Nodes[[2]int{depth, pos}]
}

// Rounds — число раундов основной сетки (5 для сетки на 32).
func (t *DrawTree) Rounds() int {
	max := -1
	for k := range t.Nodes {
		if k[0] > max {
			max = k[0]
		}
	}
	return max + 1
}

func (m *TreeMatch) hasPlayer(slug string) bool {
	for _, s := range m.Sides {
		if s != nil && s.Slug == slug {
			return true
		}
	}
	return false
}

// winner — победитель завершённого матча или nil.
func (m *TreeMatch) winner() *TreePlayer {
	if m == nil || m.Status != "completed" || m.WinnerSide == nil {
		return nil
	}
	if *m.WinnerSide == 1 || *m.WinnerSide == 2 {
		return m.Sides[*m.WinnerSide-1]
	}
	return nil
}

// playerCode — три буквы для плейсхолдера «FAA / KHA»: первые три буквы фамилии, а у
// составной фамилии — инициал имени и инициалы частей (Félix Auger-Aliassime → FAA).
func playerCode(p *TreePlayer) string {
	if p == nil {
		return "TBD"
	}
	last := strings.TrimSpace(p.LastName)
	if last == "" {
		last = lastWord(p.DisplayName)
	}
	parts := strings.FieldsFunc(last, func(r rune) bool { return r == '-' || r == ' ' })
	if len(parts) > 1 {
		// «de Minaur», «van de Zandschulp»: частицы со строчной — не часть кода
		var caps []string
		for _, part := range parts {
			if r, _ := utf8.DecodeRuneInString(part); unicode.IsUpper(r) {
				caps = append(caps, part)
			}
		}
		if len(caps) > 1 {
			code := initial(p.FirstName)
			for _, part := range caps {
				code += initial(part)
			}
			return strings.ToUpper(firstRunes(code, 3))
		}
		if len(caps) == 1 {
			last = caps[0]
		}
	}
	return strings.ToUpper(firstRunes(foldASCII(last), 3))
}

// shortLastName — фамилия для бокса сетки: длиннее 10 символов — сокращается
// («Auger-Aliassime» → «Auger-A.», «Davidovich Fokina» → «Davidovich F.»).
func shortLastName(last string) string {
	if utf8.RuneCountInString(last) <= 10 {
		return last
	}
	for _, sep := range []string{"-", " "} {
		if i := strings.LastIndex(last, sep); i > 0 {
			head, tail := last[:i], last[i+len(sep):]
			if utf8.RuneCountInString(head) <= 10 && tail != "" {
				return head + sep + initial(tail) + "."
			}
		}
	}
	return firstRunes(last, 9) + "."
}

func initial(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r, _ := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r))
}

func firstRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func lastWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

// foldASCII — «Mensík» → «Mensik» для кода из трёх букв.
func foldASCII(s string) string {
	repl := strings.NewReplacer("á", "a", "à", "a", "ä", "a", "â", "a", "ã", "a", "å", "a",
		"é", "e", "è", "e", "ë", "e", "ê", "e", "í", "i", "ì", "i", "ï", "i", "î", "i",
		"ó", "o", "ò", "o", "ö", "o", "ô", "o", "õ", "o", "ø", "o", "ú", "u", "ù", "u",
		"ü", "u", "û", "u", "ý", "y", "ñ", "n", "ç", "c", "č", "c", "ć", "c", "š", "s",
		"ž", "z", "ř", "r", "ě", "e", "ł", "l", "Á", "A", "É", "E", "Í", "I", "Ó", "O",
		"Ú", "U", "Č", "C", "Š", "S", "Ž", "Z", "Ř", "R", "Ł", "L")
	return repl.Replace(s)
}

// loadDrawTrees — деревья розыгрышей одним запросом. Розыгрыш без единого bracket_pos в
// результат не попадает: дерева у него нет.
func loadDrawTrees(ctx context.Context, pool *pgxpool.Pool, editions []string) (map[string]*DrawTree, error) {
	rows, err := pool.Query(ctx, `
		select te.slug, te.start_date, te.end_date,
		       m.id, m.round_code, m.bracket_pos, m.status::text, m.scheduled_at, m.court,
		       m.winner_side,
		       coalesce((select json_agg(json_build_array(s.side1_games, s.side2_games, s.tiebreak_loser_points)
		                                 order by s.set_no)
		                 from match_sets s where s.match_id = m.id)::text, '[]'),
		       p1.slug, p1.first_name, p1.last_name, p1.display_name, p1.photo_url, r1.rank,
		       p2.slug, p2.first_name, p2.last_name, p2.display_name, p2.photo_url, r2.rank
		from matches m
		join tournament_editions te on te.id = m.edition_id
		left join match_participants mp1 on mp1.match_id = m.id and mp1.side = 1 and mp1.slot = 1
		left join players p1 on p1.id = mp1.player_id
		left join v_current_rankings r1 on r1.player_id = p1.id and r1.tour_code = 'atp'
		left join match_participants mp2 on mp2.match_id = m.id and mp2.side = 2 and mp2.slot = 1
		left join players p2 on p2.id = mp2.player_id
		left join v_current_rankings r2 on r2.player_id = p2.id and r2.tour_code = 'atp'
		where te.slug = any($1)
		  and m.bracket_pos is not null
		  and m.status <> 'cancelled'
		  and m.round_code = any($2)`,
		editions, []string{"F", "SF", "QF", "R16", "R32", "R64", "R128"})
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]*DrawTree{}
	for rows.Next() {
		var (
			edition, setsJSON string
			start, end        time.Time
			m                 TreeMatch
			pos               int
			s1, s2            struct {
				slug, first, last, display *string
				photo                      *string
				rank                       *int
			}
		)
		if err := rows.Scan(&edition, &start, &end,
			&m.ID, &m.Round, &pos, &m.Status, &m.ScheduledAt, &m.Court, &m.WinnerSide, &setsJSON,
			&s1.slug, &s1.first, &s1.last, &s1.display, &s1.photo, &s1.rank,
			&s2.slug, &s2.first, &s2.last, &s2.display, &s2.photo, &s2.rank); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(setsJSON), &m.Sets); err != nil {
			return nil, fmt.Errorf("unmarshal sets: %w", err)
		}
		depth, _ := roundDepth(m.Round)
		m.Depth, m.Pos = depth, pos
		for i, s := range []struct {
			slug, first, last, display *string
			photo                      *string
			rank                       *int
		}{s1, s2} {
			if s.slug == nil {
				continue
			}
			m.Sides[i] = &TreePlayer{
				Slug: *s.slug, FirstName: deref(s.first), LastName: deref(s.last),
				DisplayName: deref(s.display), PhotoURL: s.photo, Rank: s.rank,
			}
			if m.Sides[i].LastName == "" {
				m.Sides[i].LastName = lastWord(m.Sides[i].DisplayName)
			}
		}
		t := out[edition]
		if t == nil {
			t = &DrawTree{Edition: edition, Start: start, End: end, Nodes: map[[2]int]*TreeMatch{}}
			out[edition] = t
		}
		node := m
		t.Nodes[[2]int{depth, pos}] = &node
	}
	return out, rows.Err()
}
