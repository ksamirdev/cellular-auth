package cell

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/ksamirdev/cellular-auth/internal/api"
	"github.com/ksamirdev/cellular-auth/internal/platform"
)

type handler struct {
	store     *Store
	db        healthCheck
	cellID    string
	log       *zap.Logger
	decisions *prometheus.CounterVec
}

func NewHandler(store *Store, db healthCheck, cfg Config, log *zap.Logger, reg *prometheus.Registry) http.Handler {
	h := &handler{
		store:  store,
		db:     db,
		cellID: cfg.ID,
		log:    log.With(zap.String("cell", cfg.ID)),
		decisions: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "cell_authorizations_total",
			Help: "Authorization decisions made by this cell.",
		}, []string{"decision", "mode", "reason"}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/authorizations", h.authorize)
	mux.HandleFunc("POST /v1/accounts", h.openAccount)
	mux.HandleFunc("GET /internal/outbox", h.outbox)
	mux.HandleFunc("GET /internal/cursors/{source}", h.cursor)
	mux.HandleFunc("POST /internal/apply", h.apply)
	mux.HandleFunc("GET /healthz", h.health)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return mux
}

func (h *handler) authorize(w http.ResponseWriter, r *http.Request) {
	var req api.AuthorizeRequest
	if err := platform.DecodeJSON(w, r, &req); err != nil {
		platform.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.TxnID == "" || req.AccountID == "" || req.AmountMinor <= 0 {
		platform.WriteError(w, http.StatusBadRequest, "txn_id, account_id and a positive amount_minor are required")
		return
	}

	resp, err := h.store.Authorize(r.Context(), req)
	switch {
	case errors.Is(err, ErrCommitUnknown):
		h.log.Warn("authorize: outcome unknown", zap.String("txn_id", req.TxnID), zap.Error(err))
		platform.WriteError(w, http.StatusInternalServerError, "outcome unknown")
	case err != nil:
		// Nothing was written, so the router may safely try another cell.
		h.log.Warn("authorize: not processed", zap.String("txn_id", req.TxnID), zap.Error(err))
		w.Header().Set(api.HeaderReroute, "1")
		platform.WriteError(w, http.StatusServiceUnavailable, "not processed")
	default:
		h.decisions.WithLabelValues(string(resp.Decision), string(resp.Mode), resp.Reason).Inc()
		platform.WriteJSON(w, http.StatusOK, resp)
	}
}

func (h *handler) openAccount(w http.ResponseWriter, r *http.Request) {
	var req api.OpenAccountRequest
	if err := platform.DecodeJSON(w, r, &req); err != nil {
		platform.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	home, backup := r.Header.Get(api.HeaderHomeCell), r.Header.Get(api.HeaderBackupCell)
	if req.AccountID == "" || req.CreditLimitMinor < 0 || home == "" || backup == "" {
		platform.WriteError(w, http.StatusBadRequest, "account_id, credit_limit_minor and owner headers are required")
		return
	}
	switch err := h.store.OpenAccount(r.Context(), req, home, backup); {
	case errors.Is(err, ErrNotHome):
		platform.WriteError(w, http.StatusConflict, err.Error())
	case err != nil:
		h.log.Error("open account", zap.Error(err))
		platform.WriteError(w, http.StatusServiceUnavailable, "unavailable")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *handler) outbox(w http.ResponseWriter, r *http.Request) {
	after, limit, err := parseOutboxQuery(r)
	if err != nil {
		platform.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}

	entries, err := h.store.ReadOutbox(r.Context(), after, limit)
	if err != nil {
		h.log.Error("read outbox", zap.Error(err))
		platform.WriteError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	platform.WriteJSON(w, http.StatusOK, api.OutboxPage{Entries: entries})
}

func (h *handler) cursor(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.Cursor(r.Context(), r.PathValue("source"))
	if err != nil {
		h.log.Error("read cursor", zap.Error(err))
		platform.WriteError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	platform.WriteJSON(w, http.StatusOK, p)
}

func (h *handler) apply(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyRequest
	if err := platform.DecodeJSON(w, r, &req); err != nil {
		platform.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Source == "" || req.Source == h.cellID {
		platform.WriteError(w, http.StatusBadRequest, "source must name another cell")
		return
	}
	if err := h.store.Apply(r.Context(), req.Source, req.Entries); err != nil {
		h.log.Error("apply", zap.String("source", req.Source), zap.Error(err))
		platform.WriteError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	if err := h.db.Ping(ctx); err != nil {
		platform.WriteError(w, http.StatusServiceUnavailable, "database unreachable")
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"cell": h.cellID})
}

func parseOutboxQuery(r *http.Request) (api.Position, int, error) {
	q := r.URL.Query()
	after, limit := startPosition, 500
	if v := q.Get("after_txid"); v != "" {
		if _, err := strconv.ParseUint(v, 10, 64); err != nil {
			return after, 0, errors.New("after_txid must be an unsigned integer")
		}
		after.TxID = v
	}
	if v := q.Get("after_seq"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return after, 0, errors.New("after_seq must be an integer")
		}
		after.Seq = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			return after, 0, errors.New("limit must be between 1 and 1000")
		}
		limit = n
	}
	return after, limit, nil
}
