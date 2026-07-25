# History surfaces (`daemon.db`)

**Status:** Implemented; legacy `history.db` design retired

**Created:** 2026-07-20 · **Updated:** 2026-07-23

**Owner:** osauer

**Related:** [Daemon SQLite authority](daemon-sqlite-authority.md),
[regime calibration](regime-calibration.md),
[post-trade truth](post-trade-truth.md), and
[RPC history contracts](../../internal/rpc/history_index.go)

## Decision

The daemon history RPCs read the authoritative event and statement tables in
`$XDG_STATE_HOME/ibkr/daemon.db`. The former `history.db` tail-ingest index,
JSONL journals, byte/genesis watermarks, rotation engine, gzip archives, and
journal-scan fallbacks are not part of normal runtime after the SQLite
authority cutover.

`daemon.db` is not a disposable index. It is the sole live daemon authority and
must never be deleted to trigger a rebuild. Legacy `history.db*`, journal, and
rotation artifacts are sealed or discarded according to the cutover manifest;
they may support explicit recovery or importer/test verification, but no
runtime producer or consumer opens them.

## Ownership

- The daemon is the sole writer and opens `daemon.db` only after winning its
  instance and persistence locks.
- CLI, MCP, app, and SPA code consume typed daemon contracts. They do not open
  the database or reconstruct history from files.
- Human/offline inspection may use a read-only database connection, but the
  schema is internal. Typed RPC is the stable product contract.
- Append-only evidence uses the SQLite event log and typed projection tables.
  Mutable current documents use compare-and-swap revisions. Coupled state and
  event updates commit in one transaction where required.
- A critical database error fails the authoritative operation and never
  activates a file fallback or mirror write.
- Rulebook transition events are analytical observability rather than
  policy-critical continuity. Their producer logs an append failure and may
  leave a history gap while the current canonical snapshot still advances;
  `rules.history` therefore cannot prove trade causality or adherence.

## Clean semantic epoch

Current regime/streak state, regime decisions, rule transitions, canary
decisions, proposals, proposal outcomes, and opportunities begin empty at
cutover. Their history surfaces contain only decisions produced by the current
implementation after the new authority became live.

Before reading retired decision journals and archives, cutover completes the
legacy rotation crash-recovery protocol. This prevents a published archive
prefix from also being imported out of the unchanged live journal after a
pre-swap crash.

Historic regime, breadth, and gamma measurements that are expensive or
impossible to regenerate are preserved separately as immutable observation
rows. Every imported row records its legacy epoch and typed
`decision_eligible=false` (also repeated in provenance metadata). Those
observations are research evidence only: they do not populate current-state
documents, prime runtime caches, create decision-history rows, or seed a
current verdict.

Policy-critical continuity is different from analytical decision history.
Allowlisted capital and governance events needed to reproduce current
risk-capital safeguards remain authoritative events. Active/uncertain order
chains, consumed-token tombstones, conservative order-ID floors, and purge
rows/fill cursors are also retained because dropping them could weaken
broker-write safety. Trading-readiness proof is reset.

## Typed history APIs

The public request/result shapes remain stable:

| Method | Authoritative data | Notes |
|---|---|---|
| `regime.history` | Post-cutover regime decision events | Seven-day default window; optional stage filter; newest first. |
| [`rules.history`](trading-rulebook.md) | Post-cutover Rulebook state-transition events | Optional rule filter; advisory/read-only; records evaluator state, not trade causality or broker evidence. |
| `canary.history` | Post-cutover canary decision events | Optional severity/action filters; advisory/read-only. |
| `recon.equity` | Current statement-equity projection plus retained capital events | Ninety-day default window; newest first; Flex XML remains original broker evidence. |
| Order open/history/status reads | Authoritative order events and projections | No journal freshness proof or scan fallback exists after attach. |

Free-text evidence, verdict, and summary fields are display/audit data. No
consumer parses them into submit eligibility, freeze state, policy, or
broker-write authority.

`HistoryIndexHealth` and the `index`/`statements` result fields remain in the
wire structs for compatibility. Their legacy byte-ingest interpretation is
retired: there is no live journal byte count to compare and no asynchronous
index catching up behind the authoritative query. Callers must use RPC success
or the daemon's typed storage-health status, not those byte counters, to judge
availability.

## Statement projection (`recon.equity`)

Retained `statements/flex-*.xml` files are immutable original broker evidence.
The daemon fingerprints the complete current file set by name, size, and
SHA-256. When it changes, the daemon parses every candidate source before
publishing anything and then transactionally replaces:

- the complete current statement-file inventory; and
- the complete current `(account_id, day)` equity winners.

Immutable file and equity-day version tables retain restatement evidence. A
same-name/same-size correction is detected by content hash. Removing a file
retracts current rows supplied only by that file and can reveal the next valid
retained winner. The newest `whenGenerated` statement wins a day, with a
deterministic source-order tie-break. A read or parse failure preserves the
last complete projection and is retried; it never publishes a partial file set.

## Event and order guarantees

Event rows and their typed projections are append-only at the SQL layer. Order
events, consumed preview tokens, and order-ID floors share the same database
authority and route scope. A write-eligible lookup is never served from stale
file-derived state, and a failed critical transaction cannot transmit a broker
order.

Purge is position liquidation, not history deletion. It appends order evidence
and updates purge authority transactionally; it does not erase or redact event
history.

## Rotation and legacy settings

Automatic decision-journal rotation is retired because there are no live
decision JSONL files. `history.rotation.enabled` and
`history.rotation.keep_raw_months` are retired settings. Compatibility fields
may remain in typed responses during the API transition, but they do not start
a maintenance worker, relocate evidence, or authorize writes to `rotated/`.

The `regime.journal.enabled` and `canary.journal.enabled` names remain for API
compatibility; they control forward collection of typed SQLite decision events,
not JSONL files.

## Recovery and offline inspection

Verified `backups/*.db`, hashed `legacy-sealed/<cutover-id>/` artifacts, and the
external `daemon.db.head` watermark are recovery/anti-rollback material. They
are never alternate query sources. The watermark is established before initial
database publication and advanced after every committed mutation. An existing
database with no watermark, changed or missing application schema objects,
stale application payload hashes, later corruption, or rollback detection
fails closed. No runtime repair or automatic restore path exists; verified
offline restore and head/signer/order/floor reconciliation are a deliberately
separate operational procedure. Rows from different epochs are never merged.

For ad-hoc inspection, prefer a verified backup or stop the daemon and open the
database read-only:

```sh
sqlite3 "file:$HOME/.local/state/ibkr/daemon.db?mode=ro"
```

Do not mutate the schema, copy only the main file while WAL frames exist, or
delete `daemon.db*`. Offline queries are implementation aids, not stable API.
