package tournaments

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
	return "tournaments"
}

func (u *Updater) Update(ctx context.Context) error {
	slog.Info("updating tournaments")

	var result int
	if err := u.pool.QueryRow(ctx, "SELECT 1").Scan(&result); err != nil {
		return err
	}

	slog.Info("tournaments update completed")
	return nil
}
