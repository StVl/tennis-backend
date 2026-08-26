package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPPort        string
	DatabaseURL     string
	TournamentsCron string
	PlayersCron     string
	UpdateTimeout   time.Duration
	DBMaxConns      int32
	DevEndpoints    bool
	LiveMatchesCap  int
}

func Load() (*Config, error) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}

	httpPort := envOrDefault("HTTP_PORT", "8080")
	tournamentsCron := envOrDefault("TOURNAMENTS_CRON", "*/30 * * * *")
	playersCron := envOrDefault("PLAYERS_CRON", "0 */2 * * *")

	updateTimeout, err := parseDuration(envOrDefault("UPDATE_TIMEOUT", "5m"))
	if err != nil {
		return nil, fmt.Errorf("parse UPDATE_TIMEOUT: %w", err)
	}

	dbMaxConns, err := parseInt32(envOrDefault("DB_MAX_CONNS", "10"))
	if err != nil {
		return nil, fmt.Errorf("parse DB_MAX_CONNS: %w", err)
	}

	// Потолок одновременных Live Activity у клиента. Константа была бы неверна:
	// лимит задаёт клиент, и менять его придётся без релиза бэкенда.
	liveMatchesCap, err := strconv.Atoi(envOrDefault("LIVE_MATCHES_CAP", "5"))
	if err != nil || liveMatchesCap < 1 {
		return nil, fmt.Errorf("LIVE_MATCHES_CAP must be a positive integer")
	}

	return &Config{
		HTTPPort:        httpPort,
		DatabaseURL:     databaseURL,
		TournamentsCron: tournamentsCron,
		PlayersCron:     playersCron,
		UpdateTimeout:   updateTimeout,
		DBMaxConns:      dbMaxConns,
		DevEndpoints:    envBool("DEV_ENDPOINTS_ENABLED", false),
		LiveMatchesCap:  liveMatchesCap,
	}, nil
}

// envBool читает булев флаг. Кривое значение НЕ роняет процесс: Load() возвращает
// ошибку, а на ошибку Load main вызывает os.Exit(1) — то есть опечатка во флаге
// поллера уронила бы весь HTTP-сервис. Логируем и берём default.
func envBool(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		slog.Warn("malformed boolean env var, using default",
			"key", key, "value", raw, "default", fallback)
		return fallback
	}
	return parsed
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func parseDuration(value string) (time.Duration, error) {
	return time.ParseDuration(value)
}

func parseInt32(value string) (int32, error) {
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(parsed), nil
}
