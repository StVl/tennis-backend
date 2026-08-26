package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HandlerConfig — то немногое из конфигурации, что нужно обработчикам на
// каждом запросе. Отдельной структурой, а не *config.Config: api не должен
// импортировать config, иначе слой ручек начинает зависеть от разбора env.
type HandlerConfig struct {
	// Подключать ли раздел /v1/dev с ручными триггерами live-статуса.
	DevEndpoints bool
	// Потолок числа матчей в /v1/users/me/live-matches: клиент держит
	// ограниченное число одновременных Live Activity.
	LiveMatchesCap int
}

type Handler struct {
	pool *pgxpool.Pool
	cfg  HandlerConfig
}

func NewHandler(pool *pgxpool.Pool, cfg HandlerConfig) *Handler {
	return &Handler{pool: pool, cfg: cfg}
}

func (h *Handler) Hello(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"message": "hello"})
}

func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
