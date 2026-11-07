-- +goose Up

-- Holds both accounts this cell is home for (authoritative) and shadow copies
-- of accounts it backs up (replicated, possibly stale).
CREATE TABLE accounts (
    account_id   text PRIMARY KEY,
    home_cell    text   NOT NULL,
    backup_cell  text   NOT NULL,
    credit_limit bigint NOT NULL CHECK (credit_limit >= 0),
    available    bigint NOT NULL,
    version      bigint NOT NULL
);

-- Every decision this cell has made or, at the home cell, accepted from a
-- backup's stand-in. txn_id is the idempotency key end to end.
CREATE TABLE authorizations (
    txn_id       text PRIMARY KEY,
    account_id   text        NOT NULL,
    amount       bigint      NOT NULL CHECK (amount > 0),
    decision     text        NOT NULL,
    reason       text        NOT NULL DEFAULT '',
    mode         text        NOT NULL,
    decided_by   text        NOT NULL,
    -- Only meaningful on the backup cell: true once the home cell has
    -- acknowledged this stand-in approval.
    reconciled   boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX authorizations_pending_standin
    ON authorizations (account_id)
    WHERE mode = 'standin' AND decision = 'approved' AND NOT reconciled;

CREATE TABLE outbox (
    seq        bigserial PRIMARY KEY,
    txid       xid8        NOT NULL DEFAULT pg_current_xact_id(),
    event      jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX outbox_position ON outbox (txid, seq);

-- How far this cell has applied each other cell's outbox.
CREATE TABLE replication_cursors (
    source_cell text   PRIMARY KEY,
    last_txid   xid8   NOT NULL,
    last_seq    bigint NOT NULL
);

-- +goose Down
DROP TABLE replication_cursors;
DROP TABLE outbox;
DROP TABLE authorizations;
DROP TABLE accounts;
