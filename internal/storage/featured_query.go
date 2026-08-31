package storage

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func loadFeaturedTournament(ctx context.Context, pool *pgxpool.Pool, lang string, now time.Time) (*FeaturedTournament, error) {
	row := pool.QueryRow(ctx, `
		select v.slug, t.slug, t.name, v.surface::text, v.status, v.start_date, v.end_date,
		       v.draw_status::text, v.draw_date, t.location, t.country_code, t.logo_url,
		       coalesce(t.conditions->>'indoor', 'false'),
		       ch.last_name, ch.display_name
		from v_tournament_editions v
		join tournaments t on t.id = v.tournament_id
		left join players ch on ch.id = v.champion_id
		order by
		  case v.status when 'ongoing' then 0 when 'upcoming' then 1 else 2 end,
		  case when v.status = 'upcoming' then v.start_date end asc nulls last,
		  case when v.status = 'completed' then v.end_date end desc nulls last
		limit 1`)

	var (
		edition, brand, name, surface, status, drawStatus, indoor string
		start, end                                                time.Time
		drawDate                                                  *time.Time
		location, country, logo, lastName, display                *string
	)
	err := row.Scan(&edition, &brand, &name, &surface, &status, &start, &end,
		&drawStatus, &drawDate, &location, &country, &logo, &indoor, &lastName, &display)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	roundLabel := ""
	if status == "ongoing" {
		roundLabel, err = currentRoundLabel(ctx, pool, lang, edition)
		if err != nil {
			return nil, err
		}
	}

	championLast := ""
	if lastName != nil && strings.TrimSpace(*lastName) != "" {
		championLast = strings.TrimSpace(*lastName)
	} else if display != nil {
		parts := strings.Fields(strings.TrimSpace(*display))
		if n := len(parts); n > 0 {
			championLast = parts[n-1]
		}
	}

	in := featuredInput{
		lang:         lang,
		now:          now,
		status:       status,
		drawStatus:   drawStatus,
		start:        start,
		end:          end,
		drawDate:     drawDate,
		roundLabel:   roundLabel,
		tournament:   brand,
		name:         name,
		surface:      surface,
		indoor:       strings.EqualFold(indoor, "true"),
		championLast: championLast,
		category:     tournamentCategory(brand),
	}
	title, meta, stage := featuredCopy(in)
	surf := strings.ToLower(surface)
	if surf != "clay" && surf != "grass" {
		surf = "hard"
	}

	if logo != nil && strings.TrimSpace(*logo) == "" {
		logo = nil
	}

	return &FeaturedTournament{
		Edition:      edition,
		SectionTitle: title,
		SectionMeta:  nonEmptyPtr(meta),
		StageLabel:   stage,
		Name:         name,
		Monogram:     tournamentMonogram(brand, name),
		CrestURL:     logo,
		DateRange:    formatDateRange(start, end),
		City:         cityFromLocation(location),
		CountryCode:  country,
		Surface:      surf,
		SurfaceLabel: surfaceLabel(lang, surface, in.indoor),
	}, nil
}

func currentRoundLabel(ctx context.Context, pool *pgxpool.Pool, lang, edition string) (string, error) {
	var label string
	err := pool.QueryRow(ctx, `
		select coalesce(ro.label->>$2, ro.label->>'en', m.round_code)
		from matches m
		join tournament_editions te on te.id = m.edition_id
		join rounds ro on ro.code = m.round_code
		where te.slug = $1 and m.status in ('live', 'scheduled')
		  and m.round_code !~* '^q[0-9]'
		order by case when m.status = 'live' then 0 else 1 end, ro.sort_order, m.id
		limit 1`, edition, lang).Scan(&label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return label, err
}
