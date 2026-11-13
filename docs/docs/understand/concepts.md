# Concepts

Updated: 2026-11-07 08:23 CET

What the load-bearing context surfaces measure, in enough depth to read the output without mis-acting on it. This page is the mental model. [Sensors](sensors.md) owns authority, freshness, last-good behaviour, and the safe checks; the [regime dashboard contract](../internals/regime-dashboard.md) owns methodology.

## Market calendars

Calendars answer one risk-relevant question: is this market supposed to be trading right now, and if not, when does the official session resume?

Supported calendars use official exchange schedules. Each one starts on
2026-01-01; this table is the reference for where each one's coverage ends:

| `--market` | Market (token) | Local regular session | Coverage ends |
| --- | --- | --- | --- |
| `us` | US cash equities, NYSE/Nasdaq (`us_equity`) | 09:30–16:00 New York | 2028-12-31 |
| `us-options` | US listed options, regular sessions (`us_options`) | 09:30–16:15 New York | 2028-12-31 |
| `de` | Xetra cash equities (`de_xetra`) | 09:00–17:30 Frankfurt | 2028-12-31 |
| `uk` | London Stock Exchange, SETS (`uk_lse`) | 08:00–16:30 London | 2028-12-31 |
| `jp` | Tokyo cash equities (`jp_tse`) | 09:00–11:30, 12:30–15:30 Tokyo | 2027-12-31 |
| `hk` | Hong Kong cash equities (`hk_hkex`) | 09:30–12:00, 13:00–16:00 Hong Kong | 2027-12-31 |
| `sg` | Singapore securities, SGX (`sg_sgx`) | 09:00–12:00, 13:00–17:00 Singapore | 2027-12-31 |
| `au` | ASX cash equities (`au_asx`) | 10:00–16:00 Sydney | 2027-12-31 |
| `cn` | China A-shares, SSE/SZSE (`cn_ashares`) | 09:30–11:30, 13:00–15:00 Shanghai | 2026-12-31 |

The token in brackets is the `market` value a response carries; `--market`
accepts it too, as well as aliases such as `us-equity`, `xetra` or `lse`.
Early closes and half-days come from the same tables. What each calendar
leaves out:

- **US equities**: other US products may keep different hours.
- **US listed options**: separate from equities because options have their own close window and holiday schedule. Per-class global hours, SPX/VIX extended sessions, curb trading, and exercise/settlement nuance are not modelled.
- **Xetra**: Frankfurt floor trading and Eurex derivatives are not modelled.
- **London, Tokyo, Hong Kong, Singapore, ASX**: ordinary cash sessions only; auctions, special products and unscheduled halts are outside the calendar.
- **China A-shares**: an IBKR account reaches them northbound through Stock Connect, which trades only on days both Hong Kong and the mainland are open and skips some settlement-constrained days. Check the HKEX Stock Connect calendar before relying on a date near a holiday.

Intraday `windows` describe the actual trading intervals; a lunch break is not
an open session. Preserve the returned coverage limits for each market.

Treat futures, FX, crypto, bonds, Eurex, and exchange-specific derivatives as out of scope unless a result explicitly names a supported market.

The schedules are embedded, not IBKR overlays, so cold starts are instant and nothing depends on a remote calendar file at runtime. The official exchange calendar is binding here. IBKR quote state still matters for entitlement, routing, and farm-health issues, but it never redefines whether the exchange is open.

The cost is bounded coverage: the response carries `coverage_start` / `coverage_end`, `days` caps at 400 calendar days, and dates outside embedded coverage return `state: "unknown"` rather than guessing from weekdays.

Typed quote evidence carries `session_context` when it helps explain stale,
frozen, or missing data. In an ordinary live regular session with prices
present, the field stays quiet. The brief, rulebook, proposals, alerts and app
read it; there is no separate quote command.

## Regime

The eight-row risk-regime dashboard summarises the market's current posture. Each row measures a different stress channel, which is what separates ordinary chop from a regime shift in progress. It also emits a broad-market lifecycle stage (`quiet`, `early_warning`, `confirmed_stress`, `panic`, `stabilization`, `opportunity`, or `data_quality`), source health, and semantic fingerprints for monitors.

1. **VIX term structure** (VIX vs VIX3M). Backwardation, short-dated vol pricing above 3-month vol, is the stress fingerprint. The deeper and more sustained the inversion, the bigger the dislocation. IBKR quotes drive the row in session; once Cboe's publication window shuts, the VIX3M leg is Cboe's own dated close, which also cross-checks what the broker is still reporting.
2. **VVIX vol-of-vol**. Cboe's VIX-of-VIX reading catches convexity demand inside the equity-vol cluster.
3. **HYG vs SPY divergence**. High-yield credit leads equity selloffs on the way down. A HYG breakdown while SPY is still near highs is the classic late-cycle warning.
4. **Credit spreads**. Official ICE BofA HY and IG cash-credit spreads (OAS) via FRED: slower than HYG, harder to dismiss as ETF noise.
5. **Funding spread**. 90-day AA financial commercial paper minus 3-month T-bill, flagging slow funding and liquidity pressure.
6. **USD/JPY weekly move**. JPY funding-pair unwinds are a recurring stress amplifier (Aug 2024, Dec 2018, Jan 2016). The row turns amber when the yen strengthens 1-2% in a week and red above 2%.
7. **Dealer zero-gamma** (SPX canonical, SPY corroboration). Whether the dealer book stabilises or amplifies day-over-day moves. See [Gamma](#gamma).
8. **S&P 500 breadth**. Whether the index's strength is broad or carried by a handful of mega-caps. See [Breadth](#breadth).

Every row bands green / yellow / red and carries a `streak` count of consecutive sessions in that band. A Day-1 stress event reads differently from a Day-5 one. The lifecycle layer keeps weak or unconfirmed red evidence visible while stopping a single noisy proxy from dominating the broad-market trigger.

Two things to expect on the wire. Gamma and breadth are heavy computes: gamma reports `status: "computing"` with an ETA, breadth `state: "computing"`, until a result is serveable. [Sensors](sensors.md) has the refresh and last-good rules. Live IBKR rows may carry a `fields_missing` array for optional sub-fields that missed the fetch budget; the primary measurement still landed, so treat it as a render hint, not an error.

Calibrate your own threshold bands against [the regime dashboard contract](../internals/regime-dashboard.md); its suggestions are starting points, not gospel.

## Stress

The portfolio stress read is narrower than regime: it asks whether today's market weather matters for the portfolio currently held. From account, positions, and regime snapshots it derives an action, planner readiness, and a semantic alert fingerprint for monitor dedupe. [Sensors](sensors.md#stress) lists the output fields.

The high-precision rule is intentional: broad-market stress must be confirmed by market evidence, not by the user's own losses or margin pressure. Account-only facts and portfolio-only facts can appear as evidence, but `defend` requires confirmed market pressure, vulnerable portfolio fit, and usable input health. Portfolio-only pressure normally becomes `rebalance` or `watch`.

`portfolio.held_stress[]` is the positions-only single-name stress surface. It is bounded to material held underlyings and appears only when an existing position shows one of these conditions:

- held-name daily P&L shock as a percent of NLV
- near-expiry held-option delta concentration
- held-name stock quote or option bid/ask degradation

The stress read calls no option chains, short-interest feeds, paid borrow vendors, or external flow sources. It does consume the daemon's market-event context for held-name tags and alert fingerprints, and those flags remain context and safety gates rather than standalone execution advice.

Stress marks the alert boundary. The diagnosis behind an alert comes from the
brief/rulebook evidence, account and positions reads, or the matching app
Monitor window.

## Mandate

The mandate bounds how much the whole book may lose. You write it as the
`[mandate]` table of the personal risk policy; without the table none of
this runs. [Writing a risk policy](risk-policy.md#what-each-section-governs)
has every key.

It compares two figures:

- The **loss measure** is what the book stands to lose by the mandate's count:
  today's value of every bought option, what it can still lose (a hedge-listed
  long put is protection: its value is named beside the measure and left out), plus the cash loss to the stop of every
  share line a working stop or trail covers, plus a held index-hedge future's
  loss in a 10% index move against it (or to its stop, when a working stop
  covers the whole line). A share line without a stop counts nothing and is
  named.
- The **loss room** is how much loss the mandate still allows: `max_loss_pct`
  of the budget base, less the drawdown already taken. The risk base is equity
  after the tax reserve; the budget base is its high-water mark, moved by
  deposits and withdrawals since the mandate's start.

The verdict reads `within` while the measure is inside the room and no single
name breaches its gap cap, the loss allowed if that name gaps. It reads
`reduce` when either fails, and `unknown` while an input is missing. A stock or
ETF order that adds risk is refused with `mandate_budget` while the verdict is
unknown, when it would take the measure over the room or deepen a gap breach,
or when it takes a sleeve over its risk allocation. Option buys are held to the
premium limits instead (`mandate_premium`, see
[The rulebook](rulebook.md#limits-that-refuse-or-hold-orders)).

The risk budget is the risk the mandate lets the whole book carry:
`vol_target_pct` a year of the budget base, shrinking with the room left. The
sleeves divide it as risk allocations: your own trading, the core index fund
and the rule-based strategies each have one.

**Forced reduction.** When two consecutive scheduled verdicts, at least five
and at most fifteen minutes apart, read `reduce`, the daemon proposes one
batch of reduce-only orders that cuts the book back to 95% of the room. It
first cuts any name over its gap cap, then the premium at risk left, each
time the cheapest relief first: the most of the loss measure removed per unit
of market value given up, so a deep out-of-the-money option goes before a
dated one and both before shares. It never sells protection, a hedge-listed
long put included, and never sells a holding on your keep list. It never buys
back a short option or sells the shares that cover one. The rows wait for your
confirmation, unless the protection policy pre-authorises `mandate_reduction`
and a standing authority is armed.
[Forced reduction](../operate/protection.md#forced-reduction) covers the batch
in detail.

**Regime gate.** The gate takes three readings on the latest settled US
close: SPY above its ten-month average, more than half of S&P 500 stocks above
their 50-day average ([Breadth](#breadth)), and HYG above its 50-day average.
With all three on it gives full size; with two, half; with one or none, no new
entries and no adds. Strategy one's screen (`canary strategy-one`) sizes its
trade plans by it. No strategy that places orders reads it, so the gate
changes no order. The gate reads those closes itself and is separate from the
[regime](#regime) stage above.

`canary mandate` shows the verdict, the reduction's state and the gate.

## Portfolio allocation

`canary portfolio` and the `canary_portfolio` tool answer one question: where the money sits. Two tables, both in signed market value as a share of net liquidation, so a book adds up with its cash. The `asset_class_measure` and `sector_measure` fields name that measure on every response.

This is allocation, not directional risk. An option counts at its premium value, so a long put is a positive asset here even though it is short the market. The delta-weighted figure lives in the positions projection as `portfolio.exposure_base[].dollar_delta_base`; a sector bar built from it can exceed the whole account many times over in an options-heavy book, which answers a risk question and not a composition one.

**Asset classes** are Cash, Stocks, Funds, Options, and Other. A fund is a stock row whose broker stock type is ETF, ETN, or another pooled vehicle.

**Sectors** follow GICS. Each underlying is classified in this order, and the first source with an answer wins:

1. The embedded fund table: SPY, QQQ, and IWM.
2. The broker's stock type, which sends any other fund to the pooled row.
3. Wikipedia's GICS sector for S&P 500 names, taken from the same constituents page the [breadth refresh](../start/updating.md#updating-the-sp-500-list-automatic) fetches daily.
4. A fixed map from the broker's industry and category. The category disambiguates where the broker's ten-sector scheme is coarser than GICS: pharmaceuticals, biotechnology, and health-care categories become Health Care, food and beverages become Consumer Staples, REITs become Real Estate, and the broker's "Internet" filing of online retailers becomes Consumer Discretionary.
5. Otherwise the row is Unclassified.

Options take their underlying's classification.

**Look-through** spreads a held index fund, stock or option, over that fund's published sector weights. Only the three funds in the embedded table look through; the weights are a dated snapshot, and `look_through[]` names each fund's `as_of` and `source` so a stale split is visible rather than silent. Every other fund pools under "Funds (no look-through)". Cash and derivatives inside a fund are not sector allocation, so a fund's weights need not sum to 100.

**Exclusions.** A holding the broker no longer quotes carries no value and is counted in `defunct_excluded`; a stock row flagged zero-mark and zero-value pending the broker's verdict is counted in `unquoted_excluded`. Neither appears in either table. Both remain account position truth in `canary positions`, where reconciliation needs them.

**Coverage.** Each row carries `observed` and `missing` counts. A row with any missing input reports no percentage: a stale price, an absent currency conversion, or a valuation the broker withheld is unvalued, never zero. `coverage_status` reads `partial` whenever a row is missing an input, an underlying is unclassified, net liquidation is unavailable, or cost basis could not be observed.

Known limits: the broker map is a best effort for names outside the S&P 500 and stays conservative, so an unknown pairing lands in Unclassified and the daemon logs the pairing without its symbol. Fund weights change only with a release. The S&P sector list refreshes with the membership and is kept in daemon.db beside it, so a restart resumes from the last parsed map; the embedded baseline applies only on a fresh install, or after a release whose baseline is newer than the stored map, until the next refresh. Broker classifications are asked once per broker session and kept until the session changes; the lookup phase of a first, uncached snapshot is bounded at 20 seconds, so names left over on a large book read Unclassified on that call and resolve on the next. A name the broker has no definition for is kept as unclassified for the session.

## Market events

Market events answer a single-name context question: does this held or requested stock or ETF have borrow, threshold-list, LULD, or halt evidence that should affect risk review or protection proposals?

The flags are reduce-only context and gates. They can annotate, prioritise, or block an existing protection proposal, but they never create buy-to-open, buy-add, or squeeze-style opportunity recommendations. The separate Opportunities surface is daemon-calculated from positions and executable market data, and covers option exercise only. When a `BUY` proposal reduces an existing short, the user-facing copy is `Buy to cover`.

The five flags:

- `borrow_inventory_tight`: IBKR shortable-share inventory fell to 10,000 shares or fewer (tight) or 1,000 or fewer (scarce). Strengthens buy-to-cover context for existing shorts; observational for long holdings.
- `borrow_fee_extreme`: the current global IBKR short-stock availability file reports an annualised fee rate of at least 50%. Emitted only from current, policy-eligible FTP evidence, never inferred from low inventory, and never emitted or cleared from stale data.
- `reg_sho_threshold`: the symbol appears on the Nasdaq Reg SHO threshold list. Non-Nasdaq listing-exchange threshold feeds are outside coverage, so absence is not universal non-threshold proof.
- `luld_pause`: a Nasdaq trade-halt reason indicates an active or recent LULD pause. Active LULD blocks proposal preview/submit; recent LULD is a warning requiring fresh quote context.
- `halt_regulatory_or_news`: a regulatory/news halt is active or recent. Active halts are hard blockers; recent halts are warning tags.

When a due FTP refresh is unusable, the daemon can inspect exact currently held short-stock contracts through TWS historical `FEE_RATE`. Those rows are portfolio-only, nullable, scale-unverified, and policy-ineligible until a controlled broker fixture commissions the numeric scale. `borrow_fee_coverage[]` separates global FTP coverage from that held-short-only TWS context and names entitlement, scale, and policy eligibility directly.

Unknown and null mean unavailable, not false or zero. Each feed's health reads `ok`, `partial`, `stale`, `unknown`, or `degraded`, and [Sensors](sensors.md#market-events) defines what each state permits. Stale and unknown health stays visible because it changes how much confidence the absence of a flag deserves.

Rule 201, the short-sale restriction, does not drive protection.

Canary evaluates held stock and ETF underlyings from a usable positions
snapshot. The flags appear in the rulebook, proposals, alerts, the brief and
the app; there is no standalone command.

## Protective stops

A protective stop is only protective while it matches the position. Sell part of the position somewhere else, in TWS for instance, and the stop keeps its old size. If it then triggers, it closes what is left and opens the remainder in the opposite direction.

The daemon treats that state as critical. The paired app shows the row in red with the consequence spelled out, one push notification goes to the phone, and the row offers a single fix that reduces the stop to the quantity still held.

That fix runs through the normal preview and confirm flow. The daemon re-reads the live position at both steps and refuses when position evidence is missing or has moved. For a sell stop or trail Canary itself placed on a stock, ETF or option, the daemon's protective stop guard makes the same fix on its own when trading is enabled: it shrinks the stop to the position, or cancels it at zero, through the ordinary modify and cancel gates ([Protection](../operate/protection.md#protective-stop-exemption-and-guard)). A sale you send through Canary beside that stop is linked to it in a broker one-cancels-all group, so the two never sell the line twice. An order placed by hand in TWS is reported and never adjusted.

The order journal underneath heals itself. After every reconnect, and every 30 minutes, the daemon asks the broker for its actual open-order list; journaled orders the broker no longer reports are closed locally as `closed_reconciled`. A cancel or fill that happened while the daemon was offline can no longer leave a stale "open" row behind.

## Gamma

Gamma describes a conditional response to market moves: positive modelled gamma
suggests damping in either direction; negative modelled gamma suggests
amplification. It does not predict whether the next move will be up or down.

**SPX/SPXW is the production signal; SPY is corroboration.** The index and ETF
books retain separate price levels, quality and expiry horizons. A missing SPY
surface does not downgrade a healthy SPX result. SPY alone is labelled a proxy.

The signed model assigns positive exposure to calls and negative exposure to
puts. Open interest does not reveal customer/dealer ownership, opening/closing
trades or intraday inventory, so this is an assumption, not an observed dealer
book. Cboe explains why gross options activity cannot establish net dealer
positioning in its [SPX 0DTE market-impact analysis](https://www.cboe.com/insights/posts/volatility-insights-evaluating-the-market-impact-of-spx-0-dte-options).

**Local sign and crossings.** Method `bs-gamma-profile-v4-local-sign-class-skew`
evaluates signed GEX at the actual observed spot. The sweep includes spot,
strikes and extra points around narrow expiry kernels, then refines detected
sign changes. It reports all detected crossings and selects the nearest one.
Being above a crossing does not by itself mean positive gamma: a profile can
have several crossings or reverse the usual orientation. Within the existing
2% transition distance, the reading is transitional; farther away, local sign
decides long or short gamma. A balanced signed reading with nonzero gross
exposure is a transition, while absent measurements remain unavailable.

Scenario IV preserves each leg's observed IV at spot, applying the relative
change from a quadratic moneyness curve fitted separately for each trading
class and expiry. SPX morning settlement and SPXW afternoon settlement are
kept distinct. An unusable or nonpositive curve falls back consistently to
sticky IV. The Black–Scholes model currently uses zero rates and dividends;
this approximation and sampled chain coverage limit precision.

**Expiry context.** The 0DTE, 1–7 DTE and term horizons each retain their own
local sign, gross exposure share and availability. Disagreement can reveal
short-lived amplification inside a broadly damping book. Missing horizons
remain missing; agreement between two covered horizons is partial agreement.

**Downside versus upside pricing.** Eligible model-tick or live-mid IVs are
interpolated between observed strikes bracketing 25-delta puts and calls in the
same trading class and expiry. The selected expiry is the covered 7–60 day
expiry nearest 30 days, with its actual tenor disclosed. Positive put-minus-call
IV means richer downside protection; negative means richer upside exposure.
This is pricing asymmetry, not a bullish/bearish probability. There is no
extrapolation, no previous-close-derived IV in this comparison, and no invented
value when either side is missing. The convention follows the put/call skew
comparison in [Cboe's option-sentiment specifications](https://datashop.cboe.com/Documents/Cboe_OptionSentiment_Specs.pdf), without claiming its standardized 30-day index.

`gamma_total_abs` is gross sampled convexity scaled to a 1% move, not actual
net dealer hedging flow. `top_strikes` retains individual option-contract
concentrations, not aggregated strike walls. `profile_metrics.gex_at_spot` is
the signed counterpart. Priced legs without observed OI may support skew, but
cannot contribute OI-weighted exposure.

Quality, feed type, observation time, assumptions and missing data travel with
the explanation through the daily Brief, CLI, MCP and app Monitor. Only the
existing eligible gamma evidence may affect Regime; skew adds context and does
not change trading thresholds. [Sensors](sensors.md#gamma) explains freshness
and ranking. [Gamma cache design](../internals/gamma-cache.md) explains persistence.

## Breadth

S&P 500 breadth tells you whether a rally is broad or narrow, which the index level alone cannot. Two readings carry the load:

- **% above 50-DMA**, the tactical signal. >55 is the existing green band; <40 with SPX at highs is the classic narrow-rally warning sign. The mandate's [regime gate](#mandate) reads it each day too: more than half above is one of its three readings.
- **% above 200-DMA**, the cyclical companion. It tops cleanly when the median name rolls over, even when the index is still being held up by mega-caps.

The daemon also reports 52-week new-highs / new-lows counts and the derived `net_new_highs_pct`. SPX near highs with `net_new_highs_pct` near zero or negative is context for a narrow rally, not a validated forecast.

IBKR does not redistribute S&P DJI's official breadth indices on retail subscriptions, so the daemon computes all three locally from the 500 constituent daily closes pulled via IBKR's historical-bar feed (methodology token: `constituent-fanout-50/200dma+nh-v3`). A once-daily post-close refresh (16:35 ET) slides each name's window forward.

Each percentage includes its own eligible-member denominator and total
membership count. Missing 200-session or annual history is unavailable, never
zero. Annual highs/lows compare the latest close with the preceding 252
session closes; 253 closes are retained so a corrected latest close does not
change the comparison window. The cold request spans 400 calendar days (the
broker rounds this to `2 Y`), and refreshes catch up across missed calendar
days. Invalid constituent responses preserve the last valid window and remain
excluded when that window is not current.

**Cold-start budget**: the first request against a fresh daemon takes about 74 minutes, because IBKR's historical-data pacing caps the constituent fan-out at ~6 names/min sustained. The response carries `state: "computing"` until done. After cold-start, the cache persists across daemon restarts and every subsequent call is instant.

The constituent list is refreshed at runtime too; [Updating](../start/updating.md#updating-the-sp-500-list-automatic) has the cadence and pinning options. Threshold derivation is left to the consumer; suggestions are in the spec.

S&P 500 only today: NDX, RUT, sector-specific, and single-stock breadth are not supported.
