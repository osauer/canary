# Concepts

Updated: 2026-07-25 09:41 CEST

What the load-bearing context surfaces measure, in enough depth to read the output without mis-acting on it. This page is the mental model. [Sensors](sensors.md) owns authority, freshness, last-good behavior, and the safe checks; the [regime dashboard contract](../internals/regime-dashboard.md) owns methodology.

## Market calendars

Calendars answer one risk-relevant question: is this market supposed to be trading right now, and if not, when does the official session resume?

The first release is official-source only and deliberately narrow:

- **US equities** (`us` / `us-equity`): regular NYSE/Nasdaq-style cash-equity sessions, holidays, and early closes.
- **US listed options** (`us-options`): regular listed-options sessions, separate because options have their own close window and holiday schedule surface. Per-class global hours, SPX/VIX extended sessions, curb trading, and exercise/settlement nuance are not modeled in v1.
- **German Xetra equities** (`de` / `de-xetra`): Deutsche Boerse Xetra cash-equity sessions and non-trading days. Frankfurt floor trading and Eurex derivatives are not modeled in v1.

Treat futures, FX, crypto, bonds, Eurex, and exchange-specific derivatives as out of scope unless a result explicitly names a supported market.

The schedules are embedded, not IBKR overlays, so cold starts are instant and nothing depends on a remote calendar file at runtime. The official exchange calendar is binding here. IBKR quote state still matters for entitlement, routing, and farm-health issues, but it never redefines whether the exchange is open.

The cost is bounded coverage: the response carries `coverage_start` / `coverage_end`, `days` caps at 400 calendar days, and dates outside embedded coverage return `state: "unknown"` rather than guessing from weekdays.

`ibkr quote` adds a `session_context` block only when it helps explain stale, frozen, or missing data. In an ordinary live regular session with prices present, quote output stays quiet.

## Regime

The eight-row risk-regime dashboard summarizes the market's current posture. Each row measures a different stress channel, which is what separates ordinary chop from a regime shift in progress. It also emits a broad-market lifecycle stage (`quiet`, `early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`, or `data_quality`), source health, and semantic fingerprints for monitors.

1. **VIX term structure** (VIX vs VIX3M). Backwardation, short-dated vol pricing above 3-month vol, is the stress fingerprint. The deeper and more sustained the inversion, the bigger the dislocation.
2. **VVIX vol-of-vol**. Cboe's VIX-of-VIX reading catches convexity demand inside the equity-vol cluster.
3. **HYG vs SPY divergence**. High-yield credit leads equity selloffs on the way down. A HYG breakdown while SPY is still near highs is the classic late-cycle warning.
4. **HY/IG OAS**. Official ICE BofA cash-credit spreads via FRED: slower than HYG, harder to dismiss as ETF noise.
5. **Funding spread**. 90-day AA financial commercial paper minus 3-month T-bill, flagging slow funding and liquidity pressure.
6. **USD/JPY weekly move**. JPY funding-pair unwinds are a recurring stress amplifier (Aug 2024, Dec 2018, Jan 2016). The row turns amber when the yen strengthens 1-2% in a week and red above 2%.
7. **Dealer zero-gamma** (SPX canonical, SPY corroboration). Whether the dealer book stabilizes or amplifies day-over-day moves. See [Gamma](#gamma).
8. **S&P 500 breadth**. Whether the index's strength is broad or carried by a handful of mega-caps. See [Breadth](#breadth).

Every row bands green / yellow / red and carries a `streak` count of consecutive sessions in that band. A Day-1 stress event reads differently from a Day-5 one. The lifecycle layer keeps weak or unconfirmed red evidence visible while stopping a single noisy proxy from dominating the broad-market trigger.

Two things to expect on the wire. Gamma and breadth are heavy computes: gamma reports `status: "computing"` with an ETA, breadth `state: "computing"`, until a result is serveable. [Sensors](sensors.md) has the refresh and last-good rules. Live IBKR rows may carry a `fields_missing` array for optional sub-fields that missed the fetch budget; the primary measurement still landed, so treat it as a render hint, not an error.

Calibrate your own threshold bands against [the regime dashboard contract](../internals/regime-dashboard.md); its suggestions are starting points, not gospel.

## Canary

The portfolio canary is narrower than regime: it asks whether today's market weather matters for the portfolio currently held. From account, positions, and regime snapshots it derives an action, planner readiness, and a semantic alert fingerprint for monitor dedupe. [Sensors](sensors.md#canary) lists the output fields.

The high-precision rule is intentional: broad-market stress must be confirmed by market evidence, not by the user's own losses or margin pressure. Account-only facts and portfolio-only facts can appear as evidence, but `defend` requires confirmed market pressure, vulnerable portfolio fit, and usable input health. Portfolio-only pressure normally becomes `rebalance` or `watch`.

`portfolio.held_stress[]` is the positions-only single-name stress surface. It is bounded to material held underlyings and appears only when an existing position shows one of these conditions:

- held-name daily P&L shock as a percent of NLV
- near-expiry held-option delta concentration
- held-name stock quote or option bid/ask degradation

Canary calls no option chains, scanners, short-interest feeds, paid borrow vendors, or external flow sources. It does consume the daemon's market-event context for held-name tags and alert fingerprints, and those flags remain context and safety gates rather than standalone execution advice.

Canary marks the alert boundary. The diagnosis behind an alert comes from `ibkr_positions`, `ibkr_regime`, `ibkr_market_events`, or `ibkr_account`.

## Market events

Market events answer a single-name context question: does this held or requested stock or ETF have borrow, threshold-list, LULD, or halt evidence that should affect risk review or protection proposals?

V1 flags are reduce-only context and gates. They can annotate, prioritize, or block an existing protection proposal, but they never create buy-to-open, buy-add, or squeeze-style opportunity recommendations. The separate Opportunities surface is daemon-calculated from positions and executable market data; its MVP bucket is option exercise only. When a `BUY` proposal reduces an existing short, the user-facing copy is `Buy to cover`.

The five V1 flags:

- `borrow_inventory_tight`: IBKR shortable-share inventory crossed the V1 tight/scarce thresholds. Strengthens buy-to-cover context for existing shorts; observational for long holdings.
- `borrow_fee_extreme`: the current global IBKR short-stock availability file reports an annualized fee rate of at least 50%. Emitted only from current, policy-eligible FTP evidence, never inferred from low inventory, and never emitted or cleared from stale data.
- `reg_sho_threshold`: the symbol appears on the Nasdaq Reg SHO threshold list. Non-Nasdaq listing-exchange threshold feeds are outside coverage, so absence is not universal non-threshold proof.
- `luld_pause`: a Nasdaq trade-halt reason indicates an active or recent LULD pause. Active LULD blocks proposal preview/submit; recent LULD is a warning requiring fresh quote context.
- `halt_regulatory_or_news`: a regulatory/news halt is active or recent. Active halts are hard blockers; recent halts are warning tags.

When a due FTP refresh is unusable, the daemon can inspect exact currently held short-stock contracts through TWS historical `FEE_RATE`. Those rows are portfolio-only, nullable, scale-unverified, and policy-ineligible until a controlled broker fixture commissions the numeric scale. `borrow_fee_coverage[]` separates global FTP coverage from that held-short-only TWS context and names entitlement, scale, and policy eligibility directly.

Unknown and null mean unavailable, not false or zero. Each feed's health reads `ok`, `partial`, `stale`, `unknown`, or `degraded`, and [Sensors](sensors.md#market-events) defines what each state permits. Stale and unknown health stays visible because it changes how much confidence the absence of a flag deserves.

Rule 201 / short-sale restriction is not a V1 protection driver. If added later, it should be context-only unless the order path is directly short-sale relevant.

`ibkr market-events --symbol GME --json` evaluates explicit symbols. Omitting symbols evaluates held stock/ETF underlyings, which needs a usable positions snapshot from the daemon/gateway.

## Protective stops

A protective stop is only protective while it matches the position. Sell part of the position somewhere else, in TWS for instance, and the stop keeps its old size. If it then triggers, it closes what is left and opens the remainder in the opposite direction.

The daemon treats that state as critical. The paired app shows the row in red with the consequence spelled out, one push notification goes to the phone, and the row offers a single fix that reduces the stop to the quantity still held.

That fix runs through the normal preview and confirm flow. The daemon re-reads the live position at both steps and refuses when position evidence is missing or has moved. Nothing is adjusted automatically.

The order journal underneath heals itself. After every reconnect, and every 30 minutes, the daemon asks the broker for its actual open-order list; journaled orders the broker no longer reports are closed locally as `closed_reconciled`. A cancel or fill that happened while the daemon was offline can no longer leave a stale "open" row behind.

## Gamma

Dealer zero-gamma is the spot price at which the aggregate options-dealer book switches from amplifying market moves (short-gamma, below zero) to stabilizing them (long-gamma, above zero). It is a regime hint rather than a precision level, and the qualitative state is what matters for short-horizon risk.

`ibkr_gamma` and the regime dashboard's dealer-gamma row both compute from IBKR's option chains using the Perfiliev convention (dealers long calls, short puts), summed across the 6 nearest non-0DTE-post-settlement expirations at ±10% strike width. Two methodology choices shape the result.

**Sticky-moneyness skew** (`bs-gamma-profile-v3-stickymoneyness-0dte-split`). The spot sweep reprices each leg's IV at the scenario-spot's *moneyness* via a per-expiry quadratic skew curve fitted at snapshot time: sticky-moneyness rather than sticky-IV. Without this, the put-side skew biases zero-gamma estimates upward by 5–10%.

**SPX/SPXW is the production signal; SPY is corroboration.** SPX index options are the canonical dealer-gamma book for the S&P 500. SPY (continuous ETF, retail flow) is useful context when its option surface is fresh and high quality, but missing or throttled SPY does not downgrade an otherwise fresh, rankable SPX result. When both books are usable, the diagnostic is **disagreement**: one book stabilizing while the other amplifies. The classifier reports `"agree:long-gamma"`, `"agree:short-gamma"`, `"agree:transition-gamma"`, or `"disagree"` directly. A crossing is long, transition, or short based on spot's distance from the identified γ-zero, not merely the existence of a crossing.

Every result carries two complementary readings:

- **Signed zero-gamma**: the price level itself, plus a `gamma_sign` ("positive"/"negative") describing the dealer book's posture at current spot.
- **Sign-agnostic magnitude**: `gamma_total_abs` (sum of |Γ|·OI in notional terms) and `top_strikes`, the largest concentrations regardless of sign.

The Perfiliev sign convention assumes the standard "dealers long calls, short puts" book. Covered-call ETF flow or autocall hedging can invert the sign, so where those flows dominate, lean on the magnitude reading instead.

How much weight a result may carry is stated on it, as `quality.rankability`. Missing OI is unknown, never zero.

[Sensors](sensors.md#gamma) has the operational side of both: what each rankability value permits, what a priced leg without observed OI can still support, and why absent 0DTE alone does not sink an otherwise healthy SPX surface. Compute timing and closed-session behavior live there too. [The gamma cache design](../internals/gamma-cache.md) covers the cache internals, including why a result survives a daemon restart.

## Breadth

S&P 500 breadth tells you whether a rally is broad or narrow, which the index level alone cannot. Two readings carry the load:

- **% above 50-DMA**, the tactical signal. >55 historically marks healthy uptrends; <40 with SPX at highs is the classic narrow-rally warning sign.
- **% above 200-DMA**, the cyclical companion. It tops cleanly when the median name rolls over, even when the index is still being held up by mega-caps.

The daemon also reports 52-week new-highs / new-lows counts and the derived `net_new_highs_pct`. SPX near highs with `net_new_highs_pct` near zero or negative is the most reliable narrow-rally fingerprint.

IBKR does not redistribute S&P DJI's official breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (methodology token: `constituent-fanout-50/200dma+nh-v2`). A once-daily post-close refresh (16:35 ET) slides each name's window forward.

**Cold-start budget**: the first request against a fresh daemon takes about 74 minutes, because IBKR's historical-data pacing caps the constituent fan-out at ~6 names/min sustained. The response carries `state: "computing"` until done. After cold-start, the cache persists across daemon restarts and every subsequent call is instant.

The constituent list is refreshed at runtime too; [Updating](../start/updating.md#updating-the-sp-500-list-automatic) has the cadence and pinning options. Threshold derivation is left to the consumer; suggestions are in the spec.

S&P 500 only today: NDX, RUT, sector-specific, and single-stock breadth are not supported.
