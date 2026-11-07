// Package router is the global transaction router: the only way into the
// cells. It picks a transaction's cell from its routing key, moves traffic to
// the backup cell when the home cell fails, and lets operators drain cells.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/ksamirdev/cellular-auth/internal/api"
	"github.com/ksamirdev/cellular-auth/internal/platform"
)

type Config struct {
	Addr           string            `env:"ROUTER_ADDR" envDefault:":8080"`
	Cells          map[string]string `env:"CELLS,required" envKeyValSeparator:"="`
	AttemptTimeout time.Duration     `env:"ATTEMPT_TIMEOUT" envDefault:"800ms"`
	HealthInterval time.Duration     `env:"HEALTH_INTERVAL" envDefault:"250ms"`
	HealthTimeout  time.Duration     `env:"HEALTH_TIMEOUT" envDefault:"200ms"`
}

type attemptResult string

const (
	resultFinal        attemptResult = "final"
	resultNotProcessed attemptResult = "not_processed"
	resultAmbiguous    attemptResult = "ambiguous"
	resultUnreachable  attemptResult = "unreachable"
)

type Router struct {
	cells    *Cells
	client   *http.Client
	timeout  time.Duration
	log      *zap.Logger
	attempts *prometheus.CounterVec
	served   *prometheus.HistogramVec
}

func New(cfg Config, cells *Cells, log *zap.Logger, reg *prometheus.Registry) *Router {
	f := promauto.With(reg)
	return &Router{
		cells: cells,
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 512,
			IdleConnTimeout:     90 * time.Second,
		}},
		timeout: cfg.AttemptTimeout,
		log:     log,
		attempts: f.NewCounterVec(prometheus.CounterOpts{
			Name: "router_attempts_total",
			Help: "Requests forwarded to cells, by how the attempt ended.",
		}, []string{"cell", "role", "result"}),
		served: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "router_authorization_seconds",
			Help:    "End-to-end authorization latency at the router.",
			Buckets: prometheus.ExponentialBuckets(0.0005, 2, 14),
		}, []string{"mode", "decision"}),
	}
}

func NewHandler(r *Router, reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/authorizations", r.authorize)
	mux.HandleFunc("POST /v1/accounts", r.openAccount)
	mux.HandleFunc("GET /admin/cells", r.listCells)
	mux.HandleFunc("PUT /admin/cells/{id}/drain", r.drain(true))
	mux.HandleFunc("DELETE /admin/cells/{id}/drain", r.drain(false))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}

// authorize tries the account's home cell, then its backup. No other cell
// can help: only the backup holds a replica of the account.
//
// Moving on after a not-processed or unreachable attempt is always safe. After
// an ambiguous attempt the home cell may already have approved; the backup
// may then approve the same txn_id again, and reconciliation at the home cell
// collapses the two into one hold.
func (r *Router) authorize(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	var body api.AuthorizeRequest
	if err := platform.DecodeJSON(w, req, &body); err != nil || body.AccountID == "" {
		platform.WriteError(w, http.StatusBadRequest, "valid json with account_id is required")
		return
	}
	raw, _ := json.Marshal(body)
	owners := Owners(r.cells.IDs(), body.AccountID)[:2]

	for i, cell := range owners {
		role := [...]string{"home", "backup"}[i]
		if !r.cells.Available(cell) {
			r.attempts.WithLabelValues(cell, role, "skipped").Inc()
			continue
		}
		resp, result := r.send(req.Context(), cell, "/v1/authorizations", raw, nil)
		r.attempts.WithLabelValues(cell, role, string(result)).Inc()
		if result != resultFinal {
			continue
		}
		var decided api.AuthorizeResponse
		_ = json.Unmarshal(resp.body, &decided)
		r.served.WithLabelValues(string(decided.Mode), string(decided.Decision)).Observe(time.Since(start).Seconds())
		resp.writeTo(w, cell)
		return
	}
	r.served.WithLabelValues("none", "unavailable").Observe(time.Since(start).Seconds())
	platform.WriteError(w, http.StatusServiceUnavailable, api.ReasonNoCell)
}

// openAccount only goes to the home cell. The backup learns about the account
// through replication; opening is not worth standing in for.
func (r *Router) openAccount(w http.ResponseWriter, req *http.Request) {
	var body api.OpenAccountRequest
	if err := platform.DecodeJSON(w, req, &body); err != nil || body.AccountID == "" {
		platform.WriteError(w, http.StatusBadRequest, "valid json with account_id is required")
		return
	}
	raw, _ := json.Marshal(body)
	owners := Owners(r.cells.IDs(), body.AccountID)
	headers := http.Header{api.HeaderHomeCell: {owners[0]}, api.HeaderBackupCell: {owners[1]}}

	if resp, result := r.send(req.Context(), owners[0], "/v1/accounts", raw, headers); result == resultFinal {
		resp.writeTo(w, owners[0])
		return
	}
	platform.WriteError(w, http.StatusServiceUnavailable, "home cell unavailable")
}

type cellResponse struct {
	status int
	header http.Header
	body   []byte
}

func (c cellResponse) writeTo(w http.ResponseWriter, cell string) {
	if ct := c.header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set(api.HeaderServedBy, cell)
	w.WriteHeader(c.status)
	_, _ = w.Write(c.body)
}

func (r *Router) send(ctx context.Context, cell, path string, body []byte, h http.Header) (cellResponse, attemptResult) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.cells.URL(cell)+path, bytes.NewReader(body))
	req.Header = h.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			r.cells.MarkDown(cell)
			return cellResponse{}, resultUnreachable
		}
		r.log.Warn("cell attempt failed", zap.String("cell", cell), zap.Error(err))
		return cellResponse{}, resultAmbiguous
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	switch {
	case err != nil:
		return cellResponse{}, resultAmbiguous
	case resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get(api.HeaderReroute) != "":
		return cellResponse{}, resultNotProcessed
	case resp.StatusCode >= 500:
		return cellResponse{}, resultAmbiguous
	}
	return cellResponse{status: resp.StatusCode, header: resp.Header, body: b}, resultFinal
}

func (r *Router) listCells(w http.ResponseWriter, _ *http.Request) {
	platform.WriteJSON(w, http.StatusOK, r.cells.Status())
}

func (r *Router) drain(drained bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !r.cells.SetDrained(req.PathValue("id"), drained) {
			platform.WriteError(w, http.StatusNotFound, "unknown cell")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
