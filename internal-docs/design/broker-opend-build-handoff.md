# OpenD build: handoff note

Updated: 2026-11-04 17:00 CET. Branch `cc-wt-/opend-build-27fd07`, cut
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
| WP2 IBKR adapter (session, quotes, bars) and the daemon seams on it | the commit carrying this note | `make test`; side check `git diff --exit-code HEAD -- internal/cli/testdata/golden pkg/ibkr` empty | see the commit body |

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
- The history read keeps one concrete capture for the data-health
  observation (`observeHistorySource` is still connector-typed); WP3 moves
  the recorder onto the seam. `handleMarketSnapshot`'s front-future
  enrichment and `setup_options.go`'s option listing stay on the
  connector by design (IBKR-only paths); `setup_options` resolves the
  underlying through the seam with `BindingFor` so the resolution memo is
  shared with the evaluation path.

## In progress

WP3 (account, positions, the observation): not started; the files are read.

## Next

WP3: `internal/broker/ibkr/account.go`, `positions.go`;
`readAccountSnapshot` takes `broker.Account`; positions rows through
`broker.Positions` with the stream health converted at the handler
boundary; `observeAccountRPC` and the data-health recorder
(`recordDataHealth`, `recordFeedHealth`, `dataHealthObservation`) onto
`broker.Session` and the neutral binding with a daemon bridge for the
concrete callers; `observeHistorySource` then loses its concrete capture.

## Not done

Everything from WP2 on. No live proof (no Gateway in this session): every
IBKR-path package reports "proof pending: next live session". Phase D is
out of scope.
