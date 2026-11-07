package api

import "strconv"

type EventKind string

const (
	// EventAccount is emitted by an account's home cell whenever the account's
	// authoritative state changes. CausedByTxnID names the authorization behind it.
	EventAccount EventKind = "account"
	// EventStandIn is emitted by a backup cell for every stand-in approval, so
	// the home cell can apply it once it is reachable again.
	EventStandIn EventKind = "standin"
)

type AccountSnapshot struct {
	AccountID     string `json:"account_id"`
	HomeCell      string `json:"home_cell"`
	BackupCell    string `json:"backup_cell"`
	CreditLimit   int64  `json:"credit_limit"`
	Available     int64  `json:"available"`
	Version       int64  `json:"version"`
	CausedByTxnID string `json:"caused_by_txn_id,omitempty"`
}

type StandInAuthorization struct {
	TxnID       string `json:"txn_id"`
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
	Cell        string `json:"cell"`
}

type Event struct {
	Kind    EventKind             `json:"kind"`
	Account *AccountSnapshot      `json:"account,omitempty"`
	StandIn *StandInAuthorization `json:"standin,omitempty"`
}

// Position is a point in a cell's outbox. Rows are ordered by the writing
// transaction's id first, see the outbox ADR for why seq alone is unsafe.
type Position struct {
	TxID string `json:"txid"`
	Seq  int64  `json:"seq"`
}

type OutboxEntry struct {
	Position
	Event Event `json:"event"`
}

type OutboxPage struct {
	Entries []OutboxEntry `json:"entries"`
}

type ApplyRequest struct {
	Source  string        `json:"source"`
	Entries []OutboxEntry `json:"entries"`
}

// Less reports whether p is strictly before o. Malformed txids sort first.
func (p Position) Less(o Position) bool {
	a, _ := strconv.ParseUint(p.TxID, 10, 64)
	b, _ := strconv.ParseUint(o.TxID, 10, 64)
	if a != b {
		return a < b
	}
	return p.Seq < o.Seq
}
