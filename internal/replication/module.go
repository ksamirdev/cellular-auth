package replication

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/platform"
)

var Module = fx.Module("replication",
	fx.Provide(
		func(c Config) platform.ListenAddr { return platform.ListenAddr(c.Addr) },
		func(reg *prometheus.Registry) http.Handler {
			mux := http.NewServeMux()
			mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
			return mux
		},
	),
	fx.Invoke(New),
)
