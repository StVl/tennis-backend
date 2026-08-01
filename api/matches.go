package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/StVl/tennis-backend/internal/storage"
)

// ListMatches — GET /v1/matches
// Фильтры: ?date=today|YYYY-MM-DD (&tz=Area/City), ?status=live,..., ?player=slug,
// ?edition=slug. Сортировки: ?sort=start_at (default) | best_rank (колонка TODAY).
func (h *Handler) ListMatches(w http.ResponseWriter, r *http.Request) {
	statuses, ok := statusList(r, "scheduled", "live", "completed", "cancelled")
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_status", "status must be of: scheduled, live, completed, cancelled")
		return
	}
	f := storage.MatchFilter{
		Statuses: statuses,
		Player:   r.URL.Query().Get("player"),
		Edition:  r.URL.Query().Get("edition"),
		Limit:    intParam(r, "limit", 50, 200),
		Offset:   intParam(r, "offset", 0, 0),
	}

	switch r.URL.Query().Get("sort") {
	case "", "start_at":
	case "best_rank":
		f.SortByRank = true
	default:
		writeError(w, http.StatusBadRequest, "bad_sort", "sort must be one of: start_at, best_rank")
		return
	}

	if rawDate := r.URL.Query().Get("date"); rawDate != "" {
		loc := time.UTC
		if tz := r.URL.Query().Get("tz"); tz != "" {
			var err error
			if loc, err = time.LoadLocation(tz); err != nil {
				writeError(w, http.StatusBadRequest, "bad_tz", "unknown timezone: "+tz)
				return
			}
		}
		var day time.Time
		if rawDate == "today" {
			now := time.Now().In(loc)
			day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
		} else {
			parsed, err := time.ParseInLocation("2006-01-02", rawDate, loc)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_date", "date must be YYYY-MM-DD or 'today'")
				return
			}
			day = parsed
		}
		from, to := day.UTC(), day.AddDate(0, 0, 1).UTC()
		f.DayFrom, f.DayTo = &from, &to
	}

	items, err := storage.ListMatches(r.Context(), h.pool, f)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetMatch — GET /v1/matches/{id}
func (h *Handler) GetMatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "match id must be an integer")
		return
	}
	m, err := storage.GetMatch(r.Context(), h.pool, id)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
