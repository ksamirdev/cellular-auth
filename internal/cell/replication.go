package cell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ksamirdev/cellular-auth/internal/api"
)

var startPosition = api.Position{TxID: "0", Seq: 0}

// ReadOutbox returns entries after pos in commit-safe order.
//
// seq alone is not a safe cursor: a transaction can take seq 10, commit after
// another transaction that took seq 11, and a reader that already moved past
// 11 never sees 10. Instead we order by the writing transaction's id and only
// return rows from transactions older than every transaction still running,
// so nothing can later appear behind the cursor.
func (s *Store) ReadOutbox(ctx context.Context, after api.Position, limit int) ([]api.OutboxEntry, error) {
	//
	// ORDER BY must name o.txid: a bare "txid" binds to the txid::text output
	// column, sorts as text, and skips every txid in 9999 < "10000".
	rows, err := s.pool.Query(ctx, `
		SELECT o.txid::text, o.seq, o.event FROM outbox o
		WHERE (o.txid, o.seq) > ($1::text::xid8, $2)
		  AND o.txid < pg_snapshot_xmin(pg_current_snapshot())
		ORDER BY o.txid, o.seq
		LIMIT $3`, after.TxID, after.Seq, limit)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (api.OutboxEntry, error) {
		var e api.OutboxEntry
		var raw []byte
		if err := r.Scan(&e.TxID, &e.Seq, &raw); err != nil {
			return e, err
		}
		return e, json.Unmarshal(raw, &e.Event)
	})
}

func (s *Store) Cursor(ctx context.Context, source string) (api.Position, error) {
	p := startPosition
	err := s.pool.QueryRow(ctx,
		`SELECT last_txid::text, last_seq FROM replication_cursors WHERE source_cell = $1`, source).
		Scan(&p.TxID, &p.Seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return startPosition, nil
	}
	return p, err
}

// Apply consumes a page of another cell's outbox. The cursor moves in the same
// transaction as the effects, so a page applied twice is a no-op the second time.
func (s *Store) Apply(ctx context.Context, source string, entries []api.OutboxEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		var cur api.Position
		err := tx.QueryRow(ctx,
			`SELECT last_txid::text, last_seq FROM replication_cursors WHERE source_cell = $1 FOR UPDATE`, source).
			Scan(&cur.TxID, &cur.Seq)
		if errors.Is(err, pgx.ErrNoRows) {
			cur = startPosition
		} else if err != nil {
			return fmt.Errorf("lock cursor: %w", err)
		}

		for _, e := range entries {
			if !cur.Less(e.Position) {
				continue // already applied by an earlier, overlapping page
			}
			if err := s.applyEvent(ctx, tx, e.Event); err != nil {
				return fmt.Errorf("apply %s@%s/%d: %w", source, e.TxID, e.Seq, err)
			}
		}

		last := entries[len(entries)-1].Position
		_, err = tx.Exec(ctx, `
			INSERT INTO replication_cursors (source_cell, last_txid, last_seq)
			VALUES ($1, $2::text::xid8, $3)
			ON CONFLICT (source_cell) DO UPDATE
			SET last_txid = excluded.last_txid, last_seq = excluded.last_seq
			WHERE (replication_cursors.last_txid, replication_cursors.last_seq) < (excluded.last_txid, excluded.last_seq)`,
			source, last.TxID, last.Seq)
		return err
	})
}

func (s *Store) applyEvent(ctx context.Context, tx pgx.Tx, e api.Event) error {
	switch e.Kind {
	case api.EventAccount:
		if e.Account.BackupCell != s.cellID {
			return nil
		}
		return s.applyShadow(ctx, tx, e.Account)
	case api.EventStandIn:
		return s.applyStandIn(ctx, tx, e.StandIn)
	default:
		return fmt.Errorf("unknown event kind %q", e.Kind)
	}
}

// applyShadow refreshes this cell's copy of an account it backs up. When the
// snapshot was caused by one of our stand-in approvals, the home cell has now
// counted it, so it stops counting against the stand-in cap.
func (s *Store) applyShadow(ctx context.Context, tx pgx.Tx, a *api.AccountSnapshot) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO accounts (account_id, home_cell, backup_cell, credit_limit, available, version)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (account_id) DO UPDATE
		SET credit_limit = excluded.credit_limit, available = excluded.available, version = excluded.version
		WHERE accounts.version < excluded.version`,
		a.AccountID, a.HomeCell, a.BackupCell, a.CreditLimit, a.Available, a.Version); err != nil {
		return fmt.Errorf("upsert shadow: %w", err)
	}
	if a.CausedByTxnID == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE authorizations SET reconciled = true WHERE txn_id = $1 AND mode = 'standin'`, a.CausedByTxnID)
	return err
}

// applyStandIn is the home cell accepting an approval its backup made while it
// was unreachable. The merchant already has an approval, so it is honored even
// if the home cell had declined the same txn_id or the account is now over its
// limit. A txn_id the home cell already approved is not charged twice.
func (s *Store) applyStandIn(ctx context.Context, tx pgx.Tx, si *api.StandInAuthorization) error {
	a, err := lockAccount(ctx, tx, si.AccountID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.HomeCell != s.cellID) {
		return nil
	}
	if err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO authorizations (txn_id, account_id, amount, decision, mode, decided_by, reconciled)
		VALUES ($1, $2, $3, 'approved', 'standin', $4, true)
		ON CONFLICT (txn_id) DO UPDATE
		SET decision = 'approved', reason = '', mode = 'standin', decided_by = excluded.decided_by, reconciled = true
		WHERE authorizations.decision = 'declined'`,
		si.TxnID, si.AccountID, si.AmountMinor, si.Cell)
	if err != nil {
		return fmt.Errorf("accept stand-in: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return s.applyHold(ctx, tx, a, si.AmountMinor, si.TxnID)
	}
	// Already approved here. Still acknowledge, so the backup releases its cap.
	return appendOutbox(ctx, tx, api.Event{Kind: api.EventAccount, Account: a.snapshot(si.TxnID)})
}
