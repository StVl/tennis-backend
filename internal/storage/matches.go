package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Match — нейтральная форма матча (не «глазами игрока»).
type Match struct {
	ID             int64           `json:"id"`
	Edition        string          `json:"edition"`
	TournamentName string          `json:"tournament_name"`
	Round          string          `json:"round"`
	ScheduledAt    *time.Time      `json:"scheduled_at"`
	Court          *string         `json:"court"`
	Status         string          `json:"status"`
	Surface        string          `json:"surface"`
	WinnerSide     *int            `json:"winner_side"`
	Outcome        *string         `json:"outcome"`
	Sides          []MatchSide     `json:"sides"`
	Sets           [][]*int        `json:"sets"` // [side1_games, side2_games, tiebreak|null]
	ScoreText      *string         `json:"score_text"`
	Live           json.RawMessage `json:"live"` // только для status=live
}

type MatchSide struct {
	Side    int           `json:"side"`
	Players []MatchPlayer `json:"players"`
}

type MatchPlayer struct {
	Slug     string  `json:"slug"`
	Name     string  `json:"name"`
	LastName *string `json:"last_name"`
	PhotoURL *string `json:"photo_url"`
	Rank     *int    `json:"rank"`
}

// участник в json_agg-выдаче
type flatParticipant struct {
	Side     int     `json:"side"`
	Slot     int     `json:"slot"`
	Slug     string  `json:"slug"`
	Name     string  `json:"name"`
	LastName *string `json:"last_name"`
	PhotoURL *string `json:"photo_url"`
	Rank     *int    `json:"rank"`
}

// MatchFilter — фильтры списка матчей.
type MatchFilter struct {
	Statuses   []string   // пусто = все
	Player     string     // slug
	Edition    string     // edition slug
	DayFrom    *time.Time // границы дня в таймзоне пользователя (UTC-инстанты)
	DayTo      *time.Time
	SortByRank bool // сортировка колонки TODAY: лучший рейтинг участника
	Limit      int
	Offset     int
}

const matchSelect = `
	select m.id, te.slug, t.name, m.round_code, m.scheduled_at, m.court,
	       m.status::text, te.surface::text, m.winner_side, m.outcome::text,
	       m.live_state::text,
	       coalesce(parts.j::text, '[]'), coalesce(sets.j::text, '[]')
	from matches m
	join tournament_editions te on te.id = m.edition_id
	join tournaments t on t.id = te.tournament_id
	left join lateral (
		select json_agg(json_build_object(
		         'side', mp.side, 'slot', mp.slot, 'slug', p.slug,
		         'name', p.display_name, 'last_name', p.last_name,
		         'photo_url', p.photo_url, 'rank', r.rank)
		       order by mp.side, mp.slot) as j
		from match_participants mp
		join players p on p.id = mp.player_id
		left join v_current_rankings r on r.player_id = p.id and r.tour_code = 'atp'
		where mp.match_id = m.id
	) parts on true
	left join lateral (
		select json_agg(json_build_array(s.side1_games, s.side2_games, s.tiebreak_loser_points)
		       order by s.set_no) as j
		from match_sets s where s.match_id = m.id
	) sets on true`

func ListMatches(ctx context.Context, pool *pgxpool.Pool, f MatchFilter) ([]Match, error) {
	var (
		conds []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if len(f.Statuses) > 0 {
		conds = append(conds, "m.status::text = any("+arg(f.Statuses)+")")
	}
	if f.Player != "" {
		conds = append(conds, `exists (select 1 from match_participants mp2
			join players p2 on p2.id = mp2.player_id
			where mp2.match_id = m.id and p2.slug = `+arg(f.Player)+")")
	}
	if f.Edition != "" {
		conds = append(conds, "te.slug = "+arg(f.Edition))
	}
	if f.DayFrom != nil && f.DayTo != nil {
		conds = append(conds, "m.scheduled_at >= "+arg(*f.DayFrom))
		conds = append(conds, "m.scheduled_at < "+arg(*f.DayTo))
	}

	q := matchSelect
	if f.SortByRank {
		q += `
	left join lateral (
		select min(r2.rank) as best_rank
		from match_participants mp3
		join v_current_rankings r2 on r2.player_id = mp3.player_id and r2.tour_code = 'atp'
		where mp3.match_id = m.id
	) br on true`
	}
	if len(conds) > 0 {
		q += "\n\twhere " + strings.Join(conds, " and ")
	}
	if f.SortByRank {
		q += "\n\torder by br.best_rank nulls last, m.scheduled_at nulls last"
	} else {
		q += "\n\torder by m.scheduled_at nulls last, m.id"
	}
	q += "\n\tlimit " + arg(f.Limit) + " offset " + arg(f.Offset)

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMatch)
}

func GetMatch(ctx context.Context, pool *pgxpool.Pool, id int64) (*Match, error) {
	rows, err := pool.Query(ctx, matchSelect+"\n\twhere m.id = $1", id)
	if err != nil {
		return nil, err
	}
	m, err := pgx.CollectExactlyOneRow(rows, scanMatch)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func scanMatch(row pgx.CollectableRow) (Match, error) {
	var (
		m          Match
		liveState  string
		partsJSON  string
		setsJSON   string
	)
	if err := row.Scan(&m.ID, &m.Edition, &m.TournamentName, &m.Round,
		&m.ScheduledAt, &m.Court, &m.Status, &m.Surface,
		&m.WinnerSide, &m.Outcome, &liveState, &partsJSON, &setsJSON); err != nil {
		return m, err
	}

	var flat []flatParticipant
	if err := json.Unmarshal([]byte(partsJSON), &flat); err != nil {
		return m, fmt.Errorf("unmarshal participants: %w", err)
	}
	bySide := map[int][]MatchPlayer{}
	for _, fp := range flat {
		bySide[fp.Side] = append(bySide[fp.Side], MatchPlayer{
			Slug: fp.Slug, Name: fp.Name, LastName: fp.LastName, PhotoURL: fp.PhotoURL, Rank: fp.Rank,
		})
	}
	m.Sides = []MatchSide{}
	for side := 1; side <= 2; side++ {
		if players, ok := bySide[side]; ok {
			m.Sides = append(m.Sides, MatchSide{Side: side, Players: players})
		}
	}

	if err := json.Unmarshal([]byte(setsJSON), &m.Sets); err != nil {
		return m, fmt.Errorf("unmarshal sets: %w", err)
	}
	if text := scoreText(m.Sets); text != "" {
		m.ScoreText = &text
	}

	if m.Status == "live" {
		m.Live = json.RawMessage(liveState)
	} else {
		m.Live = json.RawMessage("null")
	}
	return m, nil
}

// scoreText собирает "6-4, 7-6(5)" из сетов (с точки зрения стороны 1).
func scoreText(sets [][]*int) string {
	parts := make([]string, 0, len(sets))
	for _, s := range sets {
		if len(s) < 2 || s[0] == nil || s[1] == nil {
			continue
		}
		part := fmt.Sprintf("%d-%d", *s[0], *s[1])
		if len(s) > 2 && s[2] != nil {
			part += fmt.Sprintf("(%d)", *s[2])
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}
