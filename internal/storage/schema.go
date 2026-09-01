package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// liveSchemaLock — ключ advisory-блокировки применения схемы. Своё простран-
// ство, отдельное от ключей джобов (см. live_ingest.go).
const liveSchemaLock = 9

// SchemaFile — один файл DDL, уже прочитанный.
type SchemaFile struct {
	Name string
	SQL  string
}

// ApplyLiveSchema применяет DDL live-ingest'а на старте сервиса.
//
// Зачем это здесь, а не psql'ом руками: канонической схемой владеет
// tennis-data-storage, но таблицами live_* владеет этот сервис, и до продовой
// базы у людей прямого доступа нет. Все четыре файла идемпотентны
// (create table if not exists + сиды с on conflict), поэтому повторный прогон
// на каждом старте ничего не меняет и ничего не теряет.
//
// Ошибка НЕ фатальна. Схема нужна только live-фиче; уронить из-за неё весь
// HTTP-сервис было бы хуже, чем отдавать 500 на одной ручке. Вызывающий
// логирует и продолжает.
func ApplyLiveSchema(ctx context.Context, pool *pgxpool.Pool, schema []SchemaFile) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	// try, а не блокирующая: два инстанса поднимаются одновременно на деплое, и
	// второму нечего ждать — работа идемпотентна, первый её уже делает.
	var locked bool
	if err := conn.QueryRow(ctx,
		`select pg_try_advisory_lock($1, $2)`, liveLockClass, liveSchemaLock).Scan(&locked); err != nil {
		return fmt.Errorf("lock: %w", err)
	}
	if !locked {
		slog.Info("live schema: another instance is applying it, skipping")
		return nil
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx,
			`select pg_advisory_unlock($1, $2)`, liveLockClass, liveSchemaLock); err != nil {
			slog.Warn("live schema: unlock failed", "error", err)
		}
	}()

	for _, f := range schema {
		started := time.Now()
		if _, err := conn.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("apply %s: %w", f.Name, err)
		}
		slog.Info("live schema applied", "file", f.Name, "took", time.Since(started))
	}
	return nil
}
