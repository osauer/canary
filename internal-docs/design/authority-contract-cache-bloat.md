# Contract-cache bloat in the daemon authority

**Status:** Proposed — awaiting operator decision

**Written:** 2026-07-31 11:42 CEST

**Owner:** osauer

This document is self-contained. It assumes no context beyond the repository.

## Summary

`daemon.db` on the primary desk reached **5,991 MiB**. About **5.1 GB of that is
one observation kind that nothing reads**: the daemon writes its entire IBKR
contract cache into the append-only observation ledger once every 60 seconds.

The cost is not disk. Before the daemon publishes its Unix socket it verifies
the whole authority file, and that verification is linear in the file's
contents. On 2026-07-31 it measured **47.5 s and 45.4 s** on two consecutive
restarts. Until the readiness budget was fixed (see [Related work](#related-work)),
that made every `make restart-daemon` report a false failure and leave the
paired app stopped.

Growth is accelerating, because the payload written each minute grows as the
option cache fills:

| Date | contract-cache bytes written | per snapshot |
|---|---|---|
| 2026-07-21 | 225 MB | 0.23 MB |
| 2026-07-26 | 364 MB | 0.28 MB |
| 2026-07-29 | 640 MB | 4.3 MB |
| 2026-07-30 | 1,940 MB | 4.8 MB |
| 2026-07-31 (partial) | 1,214 MB | 4.9 MB |

At ~2 GB/day, pre-socket verification grows by roughly **50 s per day**.

## The defect

`Server.contractCacheSaveLoop` ([internal/daemon/server.go](../../internal/daemon/server.go))
fires every `contractCacheSaveInterval` (60 s) and calls
`coreContractCacheAuthority.SaveContractCache`
([internal/daemon/market_residual_authority.go](../../internal/daemon/market_residual_authority.go)),
which routes through `saveMarketState`
([internal/daemon/market_observation_store.go](../../internal/daemon/market_observation_store.go)).

`saveMarketState` does two writes in one transaction:

1. a compare-and-swap on the **state document** `market/contracts` /
   `contract_cache.current.v3` — one row, overwritten each time;
2. an append to the **observation ledger** — `source =
   ibkr.tws.contract_details`, `kind = contract_cache.snapshot.v3` — a new
   permanent row carrying the full payload each time.

Write 1 is correct and is the only thing boot reads: `seedFromContractStore`
calls `LoadContractCache`, which reads the state document.

Write 2 is the defect. Verified on the live authority:

- 6,087 rows, 5,105 MB, mean ~840 KB, max 5.4 MB.
- All 6,088 payload digests are **distinct**, so this is not a
  missing-deduplication bug. Each snapshot genuinely differs as contracts
  resolve and options expire.
- **No reader exists.** `contractObservationKind` appears only at its
  definition, its single write site, and one test. Nothing calls
  `ListObservations` / `LatestObservation` for that kind, in Go, MCP, or the SPA.

The contract cache is derived data — a local copy of facts IBKR returns on
request. If it were deleted entirely the daemon would refetch and be correct,
only slower for a few minutes. It is not evidence any trading decision rests on,
so it does not belong in the ledger whose purpose is to hold such evidence.

Two smaller writers are next in line if this recurs, both ~50× smaller and
neither accelerating: `gamma_open_interest.snapshot.v1` (121 rows, 103 MB) and
`trading_halts.snapshot.v1` (8,499 rows, 68 MB).

## Why this cannot be cleaned up ad hoc

`observations` is append-only by construction. Migration 1 installs two triggers
that `RAISE(ABORT, 'observations is append-only')` on any `DELETE` or `UPDATE`.
Beyond that, `corestore.Open` validates the live schema against the migration
plan's manifest, so a `DROP TRIGGER` issued from a `sqlite3` prompt would leave
a database the daemon refuses to open.

Two facts materially **reduce** the risk, and both were verified against the
live schema:

- **No foreign key anywhere references `observations`.** Deleting rows cannot
  produce a `foreign_key_check` violation.
- **The anti-rollback head does not move.** `AuthorityHead`
  (`authority_epoch`, `head_generation`, `last_event_seq`, `signer_generation`)
  lives entirely in `store_meta`; `readAuthorityHead` reads only that table.
  `observations` is not part of it, and `state_documents` stores no observation
  id. So pruning observations leaves `daemon.db.head` valid and needs no
  re-stamping, provided `store_meta` and `event_log` are untouched.

An earlier verbal version of this plan claimed the watermark would need
re-stamping. That was wrong; it does not.

## Recommended fix

### Part A — stop the write (small, do first)

Drop the observation append from `SaveContractCache`, keeping the state-document
CAS. `saveMarketState` couples the two, so this needs either a state-only
sibling in `market_observation_store.go` or an explicit "no observation" input.

Boot behavior is unchanged because boot only ever read the state document.
Confirm before cutting that no reader has appeared on the SPA or MCP side.

This stops all further growth. It does not shrink the existing file.

### Part B — prune what is already written (needs review before it runs)

Do this as a **new schema migration**, not as operator SQL. The migration path
already provides everything this operation needs, and going around it is what
makes it dangerous.

Template to follow: migration 3, `legacyStressMeasurementRename` in
[internal/daemon/corestore/schema.go](../../internal/daemon/corestore/schema.go).
It temporarily drops `observations_no_update`, mutates, and recreates the
trigger inside the same transaction, under a `destructiveApproval` carrying a
human-written reason. Statement shape here:

```
DROP TRIGGER observations_no_delete
DELETE FROM observations
 WHERE scope_key = 'market/contracts'
   AND source    = 'ibkr.tws.contract_details'
   AND kind      = 'contract_cache.snapshot.v3'
CREATE TRIGGER observations_no_delete ...   -- via appendOnlyDeleteTrigger("observations")
```

Predicate on all three columns, as migration 3 does, so it can touch nothing
else. `validateMigrationStatements` rejects `DROP`/`DELETE` unless the
migration's `destructiveApproval` names those exact statements and states why —
write that reason properly; it is the audit record for this deletion.

What runs it: `ensureCoreStoreSchemaCurrent`
([internal/daemon/core_store_upgrade.go](../../internal/daemon/core_store_upgrade.go))
already takes a verified backup, builds a candidate out of place, verifies it,
publishes it atomically, maintains the watermark, and resumes correctly after a
crash mid-upgrade. No separate manual backup step is needed, and none should be
invented.

### Part C — reclaim the disk (optional, lowest urgency)

**Part B alone fixes the boot time.** `quick_check` walks used pages and
`checkApplicationHashes` scans rows; with the rows gone, both collapse. What
Part B does *not* do is shrink the file — SQLite keeps the freed pages, so
`daemon.db` stays ~5.9 GB on disk and reuses that space for future writes.

`VACUUM` cannot go in the migration: migrations run inside a transaction and
SQLite forbids `VACUUM` there. Two options, in preference order:

1. Add a post-migration `VACUUM` to `buildUpgradeCandidate`
   ([internal/daemon/corestore/upgrade.go](../../internal/daemon/corestore/upgrade.go)),
   after the migration transaction commits and before the candidate is
   verified. The compacted file then goes through the existing verification and
   atomic publication. This is a change to the upgrade controller and deserves
   its own review.
2. Stop the daemon and `VACUUM` the file directly. `VACUUM` preserves content,
   `user_version`, and `store_meta`, so the head and `daemon.db.head` stay
   valid. It bypasses the controller's verification, so take a backup via
   `corestore.Store.Backup` first and run `CheckIntegrity` after.

Expected result either way: ~5.9 GB → under 1 GB, pre-socket verification back
to a few seconds.

## What must not be done

- **Do not add general retention or expiry to the observation ledger.** That is
  the audit record. How far back the desk can prove its own decisions is a
  policy decision for the operator, not a side effect of a disk-space fix. This
  proposal removes one kind that was never evidence; it sets no precedent for
  aging out kinds that are.
- **Do not weaken the startup integrity check** (`quick_check`,
  `foreign_key_check`, `checkApplicationHashes` in
  [internal/daemon/corestore/integrity.go](../../internal/daemon/corestore/integrity.go)).
  It is slow because the file is too large, not because it checks too much.
- **Do not drop the `observations_no_delete` trigger outside a migration.**

## Separate, unrelated to the bloat

`corestore.Open` runs `checkIntegrityDB` twice: once before migrations and once
after ([internal/daemon/corestore/store.go](../../internal/daemon/corestore/store.go),
the two call sites around the `migrate` call). On an ordinary restart no
migration applies, so the second pass re-reads and re-hashes bytes nothing
touched — roughly half the measured pre-socket cost.

Making the post-migration pass conditional on a migration having actually run
removes no verification: a file that just passed and was then not modified does
not need checking again. Small change, large constant-factor win, independent
of everything above.

## Verification for whoever implements this

- `make check` for docs/config-only work; `make test` is binding for the Go
  changes and already includes `check`.
- After Part B, restart the daemon and confirm from the daemon log that
  `daemon authority: verified in ...` has dropped from ~47 s to seconds.
- Confirm `canary status --json` reports `connected` with all subsystems ready,
  then run `make smoke-fast`; its retained quote probe proves contract data can
  be resolved after the cache reseeds from the surviving state document.
- Row-count check on the live authority before and after:
  `SELECT kind, COUNT(*), SUM(LENGTH(payload)) FROM observations GROUP BY kind`.

## Related work

The readiness-deadline half of this problem is already fixed (uncommitted on
branch `cc-wt-/loving-tharp-0ad20a` as of this writing): `dial.StartupBudget`
derives the socket wait from the authority file size instead of a flat 5 s/15 s,
`canary restart` no longer conflates the graceful-stop budget with readiness,
the wait fast-fails when the spawned daemon dies, and the daemon now logs
`daemon authority: verifying N MiB before opening the socket` /
`verified in ...` so the pre-socket window is no longer silent.

That fix makes a large authority survivable. It does not make it correct, which
is what this document is for.
