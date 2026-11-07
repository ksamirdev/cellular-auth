# 3. Outbox cursor ordered by transaction id

## Status

Accepted

## Context

Cells replicate through an outbox read by a cursor.
A `bigserial` cursor is unsafe: a transaction can take `seq` 10, commit after a transaction that took `seq` 11, and a reader that already passed 11 never sees 10.

## Decision

Each outbox row records its writing transaction's `xid8`.
Readers order by `(txid, seq)` and only return rows whose `txid` is below `pg_snapshot_xmin` of their own snapshot, so every transaction that could still add rows behind the cursor has finished.
The destination cell stores the cursor and moves it in the same transaction as the effects.

## Consequences

Delivery is exactly once per destination with a stateless replicator.
Any long transaction in the Postgres cluster, including autoanalyze, delays replication until it ends.
The query must order by the table column, not the `txid::text` output column, or it sorts as text (see the README).
