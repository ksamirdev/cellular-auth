// Package cell is one independent failure domain: it owns its database,
// decides authorizations for the accounts homed on it, and stands in for the
// accounts it backs up when their home cell is unreachable.
package cell

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/platform"
)

type Config struct {
	ID          string `env:"CELL_ID,required"`
	Addr        string `env:"CELL_ADDR" envDefault:":8101"`
	DatabaseURL string `env:"DATABASE_URL,required"`
	DBMaxConns  int32  `env:"DB_MAX_CONNS" envDefault:"32"`
	// StandInCapMinor bounds how much a backup cell may approve for a single
	// account before the home cell has acknowledged those approvals.
	StandInCapMinor int64 `env:"STANDIN_CAP_MINOR" envDefault:"50000"`
}

//go:embed migrations/*.sql
var migrations embed.FS

var Module = fx.Module("cell",
	fx.Provide(
		func(lc fx.Lifecycle, c Config) (*pgxpool.Pool, error) {
			return platform.NewPool(lc, c.DatabaseURL, c.DBMaxConns)
		},
		newHealthCheck,
		func(c Config) platform.ListenAddr { return platform.ListenAddr(c.Addr) },
		NewStore,
		NewHandler,
	),
	fx.Invoke(migrate),
)

func migrate(lc fx.Lifecycle, pool *pgxpool.Pool) {
	lc.Append(fx.StartHook(func(ctx context.Context) error {
		db := stdlib.OpenDBFromPool(pool)
		defer db.Close()
		return runMigrations(ctx, db)
	}))
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, dir, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// healthCheck pings the database over its own single connection. Sharing the
// traffic pool made health depend on load: when a neighbour failed and its
// traffic moved here, pings queued behind requests, timed out, and the router
// ejected this healthy cell too.
type healthCheck struct{ pool *pgxpool.Pool }

func newHealthCheck(lc fx.Lifecycle, c Config) (healthCheck, error) {
	p, err := platform.NewPool(lc, c.DatabaseURL, 1)
	return healthCheck{p}, err
}

func (h healthCheck) Ping(ctx context.Context) error { return h.pool.Ping(ctx) }
