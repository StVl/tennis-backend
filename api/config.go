package api

import "net/http"

type ClientConfig struct {
	Features Features `json:"features"`
}

// Features — типизированная структура, а не map: новый флаг добавляется полем,
// и клиент всегда получает все ключи, даже выключенные.
type Features struct {
	PaidSubscriptions bool `json:"paid_subscriptions"`
}

// AppConfig не ходит в БД: флаги берутся из env на старте, и ответ должен
// приходить, даже когда база недоступна.
func (h *Handler) AppConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ClientConfig{
		Features: Features{PaidSubscriptions: h.cfg.PaidSubscriptions},
	})
}
