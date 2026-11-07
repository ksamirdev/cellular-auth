package main

import (
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ksamirdev/cellular-auth/internal/api"
	"github.com/ksamirdev/cellular-auth/internal/audit"
	"github.com/ksamirdev/cellular-auth/internal/router"
)

type result struct {
	at      time.Duration
	latency time.Duration
	account string
	status  int // 0 when the request never got a response
	resp    api.AuthorizeResponse
}

type recorder struct {
	mu         sync.Mutex
	results    []result
	retries    int
	mismatches int
	drops      int
	killedAt   atomic.Int64
	revivedAt  atomic.Int64
}

func (r *recorder) add(res result) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
}

func (r *recorder) retry(first, again result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries++
	if again.status == http.StatusOK && again.resp.Decision != first.resp.Decision {
		r.mismatches++
	}
}

func (r *recorder) dropped() {
	r.mu.Lock()
	r.drops++
	r.mu.Unlock()
}

type tally struct {
	total, approved, standIn, declined, unavailable, failed int
	reasons                                                 map[string]int
}

func (t *tally) add(res result) {
	t.total++
	switch {
	case res.status == 0:
		t.failed++
	case res.status == http.StatusServiceUnavailable:
		t.unavailable++
	case res.status != http.StatusOK:
		t.failed++
	case res.resp.Decision == api.Approved:
		t.approved++
		if res.resp.Mode == api.ModeStandIn {
			t.standIn++
		}
	default:
		t.declined++
		if t.reasons == nil {
			t.reasons = map[string]int{}
		}
		t.reasons[res.resp.Reason]++
	}
}

func (t tally) String() string {
	pct := func(n int) float64 { return 100 * float64(n) / float64(max(t.total, 1)) }
	return fmt.Sprintf("%7d req  approved %6.2f%% (stand-in %6.2f%%)  declined %5.2f%%  unavailable %5.2f%%  failed %5.2f%%",
		t.total, pct(t.approved), pct(t.standIn), pct(t.declined), pct(t.unavailable), pct(t.failed))
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
}

func printReport(w io.Writer, o options, rec *recorder, cells []string, rep audit.Report, settledIn time.Duration) {
	killed, revived := time.Duration(rec.killedAt.Load()), time.Duration(rec.revivedAt.Load())
	outage := func(res result) bool { return killed > 0 && res.at >= killed && (revived == 0 || res.at < revived) }
	onVictim := func(res result) bool { return o.victim != "" && router.Owners(cells, res.account)[0] == o.victim }

	var all, victimOut, othersOut tally
	lat := map[api.Mode][]time.Duration{}
	var firstStandIn, lastVictimFailure time.Duration = -1, -1
	for _, res := range rec.results {
		all.add(res)
		if res.status == http.StatusOK {
			lat[res.resp.Mode] = append(lat[res.resp.Mode], res.latency)
		}
		if !outage(res) {
			continue
		}
		if onVictim(res) {
			victimOut.add(res)
			if res.resp.Mode == api.ModeStandIn && firstStandIn < 0 {
				firstStandIn = res.at - killed
			}
			if res.status != http.StatusOK {
				lastVictimFailure = max(lastVictimFailure, res.at-killed)
			}
		} else {
			othersOut.add(res)
		}
	}

	fmt.Fprintf(w, "\n== load: %d rps target, %.0f rps sent, %s, %d accounts over %d cells ==\n",
		o.rps, float64(len(rec.results))/o.duration.Seconds(), o.duration, o.accounts, len(cells))
	fmt.Fprintf(w, "all           %s\n", all)
	if rec.drops > 0 {
		fmt.Fprintf(w, "client could not keep up: %d requests not sent\n", rec.drops)
	}
	for _, m := range []api.Mode{api.ModeHome, api.ModeStandIn} {
		l := lat[m]
		slices.Sort(l)
		if len(l) > 0 {
			fmt.Fprintf(w, "latency %-8s p50 %8s  p99 %8s  p99.9 %8s  (n=%d)\n", m,
				percentile(l, .50).Round(10*time.Microsecond), percentile(l, .99).Round(10*time.Microsecond),
				percentile(l, .999).Round(10*time.Microsecond), len(l))
		}
	}
	for reason, n := range all.reasons {
		fmt.Fprintf(w, "declined %-20s %d\n", reason, n)
	}

	if killed > 0 {
		fmt.Fprintf(w, "\n== outage: %s killed at %s, revived at %s ==\n", o.victim, killed.Round(time.Millisecond), revived.Round(time.Millisecond))
		fmt.Fprintf(w, "homed on %-6s %s\n", o.victim, victimOut)
		fmt.Fprintf(w, "other cells   %s\n", othersOut)
		if firstStandIn >= 0 {
			fmt.Fprintf(w, "first stand-in approval %s after kill\n", firstStandIn.Round(time.Millisecond))
		}
		if lastVictimFailure >= 0 {
			fmt.Fprintf(w, "last failed request for %s accounts %s after kill\n", o.victim, lastVictimFailure.Round(time.Millisecond))
		} else {
			fmt.Fprintf(w, "no failed requests for %s accounts\n", o.victim)
		}
	}

	fmt.Fprintf(w, "\n== correctness ==\n")
	fmt.Fprintf(w, "idempotent retries   %d, changed decision %d\n", rec.retries, rec.mismatches)
	if o.dbs == "" {
		fmt.Fprintf(w, "audit skipped (no -dbs)\n")
		return
	}
	fmt.Fprintf(w, "audit after %s: accounts %d, ledger mismatches %d, lost stand-ins %d, unreconciled %d, overspent accounts %d (max %d minor)\n",
		settledIn.Round(time.Millisecond), rep.Accounts, rep.LedgerMismatches, rep.LostStandIns, rep.UnreconciledStandIns, rep.OverspentAccounts, rep.MaxOverspend)
}
