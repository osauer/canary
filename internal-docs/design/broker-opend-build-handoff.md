# OpenD build: handoff note

Updated: 2026-11-04 17:45 CET. Branch `cc-wt-/opend-build-27fd07`, cut
from the design branch at `b201852e` and merged with local `main`
(`e514c61b`). Follows
[broker-opend-build-brief.md](broker-opend-build-brief.md) against
[broker-opend.md](broker-opend.md). This note is kept current after every
work package so a fresh session resumes without the conversation.

## Rules in force

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
| WP4 `pkg/futu` frames, limits, errors, the simulator book and the test legs | the commit carrying this note | `make test` on the tree before the commit; `make test-pkg` (floor line `test-pkg: pkg/futu statement coverage 98.7% (floor 80%)`); `make ci-gate-scope-check`; account-data gate on the staged fixture (OK) | `gate test at canary--v3.33.1-13-g685871bb-dirty exit 0` |

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

## In progress

Nothing between packages: WP5 waits for the owner's ack.

## Next

WP5 per the brief, after the owner's ack of `google.golang.org/protobuf` as the eighth direct dependency (the phase A message to the parent session named it). Until the ack, WP5 cannot start; WP6 and WP7 depend on it.

WP5 inputs, read 2026-11-04: upstream is `FutunnOpen/py-futu-api` (Apache-2.0, default branch `master`), directory `futu/common/pb`, last commit touching it `9bac9d8b9b8c1efe05df16a9e98937f08b68489b` (2026-03-31, "update 10.2.6208"); it holds 77 `.proto` files, every one of the 28 the brief vendors is present, and so is `Trd_UnlockTrade.proto`, which is never vendored. This Mac has `protoc` at `/opt/homebrew/bin/protoc`, `libprotoc 32.1` (the brief's pair is `libprotoc 32.1` and `protoc v5.32.1`); `protoc-gen-go` is not installed and is pinned in `tools/go.mod` by WP5 itself. The dependency gate prints seven direct product modules today (`scripts/check-go-dependencies.sh`, line 156); WP5 makes it eight.

## Not done

Everything from WP5 on. No live proof (no Gateway in this session): every
IBKR-path package reports "proof pending: next live session". Phase D is
out of scope.
