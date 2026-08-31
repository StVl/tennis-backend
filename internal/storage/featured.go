package storage

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// FeaturedTournament — карточка турнира на главном. Все строки уже для экрана.
type FeaturedTournament struct {
	Edition      string  `json:"edition"`
	SectionTitle string  `json:"section_title"`
	SectionMeta  *string `json:"section_meta"`
	StageLabel   string  `json:"stage_label"`
	Name         string  `json:"name"`
	Monogram     string  `json:"monogram"`
	CrestURL     *string `json:"crest_url"`
	DateRange    string  `json:"date_range"`
	City         *string `json:"city"`
	CountryCode  *string `json:"country_code"`
	Surface      string  `json:"surface"`
	SurfaceLabel string  `json:"surface_label"`
}

type featuredInput struct {
	lang          string
	now           time.Time
	status        string // upcoming | ongoing | completed
	drawStatus    string // awaiting_draw | drawn
	start, end    time.Time
	drawDate      *time.Time
	roundLabel    string
	tournament    string // brand slug
	name          string
	surface       string
	indoor        bool
	championLast  string
	category      string
}

func featuredCopy(in featuredInput) (title, meta, stage string) {
	surfaceLabel := surfaceLabel(in.lang, in.surface, in.indoor)
	categoryLine := joinDot(in.category, surfaceLabel)

	switch {
	case in.status == "ongoing":
		n, total := tournamentDay(in.now, in.start, in.end)
		title = pick(in.lang, "On court now", "Идёт турнир")
		meta = fmt.Sprintf(pick(in.lang, "Day %d of %d", "День %d из %d"), n, total)
		if in.roundLabel != "" {
			stage = in.roundLabel + " · " + pick(in.lang, fmt.Sprintf("Day %d", n), fmt.Sprintf("День %d", n))
		} else {
			stage = categoryLine
		}
	case in.drawStatus == "awaiting_draw":
		title = pick(in.lang, "Coming up", "Скоро")
		if in.drawDate != nil {
			meta = pick(in.lang, "Draw ", "Сетка ") + formatDay(*in.drawDate)
		}
		stage = categoryLine
	case in.status == "completed":
		title = pick(in.lang, "Just finished", "Только что")
		if in.championLast != "" {
			meta = pick(in.lang, in.championLast+" won", in.championLast+" победил")
		}
		stage = categoryLine
	default:
		// Draw published, play not started.
		title = startsTitle(in.lang, in.now, in.start)
		meta = pick(in.lang, "Main draw set", "Сетка готова")
		stage = categoryLine
	}
	return title, meta, stage
}

func tournamentDay(now, start, end time.Time) (n, total int) {
	startDay := truncateUTCDate(start)
	endDay := truncateUTCDate(end)
	nowDay := truncateUTCDate(now)
	total = int(endDay.Sub(startDay).Hours()/24) + 1
	if total < 1 {
		total = 1
	}
	n = int(nowDay.Sub(startDay).Hours()/24) + 1
	if n < 1 {
		n = 1
	}
	if n > total {
		n = total
	}
	return n, total
}

func truncateUTCDate(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func startsTitle(lang string, now, start time.Time) string {
	nowDay := truncateUTCDate(now)
	startDay := truncateUTCDate(start)
	tomorrow := nowDay.AddDate(0, 0, 1)
	if startDay.Equal(tomorrow) {
		return pick(lang, "Starts tomorrow", "Старт завтра")
	}
	weekday := weekdayName(lang, startDay.Weekday())
	return pick(lang, "Starts "+weekday, "Старт в "+weekday)
}

func weekdayName(lang string, d time.Weekday) string {
	if lang == "ru" {
		return [...]string{
			time.Sunday:    "воскресенье",
			time.Monday:    "понедельник",
			time.Tuesday:   "вторник",
			time.Wednesday: "среду",
			time.Thursday:  "четверг",
			time.Friday:    "пятницу",
			time.Saturday:  "субботу",
		}[d]
	}
	return d.String()
}

func formatDay(t time.Time) string {
	u := t.UTC()
	return fmt.Sprintf("%d %s", u.Day(), monthAbbrev[u.Month()])
}

func surfaceLabel(lang, surface string, indoor bool) string {
	s := strings.ToLower(surface)
	if indoor && s == "hard" {
		return pick(lang, "Indoor hard", "Хард в зале")
	}
	switch s {
	case "clay":
		return pick(lang, "Clay", "Грунт")
	case "grass":
		return pick(lang, "Grass", "Трава")
	default:
		return pick(lang, "Hard", "Хард")
	}
}

func tournamentCategory(slug string) string {
	switch slug {
	case "australian_open", "roland_garros", "wimbledon", "us_open":
		return "Grand Slam"
	case "indian_wells", "miami", "monte_carlo", "madrid", "rome",
		"canada", "cincinnati", "shanghai", "paris":
		return "Masters 1000"
	default:
		return ""
	}
}

func tournamentMonogram(slug, name string) string {
	switch slug {
	case "australian_open":
		return "AO"
	case "us_open":
		return "US"
	case "roland_garros":
		return "RG"
	case "wimbledon":
		return "W"
	default:
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return "?"
		}
		r, _ := utf8.DecodeRuneInString(trimmed)
		return string(unicode.ToUpper(r))
	}
}

func cityFromLocation(location *string) *string {
	if location == nil {
		return nil
	}
	city, _, _ := strings.Cut(strings.TrimSpace(*location), ",")
	city = strings.TrimSpace(city)
	if city == "" {
		return nil
	}
	return &city
}

func joinDot(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, " · ")
}

func pick(lang, en, ru string) string {
	if lang == "ru" {
		return ru
	}
	return en
}

func nonEmptyPtr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}
