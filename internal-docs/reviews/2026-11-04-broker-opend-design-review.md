# Review record: Futu OpenD design and build brief, 2026-11-04

Documents reviewed: [design/broker-opend.md](../design/broker-opend.md) and
[design/broker-opend-build-brief.md](../design/broker-opend-build-brief.md),
both as first written on 2026-11-04 at `84e99e6a`.

Reviewer: a fresh session that did not write the documents, briefed with
`AGENTS.md`, the architecture handbook, the trading contract template,
`platform-settings.md` and the two documents, asked for defects first by
severity against the authority model and the seam, and to verify every
file and line reference against the tree. Review delivered 2026-11-04 09:24
CET: 21 findings, two blocking, ten major, nine minor. Responses applied
2026-11-04 11:55 CET. A second cold review of the revised documents follows
at the end of this record.

## Findings and responses

Each finding is the reviewer's, condensed; the response says what changed
in the documents, or why nothing did.

1. **Blocking. Adding `Kind` to every scoped key rewrites the persisted
   keys of every IBKR document** (queued authorisation terms, broker
   permissions, proposal and opportunity snapshots, desk executions, core
   rebalance, the Edge scope fingerprint; 81 non-test files use the
   scope), with no migration named, against the promise of no IBKR
   behaviour change. Response: the design now states that the kind is
   written into a key only when it is not `ibkr`, as a leading `opend:`
   segment, so every IBKR key and fingerprint keeps today's bytes by
   construction; no migration, no rewritten document. The brief's WP8
   names one helper for the writers and a fixture test that pins the
   IBKR key bytes of every scoped document family to today's values. The
   measured table now records the 81 files.

2. **Blocking. The phase A acceptance (goldens unchanged, `pkg/ibkr`
   unchanged) cannot detect a wrong adapter**: the golden harness renders
   the CLI from a fake daemon connection and never executes daemon code,
   and `pkg/ibkr` has no diff by construction. Response: a new section
   "What proves phase A" orders the proof as the adapter's mapping tests,
   the daemon tests with substituted fakes and the hermetic lifecycle
   tests, the regression spine on the landed revision, and the next live
   session (`make smoke`, `make restart-daemon`, the redacted before and
   after artifacts); the golden and `pkg/ibkr` diffs are named as side
   checks of absence of surface change only. WP2 and WP3's done criteria
   follow.

3. **Major. The paper-proof key was given a writability class** (`safety`,
   human-only) while also being daemon-written, and no owner switch was
   named, so the second half of the TWS rule (live needs the switch and
   the proof) was missing. Response: the design copies the TWS pattern
   exactly: `trading.opend.enabled` is the owner's switch (class `safety`,
   human-terminal only, advances the trading-control generation);
   `trading.opend.paper_proven_at` is daemon-written, read-only, in no
   class; a real account accepts a write only with both.

4. **Major. The unlock-state mechanism could not terminate**: while
   `locked`, Canary refused writes itself, and nothing but a write-class
   answer moved the state; the `unlocked` value was a stale observation
   printed as knowledge; the refusal code was a guess the simulator would
   prove. Response: the lock state gates nothing. Every live write-class
   request that passes Canary's own gates is sent; OpenD refuses a locked
   account and that refusal is the order's typed outcome; the owner's next
   instruction is sent again. What Canary keeps is a surface observation
   ("last live write refused at 10:02: locked"), from the broker's answers
   only. The refusal `errCode` is named as unknown until first contact,
   with the simulator's placeholder marked unconfirmed.

5. **Major. "The daemon checks the two agree on every write" was stated as
   a present mechanism** though the write binding is typed on the IBKR
   connector and the order path stays IBKR through phase C; the `trdEnv`
   stamping, the one place the pin is enforceable by construction, was not
   named; and "an unpinned login with one account is accepted"
   contradicted "several `accID` per login". Response: the design now says
   the write enforcement is phase D, designed on the pattern of the
   existing binding; names the two things true by construction (the
   connector stamps every `TrdHeader` from the scope it was built with;
   the daemon's one broker is built from the pin); and the `[opend]
   account` row says an unpinned login is accepted only when the account
   list reports exactly one account, else reads fail closed with no mode
   and the existing unpinned blocker.

6. **Major. The IBKR session binding has unexported fields and no
   constructor**, so an adapter outside `pkg/ibkr` cannot rebuild one from
   a neutral value; and `observeAccountRPC` takes the concrete connector
   and binding and was in no work package. Response: the neutral binding
   is `{Kind, Epoch}` and the adapter keeps the concrete binding of each
   epoch in a side table, answering `Current` and `Resolve` with the stored
   value; `pkg/ibkr` stays unchanged. The observation moved into the seam
   table and into WP3.

7. **Major. `protoc` cannot be pinned in a Go module**, and a check leg
   that needs it inside `check` puts an unpinned binary in the binding
   gate and CI. Response: `protoc-gen-go` is pinned in `tools/go.mod`;
   `protoc` is a developer action behind `futu-proto-regen`, which refuses
   to run unless its version equals the one in a committed `MANIFEST`
   beside the SHA-256 of each vendored definition; `futu-proto-check`, the
   leg in `check`, needs no `protoc`: it recomputes the hashes and checks
   the generated files' headers against the manifest.

8. **Major. The regression spine is CI-only and mutates committed HEAD**,
   so it proves the landed revision, not a pre-commit tree; and the
   positions-identity patch targets `handlers.go`, which the seam edits.
   Response: both documents say the spine runs on the landed revision,
   after the commit; the brief adds the re-basing step for a patch that
   stops applying (run the spine on the previous revision first, re-base
   in the same commit, say so in the body), per `AGENTS.md`.

9. **Major. "The memo" was cited eleven times** as the source of figures
   and limits: an unnamed pointer to a private document. Response: every
   citation is gone. The earlier measurement is cited by date and
   revision (2026-11-03, `12601c2a`), the limits by "Futu's public
   documentation" with the read date, and the counting method is given so
   the figures reproduce.

10. **Major. The lock-state observation had three placements** (WP7,
    WP9 with a golden, and the design's phase D). Response: one placement,
    phase D. WP7 builds `Trd_GetMaxTrdQtys` as a read with no lock logic;
    WP9's `HealthResult.OpenD` block carries no unlock field and the
    `status_opend_locked.txt` golden is dropped; the status-row table marks
    the `Trading unlock` row as phase D.

11. **Major. The coverage bar was enforced by a leg outside the binding
    gate**, the percentage would have aggregated `futusim` and generated
    code, and `pages-coverage` was in no work package. Response: the
    coverage step lives inside `test-pkg` (one of `CHECK_TEST_LEGS` and its
    own CI job), measures `./pkg/futu` alone with `-coverpkg`, and
    `pages-coverage` for `pkg/futu` is WP4.

12. **Major. The app's push-alert copy and the SPA name TWS** and no work
    package touched them. Response: the design lists the three sites and
    WP9 covers them, with `make app-check`, `make app-render-check` and
    the rendered result inspected at real size.

13. **Minor. `internal/cli/trading_status.go` does not exist** (the
    renderer is `trading.go:131`), and the existing golden already prints
    `Capabilities preview=true write=true`. Response: the broker leg joins
    the existing `Capabilities` row (`preview=true (what-if)`), the file is
    named correctly, and the IBKR golden diff is described as those words.

14. **Minor. The broker-permission rows have no OpenD source.** Response:
    under `opend` the `Broker access` row reads the account's authorised
    markets from `trdMarketAuthList` and the permission store records
    nothing; stated in the design and WP9.

15. **Minor. WP8's done criterion depended on WP9's block; WP4's
    disjointness claim ignored its `Makefile` edits; WP12 said four
    commands and listed five.** Response: WP8's criterion names the
    `HealthResult` fields that exist after WP8; the parallel-work paragraph
    names every file each package touches, including the `Makefile`; WP12
    says five.

16. **Minor. The release tier for the OpenD packages claimed a Gateway
    smoke that exercises no OpenD code**, and `make restart-daemon` with
    the redacted status appeared nowhere. Response: the gate table has two
    release rows: IBKR path (Gateway smoke, restart, artifacts) and OpenD
    path (the simulator witness as the artifact, restart against the
    process simulator); the design's tier paragraph says the same.

17. **Minor. A market-data reserve number in `risk-policy.toml`** would
    enter a fingerprinted, governance-journaled file and is not risk
    semantics. Response: the number is gone. The streaming set is the
    held and regime names recomputed on every acquire; exhaustion is
    reported by OpenD and answered by a typed fallback to snapshots. No
    policy file, no setting (WP11).

18. **Minor. The local pacer "never costs the session" overclaimed for
    OpenD-wide quotas**, and the fixtures were called regression evidence
    without a real capture. Response: the pacing section says the table
    bounds Canary's own spend and the shared quotas are learned from
    OpenD's answers; the simulator section says the fixtures are
    regression fixtures against the simulator, not conformance evidence,
    and names where the first real capture lands.

19. **Minor. Trading-tagged files in `pkg/futu` fail
    `trading-package-scope-check` until the test matrix grows; tests using
    `futusim` from inside package `futu` would cycle.** Response: the
    writes section names the `TRADING_PKG_TESTS` change for phase D; the
    simulator section and WP6 put the client-and-simulator tests in the
    external package `futu_test`.

20. **Minor. Roadmap and vendor-tier wording** ("Entity and account for
    phase 0", "Phase 2 product", "a low-tier Futu account"); the README
    linked a review record that did not exist yet. Response: reworded to
    the owner's account and OpenD build, "phases A to C", and "the
    smallest documented subscription quota"; this record now exists.

21. **Minor. The reference count did not reproduce without its method**
    (the reviewer counted 1,413 and 193 with a different pattern); the
    API protocol rows are at `status.go:104-106`; the seam's snapshot was
    keyed by instrument id where the subscription manager keys by symbol.
    Response: the counting method and pattern are stated beside the table
    (the reviewer's higher count includes unexported matches); both line
    ranges are given; `Subscribe` returns the broker's subscription key,
    the symbol for IBKR, and `Snapshot` is keyed by it, so the manager's
    behaviour is unchanged.

The reviewer named finding 1 the most severe: as written it would have
orphaned every scoped IBKR document in a running `daemon.db` while the
design promised no IBKR behaviour change. Finding 2 was the runner-up
because the acceptance offered for the same phase could not have caught it.

## Second review

Findings 1 to 8 changed the design, so the revised documents went to a
second fresh reviewer with the same brief, plus the first review's record
with the instruction to check that each response is reflected in the
documents and is itself sound. Review delivered 2026-11-04 12:05 CET: no
blocking findings, eight major, eight minor, and a hygiene pass that found
nothing (the Markdown gates green with the private-terms list present).
Responses applied 2026-11-04 12:18 CET.

1. **Major. The OpenD release tier ran `make restart-daemon` "against the
   process simulator".** That target installs the worktree build to
   `$(PREFIX)/bin` and restarts the operator's own daemon with the
   operator's config, so it would have installed an OpenD worktree build
   over the paired IBKR daemon and proved nothing about OpenD. Response:
   the brief's gate table says `restart-daemon` is never run for OpenD
   work and describes the witness stack instead: a second daemon from the
   worktree's own build (`bin/canary daemon --foreground --config
   --socket --log stderr`) with its own config (`[broker] kind = "opend"`,
   `[opend]` at the simulator), its own XDG config, state, cache and
   runtime roots under a scratch directory, and the CLI through
   `CANARY_SOCKET`; it gets no Makefile target that could be mistaken for
   `restart-daemon`. The design's tier paragraph and WP12's witness say
   the same.

2. **Major. `SessionBinding{Kind, Epoch}` was ambiguous across connector
   rebuilds.** The session epoch is a counter of one `Connection`
   (`pkg/ibkr/connection.go:317`), and the daemon replaces the whole
   connector and bumps its own `connectorEpoch` (`server.go:1948-1949`),
   so an epoch alone could match across two connectors. Response: the
   neutral binding is `{Kind, Generation, Epoch}`, the same triple the
   write binding keeps; one adapter instance is built per connector and
   discarded with it, and WP1 and WP2 test a binding across a rebuild
   whose new connection repeats the old epoch value.

3. **Major. The zero value of `Kind` was undefined** while 76 scope
   literals name only account and mode, `sameBrokerScope` compares two
   fields, and `rpc.AccountDataScope` carries account and mode only.
   Response: the zero kind means `ibkr`; comparisons normalise it; the
   literals stay (narrowed by the third review's finding 2 to the empty
   literals and the current-scope builders); the kind is never written
   for IBKR; the fixture test
   asserts the zero kind and `ibkr` produce the same bytes.

4. **Major. "Enforced by construction" versus the unpinned read path.**
   The account list needs `InitConnect` first, so a connector "built with
   the scope" could not serve the unpinned single-account case. Response:
   the unpinned path is gone. `[opend] account` is required; the
   connector is built with the pinned `accID` before it connects, reads
   `Trd_GetAccList` once after `InitConnect` to take that account's
   `trdEnv`, stamps both into every `TrdHeader` and is never re-scoped;
   a pinned account the list does not report leaves it unscoped and every
   trade-class request fails typed. The blocker is the existing
   `gateway_account_mismatch`, not `gateway_account_unpinned`.

5. **Major. A nil IBKR connector would dial IBKR.** `gatewayConnector()`
   (`server.go:2805`) calls `triggerReconnect()` on a nil connector, and
   the reconnect dials the `[gateway]` endpoint (`server.go:2563-2590`);
   46 files call it. Response: the design and WP8 gate the trigger and the
   dial loop by kind, not by a nil check; a WP8 test listens on the four
   IBKR ports and fails if an `opend` daemon connects to any of them.

6. **Major. The manifest check did not bind the generated code.** A
   hand-updated manifest line or a hand-edited `.pb.go` passed. Response:
   the manifest carries the SHA-256 of every generated file as well, and
   `futu-proto-check` recomputes both sets; WP5's red tests cover the
   vendored change and the generated change. The third review's finding 6
   showed the hand-updated-manifest case cannot go red without `protoc`,
   and the provenance claim moved to the regeneration zero-diff.

7. **Major. "The spine, CI-only, on the landed revision" was unreachable**
   because pushes happen only on the owner's instruction. Response: the
   spine runs by hand on local main after each landing (`make
   regression-spine-check`, which the Makefile allows on a landed HEAD)
   and its stamp line is quoted; both documents say so.

8. **Major. The subscription manager's downcast to `*ibkrlib.Connector`**
   (`subs.go:131-136`, `163-165`) was not named, and the seam's
   `Subscribe` returning only a key would have left it in place. Response:
   the design names it as the one bypassed seam; `Subscribe` returns the
   neutral binding, the entry stores it and rechecks with `Current`; WP2
   lists the lines and tests the recheck across a reconnect and a rebuild.

9. **Minor. "81 files" overstated the writers** (16 write a key or a
   typed term), and the two key shapes differ. Response: the design says
   81 use the scope and 16 write it, and gives the shape per family: a
   leading `opend:` segment for string keys, an `omitempty` `Broker` field
   on `rpc.QueuedAuthTerms` compared at `desk_execution_submit.go:204`.

10. **Minor. WP4's "both build modes" contradicted
    `trading-package-scope-check`.** Response: `pkg/futu` runs in the
    default build until phase D, which adds its trading-tagged line
    outside `TRADING_PKG_TESTS`.

11. **Minor. Tier inconsistencies** (WP1 "normal" against "every package
    runs `make test`"; phase B "normal" against WP6 and WP7 at `make
    test`; WP10 at `make test` against the rpc contract being release
    tier). Response: one tier per package: WP1, WP4 to WP7, WP10 and WP12
    at `make test`, with WP10's line saying why it is not release tier
    (no broker path, endpoint or policy behaviour changes); phase B says
    `make test`.

12. **Minor. No `BrokerKind` reaches the app.** The bootstrap snapshot
    carries none, and `presentation.go` is a static map. Response:
    `HealthResult` gains `Broker`; WP9 lists `internal/app/live/service.go`,
    `internal/app/http/routes.go:120` and the alert dispatcher change.

13. **Minor. The brief's coverage section and `PositionView.PriceSource`
    were in no work package.** Response: WP9 covers the coverage section
    under both kinds; WP11 sets `PriceSource` and tests it.

14. **Minor. `minServerVersion` "proven against"** an OpenD that does not
    exist before first contact; the seven-day history window was not in
    "Confirm at build". Response: declared from the documentation,
    confirmed at the first real contact; both items added to the table.

15. **Minor. WP9 edits the public handbook** without a cold read or an
    explicit go. Response: WP9 names both, per the public-writing rule.

16. **Minor. This record's second section was empty and the README stamp
    was stale.** Response: this section; the README stamp follows the last edit.

The reviewer named finding 1 the most severe: as written, the OpenD
release gate would have installed a worktree build over the paired IBKR
daemon. The hygiene pass found nothing.

## Third review

Findings 2, 4 and 5 changed mechanisms, so the changed passages went to a
third fresh reviewer briefed on those passages only, with the second
review's record, and asked whether each new mechanism would work as
described. Review delivered 2026-11-04 12:29 CET: no blocking
findings, six major, five minor; hygiene clean (gates green at `84e99e6a`,
no private terms, dates not documents). Responses applied 2026-11-04
12:35 CET.

1. **Major. The witness stack's CLI leg was not isolated.** With only
   `CANARY_SOCKET` set, a CLI call that finds no socket autospawns
   `<executable> daemon` with no flags (`cmd/canary/main.go:139-142`,
   `internal/dial/autospawn.go:361`), which reads the operator's config
   and `daemon.db` from the default roots onto the scratch socket; the
   handbook warns that the socket alone isolates nothing. Response: one
   environment for every command of the witness, daemon and CLI alike
   (the four XDG roots, `CANARY_CONFIG` and `CANARY_SOCKET`), the CLI run
   as the worktree's `bin/canary` by path and never from `PATH`, so an
   autospawn, should one happen, starts the same OpenD witness; the lock,
   log and idle-shutdown facts the reviewer confirmed are stated. Both
   documents say so.

2. **Major. "The literals stay as they are" failed for the readers.**
   About 25 of the 50 field-naming literals rebuild a scope from a
   persisted record and compare it with the current one; under `opend`
   the rebuilt scope would normalise to `ibkr` and never match. The count
   is 77 (27 empty, 50 with fields), not 76. Response: the design picks
   the first of the reviewer's two options: records stored under `opend`
   carry the kind (they already do, through the key segment and the term
   field), and the readers go through one constructor that takes the
   kind from the record's body, which gains the same `omitempty` field
   as the terms; the zero kind still means `ibkr`. WP8 lists the
   reader sites and the fixture test asserts a record stored under
   `opend` rebuilds to the current scope. The count reads 77.

3. **Major. The typed term was misidentified.** `desk_execution_submit.go:204`
   compares `rpc.DeskAuthorityTerms`, not `rpc.QueuedAuthTerms`; the
   former has a `Kind` field already (the mandate kind). Response: both
   terms gain the `omitempty` `Broker` field written only for `opend`;
   the comparison sites are listed; the fixture test pins both JSONs.
   The fourth review's finding 3 corrected the shape of the three desk
   sites: they compare account and mode field-wise, and now go through
   the constructor.

4. **Major. Two dial paths were not named as gated.** `Start` resolves
   and dials the `[gateway]` endpoint itself (`server.go:1573`, `1642`),
   and the breadth connector builds its own IBKR connector (`1128`,
   `1247`, `1285`, from four call sites); the listening test would catch
   them only by accident. Response: the design names the three families
   and the one constructor they share, `newConnector` (`server.go:1816`),
   which refuses under `opend`; `Start` runs no IBKR discovery or connect;
   the WP8 test drives `Start`, the reads, a breadth question, a reconnect
   trigger, the auto-endpoint path and the app watch while listening.

5. **Major. The trading-status pin block keys on `[gateway]`.**
   `gateway_account_unpinned`, `_unconfirmed`, `_not_concrete` and
   `gateway_client_id_unpinned` (`trading_status.go:134-147`) would fire
   under `opend` with IBKR hints. Response: WP8 makes the whole block's
   pin source per kind, reading `[opend]` and the OpenD session's
   confirmed account, with hints naming `[opend]`; a test asserts no hint
   names `[gateway]` under `opend`. `gateway_account_mismatch` is
   confirmed as the right name for a pinned account the list does not
   report.

6. **Major. The third red test could not go red.** Without `protoc`,
   nothing ties a definition's hash to its generated file's hash, so a
   manifest hand-updated for a changed definition passes; "written only
   by regen" was a convention. Response: the claim is withdrawn. The
   design now states what `futu-proto-check` binds (files to manifest
   lines, headers to generator versions) and that only a regeneration
   proves provenance: `make futu-proto-regen` then `git diff --exit-code`,
   run by the developer before the commit and named in its body, and by a
   CI job whose `protoc` download is refused unless its archive matches a
   checksum in the manifest. WP5 lists the job and the two red tests that
   remain.

7. **Minor. "Read once" against "a changed list drops the session"** with
   no re-read named; the unscoped window and the header-less list request
   were unstated. Response: the status poll re-reads the list inside the
   pacing table; the window between `InitConnect` and the first reply
   refuses trade-class requests typed; `Trd_GetAccList` carries no
   `TrdHeader`.

8. **Minor. Line references.** Response: `broker_write_transaction.go:29-31`,
   `subs.go:164-166`, and the reconnect path spelled as gate, flow,
   failover and candidate loop (`2563-2590`, `2616`, `1892`, `1948`).

9. **Minor. Overstated phrasing.** The spine target simply sits in
   `CI_ONLY_TARGETS` and runs locally; the handbook states requirements
   for a second instance rather than describing a stack. Response: both
   reworded.

10. **Minor. `rpc.AccountDataScope`** was left out. Response: it gains the
    same optional field under the same rule, so an OpenD receipt can never
    match an IBKR scope by construction.

11. **Minor. WP1's "zero value is not current"** depends on reading the
    generation after the increment. Response: stated, with the line.

The reviewer named finding 1 the most severe. The three mechanisms the
second review changed were judged sound as revised (the generation-bearing
binding, the daemon flags and environment variables, the tier table); the
responses to findings 2, 4, 5 and 6 add or change a mechanism (the body
field and constructor for persisted scopes, `newConnector` as the one
refusal point, the per-kind pin block, the checksum-pinned `protoc` job),
so those passages and the witness stack's CLI leg went to a fourth cold
reviewer. Its result is recorded below.

## Fourth review

Reviewer: a fresh session briefed on the four changed mechanisms and the
witness stack's CLI leg, with the third review's record. Review delivered
2026-11-04 12:46 CET: one blocking, five major, four minor; hygiene clean
(gates green, no private terms). Responses applied 2026-11-04 12:55 CET.

1. **Blocking. The witness stack did not isolate the policy files.** The
   rulebook, protection and opportunity policy defaults are
   `~/.config/ibkr/policies/…` expanded through the home directory
   (`internal/config/config.go:208`, `214`, `302`), the risk constitution
   is a hard-coded `~/.config/ibkr/policies/risk-policy.toml` with no
   config key (`risk_policy_manager.go:23`), and `Start` writes templates,
   migrations and backups for all four (`server.go:1542`,
   `policy_files.go:283-288`); none follows the XDG roots. An OpenD
   witness with the six variables would migrate and back up the
   operator's policy files. Response: the stack sets `HOME=<dir>` as its
   seventh variable, both documents say why, and WP12's report quotes the
   scratch policy directory after the run and the unchanged mtimes of the
   operator's files. The handbook's "Running more than one instance"
   section does not name this; the WP9 handbook edit should.

2. **Major. "No reader parses a key" was false.**
   `coreDecisionEventMatchesScope` (`core_rebalance.go:858-869`) splits the
   frozen core scope string and takes its first segment as the account;
   with a leading `opend:` segment no core decision event would match.
   Response: the constructor has a key form that strips the segment
   before the split; the reader uses it; WP8 tests a core decision event
   stored under `opend`.

3. **Major. The desk-terms comparison sites were misdescribed.** The three
   named sites compare account and mode field-wise, not the whole term;
   the whole-value comparisons are bytes and digests elsewhere. Response:
   the design and WP8 give both shapes: the byte and digest sites are
   unchanged (`omitempty` keeps IBKR bytes), and the three field-wise
   sites move to the constructor and `sameBrokerScope` so the kind takes
   part. The third review's record is corrected in place.

4. **Major. A fourth IBKR dial path.** The TWS order control opens its
   client-0 connection itself (`tws_order_control.go:245-265`), not through
   `newConnector`; it is reachable only inside an authorised write
   transaction whose binding carries an IBKR connector. Response: named
   as such in the design and WP8; the listening test adds a TWS cancel
   attempt.

5. **Major. The CI regeneration job fails `ci-gate-scope-check`**
   (`Makefile:361-372`): a `make` target CI runs outside `check` must be
   in `CI_ONLY_TARGETS`. Response: `futu-proto-regen` joins
   `CI_ONLY_TARGETS` with its reason, in the design and WP5.

6. **Major. `[gateway] account` feeds the current scope** at
   `broker_evidence.go:53` and `223` and six other reads; under `opend` the
   current scope would have no account. In `trading_status.go` the origin
   rows (62, 78, 80) and the guards at 153 and 157 also key on it.
   Response: one accessor returns the pinned account of the configured
   kind; the eight reads and the trading-status rows and guards are
   listed in WP8; a test asserts the current scope under `opend` equals
   `[opend] account`.

7. **Minor. `fill_quotes.go:201`** compares an in-memory poll with the
   current status, not a record. Response: removed from the reader list.

8. **Minor. Naming.** The mandate-reduction engine holds an
   `rpc.TradeProposalSnapshot`; the exercise reservation types are
   daemon-local. Response: both named correctly.

9. **Minor. The design's listening test omitted the auto-endpoint path.**
   Response: added.

10. **Minor. The pin block spans lines 134-149.** Response: corrected.

The reviewer named finding 1 the most severe. The responses above add
one variable, one accessor, one constructor form, one `CI_ONLY_TARGETS`
line and one named path; they were not cold-read by a fifth reviewer.
Owner decision 2026-11-04 15:53 CET: no fifth review. The build session
reads those five responses cold as its first step in WP1 and records what
it finds in this file.

## Fifth read: the build session's cold reads of the fourth responses

Owner decision 2026-11-04 15:53 CET: no fifth reviewer. The build session
instead gave each of the fourth review's five responses to a fresh
read-only subagent before WP1, with the mechanism's passages and the
file:line references, asked for defects with line evidence. Reads
delivered 2026-11-04 16:17 to 16:21 CET at `e514c61b` (the design branch
merged with local main); the build session verified each load-bearing
claim against the tree before recording it. Responses applied 2026-11-04
16:30 CET in the WP1 commit.

### The witness stack's own `HOME` (response 1)

1. **Blocking. The brief still described a six-variable stack.** Only the
   design named `HOME`; the brief's gate table listed the four XDG roots,
   `CANARY_CONFIG` and `CANARY_SOCKET` and closed "six environment
   variables and one command". A builder following it would have run the
   witness with the operator's `HOME`, and `Start` migrates and backs up
   the four live policy files (`server.go:1542`, `policy_files.go:283-288`).
   Response: the brief names `HOME=<dir>` as the seventh variable with the
   reason, and says seven.
2. **Confirmed clear.** No durable path escapes the seven variables
   (grep of non-test Go for `UserHomeDir`, `UserConfigDir`, `Getenv("HOME")`
   and `.config`): `config.toml` follows `CANARY_CONFIG`, else
   `XDG_CONFIG_HOME`, else `$HOME/.config` (`config.go:429-438`); the
   state, cache and socket families follow their roots; the four policy
   files and their backups follow `HOME`. Autospawn inherits the caller's
   environment (`autospawn.go:361`). `os.UserConfigDir` is not used.
   `HOME` also governs the flex-token and ledger-token defaults, so the
   witness reports reporting evidence absent by construction, which is
   right.
3. **The Desk architecture review's D2** (policy files under
   `$XDG_CONFIG_HOME/ibkr/policies/`, no new key) is safe for this Mac's
   daemon, which runs with `HOME` and no `XDG_*` variables and no
   LaunchAgent, so the paths resolve to today's `~/.config/ibkr/policies/`;
   it is a behaviour change for any operator whose daemon environment sets
   `XDG_CONFIG_HOME` elsewhere (their policies would be orphaned and fresh
   templates written) and needs a one-time migration note. It does not
   retire the seventh variable. The design's owner-decision table gains the
   row; the witness does not depend on it.

### The per-kind account accessor (response 6)

1. **Major. An accessor that returns an account cannot fix the mode.** All
   eight reads feed `brokerStateScopeFromSnapshot` (nine callers with
   `fill_quotes.go:658`), which fixes `Mode` through the free
   `accountModeForStatus` (`DU` prefix, else the IBKR ports). An OpenD
   `accID` is never `DU`, so every site would class an OpenD scope `live`
   whatever its `trdEnv`; a simulate account would pass the live endpoint
   check at `trading_status.go:157`. "`accountModeForStatus` becomes a
   broker method" is unreachable from a free function called by a free
   function. Response: the design replaces the accessor with a per-kind
   identity built once from config and held on the `Server` (kind, pinned
   account) and one scope function that takes the session observation:
   under `ibkr` the endpoint account, port and connected account as today;
   under `opend` the `trdEnv` the session's account list reported for the
   pin, or unknown before it. `currentBrokerStateScope` and the nine
   snapshot callers call that function; `accountModeForStatus` keeps its
   IBKR body as the `ibkr` arm.
2. **Major. Account-concrete is not scope-concrete.** `brokerScopeConcrete`
   requires paper or live; under `opend` the mode arrives with
   `Trd_GetAccList`, so a config-only scope is account-concrete and
   mode-unknown until the list reports the pin. Response: the design
   states both states and WP8's test pins both (the account from config
   before any session; the mode from the list after).
3. **Minor. The config shape.** The design's `[opend]` table does list
   `client_id` (the `InitConnect` `clientID` string, a label Canary
   chooses), but the trading-status client-id blockers
   (`gateway_client_id_unpinned`, `_mismatch`) have no OpenD reading:
   OpenD assigns `connID` itself. Response: under `opend` the two client-id
   blockers never fire; stated in WP8.
4. **Minor. Unlisted mode and connector reads.** `accountModeForStatus` is
   also called from `handlers.go:3709`, `broker_permissions.go:310` and
   `:409`, and `fill_quotes.go:318`, `:534`, `:561` on a stored account
   alone; `trading_status.go:125-127` fires `gateway_unavailable` through
   `gatewayConnector()` with IBKR wording, and the host, port and origin
   rows at `trading_status.go:54-61`, `71-76`, `84-90` read `[gateway]`.
   Response: all listed in WP8 as per-kind; a stored OpenD record's account
   equals the pin, so its mode is the session's observed `trdEnv` or
   unknown.
5. **The `Scope()`-on-the-broker-instance alternative** (Desk review,
   section 3) was weighed and rejected: the instance has two lifetimes (per
   IBKR connector, discarded with it; constructed at start under `opend`),
   and seven sites need a scope with no connector at all
   (`mandate_verdict.go:170-184`, `alert_shadow_cadence.go:452-470`,
   `handlers.go:3739-3743`, `trading_status.go:62-65` and `138-143`,
   `broker_evidence.go:224-227`, `protection_order_snapshot.go:72-85`).
   The identity lives on the server and the adapter contributes only the
   observation; a later multi-broker daemon moves the identity into the
   binding beside connector and epoch, which the write binding already
   carries, so these files change once more only at the identity's source.

### The key-form scope constructor (response 2)

1. **Major. Twelve sites build the scope from `rpc.TradingStatus`**, which
   carries no kind: `desk_execution.go:55`, `order_risk_authority.go:236`
   and `:328`, `exercise_reservations.go:141`, `protective_exit.go:402`,
   `protective_oca_trading.go:37`, `tws_order_control.go:203`,
   `delta_reduction.go:643`, `fill_quotes.go:676`,
   `broker_permissions.go:310` and `:409`. Under `opend` they would
   normalise to `ibkr` and never match a record whose body says `opend`.
   Response: `rpc.TradingStatus` gains the additive `Broker` kind in WP8
   (WP9 adds `BrokerCapabilities` beside it) and these sites go through a
   status form of the constructor.
2. **Major. A third key shape: hashed identities with validators.**
   `marketEventFeeRateScopeFingerprint` (`market_events_fee_rate.go:1021`)
   hashes account and mode into `sha256:<hex>` and its store validates the
   format (`market_events_fee_rate_store.go:131-132`, `172-174`); the same
   family holds `risk.BuildAlertAuthorityScope`
   (`internal/risk/alert_candidate.go:309-342`), `riskCapitalScopeKey`
   (`risk_capital_state.go:240-245`), `recon_engine.go:273`,
   `cash_sweep_trace.go:82-83`, `dailyPnLScopeSource`
   (`daily_pnl_close_capture.go:31-33`) and `edgeScopeFingerprint`
   (`edge_authority.go:496-504`, which the design had under string keys).
   A leading segment breaks every validator. Response: hashed shapes take
   the kind as an extra hash input under a non-IBKR kind, empty for IBKR
   so the bytes are unchanged; formats and validators untouched.
3. **Major. The `|` key and a second `:` parser.** The frozen-scope key is
   built in `strategy_target.go:460` and `:469`
   (`strategy|<id>|<account>|<mode>|<con>`), reused by
   `strategy_kill_live.go:157`; `coreDecisionEventMatchesScope` strips
   `strategy|<id>|` first and then splits. `core_rebalance.go:271` builds a
   `:`-joined episode key and `:281` trims its `core:` prefix. Response:
   the segment goes inside the account field, never in front of a family
   prefix: `|` keys read `strategy|<id>|opend:<account>|<mode>|<con>`,
   `:` keys read `core:opend:<account>:<mode>:<month>`, and the key form
   of the constructor cuts the account field at its first `:` (an IBKR
   account never contains one; the fixture asserts it).
4. **Major. Typed terms undercounted.** `desk_override.go:107` rebuilds
   from `rpc.DeskOverrideTerms`, not `QueuedAuthTerms`; and
   `rpc.LevelingBundleTerms` (`proposal_currency_leveling_bundle.go:213`,
   `:233`) was missing. Response: four terms gain the `omitempty` `Broker`
   field (`DeskAuthorityTerms`, `QueuedAuthTerms`, `DeskOverrideTerms`,
   `LevelingBundleTerms`) plus `AccountDataScope`.
5. **Major. Readers that derive a mode from a stored account by the IBKR
   rule**: `fill_quotes.go:318` and `:534`, `broker_permissions.go:310` and
   `:409`. Response: the per-kind mode function above; listed in WP8.
6. **Minor. "16 files" is 23** non-test daemon files that write account and
   mode into a key, fingerprint, journal line or typed term, plus
   `internal/risk`; the literal count reads 76 (27 empty). Not load
   bearing; the design says 23. `orderJournalRouteIdentity`
   (`order_read_model.go:1832-1836`), in-memory and endpoint-qualified, is
   order-path and stays with phase D.
7. **`<kind>:` rather than the literal `opend:`**, as the Desk review
   suggests: the one helper writes the kind value for any non-IBKR kind.
   Sort order is not a hazard: no prefix scan or `ORDER BY` over an
   account-bearing key exists.

### The `CI_ONLY_TARGETS` line and the protobuf gates (response 5)

1. **Major. One Go package cannot hold the vendored files.** Every Futu
   definition declares `package <File>` and a `go_package` under an
   unresolvable path, and most declare top-level `Request`, `Response`,
   `C2S` and `S2C`; `protoc-gen-go` would redeclare `Request` 26 times.
   Response: one Go package per vendored file under
   `pkg/futu/internal/pb/<lowername>/`, generated with
   `--go_opt=module=github.com/osauer/canary/v2` and one
   `--go_opt=M<File>.proto=<importpath>` per vendored file (the imported
   `Common`, `Qot_Common`, `Trd_Common` included), derived from the
   vendored list; the vendored files stay byte-identical to upstream.
2. **Major. Two version strings.** `protoc --version` prints
   `libprotoc 32.1`; the generated header carries `protoc v5.32.1`-style
   text from the plugin request. Response: the manifest carries both, the
   regen compares the first and `futu-proto-check` the second.
3. **Major. The analysers see generated code.** `go-doc-audit` skips
   generated files, gofmt is satisfied, but `vet`, `staticcheck`,
   `modernize` and `govulncheck` take every tracked package, and the
   modernize analysers at the pinned `x/tools` do not filter generated
   files. Response: WP5's done includes those legs green on the generated
   output; the remedy if one is red is to exclude
   `./pkg/futu/internal/pb/...` from the analyser package list, named as a
   change outside WP5's file set. Proof pending the first regeneration.
4. **Minor.** Two pins of `google.golang.org/protobuf` (tool and runtime):
   `futu-proto-check` compares them with the manifest. The plugin is
   resolved as `go -C tools tool -n protoc-gen-go` and passed with
   `--plugin`; `protoc` comes from `PATH`. The CI step is `run: make
   futu-proto-regen` on its own line (the scope check's regex is
   line-anchored) with a separate `git diff --exit-code` step. The regen
   rewrites file hashes and version lines and preserves the hand-kept
   archive checksum lines, else the Linux job's diff is never clean.
5. **Confirmed.** `ci-gate-scope-check` parses `ci.yml` for `make <target>`
   lines and requires each in `check`, `CHECK_TEST_LEGS` or
   `CI_ONLY_TARGETS`; `CHECK_TARGETS` is an open list, so
   `futu-proto-check` joins it with no `ci.yml` line; the dependency
   script's `expected_direct` has seven entries and a literal count;
   `pages-coverage` runs from `pages-deploy.yml`, which the scope check
   does not parse.

### The TWS order-control dial path (response 4)

1. **Major. The gate was named below the dials.** `reconnectFlow` calls
   `discover.Resolve` at `server.go:2627` and `2671` before
   `connectWithFailover`; with `[gateway] port` unpinned that probes all
   four standard ports (`discover.go:115-122`, `211`, `313`), a TCP connect
   `newConnector` never sees, and on no listener `classifyClosedLocalPort`
   runs `ps` and starts the app-watch poller, whose poll calls
   `triggerReconnect` again. `Start` has the same shape. Response: the gate
   is one early `return false` in `triggerReconnect` and the `opend` branch
   of `Start`; `newConnector` is the backstop, not the mechanism.
2. **Major. Six callers of `triggerReconnect`**, not two: `gatewayConnector`
   (`server.go:2812`), `gateway_app_watch.go:300`, `handlers.go:3610` (the
   status read), `order_risk_authority.go:84`, `answer_path.go:221` and
   `:274`. Gating inside `triggerReconnect` covers all six.
3. **Major. `newConnector` cannot refuse with its signature.** A nil
   `*ibkrlib.Connector` wrapped in `connectAttempter` is a typed nil and
   `tryOneHandshake` would call `Start` on it. Response: the signature
   becomes `(*ibkrlib.Connector, error)` with both callers
   (`buildAttempter`, `breadthConnectFlow`) handling the error.
4. **Minor.** The TWS refusal is `broker_write_transaction.go:252`
   (`connector == nil || !sessionCaptured`), not `:29-31` (struct fields);
   the test seams at `:247-251` and `twsOrderControlRun` must stay nil in
   the listening test. The claim narrows to "never opens the IBKR API
   socket": the Flex fetch (HTTP, gated by `cfg.Flex.Enabled`), the
   borrow-fee FTP dial and the Web API cash ledger are IBKR hosts on other
   ports; the first two are gated by kind in WP8 and WP9. The breadth flow
   is unreachable because `s.breadth` is not constructed under `opend`.
5. **Test shape.** Not bind-or-skip on the four standard ports: on this Mac
   the skip would remove the test exactly where a wrong gate dials the live
   TWS. Four `127.0.0.1:0` listeners whose `Accept` fails the test,
   injected through `discover.StandardPorts` (the package variable, with
   the restore pattern of `auto_endpoint_test.go:283-288`), `[gateway]
   port` unpinned so the probe runs, `discover.Probe` and
   `s.attempterFactory` not replaced, `CashLedger.URL` empty,
   `Flex.Enabled` false; no `t.Parallel()` beside other discover-reading
   tests.

Of the five responses, four changed a mechanism again (the accessor, the
constructor, the protobuf layout, the dial gate); only the `HOME` variable
was right as written and wrong only in the brief. These responses were
written by the build session and were not cold-read; the build proves them
by the tests the brief names. One correction to item 3 of the constructor
read followed in WP2 (2026-11-04 17:05 CET): "cut the account field at its
first `:`" describes the `|` keys only; a `:`-joined key has no delimited
account field, so there the rule is that the segment after the family
prefix is a kind when it equals a known non-IBKR kind name. The design and
the brief say both.

One correction to item 2 of the protobuf read, from WP5's first generation
(2026-11-04 20:05 CET): the generated header for `libprotoc 32.1` reads
`protoc v6.32.1`, not `v5.32.1`. The manifest carries what the plugin
reported, and the design and the brief are corrected. The rule, two version
strings with each check reading its own, stands.


## The multi-broker review applied

On 2026-11-04 16:06 CET a separate read-only review asked whether the
design forecloses running two brokers in one desk, and found that it
does not: one broker per daemon, no switch in flight, no aggregated view,
and "routing" meaning which daemon's socket an order goes to. The owner
decided on 2026-11-04 16:01 CET: one daemon per broker, with aggregation
and routing in Desk (the review's option A). The review's section on
phases A to C named the changes that keep that option cheap and the
alternatives open; the parent session relayed them to the build session
at 17:48 CET and they were applied to the design and the brief at 17:54CET, in the work packages named. Each item, with its disposition:

1. **The topology decision.** Recorded in the design's "Owner decisions"
   table with the time; the table had no topology row.
2. **Phase C1, policy paths.** The four policy files (the risk
   constitution at `risk_policy_manager.go:23`, the three defaults in
   `internal/config/config.go` at lines 208, 214 and 302) resolve through
   the home directory. WP8 moves them onto the XDG config root
   `config.DefaultPath` already uses, no new key: the operator's paths
   stay identical (this Mac's daemon sets no `XDG_*` variable) and only an
   isolated stack moves. The reason is a permanently running second daemon
   under its own LaunchAgent, not only the witness. The design's owner
   decision row for this path, previously open, now follows the topology
   decision; the witness keeps its own `HOME` for paths that still resolve
   through it.
3. **Phase C1, the scope accessor.** The brief's process-global accessor
   and per-kind `accountModeForStatus` become a method of the broker
   instance: `broker.Broker` gains `Scope()` (kind, account, mode) and
   `currentBrokerStateScope` reads it. The design had weighed and rejected
   this on 2026-11-04 (fifth read, response 6) because the instance was
   per IBKR connector and seven sites need a scope with no connector; the
   objection falls once the daemon holds one instance per process with
   the daemon's lifetime under both kinds, which the design now says, so
   a daemon that later holds N brokers changes the identity's source only
   and never re-touches the 81 scoped files. That earlier rejection is
   withdrawn here, with the reason.
4. **Phase C1, key helper wording.** Every helper writes the leading
   kind segment and the `Broker` field for any non-`ibkr` kind, never for
   `opend` by name; the bytes under `opend` are the same today. Design and
   brief say so.
5. **Phase C4, `InstrumentID`.** It carries no listing symbol. The design
   states the join for netting one stock across brokers: the row's own
   `Symbol` with `InstrumentID.Venue`, filled by both adapters for every
   row; WP10's tests hold it. No field added.
6. **Phase C3, `HealthResult.Broker`.** Kept as a stable field; Desk
   binds it as the order route. Noted in the design and WP9.
7. **The autospawn hazard with two daemons.** A client that finds no
   socket autospawns a daemon from the default config
   (`canary.go:35-37`, `internal/dial/autospawn.go`), so a second socket
   that is down silently becomes the wrong daemon. WP12's dual-daemon
   witness (the owner's ask of 2026-11-04 16:03 CET, now written into the
   brief beside the scope and config tests of WP8) proves that a down
   OpenD socket with autospawn on, under the OpenD stack's environment,
   spawns an OpenD daemon and never an IBKR daemon in its place, and the
   architecture handbook names the environment a second client must
   carry.

Not applied here, because they are the owner's and open: whether the
second daemon runs permanently under its own LaunchAgent on this Mac,
whether the Futu book is a sleeve under one risk budget or a second
mandate with its own, whether one freeze and mode switch cover both books,
and whether one device key is enrolled in both constitutions. Nothing in
phases A to C depends on them; the review's recommendation is option A as
written, which the owner chose.

## Corrections from building WP8

Written 2026-11-04 while the daemon went onto the seam. Each changes a mechanism
the documents stated, and none changes what they promise.

1. **The hashed identities take the kind inside the account.** The documents had
   an extra hash input. One helper, `keyAccount`, feeds both the string keys and
   the hashes: IBKR's input is byte-identical, and a non-IBKR kind differs. A test
   pins the bytes of 18 families and fails if any two brokers collide.
2. **The IBKR scope is per daemon and its reads per connector.** `broker.Broker`
   is `Reads` plus `Scoper`. The OpenD adapter is both; IBKR's adapter stays per
   connector, because a binding needs that, and a small per-daemon `ibkrScope`
   supplies the scope. A daemon holding N brokers changes that source only.
3. **The breadth engine stays built under OpenD.** `Start` attaches its store, so
   the gate is on its connector and its connect trigger, not its construction.
4. **Order-path readers stay for phase D.** `fill_quotes.go` still derives a mode
   from a stored account, and `proposal_prepared.go` builds a scope without a
   kind. Under OpenD no order or fill exists, and the second fails closed.
5. **`rpc.QueuedAuthTerms` is a signed contract with Desk.** Its allowlist test
   now lists `broker`, absent on every IBKR term. Desk's and the companion's lists
   need it before phase D signs a term under OpenD; the design's owner table says so.

## What the witness found building WP12

Written 2026-11-04 22:20 CET. The first changes what the design promised; the rest are readings a
reviewer should see.

1. **The regime is off under OpenD.** The design had its broker-bar inputs served
   through the history quota. WP11 built the lines; the engine does not consume them,
   and it returns before the fan-out with no IBKR connector. This is a gap against the
   design, not a correction of a mechanism: the design now says so, and the owner table
   proposes a package of its own.
2. **Portfolio stress follows the regime.** `canary stress` answers unavailable, and
   the app's card reads "Fault · transport unavailable". That reads as broken where the
   design wants off. The card's words are left for the owner with phase D's surfaces.
3. **The app's source count sits above its gaps.** The Monitor shows "22/22 sources ok"
   directly above a line of unavailable items, because a source that is off for this
   broker does not count as failing. What that count means is a label decision, left
   open.
4. **The rulebook names data gaps.** The simulator's book shows the regime stage and
   the earnings date unobserved, no FX rate for holdings outside the base currency, no
   excess liquidity for margin headroom, and the risk-capital numbers that belong in
   the owner's own policy file. Each reads as a gap, none as a pass.
5. **An OpenD daemon could report that it may write.** Written 2026-11-04 22:48 CET. Write
   authorisation had no check of the broker, so a paper-mode daemon on a simulate
   account in a trading build reported `can_write` true; only the missing IBKR
   connector stopped a send later, in the transaction binding. The witness's
   `write=false` came from its disabled mode and proved nothing. Every origin is now
   refused by `broker_writes_not_served`, and a test drives each one in paper mode.
