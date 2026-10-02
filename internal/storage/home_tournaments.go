package storage

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Блок турниров главной (спецификация главной, §5): идущие розыгрыши каруселью, у каждого —
// сетка-срез, если в нём есть подписки, иначе обложка. Порядок, раскладку и срез решает
// сервер; клиент только рисует.

// HomeTournament — карточка турнира на главной.
type HomeTournament struct {
	Edition     string  `json:"edition"`
	Name        string  `json:"name"`
	City        *string `json:"city"`
	CountryCode *string `json:"country_code"`
	// ATP 250 | ATP 500 | ATP 1000 | Grand Slam | ATP Finals; null — неизвестно.
	Category  *string   `json:"category"`
	Surface   string    `json:"surface"` // hard | clay | grass
	Indoor    bool      `json:"indoor"`
	StartDate time.Time `json:"start_date"`
	EndDate   time.Time `json:"end_date"`
	Day       int       `json:"day"`
	TotalDays int       `json:"total_days"`
	Monogram  string    `json:"monogram"`
	// bracket | cover
	Layout        string `json:"layout"`
	FollowedCount int    `json:"followed_count"`
	// Ближайший матч турнира — строка статуса обложки «Final today · 13:30».
	NextMatch *HomeNextMatch `json:"next_match"`
	Bracket   *BracketSlice  `json:"bracket"`
}

type HomeNextMatch struct {
	MatchID     int64      `json:"match_id"`
	Round       string     `json:"round"`
	Status      string     `json:"status"`
	ScheduledAt *time.Time `json:"scheduled_at"`
	Court       *string    `json:"court"`
}

// categoryByBrand — уровень турнира, пока импорт сетки не записал его в
// tournament_editions.metadata (там он берётся из очков чемпиона в PDF ATP).
var categoryByBrand = map[string]string{
	"australian_open": "Grand Slam", "roland_garros": "Grand Slam", "wimbledon": "Grand Slam", "us_open": "Grand Slam",
	"atp_finals":   "ATP Finals",
	"indian_wells": "ATP 1000", "miami": "ATP 1000", "monte_carlo": "ATP 1000", "madrid": "ATP 1000",
	"rome": "ATP 1000", "canada": "ATP 1000", "cincinnati": "ATP 1000", "shanghai": "ATP 1000",
	"paris_masters": "ATP 1000",
	"barcelona":     "ATP 500", "basel": "ATP 500", "beijing": "ATP 500", "japan_open": "ATP 500",
	"munich": "ATP 500", "vienna": "ATP 500", "washington": "ATP 500",
	"almaty": "ATP 250", "bastad": "ATP 250", "chengdu": "ATP 250", "eastbourne": "ATP 250",
	"estoril": "ATP 250", "european_open": "ATP 250", "gstaad": "ATP 250", "hangzhou": "ATP 250",
	"kitzbuhel": "ATP 250", "libema": "ATP 250", "los_cabos": "ATP 250", "lyon": "ATP 250",
	"mallorca": "ATP 250", "newport": "ATP 250", "stuttgart": "ATP 250", "umag": "ATP 250",
	"winston_salem": "ATP 250",
}

var categoryWeight = map[string]int{"Grand Slam": 5, "ATP Finals": 4, "ATP 1000": 3, "ATP 500": 2, "ATP 250": 1}

// localDay — номер дня турнира по местной дате пользователя.
func localDay(today, start, end time.Time) (n, total int) {
	d := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	total = int(d(end).Sub(d(start)).Hours()/24) + 1
	n = int(d(today).Sub(d(start)).Hours()/24) + 1
	if n < 1 {
		n = 1
	}
	if n > total {
		n = total
	}
	return n, total
}

// sortHomeTournaments — §5: сначала турниры с подписками (больше подписок — раньше),
// потом по уровню, потом по имени.
func sortHomeTournaments(ts []HomeTournament) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.FollowedCount != b.FollowedCount {
			return a.FollowedCount > b.FollowedCount
		}
		wa, wb := 0, 0
		if a.Category != nil {
			wa = categoryWeight[*a.Category]
		}
		if b.Category != nil {
			wb = categoryWeight[*b.Category]
		}
		if wa != wb {
			return wa > wb
		}
		return a.Name < b.Name
	})
}

// homeDay — границы «сегодня» и «вчера» у пользователя.
type homeDay struct {
	loc                          *time.Location
	now                          time.Time
	today                        time.Time // полночь по местному времени
	yesterdayFrom, todayFrom, to time.Time // UTC-инстанты
}

func newHomeDay(now time.Time, loc *time.Location) homeDay {
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return homeDay{
		loc: loc, now: now, today: today,
		yesterdayFrom: today.AddDate(0, 0, -1).UTC(),
		todayFrom:     today.UTC(),
		to:            today.AddDate(0, 0, 1).UTC(),
	}
}

// loadHomeTournaments — идущие розыгрыши (и вчера закончившиеся — пока сетка нужна
// подпискам) с раскладкой и срезом; плюс факты «день полуфиналов» для заголовка.
func loadHomeTournaments(ctx context.Context, pool *pgxpool.Pool, followed []string,
	day homeDay) ([]HomeTournament, []headerTournament, error) {

	todayDate := day.today.Format("2006-01-02")
	rows, err := pool.Query(ctx, `
		select te.slug, t.slug, t.name,
		       -- у части брендов location пуст; импорт сетки кладёт город из PDF ATP в metadata
		       coalesce(nullif(t.location, ''), te.metadata->>'location'),
		       t.country_code, te.surface::text,
		       coalesce(t.conditions->>'indoor', 'false'), te.start_date, te.end_date,
		       te.metadata->>'category'
		from tournament_editions te
		join tournaments t on t.id = te.tournament_id
		where te.discipline = 'singles'
		  and te.start_date <= $1::date and te.end_date >= $1::date - 1`, todayDate)
	if err != nil {
		return nil, nil, err
	}
	var (
		ts       []HomeTournament
		editions []string
	)
	for rows.Next() {
		var (
			h             HomeTournament
			brand, indoor string
			location      *string
			category      *string
		)
		if err := rows.Scan(&h.Edition, &brand, &h.Name, &location, &h.CountryCode, &h.Surface,
			&indoor, &h.StartDate, &h.EndDate, &category); err != nil {
			rows.Close()
			return nil, nil, err
		}
		h.City = cityFromLocation(location)
		h.Indoor = strings.EqualFold(indoor, "true")
		if s := strings.ToLower(h.Surface); s != "clay" && s != "grass" {
			h.Surface = "hard"
		}
		if category == nil || *category == "" {
			if c, ok := categoryByBrand[brand]; ok {
				category = &c
			} else {
				category = nil
			}
		}
		h.Category = category
		h.Monogram = tournamentMonogram(brand, h.Name)
		h.Day, h.TotalDays = localDay(day.today, h.StartDate, h.EndDate)
		ts = append(ts, h)
		editions = append(editions, h.Edition)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(ts) == 0 {
		return []HomeTournament{}, nil, nil
	}

	trees, err := loadDrawTrees(ctx, pool, editions)
	if err != nil {
		return nil, nil, err
	}
	next, err := nextMatchPerEdition(ctx, pool, editions, day.now.Add(-liveStaleGrace))
	if err != nil {
		return nil, nil, err
	}
	counts, err := followedPerEdition(ctx, pool, editions, followed)
	if err != nil {
		return nil, nil, err
	}
	roundsToday, err := roundsPerEdition(ctx, pool, editions, day.todayFrom, day.to)
	if err != nil {
		return nil, nil, err
	}

	followedSet := map[string]bool{}
	for _, s := range followed {
		followedSet[s] = true
	}
	out := make([]HomeTournament, 0, len(ts))
	for _, h := range ts {
		h.FollowedCount = counts[h.Edition]
		if m, ok := next[h.Edition]; ok {
			nm := m
			h.NextMatch = &nm
		}
		h.Layout = "cover"
		if tree := trees[h.Edition]; tree != nil {
			if slice := BuildBracketSlice(tree, followedSet, day.now, day.loc); slice != nil {
				h.Layout, h.Bracket = "bracket", slice
			}
		}
		// Вчера закончившийся турнир держится только сеткой: обложка законченного не нужна.
		if h.EndDate.Format("2006-01-02") < todayDate && h.Layout != "bracket" {
			continue
		}
		out = append(out, h)
	}
	sortHomeTournaments(out)

	facts := make([]headerTournament, 0, len(out))
	for _, h := range out {
		r := roundsToday[h.Edition]
		facts = append(facts, headerTournament{
			City: deref(h.City), Day: h.Day,
			FinalToday: r["F"], SemisToday: r["SF"], QuartersToday: r["QF"],
		})
	}
	return out, facts, nil
}

// nextMatchPerEdition — live или ближайший матч основной сетки каждого розыгрыша.
func nextMatchPerEdition(ctx context.Context, pool *pgxpool.Pool, editions []string,
	notBefore time.Time) (map[string]HomeNextMatch, error) {
	rows, err := pool.Query(ctx, `
		select distinct on (te.slug) te.slug, m.id, m.round_code, m.status::text, m.scheduled_at, m.court
		from matches m
		join tournament_editions te on te.id = m.edition_id
		where te.slug = any($1) and m.status in ('scheduled', 'live')
		  and m.round_code !~* '^q'
		  and (m.status = 'live' or m.scheduled_at >= $2)
		order by te.slug, case when m.status = 'live' then 0 else 1 end, m.scheduled_at nulls last`,
		editions, notBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]HomeNextMatch{}
	for rows.Next() {
		var (
			slug string
			m    HomeNextMatch
		)
		if err := rows.Scan(&slug, &m.MatchID, &m.Round, &m.Status, &m.ScheduledAt, &m.Court); err != nil {
			return nil, err
		}
		out[slug] = m
	}
	return out, rows.Err()
}

// followedPerEdition — сколько подписок в основной сетке каждого розыгрыша.
func followedPerEdition(ctx context.Context, pool *pgxpool.Pool, editions, followed []string) (map[string]int, error) {
	out := map[string]int{}
	if len(followed) == 0 {
		return out, nil
	}
	rows, err := pool.Query(ctx, `
		select te.slug, count(distinct p.id)
		from matches m
		join tournament_editions te on te.id = m.edition_id
		join match_participants mp on mp.match_id = m.id
		join players p on p.id = mp.player_id
		where te.slug = any($1) and p.slug = any($2)
		  and m.status <> 'cancelled' and m.round_code !~* '^q'
		group by te.slug`, editions, followed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			slug string
			n    int
		)
		if err := rows.Scan(&slug, &n); err != nil {
			return nil, err
		}
		out[slug] = n
	}
	return out, rows.Err()
}

// roundsPerEdition — какие раунды каждого розыгрыша назначены на местное «сегодня».
func roundsPerEdition(ctx context.Context, pool *pgxpool.Pool, editions []string,
	from, to time.Time) (map[string]map[string]bool, error) {
	rows, err := pool.Query(ctx, `
		select distinct te.slug, m.round_code
		from matches m
		join tournament_editions te on te.id = m.edition_id
		where te.slug = any($1) and m.status <> 'cancelled'
		  and m.scheduled_at >= $2 and m.scheduled_at < $3`, editions, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var slug, round string
		if err := rows.Scan(&slug, &round); err != nil {
			return nil, err
		}
		if out[slug] == nil {
			out[slug] = map[string]bool{}
		}
		out[slug][round] = true
	}
	return out, rows.Err()
}
