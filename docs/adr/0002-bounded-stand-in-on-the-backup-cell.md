# 2. Bounded stand-in on the backup cell

## Status

Accepted

## Context

When a home cell is down, its accounts can either be declined or decided somewhere else from a copy of their state.
Declining turns every cell outage into an outage for a quarter of all cardholders.
Deciding from a copy risks approving against a stale balance.

## Decision

Each account's backup cell keeps a replicated shadow of the account and stands in when the home cell is unreachable.
A stand-in approval counts against a per-account cap until the home cell acknowledges it.
The backup declines when the cap would be exceeded, or when the shadow balance minus pending stand-ins cannot cover the amount.
Stand-in approvals are honoured by the home cell on reconciliation, even when the home had declined the same `txn_id`, because the merchant already holds an approval.

## Consequences

Overspend caused by stale state is bounded by the cap per account.
Only the backup can stand in, so if both of an account's cells are down that account is unavailable.
A home cell that is slow but alive also pushes traffic to stand-in after the attempt timeout.
