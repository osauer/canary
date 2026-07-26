# Fresh-Ideas Screen — Minimal Canary MCP Path

Last updated: 2026-05-27 21:41 CEST

Use Canary's analysis-only MCP tools to surface **1-3 fresh trade ideas** (maximum 5 only if the tape genuinely offers them) in US and, when Xetra is open, German equities/options. The target is a credible **3-6 month violent move**: long ideas should plausibly double, short/put ideas should offer comparable payoff. Produce plans only; never place, preview, modify, or cancel orders.

Optimize for **decision quality per tool call and wall-clock latency**. A good run proves data readiness, scans wide enough, narrows once, validates only the finalists, and stops. Run independent MCP calls in parallel when the client supports it; do not serialize readiness, context, batch technical, or batch quote calls unless a later call depends on the prior result. Do not narrate progress while running; emit only the final report or a readiness/partial-stop report.

## Defaults

- `ALLOW_OFFHOURS=false`: if US listed options are closed or live option IV is unavailable, stop unless `TEST_MODE=true`.
- `TEST_MODE=false`: if true, run a shares-only smoke path, label it clearly, skip option structures, and cap universe work at one scan plus one technical call.
- Target budget after preflight: **12-16 tool calls**. Hard cap: **22 total tool calls**. Do not spend calls trying to rescue a weak screen.
- Primary scanner floor: `min_price:5`, `require_live:true`, `exclude_penny:true`, `limit:12`. Gate liquidity after scanning with `canary_technical.avg_dollar_volume_20d >= 50000000`; scanner live volume can be nil even when quotes are firm.
- Candidate caps: default 1 scan, at most 2 scans, 10 technical symbols, 5 quote survivors, 3 final ideas, 2 option-chain candidates, 2 sizing calls per idea maximum.
- PHIA probability yardstick is mandatory for all forward-looking claims: `remote <=5%`, `highly unlikely 10-20%`, `unlikely 25-35%`, `realistic possibility 40-50%`, `likely 55-75%`, `highly likely 80-90%`, `almost certain >=95%`.

## Minimal Path

### 1. Readiness Gate

Run this first. If any hard gate fails, stop with a readiness report that names the blocker, the next relevant market open, and the exact path to run later.

1. `canary_status`: require gateway connected and quote/scanner/chain subsystems not unavailable.
2. `canary_calendar` with `market:"us-options"`: require US option RTH open for a live options run. If German names may be scanned, also call `market:"de"` and skip German scans when Xetra is closed.
3. `canary_quote` for `["SPY"]`: require `quote_quality:"firm"` during US RTH. Treat indicative, stale, wide, or previous-close-only quotes as not trade-selectable.
4. `canary_chain` for `SPY` with `require_live_iv:true` and no `expiry`: require any usable live IV and `implied_move`. This is only a live-IV readiness probe; do not require the default expiry list to include the later 90-180 DTE trade horizon.
5. `canary_rules`: require a current policy fingerprint and capture breached or missing rules. If policy is unavailable or conflicting, return `insufficient_policy` and keep all candidates watch-only.
6. `canary_stress`: capture the governed market/portfolio posture and input health. Do not promote a candidate when it would worsen an existing breach or when the stress read says inputs need confirmation.

If the SPY chain returns `live_option_iv_unavailable`, `expiry_iv_unavailable`, no implied move, or times out, stop unless `TEST_MODE=true`. Do not continue into scans with fake option confidence.

### 2. Context In Three Calls

Only after readiness passes:

1. `canary_regime`: use its embedded gamma and breadth context. Do not separately call `canary_gamma` or `canary_breadth` unless the regime result explicitly says those rows are unavailable and one extra call would change sizing or direction.
2. `canary_account`: capture NLV, buying power, cash, margin, base currency.
3. `canary_positions`: treat holdings as exposure context. Exclude held tickers from "fresh" ideas unless labeling them as add-ons.

Classify the tape as **early / mid / mid-late / late** cycle with one PHIA confidence tag. Infer 2-3 themes from the regime plus current scan evidence; avoid web/Reddit browsing in the default run.

### 3. Universe Funnel

Run one scan first and a second scan only if the first scan leaves fewer than 2 non-held, non-ETF, trade-selectable candidates after the technical liquidity plus quote-quality gates. Watch-only names do not count as trade-selectable. **Do not call `canary_scan_params` in the default path.** The scan codes below are known; call `canary_scan_params` only after a scan fails with an unsupported-code/location error, then spend at most one recovery call.

- US RTH default: run `HIGH_LAST_VS_EMA50` first, using `instrument:"STK"` and `exchange:"STK.US.MAJOR"`. If a second scan is justified, prefer `HIGH_VS_52W_HL` or `HIGH_LAST_VS_EMA200`. Use `MOST_ACTIVE_USD` only as the liquidity fallback when the breakout scan leaves too few usable single-name candidates; it is broad-market beta heavy and usually less fresh. Avoid `HOT_BY_VOLUME` in the default latency path; it is a noisy fallback for unusual IPO/SPAC-style tapes and commonly returns poor liquidity after a slow enrichment pass.
- If Xetra is open and the US tape is thin, use one US scan and one German scan with `instrument:"STOCK.EU"`, `exchange:"STK.EU.IBIS"`, preferably `HIGH_VS_52W_HL` or `HIGH_LAST_VS_EMA50`.
- If both primary scans return zero rows during US RTH, run exactly one recovery scan: `MOST_ACTIVE_USD`, same exchange/instrument, `limit:12`, `min_price:5`, `exclude_penny:true`, `require_live:false`. This is a scanner-enrichment recovery only; do not accept any recovered name without firm quote validation and technical liquidity.
- Dedupe immediately to at most 10 names. Exclude held tickers, broad index ETFs, and leveraged ETFs before the technical call unless explicitly labeling them as hedges or add-ons. Prefer fresh breakouts, high relative volume, liquid optionable names, and non-obvious value-chain beneficiaries. Keep one "Other" slot for an exceptional setup.
- `canary_technical`: batch by market, up to 5 symbols per call and 2 calls total. Drop `data_quality!="ok"` or `avg_dollar_volume_20d < 50000000`. Do not retry benchmark warnings; use trend and liquidity fields or drop.
- `canary_quote`: validate at most 5 survivors, split US/German only if needed. Drop stale/wide/illiquid rows.

If the first scan took more than ~20 seconds or returned mostly micro/small-cap squeezes, do not spend a second scan unless the liquidity plus quote-quality gates leave fewer than 2 trade-selectable candidates. Shortlist 2-5 names. For each, record theme, direction, one-line edge, trend/RS evidence, liquidity, and why it can move violently in 3-6 months. For sub-$25 names with quote `spread_pct > 0.25` (percentage points), technical `atr_pct > 0.10` (decimal fraction), or a one-day move over 50%, use watch-candidate language unless a live option chain and catalyst-quality evidence support a real plan; these names do not satisfy the second-scan stop condition. If the shortlist is mostly mega-cap beta, already very extended, option-untradable, or unlikely to double without a fresh catalyst, say so and stop with 1-2 watch candidates instead of forcing trades.

### 4. Finalist Structure And Sizing

Pick 1-3 final ideas. Use options only when live chain data supports them.

- For at most 2 finalists, call `canary_chain` without `expiry` using `min_dte:90` and `max_dte:180` (or `target_dte:120` when one expiry is enough). Choose one 90-180 DTE expiry with usable IV and 1-sigma move, then call one strike grid with small `width` and the relevant `side`. Do not use `all_expiries:true` for this path.
- Hard-gate options on `options_tradable:false`, stale/model-only legs, subscribe errors, no bid/ask, no IV, no implied move, or `live_option_iv_unavailable`/`expiry_iv_unavailable`. Count expiry-list IV failures and untradable grids as chain failures; after two chain failures total or one hard-gated grid, stop chain probing and use shares, watch candidates, or drop.
- Prefer simple structures: shares, long call/put, or defined-risk debit spread. No 0DTE. No multi-leg complexity unless both legs have live quotes and tight spreads.
- Size only against the current user-approved rulebook or an explicit current-turn risk budget. For shares, use `canary_size` with entry/stop/target. For non-base trades, `canary_size.fx` expects quote-currency units per 1 base-currency unit; `canary_account.currency_exposure.exchange_rate` and `canary_positions.fx_rate` are base-currency units per 1 quote-currency unit, so invert those fields before passing `fx`. If policy or a reliable FX rate is absent, keep the name watch-only and say sizing is blocked. For long options, size premium risk by using contract debit x 100 as entry, stop 0, target debit x 100 when estimating R; label the result as contract count math, not an executable order.
- Fit each idea against open exposure, Greeks, and regime. If it conflicts, keep it only with a countermeasure: smaller risk, staggered entry, hedge, or shares-only expression.

### 5. Compact Report

Lead with the punchline. Use compact Markdown, not HTML, unless explicitly requested.

1. **Readiness + Tape:** data status, regime/cycle call, 2-3 themes, and account exposure constraints.
2. **Funnel:** scans used, shortlist, and why names were cut.
3. **Ideas:** each idea gets thesis/catalyst, direction, instrument, entry/stop/target, size, R, PHIA probability of target, key risks, invalidation, and exposure fit.
4. **Run Stats:** tool-call count estimate, what slowed/blocked the run, and 2-3 improvements for next cycle.

Guardrails: no order placement, no model-invented risk budget, no stale option structures, no held-name recycling without an add-on label, no filler ideas, and no extra research branches after the call cap is hit.
