package livesource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testRetry — политика без реального ожидания: записывает задержки вместо сна.
// Без этого тест повторов занимал бы секунды, а медленные тесты удаляют.
func testRetry(slept *[]time.Duration) RetryPolicy {
	return RetryPolicy{
		Max:    3,
		Base:   time.Second,
		Jitter: nil, // детерминизм важнее разброса
		Sleep: func(ctx context.Context, d time.Duration) error {
			*slept = append(*slept, d)
			return nil
		},
	}
}

const okBoard = `{"data":[],"meta":{"has_more":false}}`

func TestClientSendsAuthAndQuery(t *testing.T) {
	var gotAuth, gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotQuery = r.Header.Get("Authorization"), r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBoard))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-key", WithHTTPClient(srv.Client()))
	if _, err := c.PollLive(context.Background()); err != nil {
		t.Fatalf("PollLive: %v", err)
	}

	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, ожидалось Bearer secret-key", gotAuth)
	}
	if gotPath != "/matches" {
		t.Errorf("путь = %q, ожидалось /matches (лишний слэш от базового URL?)", gotPath)
	}
	if gotQuery.Get("status") != "live" || gotQuery.Get("limit") != "200" {
		t.Errorf("query = %v", gotQuery)
	}
}

// Повторяемый player= — то, ради чего выбран этот эндпоинт. url.Values.Add,
// а не Set: со Set в запрос ушёл бы один игрок из пятидесяти, и это была бы
// самая тихая из возможных ошибок.
func TestClientRepeatsPlayerFilter(t *testing.T) {
	var gotPlayers []string
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotPlayers = append(gotPlayers, r.URL.Query()["player"]...)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBoard))
	}))
	defer srv.Close()

	// 101 игрок -> ровно 3 батча по 50; на этой арифметике держится вся квота
	keys := make([]string, 101)
	for i := range keys {
		keys[i] = strconv.Itoa(1000 + i)
	}
	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()))
	if _, err := c.Fixtures(context.Background(), keys); err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	if requests != 3 {
		t.Errorf("запросов %d, ожидалось 3 (101 игрок по 50 на батч)", requests)
	}
	if len(gotPlayers) != 101 {
		t.Errorf("до вендора доехало %d игроков из 101", len(gotPlayers))
	}
}

func TestClientNoPlayersMakesNoRequests(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()))
	page, err := c.Fixtures(context.Background(), nil)
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	if requests != 0 {
		t.Errorf("запросов %d, ожидалось 0: спрашивать не о ком", requests)
	}
	if len(page.Fixtures) != 0 {
		t.Errorf("фикстур %d, ожидалось 0", len(page.Fixtures))
	}
}

// Пагинация тратит квоту, поэтому её нельзя не заметить.
func TestClientFollowsPagination(t *testing.T) {
	var offsets []string
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offsets = append(offsets, r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		if requests == 0 {
			requests++
			_, _ = w.Write([]byte(`{"data":[],"meta":{"has_more":true}}`))
			return
		}
		requests++
		_, _ = w.Write([]byte(okBoard))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()))
	if _, err := c.PollLive(context.Background()); err != nil {
		t.Fatalf("PollLive: %v", err)
	}
	if requests != 2 {
		t.Fatalf("запросов %d, ожидалось 2", requests)
	}
	if offsets[1] != "200" {
		t.Errorf("offset второй страницы = %q, ожидалось 200", offsets[1])
	}
}

// Недобранная выдача — ОШИБКА. Иначе матчи со второй страницы выглядят как
// отсутствующие, и три таких цикла погасят их карточки.
func TestClientTruncationIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[],"meta":{"has_more":true}}`)) // всегда есть ещё
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()), WithMaxPages(2))
	_, err := c.PollLive(context.Background())
	if err == nil {
		t.Fatal("частичная выдача должна быть ошибкой, а не результатом")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("ошибка = %v", err)
	}
}

// 429 и 5xx повторяем; задержка растёт; Retry-After уважается.
func TestClientRetriesAndBacksOff(t *testing.T) {
	var slept []time.Duration
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		switch attempts {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(okBoard))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()), WithRetry(testRetry(&slept)))
	if _, err := c.PollLive(context.Background()); err != nil {
		t.Fatalf("PollLive: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("попыток %d, ожидалось 3", attempts)
	}
	if len(slept) != 2 {
		t.Fatalf("пауз %d, ожидалось 2", len(slept))
	}
	if slept[1] <= slept[0] {
		t.Errorf("задержки %v не растут — это не экспоненциальный backoff", slept)
	}
}

func TestClientHonoursRetryAfter(t *testing.T) {
	var slept []time.Duration
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBoard))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()), WithRetry(testRetry(&slept)))
	if _, err := c.PollLive(context.Background()); err != nil {
		t.Fatalf("PollLive: %v", err)
	}
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Errorf("паузы %v, ожидалась одна в 7s из Retry-After", slept)
	}
}

// 4xx НЕ повторяем: пять попыток на 401 — это 5% суточной квоты впустую.
func TestClientDoesNotRetry4xx(t *testing.T) {
	var slept []time.Duration
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()), WithRetry(testRetry(&slept)))
	if _, err := c.PollLive(context.Background()); err == nil {
		t.Fatal("401 должен быть ошибкой")
	}
	if attempts != 1 {
		t.Errorf("попыток %d, ожидалась 1: 4xx не повторяем", attempts)
	}
	if len(slept) != 0 {
		t.Errorf("пауз %d, ожидалось 0", len(slept))
	}
}

// Счётчик запросов должен видеть КАЖДУЮ попытку, иначе регулятор квоты
// недосчитывается ровно на повторах — то есть тогда, когда тратится больше всего.
func TestClientCountsEveryAttempt(t *testing.T) {
	var slept []time.Duration
	counted, attempts := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBoard))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()),
		WithRetry(testRetry(&slept)), WithOnRequest(func() { counted++ }))
	if _, err := c.PollLive(context.Background()); err != nil {
		t.Fatalf("PollLive: %v", err)
	}
	if counted != 3 {
		t.Errorf("посчитано %d запросов, вендор обслужил %d", counted, attempts)
	}
}

func TestClientRejectsNonJSONContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>502</html>"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()))
	if _, err := c.PollLive(context.Background()); err == nil {
		t.Fatal("html вместо json должен быть ошибкой, а не пустым бортом")
	}
}

func TestClientRespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()))
	_, err := c.PollLive(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ошибка = %v, ожидалась отмена контекста", err)
	}
}

func TestClientParsesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tier":"free","limits":{"per_day":100,"per_minute":30},
			"today":{"calls":9,"errors":2,"remaining_day":91}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k", WithHTTPClient(srv.Client()))
	u, err := c.Usage(context.Background())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if u.PerDay != 100 || u.RemainingDay != 91 || u.Tier != "free" {
		t.Errorf("Usage = %+v", u)
	}
}

// Клиент реализует Source: смена вендора должна быть новым файлом здесь.
var _ Source = (*Client)(nil)
