package api

import (
	"net/http"
	"time"

	"github.com/StVl/tennis-backend/internal/storage"
)

// Ручные триггеры live-статуса. Подключаются только при DEV_ENDPOINTS_ENABLED
// (см. NewRouter) и существуют, чтобы iOS могла собрать и отладить весь цикл
// Live Activity, не дожидаясь ни интеграции с источником, ни загрузки сеток.
//
// Ходят через те же storage.FlipLive/FlipOut, что и поллер, и подчиняются тому
// же guard'у: флипнуть можно только scheduled-матч без победителя. Поэтому
// ручной флип не может создать состояние, которого не бывает у боевого пути —
// в частности, «живой» матч с финальным счётом и winner_side. Матчи для
// флипа создаёт db/dev_fixtures.sql.

// DevMatchLive — POST /v1/dev/matches/{id}/live
// 200 + состояние, если статус изменился; 204, если матч уже live;
// 409, если guard не пропустил (матч не scheduled либо уже с победителем).
func (h *Handler) DevMatchLive(w http.ResponseWriter, r *http.Request) {
	id, ok := matchIDParam(w, r)
	if !ok {
		return
	}
	res, err := storage.FlipLive(
		r.Context(), h.pool, id, storage.LiveSourceDev, "", nil, time.Now())
	if err != nil {
		respondQueryError(w, err)
		return
	}
	switch res {
	case storage.FlipAlreadyLive:
		w.WriteHeader(http.StatusNoContent)
		return
	case storage.FlipRefused:
		writeError(w, http.StatusConflict, "not_flippable",
			"only a scheduled match without a winner can be flipped; see db/dev_fixtures.sql")
		return
	}
	h.respondLiveState(w, r, id)
}

// DevMatchFinish — POST /v1/dev/matches/{id}/finish
// Возвращает матчу статус, который был до флипа; в ответе он лежит в status.
// 204, если матч не был флипнут.
func (h *Handler) DevMatchFinish(w http.ResponseWriter, r *http.Request) {
	id, ok := matchIDParam(w, r)
	if !ok {
		return
	}
	changed, _, err := storage.FlipOut(
		r.Context(), h.pool, id, storage.LiveEventFinished, "dev", time.Now())
	if err != nil {
		respondQueryError(w, err)
		return
	}
	if !changed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.respondLiveState(w, r, id)
}

// DevLiveState — GET /v1/dev/matches/{id}/live-state
func (h *Handler) DevLiveState(w http.ResponseWriter, r *http.Request) {
	id, ok := matchIDParam(w, r)
	if !ok {
		return
	}
	h.respondLiveState(w, r, id)
}

func (h *Handler) respondLiveState(w http.ResponseWriter, r *http.Request, id int64) {
	state, err := storage.GetLiveMatchState(r.Context(), h.pool, id)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}
