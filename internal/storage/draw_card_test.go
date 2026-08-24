package storage

import (
	"testing"
	"time"
)

func TestSlotName(t *testing.T) {
	tests := []struct {
		first, last, display, want string
	}{
		{"Carlos", "Alcaraz", "Carlos Alcaraz", "C. Alcaraz"},
		{"", "Sinner", "Jannik Sinner", "Sinner"},
		{"", "", "Jack Draper", "J. Draper"},
		{"", "", "Felix Auger-Aliassime", "F. Auger-Aliassime"},
		{"", "", "Tabilo", "Tabilo"},
		{"", "", "", ""},
	}
	for _, tc := range tests {
		if got := slotName(tc.first, tc.last, tc.display); got != tc.want {
			t.Errorf("slotName(%q,%q,%q)=%q want %q", tc.first, tc.last, tc.display, got, tc.want)
		}
	}
}

func TestFormatDateRange(t *testing.T) {
	start := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	if got := formatDateRange(start, end); got != "13 – 23 Aug" {
		t.Fatalf("same month: got %q", got)
	}
	end = time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if got := formatDateRange(start, end); got != "13 Aug – 2 Sep" {
		t.Fatalf("cross month: got %q", got)
	}
}

func TestCourtImageURL(t *testing.T) {
	tests := []struct {
		surface, want string
	}{
		{"hard", CourtImageBySurface["hard"]},
		{"HARD", CourtImageBySurface["hard"]},
		{"clay", CourtImageBySurface["clay"]},
		{"grass", CourtImageBySurface["grass"]},
	}
	for _, tc := range tests {
		got := courtImageURL(tc.surface)
		if got == nil || *got != tc.want {
			t.Errorf("courtImageURL(%q)=%v want %q", tc.surface, got, tc.want)
		}
	}
	if courtImageURL("carpet") != nil {
		t.Fatal("unknown surface should be nil")
	}
}

func TestMakeSlotTBD(t *testing.T) {
	s := makeSlot(nil, nil, nil, nil, nil, nil, nil, 1)
	if !s.TBD || s.Name != tbdSlotName || s.Winner {
		t.Fatalf("%+v", s)
	}
}
