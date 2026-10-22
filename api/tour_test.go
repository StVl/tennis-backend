package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Неизвестный тур — 400 bad_tour, до похода в БД (pool = nil), а не молчаливый ATP.
func TestTourParamRejectsUnknown(t *testing.T) {
	router := NewRouter(nil, HandlerConfig{})
	for _, path := range []string{
		"/v1/home?tour=itf",
		"/v1/players?tracked=true&tour=WTA",
	} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", path, rec.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "bad_tour" {
			t.Errorf("%s: body %s, want error.code bad_tour", path, rec.Body.String())
		}
	}
}
