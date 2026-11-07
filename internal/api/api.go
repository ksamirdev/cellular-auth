// Package api holds the wire types shared by the router, cells and replicator.
package api

const (
	// HeaderHomeCell and HeaderBackupCell tell a cell which cells own an account
	// when it is opened. The router derives both from the routing key.
	HeaderHomeCell   = "X-Home-Cell"
	HeaderBackupCell = "X-Backup-Cell"

	// HeaderReroute is set by a cell on a 503 when it guarantees the request was
	// not processed, so the router can safely send it to another cell.
	HeaderReroute = "X-Cell-Reroute"

	// HeaderServedBy is set by the router to the cell that produced the response.
	HeaderServedBy = "X-Served-By"
)

type Decision string

const (
	Approved Decision = "approved"
	Declined Decision = "declined"
)

type Mode string

const (
	// ModeHome means the account's home cell decided using authoritative state.
	ModeHome Mode = "home"
	// ModeStandIn means the backup cell decided using replicated, possibly stale
	// state, under a bounded stand-in limit.
	ModeStandIn Mode = "standin"
)

const (
	ReasonInsufficientFunds = "insufficient_funds"
	ReasonStandInLimit      = "standin_limit"
	ReasonUnknownAccount    = "unknown_account"
	ReasonNoCell            = "no_cell_available"
)

type AuthorizeRequest struct {
	TxnID       string `json:"txn_id"`
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
}

type AuthorizeResponse struct {
	TxnID    string   `json:"txn_id"`
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
	Mode     Mode     `json:"mode"`
	Cell     string   `json:"cell"`
}

type OpenAccountRequest struct {
	AccountID        string `json:"account_id"`
	CreditLimitMinor int64  `json:"credit_limit_minor"`
}

type Error struct {
	Error string `json:"error"`
}
