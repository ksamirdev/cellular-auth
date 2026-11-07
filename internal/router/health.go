package router

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"go.uber.org/fx"
	"go.uber.org/zap"
)

// failuresToEject is how many consecutive failed health checks take a cell
// out of rotation. One success brings it back.
const failuresToEject = 2

type cellState struct {
	id      string
	url     string
	healthy atomic.Bool
	drained atomic.Bool
	fails   int // owned by the poller goroutine
}

// Cells tracks which cells may receive traffic. A cell is skipped when its
// health checks fail, when a request to it cannot connect, or when an
// operator drains it.
type Cells struct {
	ids    []string
	byID   map[string]*cellState
	client *http.Client
	every  time.Duration
	log    *zap.Logger
	up     *prometheus.GaugeVec
}

func NewCells(lc fx.Lifecycle, cfg Config, log *zap.Logger, reg *prometheus.Registry) *Cells {
	c := &Cells{
		byID:   map[string]*cellState{},
		client: &http.Client{Timeout: cfg.HealthTimeout},
		every:  cfg.HealthInterval,
		log:    log,
		up: promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
			Name: "router_cell_available",
			Help: "1 if the router sends traffic to the cell.",
		}, []string{"cell"}),
	}
	for id, url := range cfg.Cells {
		s := &cellState{id: id, url: url}
		s.healthy.Store(true) // optimistic: a dead cell is caught on first contact
		c.byID[id] = s
		c.ids = append(c.ids, id)
		c.up.WithLabelValues(id).Set(1)
	}
	slices.Sort(c.ids)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			wg.Go(func() { c.poll(ctx) })
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			wg.Wait()
			return nil
		},
	})
	return c
}

func (c *Cells) IDs() []string { return c.ids }

func (c *Cells) URL(id string) string { return c.byID[id].url }

func (c *Cells) Available(id string) bool {
	s := c.byID[id]
	return s.healthy.Load() && !s.drained.Load()
}

// MarkDown ejects a cell immediately after a request could not reach it,
// without waiting for the next health check.
func (c *Cells) MarkDown(id string) { c.setHealthy(c.byID[id], false) }

func (c *Cells) SetDrained(id string, drained bool) bool {
	s, ok := c.byID[id]
	if !ok {
		return false
	}
	s.drained.Store(drained)
	c.log.Info("cell drain changed", zap.String("cell", id), zap.Bool("drained", drained))
	c.publish(s)
	return true
}

type CellStatus struct {
	ID      string `json:"id"`
	Healthy bool   `json:"healthy"`
	Drained bool   `json:"drained"`
}

func (c *Cells) Status() []CellStatus {
	out := make([]CellStatus, 0, len(c.ids))
	for _, id := range c.ids {
		s := c.byID[id]
		out = append(out, CellStatus{ID: id, Healthy: s.healthy.Load(), Drained: s.drained.Load()})
	}
	return out
}

func (c *Cells) poll(ctx context.Context) {
	t := time.NewTicker(c.every)
	defer t.Stop()
	for {
		var wg sync.WaitGroup
		for _, s := range c.byID {
			wg.Go(func() { c.check(ctx, s) })
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Cells) check(ctx context.Context, s *cellState) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/healthz", nil)
	resp, err := c.client.Do(req)
	ok := err == nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	if ok {
		s.fails = 0
		c.setHealthy(s, true)
		return
	}
	if s.fails++; s.fails >= failuresToEject {
		c.setHealthy(s, false)
	}
}

func (c *Cells) setHealthy(s *cellState, healthy bool) {
	if s.healthy.Swap(healthy) != healthy {
		c.log.Warn("cell health changed", zap.String("cell", s.id), zap.Bool("healthy", healthy))
		c.publish(s)
	}
}

func (c *Cells) publish(s *cellState) {
	v := 0.0
	if s.healthy.Load() && !s.drained.Load() {
		v = 1
	}
	c.up.WithLabelValues(s.id).Set(v)
}
