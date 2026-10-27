package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type featuredRow struct {
	edition, brand, name, surface, status, drawStatus, indoor string
	start, end                                                time.Time
	drawDate                                                  *time.Time
	location, country, logo, lastName, display, category      *string
}

func loadFeaturedTournament(ctx context.Context, pool *pgxpool.Pool, lang string, day homeDay, tour Tour) (*FeaturedTournament, error) {
	// Статус — правило v_tournament_editions, но от местной даты пользователя, а не от
	// current_date базы: карусель турниров в том же ответе отбирает «идущие» по ней же.
	//
	// rank() = 1 отдаёт всех равных лидеров, а не произвольного: идущих розыгрышей в неделю
	// обычно несколько (Вена и Базель), и limit 1 без порядка между ними мог менять карточку
	// от запроса к запросу. Выбор среди равных — chooseFeatured.
	rows, err := pool.Query(ctx, `
		select slug, brand, name, surface, status, start_date, end_date, draw_status, draw_date,
		       location, country_code, logo_url, indoor, last_name, display_name, category
		from (
			select v.slug, t.slug as brand, t.name, v.surface::text as surface, s.status,
			       v.start_date, v.end_date, v.draw_status::text as draw_status, v.draw_date,
			       t.location, t.country_code, t.logo_url,
			       coalesce(t.conditions->>'indoor', 'false') as indoor,
			       ch.last_name, ch.display_name, v.metadata->>'category' as category,
			       rank() over (order by
			         case s.status when 'ongoing' then 0 when 'upcoming' then 1 else 2 end,
			         case when s.status = 'upcoming' then v.start_date end asc nulls last,
			         case when s.status = 'completed' then v.end_date end desc nulls last) as rk
			from v_tournament_editions v
			join tournaments t on t.id = v.tournament_id
			left join players ch on ch.id = v.champion_id
			cross join lateral (select case
			    when $2::date < v.start_date then 'upcoming'
			    when $2::date > v.end_date then 'completed'
			    else 'ongoing' end as status) s
			where v.gender::text = $1 and v.discipline = 'singles'
		) x
		where rk = 1`, tour.Gender(), day.today.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	candidates, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (featuredRow, error) {
		var x featuredRow
		err := r.Scan(&x.edition, &x.brand, &x.name, &x.surface, &x.status, &x.start, &x.end,
			&x.drawStatus, &x.drawDate, &x.location, &x.country, &x.logo, &x.indoor,
			&x.lastName, &x.display, &x.category)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	f := chooseFeatured(candidates, tour)

	roundLabel := ""
	if f.status == "ongoing" {
		roundLabel, err = currentRoundLabel(ctx, pool, lang, f.edition, day.now.Add(-liveStaleGrace))
		if err != nil {
			return nil, err
		}
	}

	championLast := ""
	if f.lastName != nil && strings.TrimSpace(*f.lastName) != "" {
		championLast = strings.TrimSpace(*f.lastName)
	} else if f.display != nil {
		parts := strings.Fields(strings.TrimSpace(*f.display))
		if n := len(parts); n > 0 {
			championLast = parts[n-1]
		}
	}

	in := featuredInput{
		lang:         lang,
		today:        day.today,
		tour:         tour,
		status:       f.status,
		drawStatus:   f.drawStatus,
		start:        f.start,
		end:          f.end,
		drawDate:     f.drawDate,
		roundLabel:   roundLabel,
		tournament:   f.brand,
		name:         f.name,
		surface:      f.surface,
		indoor:       strings.EqualFold(f.indoor, "true"),
		championLast: championLast,
		category:     tournamentCategory(f.brand, tour),
	}
	title, meta, stage := featuredCopy(in)
	surf := strings.ToLower(f.surface)
	if surf != "clay" && surf != "grass" {
		surf = "hard"
	}

	logo := f.logo
	if logo != nil && strings.TrimSpace(*logo) == "" {
		logo = nil
	}

	return &FeaturedTournament{
		Edition:      f.edition,
		SectionTitle: title,
		SectionMeta:  nonEmptyPtr(meta),
		StageLabel:   stage,
		Name:         f.name,
		Monogram:     tournamentMonogram(f.brand, f.name),
		CrestURL:     logo,
		DateRange:    formatDateRange(f.start, f.end),
		City:         cityFromLocation(f.location),
		CountryCode:  f.country,
		Surface:      surf,
		SurfaceLabel: surfaceLabel(lang, f.surface, in.indoor),
	}, nil
}

// chooseFeatured — среди равных по статусу и дате: старший уровень, затем имя, как у карусели
// турниров. Подписки карточка не учитывает, поэтому при них первый турнир карусели может
// быть другим.
func chooseFeatured(rows []featuredRow, tour Tour) featuredRow {
	best, bestCat := rows[0], homeCategory(rows[0].brand, tour, rows[0].category)
	for _, r := range rows[1:] {
		if c := homeCategory(r.brand, tour, r.category); aboveByLevel(c, bestCat, r.name, best.name) {
			best, bestCat = r, c
		}
	}
	return best
}

// currentRoundLabel — самый ранний ещё не сыгранный раунд основной сетки. Отсечка и условие
// «ingest не видел конца» — те же, что у ближайшего матча: без них одна протухшая или
// возвращённая из live строка раннего раунда держала бы подпись на нём весь турнир.
func currentRoundLabel(ctx context.Context, pool *pgxpool.Pool, lang, edition string, notBefore time.Time) (string, error) {
	var label string
	err := pool.QueryRow(ctx, `
		select coalesce(ro.label->>$2, ro.label->>'en', m.round_code)
		from matches m
		join tournament_editions te on te.id = m.edition_id
		join rounds ro on ro.code = m.round_code
		where te.slug = $1 and m.status in ('live', 'scheduled')
		  and m.round_code !~* '^q[0-9]'
		  and (m.status = 'live' or m.scheduled_at is null or m.scheduled_at >= $3)
		  and (m.status = 'live' or `+notFinishedByIngest("m.id", "m.scheduled_at")+`)
		order by case when m.status = 'live' then 0 else 1 end, ro.sort_order, m.id
		limit 1`, edition, lang, notBefore).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return label, err
}
