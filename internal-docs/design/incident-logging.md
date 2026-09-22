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

## Calendar scope remains a separate decision

The existing US-equities, US-options and Xetra backend-warning scope is not a
proof that no overnight duty exists. Do not use the quote-market helper's US
fallback, a missing position snapshot, or unknown calendar coverage to silence
an incident. This change coalesces failures at all hours; it does not introduce
nighttime suppression or claim Asian-market awareness.

A future relevance policy needs an authoritative set of markets and duties,
including positions, open orders, preparation and post-close work. Compute its
session boundaries when that scope changes, then publish a small immutable
in-memory view. Logging should compare timestamps against that view, never
query a calendar database per event. Unknown or expired scope must remain
visible. Introduce that policy only with fixtures for holidays, lunch breaks,
DST, extended sessions, stale scope and outages crossing a duty boundary.

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
