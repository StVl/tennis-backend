package api

import (
	"net/http"

	"github.com/StVl/tennis-backend/internal/storage"
)

// MyLiveMatches — GET /v1/users/me/live-matches
//
// Сверка при запуске приложения: клиент спрашивает, какие матчи среди его
// подписок идут ПРЯМО СЕЙЧАС, и гасит карточки, которых в ответе нет. Нужен
// потому, что Live Activity живёт на локскрине, пока её явно не закончат, —
// если пуш об окончании не дошёл (сбой источника, APNs, офлайн), карточка
// переживает матч, и снять её больше нечем.
//
// Ответ — нейтральная форма матча БЕЗ sets, score_text и live, плюс total и
// truncated: раз клиент гасит карточки по отсутствию в ответе, ответ не имеет
// права молча что-то не показать.
func (h *Handler) MyLiveMatches(w http.ResponseWriter, r *http.Request) {
	res, err := storage.UserLiveMatches(
		r.Context(), h.pool, userID(r), h.cfg.LiveMatchesLimit)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
