---
name: canary
description: Use Canary through the local `canary` CLI. Use when the user asks
  about their IBKR account, positions, P&L, market quotes, option chains (incl. per-leg
  open interest), official market calendars, local watchlist, daily price history, technical/relative-strength screens, running a market scan, sizing a planned trade by
  fixed-fractional risk, checking the market's stress lifecycle (S&P 500 breadth, SPX-canonical dealer zero-gamma
  with SPY context and 0DTE / 1-7 / term horizon split, the broad-market
  regime dashboard), checking the portfolio-aware stress lifecycle, held-name market-event flags,
  reading daemon protection proposals, daemon opportunities, offline opportunity research diagnostics, or runtime settings/freeze state,
  or explicitly requests an order preview/status/history read. This skill is read/preview-first by default;
  explicit current-turn broker-write requests must use the gated CLI path and report a redacted execution artifact.
allowed-tools: Bash(canary account*) Bash(canary positions*) Bash(canary quote*)
  Bash(canary calendar*) Bash(canary watch --json*) Bash(canary watch --list*) Bash(canary watch --quotes*) Bash(canary watch --watch*) Bash(canary watch --timeout*) Bash(canary chain*) Bash(canary history*) Bash(canary scan*) Bash(canary size*)
  Bash(canary technical*) Bash(canary breadth*) Bash(canary gamma*) Bash(canary regime*)
  Bash(canary stress*) Bash(canary brief*) Bash(canary rules*) Bash(canary alerts*) Bash(canary market-events*) Bash(canary proposals status*) Bash(canary proposals list*) Bash(canary proposals refresh*) Bash(canary opportunities status*) Bash(canary opportunities list*) Bash(canary opportunities refresh*) Bash(canary backtest research-opportunity*) Bash(canary settings show*) Bash(canary policy show*) Bash(canary recon show*) Bash(canary trading status*) Bash(canary orders open*) Bash(canary orders history*) Bash(canary order status*) Bash(canary order preview*)
  Bash(canary status*) Bash(canary version*)
---

Updated: 2026-07-25 16:43 CEST

## When to use

If the user asks about holdings, cash, buying power, P&L, a local watchlist,
a specific stock or ETF quote, whether a supported market is open, an option chain, daily history, technical/relative-strength screen, or wants to scan the market, run the
relevant `canary` subcommand with `--json` and parse the output.

If the user asks about the *market environment* — "is the market risky today?",
"what's the regime?", "where's dealer gamma?", "how broad is the rally?" — reach
for `canary regime` (all eight indicator rows in one call), `canary breadth` (S&P 500
stocks-above-50DMA), or `canary gamma` (SPX/SPXW-canonical dealer gamma, with SPY
as corroborating ETF context when usable). `canary regime` is broad-market only; it emits
row bands, source health, semantic fingerprints, and a lifecycle stage rather
than portfolio advice.

If the user asks whether *their portfolio* needs attention under the current
account, positions, exposures, regime, margin, concentration, options, or data
quality state, run `canary stress --json`. The stress read answers with top-level
`action`, `market_confirmation`, `portfolio_fit`, `input_health`,
planner readiness, and evidence rows. It does not choose hedges, size trades,
preview orders, or execute.

If the user asks for the daily operator overview across market, calendar,
portfolio, risk limits, and process clocks, run `canary brief --json`. The brief
is a read surface for agents: agent-origin text renders never stamp the
morning/EOD artefact, and JSON mode never stamps for any origin.

If the user asks "what should I fix today", which checks in the compiled
discipline model are open, or wants the daily discipline checklist, run
`canary rules --json`. It returns the advisory 14-rule
[Rulebook](../../internal-docs/design/trading-rulebook.md) (per-name
exposure cap, option-line premium cap, sell-only cash floor, extrinsic
budget, expiry runway, earnings catalyst/overwrite/size-freeze checks, tape
rules, green-day nudge, hedge band, exit discipline on long-option losses,
FX exposure) ranked hardest-first, with per-name earnings context and
result-level input health. Statuses pass/info/watch/act/
unknown/not_evaluated: `unknown` means an input was missing and must never be
read as pass. Verdicts are advisory — nothing here blocks or authorizes an
order, and the compiled model is not proof that every threshold has operator
approval. Use `canary stress` for the regime × portfolio alert and `canary
proposals` for executable protective candidates.

If the user asks whether a held or requested stock/ETF has borrow, Reg SHO,
LULD, or halt context, run `canary market-events --json` with `--symbol` when
the symbol is explicit. Market-event flags are observational/protection context
and daemon safety gates, not buy-add or open-exposure recommendations.

If the user asks what protective actions Canary currently recommends — or why a
proposal is blocked — run `canary proposals status --json` (proposal-engine state)
or `canary proposals list --json` (the latest proposal snapshot with per-row
blockers); `canary proposals refresh` asks the daemon to recompute first. These
are read paths: `proposals preview|submit|ignore` are broker-adjacent gated
verbs outside this skill's allowlist.

If the user asks what mechanical opportunities Canary currently sees — especially
option exercise candidates — run `canary opportunities status --json` or
`canary opportunities list --json`; `canary opportunities refresh` asks the daemon
to recompute first. These are read paths: `opportunities preview|exercise|ignore`
are broker-adjacent gated verbs outside this skill's allowlist.

If the user explicitly asks to inspect scored opportunity research/backtest
files, use `canary backtest research-opportunity ... --json`. Treat it as an
offline diagnostic harness, not a daemon opportunity feed, live signal, alpha
proof, or broker-action surface.

If the user asks about runtime platform preferences or whether trading is
frozen, run `canary settings show --json`; `canary settings set` is a write, and the
`trading.freeze` switch is human-only.

If the user asks for recent order history or trade-review forensics, run
`canary orders history --json`. Treat it as bounded local order-journal evidence
for the current account/mode only: it is not an IBKR Activity Statement, Flex
query/export, trade confirmation, commission ledger, closed-position ledger, or
broker-grade historical audit. Use `canary orders open` for currently working
orders and `canary order status ID` for one order's full local audit trail.

If the user explicitly asks in the current turn for a stock/ETF order draft, use
`canary order preview` and explain `token_minted` separately from
`submit_eligible`; only an accepted broker WhatIf for the exact draft makes a
minted token submit-eligible. If the user explicitly asks to place, modify, or
cancel an order, use the gated CLI flow outside this skill's default allowlist:
`canary trading status --json` must be ready, the write must consume a valid
preview token when required, and the CLI's redacted JSON result is the execution
artifact. Do not submit through paired-PWA/browser automation. Do not invent or
simulate trade execution.

## Output discipline

- Always run with `--json` when parsing programmatically, then present results
  as a clean Markdown table.
- Always include the `data_type` field (`live` / `delayed` / `frozen`). If it
  isn't `live`, mention it in the answer so the user knows the prices may not
  reflect the current market.
- For decision-making quote/watchlist answers, prefer `quote_price` /
  `quote_price_source` when present, else `price` / `price_source`, and keep
  `regular_close`, `quote_quality`, `data_type`, `feed_type`, `price_as_of`,
  absolute/percent change, day range, 52-week range, volume, `avg_volume`,
  `avg_volume_20d`, and `avg_dollar_volume_20d` over raw `last` alone. For
  held stocks, keep IBKR's valuation `mark` separate from live/pre/post/overnight
  quote indications.
  If `stale` or `stale_reason` is present, say so plainly.
- If quote JSON includes `session_context`, surface it briefly. It explains
  official exchange-calendar state such as holidays, early closes, closed
  regular sessions, or the next known open.
- Never claim an order was placed unless the CLI returned a successful broker
  write result for the requested paper or live route.
- Redact account IDs, balances, holdings, order references, and preview tokens
  from shared execution artifacts unless the user explicitly needs one field.
- Never present `orders history` as official broker history or a commission/P&L
  ledger; it is local journal evidence only.
- For opportunity research output, say "diagnostic only" unless the JSON
  explicitly clears the evidence gate; even promising diagnostics are not alpha
  proof without locked walk-forward or live paper evidence.
- Never fabricate Greeks or implied volatility. If the JSON returns
  `"iv": null` and `"iv_status": "unavailable"`, say so plainly. The same
  applies to `delta`/`gamma`/`theta`/`vega` on option positions and to
  every `*_ccy` / `fx_rate` field: nil = "the gateway didn't deliver
  this", never substitute zero or a derived value.

## Commands

| Command | Purpose | Schema |
|---|---|---|
| `canary status` | Daemon + gateway health (run this first if anything fails) | [schemas.md#status](schemas.md#status) |
| `canary account` | One-account summary with field-by-field availability (NLV, BP, cash, margin, daily P&L); add `--watch` for in-place refresh | [schemas.md#account](schemas.md#account) |
| `canary positions` | Open positions for one named account, with freshness and per-position daily P&L; add `--watch` for in-place refresh | [schemas.md#positions](schemas.md#positions) |
| `canary watch` | Default decision-making monitor for the local saved-symbol watchlist; use `--list` only for the offline symbol inventory | [schemas.md#watch](schemas.md#watch) |
| `canary calendar` | Official sessions for US equities, US listed options regular sessions, and Xetra | [schemas.md#calendar](schemas.md#calendar) |
| `canary quote SYM[,SYM…]` | Snapshot quotes for one or many symbols | [schemas.md#quote](schemas.md#quote) |
| `canary quote SYM YYMMDD C\|P STRIKE` | Single-option snapshot | [schemas.md#quote](schemas.md#quote) |
| `canary quote SYM --watch` | Streaming ticks (Ctrl-C to stop) | streaming frames per [schemas.md#frame](schemas.md#frame) |
| `canary chain SYM` | List available option expiries for the underlying | [schemas.md#chain-expiries](schemas.md#chain-expiries) |
| `canary chain SYM --expiry YYYY-MM-DD` | Option chain ATM ± width for that expiry | [schemas.md#chain](schemas.md#chain) |
| `canary history SYM` | Daily OHLCV bars | [schemas.md#history](schemas.md#history) |
| `canary technical SYM[,SYM…]` | 50/200-DMA, RS vs benchmark, ATR, and 20-day liquidity | [schemas.md#technical](schemas.md#technical) |
| `canary scan <preset>` | Run a configured scanner preset | [schemas.md#scan](schemas.md#scan) |
| `canary scan list` | Enumerate configured scanner presets | [schemas.md#scan-list](schemas.md#scan-list) |
| `canary scan --type SCANCODE --exchange LOCATIONCODE` | Ad-hoc scan without writing a preset to config | [schemas.md#scan](schemas.md#scan) |
| `canary scan params [--instrument STK]` | Dump the gateway's scanCode / locationCode catalog | [schemas.md#scan-params](schemas.md#scan-params) |
| `canary size --symbol SYM --entry F --stop F` | Fixed-fractional position sizing pegged to live NLV | [schemas.md#size](schemas.md#size) |
| `canary breadth` | S&P 500 stocks-above-50DMA reading (the S5FI metric, computed locally) | [schemas.md#breadth](schemas.md#breadth) |
| `canary gamma` | SPX-canonical dealer zero-gamma estimate with SPY context when usable (heavy compute; first call per NY trading day kicks a background job) | [schemas.md#gamma](schemas.md#gamma) |
| `canary regime` | Broad-market stress lifecycle: equity vol, credit, funding, FX carry, SPX gamma with SPY context, and SPX breadth in one call | [schemas.md#regime](schemas.md#regime) |
| `canary stress` | Portfolio-aware action/readiness snapshot, source health, fingerprints, and planner readiness | [schemas.md#stress](schemas.md#stress) |
| `canary brief` | Daily operator brief in two movements (Review, Ready), rendered as daemon-composed prose with a typed row fallback; agent renders and all JSON reads never stamp | MCP twin: `canary_brief`, read-only and never stamps |
| `canary rules` | Advisory compiled 14-rule discipline model, current evidence, and state-transition history | — |
| `canary alerts` | Alert-source coverage: which expected producers claim coverage, disclosed per-rule gaps, and the aggregate snapshot state push delivery waits on; redacted by contract | — |
| `canary market-events` | Held or requested stock/ETF market-event flags: borrow inventory, extreme borrow fee, Nasdaq Reg SHO, LULD, and halt context | [schemas.md#market-events](schemas.md#market-events) |
| `canary proposals status\|list\|refresh` | Daemon-owned protection proposals, read paths only (`preview`/`submit`/`ignore` are gated verbs outside this skill allowlist) | [schemas.md#proposals-status](schemas.md#proposals-status), [schemas.md#proposals-list](schemas.md#proposals-list) |
| `canary backtest research-opportunity` | Offline scored opportunity research diagnostics; not a daemon opportunity feed or broker-action surface | — |
| `canary settings show` | Runtime platform preferences and observed read-only state, incl. `trading.freeze` | [schemas.md#settings-show](schemas.md#settings-show) |
| `canary policy show` | Risk-constitution view: effective limits with plain-English meanings (`--explain`), capital/drawdown state, overrides, cadence, sibling-policy pins. The write verbs (`capital-event`, `override`, `reset-drawdown`, `artefact`) are human-only governance acts the daemon rejects from agent origins — read `show` only from this skill | — |
| `canary recon show` | Post-trade reconciliation report: broker Flex statement flows vs the declared capital-event ledger, with exception categories, statement-source health, and the equity divergence check. `dismiss` and the reconcile sign-off are human-only governance acts — read `show` only from this skill | — |
| `canary trading status` | Local order-entry readiness: mode, pinned session evidence, `can_preview`/`can_write`, concrete blockers | [schemas.md#trading-status](schemas.md#trading-status) |
| `canary orders open` | Open-order lifecycle rows for the connected account/mode (read-only journal view) | [schemas.md#orders-open](schemas.md#orders-open) |
| `canary orders history` | Recent local order-journal history for the connected account/mode; not an IBKR Activity Statement/Flex/commission ledger | [schemas.md#orders-history](schemas.md#orders-history) |
| `canary order status ID` | One journaled order's lifecycle and audit events | [schemas.md#order-status](schemas.md#order-status) |
| `canary order preview …` | Tokenized stock/ETF/option order draft; report `token_minted` and `submit_eligible` separately, never transmits | [schemas.md#order-preview](schemas.md#order-preview) |
| `canary version` | Print version, commit, build date, binary path | — |

Add `--json` to any command for parseable output. Flags can come after positional
symbols — the CLI hoists them automatically.

Local lifecycle command: `canary restart [--force] [--timeout 15s] [--json]`
gracefully restarts or starts the background daemon and reports old/new PID plus
post-start health in JSON mode. Use it only when the user explicitly asks to
restart the local daemon or after a documented binary/config change; it is not a
broker-data command and is not exposed as an MCP tool.

### Per-command flags

- `canary status [--json]`
- `canary account [--watch [--rate 1s]] [--json]` — `--watch` re-polls on the rate (default 1s) and redraws in place on a TTY; appends snapshots separated by a dim rule when piped. `--watch` and `--json` are mutually exclusive.
- `canary positions [--symbol SYM] [--type stk|opt] [--sort alpha|pnl|value] [--quotes] [--by underlying] [--watch [--rate 1s]] [--json]`
  - `--quotes` adds quote-detail columns on stock rows: previous close, ranges, volume.
  - `--by underlying` groups stock + option legs per underlying with group P&L totals; the JSON `by_underlying` array is always populated regardless of this flag.
  - `--watch` re-polls on the rate (default 1s); same TTY/pipe behaviour as `account --watch`. Mutually exclusive with `--json`.
- `canary quote SYM[,SYM…] [--market us|de] [--exchange EXCH] [--primary EXCH] [--currency CCY] [--timeout 5s] [--json]`
- `canary quote SYM --watch [--rate 250ms] [--json]` — only one symbol at a time
- `canary watch [--quotes] [--timeout 5s] [--json]` — one-shot enriched quote monitor for the saved symbols; this is the default watch action, and `--quotes` is an explicit alias. JSON rows include current `price` with `price_source`, currency, change, previous close, day/52-week ranges, volume/average volume, 20-day average volume/dollar volume from daily bars, `price_as_of`, stale flags, session context, and compact stock holding context when the symbol is held.
- `canary watch --list [--json]` — read only the local saved-symbol inventory without market data. The CLI also has mutating `--add`, `--remove`, and `--clear` flags for the human, but agents should use read-only `--list` unless the user explicitly asks to change the local file.
- `canary watch --watch [--rate 1s] [--timeout 5s]` — re-poll the enriched quote monitor for the saved symbols; no JSON mode because it is an in-place live view.
- `canary calendar [--market us|us-options|de] [--date YYYY-MM-DD] [--next 14] [--json]` — official embedded calendars for US cash equities, US listed options regular sessions, and German Xetra cash equities. Use this for "is the market open?", "when is the next session?", "is this an early close?", and holiday/long-weekend context before risk checks. `--next` is a calendar-day horizon capped at 400. Other markets and asset classes are not modeled in v1; outside embedded coverage returns `state: "unknown"` rather than a weekday guess.
- `canary chain SYM [--no-iv] [--all-expiries] [--require-live-iv] [--min-dte N] [--max-dte N] [--target-dte N] [--json]` — list expiries for the underlying. Per-expiry ATM implied volatility is included **by default** (daemon caches results; second call within ~60 s during RTH is instant), along with `iv_source`, `iv_quality`, `dte` (calendar days to expiration), and `implied_move` / `implied_move_pct` (the 1-σ expected dollar move by expiration, computed `spot × IV × √(DTE/365)`). Top-level `spot` carries the underlying mid the daemon used. Treat `iv_quality: "reused_fallback"` plus `warning_details[].code="repeated_expiry_iv"` as a term-structure warning, not a normal clean IV surface. `--no-iv` skips the IV fetch (and implied move) when only the date list is needed. `--all-expiries` lifts the default 12-expiry cap (the nearest 12 are picked since the back-half LEAPS are rarely on the decision path). `--require-live-iv` fails fast when live IV is unavailable; DTE filters narrow expiry selection without fetching a full chain. Use this first when the user asks "what expiries are available for X?", "which expiry has the highest IV?", or "what move is the market pricing into earnings?".
- `canary chain SYM --expiry YYYY-MM-DD [--width 5] [--side calls|puts|both] [--class SPX|SPXW] [--json]` — full chain table for one expiry. Pick an expiry from the listing above when the user doesn't specify one. `--class` selects an option trading class for multi-class chains such as SPX/SPXW. Read `tradable_summary` and `liquidity_summary` before recommending options: `options_tradable:false` means no requested leg returned live two-sided bid/ask and option structures should be gated off. Use `liquidity_grade`, `atm_spread_pct`, `nearest_live_call`, `nearest_live_put`, `min_spread_live_strike`, `oi_coverage_pct`, and `recommended_structure_hint` (`stock_only`, `shares_or_spreads`, `calls_ok`, `untradable_chain`) as the trader-facing verdict before inspecting raw rows. Per-leg open interest is shown after IV in the text view (compact abbreviation — `1.2K`, `45K`, `1.2M`) and as `call_oi` / `put_oi` (int64, nullable) in JSON; empty cells / `null` mean the gateway didn't push tick 27/28 within the fill budget (common off-hours or for illiquid wings) — never zero-substituted.
  - **MCP params** (for `canary_chain`): `symbol` (required); `expiry` (`YYYY-MM-DD` — omit to list expiries); `width` (integer; ATM ± strikes, default 5); `side` (`"calls" | "puts" | "both"`); `trading_class` (SPX/SPXW trading class); `no_iv` (boolean — skip ATM IV in the expiry list); `all_expiries` (boolean — lift the 12-expiry cap); `require_live_iv` (boolean); `min_dte`, `max_dte`, and `target_dte` (integer DTE filters).
  - **CLI flag aliases**: `--class` maps to MCP `trading_class`; `--require-live-iv`, `--min-dte`, `--max-dte`, and `--target-dte` map directly.
- `canary history SYM [--days 90] [--json]` — calendar lookback; daily bars only
- `canary technical SYM[,SYM…] [--benchmark SPY] [--market us|de] [--exchange EXCH] [--primary EXCH] [--currency CCY] [--lookback-days 420] [--json]` — one-call weekly-screening primitive. Returns price, SMA50/SMA200, extension from each average, 21/63/126-trading-bar returns, 63/126-bar relative strength versus the benchmark (`symbol return - benchmark return`), ATR14/ATR%, `avg_volume_20d`, `avg_dollar_volume_20d`, `trend_state`, `data_quality`, and `missing_reasons`. JSON percent/return fields are decimal fractions (`0.10` = 10%). Use this after scanner/watchlist/manual candidate discovery to gate trend, relative strength, extension risk, and liquidity without stitching multiple history calls yourself.
- `canary scan <preset> [--limit N] [--json]` — built-in presets: `top-movers`, `top-losers`, `most-active`, `unusual-vol`, `gappers`, `high-iv-rank` (IV elevated vs. its own history), `unusual-opt-vol` (hot options flow). User-defined presets may also exist; run `canary scan list` first when unsure. **Each row carries enriched data:** `last`, `prev_close`, `change`, `change_pct`, `volume`, `iv` (underlying's averaged option IV, as a fraction — 0.234 = 23.4%), `week_52_high`, `week_52_low`, and `instrument_tags` for known ETFs/leveraged ETPs that IBKR may return from stock scans. Drop rows tagged `etf` or `leveraged_etp` when the prompt asks for non-ETF single-name ideas; missing tags mean unknown, not confirmed common stock. These are populated by per-row market-data subscriptions the daemon issues automatically (IBKR's scanner subscription itself only returns rank + symbol). Nil fields = gateway didn't deliver that tick within the enrichment window; common off-hours, and `iv` is nil for symbols without actively-traded options. Don't fabricate values — say "unavailable" when a field is nil. **Off-hours behaviour:** scans that depend on the current session (`gappers`, `top-movers`, `top-losers`, `high-iv-rank`, `unusual-opt-vol`) often time out or return cold-start errors before market open. If the user sees `scanner subsystem did not respond...`, retry once before reporting it as broken — the TWS scanner farm warms lazily and a second attempt frequently succeeds within a few seconds. `most-active` and `unusual-vol` rank against tape and tend to stay warm.
- `canary scan list [--json]`
- `canary scan --type SCANCODE --exchange LOCATIONCODE [--limit N] [--json]` — **ad-hoc scan, agent-preferred.** Use this when the user asks for a screen that doesn't match any existing preset (e.g. "show me losers on NASDAQ only", "find unusual put activity"). Avoids writing to the user's `config.toml`. Rows are capped at 50. The two magic strings (`scanCode` and `locationCode`) come from the gateway catalog — call `canary scan params` first to discover them rather than guessing. **Non-US exchanges:** each row carries `currency` (e.g. `EUR` for `STK.EU.IBIS`, `HKD` for `STK.HK`); render prices with the row's symbol, not a hardcoded `$`.
- `canary scan params [--instrument STK] [--raw] [--json]` — gateway scanner catalog. Returns three lists: `instruments` (e.g. STK, OPT, ETF.EQ.US), `locations` (e.g. STK.US.MAJOR, STK.NASDAQ, STK.HK), and `scan_types` (every `scanCode` with display name and the instrument types it's valid for). The catalog varies by gateway version and user permissions — never assume a `scanCode` exists without checking. `--instrument STK` narrows to stock scans. `--raw` adds the full XML (~200 KB–2 MB); skip unless you need a field not in the parsed result.
- `canary version [--json]` — print version, commit, build date, binary path; `--json` returns the same data as a structured object (use this when you need to verify the user is running a supported release).
- `canary breadth [--days 30] [--json]` — S&P 500 stocks-above-50DMA reading. The daemon computes the S&P DJI S5FI metric locally from 500 constituent daily closes (IBKR doesn't redistribute the index on retail subscriptions). Returns a headline `value` (0–100), a trailing daily series, and a `state` field — branch on `state`, not on `value == 0`. **Cold start (no cache yet) returns `state: "computing"` with `value: 0` and takes ~60 min** because IBKR's historical-data pacing limit caps the fan-out at ~6 names/min sustained; once the cache is built the result is instant on every subsequent call and persists across daemon restarts. Don't hammer the endpoint waiting for the cold start to finish — poll at minute-scale or fall back to telling the user "the breadth engine is still warming; check back in ~an hour." Spec note: > 55 healthy, 40–55 watch, < 40 with SPX at highs is the classic late-cycle divergence — report the raw number plus the spec band; never color-code on the wire.
- `canary gamma [--only=spy|spx] [--no-wait] [--force] [--explain] [--diagnostics] [--json] [--profiles]` — dealer-gamma market-structure snapshot for SPX/SPXW, with SPY as corroborating ETF context when usable. SPX is the canonical production signal; SPY-only is a labeled proxy/context read. The ready JSON leads with `result.quality.rankability`, `result.summary.primary_statement`, `zero_gamma_status`, `regime`, `confidence`, `not_advice`, and `summary.per_index`. In combined scope there is **no top-level combined zero-gamma price** because SPY and SPX use different price scales; read `summary.per_index.SPY` and `summary.per_index.SPX` for per-underlying spot, zero, swept range, and regime. `leg_count` means legs with non-zero OI-weighted GEX; `priced_leg_count` means legs that priced/fit IV but may not have usable OI. Missing OI is unknown, never zero: SPY OI can be absent outside regular option hours, while SPX OI should normally be session-stable, so missing SPX OI is data-quality evidence in any session. Non-fatal issues are in `warning_details` with scoped prose, not raw warning tokens. `gamma_total_abs` and `top_strikes` are sign-agnostic concentration/magnitude diagnostics. The signed zero-gamma convention is a regime hint, not advice or a trade level. Compute is heavy — the first NY-session call may return `status: "computing"` with ETA/progress; later calls return cached.
  - **MCP params** (CLI flags map to the same JSON keys when calling `canary_gamma` via MCP): `scope` (`"spy" | "spx" | "spy+spx"`; default `"spy+spx"`); `wait_ms` (integer ms, default 0); `force` (boolean; diagnostics); `include_profiles` (boolean; default false, include sweep arrays only for charting).
  - **CLI-only flags**: `--explain` (methodology + horizon breakdown), `--diagnostics` (raw source/provenance details, requires `--explain`), `--no-wait` (CLI sugar for `wait_ms: 0`), `--only` (CLI alias for `scope`), `--profiles` (include profile arrays in `--json`; default JSON strips them).
- `canary regime [--explain [--diagnostics]] [--watch [--rate 5m]] [--log PATH] [--view detail|monitor] [--json]` — single-call broad-market stress-lifecycle dashboard: eight rows across equity vol (VIX/VIX3M + VVIX), credit (HYG/SPY + official HY/IG OAS), funding stress (CP/T-bill spread), USD/JPY, SPX-canonical dealer gamma with SPY context, and SPX breadth. Text leads with `summary.label`, cluster evidence, lifecycle stage/readiness, and a punch line, then the eight-row audit table with an `AS OF` column (`live`, `15m delayed`, `close D-1`, `cached 11:42`, `unavailable`). Default JSON/MCP is compact: it keeps `fingerprint`, `lifecycle`, `source_health`, `summary`, `composite` raw + cluster counts, raw measurements, per-row `band`, `band_reason`, `thresholds`, `as_of`, `streak`, `*_quality`, `data_quality`, and `warning_details`, but omits long methodology `notes` and breadth history. Use `--explain` for the full text methodology view, `--diagnostics` with `--explain` for raw source/provenance details, or `--view monitor --json` for the compact monitor payload. `warning_details` is the agent-preferred failure path with scoped `{message, impact, action}` prose. Per-indicator rows carry `streak: {band, sessions, since}` only when the current row is rankable; unavailable/computing/error rows freeze the store internally but do not expose a stale prior-band streak. Expect these failure modes on a fresh daemon: gamma may return `status: "computing"` with `eta_seconds`; breadth can do the same during the IBKR-paced cold start; official Cboe/FRED daily rows can be temporarily unavailable when those sites are unreachable. MOVE/rates-vol is absent until a verified IBKR contract or licensed official connector exists, and must not be proxied with ETFs or futures. `--watch` re-polls every 5 minutes by default. `--log PATH` appends each fetched snapshot to a JSONL file at `<path>`.
  - **MCP params**: `view` (`"detail" | "monitor"`; default `"detail"`).
  - **CLI-only flags**: `--explain` (per-row streak/quality/methodology in the text view), `--diagnostics` (raw source/provenance details, requires `--explain`), `--watch` / `--rate` (auto-poll), `--log` (append JSONL trace).
- `canary stress [--details] [--view full|alert] [--json]` — portfolio-aware action/readiness snapshot for scheduled or manual risk checks. It consumes live account, positions, exposures, concentration, option-Greek coverage, margin, daily P&L, `canary regime`, and market-event context, then emits top-level `action`, `market_confirmation`, `portfolio_fit`, `input_health`, `direction`, `severity`, `planner_mode_hint`, `planner_readiness`, `signals[]`, `source_health[]`, `source_fingerprints`, and stable `fingerprint`. Treat `planner_readiness: "blocked"` and data-quality signals as hard gates before discussing major portfolio action. `source_fingerprints.account`, `.positions`, `.regime`, and `.market_events` are semantic-bucket hashes for monitor dedupe and orchestration provenance. `--details` expands the text evidence rows; `--view alert --json` returns the compact alert payload. This command is read-only: it does not select hedges, size trades, preview orders, or execute.
  - **MCP params**: `view` (`"full" | "alert"`; default `"full"`).
  - **CLI-only flags**: `--details`, `--json`.
- `canary rules [--all] [--symbol SYM] [--json]` — current compiled advisory
  Rulebook snapshot, including input health, policy identity, all typed rows,
  hardest-first ranking, offenders, exemptions, and earnings provenance.
  `canary rules history [--since YYYY-MM-DD|RFC3339] [--until
  YYYY-MM-DD|RFC3339] [--rule ID] [--limit N] [--json]` returns persisted
  evaluator state transitions; it is not trade-causality or broker evidence.
- `canary market-events [--symbol SYM[,SYM...]] [SYM ...] [--json]` — single-name market-event context for held or requested stock/ETF symbols. Explicit symbols evaluate those names; omitting symbols evaluates held stock/ETF underlyings from the daemon positions snapshot. JSON returns `flags[]`, `by_symbol`, `source_health[]`, `warning_details[]`, `fingerprint`, and `not_execution`. Unknown source health is unavailable evidence, not inactive. `borrow_inventory_tight` and `borrow_fee_extreme` only modify existing short buy-to-cover reductions; `halt_regulatory_or_news` and active `luld_pause` are hard blockers; `reg_sho_threshold` is regulatory context. V1 never creates buy-add/open-exposure recommendations.
  - **MCP params**: `symbol` (optional single or comma-separated symbols), `symbols` (optional array; omit both for held underlyings).
  - **CLI-only flags**: `--json`.
- `canary proposals status|list|refresh [--json]` — daemon-owned protection proposals, read paths. `status` reports the proposal-engine state, `list` returns the latest proposal snapshot (per-row blockers carry codes plus remediation text), `refresh` asks the daemon to recompute before returning. `preview`, `submit`, and `ignore` are gated verbs outside this skill's allowlist.
- `canary backtest research-opportunity --input SCORED_PIT.jsonl [--plan all|ID[,ID...]] [--max-slots N] [--bars BARS.jsonl] [--bars-manifest MANIFEST.json] [--json]` — offline/local opportunity research diagnostics. Use only when the user explicitly asks to inspect scored research files; treat evidence gates, feature diagnostics, and reason diagnostics as diagnostics, not trading instructions.
- `canary settings show [--json]` — runtime platform preferences plus observed read-only state, including the `trading.freeze` switch. `canary settings set --help` lists supported write keys; settings writes, including the freeze switch, are human-only.
- `canary trading status [--json]` — local order-entry readiness and blockers.
- `canary orders open [--json]` — current local open-order journal view for the connected account/mode.
- `canary orders history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--limit N] [--event-limit N] [--json]` — recent local order-journal history for the connected account/mode. Defaults to the last 7 days, returns up to 50 grouped orders by default, caps `--limit` at 500, and returns up to 20 lifecycle events per grouped order by default (`--event-limit`, max 200). Date-only `--until` includes the whole UTC day. This is local journal evidence only, not an IBKR Activity Statement/Flex export, trade confirmation, commission ledger, closed-position ledger, or broker-grade historical audit. When `events_truncated` is true, inspect `total_events_count` before treating the event sample as complete.
- `canary order status <order-ref|order-id|perm-id> [--json]` — one journaled order plus its local audit events.
- `canary order preview buy|sell SYMBOL QTY [--limit PRICE] [--order-type LMT|TRAIL|TRAIL-LIMIT] [--trail-percent PCT|--trail-amount AMT] [--initial-stop PRICE] [--limit-offset PRICE] [--trigger-method 1|2|3|4|7|8] [--tif DAY|GTC] [--outside-rth] [--market us|de] [--exchange EXCH] [--primary EXCH] [--currency CCY] [--replace-order ID] [--timeout 5s] [--json]` — stock/ETF preview.
- `canary order preview buy|sell SYMBOL YYYYMMDD C|P STRIKE QTY [same flags]` — single-leg option preview.
- `canary size --symbol SYM --entry F --stop F [--target F] [--risk-pct 1.0] [--side long|short] [--lot 1] [--fx 1.0] [--json]` — fixed-fractional sizing. Reads NLV from `account.summary` so `risk_pct` is pegged to the live account. `--fx` converts the base-currency risk budget into the trade's quote currency (e.g. `--fx 1.085` for a USD trade against an EUR account); default `1.0` is correct for same-currency trades. `--lot` rounds shares down (use `100` for one option contract's worth of stock). `--target` is optional: when set, the response also carries `r` (reward-to-risk multiple = `|target − entry| / |entry − stop|`; the standard "is this trade worth taking" filter, ≥ 2R typical), `reward_quote`, `reward_base`, and `breakeven_win_rate` (= `1 / (1 + R)`). Output `status` is `ok` | `tight_risk` (budget < per-share risk × lot — widen the stop or raise risk-pct) | `exceeds_buying_power`. The CLI never derives entry/stop/target from quotes — those are the user's trade plan; if the user asks "and what about the current price?" run `canary quote SYM --json` separately.

## Errors

The CLI exits with code 1 on a daemon-side error. The error line on stderr has
a code prefix when applicable:

- `daemon_unavailable` → the daemon could not start (the daemon is the
  long-running half of the same `canary` binary, autospawned on first call).
  The IB Gateway is probably not running, or the host/port pinned in config
  is wrong. Suggest `canary status` and pointing at `~/.local/state/ibkr/ibkr-daemon.log`.
- `gateway_unavailable` → connection to IB Gateway lost. Reconnects happen
  automatically; advise the user to retry in a few seconds. The CLI also prints
  a hint pointing at `canary status`.
- `symbol_inactive` → IBKR has flagged the symbol as delisted or halted. Do
  *not* substitute a similar symbol; show the error.
- `timeout` → the gateway didn't respond within the snapshot window. Suggest
  retrying with `--timeout 10s` (quote) or reducing `--days` (history).
- `bad_request` → wrong arguments or unknown preset. Show the user the usage
  hint emitted on stderr.
- `trading_disabled` → an order verb failed the daemon trading gate. Surface the
  blocker exactly; broker writes need a ready paper or live trading route and a
  submit-eligible preview token when required, and run outside this skill's
  default allowlist.

For `breadth`, `gamma`, and `regime`, the JSON carries a per-row `state` /
`status` field rather than an error code — the CLI exits 0 because the
daemon successfully returned a typed envelope. Don't treat these as errors:

- `state: "computing"` (breadth) / `status: "computing"` (gamma, regime
  rows) → a background compute is in flight. Tell the user when to check
  back (gamma: a few minutes; breadth cold start: ~an hour) and don't
  hammer the endpoint. The result will land on a subsequent call.
- `state: "ready"` (breadth) / `status: "ready"` (gamma) /
  `status: "ok"` (regime rows) → the value is real.
- `state: "cold"` / `status: "unavailable"` → the indicator can't run on
  this account or this gateway right now. Surface the row's `notes` field
  verbatim; never substitute a proxy. For regime rows, `error_message`
  carries the specific reason when set.
- `state: "degraded"` (breadth only) → the engine refused to persist
  because constituent coverage fell below the safety threshold. The
  previous good value still serves; report the degraded state honestly.

## Worked examples

### Position summary
```
$ canary positions --json
{
  "data_type": "live",
  "as_of": "2026-05-09T14:32:09Z",
	  "stocks": [
	    {"symbol": "AAPL", "sec_type": "STOCK", "multiplier": 1,
	     "quantity": 100, "avg_cost": 192.40, "mark": 207.88,
	     "market_value_ccy": 20788.0, "market_value_base": 20788.0,
	     "unrealized_pnl_ccy": 1548.0, "unrealized_pnl_base": 1548.0}
	  ],
	  "options": [
	    {"symbol": "AAPL", "sec_type": "OPTION", "multiplier": 100,
	     "right": "C", "expiry": "20260619", "strike": 215,
	     "quantity": 5, "avg_cost": 682.0, "mark": 9.40,
	     "market_value_ccy": 4700.0, "unrealized_pnl_ccy": 1290.0}
	  ],
	  "by_underlying": [
	    {"underlying": "AAPL", "stock": {...}, "options": [...],
	     "group_market_value_base": 25488.0,
	     "group_market_value_pct_nlv": 25.5,
	     "group_dollar_delta_base": 64440.0}
	  ]
	}
```

Render to the user as two compact tables (stocks, options) with money formatted
as currency and totals. Always mention the `data_type` if it is not `live`. If
the user asks "what's my exposure to AAPL?" or "how am I doing per name?",
reach for the `by_underlying` grouping.

`avg_cost` is **per-share** for stocks but **per-contract** for options
(the gateway sends it multiplier-inclusive). To compare to `mark` (which
is always per-share) divide by `multiplier`: a $6.82 premium call comes
off the wire as `avg_cost: 682.0` with `multiplier: 100`. The CLI's text
renderer does this division on the AVG COST column; if you're parsing
JSON yourself, do it too. `market_value_ccy`, `market_value_base`,
`unrealized_pnl_ccy`, and `unrealized_pnl_base` already have the multiplier
applied — don't double-multiply.

Option rows carry per-leg `delta`/`gamma`/`theta`/`vega` when the gateway
delivered a model-computation tick within budget. The `portfolio` block sums
these into share-equivalent `effective_delta`, `dollar_delta_ccy` (in
`dollar_delta_ccy_currency` — typically USD for an option book),
`dollar_delta_base`, `daily_theta_ccy`, `daily_theta_base` when every
theta-bearing leg has an FX path, `gamma`, `vega`, and tracks
`greeks_coverage` / `greeks_total` so you can flag partial coverage. When the
user asks "what's my net delta?" or "how much theta am I bleeding per day?",
read the `portfolio` block directly; for multi-currency books prefer
`portfolio.exposure_base` and per-underlying `group_*_base` fields over
cross-symbol share-equivalent totals.

For multi-currency accounts, every position's local money fields use a `_ccy`
suffix; base-normalized fields use `_base`. Non-base positions carry `fx_rate`
(base-per-CCY). `portfolio.exposure_base` is the clean exposure table sorted by
absolute base-currency market value, with `market_value_pct_nlv`,
`dollar_delta_base`, and base P&L fields already filled when FX is known.
`portfolio.fx_sensitivity_per_pct` answers "how much €P&L moves on a 1%
USD/EUR change?" — Σ (non-base NetLiq in CCY × FX × 0.01). It's exposure
× notional, not historical attribution; see SKILL note on `iv_status`
for the same nil-vs-zero discipline.

### Quote snapshot
```
$ canary quote AAPL --json
{ "symbol": "AAPL", "price": 207.86, "price_source": "last",
  "prev_close": 205.52, "change": 2.34, "change_pct": 1.14,
  "bid": 207.85, "ask": 207.88, "last": 207.86,
  "bid_size": 100, "ask_size": 200, "volume": 12400000, "avg_volume": 58900000,
  "price_as_of": "As of: May 22 at 04:01:02 PM EDT",
  "iv": null, "iv_status": "unavailable", "data_type": "live", ... }
```

Present as: `AAPL — $207.86 (+$2.34, +1.14%) · prev $205.52 · vol 12.4M / avg 58.9M · live · As of: May 22 at 04:01:02 PM EDT`.
If `data_type` is not `live`, prepend a short warning. Sizes and volume can be
`null` (omitted) when the gateway didn't deliver them. If `stale_reason` or
`session_context` is present, add a short freshness/calendar note, for example
`US equities closed: Memorial Day; next open 2026-05-26 09:30 EDT`.

### Calendar
```
$ canary calendar --market us --date 2026-05-25 --json
{
  "market": "us_equity",
  "label": "US equities",
  "timezone": "America/New_York",
  "coverage_start": "2026-01-01",
  "coverage_end": "2028-12-31",
  "session": {
    "date": "2026-05-25",
    "state": "holiday",
    "reason": "Memorial Day",
    "next_open": "2026-05-26T09:30:00-04:00"
  }
}
```

Use this for open/closed context, holidays, early closes, and supported
market-specific risk timing. For options, use `--market us-options`; for
German cash equities, use `--market de`.

### Daily history
```
$ canary history AAPL --days 30 --json
{
  "symbol": "AAPL",
  "days": 30,
  "data_type": "live",
  "bars": [
    {"date": "2026-04-09", "open": 195.20, "high": 198.40, "low": 194.10, "close": 197.65, "volume": 51234100},
    ...
  ]
}
```

The bar count typically lags the requested calendar window because non-trading
days are skipped. Daily granularity only.

### Option expiries
```
$ canary chain AAPL --json
{
  "symbol": "AAPL",
  "as_of": "2026-05-09T14:32:09Z",
  "expiries": [
    {"date": "2026-05-16"},
    {"date": "2026-05-23"},
    {"date": "2026-06-19"}
  ]
}
```

Use this when the user asks "what expiries are available for X?" or "when does the next AAPL option expire?". Render as a short bulleted list. By default each row carries `iv` (decimal, e.g. `0.284` for 28.4%) and `iv_status` (`ok`, `timeout`, `unavailable`) for the nearest 12 expiries; render IV as a percentage and mention any non-`ok` status. Pass `--all-expiries` to fetch IV for every listed date, or `--no-iv` to skip IV entirely. Empty `expiries` means the symbol has no listed options — say that rather than fabricating expiries.

### Position sizing
```
$ canary size --symbol AAPL --entry 207.50 --stop 202.50 --risk-pct 1 --json
{
  "symbol": "AAPL", "side": "long", "entry": 207.50, "stop": 202.50,
  "risk_pct": 1.0, "lot": 1, "fx": 1.0,
  "nlv": 1250000.00, "base_currency": "EUR",
  "risk_base": 12500.00, "risk_quote": 12500.00,
  "per_share_risk": 5.0,
  "shares": 2500, "notional": 518750.0, "max_loss": 12500.0,
  "status": "ok"
}
```

Render as a short summary: `Risk 1% of NLV (€12,500) on AAPL 207.50 entry / 202.50 stop → 2500 shares (notional €518,750, max loss €12,500).` Always quote the `status` field — `tight_risk` means shares=0 (suggest widening the stop or raising `--risk-pct`), `exceeds_buying_power` means notional > BP (suggest trimming `--risk-pct`). When the user's account base differs from the symbol's quote currency, ask them for the FX rate or pass `--fx` explicitly; never invent one.

### What about implied volatility?
The CLI never derives or estimates IV. If `iv_status` is `"unavailable"`, the
gateway didn't deliver tick 106 for that contract — most stock snapshots do
not include IV. Don't substitute historical vol or any proxy.
