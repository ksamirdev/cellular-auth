// Command chaos drives load through the router, kills and revives a cell on
// schedule, then audits every cell database and prints what happened.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ksamirdev/cellular-auth/internal/api"
	"github.com/ksamirdev/cellular-auth/internal/audit"
)

type options struct {
	router    string
	accounts  int
	limit     int64
	open      bool
	rps       int
	workers   int
	duration  time.Duration
	killAt    time.Duration
	killCmd   string
	reviveAt  time.Duration
	reviveCmd string
	victim    string
	retryRate float64
	dbs       string
	settle    time.Duration
}

func main() {
	var o options
	flag.StringVar(&o.router, "router", "http://localhost:8080", "router base URL")
	flag.IntVar(&o.accounts, "accounts", 2000, "number of accounts")
	flag.Int64Var(&o.limit, "limit", 10_000_000, "credit limit per account, minor units")
	flag.BoolVar(&o.open, "open", true, "open the accounts before the run")
	flag.IntVar(&o.rps, "rps", 500, "target requests per second")
	flag.IntVar(&o.workers, "workers", 128, "concurrent clients")
	flag.DurationVar(&o.duration, "duration", 60*time.Second, "load duration")
	flag.DurationVar(&o.killAt, "kill-at", 20*time.Second, "when to run -kill")
	flag.StringVar(&o.killCmd, "kill", "", "shell command that kills a cell")
	flag.DurationVar(&o.reviveAt, "revive-at", 40*time.Second, "when to run -revive")
	flag.StringVar(&o.reviveCmd, "revive", "", "shell command that brings the cell back")
	flag.StringVar(&o.victim, "victim", "", "id of the cell being killed, for the blast radius report")
	flag.Float64Var(&o.retryRate, "retry-rate", 0.05, "fraction of requests resent with the same txn_id")
	flag.StringVar(&o.dbs, "dbs", "", "cell databases to audit: cell-1=postgres://...,cell-2=...")
	flag.DurationVar(&o.settle, "settle", 30*time.Second, "how long to wait for reconciliation before auditing")
	flag.Parse()

	if err := run(o); err != nil {
		log.Fatal(err)
	}
}

func run(o options) error {
	ctx := context.Background()
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: o.workers}}
	runID := randomID()
	accountIDs := make([]string, o.accounts)
	for i := range accountIDs {
		accountIDs[i] = fmt.Sprintf("acct-%d", i)
	}

	if o.open {
		if err := openAccounts(ctx, client, o, accountIDs); err != nil {
			return err
		}
		time.Sleep(2 * time.Second) // let backups receive the new accounts
	}

	rec := &recorder{}
	jobs := make(chan api.AuthorizeRequest, o.workers)
	var wg sync.WaitGroup
	start := time.Now()
	for range o.workers {
		wg.Go(func() {
			for req := range jobs {
				res := send(ctx, client, o.router, req, start)
				rec.add(res)
				if res.status == http.StatusOK && mrand.Float64() < o.retryRate {
					again := send(ctx, client, o.router, req, start)
					rec.retry(res, again)
				}
			}
		})
	}

	schedule(start, o.killAt, o.killCmd, &rec.killedAt)
	schedule(start, o.reviveAt, o.reviveCmd, &rec.revivedAt)

	// Pace against the schedule, not a ticker: a ticker drops ticks when the
	// loop falls behind, which silently lowers the real rate.
	interval := time.Second / time.Duration(o.rps)
	for n := 0; ; n++ {
		next := start.Add(time.Duration(n) * interval)
		if next.Sub(start) >= o.duration {
			break
		}
		time.Sleep(time.Until(next))
		req := api.AuthorizeRequest{
			TxnID:       fmt.Sprintf("%s-%d", runID, n),
			AccountID:   accountIDs[mrand.IntN(len(accountIDs))],
			AmountMinor: 100 + mrand.Int64N(4900),
		}
		select {
		case jobs <- req:
		default:
			rec.dropped()
		}
	}
	close(jobs)
	wg.Wait()

	rep, settledIn, err := auditAfterSettle(ctx, o)
	if err != nil {
		return err
	}
	cells, err := cellIDs(ctx, client, o.router)
	if err != nil {
		return err
	}
	printReport(os.Stdout, o, rec, cells, rep, settledIn)
	if !rep.Clean() {
		return fmt.Errorf("audit failed")
	}
	return nil
}

func openAccounts(ctx context.Context, client *http.Client, o options, ids []string) error {
	sem := make(chan struct{}, 32)
	errs := make(chan error, len(ids))
	var wg sync.WaitGroup
	for _, id := range ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			b, _ := json.Marshal(api.OpenAccountRequest{AccountID: id, CreditLimitMinor: o.limit})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.router+"/v1/accounts", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errs <- fmt.Errorf("open %s: %s", id, resp.Status)
			}
		})
	}
	wg.Wait()
	close(errs)
	return <-errs // nil when the channel is empty
}

func send(ctx context.Context, client *http.Client, router string, r api.AuthorizeRequest, start time.Time) result {
	b, _ := json.Marshal(r)
	t0 := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, router+"/v1/authorizations", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res := result{at: t0.Sub(start), account: r.AccountID}
	resp, err := client.Do(req)
	res.latency = time.Since(t0)
	if err != nil {
		return res
	}
	defer resp.Body.Close()
	res.status = resp.StatusCode
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		_ = json.Unmarshal(body, &res.resp)
	}
	return res
}

func schedule(start time.Time, at time.Duration, cmd string, mark *atomic.Int64) {
	if cmd == "" {
		return
	}
	time.AfterFunc(time.Until(start.Add(at)), func() {
		mark.Store(int64(time.Since(start)))
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		if err != nil {
			log.Printf("%q: %v: %s", cmd, err, strings.TrimSpace(string(out)))
		}
	})
}

func auditAfterSettle(ctx context.Context, o options) (audit.Report, time.Duration, error) {
	if o.dbs == "" {
		return audit.Report{}, 0, nil
	}
	pools := map[string]*pgxpool.Pool{}
	for _, kv := range strings.Split(o.dbs, ",") {
		id, dsn, ok := strings.Cut(kv, "=")
		if !ok {
			return audit.Report{}, 0, fmt.Errorf("bad -dbs entry %q", kv)
		}
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return audit.Report{}, 0, err
		}
		defer p.Close()
		pools[id] = p
	}
	t0 := time.Now()
	for {
		rep, err := audit.Check(ctx, pools)
		if err == nil && rep.UnreconciledStandIns == 0 || time.Since(t0) > o.settle {
			return rep, time.Since(t0), err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func cellIDs(ctx context.Context, client *http.Client, router string) ([]string, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, router+"/admin/cells", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var cells []struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&cells); err != nil {
		return nil, err
	}
	ids := make([]string, len(cells))
	for i, c := range cells {
		ids[i] = c.ID
	}
	return ids, nil
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
