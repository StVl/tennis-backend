package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/StVl/tennis-backend/internal/storage"
)

// playerIDsParam парсит ?player_ids=sinner,alcaraz (подписки пользователя;
// до появления серверных follows клиент передаёт их сам из App Group).
func playerIDsParam(r *http.Request) []string {
	raw := r.URL.Query().Get("player_ids")
	if raw == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// highlightsDaysParam — ширина окна «итогов недели», ?highlights_days=. По умолчанию 7; 0
// выключает блок. Потолок в год: окно шире — это уже не итоги недели, а история.
func highlightsDaysParam(r *http.Request) int {
	return intParam(r, "highlights_days", 7, 365)
}

// Home — GET /v1/home?player_ids=...&lang=&highlights_days=
// Главный экран одним запросом: карточки подписок + полная сетка ростера + итоги недели.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) {
	feed, err := storage.GetHomeFeed(
		r.Context(), h.pool, langParam(r), playerIDsParam(r), highlightsDaysParam(r),
	)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feed)
}

// Widget — GET /v1/widget?player_ids=...&tz=Europe/Belgrade
// Готовый таймлайн виджета: состояние, до 3 строк, колонка TODAY.
func (h *Handler) Widget(w http.ResponseWriter, r *http.Request) {
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			writeError(w, http.StatusBadRequest, "bad_tz", "unknown timezone: "+tz)
			return
		}
	}
	feed, err := storage.GetWidgetFeed(r.Context(), h.pool, playerIDsParam(r), loc, time.Now())
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feed)
}
