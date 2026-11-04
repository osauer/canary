# OpenD build: handoff note

Updated: 2026-11-04 17:27 CET. Branch `cc-wt-/opend-build-27fd07`, cut
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
| WP3 IBKR adapter (account, positions) and the data-health recorder on the seam | the commit carrying this note | `make test` on the tree before the commit; spine on the landed revision (stamp recorded in the next package's tree); side check empty | `gate test at canary--v3.33.1-12-g7ea49dd3-dirty exit 0` |

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

## In progress

Nothing between packages: the WP3 commit carries this note; its spine run on the landed revision is recorded in the next package's tree, as WP2's was.

## Next

WP4 per the brief: `pkg/futu` frames, limits, book model and the test legs
(`test-pkg` with the 80 percent coverage step, `pages-coverage`); new
files only under `pkg/futu` plus the `Makefile`. Then the phase A message
to the parent session (commits, gates, HEADs, stamps).

## Not done

Everything from WP2 on. No live proof (no Gateway in this session): every
IBKR-path package reports "proof pending: next live session". Phase D is
out of scope.
