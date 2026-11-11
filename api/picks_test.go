package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppConfigReportsDailyPicks(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		router := NewRouter(nil, HandlerConfig{DailyPicks: enabled})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
		var body map[string]map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %q: %v", rec.Body.String(), err)
		}
		got, ok := body["features"]["daily_picks"]
		if !ok || got != enabled {
			t.Fatalf("daily_picks = %v (present %v), want %v", got, ok, enabled)
		}
	}
}

// Флаг выключен — пустой ответ без запроса в БД (pool = nil упал бы).
func TestPicks_FlagOffEmpty(t *testing.T) {
	h := NewHandler(nil, HandlerConfig{DailyPicks: false})
	rec := httptest.NewRecorder()
	h.MyPicks(rec, httptest.NewRequest(http.MethodGet, "/v1/users/me/picks?tour=atp", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"days\":[]}\n" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestPicks_BadParams(t *testing.T) {
	h := NewHandler(nil, HandlerConfig{DailyPicks: true})
	for _, q := range []string{"tour=xx", "pinned=1,abc", "pinned=1,2,3,4,5,6,7"} {
		rec := httptest.NewRecorder()
		h.MyPicks(rec, httptest.NewRequest(http.MethodGet, "/v1/users/me/picks?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, rec.Code)
		}
	}
}
