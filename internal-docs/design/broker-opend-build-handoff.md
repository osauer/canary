# OpenD build: handoff note

Updated: 2026-11-04 16:40 CET. Branch `cc-wt-/opend-build-27fd07`, cut
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
| WP1 `internal/broker` types, the fifth read recorded, design and brief corrected | the commit carrying this note | `make test` at `e514c61b` plus the WP1 tree | see the commit body |

The fifth read (five cold subagent reads of the fourth review's responses)
is in the review record under "Fifth read". Four of five mechanisms
changed: the per-kind identity lives on the `Server` with a scope function
over the session observation; the key form cuts the account at its first
`:` and the kind goes inside the account field, hashed shapes take the kind
as an extra hash input, four typed terms and `rpc.TradingStatus` gain
`Broker`; protobuf is one Go package per vendored file; the IBKR dial gate
is in `triggerReconnect` with `newConnector` returning an error. The brief
and design carry these; WP5 and WP8 build to them.

## In progress

WP2 the IBKR adapter (session, quotes, bars). The adapter package
`internal/broker/ibkr` is written and its own tests pass (mapping totality
over every daemon security type, round trips, nil behaviour); the daemon
seams are not yet moved onto it.

## Next

WP2's daemon half: `subs.go`, `market_data.go`, `setups.go`,
`setup_resolve.go`, `mandate_history.go` onto `broker.Quotes`,
`broker.Bars`, `broker.Instruments`; the adapter built beside the
connector at the `s.connector = real` site; binding-across-reconnect and
rebuild tests in the daemon package; `market_data_delayed_test.go` moves
to the adapter. Then WP3 (account, positions, observation).

## Not done

Everything from WP2 on. No live proof (no Gateway in this session): every
IBKR-path package reports "proof pending: next live session". Phase D is
out of scope.
