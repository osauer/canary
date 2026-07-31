# Daemon authority storage maintenance

**Status:** Implementation contract

**Updated:** 2026-07-31

## Purpose

`daemon.db` contains three materially different storage classes:

1. current mutable authority, stored as versioned state documents;
2. retained evidence, stored in append-only event and observation tables;
3. refetchable acceleration state, such as the IBKR contract cache.

Those classes must not share an accidental retention policy. Current state is
replaced through compare-and-swap. Evidence is retained unless the operator
approves a specific policy change. Refetchable acceleration state belongs only
in its current state document and may be discarded when it has no reader,
decision, or recovery claim.

This contract governs repairs where a defect wrote refetchable state into an
append-only evidence table and made the authority materially too large.

## Non-negotiable boundaries

- The daemon remains the only owner of the live database.
- Startup validates the authority before RPC or broker connectivity.
- No maintenance path mutates the published database in place.
- No general age-, size-, or count-based retention applies to evidence.
- A discard migration identifies one exact row class. Near matches survive.
- The append-only trigger is removed and recreated in the same migration
  transaction.
- The source database remains unchanged until a compact candidate and an exact
  source-head backup have both been verified.
- The exact source-head backup remains in place after publication until an
  independent compact target-head backup and the maintenance receipt verify.
- An interrupted preparation may leave only named, unpublished artifacts. They
  are rebuilt or resumed from a durable manifest; filenames and timestamps
  never establish authority.
- Broker-route, order, policy, capital, reconciliation, and statement evidence
  is outside refetchable-cache maintenance.

## Contract-cache repair

The defective row class is the exact triple:

| Column | Value |
|---|---|
| `scope_key` | `market/contracts` |
| `source` | `ibkr.tws.contract_details` |
| `kind` | `contract_cache.snapshot.v3` |

The current contract cache remains in the state document
`market/contracts` / `contract_cache.current.v3`. The daemon reads that
document at boot and can repopulate it from IBKR. No product surface reads the
discarded observation kind and no decision cites it.

The schema migration therefore:

1. drops only `observations_no_delete`;
2. deletes only the exact triple above;
3. recreates `observations_no_delete`;
4. records the immutable migration checksum and destructive approval;
5. reports the removed row count, payload bytes, and a deterministic digest
   over the removed observation identities and payload digests.

That approval does not authorize pruning any other observation.

## Large-authority upgrade sequence

Let `S` be the committed main-file and sidecar footprint. A large delete runs
inside one transaction and its WAL can approach `S`, so ordinary
source-plus-backup-plus-candidate ordering is not space-safe.

Before writing upgrade intent, the controller checks available-to-process
filesystem bytes. A new preparation requires at least:

`2 * S + max(1 GiB, 5% of S)`

Failure is typed and actionable. It reports source, required, and available
bytes and states that the published authority was unchanged.

Preparation then runs in this order:

1. inspect and bind the exact source schema and authority head;
2. create one disposable, standalone snapshot of that source;
3. apply all pending migrations atomically to the snapshot;
4. verify the discard receipt and prove the exact row class is absent;
5. use `VACUUM INTO` to create a new compact file;
6. fully validate the compact file and make it the unpublished candidate;
7. remove the bloated disposable snapshot;
8. create and verify the immutable exact old-head backup from the still
   unchanged source;
9. fingerprint the candidate and source backup in the durable upgrade
   manifest.

Only then may source quiescing, atomic rename, directory sync, reopen, and
verification publish the candidate.

For the maintenance-only schema 3 to schema 4 repair, `store_meta` and
`event_log` are unchanged. The candidate therefore has the same authority head
as the source. The controller must neither rewrite nor re-stamp
`daemon.db.head`; its bytes and filesystem timestamp remain unchanged. The
schema version and the typed maintenance metadata distinguish this publication
from a rollback. If an older schema also needs an ordinary authority migration
in the same run, the combined upgrade retains the existing rule: advance the
authority head exactly once and arm the watermark for that new head.

Plain `VACUUM`, journal disabling, hard-link “backups,” in-place deletion, and
batched weakening of the migration transaction are not permitted.

## Backup retirement

Keeping the bloated exact-head backup forever would move the defect rather than
reclaim disk. It remains mandatory until publication succeeds.

After publication, while the exact source-head backup still exists, the
controller creates and verifies an independent compact target-head backup from
the verified live target. Deferring this copy until after publication keeps the
peak additional storage within the preflight's `2 * S` bound even when
compaction reclaims little space. A crash before this step resumes from the
retained source backup and verified live target.

After the target backup verifies, the controller writes and fsyncs a
maintenance receipt containing:

- source and target schema versions and authority heads;
- exact discard selector;
- removed row count and payload bytes;
- removed-set digest;
- old backup fingerprint;
- compact backup fingerprint.

Once that receipt is durable, and only when the exact migration metadata
authorizes retirement **and the receipt proves at least one matching row was
removed**, the controller removes the bloated old backup and fsyncs its
directory. A crash after receipt publication may resume with the old backup
either present or absent; the compact target-head backup and receipt are then
the recovery proof. Missing old backup without that proof fails closed.

Normal schema upgrades, and schema 4 upgrades that removed zero matching rows,
continue to retain their exact old-head backup.

## Historical installation boundary

Direct-upgrade support begins with:

- v1.7.1 through v2.2.1 for file-backed installations whose retained order
  evidence carries a complete broker route;
- v2.3.0 for SQLite installations.

The known `purge-ledger-v1` format receives a strict adapter because its
restore quantities and fill cursors map losslessly to v2 once the complete
route is re-derived from exact order evidence.

v1.7.1 is the last writer of that v1 ledger. v1.8 through v2.2.1 use the same
v2 purge-ledger format exercised by the pinned v2.2.1 fixture, so their
retained file authority is directly importable. Those historical binaries
also treated an inherited v1 ledger as an unknown schema and removed it when
they opened it. The current migration cannot recover rows an earlier upgrade
already discarded. An installation still on v1.7.1 must therefore jump
directly through the current installer; staging through an intermediate old
binary could destroy the state the current adapter is designed to preserve.

Legacy order rows whose `client_id=0` was omitted by historical JSON remain
ambiguous. The importer must not infer zero from current configuration.
Safety-relevant ambiguous rows fail closed until an explicit, separately
reviewed provenance or recovery mechanism exists.

Release tests use immutable artifacts generated by the tagged historical
writers. Normal tests consume the committed bytes; they do not rebuild old
releases or contact the network.

## Verification

The binding evidence is:

- exact-match prune tests with one near miss for every selector column;
- state-document survival and zero future contract-cache observations;
- append-only trigger restoration;
- unchanged `store_meta`, external watermark bytes, and watermark timestamp for
  maintenance-only schema 3 to schema 4;
- one authority-head advance for a combined older-schema to schema 4 upgrade;
- unchanged source plus exact old-head backup;
- compact candidate and independent target-head backup at the expected head;
- crash recovery before and after publication and backup retirement;
- typed insufficient-space refusal before intent;
- tagged v1.7.1 and v2.2.1 file cutovers plus the v2.3.0 database upgrade
  fixture;
- `make test`, followed by installed-daemon and redacted status evidence only
  after the live disk-capacity and migration artifacts have been reviewed.
