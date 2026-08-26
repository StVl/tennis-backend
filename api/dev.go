package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/StVl/tennis-backend/internal/storage"
)

// Ручные триггеры live-статуса. Подключаются только при DEV_ENDPOINTS_ENABLED
// (см. NewRouter) и существуют, чтобы iOS могла собрать и отладить весь цикл
// Live Activity, не дожидаясь ни интеграции с источником, ни загрузки сеток.
// Ходят через те же storage.FlipLive/FlipOut, что и поллер, — так ручной флип
// не может создать состояние, которого не бывает у боевого пути.

// DevMatchLive — POST /v1/dev/matches/{id}/live
// 200 + состояние флипа, если статус изменился; 204, если матч уже live.
func (h *Handler) DevMatchLive(w http.ResponseWriter, r *http.Request) {
	id, ok := matchIDParam(w, r)
	if !ok {
		return
	}
	changed, err := storage.FlipLive(
		r.Context(), h.pool, id, storage.LiveSourceDev, "", nil, time.Now())
	if err != nil {
		respondQueryError(w, err)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.respondLiveFlag(w, r, id)
}

// DevMatchFinish — POST /v1/dev/matches/{id}/finish
// Возвращает матчу статус, который был до флипа. 204, если матч не был флипнут.
func (h *Handler) DevMatchFinish(w http.ResponseWriter, r *http.Request) {
	id, ok := matchIDParam(w, r)
	if !ok {
		return
	}
	changed, err := storage.FlipOut(
		r.Context(), h.pool, id, storage.LiveEventFinished, "dev", time.Now())
	if err != nil {
		respondQueryError(w, err)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.respondLiveFlag(w, r, id)
}

// DevLiveState — GET /v1/dev/matches/{id}/live-state
// Текущий статус матча и наш флаг, если он есть.
func (h *Handler) DevLiveState(w http.ResponseWriter, r *http.Request) {
	id, ok := matchIDParam(w, r)
	if !ok {
		return
	}
	h.respondLiveFlag(w, r, id)
}

func (h *Handler) respondLiveFlag(w http.ResponseWriter, r *http.Request, id int64) {
	flag, err := storage.GetLiveFlag(r.Context(), h.pool, id)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, flag)
}

// matchIDParam — общий разбор {id}; false означает, что ответ уже записан.
func matchIDParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "match id must be an integer")
		return 0, false
	}
	return id, true
}
