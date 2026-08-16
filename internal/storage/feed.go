package storage

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HomeFeed — весь главный экран одним ответом (замена config.json + playerCards).
type HomeFeed struct {
	YourSeason []SeasonCard `json:"your_season"`
	AllPlayers []GridPlayer `json:"all_players"`
}

// SeasonCard — большая карточка подписанного игрока.
type SeasonCard struct {
	Player         PlayerListItem    `json:"player"`
	NextMatch      *PlayerMatch      `json:"next_match"`
	NextTournament *PlayerTournament `json:"next_tournament"` // только если нет матча
}

// GridPlayer — элемент сетки "All players" / онбординга.
type GridPlayer struct {
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	PhotoURL  *string `json:"photo_url"`
	Rank      *int    `json:"rank"`       // null, если нет свежего снапшота в v_current_rankings
	RankDelta *int    `json:"rank_delta"` // к предыдущему снапшоту
	Followed  bool    `json:"followed"`
}

// WidgetFeed — готовый таймлайн виджета: клиент только рендерит.
type WidgetFeed struct {
	State       string       `json:"state"` // rows | split | no_follows | no_matches
	Rows        []WidgetRow  `json:"rows"`
	TodayColumn []TodayMatch `json:"today_column,omitempty"` // только для state=split
}

type WidgetRow struct {
	Type           string     `json:"type"` // match | tournament
	Player         Opponent   `json:"player"`
	Opponent       *Opponent  `json:"opponent,omitempty"` // null = TBD; только для match
	TournamentName string     `json:"tournament_name"`
	Surface        string     `json:"surface"`
	StartAt        *time.Time `json:"start_at,omitempty"`   // match
	StartDate      *time.Time `json:"start_date,omitempty"` // tournament
	EndDate        *time.Time `json:"end_date,omitempty"`   // tournament
	IsToday        bool       `json:"is_today"`
	// Код раунда (R128…F) — только для match. Виджет клеит его к покрытию: «QF · Grunt».
	Round string `json:"round,omitempty"`
	// Матч идёт прямо сейчас (status = live). Не omitempty: клиент ждёт ключ всегда,
	// а у строки-турнира честное false.
	IsLive bool `json:"is_live"`
	// Город турнира — только для tournament. Null, если у бренда не заполнен.
	Location *string `json:"location,omitempty"`
}

// TodayMatch — карточка в колонке TODAY: только фамилии.
type TodayMatch struct {
	P1LastName string     `json:"p1_last_name"`
	P2LastName string     `json:"p2_last_name"`
	StartAt    *time.Time `json:"start_at"`
}

// nextMatchPerPlayer — ближайший scheduled/live матч каждого игрока из списка.
func nextMatchPerPlayer(ctx context.Context, pool *pgxpool.Pool, slugs []string) (map[string]PlayerMatch, error) {
	rows, err := pool.Query(ctx, `
		select distinct on (pl.slug)
		       pl.slug,
		       vpm.match_id, vpm.round_code, vpm.scheduled_at, vpm.status::text,
		       vpm.outcome::text, vpm.result, vpm.score_text,
		       op.slug, op.display_name, op.photo_url,
		       te.slug, t.name, te.surface::text
		from v_player_matches vpm
		join players pl on pl.id = vpm.player_id
		left join players op on op.id = vpm.opponent_id
		join tournament_editions te on te.id = vpm.edition_id
		join tournaments t on t.id = te.tournament_id
		where pl.slug = any($1) and vpm.status in ('scheduled', 'live')
		order by pl.slug, vpm.scheduled_at asc nulls last`,
		slugs)
	if err != nil {
		return nil, err
	}
	out := map[string]PlayerMatch{}
	for rows.Next() {
		var (
			slug                          string
			m                             PlayerMatch
			oppSlug, oppName, oppPhotoURL *string
		)
		if err := rows.Scan(&slug, &m.MatchID, &m.Round, &m.ScheduledAt, &m.Status,
			&m.Outcome, &m.Result, &m.ScoreText,
			&oppSlug, &oppName, &oppPhotoURL,
			&m.Edition, &m.TournamentName, &m.Surface); err != nil {
			return nil, err
		}
		if oppSlug != nil {
			m.Opponent = &Opponent{Slug: *oppSlug, Name: deref(oppName), PhotoURL: oppPhotoURL}
		}
		out[slug] = m
	}
	return out, rows.Err()
}

// nextTournamentPerPlayer — ближайший upcoming-турнир каждого игрока из списка.
func nextTournamentPerPlayer(ctx context.Context, pool *pgxpool.Pool, slugs []string) (map[string]PlayerTournament, error) {
	rows, err := pool.Query(ctx, `
		select distinct on (pl.slug)
		       pl.slug, v.slug, t.name, v.start_date, v.end_date, v.surface::text, v.status, e.seed,
		       t.location
		from tournament_entries e
		join players pl on pl.id = e.player_id
		join v_tournament_editions v on v.id = e.edition_id
		join tournaments t on t.id = v.tournament_id
		where pl.slug = any($1) and v.status = 'upcoming'
		order by pl.slug, v.start_date asc`,
		slugs)
	if err != nil {
		return nil, err
	}
	out := map[string]PlayerTournament{}
	for rows.Next() {
		var (
			slug string
			t    PlayerTournament
		)
		if err := rows.Scan(&slug, &t.Edition, &t.TournamentName, &t.StartDate, &t.EndDate,
			&t.Surface, &t.Status, &t.Seed, &t.Location); err != nil {
			return nil, err
		}
		out[slug] = t
	}
	return out, rows.Err()
}

// GetHomeFeed собирает главный экран: карточки подписок + полная сетка ростера.
func GetHomeFeed(ctx context.Context, pool *pgxpool.Pool, lang string, followed []string) (*HomeFeed, error) {
	roster, err := ListPlayers(ctx, pool, lang, true, "")
	if err != nil {
		return nil, err
	}

	followedSet := map[string]bool{}
	for _, s := range followed {
		followedSet[s] = true
	}

	feed := &HomeFeed{YourSeason: []SeasonCard{}, AllPlayers: make([]GridPlayer, 0, len(roster))}
	rosterBySlug := map[string]PlayerListItem{}
	for _, p := range roster {
		rosterBySlug[p.Slug] = p
		feed.AllPlayers = append(feed.AllPlayers, GridPlayer{
			Slug: p.Slug, Name: p.Name, PhotoURL: p.PhotoURL,
			Rank: p.Rank, RankDelta: p.RankDelta, Followed: followedSet[p.Slug],
		})
	}

	if len(followed) == 0 {
		return feed, nil
	}
	nextMatches, err := nextMatchPerPlayer(ctx, pool, followed)
	if err != nil {
		return nil, err
	}
	nextTournaments, err := nextTournamentPerPlayer(ctx, pool, followed)
	if err != nil {
		return nil, err
	}

	// порядок карточек = порядок в player_ids; неизвестные слаги молча пропускаем
	for _, slug := range followed {
		p, ok := rosterBySlug[slug]
		if !ok {
			continue
		}
		card := SeasonCard{Player: p}
		if m, ok := nextMatches[slug]; ok {
			card.NextMatch = &m
		} else if t, ok := nextTournaments[slug]; ok {
			card.NextTournament = &t
		}
		feed.YourSeason = append(feed.YourSeason, card)
	}
	return feed, nil
}

// GetWidgetFeed вычисляет все четыре состояния виджета (логика из README виджета).
func GetWidgetFeed(ctx context.Context, pool *pgxpool.Pool, followed []string, loc *time.Location, now time.Time) (*WidgetFeed, error) {
	if len(followed) == 0 {
		return &WidgetFeed{State: "no_follows", Rows: []WidgetRow{}}, nil
	}

	localNow := now.In(loc)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	dayFrom, dayTo := dayStart.UTC(), dayStart.AddDate(0, 0, 1).UTC()

	nextMatches, err := nextMatchPerPlayer(ctx, pool, followed)
	if err != nil {
		return nil, err
	}
	nextTournaments, err := nextTournamentPerPlayer(ctx, pool, followed)
	if err != nil {
		return nil, err
	}
	playerHeaders, err := playerHeadersBySlug(ctx, pool, followed)
	if err != nil {
		return nil, err
	}

	// слот на каждого подписанного: ближайший матч, иначе ближайший турнир
	type slot struct {
		row  WidgetRow
		when time.Time
	}
	var slots []slot
	for _, slug := range followed {
		hdr, ok := playerHeaders[slug]
		if !ok {
			continue
		}
		if m, ok := nextMatches[slug]; ok {
			when := dayTo.AddDate(10, 0, 0) // матчи без времени — в конец
			isToday := false
			if m.ScheduledAt != nil {
				when = *m.ScheduledAt
				isToday = !m.ScheduledAt.Before(dayFrom) && m.ScheduledAt.Before(dayTo)
			}
			slots = append(slots, slot{when: when, row: WidgetRow{
				Type: "match", Player: hdr, Opponent: m.Opponent,
				TournamentName: m.TournamentName, Surface: m.Surface,
				StartAt: m.ScheduledAt, IsToday: isToday,
				Round: m.Round, IsLive: m.Status == "live",
			}})
		} else if t, ok := nextTournaments[slug]; ok {
			start, end := t.StartDate, t.EndDate
			slots = append(slots, slot{when: start, row: WidgetRow{
				Type: "tournament", Player: hdr,
				TournamentName: t.TournamentName, Surface: t.Surface,
				StartDate: &start, EndDate: &end,
				Location: t.Location,
			}})
		}
	}
	if len(slots) == 0 {
		return &WidgetFeed{State: "no_matches", Rows: []WidgetRow{}}, nil
	}

	// сортировка по дате, первые 3 строки
	for i := 1; i < len(slots); i++ {
		for j := i; j > 0 && slots[j].when.Before(slots[j-1].when); j-- {
			slots[j], slots[j-1] = slots[j-1], slots[j]
		}
	}
	if len(slots) > 3 {
		slots = slots[:3]
	}
	rows := make([]WidgetRow, len(slots))
	for i, s := range slots {
		rows[i] = s.row
	}

	// split: никто из подписок не играет сегодня, но сегодня есть чужие матчи
	todayMatches, err := ListMatches(ctx, pool, MatchFilter{
		DayFrom: &dayFrom, DayTo: &dayTo, SortByRank: true, Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	followedSet := map[string]bool{}
	for _, s := range followed {
		followedSet[s] = true
	}
	followedPlaysToday := false
	var todayColumn []TodayMatch
	for _, m := range todayMatches {
		anyFollowed := false
		for _, side := range m.Sides {
			for _, p := range side.Players {
				if followedSet[p.Slug] {
					anyFollowed = true
				}
			}
		}
		if anyFollowed {
			followedPlaysToday = true
			continue
		}
		if len(todayColumn) < 5 {
			todayColumn = append(todayColumn, TodayMatch{
				P1LastName: sideLastName(m.Sides, 1),
				P2LastName: sideLastName(m.Sides, 2),
				StartAt:    m.ScheduledAt,
			})
		}
	}

	if !followedPlaysToday && len(todayColumn) > 0 {
		return &WidgetFeed{State: "split", Rows: rows, TodayColumn: todayColumn}, nil
	}
	return &WidgetFeed{State: "rows", Rows: rows}, nil
}

// playerHeadersBySlug — slug/имя/фото для строк виджета.
func playerHeadersBySlug(ctx context.Context, pool *pgxpool.Pool, slugs []string) (map[string]Opponent, error) {
	rows, err := pool.Query(ctx,
		`select slug, display_name, photo_url from players where slug = any($1)`, slugs)
	if err != nil {
		return nil, err
	}
	out := map[string]Opponent{}
	type row struct{ o Opponent }
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.o.Slug, &x.o.Name, &x.o.PhotoURL)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	for _, x := range items {
		out[x.o.Slug] = x.o
	}
	return out, nil
}

func sideLastName(sides []MatchSide, side int) string {
	for _, s := range sides {
		if s.Side != side || len(s.Players) == 0 {
			continue
		}
		p := s.Players[0]
		if p.LastName != nil && *p.LastName != "" {
			return *p.LastName
		}
		// фолбэк: последнее слово полного имени
		parts := strings.Fields(p.Name)
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return "TBD"
}
