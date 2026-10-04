package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAppConfigReportsPaidSubscriptions(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		// pool = nil: ручка не должна трогать БД
		router := NewRouter(nil, HandlerConfig{PaidSubscriptions: enabled})

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/config", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%v: status %d, want 200", enabled, rec.Code)
		}

		var body map[string]map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("enabled=%v: decode %q: %v", enabled, rec.Body.String(), err)
		}
		got, ok := body["features"]["paid_subscriptions"]
		if !ok {
			t.Fatalf("enabled=%v: features.paid_subscriptions missing in %s", enabled, rec.Body.String())
		}
		if got != enabled {
			t.Errorf("paid_subscriptions = %v, want %v", got, enabled)
		}
	}
}
