// Package replication ships each cell's outbox to every other cell, outside
// the transaction path. It keeps no state of its own: each destination cell
// stores how far it has applied each source, so the replicator can crash, be
// restarted or run twice without losing or double-applying events.
package replication

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/ksamirdev/cellular-auth/internal/api"
)

type Config struct {
	Addr      string            `env:"REPLICATOR_ADDR" envDefault:":8090"`
	Cells     map[string]string `env:"CELLS,required" envKeyValSeparator:"="`
	Interval  time.Duration     `env:"REPLICATION_INTERVAL" envDefault:"100ms"`
	BatchSize int               `env:"REPLICATION_BATCH" envDefault:"500"`
}

type Replicator struct {
	cfg     Config
	client  *http.Client
	log     *zap.Logger
	applied *prometheus.CounterVec
}

func New(lc fx.Lifecycle, cfg Config, log *zap.Logger, reg *prometheus.Registry) *Replicator {
	r := &Replicator{
		cfg:    cfg,
		client: &http.Client{Timeout: 2 * time.Second},
		log:    log,
		applied: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "replication_events_total",
			Help: "Outbox entries shipped from one cell to another.",
		}, []string{"source", "dest"}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			for _, src := range slices.Sorted(maps.Keys(cfg.Cells)) {
				for _, dst := range slices.Sorted(maps.Keys(cfg.Cells)) {
					if src != dst {
						wg.Go(func() { r.run(ctx, src, dst) })
					}
				}
			}
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			wg.Wait()
			return nil
		},
	})
	return r
}

// run ships src's outbox to dst until ctx ends. It drains backlog without
// pausing and backs off to the poll interval once caught up or on error.
func (r *Replicator) run(ctx context.Context, src, dst string) {
	log := r.log.With(zap.String("source", src), zap.String("dest", dst))
	failing := false
	for {
		n, err := r.step(ctx, src, dst)
		switch {
		case err != nil && !failing:
			log.Warn("replication stalled", zap.Error(err))
			failing = true
		case err == nil && failing:
			log.Info("replication resumed")
			failing = false
		}
		if err == nil && n == r.cfg.BatchSize {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.cfg.Interval):
		}
	}
}

func (r *Replicator) step(ctx context.Context, src, dst string) (int, error) {
	var pos api.Position
	if err := r.get(ctx, r.cfg.Cells[dst]+"/internal/cursors/"+url.PathEscape(src), &pos); err != nil {
		return 0, fmt.Errorf("read cursor: %w", err)
	}
	q := url.Values{
		"after_txid": {pos.TxID},
		"after_seq":  {strconv.FormatInt(pos.Seq, 10)},
		"limit":      {strconv.Itoa(r.cfg.BatchSize)},
	}
	var page api.OutboxPage
	if err := r.get(ctx, r.cfg.Cells[src]+"/internal/outbox?"+q.Encode(), &page); err != nil {
		return 0, fmt.Errorf("read outbox: %w", err)
	}
	if len(page.Entries) == 0 {
		return 0, nil
	}
	body, _ := json.Marshal(api.ApplyRequest{Source: src, Entries: page.Entries})
	if err := r.post(ctx, r.cfg.Cells[dst]+"/internal/apply", body); err != nil {
		return 0, fmt.Errorf("apply: %w", err)
	}
	r.applied.WithLabelValues(src, dst).Add(float64(len(page.Entries)))
	return len(page.Entries), nil
}

func (r *Replicator) get(ctx context.Context, u string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (r *Replicator) post(ctx context.Context, u string, body []byte) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("POST %s: %s", u, resp.Status)
	}
	return nil
}
