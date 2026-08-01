package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/StVl/tennis-backend/internal/storage"
)

// ListPlayers — GET /v1/players?tracked=true&search=&lang=
// По умолчанию только ростер; ?tracked=false — все, включая соперников извне.
func (h *Handler) ListPlayers(w http.ResponseWriter, r *http.Request) {
	trackedOnly := r.URL.Query().Get("tracked") != "false"
	items, err := storage.ListPlayers(r.Context(), h.pool, langParam(r), trackedOnly, r.URL.Query().Get("search"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetPlayer — GET /v1/players/{slug}
func (h *Handler) GetPlayer(w http.ResponseWriter, r *http.Request) {
	p, err := storage.GetPlayer(r.Context(), h.pool, langParam(r), chi.URLParam(r, "slug"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ListPlayerMatches — GET /v1/players/{slug}/matches?status=completed&limit=3
// Матчи «глазами игрока»: оппонент, result won/lost, счёт с его стороны.
func (h *Handler) ListPlayerMatches(w http.ResponseWriter, r *http.Request) {
	statuses, ok := statusList(r, "scheduled", "live", "completed", "cancelled")
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_status", "status must be of: scheduled, live, completed, cancelled")
		return
	}
	// история (только completed) — новые сверху; расписание — ближайшие сверху
	newestFirst := len(statuses) == 1 && statuses[0] == "completed"
	items, err := storage.ListPlayerMatches(r.Context(), h.pool, chi.URLParam(r, "slug"),
		statuses, newestFirst, intParam(r, "limit", 50, 200), intParam(r, "offset", 0, 0))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ListPlayerTournaments — GET /v1/players/{slug}/tournaments?status=upcoming,ongoing
func (h *Handler) ListPlayerTournaments(w http.ResponseWriter, r *http.Request) {
	statuses, ok := statusList(r, "upcoming", "ongoing", "completed")
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_status", "status must be of: upcoming, ongoing, completed")
		return
	}
	items, err := storage.ListPlayerTournaments(r.Context(), h.pool, chi.URLParam(r, "slug"), statuses)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// PlayerRankingHistory — GET /v1/players/{slug}/ranking-history
func (h *Handler) PlayerRankingHistory(w http.ResponseWriter, r *http.Request) {
	items, err := storage.PlayerRankingHistory(r.Context(), h.pool, chi.URLParam(r, "slug"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// HeadToHead — GET /v1/players/{slug}/h2h/{other} (глазами первого игрока)
func (h *Handler) HeadToHead(w http.ResponseWriter, r *http.Request) {
	res, err := storage.HeadToHead(r.Context(), h.pool, chi.URLParam(r, "slug"), chi.URLParam(r, "other"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ListRankings — GET /v1/rankings?limit=100&tour=atp
func (h *Handler) ListRankings(w http.ResponseWriter, r *http.Request) {
	tour := r.URL.Query().Get("tour")
	if tour == "" {
		tour = "atp"
	}
	items, err := storage.ListRankings(r.Context(), h.pool, tour, intParam(r, "limit", 100, 500))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ListPlayStyles — GET /v1/play-styles
func (h *Handler) ListPlayStyles(w http.ResponseWriter, r *http.Request) {
	items, err := storage.ListPlayStyles(r.Context(), h.pool, langParam(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
