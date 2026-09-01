package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

	// lock_timeout по умолчанию БЕСКОНЕЧЕН, а DDL здесь берёт лок на matches:
	// на неё ссылаются ЧЕТЫРЕ таблицы в ДВУХ файлах (live_observations,
	// live_flags, live_events в live_ingest.sql и live_activity_sessions в
	// live_push.sql), поэтому ожидание может быть оплачено дважды. Плюс
	// live_push.sql на каждом старте берёт ACCESS EXCLUSIVE на live_events
	// своими alter table ... add column if not exists.
	//
	// Если пайплайн контента держит на matches транзакцию, старт без
	// ограничения встаёт насовсем — причём HTTP-слушатель поднимается ПОСЛЕ
	// этого вызова, то есть висит весь сервис, а не только live-фича.
	//
	// Снимается в defer: соединение уходит обратно в пул, и оставленный на нём
	// lock_timeout ронял бы по таймауту чужие запросы.
	if _, err := conn.Exec(ctx, `set lock_timeout = '3s'`); err != nil {
		return fmt.Errorf("set lock_timeout: %w", err)
	}
	defer func() {
		resetCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(resetCtx, `set lock_timeout = default`); err != nil {
			// Соединение может ещё нести на себе и GUC, и advisory-блокировку.
			// Вернуть такое в пул нельзя: так же поступает LiveLock.Release.
			slog.Warn("live schema: resetting lock_timeout failed; dropping the connection",
				"error", err)
			if raw := conn.Hijack(); raw != nil {
				closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancelClose()
				_ = raw.Close(closeCtx)
			}
		}
	}()

	// Один упавший файл НЕ отменяет остальные.
	//
	// Файлы независимы (сиды джойнятся по слагам, а не друг по другу), и все
	// идемпотентны. Прерывать набор на первой ошибке означало бы, что одна
	// чужая транзакция на matches стоит нам ВСЕЙ схемы на этот старт — включая
	// сиды, без которых Job A не находит ни одного игрока, — а повторить
	// применение до следующего деплоя нечем.
	var failures []error
	for _, f := range schema {
		if err := applyFile(ctx, conn, f); err != nil {
			slog.Error("live schema: file not applied; continuing with the rest",
				"file", f.Name, "error", err)
			failures = append(failures, fmt.Errorf("apply %s: %w", f.Name, err))
		}
	}
	return errors.Join(failures...)
}

// Отказ по lock_timeout почти всегда транзиентный: конфликтующая пачка
// пайплайна живёт секунды. Поэтому короткий повтор — он дешевле, чем сутки без
// схемы, и всё равно упирается в общий дедлайн вызывающего.
const (
	lockAttempts = 2
	lockBackoff  = 2 * time.Second
)

// applyFile применяет один файл, повторяя попытку только при 55P03
// (lock_not_available) — то есть ровно тогда, когда мы не дождались чужого
// лока. Любая другая ошибка возвращается сразу: повторять сломанный DDL смысла
// нет.
func applyFile(ctx context.Context, conn *pgxpool.Conn, f SchemaFile) error {
	var lastErr error
	for attempt := 1; attempt <= lockAttempts; attempt++ {
		started := time.Now()
		if _, err := conn.Exec(ctx, f.SQL); err == nil {
			slog.Info("live schema applied",
				"file", f.Name, "took", time.Since(started), "attempt", attempt)
			return nil
		} else {
			lastErr = err
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != pgErrLockNotAvailable {
				return err
			}
		}
		if attempt == lockAttempts {
			break
		}
		slog.Warn("live schema: timed out waiting for a lock, retrying",
			"file", f.Name, "attempt", attempt, "backoff", lockBackoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockBackoff):
		}
	}
	return lastErr
}

// pgErrLockNotAvailable — SQLSTATE 55P03, то, во что lock_timeout превращает
// ожидание. Константой, а не через отдельную зависимость на pgerrcode.
const pgErrLockNotAvailable = "55P03"
