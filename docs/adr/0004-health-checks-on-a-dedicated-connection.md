# 4. Health checks on a dedicated connection

## Status

Accepted

## Context

A cell reports healthy when it can reach its database.
The check first used the same connection pool as traffic.
Under a neighbour's failover load that pool queued, the ping timed out, and the router ejected healthy cells, spreading the outage.

## Decision

`/healthz` pings the database over its own single-connection pool.
The router ejects a cell after two consecutive failed checks, or immediately when a connection is refused.

## Consequences

Health reflects whether the cell can work, not how busy it is.
An overloaded cell stays in rotation, and its slow requests fall back to stand-in through the attempt timeout instead.
