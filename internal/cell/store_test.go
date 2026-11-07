package cell

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/ksamirdev/cellular-auth/internal/api"
	"github.com/ksamirdev/cellular-auth/internal/testpg"
)

func newStore(t *testing.T, id string, standInCap int64) *Store {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testpg.New(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := runMigrations(ctx, stdlib.OpenDBFromPool(pool)); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool, Config{ID: id, StandInCapMinor: standInCap})
}

func available(t *testing.T, s *Store, account string) int64 {
	t.Helper()
	var v int64
	if err := s.pool.QueryRow(context.Background(), `SELECT available FROM accounts WHERE account_id = $1`, account).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func mustAuthorize(t *testing.T, s *Store, txn, account string, amount int64) api.AuthorizeResponse {
	t.Helper()
	r, err := s.Authorize(context.Background(), api.AuthorizeRequest{TxnID: txn, AccountID: account, AmountMinor: amount})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// replicate ships everything in from's outbox to to, as the replicator would.
func replicate(t *testing.T, from, to *Store) {
	t.Helper()
	ctx := context.Background()
	for {
		pos, err := to.Cursor(ctx, from.cellID)
		if err != nil {
			t.Fatal(err)
		}
		entries, err := from.ReadOutbox(ctx, pos, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			return
		}
		if err := to.Apply(ctx, from.cellID, entries); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuthorizeIsIdempotent(t *testing.T) {
	s := newStore(t, "cell-1", 0)
	ctx := context.Background()
	if err := s.OpenAccount(ctx, api.OpenAccountRequest{AccountID: "a", CreditLimitMinor: 1000}, "cell-1", "cell-2"); err != nil {
		t.Fatal(err)
	}

	first := mustAuthorize(t, s, "t1", "a", 400)
	again := mustAuthorize(t, s, "t1", "a", 400)
	if first != again || first.Decision != api.Approved {
		t.Fatalf("retry changed the decision: %+v then %+v", first, again)
	}
	if got := available(t, s, "a"); got != 600 {
		t.Fatalf("available = %d after one approval of 400, want 600", got)
	}
}

func TestConcurrentRetriesHoldOnce(t *testing.T) {
	s := newStore(t, "cell-1", 0)
	ctx := context.Background()
	if err := s.OpenAccount(ctx, api.OpenAccountRequest{AccountID: "a", CreditLimitMinor: 1000}, "cell-1", "cell-2"); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { mustAuthorize(t, s, "same-txn", "a", 100) })
	}
	wg.Wait()
	if got := available(t, s, "a"); got != 900 {
		t.Fatalf("available = %d, want 900: concurrent retries were charged more than once", got)
	}
}

func TestHomeDeclinesOverLimit(t *testing.T) {
	s := newStore(t, "cell-1", 0)
	if err := s.OpenAccount(context.Background(), api.OpenAccountRequest{AccountID: "a", CreditLimitMinor: 500}, "cell-1", "cell-2"); err != nil {
		t.Fatal(err)
	}
	mustAuthorize(t, s, "t1", "a", 300)
	r := mustAuthorize(t, s, "t2", "a", 300)
	if r.Decision != api.Declined || r.Reason != api.ReasonInsufficientFunds {
		t.Fatalf("got %+v, want insufficient_funds", r)
	}
}

func TestStandInIsCappedAndReconciles(t *testing.T) {
	home := newStore(t, "cell-1", 0)
	backup := newStore(t, "cell-2", 1000)
	ctx := context.Background()
	if err := home.OpenAccount(ctx, api.OpenAccountRequest{AccountID: "a", CreditLimitMinor: 10_000}, "cell-1", "cell-2"); err != nil {
		t.Fatal(err)
	}
	replicate(t, home, backup)

	// Home is down. The backup stands in up to its cap and no further.
	if r := mustAuthorize(t, backup, "s1", "a", 600); r.Decision != api.Approved || r.Mode != api.ModeStandIn {
		t.Fatalf("first stand-in: %+v", r)
	}
	if r := mustAuthorize(t, backup, "s2", "a", 600); r.Reason != api.ReasonStandInLimit {
		t.Fatalf("second stand-in should hit the cap, got %+v", r)
	}

	// Home is back. It takes the stand-in approval, and its acknowledgement
	// frees the backup's cap.
	replicate(t, backup, home)
	if got := available(t, home, "a"); got != 9_400 {
		t.Fatalf("home available = %d, want 9400", got)
	}
	replicate(t, home, backup)
	if r := mustAuthorize(t, backup, "s3", "a", 600); r.Decision != api.Approved {
		t.Fatalf("cap was not released after reconciliation: %+v", r)
	}
}

// The home cell approves, its response is lost, and the router sends the same
// txn_id to the backup, which approves it too. The account must be held once.
func TestDuplicateAcrossCellsHoldsOnce(t *testing.T) {
	home := newStore(t, "cell-1", 0)
	backup := newStore(t, "cell-2", 1000)
	ctx := context.Background()
	if err := home.OpenAccount(ctx, api.OpenAccountRequest{AccountID: "a", CreditLimitMinor: 10_000}, "cell-1", "cell-2"); err != nil {
		t.Fatal(err)
	}
	replicate(t, home, backup)

	mustAuthorize(t, home, "t1", "a", 500)
	mustAuthorize(t, backup, "t1", "a", 500)
	replicate(t, backup, home)
	replicate(t, home, backup)

	if got := available(t, home, "a"); got != 9_500 {
		t.Fatalf("home available = %d, want 9500", got)
	}
	var pending int
	if err := backup.pool.QueryRow(ctx, `SELECT count(*) FROM authorizations WHERE NOT reconciled AND mode = 'standin'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d stand-in approvals left unreconciled", pending)
	}
}

// A stand-in approval the merchant already received must be honored even if
// the home cell had declined the same txn_id.
func TestStandInOverridesHomeDecline(t *testing.T) {
	home := newStore(t, "cell-1", 0)
	backup := newStore(t, "cell-2", 1000)
	ctx := context.Background()
	if err := home.OpenAccount(ctx, api.OpenAccountRequest{AccountID: "a", CreditLimitMinor: 1000}, "cell-1", "cell-2"); err != nil {
		t.Fatal(err)
	}
	replicate(t, home, backup)
	mustAuthorize(t, home, "t0", "a", 800)
	if r := mustAuthorize(t, home, "t1", "a", 500); r.Decision != api.Declined {
		t.Fatalf("home should decline t1: %+v", r)
	}
	// The backup has not seen t0 yet, so it approves t1 from stale state.
	if r := mustAuthorize(t, backup, "t1", "a", 500); r.Decision != api.Approved {
		t.Fatalf("backup should approve t1 from stale state: %+v", r)
	}
	replicate(t, backup, home)
	if got := available(t, home, "a"); got != -300 {
		t.Fatalf("home available = %d, want -300 (overspend bounded by the stand-in cap)", got)
	}
}

// Writers commit out of seq order. A reader paging with the outbox cursor must
// see every entry exactly once. Entries may show up late (any open transaction
// in the cluster, even autoanalyze, holds back the visibility horizon), so the
// reader polls until it has them all or gives up.
func TestOutboxCursorMissesNothing(t *testing.T) {
	s := newStore(t, "cell-1", 0)
	ctx := context.Background()
	const writers, perWriter = 16, 50
	const total = writers * perWriter

	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				id := fmt.Sprintf("w%d-%d", w, i)
				if err := s.OpenAccount(ctx, api.OpenAccountRequest{AccountID: id, CreditLimitMinor: 1}, "cell-1", "cell-2"); err != nil {
					t.Error(err)
				}
			}
		})
	}

	seen := map[int64]int{}
	pos := startPosition
	deadline := time.Now().Add(10 * time.Second)
	for len(seen) < total && time.Now().Before(deadline) {
		entries, err := s.ReadOutbox(ctx, pos, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			seen[e.Seq]++
			pos = e.Position
		}
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("reader saw %d distinct entries, want %d", len(seen), total)
	}
	for seq, n := range seen {
		if n != 1 {
			t.Fatalf("entry %d seen %d times", seq, n)
		}
	}
}

// Regression: the outbox was once sorted by txid as text, so a page starting
// below 10000 jumped straight to "10000" and skipped 9999.
func TestOutboxOrdersTxidsNumerically(t *testing.T) {
	s := newStore(t, "cell-1", 0)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO outbox (txid, event) VALUES ('10000', '{}'), ('9999', '{}')`); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ReadOutbox(ctx, startPosition, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.TxID)
	}
	if fmt.Sprint(got) != "[9999 10000]" {
		t.Fatalf("outbox order = %v, want numeric", got)
	}
}
