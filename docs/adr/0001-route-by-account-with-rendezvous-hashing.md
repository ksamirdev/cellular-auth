# 1. Route by account with rendezvous hashing

## Status

Accepted

## Context

The American Express post routes on partner, market or payment type, and sends each transaction to the cell that holds its authoritative state.
In card authorization the state that decides a transaction is the account's available credit.
Any routing key other than the account would let two cells decide against the same balance.

## Decision

The routing key is the account id.
Owners are ranked by rendezvous hashing over the cell ids: the first is the home cell, the second the backup.
Scores are FNV-1a 64 passed through the splitmix64 finalizer, because plain FNV clusters on sequential ids such as `acct-1`, `acct-2`.

## Consequences

Routers need no shared state or coordination to agree on an account's owners.
Adding a cell moves only about 1/N of accounts, all of them to the new cell, which `TestOwnersMovesOnlyKeysOfAddedCell` checks.
Moving those accounts' data is not implemented, so the cell count is fixed per deployment for now.
