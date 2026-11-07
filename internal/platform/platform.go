// Package platform wires the process-level pieces every binary needs:
// logging, a metrics registry, the HTTP server and the Postgres pool.
package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/zap"
)

// ListenAddr is the address the process's HTTP server binds to.
type ListenAddr string

// Module must be passed to fx.New directly: fx.WithLogger only applies at the
// root of the graph.
var Module = fx.Options(
	fx.Provide(newLogger, newRegistry),
	fx.Invoke(runHTTPServer),
	fx.WithLogger(func(l *zap.Logger) fxevent.Logger {
		return &fxevent.ZapLogger{Logger: l.WithOptions(zap.IncreaseLevel(zap.WarnLevel))}
	}),
)

func newLogger() (*zap.Logger, error) {
	if os.Getenv("LOG_FORMAT") == "console" {
		return zap.NewDevelopment()
	}
	return zap.NewProduction()
}

// newRegistry returns a registry per process instead of the global default, so
// several cells can run in one test binary without colliding.
func newRegistry() *prometheus.Registry {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return r
}

func runHTTPServer(lc fx.Lifecycle, addr ListenAddr, h http.Handler, log *zap.Logger) {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ln, err := net.Listen("tcp", string(addr))
			if err != nil {
				return fmt.Errorf("listen %s: %w", addr, err)
			}
			log.Info("http listening", zap.String("addr", ln.Addr().String()))
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http server", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: srv.Shutdown,
	})
}

// NewPool opens a pool closed on stop. maxConns 0 keeps the pgx default.
func NewPool(lc fx.Lifecycle, dsn string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.StopHook(pool.Close))
	return pool, nil
}
