# OpenD build: handoff note

Updated: 2026-11-07 07:17 CET. Branch `cc-wt-/opend-build-27fd07`, cut
from the design branch at `b201852e` and merged with local `main`
(`e514c61b`). Follows
[broker-opend-build-brief.md](broker-opend-build-brief.md) against
[broker-opend.md](broker-opend.md). This note is kept current after every
work package so a fresh session resumes without the conversation.

## First real session (2026-11-07 07:10 CET)

Read-only, OpenD 10.12 build 7208, serverVer 1012, a Canary build without the trading tag, isolated roots. A quarantined OpenD download runs on macOS from an App Translocation copy, so the installed path is not the running path.

Where real OpenD disagreed with the simulator:

- In the real environment the account list omits universal accounts unless `needGeneralSecAccount` is set, so a pin to one stayed unscoped. Fixed: `accountListRequest` asks, the simulator hides real accounts otherwise, `TestRealAccountIsListedAndScoped` failed before the fix.
- OpenD does not push quote rights on connect, so status had no quote-rights row. Fixed: each session reads them once with `GetUserInfo`, flag `QotRight` alone, so the login's name and user id are never sent. `GetUserInfo.proto` is vendored from 10.12.7208 beside the older definitions, and level 6 (`Level3`) is mapped by hand because the vendored `Qot_Common` stops before it.
- Singapore: moomoo's authority page lists Singapore quotes as unsupported, so README and changelog no longer claim them.
- SPY is `AMEX` in `exchType` (the fixture said NYSE, now AMEX). IBKR is expected to say `ARCA`, not read here, and the two would not join: recorded for Desk's parked multi-book work, nothing to do on this branch.
- `listTime` reads 1970-01-01 for several instruments. `Instrument.Listed` has no reader today.

Confirmed on the real OpenD: `clientVer` 334 is accepted; a US ETF and an HK ETF are `Trust`; an option is `Drvt` with lot size 100; a future is `Future` with lot size 50; an expired option answers `Unknown` with "Unknown stock.", no error, and `delisted` true; snapshots work without the new optional `QotHeader`; update times are right in UTC; the history quota window is seven days, as moomoo's page says. Refreshing the vendored definitions to 10.12.7208 is not needed: nothing used changed, and every difference is an addition.

Defects seen on both real and simulator, fixed the same day:

- `market.snapshot`, `portfolio.snapshot`, `market.tape`, `technical.snapshot`, `chain.fetch`, `chain.expiries`, `gamma.zero_spx` and `core.resolve` read the IBKR connector and answered `gateway_unavailable` with OpenD up. They are now in `ibkrOnlyMethods`. The list came from calling every non-streaming method with OpenD up, with empty and with one-symbol parameters, so a method that refuses those parameters earlier was not reached.
- The market tape joins on IBKR contract ids, so each pass under OpenD spent history quota on names it then discarded, and warned. The collector no longer starts under OpenD; a simulator status read history 0 of 100 afterwards.
- The earnings path no longer installs the WSH provider under OpenD, which warned per held name.
- The CLI hint names OpenD, and the readiness blocker's code is `broker_writes_not_served` while the session is up, `gateway_unavailable` only when it is down.

Everything not listed above has run only against the simulator.

## Rules in force

- The multi-broker review of 2026-11-04 (owner decision 16:01 CET: one
  daemon per broker, aggregation and routing in Desk) was applied to the
  design and the brief at 17:54 CET; WP8 builds to `Broker.Scope()`, the
  policy files on the XDG config root and the kind-neutral key helpers,
  WP10 to the symbol-and-venue join, WP12 to the dual-daemon witness with
  the autospawn proof. The review record lists the seven items.
- One commit per work package, in the brief's order; each commit body names
  the package, the gate command, the HEAD it ran at and the stamp line.
- The IBKR path changes no behaviour; `pkg/ibkr` and the IBKR goldens stay
  byte-identical except the `Capabilities` row's broker leg (WP9).
- `pkg/futu` coverage at or above 80 percent from WP4 on, measured on
  `./pkg/futu` alone inside `test-pkg`.
- Never `make restart-daemon`, never the live daemon, socket or config; the
  OpenD witness is the simulator stack with its own `HOME` and XDG roots.
- Focused raw runs: `GOMAXPROCS=2 go test -p 1 -parallel 2 ...`; one full
  gate at a time on this shared machine.
- No merge to main, no push, no publication, no Desk pin bump.

## Landed

| WP | Commit | Gate | Stamp |
| --- | --- | --- | --- |
| (before WP1) the no-room mandate render fixture's synthetic capital moved away from the live book; the account-data gate's two-percent band for files not yet on origin/main had caught it and the pre-commit hook refused every commit | `0b074572` | `check-no-account-data.sh` on the index, `make test` | `gate test at canary--v3.33.1-9-ge514c61b-dirty exit 0` |
| WP1 `internal/broker` types, the fifth read recorded, design and brief corrected | `599cf806` | `make test` | `gate test at canary--v3.33.1-9-ge514c61b-dirty exit 0` (one run, both diffs in the tree) |
| WP2 IBKR adapter (session, quotes, bars) and the daemon seams on it | `7ea49dd3` | `make test` on the tree before the commit; `make regression-spine-check` on `7ea49dd3`; side check `git diff --exit-code HEAD -- internal/cli/testdata/golden pkg/ibkr` empty | `gate test at canary--v3.33.1-11-g599cf806-dirty exit 0`; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 147s elapsed ... exit 0` |
| WP3 IBKR adapter (account, positions) and the data-health recorder on the seam | `685871bb` | `make test` on the tree before the commit; `make regression-spine-check` on `685871bb`; side check empty | `gate test at canary--v3.33.1-12-g7ea49dd3-dirty exit 0`; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 134s elapsed 361.6s user 67.0s sys exit 0` |
| WP4 `pkg/futu` frames, limits, errors, the simulator book and the test legs | `939c4e6c` | `make test` on the tree before the commit; `make test-pkg` (floor line `test-pkg: pkg/futu statement coverage 98.7% (floor 80%)`); `make ci-gate-scope-check`; account-data gate on the staged fixture (OK) | `gate test at canary--v3.33.1-13-g685871bb-dirty exit 0` |
| the multi-broker review applied to the design and brief (seven items; the review record lists them) | `12fc8efe` | `make account-data-check product-identity-check`, `make docs-check` on the staged tree | `check-no-account-data: OK`, `check-product-identity: OK` |
| WP5 protobuf: vendored, generated, manifest, gate, the owner's acceptance recorded | `09f3ee8c` | `make test` on the tree before the commit; `make futu-proto-regen` twice, manifest unchanged; the release CI contract checks | `gate test at canary--v3.33.1-15-g12fc8efe-dirty exit 0`; `futu-proto: OK (28 definitions; protoc-gen-go v1.36.12; protoc v6.32.1)` |
| WP6 `pkg/futu` session and the simulator server | `ef3ae1e2` | `make test` on the tree before the commit | `gate test at canary--v3.33.1-16-g09f3ee8c-dirty exit 0`; `test-pkg: pkg/futu statement coverage 94.4% (floor 80%)` |
| WP7 `pkg/futu` reads and their simulator handlers | `2928cd46` | `make test` on the tree before the commit | `gate test at canary--v3.33.1-17-g621b17f1-dirty exit 0`; `test-pkg: pkg/futu statement coverage 93.2% (floor 80%)` |
| WP8 the daemon on OpenD | `3c922e70` | `make test` on the tree before the commit; `make regression-spine-check` on `3c922e70` | `gate test at canary--v3.33.1-18-g2928cd46-dirty exit 0`; `regression-spine: OK (32/32 historical regressions killed)` |
| WP9 surfaces | `18512599` | `make test` on the tree before the commit | `gate test at canary--v3.33.1-19-g3c922e70-dirty exit 0` |
| WP11 quota budgeting and snapshot delivery (WP10 waits for the identity ack) | `2b2d12b7` | `make test` on the tree before the commit; spine on the landed revision | `gate test at canary--v3.33.1-20-g18512599-dirty exit 0`; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 131s elapsed 361.2s user 72.6s sys exit 0` |
| WP12 `cmd/opend-sim`, the witness and the dual-daemon witness | `367b018a` | `make test` on the tree before the commit; `make account-data-check product-identity-check` | `gate test at canary--v3.33.1-21-g2b2d12b7-dirty exit 0`; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 128s elapsed 359.3s user 73.0s sys exit 0` |
| Merge of local main (standing authority, hedge rule, send order) | `96446ed2` | `make test` before the commit | `gate test at canary--v3.33.1-22-g367b018a-dirty exit 0` |
| OpenD fails closed: no broker write, no IBKR port, no default path | `acec8305` | `make test` before the commit; spine on the landed revision | `gate test at canary--v3.33.1-31-g96446ed2-dirty exit 0` |
| WP10 `rpc` instrument identity, the owner's ack of 2026-11-05 07:47 CET recorded | `e5cf6ec8` | `make test` before the commit | `gate test at canary--v3.33.1-32-gacec8305-dirty exit 0`. The account-data money check ran OK on that tree. Its commit body says "first time since WP8"; the correct wording is first since the owner's stop of 2026-11-04 20:38 CET, which WP8 to the merge all followed. |
| Merge of local main (the held reduction batch fix `01668714`) | `28119a91` | `make test` and `make regression-spine-check` on `28119a91`; `scripts/opend-witness.sh` on its build | `gate test at canary--v3.33.1-35-g28119a91 exit 0`; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 662s elapsed 429.0s user 126.8s sys exit 0`; `opend-witness: OK: operator paths unchanged`. The money check skipped on this run: `the daemon has no live account data (gateway_unavailable: IBKR connection unavailable)`. |
| Merge of local main at `cac66db9` (v3.34.1), the owner's instruction of 2026-11-05 14:40 CET | `d983af3e` | `make test` before the commit; `make regression-spine-check` and `scripts/opend-witness.sh` on `d983af3e` | `gate test at canary--v3.33.1-41-g9c28e5b6-dirty exit 0`, money check OK; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 195s elapsed 402.8s user 80.4s sys exit 0`; `opend-witness: OK: operator paths unchanged` |
| Options and futures no longer read as stocks, the owner's request of 2026-11-05 15:05 CET | `b3f0ea60` | `make test` before the commit; `make regression-spine-check` and `scripts/opend-witness.sh` on `b3f0ea60` | `gate test at canary--v3.34.1-28-g844989d2-dirty exit 0`; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 134s elapsed 366.2s user 73.5s sys exit 0`; `opend-witness: OK: operator paths unchanged` |
| A lost OpenD session names OpenD, the owner's request of 2026-11-05 08:37 CET, on local main at `82e5599a` | `85db5500` | `make test` before the commit; `make regression-spine-check` on `85db5500` | `gate test at canary--v3.33.1-38-g04a34a7f-dirty exit 0`, money check OK against the running daemon; `regression-spine: OK (32/32 historical regressions killed)`, `leg regression-spine 184s elapsed 367.0s user 66.1s sys exit 0` |

The fifth read (five cold subagent reads of the fourth review's responses)
is in the review record under "Fifth read". Four of five mechanisms
changed: the per-kind identity lives on the `Server` with a scope function
over the session observation; the key form cuts the account at its first
`:` and the kind goes inside the account field, hashed shapes take the kind
as an extra hash input, four typed terms and `rpc.TradingStatus` gain
`Broker`; protobuf is one Go package per vendored file; the IBKR dial gate
is in `triggerReconnect` with `newConnector` returning an error. The brief
and design carry these; WP5 and WP8 build to them.

## How WP2 is wired (what a fresh session must know)

- `Server.brokerReads()` is the seam accessor: nil exactly when
  `gatewayConnector()` is (same readiness rule, same reconnect arming).
  The adapter is built lazily per connector in `adapterFor` under `s.mu`,
  not at the publication site, because many tests seat `s.connector`
  directly; `brokerGeneration` counts adapters, so a binding of a
  rebuilt connector's predecessor is never current.
- The daemon runs connections without the client's auto-reconnect
  (`newConnector`), so a lost session is always followed by a rebuild:
  the "same adapter, new epoch" case cannot occur in the daemon and the
  adapter's epoch table holds one live binding at a time. The daemon
  tests prove the lost-session and rebuild cases; the adapter's unit
  tests prove the table.
- The subscription manager (`subs.go`) holds `marketQuotes` (the seam's
  `Quotes` plus `Current`); fakes must issue a non-zero binding
  (`fakeSession`), since a zero binding is never current.
- `handleMarketSnapshot`'s front-future enrichment and
  `setup_options.go`'s option listing stay on the connector by design
  (IBKR-only paths); `setup_options` resolves the underlying through the
  seam with `BindingFor` so the resolution memo is shared with the
  evaluation path.

## How WP3 is wired

- The account snapshot (`account_authority.go`) reads through
  `accountReader` (the seam's `Session` plus `Account`); its source is the
  reader and the neutral binding, and the snapshot holds
  `broker.AccountSummary` with `broker.Provenance`. The daemon-wide
  provenance (`accountSummaryAuthority.Provenance`, the rulebook, the cash
  ledger, the intraday sampler) is `broker.Provenance`; the string values
  are the client's. The ledger helpers (`buildCurrencyExposure`,
  `mergeCurrencyLedgers`, FX repair and harvest, `annotateLedgerCash`,
  `captureReconValuation`'s summary) take `broker.CurrencyLedger`; the
  two stream caches the handlers still read on the connector
  (`CurrencyLedgerSnapshot`, `CachedAccountSummary`) are mapped at the
  site with `brokerIBKR.Ledger` and `brokerIBKR.Summary`.
- `BrokerReadSession.SocketEpoch` is the client's own socket epoch, a
  wire value: the account handler gets it back with `Adapter.Concrete`
  (the side table's lookup, exported for this one IBKR-only consumer).
- The positions handler reads `[]broker.Position` and
  `broker.PositionsHealth` from `Adapter.Positions` and converts the
  receipt once (`brokerIBKR.StreamHealth`) for the daemon's shared
  classification helpers, which stay on the client's struct across the
  order and rulebook paths; the two structs are field for field the same
  and a test holds them so. The scope boundary is shared:
  `heldOutsideBrokerScope` is the rule, `cachedPositionsMatchBrokerScope`
  (order integrity, raw rows) and `positionsMatchBrokerScope` (the
  handler, seam rows) apply it.
- The data-health recorder (`dataHealthObservation`, `recordDataHealth`,
  `recordFeedHealth`, `observeAccountRPC`, `observeEventHealth`,
  `observeHistorySource`) stores `broker.Session` and the neutral
  binding; a row is a previous-session observation when the session is
  nil, the binding is not current, or `Health().BackendDown`.
  `observeQuoteHealth` takes the adapter, since the entitlement rows
  still come from the client's absence table. The bridge for concrete
  captures is `BindingFor`; `sessionOf` turns a nil adapter into a nil
  `Session`, never an interface around a nil pointer (that would read as
  broker-bound).
- Untouched by design: the daily P&L stream, the settlement cash probe,
  the order and trading paths, `readOrderIntegrityCachedPositions`.

## How WP4 is wired

- `pkg/futu` has no protobuf yet: `Frame` carries body bytes; `EncodeFrame`,
  `WriteFrame` and `ReadFrame` own the 44-byte header, the SHA-1 check and
  a 16 MB body cap (`MaxBodySize`, the same cap `pkg/ibkr` reads under).
  Every malformed header is `ErrFrame`; a short read is `io.ErrUnexpectedEOF`.
- `Limits` is the one pacing table (protocol, requests per 30 s, items per
  request, page, date read). Every row was read on 2026-11-04 from the
  interface page it names: 60 per 30 s and 400 per request for the
  snapshot; 60 per 30 s for history candles (first page only, per the
  page); 10 per 30 s per account for funds, positions, max quantities and
  the four order and fill lists; no stated frequency for the rest. The
  trade unlock (2005) has no row and no constant; `LimitFor(2005)` is
  false and a test holds it so. `Pacer` is a sliding window per protocol
  on an injected clock; `futusim` imports the table and the quota rules
  (`SubscriptionMinimumHold`, `HistoryQuotaWindow`,
  `SubscriptionQuotaTiers`), which is the "two read one source" tie.
- `futusim.Book` is the state behind the WP6 server: `Default()` embeds
  `testdata/book.json`; the quote walk is `rand/v2` PCG seeded from the
  book seed and the code, stepped once per open trading second of the
  instrument's market (HK, US, SG session tables with `time/tzdata`), so
  a closed market holds its price and two books on one clock agree.
  Orders fill at the touch (market) or the limit (limit) on `Settle()` and
  book into positions and cash; a locked real account refuses every order
  and nothing unlocks it. Quotas: subscription lines with the one-minute
  hold, the seven-day history window with free repeats.
- `test-pkg` runs `./pkg/futu/...` under `-race` and then the floor step
  (`go test -coverpkg=./pkg/futu ./pkg/futu`, `awk` on the `total:` line,
  fails under 80); `pages-coverage` writes the `pkg-futu` trio beside
  `pkg-ibkr` in one loop. No new target, so `ci-gate-scope-check` is
  unchanged.
- For WP6: the changelog page (read 2026-11-04) lists OpenD 10.10.7008 of
  2026-08-13 under a documentation banner of v10.12; re-read it before
  declaring `minServerVersion` rather than taking either number.

## How WP5 is wired

- `scripts/futu-proto.sh` holds both actions; `make futu-proto-check` (a leg of `check`) runs its test and the check, `make futu-proto-regen` regenerates. The check names `Trd_UnlockTrade.proto` and refuses it. The manifest's comments and archive lines are hand-kept.
- Vendored: the brief's 26 definitions plus `Trd_GetHistoryOrderList` and `Trd_GetHistoryOrderFillList`, which the design's reads table and the pacing table name. Upstream is `FutunnOpen/py-futu-api` at `9bac9d8b` (2026-03-31, 10.2.6208); `NOTICE` names it. All 28 are byte-identical to upstream, compared after copying.
- Generated: `pkg/futu/internal/pb/<lowername>/`, nothing imports it yet. The header reads `protoc v6.32.1` for `libprotoc 32.1`; the design and brief had said v5.
- `./pkg/futu/internal/pb/` leaves the modernize analyser's package list only; `go fix`, `vet`, `staticcheck` and `govulncheck` are green on it as generated.
- The new CI job `futu-proto` joined the release CI contract, which pins every `ci.yml` job and run step, so release authority waits for it. Proof pending: its first run, at the first push; nothing here pushes.

## How WP6 is wired

- `connection` (unexported) owns one socket: the serial numbers, frames, matching a reply by serial and protocol, everything else to the push handler. A malformed frame, an undecodable body or a session-level `retType` closes it; `*Error` is what the caller gets. Its pacer belongs to the `Connector`, because OpenD's windows outlive a socket.
- `Connector` owns the session loop: dial, `handshake` (InitConnect and the version gate), then `maintain`: a keep-alive every interval, dropped after two intervals without a received frame on real elapsed time, whatever `Config.Now` says. The state and scope read beside that loop (`startRefresh`), so a silent OpenD cannot hold the heartbeat. `Phase` is how far it came. A new session is a new `Epoch`, and a refresh that outlives its session writes nothing.
- The scope comes only from the account list, is never re-scoped, and `tradeHeaders` is the only way to a `TrdHeader`: the pin and the environment read for it, one header per authorised market.
- `futusim.Listen` is the server, `futusim.Start` the same for a test (a 2005 frame fails the test). Switches: `SetSilent`, `SetProgramStatus`, `SetLoggedIn`, `SetAccountListed`, `KickOut`, `PushGatewayEvent`, `PushQuotaState`, `PushQuoteRights`; options `RequireRSA`, `ServerVersion`, `KeepAliveSeconds`, `ProgramStatus`. A refusal of any protocol is one `notify.Response`, because every generated Response carries `retType`, `retMsg` and `errCode` under the same field numbers.
- Fixtures: `pkg/futu/testdata/frames/*.hex`, `>` client and `<` simulator. `TestFixtures...` in `futu_test` holds the simulator to them byte for byte and re-records with `-record`; `TestReplay...` in `futu` plays them at the client over a pipe and holds its bytes to them.
- Finding for WP7: `Common.PacketID` appears in no vendored definition, only in `Common.proto`; the order-writing definitions that use it arrive in phase D. The brief's "PacketID on every trade request" has nothing to exercise until then.

## How WP7 is wired

- Every read has a lower half on `*connection` (`readFunds`, `readSnapshots` and so on, which the frame fixtures replay against) and a `Connector` method that adds the session or the pin. No protobuf type leaves `pkg/futu`.
- Pacing follows each interface page: funds, positions, orders and fills count the 30-second window only when they refresh OpenD's cache (`CachedReadsFree`); history candles count only the first page (`FirstPageOnly`); the 10-per-30-second rows are per account (`PerAccount`), which the simulator keeps per account and the client, pinned to one, does not need to. A plain read goes through `requestUnpaced`.
- `TrdHeader` per authorised market; positions, orders and fills are one request per market, merged by id. A code on a trade row is rebuilt from its bare code and `secMarket`.
- A subscribe refusal is `ErrQuotaExhausted` only when `Qot_GetSubInfo` after it reports nothing remaining; the free text is never read. A release inside the hold is refused locally (`SubscriptionHeldError`) and the caller defers it (WP11). Lines belong to a session, and a new session starts empty.
- Finding for WP8: `Qot_UpdateBasicQot` and `Qot_GetBasicQot` carry no bid or ask, only a snapshot does, so a streaming line has last, open, high, low and volume with bid and ask zero.
- Funds are read in the currency of the first authorised market (`marketCurrency`): a universal account requires one. Real behaviour with several markets is proof pending at the first real session.

## How WP8 is wired

- `s.opend` is non-nil exactly under kind `opend`, set in `New` and never replaced. `gatewayConnector` returns nil and `triggerReconnect` false under it, which is what keeps all 63 connector callers answering their typed unavailable reason. `newConnector` returning an error is the backstop.
- `brokerReads()` returns `broker.Reads`. Under `opend` it is the adapter; under IBKR the per-connector adapter. A site that needs the IBKR connector itself takes `gatewayConnector` and `adapterFor`, as `setup_options.go` now does.
- Scope: `brokerStateScope.Kind`, zero meaning IBKR. `keyAccount` for keys and hashes, `brokerField` for record bodies, `recordScope` to rebuild from a body, `scopeFromKeyAccount` to rebuild from a key. `currentBrokerStateScope` reads `scopeSource().Scope()`. `scopeFromSnapshot` replaces the snapshot form outside the IBKR-only write and evidence paths.
- `broker_scope_fixture_test.go` pinned 18 families' IBKR bytes before any scope site moved. If it goes red, an IBKR key changed.
- OpenD reads fork at the top of three handlers: `opendAccountSummary`, `opendPositions`, `opendQuote` in `broker_opend.go`. They are smaller than the IBKR paths on purpose: no daily P&L stream, no options, no Greeks, no lending. WP9 names what is missing.
- `runOpenDSession` holds the connector, polls state and quota every 30 seconds, and publishes the last attempt's verdict in words into `lastConnectError`. The process list is read only when the phase changes.
- The IBKR hosts are off by kind: `disableIBKRHosts` clears Flex and the cash ledger on the daemon's copy, `noIBKRHosts` stops the borrow-fee FTP dial.
- Mapping decisions: a bare symbol is its own code if prefixed, Hong Kong if all digits, else US; a Singapore name needs its code, venue or currency. The US venue is the literal `US`, since OpenD names no primary exchange in these reads. `observeQuoteHealth` has no OpenD source: OpenD has no absence table, so WP9 should not look for one.
- A streaming line has no bid or ask, so `opendQuote` prefers a line for `last` and gets bid and ask only from a snapshot.
- Release note needed at release time, not now: an operator whose daemon sets `XDG_CONFIG_HOME` elsewhere finds the policy files move.

## How WP9 is wired

- `rpc.CodeNotApplicable` is the typed answer; `ibkrOnlyMethods` in `broker_opend_surfaces.go` gates them at the top of `dispatch`. MCP turns the code into a result in `handleToolsCall`.
- `opendDataHealth` rewrites the rows after collection; `dataHealthIBKROnly` names the sources by id prefix. A new IBKR-only source needs its prefix there.
- The status rows are `renderOpenDRows` in `internal/cli/status_opend.go`.
- Held for the owner: the handbook edit, drafted in the shared notes folder as `architecture-handbook-draft-wp9.md`.
- Owed by WP12: inspect the app on the simulator stack at real size and name the screenshot.

## How WP11 is wired

- `refreshOpenDLines` in `broker_opend_quotes.go` moves lines toward the wants: `orderedLineWants` is the book then `opendRegimeCodes`, and OpenD's quota sets the reach. It runs on every unfiltered positions read.
- The adapter's `SetLineBudget` limits ad hoc streams to that set; outside it, and past the quota, `broker.ErrSnapshotOnly`. `broker.ReleaseHeldError` is the seam's form of the hold, and `subManager.deferRelease` retries it.
- A line has no quote until a push, so marks fall through to the batch until then.
- Tests that move a hold give the connector the book's clock in `startOpenDServer`; the adapter stays on the wall clock for receipts.

## How WP12 is wired

- `cmd/opend-sim` serves the book as a process; `make opend-sim` runs it with `-clock now`. Runbook in the design's simulator section.
- `TestLifecycle_TwoBrokersSideBySide` in `test/integration/dual_broker_test.go` runs both stacks and proves a down OpenD socket autospawns an OpenD daemon and leaves the IBKR process alone.
- Under `opend`, `subsystemHealth` lists only what the broker has (`opendSubsystems`), `GatewaySetup`, `Executions`, `AnswerPath` and `BrokerPermissions` are absent, the data check is one snapshot of a reference name, and the status poll refreshes account and positions so feed health has a read to report.
- `scripts/opend-witness.sh <outdir>` is the witness, after `make build`. It fails closed on its paths and fails if the operator's socket, stop marker or policy directory changed. Captures are in the shared notes `opend-witness` folder with the app screenshot `app-opend-1280x900.png`.
- The live daemon is stopped on purpose (owner, 2026-11-04 20:38:45 CET). Never run `canary restart`, `make restart-daemon` or anything that clears `~/.cache/ibkr/ibkr.sock.stopped`, and start no daemon on the default socket or roots.
- Under `opend` write authorisation refuses every origin with `broker_writes_not_served`; phase D removes that on purpose. `[opend] port` and `opend-sim -listen` refuse IBKR's API ports.

## How holdings that are not stocks are wired

- A position row names no type, so `Adapter.secTypeOf` reads each held code from `Qot_GetStaticInfo`, one request per code, kept once read. One refused code cannot poison the rest. A type OpenD did not give stays `UNKNOWN` and is read again next time.
- `opendPositions` takes only `STK` into the stock rows, the marks and the line budget. The rest go in `rpc.PositionsResult.NotServed` at OpenD's own price and value, with no analysis, and `PortfolioComplete` is false, which the stock-add and portfolio-plan paths already refuse on.
- Surfaces: `renderPositionsNotServed` in both CLI position views, a brief coverage line, a warning row in the app's portfolio detail, the `not_served` field on `canary_positions`, `not_served_count` on the risk view.
- If static information fails while OpenD's quote server is logged out, every uncached holding moves to this list until it returns: nothing is lost and the book reads as incomplete.
- Captures in the shared notes `opend-witness-not-served` folder, with `app-held-not-analysed-1280x900.png`.

## In progress

Nothing between packages.

## Next

The hold of 2026-11-05 08:43 CET is lifted for read-only bring-up. No landing, no merge to main and no push without the owner's word, and the agreement read comes first.

Merge local main again and re-run the gate and spine if main has moved.

First steps on resuming, with real OpenD: run the first-session checks under "Known gaps", then decide the regime and FX package. Desk's re-pin comes after landing.

Public text: `CHANGELOG.md` carries the stub `## v3.35.0 — Unreleased` at the owner's instruction, which release prep renames; `README.md` has an "Experimental: Futu OpenD" section and a line in its legal paragraph. A fresh reader cold-read both on 2026-11-05 and its ten findings were applied after checking them against the source. The changelog and README say what the gaps below make true, so each fix that closes a gap also edits them.

## Known gaps

Fixed, recorded here because each was reported open: an OpenD daemon saying "IBKR connection unavailable" (`85db5500`, `TestALostOpenDSessionNamesOpenD`), and options and futures reading as stocks (`b3f0ea60`, `TestOpenDHoldingThatIsNotAStockIsListedApart`).

Open:

1. Regime and portfolio stress are off under OpenD, against the design's first text: the regime read stops at its IBKR connector check. A package of its own, proposed in the owner table.
2. OpenD supplies no FX rate, so holdings outside the base currency read as not measured. With the regime package.
3. Partly met a real OpenD; see "First real session". Still proof pending: the RSA refusal shape, funds currency on a multi-market account, and whether a position's value carries the multiplier.
4. Proof pending, next live session: the IBKR path on a real Gateway, including an IBKR identity resolved through it, proven here against a fake.
5. IBKR wording: a sweep of every daemon answer and the CLI read commands under OpenD, on the simulator, found and fixed the reconnect action, the order-journal disclaimer, the write refusal's action and the borrow sources. What stays names IBKR on purpose: "IBKR only" reasons, the `TWS order control` setting and its help text, and the IBKR-named source label in the breadth snapshot. MCP tool descriptions are static and name IBKR for every broker.
6. Labels waiting on the owner: "22/22 sources ok" above a line of unavailable sources, the "Not on OpenD" wording, and the handbook edit draft.
7. The rulebook reads "10 data gaps" on the simulator's book.
8. Log noise: fixed; see "First real session".
9. `market: hk` works on `quote.snapshot` only; `market.history` and the other paths on the shared IBKR normaliser take the venue or the identity.
10. An IBKR row with only a smart route carries no identity and keeps `con_id`; all IBKR watchlist rows are in that state today.
11. No LaunchAgent or install step for a permanent second daemon; the setup has run only in the witness and the dual-daemon test.
12. The spine and the simulator witness last ran at `b3f0ea60`, the money check on its tree; they are landing steps again after the next merge. The paired app's Monitor tab was inspected on the simulator stack at `b3f0ea60`'s tree for the new row only; an earlier capture at `28119a91` shows stress as "Fault · transport unavailable", and the public text does not claim the app.
15. A holding not analysed appears in the app's portfolio detail, not on its Positions tab, and the rulebook's checks do not name it. `canary positions`, the brief and `canary_positions` do.
13. Not done before phase D: `broker` in Desk's and the companion's signed-term allowlists.
14. Whether the OpenAPI agreement permits Canary is open and the owner's, read inside OpenD. It comes before any landing, since landing publishes the README section at the next release.

## Not done

No live proof (no Gateway in this session): every
IBKR-path package reports "proof pending: next live session". Phase D is
out of scope.
