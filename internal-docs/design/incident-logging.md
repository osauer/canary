# Bounded connection diagnostics

The daemon owns a single in-memory connection incident. Failed discovery,
handshake attempts and watchdog observations open it. The first observation
warns, subsequent details are debug-only, and continuing failures produce a
reminder every fifteen minutes. A completed connection closes it with a warning
bookend, so recovery remains visible at the default log level. Current typed
status is updated on every failure, independently of this diagnostic cadence.

History refreshes classified by the typed IBKR-unavailable error and proposal or
opportunity refreshes blocked solely by `account_unavailable` may join an
already-open connection incident. Gamma refreshes with no usable connector join
the same incident instead of warning each minute. They cannot create that
incident. Independent history defects and other blockers retain their warnings. The counter measures
observations, including dependent failures, not distinct outages or retries.

Managed broker connections send recoverable handshake-attempt detail to debug;
standalone library clients retain their existing severity. Protocol errors,
identity conflicts, read-loop failures and backend-link notices are not covered
by that switch. Neither retries nor trading gates change.

P&L silence has a separate bounded incident. Rebuild attempts continue at the
existing cadence. Only a frame for the current account subscription closes the
silence incident; an old subscription frame, reconnect, or attempted rebuild
cannot claim recovery. Frame quality remains separately visible in account
health.

## Resource budget

Each incident uses fixed in-memory timestamps, a counter and a short mutex.
There are no per-symbol maps, retained error strings, timers, goroutines,
database queries, network requests or calendar lookups in this mechanism.
Disabled debug messages are tested to avoid formatting. Benchmarks cover both
the incident primitive and the complete suppressed daemon diagnostic path.

## Configured gateway duty windows

Startup TOML `[daemon]` owns diagnostic scheduling; it is not a runtime platform
preference and never changes broker-write gates, retry cadence, or data health.

- `log_calendar_mode = "conservative"` (default) retains first/ongoing incident
  and recovery warnings. Cached calendar windows decide when repeated backend
  losses deserve per-event warnings. No portfolio polling is added in this mode.
- `log_calendar_mode = "scheduled"` explicitly declares that the configured
  markets and padding cover the operator's intended duties, including manual
  orders that Canary cannot enumerate. An entire known off-duty incident may
  use INFO. Unknown scope always retains WARN.
- `log_markets` defaults to `us_equity`, `us_options`, `de_xetra`, `uk_lse`,
  `jp_tse`, and `hk_hkex`. A configured list replaces that baseline; automatic
  US analytics still add US equities/options. `["always"]` disables quieting.
- `log_before_open_minutes` defaults to 360; `log_after_close_minutes` defaults
  to 240. Both accept 0..720, including explicit zero. These are diagnostic
  duty envelopes, not claims about exchange product hours. Lunch stays relevant.

Example opt-in (restart required):

```toml
[daemon]
log_calendar_mode = "scheduled"
log_markets = ["us_equity", "us_options", "de_xetra", "uk_lse", "jp_tse", "hk_hkex"]
log_before_open_minutes = 360
log_after_close_minutes = 240
```

There is no authoritative watchlist store in the current daemon. Scheduled mode
samples passive, exact-session portfolio evidence once per minute and reuses
already collected API-order snapshots; neither path sends broker requests.
Recognized stock venues add markets. Ambiguous SMART routes, unsupported
securities (including options with potentially global hours), outside-RTH orders,
or invalid inventory keep warning relevance conservative. Inferred markets and
unsupported evidence are retained until daemon restart; closing a position does
not automatically narrow the logging scope. API snapshots do not cover every
manual TWS order; the configured duty declaration is essential, not inferred
from an empty portfolio.

Completed inventory is required before quieting and retained for at most 24 hours
from its receipt through a same-scope outage. New connector/session scope must
provide its own completed inventory. Obsolete workers cannot publish over a
successor connector. Missing, expired, future-dated, or invalid evidence warns.
A minute-cadence refresh may take up to one minute to observe new portfolio/order
scope; it does not participate in trading decisions.

Embedded calendars are compiled at most hourly unless the inferred market scope
changes. One atomic pointer publishes merged intervals; log events perform only
bounded timestamp comparisons, with no database, network, timezone, or calendar
query. Expired views, clock rollback, and missing calendar coverage warn. A
quiet incident promotes on the next connection failure when duty begins; an
ongoing backend outage promotes within the worker's minute cadence even without
a new broker notice. Recovery considers the entire outage interval.

## P&L recovery integrity

Hermetic reproduction found that an asynchronous 1101 recovery from a retired
session could rebuild successor P&L subscriptions. This is a demonstrated code
defect, not a proven explanation for the September 21 feed incident. The ordinary
1101 replay and silent-after-1102 repair paths work in the synthetic wire tests.

P&L recovery now carries the originating connection and epoch through account
updates, cancellation and replacement requests. The existing guarded transport
checks the epoch again after pacing, immediately before writing. Short cache
commits exclude session retirement; conditional cleanup cannot erase successor
request identities. Receipts enter under publication, inbound and evidence
leases in that order, and both session and request identity must match.

Rebuilds serialize independently of receipt handling. A stale monitor decision
cannot replace a newer repair, and backend-down state defers pointless repair.
Failed request slots remain retryable; a bounded per-rebuild failure summary
contains counts, not account or contract identifiers. Position demand remains
responsible for retrying an individual failed position subscription. Restored
backend messages now identify the restoration code. These diagnostics distinguish
transport/subscription activity from valid P&L health; only a newer valid frame
clears the separate health failure.

Tests cover normal 1101/1102 behavior, rejected old frames, queued requests
crossing socket generations, failed sends, concurrent display/publication reads,
and cache publication against epoch retirement. No test places a broker order.
