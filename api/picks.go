package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/StVl/tennis-backend/internal/storage"
)

// picksPinnedMax — больше матчей в игровом дне не бывает.
const picksPinnedMax = 6

// MyPicks — GET /v1/users/me/picks?tour=atp|wta&pinned=1,2,3
// Игровые дни Daily picks на вчера, сегодня и завтра (по местной дате турнира). pinned — матчи
// уже начавшегося у клиента дня: их список не пересобирается. Флаг выключен — {"days": []}.
func (h *Handler) MyPicks(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.DailyPicks {
		writeJSON(w, http.StatusOK, storage.PicksResponse{Days: []storage.PicksDay{}})
		return
	}
	tour, ok := tourParam(w, r)
	if !ok {
		return
	}
	pinned, ok := pinnedParam(w, r)
	if !ok {
		return
	}
	followed, err := storage.FollowSlugs(r.Context(), h.pool, userID(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	resp, err := storage.GetPicksDays(r.Context(), h.pool, followed, tour, time.Now(), pinned)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// pinnedParam — ?pinned=1,2,3: id матчей, не больше шести. Кривое значение — 400 bad_pinned.
func pinnedParam(w http.ResponseWriter, r *http.Request) ([]int64, bool) {
	raw := r.URL.Query().Get("pinned")
	if raw == "" {
		return nil, true
	}
	parts := strings.Split(raw, ",")
	if len(parts) > picksPinnedMax {
		writeError(w, http.StatusBadRequest, "bad_pinned", "at most 6 pinned matches")
		return nil, false
	}
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "bad_pinned", "pinned must be match ids")
			return nil, false
		}
		out = append(out, id)
	}
	return out, true
}
