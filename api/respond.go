package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: message}})
}

// respondQueryError переводит ошибки слоя запросов в HTTP-ответ.
func respondQueryError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

func langParam(r *http.Request) string {
	if lang := r.URL.Query().Get("lang"); lang != "" {
		return lang
	}
	return "en"
}

func intParam(r *http.Request, name string, def, max int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		return def
	}
	if max > 0 && v > max {
		return max
	}
	return v
}

// statusList парсит ?status=a,b и валидирует по allowed; пусто = все allowed.
func statusList(r *http.Request, allowed ...string) ([]string, bool) {
	raw := r.URL.Query().Get("status")
	if raw == "" {
		return allowed, true
	}
	set := map[string]bool{}
	for _, a := range allowed {
		set[a] = true
	}
	var out []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !set[s] {
			return nil, false
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return allowed, true
	}
	return out, true
}
