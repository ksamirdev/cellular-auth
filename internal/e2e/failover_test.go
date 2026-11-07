// Package e2e runs real cells, router and replicator in one process against
// real Postgres, then kills a cell mid-traffic.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/ksamirdev/cellular-auth/internal/api"
	"github.com/ksamirdev/cellular-auth/internal/audit"
	"github.com/ksamirdev/cellular-auth/internal/cell"
	"github.com/ksamirdev/cellular-auth/internal/platform"
	"github.com/ksamirdev/cellular-auth/internal/replication"
	"github.com/ksamirdev/cellular-auth/internal/router"
	"github.com/ksamirdev/cellular-auth/internal/testpg"
)

const standInCap = 5_000

type cluster struct {
	t         *testing.T
	cells     map[string]cell.Config
	apps      map[string]*fx.App
	pools     map[string]*pgxpool.Pool
	urls      map[string]string
	routerURL string
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func start(t *testing.T, opts ...fx.Option) *fx.App {
	t.Helper()
	app := fx.New(append(opts, platform.Module)...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return app
}

func stop(t *testing.T, app *fx.App) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := app.Stop(ctx); err != nil {
		t.Error(err)
	}
}

func newCluster(t *testing.T, n int) *cluster {
	c := &cluster{t: t, cells: map[string]cell.Config{}, apps: map[string]*fx.App{}, pools: map[string]*pgxpool.Pool{}, urls: map[string]string{}}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("cell-%d", i)
		cfg := cell.Config{ID: id, Addr: freeAddr(t), DatabaseURL: testpg.New(t), StandInCapMinor: standInCap}
		c.cells[id] = cfg
		c.urls[id] = "http://" + cfg.Addr
		c.startCell(id)
		pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.pools[id] = pool
	}

	addr := freeAddr(t)
	c.routerURL = "http://" + addr
	r := start(t, fx.Supply(router.Config{
		Addr: addr, Cells: c.urls,
		AttemptTimeout: time.Second, HealthInterval: 50 * time.Millisecond, HealthTimeout: 100 * time.Millisecond,
	}), router.Module)
	rep := start(t, fx.Supply(replication.Config{
		Addr: freeAddr(t), Cells: c.urls, Interval: 20 * time.Millisecond, BatchSize: 500,
	}), replication.Module)
	t.Cleanup(func() {
		stop(t, rep)
		stop(t, r)
		for _, app := range c.apps {
			stop(t, app)
		}
	})
	return c
}

func (c *cluster) startCell(id string) {
	c.apps[id] = start(c.t, fx.Supply(c.cells[id]), cell.Module)
}

func (c *cluster) killCell(id string) {
	stop(c.t, c.apps[id])
	delete(c.apps, id)
}

func (c *cluster) post(path string, body any) (status int, respBody []byte) {
	c.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(c.routerURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func (c *cluster) authorize(txn, account string, amount int64) api.AuthorizeResponse {
	c.t.Helper()
	status, body := c.post("/v1/authorizations", api.AuthorizeRequest{TxnID: txn, AccountID: account, AmountMinor: amount})
	if status != http.StatusOK {
		c.t.Fatalf("authorize %s: %d %s", txn, status, body)
	}
	var r api.AuthorizeResponse
	if err := json.Unmarshal(body, &r); err != nil {
		c.t.Fatal(err)
	}
	return r
}

func (c *cluster) audit() audit.Report {
	c.t.Helper()
	r, err := audit.Check(context.Background(), c.pools)
	if err != nil {
		c.t.Fatal(err)
	}
	return r
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHomeCellFailover(t *testing.T) {
	c := newCluster(t, 3)
	ids := []string{"cell-1", "cell-2", "cell-3"}

	const accounts = 60
	for i := range accounts {
		status, body := c.post("/v1/accounts", api.OpenAccountRequest{AccountID: fmt.Sprintf("acct-%d", i), CreditLimitMinor: 100_000})
		if status != http.StatusNoContent {
			t.Fatalf("open account: %d %s", status, body)
		}
	}
	eventually(t, "shadows replicated to backups", func() bool {
		var shadows int
		for id, p := range c.pools {
			var n int
			_ = p.QueryRow(context.Background(), `SELECT count(*) FROM accounts WHERE backup_cell = $1`, id).Scan(&n)
			shadows += n
		}
		return shadows == accounts
	})

	victim := router.Owners(ids, "acct-0")[0]
	homedOnVictim := func(acct string) bool { return router.Owners(ids, acct)[0] == victim }
	c.killCell(victim)

	var standIns, homes int
	for i := range accounts {
		acct := fmt.Sprintf("acct-%d", i)
		r := c.authorize(fmt.Sprintf("down-%d", i), acct, 1_000)
		if r.Decision != api.Approved {
			t.Fatalf("%s declined during outage: %+v", acct, r)
		}
		switch {
		case homedOnVictim(acct) && r.Mode == api.ModeStandIn:
			standIns++
		case !homedOnVictim(acct) && r.Mode == api.ModeHome:
			homes++
		default:
			t.Fatalf("%s served in mode %s by %s", acct, r.Mode, r.Cell)
		}
	}
	if standIns == 0 || homes == 0 {
		t.Fatalf("expected both stand-in and home traffic, got %d stand-in, %d home", standIns, homes)
	}

	// The cap holds while the home cell is still down.
	acct := ""
	for i := range accounts {
		if a := fmt.Sprintf("acct-%d", i); homedOnVictim(a) {
			acct = a
			break
		}
	}
	if r := c.authorize("over-cap", acct, standInCap); r.Reason != api.ReasonStandInLimit {
		t.Fatalf("stand-in cap not enforced: %+v", r)
	}

	c.startCell(victim)
	eventually(t, "stand-ins reconciled", func() bool { return c.audit().UnreconciledStandIns == 0 })

	if r := c.audit(); !r.Clean() || r.OverspentAccounts != 0 {
		t.Fatalf("audit failed after recovery: %+v", r)
	}
	if r := c.authorize("after", acct, 1_000); r.Mode != api.ModeHome {
		t.Fatalf("traffic did not return home: %+v", r)
	}
	// A retry of a stand-in transaction returns the same decision from the
	// home cell, which now owns it.
	if r := c.authorize("down-0", "acct-0", 1_000); r.Decision != api.Approved {
		t.Fatalf("retry after recovery changed the decision: %+v", r)
	}
	t.Logf("stand-in served %d accounts, home cells served %d", standIns, homes)
}
