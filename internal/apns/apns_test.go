package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testKeyPEM(t *testing.T) ([]byte, *ecdsa.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), &key.PublicKey
}

func testClient(t *testing.T, srv *httptest.Server, opts ...Option) (*Client, *ecdsa.PublicKey) {
	t.Helper()
	keyPEM, pub := testKeyPEM(t)
	opts = append([]Option{WithHTTPClient(srv.Client())}, opts...)
	c, err := New(Config{
		KeyID: "KEY123", TeamID: "TEAM456", BundleID: "com.example.tennis",
		PrivateKeyPEM: keyPEM, Host: srv.URL,
	}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c, pub
}

// Подпись должна проверяться публичным ключом. Apple молча отвергает неверную,
// поэтому единственный способ узнать об ошибке до продакшна — проверить здесь.
func TestSendSignsVerifiableES256(t *testing.T) {
	var gotAuth, gotTopic, gotType, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotTopic = r.Header.Get("apns-topic")
		gotType = r.Header.Get("apns-push-type")
		gotPath = r.URL.Path
	}))
	defer srv.Close()

	c, pub := testClient(t, srv)
	if err := c.Send(context.Background(), Notification{
		Token: "devicetoken1", Type: PushStart, Payload: map[string]any{"aps": map[string]any{}},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if gotPath != "/3/device/devicetoken1" {
		t.Errorf("путь = %q", gotPath)
	}
	if gotType != "liveactivity" {
		t.Errorf("apns-push-type = %q", gotType)
	}
	if gotTopic != "com.example.tennis.push-type.liveactivity" {
		t.Errorf("apns-topic = %q: Live Activity требует суффикс, иначе TopicDisallowed", gotTopic)
	}

	jwt := strings.TrimPrefix(gotAuth, "bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT из %d частей", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 64 {
		t.Fatalf("подпись %d байт, ES256 требует ровно 64: усечённые ведущие нули "+
			"Apple отвергает молча", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		t.Fatal("подпись не проверяется публичным ключом")
	}

	var header struct {
		Alg, Kid string
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(raw, &header)
	if header.Alg != "ES256" || header.Kid != "KEY123" {
		t.Errorf("заголовок = %+v", header)
	}
}

// Токен переиспользуется: Apple отвергает старше часа и штрафует за создание
// чаще раза в 20 минут, так что подписывать на каждый пуш нельзя.
func TestAuthTokenIsCachedAndRotated(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("authorization"))
	}))
	defer srv.Close()

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	c, _ := testClient(t, srv, WithClock(func() time.Time { return now }))

	send := func() {
		if err := c.Send(context.Background(), Notification{Token: "t", Type: PushUpdate}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	send()
	send()
	if seen[0] != seen[1] {
		t.Fatal("токен пересоздан на втором пуше: Apple штрафует за частое создание")
	}

	now = now.Add(tokenLifetime + time.Minute)
	send()
	if seen[2] == seen[0] {
		t.Fatal("токен не обновился по истечении срока: Apple отвергнет его через час")
	}
}

// 410 обязан быть распознаваемым: на мёртвый токен нельзя слать повторно,
// Apple считает это злоупотреблением.
func TestSendMapsErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"410 Gone", http.StatusGone, `{"reason":"Unregistered"}`, ErrUnregistered},
		{"400 Unregistered", http.StatusBadRequest, `{"reason":"Unregistered"}`, ErrUnregistered},
		{"400 BadDeviceToken", http.StatusBadRequest, `{"reason":"BadDeviceToken"}`, ErrBadDeviceToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c, _ := testClient(t, srv)
			err := c.Send(context.Background(), Notification{Token: "t", Type: PushEnd})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, ожидалось %v", err, tc.want)
			}
		})
	}

	t.Run("прочие ошибки не глотаются", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"reason":"ServiceUnavailable"}`))
		}))
		defer srv.Close()
		c, _ := testClient(t, srv)
		err := c.Send(context.Background(), Notification{Token: "t", Type: PushEnd})
		if err == nil || errors.Is(err, ErrUnregistered) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNewRejectsBadKey(t *testing.T) {
	for name, key := range map[string][]byte{
		"не PEM":   []byte("nonsense"),
		"пустой":   nil,
		"не ECDSA": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(Config{PrivateKeyPEM: key}); err == nil {
				t.Fatal("кривой ключ должен отвергаться при создании, а не при первой отправке")
			}
		})
	}
}
