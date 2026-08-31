package storage

import (
	"testing"
	"time"
)

func TestTournamentDay(t *testing.T) {
	start := time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC)
	n, total := tournamentDay(now, start, end)
	if n != 8 || total != 14 {
		t.Fatalf("got day %d of %d, want 8 of 14", n, total)
	}
}

func TestFeaturedCopyOngoing(t *testing.T) {
	title, meta, stage := featuredCopy(featuredInput{
		lang:       "en",
		now:        time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		status:     "ongoing",
		drawStatus: "drawn",
		start:      time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		end:        time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		roundLabel: "Round of 16",
		category:   "Grand Slam",
		surface:    "grass",
	})
	if title != "On court now" || meta != "Day 8 of 14" || stage != "Round of 16 · Day 8" {
		t.Fatalf("got %q / %q / %q", title, meta, stage)
	}

	title, _, _ = featuredCopy(featuredInput{
		lang:       "ru",
		now:        time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
		status:     "ongoing",
		drawStatus: "drawn",
		start:      time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		end:        time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		surface:    "grass",
	})
	if title != "Идёт турнир" {
		t.Fatalf("ru title %q", title)
	}
}

func TestFeaturedCopyAwaitingDraw(t *testing.T) {
	draw := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	title, meta, stage := featuredCopy(featuredInput{
		lang:       "en",
		now:        time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		status:     "upcoming",
		drawStatus: "awaiting_draw",
		start:      time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC),
		end:        time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC),
		drawDate:   &draw,
		category:   "Masters 1000",
		surface:    "clay",
	})
	if title != "Coming up" || meta != "Draw 10 Apr" || stage != "Masters 1000 · Clay" {
		t.Fatalf("got %q / %q / %q", title, meta, stage)
	}
}

func TestFeaturedCopyStartsTomorrow(t *testing.T) {
	title, meta, stage := featuredCopy(featuredInput{
		lang:       "en",
		now:        time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC),
		status:     "upcoming",
		drawStatus: "drawn",
		start:      time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
		end:        time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC),
		category:   "Masters 1000",
		surface:    "hard",
	})
	if title != "Starts tomorrow" || meta != "Main draw set" || stage != "Masters 1000 · Hard" {
		t.Fatalf("got %q / %q / %q", title, meta, stage)
	}
}

func TestFeaturedCopyJustFinished(t *testing.T) {
	title, meta, _ := featuredCopy(featuredInput{
		lang:         "en",
		now:          time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC),
		status:       "completed",
		drawStatus:   "drawn",
		start:        time.Date(2026, 6, 29, 0, 0, 0, 0, time.UTC),
		end:          time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC),
		championLast: "Sinner",
		surface:      "grass",
	})
	if title != "Just finished" || meta != "Sinner won" {
		t.Fatalf("got %q / %q", title, meta)
	}
}

func TestFeaturedCopyIndoorHard(t *testing.T) {
	_, _, stage := featuredCopy(featuredInput{
		lang:       "en",
		now:        time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		status:     "upcoming",
		drawStatus: "awaiting_draw",
		start:      time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
		end:        time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC),
		surface:    "hard",
		indoor:     true,
	})
	if stage != "Indoor hard" {
		t.Fatalf("stage %q", stage)
	}
}

func TestTournamentMonogram(t *testing.T) {
	if got := tournamentMonogram("australian_open", "Australian Open"); got != "AO" {
		t.Fatalf("AO: %q", got)
	}
	if got := tournamentMonogram("wimbledon", "The Championships, Wimbledon"); got != "W" {
		t.Fatalf("Wimbledon: %q", got)
	}
}

func TestCityFromLocation(t *testing.T) {
	loc := "London, UK"
	if got := cityFromLocation(&loc); got == nil || *got != "London" {
		t.Fatalf("got %v", got)
	}
}
