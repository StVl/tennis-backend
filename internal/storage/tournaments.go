package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EditionListItem — розыгрыш в списках (будущие/текущие/прошедшие).
type EditionListItem struct {
	Edition    string    `json:"edition"`
	Tournament string    `json:"tournament"`
	Name       string    `json:"name"`
	Year       int       `json:"year"`
	StartDate  time.Time `json:"start_date"`
	EndDate    time.Time `json:"end_date"`
	Surface    string    `json:"surface"`
	Status     string    `json:"status"`
	Location   *string   `json:"location"`
	LogoURL    *string   `json:"logo_url"`
	Champion   *Opponent `json:"champion"`
	RunnerUp   *Opponent `json:"runner_up"`
}

// EditionDetail — карточка розыгрыша.
type EditionDetail struct {
	EditionListItem
	Description   json.RawMessage `json:"description"`
	Conditions    json.RawMessage `json:"conditions"`
	DrawSize      *int            `json:"draw_size"`
	PrizeMoney    *int64          `json:"prize_money"`
	PrizeCurrency *string         `json:"prize_currency"`
	DrawDate      *time.Time      `json:"draw_date"`
	DrawStatus    string          `json:"draw_status"`
	CountryCode   *string         `json:"country_code"`
	DateRange     string          `json:"date_range"`
	CourtImageURL *string         `json:"court_image_url"`
	Rounds        []DrawCardRound `json:"rounds"`
	Entries       []EditionEntry  `json:"entries"`
}

// DrawCardRound — раунд основной сетки для карточки турнира.
type DrawCardRound struct {
	Code    string          `json:"code"`
	Title   string          `json:"title"`
	Matches []DrawCardMatch `json:"matches"`
}

type DrawCardMatch struct {
	ID         int64    `json:"id"`
	BracketPos *int     `json:"bracket_pos"`
	Top        DrawSlot `json:"top"`
	Bottom     DrawSlot `json:"bottom"`
}

// DrawSlot — одна сторона вилки. tbd=true, если игрока ещё нет.
type DrawSlot struct {
	Name   string  `json:"name"`
	Slug   *string `json:"slug"`
	Flag   *string `json:"flag"`
	Seed   *int    `json:"seed"`
	Winner bool    `json:"winner"`
	TBD    bool    `json:"tbd"`
}

type EditionEntry struct {
	Slug     string  `json:"slug"`
	Name     string  `json:"name"`
	PhotoURL *string `json:"photo_url"`
	Seed     *int    `json:"seed"`
	Status   string  `json:"status"`
	Rank     *int    `json:"rank"`
}

// DrawRound — раунд сетки со списком матчей.
type DrawRound struct {
	Round   string  `json:"round"`
	Label   string  `json:"label"`
	Matches []Match `json:"matches"`
}

// PastEdition — строка истории розыгрышей турнира.
type PastEdition struct {
	Edition  string    `json:"edition"`
	Year     int       `json:"year"`
	Surface  string    `json:"surface"`
	Champion *Opponent `json:"champion"`
	RunnerUp *Opponent `json:"runner_up"`
}

const editionSelect = `
	select v.slug, t.slug, t.name, v.year, v.start_date, v.end_date,
	       v.surface::text, v.status, t.location, t.logo_url,
	       ch.slug, ch.display_name, ch.photo_url,
	       ru.slug, ru.display_name, ru.photo_url
	from v_tournament_editions v
	join tournaments t on t.id = v.tournament_id
	left join players ch on ch.id = v.champion_id
	left join players ru on ru.id = v.runner_up_id`

func scanEdition(row pgx.CollectableRow) (EditionListItem, error) {
	var (
		e                       EditionListItem
		chSlug, chName, chPhoto *string
		ruSlug, ruName, ruPhoto *string
	)
	err := row.Scan(&e.Edition, &e.Tournament, &e.Name, &e.Year, &e.StartDate, &e.EndDate,
		&e.Surface, &e.Status, &e.Location, &e.LogoURL,
		&chSlug, &chName, &chPhoto, &ruSlug, &ruName, &ruPhoto)
	if err != nil {
		return e, err
	}
	if chSlug != nil {
		e.Champion = &Opponent{Slug: *chSlug, Name: deref(chName), PhotoURL: chPhoto}
	}
	if ruSlug != nil {
		e.RunnerUp = &Opponent{Slug: *ruSlug, Name: deref(ruName), PhotoURL: ruPhoto}
	}
	return e, nil
}

func ListEditions(ctx context.Context, pool *pgxpool.Pool, statuses []string, year int) ([]EditionListItem, error) {
	rows, err := pool.Query(ctx, editionSelect+`
		where v.status = any($1) and ($2 = 0 or v.year = $2)
		order by v.start_date, t.name`,
		statuses, year)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanEdition)
}

func GetEdition(ctx context.Context, pool *pgxpool.Pool, lang, editionSlug string) (*EditionDetail, error) {
	rows, err := pool.Query(ctx, `
		select v.slug, t.slug, t.name, v.year, v.start_date, v.end_date,
		       v.surface::text, v.status, t.location, t.logo_url,
		       ch.slug, ch.display_name, ch.photo_url,
		       ru.slug, ru.display_name, ru.photo_url,
		       coalesce(t.description->>$2, t.description->>'en', t.description->>'ru'),
		       t.conditions::text, v.draw_size, v.prize_money::bigint, v.prize_currency,
		       v.draw_date, v.draw_status::text, t.country_code
		from v_tournament_editions v
		join tournaments t on t.id = v.tournament_id
		left join players ch on ch.id = v.champion_id
		left join players ru on ru.id = v.runner_up_id
		where v.slug = $1`,
		editionSlug, lang)
	if err != nil {
		return nil, err
	}
	d, err := pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (EditionDetail, error) {
		var (
			d                       EditionDetail
			chSlug, chName, chPhoto *string
			ruSlug, ruName, ruPhoto *string
			desc, conds             *string
		)
		err := row.Scan(&d.Edition, &d.Tournament, &d.Name, &d.Year, &d.StartDate, &d.EndDate,
			&d.Surface, &d.Status, &d.Location, &d.LogoURL,
			&chSlug, &chName, &chPhoto, &ruSlug, &ruName, &ruPhoto,
			&desc, &conds, &d.DrawSize, &d.PrizeMoney, &d.PrizeCurrency,
			&d.DrawDate, &d.DrawStatus, &d.CountryCode)
		if err != nil {
			return d, err
		}
		if chSlug != nil {
			d.Champion = &Opponent{Slug: *chSlug, Name: deref(chName), PhotoURL: chPhoto}
		}
		if ruSlug != nil {
			d.RunnerUp = &Opponent{Slug: *ruSlug, Name: deref(ruName), PhotoURL: ruPhoto}
		}
		if desc != nil {
			b, _ := json.Marshal(*desc)
			d.Description = b
		} else {
			d.Description = json.RawMessage("null")
		}
		d.Conditions = rawOrNull(conds)
		return d, nil
	})
	if err != nil {
		return nil, err
	}

	entryRows, err := pool.Query(ctx, `
		select p.slug, p.display_name, p.photo_url, e.seed, e.status::text, r.rank
		from tournament_entries e
		join players p on p.id = e.player_id
		left join v_current_rankings r on r.player_id = p.id and r.tour_code = 'atp'
		where e.edition_id = (select id from tournament_editions where slug = $1)
		order by coalesce(r.rank, 100000), p.display_name`,
		editionSlug)
	if err != nil {
		return nil, err
	}
	d.Entries, err = pgx.CollectRows(entryRows, func(row pgx.CollectableRow) (EditionEntry, error) {
		var e EditionEntry
		err := row.Scan(&e.Slug, &e.Name, &e.PhotoURL, &e.Seed, &e.Status, &e.Rank)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	d.DateRange = formatDateRange(d.StartDate, d.EndDate)
	d.CourtImageURL = courtImageURL(d.Surface)
	d.Rounds = []DrawCardRound{}
	if d.DrawStatus == "drawn" {
		d.Rounds, err = loadDrawCard(ctx, pool, lang, editionSlug)
		if err != nil {
			return nil, err
		}
		if d.Rounds == nil {
			d.Rounds = []DrawCardRound{}
		}
	}
	return &d, nil
}

// TournamentDraw — сетка розыгрыша: матчи по раундам в порядке rounds.sort_order.
func TournamentDraw(ctx context.Context, pool *pgxpool.Pool, lang, editionSlug string) ([]DrawRound, error) {
	rows, err := pool.Query(ctx, matchSelect+`
		join rounds ro on ro.code = m.round_code
		where te.slug = $1
		order by ro.sort_order, m.bracket_pos nulls last, m.scheduled_at nulls last, m.id`,
		editionSlug)
	if err != nil {
		return nil, err
	}
	matches, err := pgx.CollectRows(rows, scanMatch)
	if err != nil {
		return nil, err
	}

	labelRows, err := pool.Query(ctx,
		`select code, coalesce(label->>$1, label->>'en') from rounds`, lang)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for labelRows.Next() {
		var code, label string
		if err := labelRows.Scan(&code, &label); err != nil {
			return nil, err
		}
		labels[code] = label
	}

	result := []DrawRound{}
	for _, m := range matches {
		if len(result) == 0 || result[len(result)-1].Round != m.Round {
			result = append(result, DrawRound{Round: m.Round, Label: labels[m.Round]})
		}
		last := &result[len(result)-1]
		last.Matches = append(last.Matches, m)
	}
	return result, nil
}

// TournamentHistory — прошлые розыгрыши турнира (бренда) по годам.
func TournamentHistory(ctx context.Context, pool *pgxpool.Pool, tournamentSlug string) ([]PastEdition, error) {
	rows, err := pool.Query(ctx, `
		select te.slug, te.year, te.surface::text,
		       ch.slug, ch.display_name, ch.photo_url,
		       ru.slug, ru.display_name, ru.photo_url
		from tournament_editions te
		join tournaments t on t.id = te.tournament_id
		left join players ch on ch.id = te.champion_id
		left join players ru on ru.id = te.runner_up_id
		where t.slug = $1
		order by te.year desc`,
		tournamentSlug)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PastEdition, error) {
		var (
			e                       PastEdition
			chSlug, chName, chPhoto *string
			ruSlug, ruName, ruPhoto *string
		)
		err := row.Scan(&e.Edition, &e.Year, &e.Surface,
			&chSlug, &chName, &chPhoto, &ruSlug, &ruName, &ruPhoto)
		if err != nil {
			return e, err
		}
		if chSlug != nil {
			e.Champion = &Opponent{Slug: *chSlug, Name: deref(chName), PhotoURL: chPhoto}
		}
		if ruSlug != nil {
			e.RunnerUp = &Opponent{Slug: *ruSlug, Name: deref(ruName), PhotoURL: ruPhoto}
		}
		return e, nil
	})
}
