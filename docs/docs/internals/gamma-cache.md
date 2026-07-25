# Gamma cache persistence

**Status:** Implemented and live since v1.0.0; migrated to the `daemon.db` authority on 2026-07-20.
**Created:** 2026-05-22 07:30 CEST
**Last update:** 2026-07-20
**Owner:** osauer
**Related:** [internal/daemon/gamma_zero_cache.go](../../../internal/daemon/gamma_zero_cache.go), [internal/daemon/gamma_zero_store.go](../../../internal/daemon/gamma_zero_store.go), [Concepts](../understand/concepts.md#gamma)

## Why this exists

The gamma-zero compute is intentionally heavy. It fans out across SPY and SPX
option chains, filters out post-settlement 0DTE expiries, fits sticky-moneyness
skew curves, and produces per-index plus combined regime diagnostics. After the
slot-anchored expiry picker landed, a cold combined run could cross the
five-minute mark.

Restarting the daemon during a trading day should not force every dashboard or
CLI caller through that full compute again. The result's natural TTL is one New
York trading session, so the cache can persist safely as long as stale sessions,
stale methodology, and wrong-scope data are rejected on load.

## Design

Gamma persistence is per-domain. It keeps gamma's lifecycle and validation
local while using the daemon's sole live SQLite authority:

- One compare-and-swap state document per scope for the currently served
  result.
- One immutable typed observation for each successful compute, preserving
  market/gamma history without making old results decision-eligible by
  accident during cutover.
- A schema `version` field inside the typed payload.
- A `method` token gate tied to the current gamma methodology.
- Missing, stale, or incompatible state documents collapse to a cold cache,
  not a stale-value guess.
- Once `daemon.db` is attached there is no JSON-file fallback. A persistence
  failure is disclosed and participates in storage health; it cannot split
  authority.
- Snapshot-time `quality.rankability` separates "served for context" from
  "rankable market evidence" so stale/degraded cache cannot confirm regime or
  stress state.

The live location is `$XDG_STATE_HOME/ibkr/daemon.db`, falling back to
`$HOME/.local/state/ibkr/daemon.db`. Former
`$XDG_CACHE_HOME/ibkr/gamma-zero/*.json` files are one-time cutover inputs and
isolated codec-test seams only.

## Scope isolation

The daemon supports three cache scopes:

- `spy+spx` for dashboard, regime, and default `ibkr gamma` calls.
- `spy` for `ibkr gamma --only=spy`.
- `spx` for `ibkr gamma --only=spx`.

Each scope has its own stable authority key:

```text
market/gamma/zero/spy+spx
market/gamma/zero/spy
market/gamma/zero/spx
```

Scope-keying is load-bearing. Without it, an SPY-only diagnostic call could
poison the combined dashboard state, and persistence would make that
wrong-scope value survive a daemon restart.

## Persisted shape

```json
{
  "version": 1,
  "session_key": "2026-05-22",
  "scope": "spy+spx",
  "method": "bs-gamma-profile-v3-stickymoneyness-0dte-split",
  "result": { "...": "rpc.GammaZeroComputed" }
}
```

Four independent gates turn persisted state into a cold cache:

- `version != 1`
- `session_key` does not match today's New York session date
- envelope `scope` does not match the authority key/requested scope
- `method` does not match the current gamma method token

The store never migrates or coerces old results. A methodology bump should
recompute from IBKR data, not reinterpret prior cache contents.

Closed-session stale fallback and failed-refresh fallback are serve paths, not
rankability paths. The payload remains inspectable, but `quality.rankability`
must be `context_only`, `blocked`, or `unavailable` until freshness and coverage
gates recover.

## Non-goals

- No second historical file log. Immutable gamma observations live beside the
  current state in `daemon.db` and are retained for calibration/replay.
- No background deletion or automatic repair. Current state is replaced on a
  successful compute; observation retention follows the authority recovery
  policy.
- No cross-domain cache abstraction. The shared logic is small, and gamma's
  invalidation semantics are easier to audit when they stay local.

## Test coverage

The store tests cover `daemon.db` round-trip persistence, current-state plus
observation writes, scope isolation, missing state, version/session/scope/method
mismatches, and constructor seeding from valid persisted scopes. Legacy file
codec tests remain only to prove deterministic one-time import.
