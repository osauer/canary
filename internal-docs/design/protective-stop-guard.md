# Protective stop exemption and guard

Updated: 2026-10-05 17:56 CEST
Status: implemented

Contract per `.agents/docs/daemon-cli-trading-contract.md` and
`.agents/docs/risk-policy-contract.md`.

## Decision

- **Goal:** a broker-side GTC trailing stop sized to a whole long stock
  position (bucket `trailing_stop`) can be placed. Before this change two gates
  in `validateOrderRiskAuthority` refused it: `[trading].max_notional` binds
  apparent exits, and every risk-reducing stock sell is re-read as
  `open_short` because `reqAllOpenOrders` cannot prove future manual-TWS
  activity, so it needs `allow_stock_short`. The proposal row still read
  `ready`; the owner learned at prepare/preview (`order_risk_limit`).
- **Policy owner and approval:** the owner, 2026-10-05 17:36 CEST, "exempt
  with the guard".
- **Amendment (2026-10-05 20:28 CEST):** the notional cap and
  `allow_stock_short` moved to the risk constitution's `[order_limits]`
  (owner decision 2026-10-05 19:56 CEST); the cap now scales with the book.
  The exemption is unchanged and applies against the cap in force.
- **Enforcement class:** pre-trade hard gate (narrowed), plus an automatic
  post-trade correction of Canary's own stops.

## Meaning

- **Exemption (`protective_exit.go`).** `protectiveStockExitExempt` holds for
  a single-contract `STK`/`ETF` `SELL` whose order type is `TRAIL`,
  `TRAIL LIMIT`, `STP` or `STP LMT`, with `position.Before > 0`,
  `quantity <= Before` and effect `close` or `reduce`, when the complete,
  current all-client open-order inventory of the order's own account and mode
  shows other working `SELL` orders for the exact contract (hand orders
  included; an order without a ConID that names the same symbol counts) whose
  remaining quantity plus this one is at most `Before`. A modify whose target
  is a working sell stop and whose new quantity is below the target's
  remaining quantity is exempt without the competing-sell condition: it can
  only shrink what is already working. Unavailable, stale, incomplete or
  foreign-scope inventory never exempts. The exempt order skips the notional
  cap and the short re-read; FX evidence checks, `max_option_contracts`,
  option rules, bond rules and strategy rules are unchanged.
- **Where it is read.** Preview reads the inventory once per candidate;
  admission (`bindPreviewOrderRiskAuthority`) reads it again, so a hand sale
  entered after the preview withdraws the exemption; the first-byte guard
  reuses the admission evidence with the re-read position and issues no broker
  request. Proposal provenance is not required: the CLI preview of the same
  proposal has none.
- **Readiness.** A stock/ETF trailing-stop row the gates would refuse because
  the exemption cannot apply carries one typed blocker:
  `protective_exit_competing_sell` (not executable),
  `protective_exit_inventory_unavailable` (waiting for the broker) or
  `protective_exit_exceeds_position`. With `allow_stock_short = true` the row
  carries none; the notional cap then decides at preview from exact-session
  FX evidence the row does not have.
- **Guard (`protective_stop_guard.go`).** After each proposal refresh and the
  pre-authorised and queued cycles (the proposal cadence, 30 s by default) the
  daemon plans, for each working broker order that is a stock `SELL` stop and
  that the journal tracks as Canary-placed (reserved order ID, order
  reference, preview token, same account and mode): position `<= 0` (or under
  one whole share) → cancel; `0 < position < remaining` → shrink to
  `floor(position)` through a replacement preview and the ordinary modify
  path, keeping trail amount or percent, current stop price, limit offset,
  trigger method, TIF and outside-RTH. A stop Canary did not place is
  reported (daemon log and decision log, once per reason), never touched.
- **Evidence and debounce.** A pass needs a concrete broker scope, a current
  portfolio projection (`classifyPortfolioStreamHealth == current`: completed
  receipt for the account, no short download, fresh) with every row in scope,
  the complete open-order inventory for that scope, and the journal. Without
  any of them the pass is paused and the settle clock is cleared. A step acts
  only after it was planned on consecutive passes at least 20 s apart, and the
  write path re-plans under `brokerWriteMu` and acts only if the same step is
  still planned.
- **Authority.** Writes use the origin `daemon-protective-guard`
  (`rpc.OrderOriginDaemonProtectiveGuard`). The write gate accepts it only
  while the guard holds a grant naming the exact order and change (cancel, or
  shrink to exactly the planned quantity below the remaining quantity).
  Trading mode, gateway, pins, storage and journal health, preview token,
  broker WhatIf and the protective modify rule (`validateProtectiveModifyQuantity`)
  all still decide.
- **Freeze.** `trading.freeze` blocks every new write including modifies and
  allows cancels (`docs/docs/reference/config.md`). The guard follows that: a
  shrink waits while frozen, a cancel goes through.
- **Audit and notice.** Each change is journaled on the order with the guard
  origin and written to `events.jsonl` as `protective_stop_guard.shrink`,
  `.cancel` or `.report` with outcome `sent`, `failed`, `blocked` or
  `reported`. The owner's notice is the existing Protection
  reconciliation-required episode for the mismatched order, which recovers
  when the change lands; the guard adds no new alert kind.

## Residual risk accepted

- While the daemon is down, a hand sale is not followed: the stop can stay
  larger than the position until Canary is back and the guard has run two
  passes.
- Between a hand sale and the guard's second pass (about 30–60 s) a triggered
  stop can still sell shares no longer held.
- A shrink needs a submit-eligible replacement preview (a quote and an
  accepted broker WhatIf); while it cannot get one
  the guard logs the failure and retries every pass. A freeze holds a shrink
  for as long as it is set.

## Verification

- `internal/daemon/protective_exit_test.go`: exemption boundary table (stop vs
  limit, long vs flat/short, quantity above position, competing sell, stale
  inventory, ETF, option unaffected, shrink of a working stop), inventory
  counting, and trailing-stop row readiness.
- `internal/daemon/protective_stop_guard_trading_test.go` (trading build):
  shrink, cancel at zero (also while frozen), shrink held by the freeze, kill
  switch, stale projection and settle restart, hand order untouched, origin
  grant.
