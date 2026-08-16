package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PlayerListItem — карточка игрока для списков (онбординг, сетка "All players").
type PlayerListItem struct {
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	PhotoURL  *string `json:"photo_url"`
	IsTracked bool    `json:"is_tracked"`
	Rank      *int    `json:"rank"`
	RankDelta *int    `json:"rank_delta"`
	PlayStyle *string `json:"play_style"`
}

// PlayerDetail — полный профиль для карточки игрока.
type PlayerDetail struct {
	Slug       string          `json:"slug"`
	Name       string          `json:"name"`
	FirstName  *string         `json:"first_name"`
	LastName   *string         `json:"last_name"`
	PhotoURL   *string         `json:"photo_url"`
	BirthDate  *time.Time      `json:"birth_date"`
	Hand       *string         `json:"hand"`
	HeightCm   *int            `json:"height_cm"`
	Country    *string         `json:"country"`
	IsTracked  bool            `json:"is_tracked"`
	Traits     json.RawMessage `json:"traits"`
	ProTip     *string         `json:"pro_tip"`
	Links      json.RawMessage `json:"links"`
	PlayStyle  *PlayStyleInfo  `json:"play_style"`
	Rank       *int            `json:"rank"`
	RankDelta  *int            `json:"rank_delta"`
	Points     *int            `json:"points"`
	RacePoints *int            `json:"race_points"`
}

type PlayStyleInfo struct {
	Slug        string          `json:"slug"`
	Name        string          `json:"name"`
	Description json.RawMessage `json:"description"`
}

// PlayerMatch — матч «глазами игрока» (v_player_matches).
type PlayerMatch struct {
	MatchID        int64      `json:"match_id"`
	Round          string     `json:"round"`
	ScheduledAt    *time.Time `json:"scheduled_at"`
	Status         string     `json:"status"`
	Outcome        *string    `json:"outcome"`
	Result         *string    `json:"result"`
	ScoreText      *string    `json:"score_text"`
	Opponent       *Opponent  `json:"opponent"` // null = TBD
	Edition        string     `json:"edition"`
	TournamentName string     `json:"tournament_name"`
	Surface        string     `json:"surface"`
}

type Opponent struct {
	Slug     string  `json:"slug"`
	Name     string  `json:"name"`
	PhotoURL *string `json:"photo_url"`
}

// PlayerTournament — заявка игрока на розыгрыш.
type PlayerTournament struct {
	Edition        string    `json:"edition"`
	TournamentName string    `json:"tournament_name"`
	StartDate      time.Time `json:"start_date"`
	EndDate        time.Time `json:"end_date"`
	Surface        string    `json:"surface"`
	Status         string    `json:"status"`
	Seed           *int      `json:"seed"`
	// Город бренда турнира (tournaments.location), не розыгрыша — тот же источник,
	// что и `location` в /v1/tournaments. Null, если не заполнен.
	Location *string `json:"location"`
}

// RankingSnapshot — точка истории рейтинга.
type RankingSnapshot struct {
	Date       time.Time `json:"date"`
	Rank       int       `json:"rank"`
	Points     *int      `json:"points"`
	RacePoints *int      `json:"race_points"`
}

// RankingRow — строка текущего рейтинга.
type RankingRow struct {
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	PhotoURL  *string `json:"photo_url"`
	Rank      int     `json:"rank"`
	Points    *int    `json:"points"`
	RacePts   *int    `json:"race_points"`
	RankDelta *int    `json:"rank_delta"`
}

// H2H — личные встречи двух игроков (глазами первого).
type H2H struct {
	Wins    int           `json:"wins"`
	Losses  int           `json:"losses"`
	Matches []PlayerMatch `json:"matches"`
}

func ListPlayers(ctx context.Context, pool *pgxpool.Pool, lang string, trackedOnly bool, search string) ([]PlayerListItem, error) {
	rows, err := pool.Query(ctx, `
		select p.slug, p.display_name, p.photo_url, p.is_tracked,
		       r.rank, r.delta_vs_prev,
		       coalesce(ps.name->>$1, ps.name->>'en')
		from players p
		left join v_current_rankings r on r.player_id = p.id and r.tour_code = 'atp'
		left join play_styles ps on ps.id = p.play_style_id
		where (not $2 or p.is_tracked)
		  and ($3 = '' or p.display_name ilike '%' || $3 || '%')
		order by coalesce(r.rank, 100000), p.display_name`,
		lang, trackedOnly, search)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlayerListItem, error) {
		var p PlayerListItem
		err := row.Scan(&p.Slug, &p.Name, &p.PhotoURL, &p.IsTracked, &p.Rank, &p.RankDelta, &p.PlayStyle)
		return p, err
	})
}

func GetPlayer(ctx context.Context, pool *pgxpool.Pool, lang, slug string) (*PlayerDetail, error) {
	var (
		p         PlayerDetail
		traits    *string
		links     *string
		styleSlug *string
		styleName *string
		styleDesc *string
	)
	err := pool.QueryRow(ctx, `
		select p.slug, p.display_name, p.first_name, p.last_name, p.photo_url,
		       p.birth_date, p.hand::text, p.height_cm, p.country_code, p.is_tracked,
		       coalesce(p.traits->$2, p.traits->'en', p.traits->'ru')::text,
		       coalesce(p.pro_tip->>$2, p.pro_tip->>'en', p.pro_tip->>'ru'),
		       p.links::text,
		       ps.slug, coalesce(ps.name->>$2, ps.name->>'en'),
		       coalesce(ps.description->$2, ps.description->'en', ps.description->'ru')::text,
		       r.rank, r.delta_vs_prev, r.points, r.race_points
		from players p
		left join play_styles ps on ps.id = p.play_style_id
		left join v_current_rankings r on r.player_id = p.id and r.tour_code = 'atp'
		where p.slug = $1`,
		slug, lang).Scan(
		&p.Slug, &p.Name, &p.FirstName, &p.LastName, &p.PhotoURL,
		&p.BirthDate, &p.Hand, &p.HeightCm, &p.Country, &p.IsTracked,
		&traits, &p.ProTip, &links,
		&styleSlug, &styleName, &styleDesc,
		&p.Rank, &p.RankDelta, &p.Points, &p.RacePoints)
	if err != nil {
		return nil, err
	}
	p.Traits = rawOrNull(traits)
	p.Links = rawOrNull(links)
	if styleSlug != nil {
		p.PlayStyle = &PlayStyleInfo{
			Slug:        *styleSlug,
			Name:        deref(styleName),
			Description: rawOrNull(styleDesc),
		}
	}
	return &p, nil
}

func ListPlayerMatches(ctx context.Context, pool *pgxpool.Pool, slug string, statuses []string, newestFirst bool, limit, offset int) ([]PlayerMatch, error) {
	order := "vpm.scheduled_at asc nulls last"
	if newestFirst {
		order = "vpm.scheduled_at desc nulls last"
	}
	rows, err := pool.Query(ctx, `
		select vpm.match_id, vpm.round_code, vpm.scheduled_at, vpm.status::text,
		       vpm.outcome::text, vpm.result, vpm.score_text,
		       op.slug, op.display_name, op.photo_url,
		       te.slug, t.name, te.surface::text
		from v_player_matches vpm
		left join players op on op.id = vpm.opponent_id
		join tournament_editions te on te.id = vpm.edition_id
		join tournaments t on t.id = te.tournament_id
		where vpm.player_id = (select id from players where slug = $1)
		  and vpm.status::text = any($2)
		order by `+order+`
		limit $3 offset $4`,
		slug, statuses, limit, offset)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanPlayerMatch)
}

func scanPlayerMatch(row pgx.CollectableRow) (PlayerMatch, error) {
	var (
		m                             PlayerMatch
		oppSlug, oppName, oppPhotoURL *string
	)
	err := row.Scan(&m.MatchID, &m.Round, &m.ScheduledAt, &m.Status,
		&m.Outcome, &m.Result, &m.ScoreText,
		&oppSlug, &oppName, &oppPhotoURL,
		&m.Edition, &m.TournamentName, &m.Surface)
	if err != nil {
		return m, err
	}
	if oppSlug != nil {
		m.Opponent = &Opponent{Slug: *oppSlug, Name: deref(oppName), PhotoURL: oppPhotoURL}
	}
	return m, nil
}

func ListPlayerTournaments(ctx context.Context, pool *pgxpool.Pool, slug string, statuses []string) ([]PlayerTournament, error) {
	rows, err := pool.Query(ctx, `
		select v.slug, t.name, v.start_date, v.end_date, v.surface::text, v.status, e.seed, t.location
		from tournament_entries e
		join v_tournament_editions v on v.id = e.edition_id
		join tournaments t on t.id = v.tournament_id
		where e.player_id = (select id from players where slug = $1)
		  and v.status = any($2)
		order by v.start_date`,
		slug, statuses)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlayerTournament, error) {
		var t PlayerTournament
		err := row.Scan(&t.Edition, &t.TournamentName, &t.StartDate, &t.EndDate, &t.Surface, &t.Status, &t.Seed, &t.Location)
		return t, err
	})
}

func PlayerRankingHistory(ctx context.Context, pool *pgxpool.Pool, slug string) ([]RankingSnapshot, error) {
	rows, err := pool.Query(ctx, `
		select rs.snapshot_date, rs.rank, rs.points, rs.race_points
		from ranking_snapshots rs
		where rs.player_id = (select id from players where slug = $1)
		  and rs.tour_code = 'atp'
		order by rs.snapshot_date`,
		slug)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RankingSnapshot, error) {
		var s RankingSnapshot
		err := row.Scan(&s.Date, &s.Rank, &s.Points, &s.RacePoints)
		return s, err
	})
}

func ListRankings(ctx context.Context, pool *pgxpool.Pool, tour string, limit int) ([]RankingRow, error) {
	rows, err := pool.Query(ctx, `
		select p.slug, p.display_name, p.photo_url,
		       r.rank, r.points, r.race_points, r.delta_vs_prev
		from v_current_rankings r
		join players p on p.id = r.player_id
		where r.tour_code = $1
		order by r.rank
		limit $2`,
		tour, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RankingRow, error) {
		var r RankingRow
		err := row.Scan(&r.Slug, &r.Name, &r.PhotoURL, &r.Rank, &r.Points, &r.RacePts, &r.RankDelta)
		return r, err
	})
}

func ListPlayStyles(ctx context.Context, pool *pgxpool.Pool, lang string) ([]PlayStyleInfo, error) {
	rows, err := pool.Query(ctx, `
		select slug, coalesce(name->>$1, name->>'en'),
		       coalesce(description->$1, description->'en', description->'ru')::text
		from play_styles order by slug`,
		lang)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PlayStyleInfo, error) {
		var s PlayStyleInfo
		var desc *string
		err := row.Scan(&s.Slug, &s.Name, &desc)
		s.Description = rawOrNull(desc)
		return s, err
	})
}

// HeadToHead — личные встречи глазами игрока a.
func HeadToHead(ctx context.Context, pool *pgxpool.Pool, a, b string) (*H2H, error) {
	rows, err := pool.Query(ctx, `
		select vpm.match_id, vpm.round_code, vpm.scheduled_at, vpm.status::text,
		       vpm.outcome::text, vpm.result, vpm.score_text,
		       op.slug, op.display_name, op.photo_url,
		       te.slug, t.name, te.surface::text
		from v_player_matches vpm
		join players op on op.id = vpm.opponent_id and op.slug = $2
		join tournament_editions te on te.id = vpm.edition_id
		join tournaments t on t.id = te.tournament_id
		where vpm.player_id = (select id from players where slug = $1)
		  and vpm.status = 'completed'
		order by vpm.scheduled_at desc nulls last`,
		a, b)
	if err != nil {
		return nil, err
	}
	matches, err := pgx.CollectRows(rows, scanPlayerMatch)
	if err != nil {
		return nil, err
	}
	h := &H2H{Matches: matches}
	for _, m := range matches {
		switch deref(m.Result) {
		case "won":
			h.Wins++
		case "lost":
			h.Losses++
		}
	}
	return h, nil
}

func rawOrNull(s *string) json.RawMessage {
	if s == nil {
		return json.RawMessage("null")
	}
	return json.RawMessage(*s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
