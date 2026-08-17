package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/StVl/tennis-backend/internal/storage"
)

type ctxKey int

const ctxUserID ctxKey = iota

// requireUser — middleware зоны /users/me: Bearer-токен -> user_id в контексте.
func (h *Handler) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(auth, "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Bearer token required")
			return
		}
		userID, err := storage.AuthUser(r.Context(), h.pool, token)
		if errors.Is(err, storage.ErrUnauthorized) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "unknown token")
			return
		}
		if err != nil {
			respondQueryError(w, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUserID, userID)))
	})
}

func userID(r *http.Request) string {
	id, _ := r.Context().Value(ctxUserID).(string)
	return id
}

// CreateUser — POST /v1/users {device_id?} -> 201 {user_id, token}
// Анонимная регистрация; токен показывается один раз, клиент хранит его в Keychain.
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DeviceID string `json:"device_id"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body) // тело опционально
	}
	u, err := storage.CreateUser(r.Context(), h.pool, body.DeviceID)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

// Me — GET /v1/users/me
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, err := storage.GetProfile(r.Context(), h.pool, userID(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ListFollows — GET /v1/users/me/follows
func (h *Handler) ListFollows(w http.ResponseWriter, r *http.Request) {
	items, err := storage.ListFollows(r.Context(), h.pool, userID(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// AddFollow — PUT /v1/users/me/follows/{slug} (идемпотентно)
func (h *Handler) AddFollow(w http.ResponseWriter, r *http.Request) {
	ok, err := storage.AddFollow(r.Context(), h.pool, userID(r), chi.URLParam(r, "slug"))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "player not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RemoveFollow — DELETE /v1/users/me/follows/{slug} (идемпотентно)
func (h *Handler) RemoveFollow(w http.ResponseWriter, r *http.Request) {
	if err := storage.RemoveFollow(r.Context(), h.pool, userID(r), chi.URLParam(r, "slug")); err != nil {
		respondQueryError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReplaceFollows — PUT /v1/users/me/follows {"player_slugs": [...]} (онбординг)
func (h *Handler) ReplaceFollows(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PlayerSlugs []string `json:"player_slugs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", `expected {"player_slugs": ["sinner", ...]}`)
		return
	}
	unknown, err := storage.ReplaceFollows(r.Context(), h.pool, userID(r), body.PlayerSlugs)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	items, err := storage.ListFollows(r.Context(), h.pool, userID(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unknown_slugs": unknown})
}

// MyHome — GET /v1/users/me/home: как /v1/home, но подписки из БД.
func (h *Handler) MyHome(w http.ResponseWriter, r *http.Request) {
	followed, err := storage.FollowSlugs(r.Context(), h.pool, userID(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	feed, err := storage.GetHomeFeed(
		r.Context(), h.pool, langParam(r), followed, highlightsDaysParam(r),
	)
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feed)
}

// MyWidget — GET /v1/users/me/widget?tz=: как /v1/widget, но подписки из БД.
func (h *Handler) MyWidget(w http.ResponseWriter, r *http.Request) {
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			writeError(w, http.StatusBadRequest, "bad_tz", "unknown timezone: "+tz)
			return
		}
	}
	followed, err := storage.FollowSlugs(r.Context(), h.pool, userID(r))
	if err != nil {
		respondQueryError(w, err)
		return
	}
	feed, err := storage.GetWidgetFeed(r.Context(), h.pool, followed, loc, time.Now())
	if err != nil {
		respondQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, feed)
}
