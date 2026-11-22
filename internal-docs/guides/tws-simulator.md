# The TWS Simulator

Updated: 2026-11-23 00:11 CET

## What it is

`pkg/ibkr/twssim` is a simulated TWS: a broker and a market on a loopback TCP
port that speaks the TWS API at server version 203. The real daemon, its real
IBKR connector and its wire codec run against it: connect, account and
portfolio streams, contract details, quotes, history, option chains, WhatIf,
orders, combinations (BAG), fills and executions. `cmd/canary-sim` runs the
daemon against it as a process, so the real `canary` CLI and Desk can drive
it.

## What it proves, and what it cannot

It proves sequencing, gates, evidence handling, recovery and what reaches
the wire. A scenario reads the broker's view (the simulator's book and
every frame the client sent) beside the daemon's record.

It does not prove that real TWS accepts a frame or answers as modelled:

- The client's placeOrder and cancelOrder are decoded by IB Gateway
  10.50's own protocol-buffer field table (`schema.go`), so a field the
  broker would drop shows as `IgnoredByBroker`. This was audited against
  the official proto files and the Gateway bytecode.
- Every frame the simulator sends is shaped to the layouts Canary's decoder
  reads. On those, the simulator agrees with Canary by construction.
- The behaviours modelled from assumption are listed in the table in
  `pkg/ibkr/twssim/doc.go`. Examples: fills at the touch, the WhatIf margin
  model, rests while closed, the combination execution shape, untransmitted
  parents held silently, and every scripted fault.

What has no TWS source is seeded and named in `internal/daemon/simulation.go`:

- the regime's public series;
- a calm regime latch, because breadth needs about 74 minutes of pacing and
  a weekend never latches;
- one reconcile event, because there are no Flex statements.

## Isolation, and why live mode

A simulated daemon (`daemon.Options.Simulator`) refuses to start unless:

- the gateway port is pinned, on loopback and off 4001/4002/7496/7497;
- the account is the placeholder `U1234567`;
- the mode is `live`;
- Flex is off and any cash-ledger service is on loopback.

It asks no public site. The stack runs live mode because Monday's orders run
the live path, and paper mode cannot add at all: the capital store observes
no paper equity.

`canary-sim` writes only under `-dir` and refuses any root outside it, and
it refuses the operator's daemon socket. The installed `canary` binary never
sets `Simulator`, and only `cmd/canary-sim` installs a simulated exchange
session; tests hold both rules.

## Running it

```
go build -tags trading -o canary-sim ./cmd/canary-sim   # without the tag it previews but cannot place
canary-sim -dir /tmp/csim1 [-book FILE]
```

It prints the simulator's address and an `export` block (`CANARY_SOCKET`,
`CANARY_CONFIG`, `CANARY_LOG`, `HOME`, `XDG_*`). With those set, the real
`canary` CLI talks to the simulated daemon only. A CLI autospawn from that
config reaches only the simulator's port.

Control it while it runs with `canary-sim ctl -dir D COMMAND`.
`canary-sim -h` lists the commands:

- `quote`
- `session MARKET open|closed|opens-in S|closes-in S` sets the simulated
  exchange and the daemon's calendar together.
- `fault KIND` scripts the next order or WhatIf.
- `fill`, `endday`
- `cash`, `tick`
- `position SYM|CONID QTY [AVG]`
- `options SYM [STRIKES]`
- `frames [SINCE]`, `orders`
- `drop`, `account CODE`

Exchange sessions start as the official calendar has them now.

### A book file

`-book FILE` starts from a JSON book instead of the default synthetic one.
The loader is strict and refuses unknown fields. It refuses any account but
`U1234567`, so a book can never carry a real account id.

- Top level: `account`, `base_currency`, `cash` (currency to amount), and
  an optional `fx` (currency to base rate).
- `positions[]`: `con_id`, `symbol`, `sec_type` (STK|OPT), `currency`,
  `exchange`, `primary_exchange`, `trading_class`, `quantity`, `avg_cost`,
  `last`, `bid`, `ask`.
  - Options add `underlying_con_id`, `right`, `strike`, `expiry` and
    `multiplier`. The expiry is a date (YYYYMMDD), or `+Nw`: the Friday N
    weeks after the first Friday after today (UTC). The committed example
    uses `+Nw` so it never expires.
  - Quantity 0 lists a contract without holding it.

The example `pkg/ibkr/twssim/testdata/owner-mix-book.json` has the owner's
asset mix in SYN* tickers with invented figures:

- 100 SYNA shares;
- six long calls;
- two puts;
- a SYNB bear put spread that the daemon groups as a strategy.

A book built from a live read belongs in an untracked temp file, never in
Git.

## Writing a scenario

Scenarios are trading-tagged tests in `internal/daemon/twssim_*_trading_test.go`.
`startSimDesk(t, twssim.Options{...}, true)` starts the real `Server.Start`
path in simulator mode against a fresh simulator. It uses its own XDG roots
and the synthetic owner's reviewed policies, and waits for a ready session
and a regime.

- Drive the daemon through its RPCs (`d.plan`, `d.preview`, `d.place`,
  `d.status`).
- Script the broker with `d.sim.ArmFault`, `d.sim.SetSession`,
  `d.sim.Fill` and `d.sim.EndDay`.
- Assert the broker's book and `d.realPlaces()` (the transmitted frames).

A scenario blocked by a known product defect skips and names the branch
that fixes it. Nothing in a scenario may depend on the weekday, the date
or the real exchange calendar:

- option expiries come from `twssim.OptionExpiries`, which keeps each
  expiry in its horizon on every day;
- sessions are set with `marketcal.SimulateSession` and `SetSession`
  (`ctl session` in canary-sim);
- a DAY order ends only when the scenario calls `EndDay`.

## Test tiers

- The normal gate (`make check`, daemon trading leg) runs only the smoke
  scenarios listed in `simSmoke`.
- `make test-sim` runs every scenario (`CANARY_TEST_SIM=1`). It takes
  minutes: run it before a change to the order path lands.
- `pkg/ibkr/twssim` has its own engine tests on a bare TCP client.
