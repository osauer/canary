# Concepts

Updated: 2026-07-19 19:40 CEST

What the load-bearing context surfaces measure, in enough depth to read the output without mis-acting on it. Start with [Sensors](./sensors.md) for authority, freshness, last-good behavior, and safe checks. Methodology rationale lives in the [regime dashboard contract](./specs/risk-regime-dashboard.md); this page is the user's mental model.

---

## Market Calendars

Market calendars answer a simple but risk-relevant question: *is this market supposed to be trading right now, and if not, when does the official session resume?*

The first release is deliberately narrow and official-source only:

- **US equities** (`us` / `us-equity`): regular NYSE/Nasdaq-style cash-equity sessions, holidays, and early closes.
- **US listed options** (`us-options`): regular listed-options sessions. This is a separate calendar because options have their own close window and holiday schedule surface. Per-class global hours, SPX/VIX extended sessions, curb trading, and exercise/settlement nuance are not modeled in v1.
- **German Xetra equities** (`de` / `de-xetra`): Deutsche Boerse Xetra cash-equity sessions and non-trading days. Frankfurt floor trading and Eurex derivatives are not modeled in v1.

Other markets and asset classes are therefore only partly supported today. Futures, FX, crypto, bonds, Eurex, and exchange-specific derivatives calendars should be treated as out of scope unless a result explicitly names a supported market.

Calendars are embedded official schedules, not IBKR overlays. The official exchange calendar is binding for this feature; IBKR quote state still matters for entitlement, routing, and farm-health issues, but it is not used to redefine whether the exchange is open. This keeps cold starts instant and avoids a runtime dependency on remote calendar files. The tradeoff is explicit coverage: the response includes `coverage_start` / `coverage_end`, `days` is capped at 400 calendar days, and dates outside embedded coverage return `state: "unknown"` rather than guessing from weekdays.

`ibkr quote` adds a `session_context` block only when it helps explain stale/frozen/missing data. During an ordinary live regular session with prices present, quote output stays quiet.

---

## Regime

The eight-row risk-regime dashboard summarizes *the market's current posture* in one snapshot. It emits a broad-market lifecycle stage (`quiet`, `early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`, or `data_quality`) plus source health and semantic fingerprints for monitors. Each row measures a different stress channel; together they distinguish "ordinary chop" from "regime shift in progress."

The rows:

1. **VIX term structure** (VIX vs VIX3M). Backwardation (short-dated vol pricing above 3-month vol) is the stress fingerprint. The deeper and more sustained the inversion, the bigger the dislocation.
2. **VVIX vol-of-vol**. Cboe's VIX-of-VIX reading catches convexity demand inside the equity-vol cluster.
3. **HYG vs SPY divergence**. High-yield credit (HYG) leads equity selloffs on the way down; a HYG breakdown while SPY is still near highs is the classic late-cycle warning.
4. **HY/IG OAS**. Official ICE BofA cash-credit spreads via FRED are slower than HYG but harder to dismiss as ETF noise.
5. **Funding spread**. 90-day AA financial commercial paper minus 3-month T-bill flags slow funding/liquidity pressure.
6. **USD/JPY weekly move**. JPY funding-pair unwinds are a recurring stress amplifier (Aug 2024, Dec 2018, Jan 2016). A >3% week is a Tier-1 signal.
7. **Dealer zero-gamma** (SPX canonical, SPY corroboration). Whether the dealer book stabilizes or amplifies day-over-day moves. See the [Gamma](#gamma) section.
8. **S&P 500 breadth**. Whether the index's strength is broad or carried by a handful of mega-caps. See the [Breadth](#breadth) section.

Each row carries raw measurements, status/as-of metadata, green / yellow / red banding, and a `streak` field counting consecutive sessions in the current band; a Day-1 stress event reads differently from a Day-5 one. The lifecycle layer keeps weak or unconfirmed red evidence visible while preventing a single noisy proxy from dominating the broad-market trigger.

Two failure modes worth flagging on the wire:

- Gamma and breadth are heavy computes. Gamma may return `status: "computing"` with an ETA when no serveable result exists; during options RTH, a served result refreshes in the background after 15 minutes. On a fresh daemon, breadth returns `state: "computing"` while the constituent fan-out runs (~60 min cold).
- Live IBKR rows may carry a `fields_missing` array for optional sub-fields that didn't land within the fetch budget. The primary measurement still landed; treat `fields_missing` as a render hint, not an error.

The full methodology spec is at [`docs/specs/risk-regime-dashboard.md`](./specs/risk-regime-dashboard.md). Use it when calibrating your own threshold bands; the spec's suggestions are starting points, not gospel.

---

## Canary

The portfolio canary is narrower than regime: it asks whether today's market weather matters for the portfolio currently held in the account. It consumes account, positions, and regime snapshots, then emits `action`, `market_confirmation`, `portfolio_fit`, `input_health`, planner readiness, source health, and a semantic alert fingerprint for monitor dedupe.

The high-precision rule is intentional: broad-market stress must be confirmed by market evidence, not by the user's own losses or margin pressure. Account-only facts and portfolio-only facts can appear as evidence, but `defend` requires confirmed market pressure, vulnerable portfolio fit, and usable input health. Portfolio-only pressure normally becomes `rebalance` or `watch`.

`portfolio.held_stress[]` is the positions-only single-name stress surface. It is bounded to material held underlyings and appears only when an existing position shows one of these conditions:

- held-name daily P&L shock as a percent of NLV
- near-expiry held-option delta concentration
- held-name stock quote or option bid/ask degradation

Canary does not call option chains, scanners, short-interest feeds, paid borrow vendors, or external flow sources. It consumes the daemon's market-event context for held-name tags and alert fingerprints, but those flags remain context/safety gates rather than standalone execution advice. For deeper diagnosis, use `ibkr_positions`, `ibkr_regime`, `ibkr_market_events`, or `ibkr_account`; canary is the alert boundary, not the investigation.

---

## Market Events

Market events answer a single-name context question: *does this held or requested stock/ETF have borrow, threshold-list, LULD, or halt evidence that should affect risk review or protection proposals?*

V1 flags are reduce-only context and gates. They can annotate, prioritize, or block an existing protection proposal, but they never create buy-to-open, buy-add, or squeeze-style opportunity recommendations. The separate Opportunities surface is daemon-calculated from positions and executable market data; its MVP bucket is option exercise only. When a `BUY` proposal reduces an existing short, the user-facing copy is `Buy to cover`.

The five V1 flags are:

- `borrow_inventory_tight`: IBKR shortable-share inventory crossed the V1 tight/scarce thresholds. This strengthens buy-to-cover context for existing shorts and is observational for long holdings.
- `borrow_fee_extreme`: the current global IBKR short-stock availability file reports an annualized fee rate of at least 50%. This is emitted only from current, policy-eligible FTP evidence, never inferred from low inventory or emitted/cleared from stale data. If a due FTP refresh is unusable, the daemon can inspect exact currently held short-stock contracts through TWS historical `FEE_RATE`; those rows are explicitly portfolio-only, nullable, scale-unverified, and policy-ineligible until a controlled broker fixture commissions the numeric scale.
- `reg_sho_threshold`: the symbol appears on the Nasdaq Reg SHO threshold list. V1 names the source scope explicitly; non-Nasdaq listing-exchange threshold feeds are outside coverage, so absence is not universal non-threshold proof.
- `luld_pause`: a Nasdaq trade-halt reason indicates an active or recent LULD pause. Active LULD blocks proposal preview/submit; recent LULD is a warning requiring fresh quote context.
- `halt_regulatory_or_news`: a regulatory/news halt is active or recent. Active halts are hard blockers; recent halts are warning tags.

Unknown and null mean unavailable, not false or zero. Source health reports whether each feed is `ok`, `partial`, `stale`, `unknown`, or `degraded`; stale/unknown source health must stay visible because it changes how much confidence absence of a flag deserves. `borrow_fee_coverage[]` additionally distinguishes global FTP coverage from exact-contract, held-short-only TWS context and names entitlement/scale/policy eligibility directly.

Rule 201 / short-sale restriction is not a V1 protection driver. If added later, it should be context-only unless the order path is directly short-sale relevant.

`ibkr market-events --symbol GME --json` evaluates explicit symbols. Omitting symbols evaluates held stock/ETF underlyings, which requires a usable positions snapshot from the daemon/gateway.

---

## Protective stops

A protective stop is only protective while it matches the position. Sell part of the position somewhere else, in TWS for instance, and the stop keeps its old size. If it then triggers, it closes what is left and opens the remainder in the opposite direction. The daemon treats that state as critical: the paired app shows the row in red with the consequence spelled out, one push notification goes to the phone, and the row offers a single fix that reduces the stop to the quantity still held. The fix runs through the normal preview and confirm flow. The daemon re-reads the live position at both steps and refuses when position evidence is missing or has moved. Nothing is adjusted automatically.

The order journal underneath heals itself. After every reconnect, and every 30 minutes, the daemon asks the broker for its actual open-order list; journaled orders the broker no longer reports are closed locally as `closed_reconciled`. A cancel or fill that happened while the daemon was offline can no longer leave a stale "open" row behind.

---

## Gamma

Dealer zero-gamma is the spot price at which the aggregate options-dealer book switches from amplifying market moves (short-gamma, below zero) to stabilizing them (long-gamma, above zero). It's a regime hint, not a precision level, but the qualitative state matters for short-horizon risk.

`ibkr_gamma` and the regime row's indicator 4 both compute from IBKR's option chains using the Perfiliev convention (dealers long calls, short puts), summed across the 6 nearest non-0DTE-post-settlement expirations at ±10% strike width. Two key methodology choices:

1. **Sticky-moneyness skew** (`bs-gamma-profile-v3-stickymoneyness-0dte-split`). The spot sweep reprices each leg's IV at the scenario-spot's *moneyness* via a per-expiry quadratic skew curve fitted at snapshot time: sticky-moneyness rather than sticky-IV. Without this, the put-side skew biases zero-gamma estimates upward by 5–10%.

2. **SPX/SPXW is the production signal; SPY is corroboration**. SPX index options are the canonical dealer-gamma book for the S&P 500 regime signal. SPY (continuous ETF, retail flow) is useful context when its option surface is fresh and high quality, but missing or throttled SPY does not downgrade an otherwise fresh, rankable SPX result. When both books are usable, the diagnostic is **disagreement**: one book stabilizing while the other amplifies. The classifier reports `"agree:long-gamma"`, `"agree:short-gamma"`, `"agree:transition-gamma"`, or `"disagree"` directly so consumers don't have to derive it. A crossing is long/transition/short based on spot's distance from the identified γ-zero, not merely the existence of a crossing.

Two complementary outputs on every result:

- **Signed zero-gamma**: the price level itself, plus a `gamma_sign` ("positive"/"negative") describing the dealer book's posture at current spot. The Perfiliev sign convention assumes the standard "dealers long calls, short puts" book; in regimes dominated by covered-call ETF flow or autocall hedging the sign can invert. When those flows dominate, trust the sign less and lean on the magnitude view below.
- **Sign-agnostic magnitude**: `gamma_total_abs` (sum of |Γ|·OI in notional terms) and `top_strikes` (the largest concentrations regardless of sign). Sign-convention agnostic, so it stays useful when the signed reading is suspect.

Every ready gamma result also carries `quality.rankability`:

- `rankable`: fresh and covered enough for `regime` / `canary` to count the gamma band as market evidence. A rankable SPX result is stable production signal even when SPY is unavailable and disclosed as context.
- `context_only`: useful structure context, but not independent confirmation.
- `blocked` / `unavailable`: do not rank gamma or confirm stress from it.

The quality object records session key, age, coverage, OI observed/positive ratios, horizon coverage, derived-IV share, skew fit quality, strike concentration, and explicit blockers/context notes. Missing OI is unknown, never zero. Priced legs without observed OI may help IV/skew fitting, but they do not contribute OI-weighted GEX.

Missing 0DTE remains visible in horizon coverage and warnings, but it is not
alone a no-vote when SPX has healthy 1-7DTE and term coverage.
After the expiring SPXW series closes, the 0DTE bucket can be absent while the
broader SPX surface remains usable.

Compute timing: the first eligible call without a serveable result kicks a multi-minute background job. During options RTH, later calls keep serving the last-good result and trigger one background refresh after 15 minutes; a successful replacement is promoted atomically. Closed-session automatic refresh is suppressed, and the latest completed-session result remains Regime `not_due` context until the next options open. The cache persists across daemon restarts.

Full methodology at [`docs/specs/risk-regime-dashboard.md`](./specs/risk-regime-dashboard.md). Cache persistence details are in [`docs/design/gamma-zero-cache-persistence.md`](./design/gamma-zero-cache-persistence.md).

---

## Breadth

S&P 500 breadth tells you whether a rally is broad or narrow, which the index level alone can't. Two readings carry the load:

- **% above 50-DMA**: the tactical signal. >55 historically marks healthy uptrends; <40 with SPX at highs is the classic narrow-rally warning sign.
- **% above 200-DMA**: the cyclical companion. Tops cleanly when the median name rolls over, even when the index is still being held up by mega-caps.

The daemon also reports 52-week new-highs / new-lows counts and the derived `net_new_highs_pct`. The "SPX near highs with net_new_highs_pct near zero or negative" pattern is the most reliable narrow-rally fingerprint.

IBKR doesn't redistribute S&P DJI's official breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (methodology token: `constituent-fanout-50/200dma+nh-v2`). A once-daily post-close refresh (16:35 ET) slides each name's window forward.

**Cold-start budget**: the first request against a fresh daemon takes ~60 minutes; IBKR's historical-data pacing caps the constituent fan-out at ~6 names/min sustained. The response carries `state: "computing"` until done; after cold-start, the cache persists across daemon restarts and every subsequent call is instant.

The constituent list itself is also refreshed at runtime; see [Updating](./guides/updating.md#updating-the-sp-500-list--automatic) for the cadence and pinning options. Threshold derivation is left to the consumer; suggestions are in the spec.

S&P 500 only today: NDX, RUT, sector-specific, and single-stock breadth are not supported.
