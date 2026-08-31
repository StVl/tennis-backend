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
	// Применять DDL live-ingest'а на старте. По умолчанию ДА: таблицами live_*
	// владеет этот сервис, файлы идемпотентны, а прямого доступа к продовой
	// базе у людей нет.
	ApplyLiveSchema  bool
	LiveMatchesLimit int
	Live             LiveConfig
	Push             PushConfig
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

	live, err := loadLive()
	if err != nil {
		return nil, err
	}

	push, err := loadPush()
	if err != nil {
		return nil, err
	}

	return &Config{
		HTTPPort:         httpPort,
		DatabaseURL:      databaseURL,
		TournamentsCron:  tournamentsCron,
		PlayersCron:      playersCron,
		UpdateTimeout:    updateTimeout,
		DBMaxConns:       dbMaxConns,
		DevEndpoints:     envBool("DEV_ENDPOINTS_ENABLED", false),
		ApplyLiveSchema:  envBool("LIVE_SCHEMA_AUTO_APPLY", true),
		LiveMatchesLimit: envInt("LIVE_MATCHES_LIMIT", 50),
		Live:             live,
		Push:             push,
	}, nil
}

// Правило для необязательных настроек: кривое значение НЕ роняет процесс.
// Load() возвращает ошибку, а на ошибку Load main вызывает os.Exit(1) — то есть
// опечатка в тюнинге поллера уронила бы весь HTTP-сервис. Обязательное
// (DATABASE_URL) по-прежнему падает; тюнинг логируется и берёт default.

// envBool читает булев флаг.
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

// envInt читает целое положительное. Ноль и отрицательные считаются кривым
// значением: лимит выдачи в 0 записей — это не настройка, а поломка.
func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		slog.Warn("malformed integer env var, using default",
			"key", key, "value", raw, "default", fallback)
		return fallback
	}
	return parsed
}

// envDuration — как envInt: кривое значение логируется и берётся default.
func envDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		slog.Warn("malformed duration env var, using default",
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
