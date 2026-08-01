package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/StVl/tennis-backend/internal/storage"
)

// ListTournaments — GET /v1/tournaments?status=upcoming|ongoing|completed&year=2026
// Возвращает розыгрыши (editions); статус вычислен из дат.
func (h *Handler) ListTournaments(w http.ResponseWriter, r *http.Request) {
	statuses, ok := statusList(r, "upcoming", "ongoing", "completed")
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_status", "status must be of: upcoming, ongoing, completed")
		return
	}
	items, err := storage.ListEditions(r.Context(), h.pool, statuses, intParam(r, "year", 0, 0))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetTournament — GET /v1/tournaments/{slug} (slug розыгрыша, напр. wimbledon_2026)
func (h *Handler) GetTournament(w http.ResponseWriter, r *http.Request) {
	d, err := storage.GetEdition(r.Context(), h.pool, langParam(r), chi.URLParam(r, "slug"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// TournamentDraw — GET /v1/tournaments/{slug}/draw — сетка по раундам.
func (h *Handler) TournamentDraw(w http.ResponseWriter, r *http.Request) {
	rounds, err := storage.TournamentDraw(r.Context(), h.pool, langParam(r), chi.URLParam(r, "slug"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rounds": rounds})
}

// TournamentHistory — GET /v1/tournaments/{slug}/history
// slug ТУРНИРА (бренда, напр. wimbledon) — розыгрыши по годам.
func (h *Handler) TournamentHistory(w http.ResponseWriter, r *http.Request) {
	items, err := storage.TournamentHistory(r.Context(), h.pool, chi.URLParam(r, "slug"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
