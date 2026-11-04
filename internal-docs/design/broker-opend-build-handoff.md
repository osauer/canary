# OpenD build: handoff note

Updated: 2026-11-04 22:04 CET. Branch `cc-wt-/opend-build-27fd07`, cut
from the design branch at `b201852e` and merged with local `main`
(`e514c61b`). Follows
[broker-opend-build-brief.md](broker-opend-build-brief.md) against
[broker-opend.md](broker-opend.md). This note is kept current after every
work package so a fresh session resumes without the conversation.

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
| WP11 quota budgeting and snapshot delivery (WP10 waits for the identity ack) | the commit carrying this note | `make test` on the tree before the commit; spine on the landed revision | `gate test at canary--v3.33.1-20-g18512599-dirty exit 0` |

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

## In progress

Nothing between packages.

## Next

WP12: `cmd/opend-sim`, the witness stack, the dual-daemon witness with the autospawn proof, the app inspected on the simulator stack. WP10 waits for the owner's ack of the identity, asked of the parent session 2026-11-04 21:53 CET.

## Not done

WP10 and WP12. No live proof (no Gateway in this session): every
IBKR-path package reports "proof pending: next live session". Phase D is
out of scope.
