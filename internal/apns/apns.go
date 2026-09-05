// Package apns — доставка Live Activity пушей напрямую в Apple.
//
// Напрямую, а не через FCM: у приложения есть Firebase Analytics и Crashlytics,
// но не Messaging, и маршрут через FCM добавил бы таблицу сопоставления
// registration-токенов и второй домен отказа без единого выигрыша.
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const (
	HostSandbox    = "https://api.sandbox.push.apple.com"
	HostProduction = "https://api.push.apple.com"

	// Apple отвергает токен старше часа и не любит чаще раза в 20 минут.
	tokenLifetime = 50 * time.Minute
)

// ErrUnregistered — 410. Токен мёртв, и его надо удалить: Apple считает
// повторные отправки на мёртвый токен злоупотреблением.
var ErrUnregistered = errors.New("apns: device token is no longer registered")

// ErrBadDeviceToken — 400 BadDeviceToken. Чаще всего означает не битый токен,
// а окружение: sandbox-токен ушёл на боевой хост или наоборот.
var ErrBadDeviceToken = errors.New("apns: bad device token (wrong environment?)")

// ErrProviderToken — 403: Apple не приняла наш JWT. Кэш подписи при этом
// сброшен, поэтому следующая попытка подпишет заново. Без сброса разошедшиеся
// часы или заменённый ключ роняли бы каждый пуш весь час жизни токена.
var ErrProviderToken = errors.New("apns: provider token rejected")

type Config struct {
	KeyID    string
	TeamID   string
	BundleID string
	// Содержимое .p8 (PEM, PKCS#8, ECDSA P-256).
	PrivateKeyPEM []byte
	Host          string
}

type Client struct {
	cfg  Config
	http *http.Client
	key  *ecdsa.PrivateKey
	now  func() time.Time

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

type Option func(*Client)

// WithHTTPClient подменяет транспорт. Тем же швом пользуется клиент источника:
// без него проверить подпись, ротацию и обработку 410 можно было бы только
// против настоящего Apple.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.http = hc } }

// WithClock фиксирует время — нужно, чтобы проверять ротацию токена.
func WithClock(fn func() time.Time) Option { return func(c *Client) { c.now = fn } }

func New(cfg Config, opts ...Option) (*Client, error) {
	key, err := parseP8(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, err
	}
	if cfg.Host == "" {
		cfg.Host = HostSandbox
	}
	c := &Client{
		cfg:  cfg,
		key:  key,
		now:  time.Now,
		http: &http.Client{Timeout: 15 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

func parseP8(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("apns: private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("apns: parse p8: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("apns: p8 is not an ECDSA key")
	}
	return key, nil
}

// PushType различает старт активности и всё остальное: Apple требует разные
// заголовки, и на push-to-start уходит другой топик.
type PushType string

const (
	PushStart  PushType = "start"
	PushUpdate PushType = "update"
	PushEnd    PushType = "end"
)

type Notification struct {
	Token   string
	Type    PushType
	Payload any
}

func (c *Client) Send(ctx context.Context, n Notification) error {
	body, err := json.Marshal(n.Payload)
	if err != nil {
		return err
	}
	jwt, err := c.authToken()
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.Host+"/3/device/"+n.Token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-push-type", "liveactivity")
	// Топик для Live Activity — bundle id с суффиксом, иначе Apple отвечает
	// TopicDisallowed, а сообщение об этом ничего не объясняет.
	req.Header.Set("apns-topic", c.cfg.BundleID+".push-type.liveactivity")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")
	if n.Type == PushStart {
		req.Header.Set("apns-expiration", strconv.FormatInt(c.now().Add(time.Hour).Unix(), 10))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// URL из ошибки вырезается: http.Client заворачивает всё в *url.Error,
		// а тот печатает адрес целиком — вместе с /3/device/<token>. Токен
		// устройства попадал бы в лог на каждом сетевом сбое и жил там весь
		// срок хранения; вместе с .p8 этого достаточно, чтобы слать на чужое
		// устройство. Причина сбоя во вложенной ошибке, она и остаётся.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("apns: transport: %w", urlErr.Err)
		}
		return fmt.Errorf("apns: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusGone:
		return ErrUnregistered
	}
	var apnsErr struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(respBody, &apnsErr)
	switch apnsErr.Reason {
	case "BadDeviceToken":
		return ErrBadDeviceToken
	case "Unregistered":
		return ErrUnregistered
	case "ExpiredProviderToken", "InvalidProviderToken", "MissingProviderToken":
		c.invalidateAuthToken()
		return ErrProviderToken
	}
	return fmt.Errorf("apns: status %d, reason %q", resp.StatusCode, apnsErr.Reason)
}

// authToken отдаёт кэшированный JWT, пересоздавая его раз в tokenLifetime.
// Apple отвергает токен старше часа и штрафует за создание чаще раза в 20 минут,
// поэтому подписывать на каждый пуш нельзя.
func (c *Client) authToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	if c.token != "" && now.Sub(c.tokenAt) < tokenLifetime {
		return c.token, nil
	}

	header := b64(fmt.Sprintf(`{"alg":"ES256","kid":%q}`, c.cfg.KeyID))
	claims := b64(fmt.Sprintf(`{"iss":%q,"iat":%d}`, c.cfg.TeamID, now.Unix()))
	signing := header + "." + claims

	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.key, digest[:])
	if err != nil {
		return "", err
	}
	// ES256 требует фиксированные 32 байта на каждую половину, а не ASN.1:
	// big.Int.Bytes() обрезает ведущие нули, и подпись изредка оказывалась бы
	// короче — Apple такие молча отвергает.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])

	c.token = signing + "." + base64.RawURLEncoding.EncodeToString(sig)
	c.tokenAt = now
	return c.token, nil
}

func (c *Client) invalidateAuthToken() {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
