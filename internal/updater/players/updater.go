package players

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Updater struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Updater {
	return &Updater{pool: pool}
}

func (u *Updater) Name() string {
	return "players"
}

func (u *Updater) Update(ctx context.Context) error {
	slog.Info("updating players")

	var result int
	if err := u.pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
		return err
	}

	slog.Info("players update completed")
	return nil
}
