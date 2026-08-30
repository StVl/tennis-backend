package livesource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// Максимум строк на страницу у вендора.
	pageLimit = 200
	// Максимум id в повторяемом фильтре player= — документированный предел
	// вендора. Ради него мы и ходим в /matches, а не в /fixtures: 101
	// отслеживаемый игрок укладывается в 3 запроса.
	playerBatchSize = 50
	// Потолок тела ответа. Railway-контейнер небольшой, и битый ответ не должен
	// уметь его положить.
	maxBodyBytes = 8 << 20
)

// Client — клиент Live Tennis API. Реализует Source.
type Client struct {
	baseURL   string
	apiKey    string
	http      *http.Client
	onRequest func()
	retry     RetryPolicy
	// Максимум страниц на один логический опрос. Частичная выдача — это
	// ОШИБКА, а не «сколько успели»: недобранная страница выглядит как
	// отсутствие матчей, и три таких цикла погасят их карточки.
	maxPages int
}

// RetryPolicy вынесена наружу целиком ради тестов: без инъекции Sleep проверка
// повторов занимала бы секунды реального времени, а такие тесты удаляют.
type RetryPolicy struct {
	Max    int
	Base   time.Duration
	Jitter func(time.Duration) time.Duration
	Sleep  func(context.Context, time.Duration) error
}

func defaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		Max:  3,
		Base: 500 * time.Millisecond,
		// Глобальный источник math/rand, а не фиксированное семя: разброс нужен
		// ровно для того, чтобы инстансы и повторы не попадали в такт, а
		// постоянное семя давало бы у всех одинаковую последовательность
		// задержек — то есть ровно такт. Детерминизм для тестов не нужен: они
		// подставляют свою RetryPolicy через WithRetry.
		Jitter: func(d time.Duration) time.Duration {
			return d + time.Duration(rand.Int63n(int64(d/2)+1))
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
}

type Option func(*Client)

func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.http = hc } }
func WithRetry(rp RetryPolicy) Option       { return func(c *Client) { c.retry = rp } }
func WithMaxPages(n int) Option             { return func(c *Client) { c.maxPages = n } }

// WithOnRequest ставит счётчик запросов. Он срабатывает перед КАЖДОЙ попыткой,
// включая повторы и ленивый резолвер игроков. Если считать запросы в цикле
// опроса, а не здесь, повторы и резолвер окажутся невидимы для регулятора
// квоты — и один турнир категории Challenger съест дневной лимит.
func WithOnRequest(fn func()) Option { return func(c *Client) { c.onRequest = fn } }

func NewClient(baseURL, apiKey string, opts ...Option) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// Явный таймаут: у http.DefaultClient его нет вовсе, и запрос мог бы
		// пережить весь интервал между тиками.
		http:      &http.Client{Timeout: 20 * time.Second},
		onRequest: func() {},
		retry:     defaultRetryPolicy(),
		maxPages:  2,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) Name() string { return SourceName }

// PollLive — весь live-борт. Один запрос независимо от числа турниров: ради
// этого источник и выбран.
func (c *Client) PollLive(ctx context.Context) (Board, error) {
	var out Board
	for page := 0; ; page++ {
		if page >= c.maxPages {
			return out, fmt.Errorf("live board truncated after %d pages: "+
				"partial board is a failure, not a result", c.maxPages)
		}
		body, err := c.get(ctx, "/matches", url.Values{
			"status": {"live"},
			"limit":  {strconv.Itoa(pageLimit)},
			"offset": {strconv.Itoa(page * pageLimit)},
		})
		if err != nil {
			return Board{}, err
		}
		b, err := ParseBoard(body)
		if err != nil {
			return Board{}, err
		}
		out.Observations = append(out.Observations, b.Observations...)
		out.RowsParsed += b.RowsParsed
		out.RowsDoubles += b.RowsDoubles
		out.RowsUnusable += b.RowsUnusable
		out.UnknownEventStatuses = append(out.UnknownEventStatuses, b.UnknownEventStatuses...)
		if !b.HasMore {
			return out, nil
		}
	}
}

// Fixtures — предстоящие матчи заданных игроков, батчами по playerBatchSize.
// Пустой список игроков означает пустой результат и НОЛЬ запросов: спрашивать
// вендора не о ком.
//
// При ошибке возвращает УЖЕ НАБРАННОЕ вместе с ошибкой — в отличие от PollLive,
// который частичный результат выбрасывает. Асимметрия намеренная: лишняя
// фикстура только расширяет окно наблюдения (безопасная сторона), а недобранный
// live-борт делает матчи похожими на отсутствующие и гасит их карточки.
func (c *Client) Fixtures(ctx context.Context, playerKeys []string) (FixturePage, error) {
	var out FixturePage
	for start := 0; start < len(playerKeys); start += playerBatchSize {
		end := min(start+playerBatchSize, len(playerKeys))
		batch := playerKeys[start:end]

		for page := 0; ; page++ {
			if page >= c.maxPages {
				return out, fmt.Errorf("fixtures truncated after %d pages", c.maxPages)
			}
			q := url.Values{
				"status": {"upcoming"},
				"limit":  {strconv.Itoa(pageLimit)},
				"offset": {strconv.Itoa(page * pageLimit)},
			}
			// повторяемый параметр: player=34&player=13&...
			for _, key := range batch {
				q.Add("player", key)
			}
			body, err := c.get(ctx, "/matches", q)
			if err != nil {
				return out, err
			}
			p, err := ParseFixtures(body)
			if err != nil {
				return out, err
			}
			out.Fixtures = append(out.Fixtures, p.Fixtures...)
			out.RowsParsed += p.RowsParsed
			out.RowsDoubles += p.RowsDoubles
			out.RowsCancelled += p.RowsCancelled
			out.RowsUnusable += p.RowsUnusable
			if !p.HasMore {
				break
			}
		}
	}
	return out, nil
}

// Player — карточка игрока. Один запрос из общей квоты, поэтому вызывающий
// обязан сам ограничивать частоту (см. ленивый резолвер).
func (c *Client) Player(ctx context.Context, key string) (VendorPlayer, error) {
	body, err := c.get(ctx, "/players/"+url.PathEscape(key), nil)
	if err != nil {
		return VendorPlayer{}, err
	}
	var wire struct {
		ID   flexKey `json:"id"`
		Name string  `json:"name"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return VendorPlayer{}, fmt.Errorf("parse player: %w", err)
	}
	return VendorPlayer{Key: string(wire.ID), Name: wire.Name}, nil
}

// Usage — остаток суточной квоты по данным САМОГО вендора.
//
// Наша арифметика исходит из того, что сутки квоты сбрасываются в полночь UTC.
// Если у вендора иначе, регулятор часть суток считает неверно и мы упираемся
// в 429 в предсказуемое время. Этот вызов — сверка с истиной, и он бесплатен
// на free-тарифе.
type Usage struct {
	Tier         string
	PerDay       int
	PerMinute    int
	CallsToday   int
	RemainingDay int
}

func (c *Client) Usage(ctx context.Context) (Usage, error) {
	body, err := c.get(ctx, "/usage", nil)
	if err != nil {
		return Usage{}, err
	}
	var wire struct {
		Tier   string `json:"tier"`
		Limits struct {
			PerDay    int `json:"per_day"`
			PerMinute int `json:"per_minute"`
		} `json:"limits"`
		Today struct {
			Calls        int `json:"calls"`
			RemainingDay int `json:"remaining_day"`
		} `json:"today"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return Usage{}, fmt.Errorf("parse usage: %w", err)
	}
	return Usage{
		Tier: wire.Tier, PerDay: wire.Limits.PerDay, PerMinute: wire.Limits.PerMinute,
		CallsToday: wire.Today.Calls, RemainingDay: wire.Today.RemainingDay,
	}, nil
}

// get выполняет запрос с повторами. Возвращает сырое тело: разбор отделён от
// сети, поэтому тесты разбора не нуждаются в клиенте, а тесты клиента — в парсере.
func (c *Client) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	target := c.baseURL + path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}

	var lastErr error
	for attempt := 0; attempt <= c.retry.Max; attempt++ {
		if attempt > 0 {
			delay := c.retry.Base << (attempt - 1)
			if retryAfter, ok := lastErr.(*retryAfterError); ok && retryAfter.after > 0 {
				delay = retryAfter.after
			} else if c.retry.Jitter != nil {
				delay = c.retry.Jitter(delay)
			}
			if err := c.retry.Sleep(ctx, delay); err != nil {
				return nil, err
			}
		}

		body, err := c.attempt(ctx, target)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", c.retry.Max+1, lastErr)
}

func (c *Client) attempt(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	// перед КАЖДОЙ попыткой, а не перед логическим запросом
	c.onRequest()

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &transportError{err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &retryAfterError{status: resp.StatusCode, after: parseRetryAfter(resp)}
	}
	if resp.StatusCode >= 500 {
		return nil, &retryAfterError{status: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		// 4xx повторять нельзя: пять попыток на 401 — это 5% суточной квоты
		// на заведомо безнадёжный запрос.
		return nil, fmt.Errorf("livetennisapi: unexpected status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return nil, fmt.Errorf("livetennisapi: unexpected content-type %q", ct)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, &transportError{err: err}
	}
	return body, nil
}

type transportError struct{ err error }

func (e *transportError) Error() string { return "livetennisapi transport: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

type retryAfterError struct {
	status int
	after  time.Duration
}

func (e *retryAfterError) Error() string {
	return fmt.Sprintf("livetennisapi: status %d", e.status)
}

func isRetryable(err error) bool {
	switch err.(type) {
	case *transportError, *retryAfterError:
		return true
	}
	return false
}

func parseRetryAfter(resp *http.Response) time.Duration {
	raw := resp.Header.Get("Retry-After")
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}
