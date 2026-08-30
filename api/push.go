package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/StVl/tennis-backend/internal/storage"
)

// RegisterPushToken — PUT /v1/users/me/push-token
// Токен push-to-start, выданный iOS приложению.
func (h *Handler) RegisterPushToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Env   string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", "expected {\"token\": \"...\", \"env\": \"...\"}")
		return
	}
	if body.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_token", "token is required")
		return
	}
	// Окружение обязательно и не угадывается: sandbox-токен от боевого по виду
	// не отличить, а Apple на чужой хост отвечает BadDeviceToken.
	if body.Env != "sandbox" && body.Env != "production" {
		writeError(w, http.StatusBadRequest, "bad_env", "env must be sandbox or production")
		return
	}
	if err := storage.UpsertPushToken(r.Context(), h.pool, userID(r),
		body.Token, body.Env, time.Now()); err != nil {
		respondQueryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RegisterActivityToken — PUT /v1/users/me/live-activities/{id}
//
// Токен, который iOS выдаёт уже запущенной активности. Пока он не пришёл,
// завершить карточку пушем нечем — её снимет сама iOS через несколько часов.
func (h *Handler) RegisterActivityToken(w http.ResponseWriter, r *http.Request) {
	matchID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "match id must be an integer")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeError(w, http.StatusBadRequest, "bad_body", "expected {\"token\": \"...\"}")
		return
	}
	ok, err := storage.SetSessionUpdateToken(r.Context(), h.pool, userID(r), matchID, body.Token)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	if !ok {
		// Сессии нет: старт не отправлялся, либо карточка уже закончена.
		writeError(w, http.StatusNotFound, "no_session", "no open activity for this match")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
