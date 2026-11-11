package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Daily picks — игровой день: турнир, 3–6 матчей, их статус, победитель и счёт. Выборы
// пользователя хранятся на устройстве; сервер только собирает день (см. picks_select.go).

type PicksResponse struct {
	Days []PicksDay `json:"days"`
}

type PicksDay struct {
	ID             string          `json:"id"` // "atp-2026-11-10": лига + местная дата турнира
	League         Tour            `json:"league"`
	Date           string          `json:"date"`
	Edition        string          `json:"edition"`
	TournamentName string          `json:"tournament_name"`
	Category       *string         `json:"category"`
	Surface        string          `json:"surface"`
	Round          string          `json:"round"`
	FinalRound     bool            `json:"final_round"`
	Matches        []PicksMatch    `json:"matches"`
	NextRound      *PicksNextRound `json:"next_round"`
	ChampionSlug   *string         `json:"champion_slug"`
}

type PicksMatch struct {
	MatchID    int64          `json:"match_id"`
	Order      int            `json:"order"`
	Round      string         `json:"round"`
	LockAt     time.Time      `json:"lock_at"`
	Status     string         `json:"status"`      // scheduled | live | finished | void
	VoidReason *string        `json:"void_reason"` // walkover | cancelled | unplayed
	WinnerSlug *string        `json:"winner_slug"`
	ScoreText  *string        `json:"score_text"` // со стороны победителя
	Players    [2]PicksPlayer `json:"players"`
	H2H        *[2]int        `json:"h2h"` // победы players[0], players[1]; null — не встречались
}

type PicksPlayer struct {
	Slug      string  `json:"slug"`
	FirstName string  `json:"first_name"`
	LastName  string  `json:"last_name"`
	Rank      *int    `json:"rank"`
	PhotoURL  *string `json:"photo_url"`
}

// PicksNextRound — следующий раунд: пары слотов, каждый слот — один матч этого раунда.
type PicksNextRound struct {
	Round string         `json:"round"`
	Pairs [][2]PicksSlot `json:"pairs"`
}

type PicksSlot struct {
	Candidates []string `json:"candidates"`
	WinnerSlug *string  `json:"winner_slug"`
}

var nextRoundCode = map[string]string{
	"R128": "R64", "R64": "R32", "R32": "R16", "R16": "QF", "QF": "SF", "SF": "F",
}

type picksRow struct {
	cand    pickCandidate
	country string
	surface string
	sets    [][]*int
	players [2]PicksPlayer
}

// GetPicksDays — игровые дни тура на вчера, сегодня и завтра (по местной дате турнира),
// старые первыми. pinned — матчи дня, который у клиента уже заморожен (начался первый матч):
// для дня этих матчей список берётся из pinned, а не отбирается заново, чтобы смена подписок
// или рангов посреди дня не поменяла игру.
func GetPicksDays(ctx context.Context, pool *pgxpool.Pool, followed []string, tour Tour,
	now time.Time, pinned []int64) (*PicksResponse, error) {
	rows, err := loadPicksCandidates(ctx, pool, tour, now)
	if err != nil {
		return nil, err
	}
	followedSet := map[string]bool{}
	for _, s := range followed {
		followedSet[s] = true
	}
	pinnedSet := map[int64]bool{}
	for _, id := range pinned {
		pinnedSet[id] = true
	}

	byID := map[int64]picksRow{}
	byDate := map[string][]pickCandidate{}
	zones := map[int64]*time.Location{}
	pinnedDate := ""
	for _, r := range rows {
		zone := tournamentZone(r.cand.Brand, r.country)
		date := gameDayDate(r.cand, zone)
		today := civilOf(now.In(zone))
		if !withinOneDay(date, today) {
			continue
		}
		r.cand.Followed = followedSet[r.cand.A.Slug] || followedSet[r.cand.B.Slug]
		byID[r.cand.MatchID] = r
		zones[r.cand.MatchID] = zone
		key := date.String()
		byDate[key] = append(byDate[key], r.cand)
		if pinnedSet[r.cand.MatchID] {
			pinnedDate = key
		}
	}

	dates := make([]string, 0, len(byDate))
	for d := range byDate {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	resp := &PicksResponse{Days: []PicksDay{}}
	for _, date := range dates {
		var chosen []pickCandidate
		ok := false
		if date == pinnedDate {
			for _, c := range byDate[date] {
				if pinnedSet[c.MatchID] {
					chosen = append(chosen, c)
				}
			}
			sortByStart(chosen)
			ok = len(chosen) > 0
		} else {
			_, chosen, ok = selectGameDay(byDate[date])
		}
		if !ok {
			continue
		}
		day, err := buildPicksDay(ctx, pool, tour, date, chosen, byID, zones, now)
		if err != nil {
			return nil, err
		}
		resp.Days = append(resp.Days, day)
	}
	return resp, nil
}

func withinOneDay(d, today civilDate) bool {
	a := time.Date(d.Y, d.M, d.D, 0, 0, 0, 0, time.UTC)
	b := time.Date(today.Y, today.M, today.D, 0, 0, 0, 0, time.UTC)
	diff := a.Sub(b).Hours() / 24
	return diff >= -1 && diff <= 1
}

func buildPicksDay(ctx context.Context, pool *pgxpool.Pool, tour Tour, date string, chosen []pickCandidate,
	byID map[int64]picksRow, zones map[int64]*time.Location, now time.Time) (PicksDay, error) {
	first := byID[chosen[0].MatchID]
	day := PicksDay{
		ID: string(tour) + "-" + date, League: tour, Date: date,
		Edition: first.cand.Edition, TournamentName: first.cand.Name, Category: first.cand.Category,
		Surface: first.surface, Matches: make([]PicksMatch, 0, len(chosen)),
	}
	roundCount := map[string]int{}
	for i, c := range chosen {
		r := byID[c.MatchID]
		status, reason := pickStatus(c, now, zones[c.MatchID])
		m := PicksMatch{
			MatchID: c.MatchID, Order: i, Round: c.Round, LockAt: c.ScheduledAt,
			Status: status, Players: r.players,
		}
		if reason != "" {
			m.VoidReason = &reason
		}
		if status == "finished" && c.WinnerSide != nil {
			w := r.players[*c.WinnerSide-1].Slug
			m.WinnerSlug = &w
			if text := scoreText(winnerSets(r.sets, *c.WinnerSide)); text != "" {
				m.ScoreText = &text
			}
		}
		h2h, err := picksH2H(ctx, pool, r.players[0].Slug, r.players[1].Slug, c.MatchID)
		if err != nil {
			return day, err
		}
		m.H2H = h2h
		day.Matches = append(day.Matches, m)
		roundCount[c.Round]++
	}
	for round, n := range roundCount {
		if n > roundCount[day.Round] || (n == roundCount[day.Round] && round < day.Round) {
			day.Round = round
		}
	}
	day.FinalRound = day.Round == "F"
	if day.FinalRound {
		day.ChampionSlug = day.Matches[0].WinnerSlug
		return day, nil
	}
	next, err := picksNextRound(ctx, pool, first.cand.Edition, day.Round, chosen)
	if err != nil {
		return day, err
	}
	day.NextRound = next
	return day, nil
}

// winnerSets — сеты со стороны победителя: «6-4 7-5», а не «4-6 5-7».
func winnerSets(sets [][]*int, winnerSide int) [][]*int {
	if winnerSide != 2 {
		return sets
	}
	out := make([][]*int, len(sets))
	for i, s := range sets {
		flipped := append([]*int(nil), s...)
		if len(flipped) >= 2 {
			flipped[0], flipped[1] = s[1], s[0]
		}
		out[i] = flipped
	}
	return out
}

const picksCandidatesQuery = `
	select m.id, te.slug, t.slug, t.name, coalesce(t.country_code, ''), m.round_code,
	       coalesce(te.metadata->>'category', ''), te.surface::text,
	       m.scheduled_at, m.status::text, m.outcome::text, m.winner_side, m.bracket_pos,
	       coalesce(parts.j::text, '[]'), coalesce(sets.j::text, '[]')
	from matches m
	join tournament_editions te on te.id = m.edition_id
	join tournaments t on t.id = te.tournament_id
	left join lateral (
		select json_agg(json_build_object(
		         'side', mp.side, 'slug', p.slug, 'first_name', coalesce(p.first_name, ''),
		         'last_name', coalesce(p.last_name, p.display_name), 'photo_url', p.photo_url,
		         'rank', r.rank)
		       order by mp.side, mp.slot) as j
		from match_participants mp
		join players p on p.id = mp.player_id
		left join v_current_rankings r on r.player_id = p.id and r.tour_code = tour_code_for(p.gender)
		where mp.match_id = m.id
	) parts on true
	left join lateral (
		select json_agg(json_build_array(s.side1_games, s.side2_games, s.tiebreak_loser_points)
		       order by s.set_no) as j
		from match_sets s where s.match_id = m.id
	) sets on true
	where m.discipline = 'singles'
	  and te.gender = $1::gender_t
	  and m.round_code not in ('Q1', 'Q2', 'Q3')
	  and m.scheduled_at between $2 and $3
	order by m.scheduled_at, m.id`

type picksParticipant struct {
	Side      int     `json:"side"`
	Slug      string  `json:"slug"`
	FirstName string  `json:"first_name"`
	LastName  string  `json:"last_name"`
	PhotoURL  *string `json:"photo_url"`
	Rank      *int    `json:"rank"`
}

// loadPicksCandidates — матчи тура в окне ±3 дня; день турнира считается уже в Go. Матчи
// без обоих участников (не известен соперник) отбрасываются.
func loadPicksCandidates(ctx context.Context, pool *pgxpool.Pool, tour Tour, now time.Time) ([]picksRow, error) {
	rows, err := pool.Query(ctx, picksCandidatesQuery, tour.Gender(),
		now.Add(-72*time.Hour), now.Add(72*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []picksRow
	for rows.Next() {
		var (
			r                   picksRow
			category, partsJSON string
			setsJSON            string
			outcome             *string
		)
		if err := rows.Scan(&r.cand.MatchID, &r.cand.Edition, &r.cand.Brand, &r.cand.Name, &r.country,
			&r.cand.Round, &category, &r.surface, &r.cand.ScheduledAt, &r.cand.Status, &outcome,
			&r.cand.WinnerSide, &r.cand.BracketPos, &partsJSON, &setsJSON); err != nil {
			return nil, err
		}
		if category != "" {
			r.cand.Category = &category
		} else {
			r.cand.Category = homeCategory(r.cand.Brand, tour, nil)
		}
		if outcome != nil {
			r.cand.Outcome = *outcome
		}
		var parts []picksParticipant
		if err := json.Unmarshal([]byte(partsJSON), &parts); err != nil {
			return nil, fmt.Errorf("unmarshal participants: %w", err)
		}
		if len(parts) != 2 || parts[0].Side != 1 || parts[1].Side != 2 {
			continue
		}
		for i, p := range parts {
			r.players[i] = PicksPlayer{Slug: p.Slug, FirstName: p.FirstName, LastName: p.LastName,
				Rank: p.Rank, PhotoURL: p.PhotoURL}
		}
		r.cand.A = MatchPlayer{Slug: parts[0].Slug, Rank: parts[0].Rank}
		r.cand.B = MatchPlayer{Slug: parts[1].Slug, Rank: parts[1].Rank}
		if err := json.Unmarshal([]byte(setsJSON), &r.sets); err != nil {
			return nil, fmt.Errorf("unmarshal sets: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// picksH2H — личные встречи до этого матча; null, если не встречались.
func picksH2H(ctx context.Context, pool *pgxpool.Pool, a, b string, exclude int64) (*[2]int, error) {
	var won, lost int
	err := pool.QueryRow(ctx, `
		select count(*) filter (where vpm.result = 'won'), count(*) filter (where vpm.result = 'lost')
		from v_player_matches vpm
		join players p on p.id = vpm.player_id and p.slug = $1
		join players op on op.id = vpm.opponent_id and op.slug = $2
		where vpm.status = 'completed' and vpm.match_id <> $3`, a, b, exclude).Scan(&won, &lost)
	if err != nil {
		return nil, err
	}
	if won+lost == 0 {
		return nil, nil
	}
	return &[2]int{won, lost}, nil
}

// picksNextRound — пары следующего раунда для блока «Up next»: матчи раунда дня с позициями
// 2k и 2k+1 дают одну пару. Берутся все матчи раунда, а не только выбранные: соседом
// выбранного матча может быть матч не из игры. Нет позиций в сетке — блока нет.
func picksNextRound(ctx context.Context, pool *pgxpool.Pool, edition, round string,
	chosen []pickCandidate) (*PicksNextRound, error) {
	next, ok := nextRoundCode[round]
	if !ok {
		return nil, nil
	}
	wanted := map[int]bool{}
	for _, c := range chosen {
		if c.Round == round && c.BracketPos != nil {
			wanted[*c.BracketPos/2] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		select m.bracket_pos, m.status::text, m.winner_side,
		       array(select p.slug from match_participants mp join players p on p.id = mp.player_id
		             where mp.match_id = m.id order by mp.side, mp.slot)
		from matches m join tournament_editions te on te.id = m.edition_id
		where te.slug = $1 and m.round_code = $2 and m.bracket_pos is not null
		order by m.bracket_pos`, edition, round)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	slots := map[int]*[2]PicksSlot{}
	var keys []int
	for rows.Next() {
		var (
			pos     int
			status  string
			winner  *int
			players []string
		)
		if err := rows.Scan(&pos, &status, &winner, &players); err != nil {
			return nil, err
		}
		if !wanted[pos/2] {
			continue
		}
		pair := slots[pos/2]
		if pair == nil {
			pair = &[2]PicksSlot{{Candidates: []string{}}, {Candidates: []string{}}}
			slots[pos/2] = pair
			keys = append(keys, pos/2)
		}
		slot := PicksSlot{Candidates: players}
		if status == "completed" && winner != nil && *winner-1 < len(players) {
			w := players[*winner-1]
			slot.WinnerSlug = &w
		}
		pair[pos%2] = slot
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Ints(keys)
	out := &PicksNextRound{Round: next, Pairs: make([][2]PicksSlot, 0, len(keys))}
	for _, k := range keys {
		out.Pairs = append(out.Pairs, *slots[k])
	}
	return out, nil
}
