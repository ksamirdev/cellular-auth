// Package audit checks the money invariants across all cell databases. It
// reads the databases directly instead of trusting any service's API.
package audit

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Report struct {
	Accounts int
	// LedgerMismatches counts home accounts whose balance disagrees with the
	// sum of their approved authorizations. Must be 0.
	LedgerMismatches int
	// UnreconciledStandIns counts stand-in approvals the home cell has not yet
	// acknowledged. Must reach 0 once every cell is healthy.
	UnreconciledStandIns int
	// LostStandIns counts stand-in approvals that never reached the home cell
	// ledger. Must be 0 after reconciliation.
	LostStandIns int
	// OverspentAccounts and MaxOverspend measure how far stale stand-in state
	// pushed accounts past their limit. Bounded by the stand-in cap.
	OverspentAccounts int
	MaxOverspend      int64
}

func (r Report) Clean() bool {
	return r.LedgerMismatches == 0 && r.UnreconciledStandIns == 0 && r.LostStandIns == 0
}

func Check(ctx context.Context, cells map[string]*pgxpool.Pool) (Report, error) {
	var r Report
	standIns := map[string][]string{} // home cell -> txn ids approved by its backups

	for _, id := range slices.Sorted(maps.Keys(cells)) {
		db := cells[id]
		var n, mismatches, unreconciled, overspent int
		var maxOver int64
		err := db.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM accounts WHERE home_cell = $1),
			  (SELECT count(*) FROM (
			     SELECT a.account_id FROM accounts a
			     LEFT JOIN authorizations z ON z.account_id = a.account_id AND z.decision = 'approved'
			     WHERE a.home_cell = $1
			     GROUP BY a.account_id, a.credit_limit, a.available
			     HAVING a.credit_limit - a.available <> coalesce(sum(z.amount), 0)) m),
			  (SELECT count(*) FROM authorizations
			     WHERE decided_by = $1 AND mode = 'standin' AND decision = 'approved' AND NOT reconciled),
			  (SELECT count(*) FROM accounts WHERE home_cell = $1 AND available < 0),
			  (SELECT coalesce(max(-available), 0) FROM accounts WHERE home_cell = $1 AND available < 0)`,
			id).Scan(&n, &mismatches, &unreconciled, &overspent, &maxOver)
		if err != nil {
			return r, fmt.Errorf("%s: %w", id, err)
		}
		r.Accounts += n
		r.LedgerMismatches += mismatches
		r.UnreconciledStandIns += unreconciled
		r.OverspentAccounts += overspent
		r.MaxOverspend = max(r.MaxOverspend, maxOver)

		rows, err := db.Query(ctx, `
			SELECT z.txn_id, a.home_cell FROM authorizations z
			JOIN accounts a ON a.account_id = z.account_id
			WHERE z.decided_by = $1 AND z.mode = 'standin' AND z.decision = 'approved'`, id)
		if err != nil {
			return r, fmt.Errorf("%s: %w", id, err)
		}
		for rows.Next() {
			var txn, home string
			if err := rows.Scan(&txn, &home); err != nil {
				return r, err
			}
			standIns[home] = append(standIns[home], txn)
		}
		if err := rows.Err(); err != nil {
			return r, err
		}
	}

	for home, txns := range standIns {
		db, ok := cells[home]
		if !ok {
			r.LostStandIns += len(txns)
			continue
		}
		var found int
		if err := db.QueryRow(ctx,
			`SELECT count(*) FROM authorizations WHERE txn_id = ANY($1) AND decision = 'approved'`, txns).
			Scan(&found); err != nil {
			return r, fmt.Errorf("%s: %w", home, err)
		}
		r.LostStandIns += len(txns) - found
	}
	return r, nil
}
