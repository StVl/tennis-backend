package api

import (
	"io"
	"net/http"
	"time"

	"github.com/StVl/tennis-backend/internal/livesource"
	"github.com/StVl/tennis-backend/internal/storage"
	"github.com/StVl/tennis-backend/internal/updater/live"
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

// DevLiveIngest — POST /v1/dev/live/ingest, тело = сырой ответ борта источника.
//
// Прогоняет ПОЛНЫЙ цикл разбора — тот же код, что и поллер, — но из
// сохранённого файла и без единого запроса к вендору. Это единственный способ
// проверить переход scheduled -> live -> прежний статус с событиями в outbox,
// не дожидаясь настоящего живого матча и не тратя суточную квоту, которой
// всего 100.
//
// Handler по-прежнему не знает ни про клиент источника, ни про конфиг целиком:
// тело приходит в запросе, а из настроек нужны две длительности.
func (h *Handler) DevLiveIngest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "cannot read request body")
		return
	}
	board, err := livesource.ParseBoard(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_board", err.Error())
		return
	}

	// Единственное обращение к часам: started_at прогона, как и в поллере.
	runID, now, err := storage.StartRun(r.Context(), h.pool, "live", livesource.SourceName)
	if err != nil {
		respondQueryError(w, err)
		return
	}

	prevInScope, err := storage.PrevSuccessfulInScope(r.Context(), h.pool)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	res, ingestErr := live.Ingest(r.Context(), h.pool, board, live.IngestParams{
		RunID:       runID,
		Now:         now,
		MatchWindow: h.cfg.LiveMatchWindow,
		MaxLiveAge:  h.cfg.LiveMaxLiveAge,
		PrevInScope: prevInScope,
	})

	// Счётчики заполняются так же, как у поллера: строка прогона — единственное
	// окно в то, почему карточка появилась или нет, и повтор не должен быть в
	// ней менее читаемым.
	//
	// Чего здесь СОЗНАТЕЛЬНО нет — IncRunRequests: повтор не ходит к вендору и
	// не должен перекашивать регулятор квоты. Именно поэтому в предикате
	// пропусков (HeldLiveFlags) нет условия requests_made > 0 — иначе повтор
	// не считался бы пропуском и трёхцикловой выход из live через dev-путь
	// проверить было бы нельзя. Асимметрия несущая, «чинить» её не надо.
	runResult := storage.RunResult{
		Mode:                  "replay",
		RowsParsed:            &board.RowsParsed,
		RowsInScope:           &res.RowsInScope,
		RowsMatched:           &res.RowsMatched,
		RowsDroppedUnresolved: &res.RowsDropped,
	}
	if ingestErr != nil {
		runResult.Error = ingestErr.Error()
	} else if res.GuardTripped != "" {
		runResult.Error = res.GuardTripped
	}
	_ = storage.FinishRun(r.Context(), h.pool, runID, runResult)

	if ingestErr != nil {
		respondQueryError(w, ingestErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":        runID,
		"rows_parsed":   board.RowsParsed,
		"rows_doubles":  board.RowsDoubles,
		"rows_unusable": board.RowsUnusable,
		"in_scope":      res.RowsInScope,
		"matched":       res.RowsMatched,
		"dropped":       res.RowsDropped,
		"entered":       res.Entered,
		"left":          res.Left,
		"refused":       res.Refused,
		"guard":         res.GuardTripped,
	})
}

// DevUnmatchedQueue — GET /v1/dev/live/unmatched
//
// Очередь ревью в разрезе причин. Существует потому, что она задокументирована
// как вход для пополнения сидов розыгрышей и игроков, а прочитать её иначе
// можно было только через psql на проде.
func (h *Handler) DevUnmatchedQueue(w http.ResponseWriter, r *http.Request) {
	queue, err := storage.UnmatchedQueue(r.Context(), h.pool, livesource.SourceName)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": queue})
}
