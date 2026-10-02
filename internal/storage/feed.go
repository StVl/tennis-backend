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
	YourSeason         []SeasonCard        `json:"your_season"`
	AllPlayers         []GridPlayer        `json:"all_players"`
	WeeklyHighlights   []Match             `json:"weekly_highlights"`
	FeaturedTournament *FeaturedTournament `json:"featured_tournament"`
	// Местная дата пользователя (?tz=), относительно которой решены «сегодня»/«вчера».
	Today string `json:"today"`
	// Статус в заголовке (§4.1 спецификации главной).
	HeaderStatus HeaderStatus `json:"header_status"`
	// Идущие турниры в порядке карусели; пусто — блок скрыт.
	Tournaments []HomeTournament `json:"tournaments"`
}

// SeasonCard — большая карточка подписанного игрока.
type SeasonCard struct {
	Player         PlayerListItem    `json:"player"`
	NextMatch      *PlayerMatch      `json:"next_match"`
	NextTournament *PlayerTournament `json:"next_tournament"` // только если нет матча
	// Последний сыгранный матч, если он был вчера или сегодня (по ?tz=) — «Won the title ·
	// def. Hurkacz 6-4 7-6» в Your week. Независим от next_match.
	LastMatch *PlayerMatch `json:"last_match"`
	// Имя и фамилия раздельно: в player только display name.
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// GridPlayer — элемент сетки "All players" / онбординга.
type GridPlayer struct {
	Slug      string  `json:"slug"`
	Name      string  `json:"name"`
	PhotoURL  *string `json:"photo_url"`
	Rank      *int    `json:"rank"`       // null, если нет свежего снапшота в v_current_rankings
	RankDelta *int    `json:"rank_delta"` // к предыдущему снапшоту
	// С начала сезона: ранг в первом снапшоте года минус текущий (+2 — поднялся на два места).
	SeasonRankDelta *int `json:"season_rank_delta"`
	Followed        bool `json:"followed"`
}

// WidgetFeed — готовый таймлайн виджета: клиент только рендерит.
type WidgetFeed struct {
	State       string       `json:"state"` // rows | same_tournament | split | no_follows | no_matches
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

// liveStaleGrace — насколько давно прошедший матч ещё может считаться
// «ближайшим». Шире, чем окно наблюдения ingest'а (6 часов), чтобы реально
// задержанный матч не выпадал: в срезе борта источника встречались фикстуры,
// висевшие upcoming через 4 часа 44 минуты после назначенного времени.
const liveStaleGrace = 12 * time.Hour

// nextMatchPerPlayer — ближайший scheduled/live матч каждого игрока из списка.
//
// notBefore отсекает протухшие строки. Без него любой scheduled-матч с датой в
// прошлом навсегда становится «следующим» у игрока: сортировка идёт по
// scheduled_at по возрастанию, то есть чем древнее строка, тем выше она стоит.
// Данные в matches приезжают пакетно и с лагом в недели, а ingest live-статуса
// возвращает матч в прежний статус после окончания — без отсечки такой матч
// оставался бы на главной и в виджете как предстоящий.
//
// На live-строки отсечка НЕ распространяется: живой матч актуален по
// определению, каким бы старым ни было его scheduled_at. Иначе главная и виджет
// начинают расходиться с /v1/matches?status=live и /v1/users/me/live-matches —
// а весь смысл фичи в том, что карточка и экран читают одну и ту же колонку и
// разойтись не могут. Случаи выше 12 часов реальны: возобновление после дождя
// на следующий день и фикстура, у которой известна только дата (00:00Z), а на
// корт она выходит вечером — ровно то, что создаёт писатель фикстур из Phase 8.
func nextMatchPerPlayer(ctx context.Context, pool *pgxpool.Pool, slugs []string,
	notBefore time.Time) (map[string]PlayerMatch, error) {
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
		  and (vpm.status = 'live' or vpm.scheduled_at is null or vpm.scheduled_at >= $2)
		order by pl.slug, vpm.scheduled_at asc nulls last`,
		slugs, notBefore)
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
		  and e.status is distinct from 'withdrawn'
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

// weeklyHighlightsLimit — сколько карточек отдаём (§7 спецификации главной).
const weeklyHighlightsLimit = 10

// weeklyHighlights — завершённые матчи подписок за последние `days` дней.
//
// Порядок — правило экрана, поэтому оно здесь: сначала важные раунды (`rounds.sort_order`, тот же
// столбец, по которому строится сетка), внутри раунда — свежие. Клиенту остаётся отрисовать.
//
// Матч двух подписок попадает в выдачу один раз: форма нейтральная, а не «глазами игрока».
func weeklyHighlights(ctx context.Context, pool *pgxpool.Pool, followed []string, since time.Time) ([]Match, error) {
	rows, err := pool.Query(ctx, matchSelect+`
		left join rounds ro on ro.code = m.round_code
		where m.status::text = 'completed'
		  and m.scheduled_at >= $2
		  and exists (select 1 from match_participants mp2
		              join players p2 on p2.id = mp2.player_id
		              where mp2.match_id = m.id and p2.slug = any($1))
		order by ro.sort_order desc nulls last, m.scheduled_at desc nulls last, m.id desc
		limit $3`,
		followed, since, weeklyHighlightsLimit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMatch)
}

// liveTournamentHighlights — добивка итогов недели: последние раунды идущих турниров, кроме
// уже взятых матчей подписок. Без неё у пользователя без подписок (или с тихой неделей)
// секция пуста, а по спецификации она есть всегда.
func liveTournamentHighlights(ctx context.Context, pool *pgxpool.Pool, editions []string,
	since time.Time, exclude []int64, limit int) ([]Match, error) {
	if limit <= 0 || len(editions) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, matchSelect+`
		left join rounds ro on ro.code = m.round_code
		where m.status::text = 'completed'
		  and te.slug = any($1)
		  and m.scheduled_at >= $2
		  and m.round_code !~* '^q'
		  and m.id <> all($3)
		order by ro.sort_order desc nulls last, m.scheduled_at desc nulls last, m.id desc
		limit $4`,
		editions, since, exclude, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanMatch)
}

// lastMatchPerPlayer — последний завершённый матч каждого игрока не раньше since.
func lastMatchPerPlayer(ctx context.Context, pool *pgxpool.Pool, slugs []string,
	since time.Time) (map[string]PlayerMatch, error) {
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
		where pl.slug = any($1) and vpm.status = 'completed'
		  and vpm.scheduled_at >= $2
		order by pl.slug, vpm.scheduled_at desc`,
		slugs, since)
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

// playerNames — имя и фамилия раздельно (у list-форм есть только display name).
func playerNames(ctx context.Context, pool *pgxpool.Pool, slugs []string) (map[string][2]string, error) {
	rows, err := pool.Query(ctx,
		`select slug, coalesce(first_name, ''), coalesce(last_name, ''), display_name
		 from players where slug = any($1)`, slugs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]string{}
	for rows.Next() {
		var slug, first, last, display string
		if err := rows.Scan(&slug, &first, &last, &display); err != nil {
			return nil, err
		}
		if last == "" {
			last = lastWord(display)
		}
		out[slug] = [2]string{first, last}
	}
	return out, rows.Err()
}

// headerFactsFor — сегодняшние и вчерашние матчи подписок для статуса в заголовке.
// Ближайший и последний матч уже загружены лентой; отдельного запроса не нужно: подписка,
// игравшая сегодня, видна либо как next_match сегодня, либо как last_match сегодня.
func headerFactsFor(cards []SeasonCard, day homeDay, cityByEdition map[string]string) headerFacts {
	var f headerFacts
	in := func(t *time.Time, from, to time.Time) bool {
		return t != nil && !t.Before(from) && t.Before(to)
	}
	for _, c := range cards {
		p := HeaderPlayer{Slug: c.Player.Slug, FirstName: c.FirstName, LastName: c.LastName}
		add := func(m *PlayerMatch, today bool) {
			hm := headerMatch{Player: p, Round: m.Round, At: m.ScheduledAt, Status: m.Status,
				Won: m.Result != nil && *m.Result == "won", City: cityByEdition[m.Edition]}
			if today {
				f.Today = append(f.Today, hm)
			} else {
				f.Yesterday = append(f.Yesterday, hm)
			}
		}
		if m := c.NextMatch; m != nil && (m.Status == "live" || in(m.ScheduledAt, day.todayFrom, day.to)) {
			add(m, true)
		}
		if m := c.LastMatch; m != nil {
			switch {
			case in(m.ScheduledAt, day.todayFrom, day.to):
				add(m, true)
			case in(m.ScheduledAt, day.yesterdayFrom, day.todayFrom):
				add(m, false)
			}
		}
	}
	return f
}

// GetHomeFeed собирает главный экран: карточки подписок + полная сетка ростера + итоги недели.
//
// `highlightDays` — ширина окна итогов; 0 выключает блок. Параметр, а не константа, потому что
// «последние 7 дней» — правильная семантика, но пустая на снапшоте базы, который отстал: с
// `?highlights_days=` секцию видно в разработке, не подменяя смысл в проде.
//
// loc — часовой пояс пользователя (?tz=): от него зависят «сегодня»/«вчера» в заголовке,
// last_match и день турнира. now — параметром ради тестов.
func GetHomeFeed(ctx context.Context, pool *pgxpool.Pool, lang string, followed []string,
	highlightDays int, loc *time.Location, now time.Time) (*HomeFeed, error) {
	roster, err := ListPlayers(ctx, pool, lang, true, "")
	if err != nil {
		return nil, err
	}

	followedSet := map[string]bool{}
	for _, s := range followed {
		followedSet[s] = true
	}

	day := newHomeDay(now, loc)
	feed := &HomeFeed{
		YourSeason:         []SeasonCard{},
		AllPlayers:         make([]GridPlayer, 0, len(roster)),
		WeeklyHighlights:   []Match{},
		FeaturedTournament: nil,
		Today:              day.today.Format("2006-01-02"),
		Tournaments:        []HomeTournament{},
	}
	rosterBySlug := map[string]PlayerListItem{}
	for _, p := range roster {
		rosterBySlug[p.Slug] = p
		feed.AllPlayers = append(feed.AllPlayers, GridPlayer{
			Slug: p.Slug, Name: p.Name, PhotoURL: p.PhotoURL,
			Rank: p.Rank, RankDelta: p.RankDelta, SeasonRankDelta: p.SeasonRankDelta,
			Followed: followedSet[p.Slug],
		})
	}

	featured, err := loadFeaturedTournament(ctx, pool, lang, now)
	if err != nil {
		return nil, err
	}
	feed.FeaturedTournament = featured

	tournaments, tournamentFacts, err := loadHomeTournaments(ctx, pool, followed, day)
	if err != nil {
		return nil, err
	}
	feed.Tournaments = tournaments
	cityByEdition := map[string]string{}
	liveEditions := make([]string, 0, len(tournaments))
	for _, t := range tournaments {
		cityByEdition[t.Edition] = deref(t.City)
		liveEditions = append(liveEditions, t.Edition)
	}

	if len(followed) > 0 {
		if err := fillSeason(ctx, pool, feed, followed, rosterBySlug, day); err != nil {
			return nil, err
		}
	}
	facts := headerFactsFor(feed.YourSeason, day, cityByEdition)
	facts.Live = tournamentFacts
	feed.HeaderStatus = pickHeaderStatus(facts)

	if highlightDays > 0 {
		since := now.AddDate(0, 0, -highlightDays)
		var taken []int64
		if len(followed) > 0 {
			highlights, err := weeklyHighlights(ctx, pool, followed, since)
			if err != nil {
				return nil, err
			}
			feed.WeeklyHighlights = append(feed.WeeklyHighlights, highlights...)
			for _, m := range highlights {
				taken = append(taken, m.ID)
			}
		}
		if taken == nil {
			taken = []int64{}
		}
		more, err := liveTournamentHighlights(ctx, pool, liveEditions, since, taken,
			weeklyHighlightsLimit-len(feed.WeeklyHighlights))
		if err != nil {
			return nil, err
		}
		feed.WeeklyHighlights = append(feed.WeeklyHighlights, more...)
	}
	return feed, nil
}

// fillSeason — карточки подписок: ближайший матч (иначе турнир), последний матч за вчера и
// сегодня, имя и фамилия.
func fillSeason(ctx context.Context, pool *pgxpool.Pool, feed *HomeFeed, followed []string,
	rosterBySlug map[string]PlayerListItem, day homeDay) error {
	nextMatches, err := nextMatchPerPlayer(ctx, pool, followed, day.now.Add(-liveStaleGrace))
	if err != nil {
		return err
	}
	nextTournaments, err := nextTournamentPerPlayer(ctx, pool, followed)
	if err != nil {
		return err
	}
	lastMatches, err := lastMatchPerPlayer(ctx, pool, followed, day.yesterdayFrom)
	if err != nil {
		return err
	}
	names, err := playerNames(ctx, pool, followed)
	if err != nil {
		return err
	}

	// порядок карточек = порядок в player_ids; неизвестные слаги молча пропускаем
	for _, slug := range followed {
		p, ok := rosterBySlug[slug]
		if !ok {
			continue
		}
		card := SeasonCard{Player: p, FirstName: names[slug][0], LastName: names[slug][1]}
		if m, ok := nextMatches[slug]; ok {
			card.NextMatch = &m
		} else if t, ok := nextTournaments[slug]; ok {
			card.NextTournament = &t
		}
		if m, ok := lastMatches[slug]; ok {
			card.LastMatch = &m
		}
		feed.YourSeason = append(feed.YourSeason, card)
	}
	return nil
}

// GetWidgetFeed вычисляет все четыре состояния виджета (логика из README виджета).
func GetWidgetFeed(ctx context.Context, pool *pgxpool.Pool, followed []string, loc *time.Location, now time.Time) (*WidgetFeed, error) {
	if len(followed) == 0 {
		return &WidgetFeed{State: "no_follows", Rows: []WidgetRow{}}, nil
	}

	localNow := now.In(loc)
	dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	dayFrom, dayTo := dayStart.UTC(), dayStart.AddDate(0, 0, 1).UTC()

	nextMatches, err := nextMatchPerPlayer(ctx, pool, followed, now.Add(-liveStaleGrace))
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
		// Слаг розыгрыша — ключ склейки для state=same_tournament. Наружу не отдаётся:
		// решение принимает сервер, клиент только рисует.
		edition string
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
			slots = append(slots, slot{when: when, edition: m.Edition, row: WidgetRow{
				Type: "match", Player: hdr, Opponent: m.Opponent,
				TournamentName: m.TournamentName, Surface: m.Surface,
				StartAt: m.ScheduledAt, IsToday: isToday,
				Round: m.Round, IsLive: m.Status == "live",
			}})
		} else if t, ok := nextTournaments[slug]; ok {
			start, end := t.StartDate, t.EndDate
			slots = append(slots, slot{when: start, edition: t.Edition, row: WidgetRow{
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

	// split проверяется раньше склейки: он про «сегодня», а склейка — про афишу, и матчи одного
	// розыгрыша в один день делают followedPlaysToday истинным, так что пересечься они могут только
	// на будущем дне, где полезнее колонка TODAY.
	if !followedPlaysToday && len(todayColumn) > 0 {
		return &WidgetFeed{State: "split", Rows: rows, TodayColumn: todayColumn}, nil
	}

	// same_tournament: 2–3 подписки в одном розыгрыше — виджет рисует афишу турнира вместо списка.
	// Матчам нужен ещё и общий локальный день: два матча одного турнира в разные дни — это список.
	if len(slots) >= 2 && slots[0].edition != "" {
		same, day := true, ""
		for _, s := range slots {
			if s.edition != slots[0].edition || s.row.Type != slots[0].row.Type {
				same = false
				break
			}
			if s.row.Type != "match" {
				continue
			}
			if s.row.StartAt == nil {
				same = false
				break
			}
			d := s.row.StartAt.In(loc).Format("2006-01-02")
			if day == "" {
				day = d
			} else if d != day {
				same = false
				break
			}
		}
		if same {
			return &WidgetFeed{State: "same_tournament", Rows: rows}, nil
		}
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
