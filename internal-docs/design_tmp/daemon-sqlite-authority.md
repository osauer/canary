# Daemon SQLite authority

**Status:** Implemented

**Decision date:** 2026-07-20

**Owner:** osauer

## Decision

The daemon uses `$XDG_STATE_HOME/ibkr/daemon.db` as the sole live
authority for daemon-owned state, event evidence, and retained market/gamma
observations. It does not promote or reuse `history.db`: that database, its
WAL/SHM files, JSONL journals, and rotated archives belong to the sealed legacy
epoch and are never opened by normal runtime paths.

This is a clean semantic epoch. Legacy regime, rules, canary, proposal, and
opportunity histories are not imported because they may encode behavior from
buggy implementations. Irreplaceable observations and state whose loss could
weaken a guardrail are imported through an explicit allowlist. Former live
files are sealed as rollback evidence after the database passes its cutover
checks; they are not a second runtime authority.

No risk threshold, enforcement policy, exception authority, trading mode,
freeze rule, or broker-write gate changes in this migration.

## Ownership boundaries

| Class | Authority after cutover | Notes |
|---|---|---|
| Daemon state and events | `daemon.db` | Sole daemon writer. CLI, MCP, app, and offline tools use typed daemon contracts or read-only database access. |
| Market/gamma observations | `daemon.db` | Preserve source, method/version, as-of, quality, and original payload. Former cache files cease to be live authority. |
| Operator configuration and policy | `config.toml`, `policies/*.toml` | Human-owned declarations remain separate from daemon state. |
| Broker statements | `statements/flex-*.xml` | Original broker evidence remains immutable and external; the complete current file inventory and per-day winners refresh together in one SQLite transaction. Immutable file/equity versions preserve restatement evidence. |
| Secrets and key material | Existing private files | Token signer, Flex token, and comparable secrets are not ordinary database state. Cutover rotates the order-preview signer to a new path/generation. |
| App-local state | App-owned persistence | The app remains a separate authority and never opens the daemon database. |
| Backups, sealed legacy state, and `daemon.db.head` | Private, hashed artifacts plus an external monotonic watermark | Recovery and anti-rollback material only; never read as alternate live business state or written as a mirror. |

The daemon opens `daemon.db` only after winning both its normal instance lock
and a persistence lock rooted beside the database. Alternate socket paths do
not create permission to share one state database.

## Clean-slate allowlist

### Preserve

- Exact platform settings, especially `trading.freeze`, trading limits,
  feature switches, and rulebook earnings overrides.
- Risk-capital state required to preserve account binding, adjusted peak,
  drawdown latch, statement incorporation, overrides, artefacts, and
  reconciliation continuity. Valid capital/governance events required to
  reproduce that current state are safety state, not imported analytical
  history.
- Rules regime-stage latch, purge/restore rows and fill cursors, and
  governance-nudge state containing explicit review/completion evidence.
- Full event chains for active or uncertain orders; all consumed preview-token
  tombstones; and a conservative global broker-order-ID floor plus any fully
  scoped floors derivable from the legacy journal. Every write-capable identity
  is bound to endpoint, client ID, account, and mode.
- Original Flex XML statements, re-derived through the current parser.
- Historical market/gamma inputs and measurements that are expensive or
  impossible to regenerate: regime HMDS bars and official series, breadth
  windows/history, gamma results, option open-interest observations, expiry
  grids, and gamma-skew diagnostics.
- Raw measurement payloads embedded in legacy decision journals and rotated
  archives, stored under a legacy semantic epoch with
  typed `decision_eligible=false` plus matching provenance metadata. Legacy verdicts, stages, and policy conclusions
  are not projected into current decision tables. All imported historical
  measurements are immutable observations; none populates a current-state
  document, primes a live cache, or seeds a current decision.

Every imported artifact records its source path, size, SHA-256, validation
result, import class, and row or payload count.

### Start empty

- Current regime/streak state and regime decision history.
- Derived brief fingerprints and comparison baselines.
- Rule transition history.
- Canary decision history.
- Proposal and proposal-outcome history and current proposal snapshot.
- Opportunity history and current opportunity snapshot.
- Trading-readiness proof; broker writes remain blocked until a new paper-smoke
  artifact is issued.
- Derived verdicts from rotated decision archives and their mirrored
  `history.db` rows. The original raw observation payloads are preserved as
  described above.

The skipped sources are recorded in the cutover manifest. They are not
silently ignored and are not queried by the new daemon.

## Store contract

- The application schema version is recorded in `PRAGMA user_version` and an
  immutable `schema_migrations` ledger. Migrations are ordered, versioned,
  checksummed, and transactional. Released migrations are never edited,
  reordered, removed, or assigned a new checksum.
- A future schema, corruption, checksum mismatch, failed integrity check, or
  migration error is fatal; the daemon never deletes or silently recreates
  `daemon.db`.
- The migration ledger is not accepted as proof by itself. Startup and backup
  verification compare every application-owned table, index, and trigger with
  the canonical schema object manifest, and recompute stored SHA-256 values for
  state documents and append-only evidence payloads.
- WAL, foreign keys, `synchronous=FULL`, a bounded busy timeout, and supported
  platform full-fsync controls are enabled. Only the daemon writes.
- Event and observation rows are append-only at the SQL layer. Mutable state
  documents use explicit schema versions and compare-and-swap revisions.
- Observation eligibility is a typed, non-null column. Current-state commits
  force their coupled observations eligible; legacy import APIs force false.
  Live decision code is statically barred from generic all-history readers and
  may use only the eligible-only API.
- A consumed preview token is a unique tombstone, not a read-then-write
  convention. Token consumption, order-ID floor advancement, and durable
  pre-transmit order events commit in one transaction before broker transmit.
  Place, modify, cancel, purge, and restore all use this rule.
- Cutover creates a new authority epoch, signer generation, token version, and
  private signer path. Every unspent legacy preview token becomes invalid and
  trading-readiness proof is reset until a new paper-smoke artifact is issued.
- Order-ID floors never decrease. Cutover retains the legacy global maximum;
  adding scoped floors may make the rule more conservative, never less.
- Every order event, tombstone, floor, and write-eligible lookup is scoped by
  endpoint, client ID, account, and mode. Identical local references, broker
  order IDs, or permanent IDs on different routes never alias.
- A failed pre-transmit transaction blocks broker transmission. A failed
  policy-critical state transaction does not publish the new in-memory state.
  Best-effort observation failures remain non-authorizing but are disclosed as
  degraded persistence.
- Runtime `SQLITE_FULL`, `IOERR`, `READONLY`, `BUSY`, or corruption on a
  critical operation latches a typed trading blocker. It does not silently
  fall back to files. Emergency cancellation while the database is unavailable
  is a human action in TWS/Gateway, not an unjournaled harness path.
- The database carries a monotonic head generation and event sequence. A
  restored backup whose head is older than the last sealed watermark is
  refused at startup. Any future recovery procedure must rotate the signer
  generation, reconcile broker-open orders, and re-establish conservative
  floors before broker writes can resume.
- Before initial database publication, the daemon writes and fsyncs the private
  external `daemon.db.head` watermark. It advances that watermark after every
  committed mutation. Startup passes it back as the minimum acceptable head;
  an existing database with a missing watermark fails closed and requires an
  explicit, verified recovery.
- Original broker statements remain the post-trade source of truth. SQLite
  stores their complete current inventory, immutable content versions, and
  derived equity views. A parse/read failure leaves the previous complete
  projection intact; same-name/same-size restatements are detected by SHA-256,
  and removed files retract only their current winners.

## Schema evolution

Schema inspection and upgrade are daemon-startup prerequisites. They complete
after the daemon wins its instance and persistence locks and before state
adapters attach, the RPC socket is served, schedulers run, or broker
connections start.

A fresh installation creates an unpublished `daemon.db` directly at the
binary's target schema, validates it, and follows the normal initial-publication
protocol. It does not enter the existing-authority upgrade path.

For an existing database, startup validates the recorded version, migration
ledger, canonical objects, content hashes, integrity, foreign keys, authority
identity, and external minimum head, then compares the on-disk version with the
binary target:

- Equal: open the validated database normally.
- Newer: refuse to start; automatic downgrade is never allowed.
- Older: run the automatic out-of-place upgrade below. The ledger must be an
  exact checksummed prefix of the binary's immutable migration plan.

The upgrade protocol is:

1. Create and verify an immutable, standalone pre-upgrade backup at the exact
   authority epoch and head. It must reopen without WAL, SHM, journal, or other
   sidecar state. The source remains the published authority.
2. Create an unpublished candidate from that exact head. Apply every pending
   migration in order and transactionally; a failed migration cannot publish a
   partial target schema.
3. Fully validate the candidate against the binary target: migration ledger,
   exact table/index/trigger manifest, SQLite integrity and foreign keys,
   stored content hashes, and authority invariants. The candidate preserves the
   authority epoch, signer generation, event sequence, and all existing state
   and evidence, while advancing `head_generation` exactly once for the schema
   transition.
4. Persist and fsync a transient recovery manifest that binds the source,
   backup, and candidate fingerprints; old and target schema versions; old and
   candidate heads; and the last durable phase. The manifest is coordination
   state only and is removed after a verified successful upgrade.
5. With the validated candidate and manifest durable, advance the external
   monotonic watermark in its normal anti-rollback order, atomically publish
   the candidate as `daemon.db`, fsync the parent directory, reopen and
   revalidate the published database, then remove the manifest durably.

Restart resumes from the manifest's last verified phase rather than guessing
from filenames or modification times. Before publication it continues only
from artifacts whose recorded fingerprints and heads still match. After
publication it verifies and finalizes the target database. Missing, conflicting,
or unverifiable artifacts fail closed. The daemon never repairs a candidate,
rolls back automatically, or restores the pre-upgrade backup automatically;
that backup remains recovery-only under the explicit offline recovery policy.

Three version domains remain separate:

- SQL schema migrations change application-owned tables, indexes, constraints,
  and triggers.
- Mutable state documents carry kind-specific payload versions. Attribute
  changes use typed per-document migration and validation; the SQL schema
  version is not used as a proxy for a document's shape.
- Append-only events and observations remain immutable. Evolution introduces a
  versioned event/payload reader or a new projection; it never rewrites retained
  evidence merely to match the newest representation.

## Cutover

1. Stop the old daemon and win the instance and persistence locks.
2. Build `daemon.db.cutover-<id>.tmp`; validate every allowlisted source and
   record every explicit skip. Before scanning retired decision journals, run
   the legacy rotation crash-recovery protocol under the cutover lock so an
   already-published archive prefix cannot also be imported from an unchanged
   live journal. Safety-state JSON is decoded strictly: unknown fields,
   trailing values, or journals without their owning risk-capital document fail
   the unpublished cutover instead of being normalized away. Order import uses
   the canonical decoder/fold,
   counts a valid unterminated final line, rejects malformed/oversize lines,
   preserves legitimate duplicates, and imports complete active/uncertain
   chains, every consuming-token tombstone, and the global maximum order ID.
3. Import policy-critical state, order-safety continuity, and preserved
   observations transactionally. Rebuild statement projections from Flex XML.
4. Prove semantic parity for settings/freeze, capital latch and account scope,
   purge authority, active orders, consumed-token membership, order-ID floors,
   statement fingerprints, and preserved-observation hashes.
5. Run SQLite quick/integrity and foreign-key checks, checkpoint to zero WAL
   frames, close the temporary database, create and verify a private backup,
   and fsync the database. Write and fsync `daemon.db.head` before atomically
   publishing `daemon.db`, then fsync the parent directory. Never rename or
   copy a live main file without its WAL state.
6. Reopen the published database and run the same semantic assertions.
7. Seal former live files under `legacy-sealed/<cutover-id>/` with a hash
   manifest. Leave operator config and Flex XML in place. Move the old preview
   key into the sealed set and publish a new-generation key at a new path.
8. Put deterministic fail-closed blockers at both legacy `order-preview-key`
   and `order-journal.jsonl` paths so reachable older binaries cannot mint a
   token or create a fresh journal. Rollback is an explicit offline recovery
   decision, never an automatic fallback.
9. Start RPC serving and broker connection only after the database is ready.

A crash before atomic publication leaves the legacy sources untouched. A crash
after publication resumes sealing from the database cutover manifest. A temp
database never authorizes source cleanup.

## Runtime boundary

All normal daemon producers write `daemon.db` directly and normal daemon
consumers read it directly; external surfaces use typed RPC. JSONL tail ingest,
byte offsets/genesis bookkeeping, decision
journal rotation, dual-read freshness proofs, journal-scan fallbacks, file
mirroring, and file fallback are retired. Legacy readers and writers may remain
only behind explicit cutover import or test-oracle seams; attaching the
published database permanently disables those branches for that process.

The legacy rotation settings are retired. Keeping their RPC fields temporarily
for wire compatibility does not preserve a rotation worker or authorize writes
to old journals.

## Verification

- Package tests: migrations, corrupt/future refusal, append-only constraints,
  state CAS, concurrent token single-winner, floor monotonicity, import
  allowlist/exclusions, backup/restore, and crash/reopen behavior.
- Daemon tests: immediate read-after-write, account/mode isolation, active-order
  equivalence, four-dimensional route-collision isolation, recon and
  policy-state parity, and absence of new JSONL writes.
- Failure tests cover concurrent token redemption, commit-observer failure,
  broker-send exclusion after failed staging, corrupt/future schema refusal,
  changed/missing schema objects, stale application payload hashes, busy
  latching, stale-backup and missing-watermark refusal, malformed and partial
  legacy authority, interrupted legacy rotation recovery, same-size Flex
  restatements, and deterministic legacy downgrade blockers.
  Disk-full/read-only/I/O fault injection and an actual prior-binary isolation
  run remain release-hardening work, not claims made by this cutover.
- Repository gates: `make test` and full `make smoke`.
- Installed runtime: `make restart-daemon`, redacted `ibkr status --json`,
  history reads, order open/history/status reads, and integrity/backup health.
  No broker submission is required or authorized.

## Rollback

Cutover creates verified backups, but the runtime has no automatic repair or
restore path. Later corruption or a missing watermark fails closed. Operational
restore is deliberately separate: stop the daemon, verify one complete database
backup or sealed legacy epoch, then perform the required head, signer,
broker-open-order, and conservative-floor reconciliation before broker writes
resume. Never combine epochs, silently repair a corrupt authority database, or
let an older binary operate against post-cutover state.
