package cell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ksamirdev/cellular-auth/internal/api"
)

var (
	ErrNotHome = errors.New("account is not homed on this cell")

	// ErrCommitUnknown means the commit was sent but its outcome is unknown, so
	// the request may or may not have been processed.
	ErrCommitUnknown = errors.New("commit outcome unknown")
)

// inTx runs fn in a transaction. Any error other than ErrCommitUnknown
// guarantees nothing was written.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrCommitUnknown, err)
	}
	return nil
}

// Store is the cell's only path to its database. Every state change and the
// outbox event describing it are written in the same transaction.
type Store struct {
	pool       *pgxpool.Pool
	cellID     string
	standInCap int64
}

func NewStore(pool *pgxpool.Pool, cfg Config) *Store {
	return &Store{pool: pool, cellID: cfg.ID, standInCap: cfg.StandInCapMinor}
}

type account struct {
	ID          string
	HomeCell    string
	BackupCell  string
	CreditLimit int64
	Available   int64
	Version     int64
}

func (a account) snapshot(causedBy string) *api.AccountSnapshot {
	return &api.AccountSnapshot{
		AccountID:     a.ID,
		HomeCell:      a.HomeCell,
		BackupCell:    a.BackupCell,
		CreditLimit:   a.CreditLimit,
		Available:     a.Available,
		Version:       a.Version,
		CausedByTxnID: causedBy,
	}
}

func (s *Store) OpenAccount(ctx context.Context, req api.OpenAccountRequest, home, backup string) error {
	if home != s.cellID {
		return ErrNotHome
	}
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		a := account{ID: req.AccountID, HomeCell: home, BackupCell: backup, CreditLimit: req.CreditLimitMinor, Available: req.CreditLimitMinor, Version: 1}
		tag, err := tx.Exec(ctx, `
			INSERT INTO accounts (account_id, home_cell, backup_cell, credit_limit, available, version)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (account_id) DO NOTHING`,
			a.ID, a.HomeCell, a.BackupCell, a.CreditLimit, a.Available, a.Version)
		if err != nil {
			return fmt.Errorf("insert account: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return nil // already open; opening is idempotent
		}
		return appendOutbox(ctx, tx, api.Event{Kind: api.EventAccount, Account: a.snapshot("")})
	})
}

// Authorize decides a transaction exactly once per txn_id. On the account's
// home cell it uses authoritative state. On its backup cell it stands in:
// it approves against replicated state, but never lets unreconciled stand-in
// approvals for one account exceed standInCap.
func (s *Store) Authorize(ctx context.Context, req api.AuthorizeRequest) (api.AuthorizeResponse, error) {
	var resp api.AuthorizeResponse
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if prev, ok, err := findAuthorization(ctx, tx, req.TxnID); err != nil || ok {
			resp = prev
			return err
		}

		a, err := lockAccount(ctx, tx, req.AccountID)
		if errors.Is(err, pgx.ErrNoRows) {
			resp, _, err = s.record(ctx, tx, req, api.Declined, api.ReasonUnknownAccount, api.ModeHome)
			return err
		}
		if err != nil {
			return err
		}

		switch s.cellID {
		case a.HomeCell:
			resp, err = s.authorizeHome(ctx, tx, req, a)
		case a.BackupCell:
			resp, err = s.authorizeStandIn(ctx, tx, req, a)
		default:
			resp, _, err = s.record(ctx, tx, req, api.Declined, api.ReasonUnknownAccount, api.ModeHome)
		}
		return err
	})
	return resp, err
}

func (s *Store) authorizeHome(ctx context.Context, tx pgx.Tx, req api.AuthorizeRequest, a account) (api.AuthorizeResponse, error) {
	if req.AmountMinor > a.Available {
		resp, _, err := s.record(ctx, tx, req, api.Declined, api.ReasonInsufficientFunds, api.ModeHome)
		return resp, err
	}
	resp, inserted, err := s.record(ctx, tx, req, api.Approved, "", api.ModeHome)
	if err != nil || !inserted {
		return resp, err
	}
	return resp, s.applyHold(ctx, tx, a, req.AmountMinor, req.TxnID)
}

func (s *Store) authorizeStandIn(ctx context.Context, tx pgx.Tx, req api.AuthorizeRequest, a account) (api.AuthorizeResponse, error) {
	var pending int64
	err := tx.QueryRow(ctx, `
		SELECT coalesce(sum(amount), 0) FROM authorizations
		WHERE account_id = $1 AND mode = 'standin' AND decision = 'approved' AND NOT reconciled`,
		a.ID).Scan(&pending)
	if err != nil {
		return api.AuthorizeResponse{}, fmt.Errorf("sum pending stand-in: %w", err)
	}

	decision, reason := api.Approved, ""
	switch {
	case pending+req.AmountMinor > s.standInCap:
		decision, reason = api.Declined, api.ReasonStandInLimit
	case req.AmountMinor > a.Available-pending:
		decision, reason = api.Declined, api.ReasonInsufficientFunds
	}
	resp, inserted, err := s.record(ctx, tx, req, decision, reason, api.ModeStandIn)
	if err != nil || !inserted || decision != api.Approved {
		return resp, err
	}
	return resp, appendOutbox(ctx, tx, api.Event{Kind: api.EventStandIn, StandIn: &api.StandInAuthorization{
		TxnID: req.TxnID, AccountID: a.ID, AmountMinor: req.AmountMinor, Cell: s.cellID,
	}})
}

// record persists a decision. If a concurrent request for the same txn_id
// committed first, it returns that decision with inserted=false and the caller
// must not apply side effects.
func (s *Store) record(ctx context.Context, tx pgx.Tx, req api.AuthorizeRequest, d api.Decision, reason string, mode api.Mode) (resp api.AuthorizeResponse, inserted bool, err error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO authorizations (txn_id, account_id, amount, decision, reason, mode, decided_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (txn_id) DO NOTHING`,
		req.TxnID, req.AccountID, req.AmountMinor, d, reason, mode, s.cellID)
	if err != nil {
		return resp, false, fmt.Errorf("insert authorization: %w", err)
	}
	if tag.RowsAffected() == 0 {
		resp, _, err = findAuthorization(ctx, tx, req.TxnID)
		return resp, false, err
	}
	return api.AuthorizeResponse{TxnID: req.TxnID, Decision: d, Reason: reason, Mode: mode, Cell: s.cellID}, true, nil
}

func (s *Store) applyHold(ctx context.Context, tx pgx.Tx, a account, amount int64, txnID string) error {
	a.Available -= amount
	a.Version++
	if _, err := tx.Exec(ctx, `UPDATE accounts SET available = $2, version = $3 WHERE account_id = $1`,
		a.ID, a.Available, a.Version); err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	return appendOutbox(ctx, tx, api.Event{Kind: api.EventAccount, Account: a.snapshot(txnID)})
}

func findAuthorization(ctx context.Context, tx pgx.Tx, txnID string) (api.AuthorizeResponse, bool, error) {
	r := api.AuthorizeResponse{TxnID: txnID}
	err := tx.QueryRow(ctx, `SELECT decision, reason, mode, decided_by FROM authorizations WHERE txn_id = $1`, txnID).
		Scan(&r.Decision, &r.Reason, &r.Mode, &r.Cell)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("find authorization: %w", err)
	}
	return r, true, nil
}

func lockAccount(ctx context.Context, tx pgx.Tx, id string) (account, error) {
	var a account
	err := tx.QueryRow(ctx, `
		SELECT account_id, home_cell, backup_cell, credit_limit, available, version
		FROM accounts WHERE account_id = $1 FOR UPDATE`, id).
		Scan(&a.ID, &a.HomeCell, &a.BackupCell, &a.CreditLimit, &a.Available, &a.Version)
	return a, err
}

func appendOutbox(ctx context.Context, tx pgx.Tx, e api.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox (event) VALUES ($1)`, b); err != nil {
		return fmt.Errorf("append outbox: %w", err)
	}
	return nil
}
