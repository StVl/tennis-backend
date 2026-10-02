package storage

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tbdSlotName = "To be determined"

// CourtImageBySurface — фото корта по покрытию (hard/clay/grass).
var CourtImageBySurface = map[string]string{
	"hard":  "https://raw.githubusercontent.com/StVl/tennis-widget-config/refs/heads/main/usopen%20court.webp",
	"clay":  "https://raw.githubusercontent.com/StVl/tennis-widget-config/refs/heads/main/garros%20court.jpg",
	"grass": "https://raw.githubusercontent.com/StVl/tennis-widget-config/refs/heads/main/wimbledon.jpg",
}

func courtImageURL(surface string) *string {
	url := CourtImageBySurface[strings.ToLower(surface)]
	if url == "" {
		return nil
	}
	return &url
}

var monthAbbrev = [...]string{
	"", "Jan", "Feb", "Mar", "Apr", "May", "Jun",
	"Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
}

func formatDateRange(start, end time.Time) string {
	if start.Month() == end.Month() && start.Year() == end.Year() {
		return fmt.Sprintf("%d – %d %s", start.Day(), end.Day(), monthAbbrev[start.Month()])
	}
	return fmt.Sprintf("%d %s – %d %s",
		start.Day(), monthAbbrev[start.Month()],
		end.Day(), monthAbbrev[end.Month()])
}

func slotName(first, last, display string) string {
	first, last, display = strings.TrimSpace(first), strings.TrimSpace(last), strings.TrimSpace(display)
	if last != "" && first != "" {
		r, _ := utf8.DecodeRuneInString(first)
		return string(r) + ". " + last
	}
	if last != "" {
		return last
	}
	parts := strings.Fields(display)
	if len(parts) >= 2 {
		r, _ := utf8.DecodeRuneInString(parts[0])
		return string(r) + ". " + strings.Join(parts[1:], " ")
	}
	return display
}

func loadDrawCard(ctx context.Context, pool *pgxpool.Pool, lang, editionSlug string) ([]DrawCardRound, error) {
	rows, err := pool.Query(ctx, `
		select m.id, m.round_code, m.bracket_pos, m.winner_side,
		       coalesce(ro.label->>$2, ro.label->>'en', m.round_code),
		       p1.slug, p1.display_name, p1.first_name, p1.last_name, c1.flag_emoji, e1.seed,
		       p2.slug, p2.display_name, p2.first_name, p2.last_name, c2.flag_emoji, e2.seed
		from matches m
		join tournament_editions te on te.id = m.edition_id
		join rounds ro on ro.code = m.round_code
		left join match_participants mp1 on mp1.match_id = m.id and mp1.side = 1 and mp1.slot = 1
		left join players p1 on p1.id = mp1.player_id
		left join countries c1 on c1.code = p1.country_code
		left join tournament_entries e1 on e1.edition_id = te.id and e1.player_id = p1.id
		left join match_participants mp2 on mp2.match_id = m.id and mp2.side = 2 and mp2.slot = 1
		left join players p2 on p2.id = mp2.player_id
		left join countries c2 on c2.code = p2.country_code
		left join tournament_entries e2 on e2.edition_id = te.id and e2.player_id = p2.id
		where te.slug = $1
		  and m.round_code !~* '^q[0-9]'
		  and m.status <> 'cancelled'
		order by ro.sort_order, m.bracket_pos nulls last, m.id`,
		editionSlug, lang)
	if err != nil {
		return nil, err
	}
	type row struct {
		matchID                  int64
		round, title             string
		pos                      *int
		winner                   *int
		s1slug, s1disp, s1f, s1l *string
		s1flag                   *string
		s1seed                   *int
		s2slug, s2disp, s2f, s2l *string
		s2flag                   *string
		s2seed                   *int
	}
	raw, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.matchID, &x.round, &x.pos, &x.winner, &x.title,
			&x.s1slug, &x.s1disp, &x.s1f, &x.s1l, &x.s1flag, &x.s1seed,
			&x.s2slug, &x.s2disp, &x.s2f, &x.s2l, &x.s2flag, &x.s2seed)
		return x, err
	})
	if err != nil {
		return nil, err
	}

	out := []DrawCardRound{}
	for _, x := range raw {
		m := DrawCardMatch{
			ID:         x.matchID,
			BracketPos: x.pos,
			Top:        makeSlot(x.s1slug, x.s1disp, x.s1f, x.s1l, x.s1flag, x.s1seed, x.winner, 1),
			Bottom:     makeSlot(x.s2slug, x.s2disp, x.s2f, x.s2l, x.s2flag, x.s2seed, x.winner, 2),
		}
		if len(out) == 0 || out[len(out)-1].Code != x.round {
			out = append(out, DrawCardRound{Code: x.round, Title: x.title, Matches: []DrawCardMatch{}})
		}
		last := &out[len(out)-1]
		last.Matches = append(last.Matches, m)
	}
	return completeDrawCard(ctx, pool, lang, out)
}

// completeDrawCard — полная сетка до финала для розыгрыша с позициями (импорт PDF ATP).
//
// Строки в БД есть только у матчей, где известен хотя бы один участник, поэтому без
// достройки карточка обрывалась на последнем раунде, где кто-то уже вышел. Узлы без строки
// становятся TBD/TBD с отрицательным id (-(глубина*1000+позиция)) — стабильным и не
// пересекающимся с matches.id. Первый раунд не достраивается: там пустая позиция — bye.
func completeDrawCard(ctx context.Context, pool *pgxpool.Pool, lang string, rounds []DrawCardRound) ([]DrawCardRound, error) {
	maxDepth := -1
	for _, r := range rounds {
		d, ok := roundDepth(r.Code)
		if !ok {
			return rounds, nil // старые коды R1…R4 — дерева нет, отдаём как есть
		}
		for _, m := range r.Matches {
			if m.BracketPos == nil {
				return rounds, nil
			}
		}
		if d > maxDepth {
			maxDepth = d
		}
	}
	if maxDepth < 1 {
		return rounds, nil
	}
	titles, err := roundTitles(ctx, pool, lang)
	if err != nil {
		return nil, err
	}
	byCode := map[string]DrawCardRound{}
	for _, r := range rounds {
		byCode[r.Code] = r
	}
	out := make([]DrawCardRound, 0, maxDepth+1)
	for d := maxDepth; d >= 0; d-- {
		code := roundCode(d)
		r, ok := byCode[code]
		if !ok {
			r = DrawCardRound{Code: code, Title: titles[code], Matches: []DrawCardMatch{}}
		}
		if d < maxDepth {
			r.Matches = fillDrawRound(r.Matches, d)
		}
		out = append(out, r)
	}
	return out, nil
}

// fillDrawRound — матчи раунда глубины d на всех позициях 1…2^d, по порядку.
func fillDrawRound(matches []DrawCardMatch, depth int) []DrawCardMatch {
	byPos := map[int]DrawCardMatch{}
	for _, m := range matches {
		byPos[*m.BracketPos] = m
	}
	n := 1 << depth
	out := make([]DrawCardMatch, 0, n)
	for pos := 1; pos <= n; pos++ {
		if m, ok := byPos[pos]; ok {
			out = append(out, m)
			continue
		}
		p := pos
		out = append(out, DrawCardMatch{
			ID:         -int64(depth*1000 + pos),
			BracketPos: &p,
			Top:        DrawSlot{Name: tbdSlotName, TBD: true},
			Bottom:     DrawSlot{Name: tbdSlotName, TBD: true},
		})
	}
	return out
}

func roundTitles(ctx context.Context, pool *pgxpool.Pool, lang string) (map[string]string, error) {
	rows, err := pool.Query(ctx, `select code, coalesce(label->>$1, label->>'en', code) from rounds`, lang)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var code, title string
		if err := rows.Scan(&code, &title); err != nil {
			return nil, err
		}
		out[code] = title
	}
	return out, rows.Err()
}

func makeSlot(slug, display, first, last, flag *string, seed *int, winnerSide *int, side int) DrawSlot {
	if slug == nil {
		return DrawSlot{Name: tbdSlotName, TBD: true}
	}
	return DrawSlot{
		Name:   slotName(deref(first), deref(last), deref(display)),
		Slug:   slug,
		Flag:   emptyToNil(flag),
		Seed:   seed,
		Winner: winnerSide != nil && *winnerSide == side,
	}
}

func emptyToNil(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}
