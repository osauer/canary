# `canary` JSON schemas

Updated: 2026-08-04

This document describes the `--json` outputs agents most often read. It is
NOT complete: the typed structs in `internal/rpc` are the wire truth for
every command, and this file documents a curated subset on top of them.
Field absence semantics matter:

- `null` for a `float64` field means the gateway did not deliver that tick.
- An empty array means the user genuinely has nothing to show, not a failure.
- Numbers are always `float64` unless explicitly marked `int64`.
- Times are RFC 3339 with timezone.

Coverage boundary: commands without a section here — `rules` (+`history`),
`recon`, `policy`, `brief`, `purge`, `backtest`, `opportunities`, the
`proposals` write subcommands, `settings set`, `trading paper-smoke`,
`order place|modify|cancel`, and `update` — still emit `--json`, shaped by
their result structs in `internal/rpc/rpc.go` and neighbors. For those, run
the command with `--json` and read the actual field names; nothing in this
file overrides the daemon's typed contract.

## account

`canary account --json`

```json
{
  "account_id": "U1234567",
  "base_currency": "EUR",
  "net_liquidation": 1250000.00,
  "buying_power": 4800000.00,
  "available_funds": 1100000.00,
  "excess_liquidity": 1100000.00,
  "total_cash": 150000.00,
  "maintenance_margin": 25000.00,
  "initial_margin": 31000.00,
  "currency_exposure": [
    {
      "currency": "USD",
      "net_liquidation_ccy": 540000.00,
      "cash_ccy": 60000.00,
      "stock_market_value_ccy": 480000.00,
      "option_market_value_ccy": 0,
      "unrealized_pnl_ccy": 12000.00,
      "realized_pnl_ccy": 0,
      "exchange_rate": 0.9000,
      "net_liquidation_base": 486000.00
    }
  ],
  "data_type": "live",
  "as_of": "2026-05-09T14:32:08+02:00",
  "authority": {
    "scope": {"account_id": "U1234567", "account_mode": "live"},
    "source": "account_summary_request",
    "availability": "available",
    "freshness": "current",
    "as_of": "2026-05-09T14:32:08+02:00",
    "fields": {
      "account_type": true,
      "base_currency": true,
      "net_liquidation": true,
      "buying_power": true,
      "available_funds": true,
      "excess_liquidity": true,
      "total_cash": true,
      "maintenance_margin": true,
      "initial_margin": true,
      "gross_position_value": true,
      "unrealized_pnl": true,
      "realized_pnl": true,
      "cushion": true,
      "look_ahead_init_margin": true,
      "look_ahead_maint_margin": true,
      "look_ahead_available_funds": true,
      "look_ahead_excess_liquidity": true,
      "daily_pnl": true,
      "pnl_unrealized_total": true,
      "pnl_realized_total": true,
      "currency_exposure": true
    }
  }
}
```

Field meanings:
- `authority` — the one account and paper/live mode the result belongs to, its
  source, whether it is usable, and whether it is current. Check
  `authority.fields` before using a number: `true` means the broker supplied
  the field, so zero is a real zero; `false` means the legacy numeric field is
  only a compatibility placeholder and must be treated as unavailable.
- `net_liquidation` — total account value in `base_currency`.
- `buying_power` — funds available for new positions.
- `available_funds` — cash net of margin requirements.
- `excess_liquidity` — buffer above maintenance margin.
- `currency_exposure[]` — one row per non-base currency the gateway
  reported via `$LEDGER:ALL`. Empty / omitted on a single-currency
    account. Rows reconcile within ~0.5%:
  `net_liquidation_ccy × exchange_rate ≈ net_liquidation_base`.
  - `exchange_rate` is BASE per CCY (how many base-currency units 1
    unit of the named currency converts to — matches IBKR's `$LEDGER`
    semantics so the reconciliation works without inversion).
  - `*_ccy` fields are in the named currency; `net_liquidation_base`
    is in the account's `base_currency`. Zero fields are real zeros
    from the gateway (e.g. no options held in that currency), not
    "unavailable".
- `data_type` — one of `live`, `delayed`, `frozen`, `delayed-frozen`.

## positions

`canary positions --json`

```json
{
  "data_type": "live",
  "as_of": "2026-05-09T14:32:09Z",
  "account_id": "U1234567",
  "authority": {
    "scope": {"account_id": "U1234567", "account_mode": "live"},
    "source": "portfolio_stream",
    "availability": "available",
    "freshness": "current",
    "as_of": "2026-05-09T14:32:09Z"
  },
  "stocks": [
    {
      "symbol": "NVDA",
      "sec_type": "STOCK",
      "exchange": "NASDAQ",
      "currency": "USD",
      "quantity": 100,
      "multiplier": 1,
      "avg_cost": 400.00,
      "mark": 450.00,
      "data_type": "live",
      "prev_close": 445.00,
      "day_change": 5.00,
      "day_change_pct": 1.12,
      "day_high": 452.00,
      "day_low": 444.00,
      "week_52_high": 500.00,
      "week_52_low": 300.00,
      "volume": 40000000,
      "avg_volume": 55000000,
      "price_at": "2026-05-09T14:31:58-04:00",
      "price_as_of": "As of: May 9 at 02:31:58 PM EDT",
      "market_value_ccy": 45000.00,
      "market_value_base": 40500.00,
      "fx_rate": 0.9000,
      "unrealized_pnl_ccy": 5000.00,
      "unrealized_pnl_base": 4500.00,
      "realized_pnl_ccy": 0,
      "realized_pnl_base": 0
    }
  ],
  "options": [
    {
      "symbol": "AAPL",
      "sec_type": "OPTION",
      "currency": "USD",
      "quantity": 5,
      "multiplier": 100,
      "avg_cost": 600.0,
      "mark": 8.00,
      "market_value_ccy": 4000.00,
      "market_value_base": 3600.00,
      "unrealized_pnl_ccy": 1000.0,
      "unrealized_pnl_base": 900.00,
      "realized_pnl_ccy": 0,
      "realized_pnl_base": 0,
      "expiry": "20260619",
      "strike": 215.0,
      "right": "C",
      "delta": 0.42,
      "gamma": 0.018,
      "theta": -0.08,
      "vega": 0.42,
      "option_bid": 7.95,
      "option_ask": 8.05,
      "option_prev_close": 7.60,
      "iv": 0.284
    }
  ],
  "portfolio": {
    "effective_delta": 310.0,
    "dollar_delta_ccy": 87000.0,
    "dollar_delta_ccy_currency": "USD",
    "dollar_delta_base": 78300.00,
    "dollar_delta_base_currency": "EUR",
    "daily_theta_ccy": -40.00,
    "daily_theta_ccy_currency": "USD",
    "daily_theta_base": -36.00,
    "daily_theta_base_currency": "EUR",
    "gamma": 9.0,
    "vega": 210.0,
    "greeks_coverage": 1,
    "greeks_total": 1,
    "base_currency": "EUR",
    "net_liquidation_base": 1250000.0,
    "exposure_base": [
      {
        "underlying": "AAPL",
        "market_value_base": 3600.00,
        "market_value_pct_nlv": 0.29,
        "dollar_delta_base": 37800.00,
        "unrealized_pnl_base": 900.00,
        "base_currency": "EUR"
      }
    ],
    "fx_sensitivity_per_pct": -441.00,
    "fx_base_currency": "EUR"
  },
  "by_underlying": [
    {
      "underlying": "AAPL",
      "stock": { "...": "STOCK row, same shape as stocks[]" },
      "options": [ { "...": "OPTION row, same shape as options[]" } ],
      "group_market_value_ccy": 4000.0,
      "group_market_value_base": 3600.00,
      "group_market_value_pct_nlv": 0.29,
      "group_unrealized_pnl_ccy": 1000.0,
      "group_unrealized_pnl_base": 900.00,
      "group_dollar_delta_ccy": 42000.0,
      "group_dollar_delta_ccy_currency": "USD",
      "group_dollar_delta_base": 37800.00
    }
  ]
}
```

Read `authority` before interpreting an empty position list. Only
`availability:"available"` on one concrete account can prove that the book is
genuinely empty. A stale or unavailable result may retain rows as context, but
it cannot prove a clean or empty book.

The `stocks`, `options`, and `by_underlying` arrays are always present
(possibly empty). For options, the `symbol` is the underlying (e.g. `AAPL`),
and `expiry` / `strike` / `right` together identify the contract.

### Field meanings

- `sec_type` — wire constants from `pkg/ibkr.AssetType`: `STOCK`,
  `OPTION`, `FUTURE`, `INDEX`. Compare against the full word, not a
  three-letter short form.
- `multiplier` — 1 for stocks, 100 for standard equity options, sometimes
  higher for index/futures options. Always present (defaults to 1 when
  the gateway didn't supply one).
- `avg_cost` — **per-share** for stocks, **per-contract** (multiplier-
  inclusive) for options. To get the per-share premium on options divide
  by `multiplier`. The CLI does this automatically on the rendered AVG
  COST column; JSON output stays IBKR-faithful. `market_value_ccy`,
  `market_value_base`, `unrealized_pnl_ccy`, and `unrealized_pnl_base`
  are already multiplier-applied.
- `regular_close`, `regular_close_at`, `prior_regular_close` — stock-only
  close anchors from the same quote path as `canary quote` / `canary watch`.
  `regular_close` is the latest completed regular-session close; do not treat
  pre/post/overnight indications as the regular close.
- `quote_price`, `quote_price_source`, `quote_price_at`, `quote_price_as_of`
  — stock-only current live/pre/post/overnight indication. Use with
  `quote_quality`, `data_type`, and `feed_type` to explain freshness; keep it
  separate from the account valuation `mark`.
- `prev_close`, `day_change`, `day_change_pct` — comparison anchor and
  movement for the account valuation mark / selected price. `null` means the
  daemon has no reliable anchor.
- `day_high`, `day_low`, `week_52_high`, `week_52_low`, `volume`,
  `avg_volume`, `price_at`, `price_as_of`, `stale`, `stale_reason`,
  `quote_quality`, `feed_type`, and `data_type` — stock-only quote context
  reused from the same market-data path as `canary quote` / `canary watch`. Nil
  fields mean the gateway did not deliver that tick within the short prewarm
  window. `price_as_of` / `quote_price_as_of` are display-ready text;
  `price_at` / `quote_price_at` are typed timestamps.
- `market_value_ccy`, `unrealized_pnl_ccy`, `realized_pnl_ccy`,
  `daily_pnl_ccy` — contract-currency row values.
- `market_value_base`, `unrealized_pnl_base`, `realized_pnl_base`,
  `daily_pnl_base` — account-base conversions. Present when the account
  base currency is known and either the row is already in base currency or
  `fx_rate` is available. `fx_rate` is gateway-reported BASE/CCY and is
  omitted on same-currency rows where the conversion is implicitly 1.0.
- `delta`, `gamma`, `theta`, `vega` — option-only, populated when the
  daemon captured a model-computation tick (msg 21 tickType 13) within
  budget. `null` = unavailable (illiquid leg, OOH model abstention, busy
  subscribe slot); never zero-substituted.
- `option_bid`, `option_ask`, `option_prev_close`, `iv` — option-only,
  populated from the per-leg market-data subscription the daemon already
  opens for Greeks. `iv` is a decimal fraction (0.284 = 28.4%).
- `mark_outside_bid_ask` — option-only boolean plus a structured
  `warning_details` entry when the account valuation mark is outside the
  captured bid/ask range. Treat as data-quality risk, especially off-hours.
- `portfolio` — daemon-computed aggregate block. Present when at least
  one option leg captured Greeks OR any non-base currency exposure has a
  known FX rate. Inner fields are nil when their inputs were unavailable
  — never zero-substituted.
  - `effective_delta` — sum of per-leg signed share-equivalents (stocks
    contribute signed quantity; options contribute
    delta × signed_qty × multiplier).
  - `dollar_delta_ccy` / `dollar_delta_ccy_currency` — share-equivalents
    multiplied by each leg's contract-currency spot. Currency is "MIX"
    when contributors span currencies.
  - `dollar_delta_base` / `dollar_delta_base_currency` — same exposure
    converted to account base when every contributing leg has an FX path.
  - `daily_theta_ccy` / `daily_theta_ccy_currency` — Σ (theta × signed_qty ×
    multiplier). IBKR reports theta as daily decay, so the sum is the
    daily P&L from time decay assuming everything else holds. The
    currency follows the same single-ccy-or-"MIX" convention as
    `dollar_delta_ccy_currency`: an ISO code when every contributing option
    leg agrees, "MIX" when the book mixes currencies (in which case
    the sum is genuinely undefined — render it without a single symbol).
  - `daily_theta_base` / `daily_theta_base_currency` — theta bleed converted
    to account base when every theta-bearing leg has an FX path.
  - `exposure_base[]` — compact per-underlying exposure table sorted by
    absolute `market_value_base`. Use this for multi-currency portfolio
    maps instead of asking the model to aggregate rows manually.
  - `greeks_coverage` / `greeks_total` — count of option legs whose
    Greeks were captured / total option legs. Render partial-coverage
    explicitly to the user.
  - `fx_sensitivity_per_pct` — Σ (non-base market value in base) × 0.01;
    "how many base-currency units of P&L move per 1% FX shift". In
    `fx_base_currency`.
- `by_underlying[]` — groups stock leg (optional) + option legs by
  underlying. Always populated regardless of the `--by underlying` flag,
  which only affects the text view. `group_*_ccy` totals sum every leg in
  local/security currency; `group_*_base` fields are base-currency totals
  when all contributing rows have a conversion path.

## watch

`canary watch --list --json`

```json
{
  "name": "default",
  "symbols": ["IBM", "SPY", "AAPL"],
  "as_of": "2026-05-25T10:13:00+02:00"
}
```

Field meanings:
- `name` — always `"default"` in this release; named lists are not exposed.
- `symbols` — locally stored symbols, normalized the same way as
  `canary quote`: comma-separated input is split, whitespace is trimmed,
  and symbols are uppercased. No IBKR lookup is performed when storing.
- `as_of` — local read time; this is not an IBKR market-data timestamp.

Human CLI mutations are `canary watch SYM --add`, `canary watch SYM --remove`,
and `canary watch --clear`. `canary watch` defaults to the enriched quote
monitor; use `--list` only when you need the offline symbol inventory
without market data. MCP exposes the same read-only list and enriched quote
views, with quote rows enabled by default.

`canary watch --json` (same as `canary watch --quotes --json`)

```json
{
  "name": "default",
  "symbols": ["AAPL", "MSFT"],
  "rows": [
    {
      "symbol": "AAPL",
      "contract": {
        "symbol": "AAPL",
        "sec_type": "STK",
        "currency": "USD"
      },
      "price": 190.12,
      "price_source": "last",
      "prev_close": 188.20,
      "change": 1.92,
      "change_pct": 1.02,
      "day_high": 191.30,
      "day_low": 187.55,
      "week_52_high": 199.62,
      "week_52_low": 164.08,
      "volume": 41762007,
      "avg_volume": 58900000,
      "data_type": "delayed",
      "price_at": "2026-05-22T16:01:02-04:00",
      "price_as_of": "Delayed: May 22 at 04:01:02 PM EDT",
      "stale": true,
      "stale_reason": "price timestamp is 20m old during market hours",
      "session_context": {
        "market": "us_equity",
        "label": "US equities",
        "date": "2026-05-25",
        "timezone": "America/New_York",
        "state": "holiday",
        "is_open": false,
        "reason": "Memorial Day",
        "next_open": "2026-05-26T09:30:00-04:00"
      },
      "holding": {
        "quantity": 25,
        "avg_cost": 180.00,
        "mark": 200.00,
        "market_value_ccy": 5000.00,
        "unrealized_pnl_ccy": 500.00,
        "daily_pnl_ccy": 50.00,
        "exchange": "NASDAQ",
        "currency": "USD"
      }
    }
  ],
  "as_of": "2026-05-25T16:15:00+02:00"
}
```

`rows[]` uses the same quote fields documented below, flattened onto each
watchlist row, plus optional `holding` context for saved symbols that are
currently held as stocks. When a holding is present, its `currency` and
`exchange` are also reused for the quote request so non-USD watched
positions route like `canary positions`. Holding marks remain in the nested
`holding` block and are never used as substitute quote prices; if the quote
path fails, `price` and movement fields stay absent. A per-row `error` string
can appear when one symbol fails while the rest of the watchlist succeeds.
`canary watch --watch` renders the same enriched rows repeatedly in text mode; `--watch`
and `--json` are mutually exclusive.

## quote

`canary quote AAPL --json` (single symbol) or `canary quote AAPL,MSFT,SPY --json`
(comma-separated → array).

```json
{
  "symbol": "AAPL",
  "contract": {
    "symbol": "AAPL",
    "sec_type": "STK",
    "exchange": "SMART",
    "currency": "USD"
  },
  "price": 207.87,
  "price_source": "last",
  "regular_close": 205.52,
  "regular_close_at": "2026-05-22T16:00:00-04:00",
  "prior_regular_close": 204.10,
  "quote_price": 207.87,
  "quote_price_source": "last",
  "quote_price_at": "2026-05-22T16:01:02-04:00",
  "quote_price_as_of": "At close: May 22 at 04:01:02 PM EDT",
  "quote_quality": "firm",
  "prev_close": 205.52,
  "change": 2.35,
  "change_pct": 1.14,
  "bid": 207.86,
  "ask": 207.88,
  "last": 207.87,
  "mark": 207.87,
  "day_high": 209.10,
  "day_low": 204.80,
  "week_52_high": 237.49,
  "week_52_low": 164.08,
  "bid_size": 100,
  "ask_size": 200,
  "volume": 12400000,
  "avg_volume": 58900000,
  "avg_volume_20d": 61200000,
  "avg_dollar_volume_20d": 12721440000,
  "liquidity_status": "ok",
  "liquidity_source": "daily_bars",
  "liquidity_sample_days": 20,
  "liquidity_as_of": "2026-05-22T00:00:00Z",
  "iv": null,
  "iv_status": "unavailable",
  "data_type": "frozen",
  "feed_type": "frozen",
  "price_at": "2026-05-22T16:01:02-04:00",
  "price_as_of": "At close: May 22 at 04:01:02 PM EDT",
  "as_of": "2026-05-25T16:32:11.421+02:00",
  "session_context": {
    "market": "us_equity",
    "label": "US equities",
    "date": "2026-05-25",
    "timezone": "America/New_York",
    "state": "holiday",
    "is_open": false,
    "reason": "Memorial Day",
    "next_open": "2026-05-26T09:30:00-04:00",
    "next_close": "2026-05-26T16:00:00-04:00",
    "coverage_start": "2026-01-01",
    "coverage_end": "2028-12-31"
  }
}
```

Field meanings:
- `regular_close` — latest completed regular-session close. `prior_regular_close`
  is the close before that. `regular_change` / `regular_change_pct` compare
  those two closes when present.
- `quote_price` — current live/pre/post/overnight indication selected from
  last -> mark -> bid/ask midpoint -> bid -> ask. `quote_price_source`,
  `quote_price_at`, and `quote_price_as_of` describe that indication.
- `price` — the legacy selected display price: `quote_price` when an
  indication exists, otherwise `regular_close`. `price_source` names the input
  used. Keep the source visible when it is not a live last trade.
- `prev_close`, `change`, `change_pct` — comparison anchor and movement for
  `price`; usually `regular_close` for live/indicative quotes and
  `prior_regular_close` for close-only rows. Pointers/nulls preserve "not
  delivered" separately from an exactly flat day.
- `bid`, `ask`, `last` — `null` means not delivered. Do not substitute.
- `mark` — optional IBKR tick 37 mark/fair price. It is most useful
  off-hours or for instruments where bid/ask/last do not flow; render it
  as a fallback price, not as an actual last trade.
- `day_high`, `day_low`, `week_52_high`, `week_52_low` — range context
  from the market-data subscription. Nil means the gateway did not push
  the tick within the snapshot window.
- `bid_size`, `ask_size` — top-of-book size in shares (stocks/ETFs) or
  contracts (options). Omitted when the gateway didn't deliver tick 0/3.
- `volume` — cumulative day total. Omitted when the gateway didn't deliver
  tick 8.
- `avg_volume` — IBKR average volume tick from the Misc Stats bundle.
  Omitted when not delivered.
- `avg_volume_20d`, `avg_dollar_volume_20d` — computed from the latest
  daily bars, not from scanner output. Use these as the investability
  liquidity gate for stock screens. `avg_dollar_volume_20d` is average
  `close × volume` over the sample.
- `liquidity_status` — `ok`, `partial`, or `unavailable`. `partial`
  means fewer than 20 valid daily bars contributed; do not compare it
  as confidently as a full 20-day sample.
- `liquidity_source` — currently `daily_bars` when the computed fields
  are present.
- `liquidity_sample_days`, `liquidity_as_of` — number of daily bars used
  and the latest bar timestamp/date for the liquidity calculation.
- `iv` / `iv_status` — populated only when IBKR sends tick 106
  (Option Implied Volatility). For a stock snapshot this is almost always
  `null` / `"unavailable"` — that's an honest signal, not an error.
- `data_type` — effective data context: `live`, `delayed`, `frozen`,
  `delayed-frozen`, `prev_close`, or `closed`. `feed_type` preserves the
  gateway feed type when it differs from the effective data context.
- `quote_quality` — compact machine hint: `firm`, `indicative`, `wide`,
  `prev_close`, `stale`, or `missing`. `warning_details` carries reasons.
- `historical_close` — latest daily bar close fallback when market-data
  ticks are absent but historical bars are available.
- `price_at` / `price_as_of` — timestamp for `price` and display-ready
  freshness text. Last trades use IBKR's last-timestamp tick when present;
  closed-market last/mark prices without an exchange-side tick timestamp use
  the relevant official regular-session close rather than local observation
  time. Previous-close and historical-close fallbacks use the relevant
  official regular-session close.
- `stale` / `stale_reason` — present when the market is officially open
  but the best available price is only previous close or its timestamp is
  old enough to be misleading.
- `session_context` — optional official calendar explanation. Present
  when the snapshot is frozen/delayed/missing prices or the market is not
  in an ordinary open regular session. It names the supported market,
  current state (`regular`, `closed`, `holiday`, `early_close`, `unknown`),
  reason when known, session hours for trading days, next open/close when
  known, and embedded coverage. During ordinary live RTH with prices
  present, this field is omitted.

For the multi-symbol form, the response is a top-level JSON array of these
objects.

## calendar

`canary calendar --market us --date 2026-05-25 --next 3 --json`

Supported markets:

- `us` / `us-equity` — US cash equities.
- `us-options` — US listed options regular sessions.
- `de` / `de-xetra` — German Xetra cash equities.

Other markets and asset classes are only partly supported today. Futures,
FX, crypto, bonds, Eurex, Frankfurt floor trading, and per-class SPX/VIX
global-hours nuance are out of scope for v1.

```json
{
  "market": "us_equity",
  "label": "US equities",
  "timezone": "America/New_York",
  "as_of": "2026-05-25T11:44:00+02:00",
  "coverage_start": "2026-01-01",
  "coverage_end": "2028-12-31",
  "source": "official_exchange_calendar",
  "source_url": "https://www.nyse.com/markets/hours-calendars",
  "session": {
    "market": "us_equity",
    "label": "US equities",
    "date": "2026-05-25",
    "timezone": "America/New_York",
    "state": "holiday",
    "is_open": false,
    "reason": "Memorial Day",
    "next_open": "2026-05-26T09:30:00-04:00",
    "next_close": "2026-05-26T16:00:00-04:00",
    "source": "official_exchange_calendar",
    "source_url": "https://www.nyse.com/markets/hours-calendars",
    "coverage_start": "2026-01-01",
    "coverage_end": "2028-12-31",
    "notes": "Official NYSE/Nasdaq cash-equity holidays and early closes; other U.S. products may differ."
  },
  "sessions": [
    {
      "market": "us_equity",
      "label": "US equities",
      "date": "2026-05-25",
      "timezone": "America/New_York",
      "state": "holiday",
      "is_open": false,
      "reason": "Memorial Day",
      "next_open": "2026-05-26T09:30:00-04:00",
      "next_close": "2026-05-26T16:00:00-04:00"
    },
    {
      "market": "us_equity",
      "label": "US equities",
      "date": "2026-05-26",
      "timezone": "America/New_York",
      "state": "regular",
      "is_open": false,
      "open": "2026-05-26T09:30:00-04:00",
      "close": "2026-05-26T16:00:00-04:00"
    }
  ]
}
```

Field meanings:

- `market` — stable market id: `us_equity`, `us_options`, or `de_xetra`.
- `session` — the current/date session requested by `--date` or by now
  when omitted.
- `sessions[]` — forward calendar-day rows starting at the requested date
  or current market date. `--next` defaults to 14 and is capped at 400.
- `state` — `regular`, `closed`, `holiday`, `early_close`, or `unknown`.
  `unknown` means outside embedded official coverage; do not infer open
  from weekdays.
- `open` / `close` — present on trading days. Times are RFC 3339 in the
  market's timezone.
- `next_open` / `next_close` — present when the requested instant is closed
  and a next session is known within coverage.
- `coverage_start` / `coverage_end` — embedded official schedule coverage.
  Calendar updates arrive with binary releases in v1; there is no runtime
  network refresh.

## frame

Streaming frames emitted by `canary quote SYM --watch --json` and by the
MCP streaming-resource notification path. One JSON object per line / per
notification.

```json
{ "t": "2026-05-09T14:32:11.421Z", "bid": 207.86, "ask": 207.88, "last": 207.87,
  "bid_size": 100, "ask_size": 200, "data_type": "live" }
```

All price and size fields are optional and may be omitted between ticks.
Volume is intentionally not streamed — it is a slow, monotonically
increasing day total and clutters the tick feed.

### Terminal error frames

A frame with the `error` field populated is the **last** frame on its
subscription — the daemon will not send anything after. Price/size
fields are nil. The CLI watcher renders the error and exits cleanly;
MCP subscribers receive the error frame as a normal
`notifications/resources/updated` payload, after which the subscription
is removed daemon-side.

```json
{
  "t": "2026-05-09T14:32:14.802Z",
  "error": {
    "code": "gateway_lost",
    "message": "IB Gateway connection dropped during streaming subscription"
  }
}
```

`error.code` is one of:

- `gateway_lost` — IB Gateway connection dropped mid-stream.
- `entitlement_lost` — data-type slid below a viable level (e.g. `delayed` → no data).
- `subscription_rejected` — post-subscribe IBKR rejection (delisted, halted permanently).
- `daemon_shutdown` — daemon doing a clean exit (signal received).

Synchronous errors (symbol not found at subscribe time, gateway down at
subscribe time) ride the JSON-RPC error response on the subscribe call
itself, not the notification channel.

## MCP streaming resources

The MCP server (`canary mcp`) exposes live stock and ETF quotes via the
`canary://quote/{symbol}` URI template, discoverable through
`resources/templates/list`:

```
canary://quote/{symbol}
```

`{symbol}` is the uppercase ticker.

Example:

```
canary://quote/AAPL
```

`resources/read` returns the current snapshot ([quote](#quote) shape)
in a single text content block. `resources/subscribe` returns `{}` and
then streams coalesced [frames](#frame) via `notifications/resources/updated`
notifications, with the frame JSON embedded in `params.contents`:

```json
{
  "jsonrpc": "2.0",
  "method": "notifications/resources/updated",
  "params": {
    "uri": "canary://quote/AAPL",
    "contents": [{
      "uri": "canary://quote/AAPL",
      "mimeType": "application/json",
      "text": "{ \"t\": \"...\", \"bid\": 207.86, ... }"
    }]
  }
}
```

Unsubscribe explicitly via `resources/unsubscribe`, or close the MCP
server's stdio — the subscription drops either way and the daemon's
refcount on the IBKR market-data line decrements.

Multiple concurrent subscribers (CLI `--watch`, MCP subscribers, snapshot
polls) on the same symbol share **one** IBKR market-data line via the
daemon's fan-out manager. The line is released when the last subscriber
goes away.

## chain

`canary chain AAPL --expiry 2026-06-19 --width 5 --json`

```json
{
  "symbol": "AAPL",
  "spot": 207.88,
  "expiry": "2026-06-19",
  "dte": 41,
  "data_type": "live",
  "tradable_summary": {
    "total_legs": 22,
    "live_bid_ask_legs": 18,
    "one_sided_live_legs": 2,
    "stale_legs": 0,
    "model_only_legs": 1,
    "subscribe_error_legs": 0,
    "no_quote_legs": 1,
    "oi_covered_legs": 16,
    "oi_coverage_pct": 0.7273,
    "options_tradable": true
  },
  "liquidity_summary": {
    "liquidity_grade": "good",
    "atm_spread_pct": 0.0193,
    "nearest_live_call": {"right": "C", "strike": 210.0, "bid": 7.10, "ask": 7.25, "mid": 7.175, "spread": 0.15, "spread_pct": 0.0209, "oi": 18420},
    "nearest_live_put": {"right": "P", "strike": 205.0, "bid": 5.95, "ask": 6.10, "mid": 6.025, "spread": 0.15, "spread_pct": 0.0249, "oi": 9215},
    "min_spread_live_strike": {"right": "C", "strike": 210.0, "bid": 7.10, "ask": 7.25, "mid": 7.175, "spread": 0.15, "spread_pct": 0.0209, "oi": 18420},
    "oi_coverage_pct": 0.7273,
    "recommended_structure_hint": "calls_ok"
  },
  "as_of": "2026-05-09T14:32:11Z",
  "strikes": [
    {
      "strike": 200.0,
      "is_atm": false,
      "call_bid": 12.80, "call_ask": 13.05, "call_last": 12.90, "call_iv": null, "call_oi": 18420,
      "put_bid": 1.85, "put_ask": 1.92, "put_last": 1.88, "put_iv": null, "put_oi": 9215
    }
  ]
}
```

The `is_atm: true` row is the strike closest to spot. Greeks are populated
only when IBKR delivers them; per-leg quotes may be `null` when the option
contract cannot be resolved without conID hydration.

Top-level summaries:

- `tradable_summary.options_tradable` — `true` only when at least one
  requested option leg has live two-sided bid/ask. `false` is a hard gate
  for option structures in screening output.
- `tradable_summary.live_bid_ask_legs`, `stale_legs`, `model_only_legs`,
  `subscribe_error_legs`, `no_quote_legs` — counts across the requested
  calls/puts, explaining why the chain is or is not executable.
- `tradable_summary.oi_coverage_pct` — fraction of requested legs where
  IBKR delivered open interest. Missing OI is unknown, not zero.
- `tradable_summary.feed_gap` — best-effort weak-data classification:
  `stale_close_only`, `thin_contract`, or `unknown_feed_gap`.
- `liquidity_summary.liquidity_grade` — `good`, `fair`, `poor`, or
  `untradable`; derived from two-sided bid/ask availability and spread.
- `liquidity_summary.recommended_structure_hint` — `calls_ok`,
  `shares_or_spreads`, `stock_only`, or `untradable_chain`.
- `liquidity_summary.atm_spread_pct` and `min_spread_live_strike.spread_pct`
  are fractions (`0.10` = 10% spread / mid).

Per-leg fields:

- `call_oi`, `put_oi` — option open interest (int64), best-effort from
  tick types 27 (`callOpenInterest`) and 28 (`putOpenInterest`) on the
  same per-leg subscription that drives bid/ask/IV. `null` when the
  gateway didn't deliver the tick within the chain fill budget — common
  off-hours or for illiquid wing strikes. Never zero-substituted.

## chain-expiries

`canary chain AAPL --json` (no `--expiry` → expiry listing with ATM IV
for the nearest 12 expiries by default; daemon caches results).
`canary chain AAPL --no-iv --json` returns the fast skeleton (date list only).
`canary chain AAPL --all-expiries --json` fetches IV for every listed date.

```json
{
  "symbol": "AAPL",
  "spot": 207.42,
  "as_of": "2026-05-09T14:32:11Z",
  "expiries": [
    {"date": "2026-05-16", "dte": 7, "iv": 0.312, "iv_status": "ok", "iv_source": "live_model", "iv_quality": "live_model", "implied_move": 9.04, "implied_move_pct": 0.0436},
    {"date": "2026-05-23", "dte": 14, "iv": 0.298, "iv_status": "ok", "iv_source": "cached", "iv_quality": "cached", "implied_move": 12.21, "implied_move_pct": 0.0589},
    {"date": "2026-06-19", "dte": 41, "iv": 0.284, "iv_status": "ok", "iv_source": "live_model", "iv_quality": "live_model", "implied_move": 19.85, "implied_move_pct": 0.0957},
    {"date": "2026-07-17", "dte": 69, "iv": null, "iv_status": "timeout", "iv_source": "unavailable", "iv_quality": "unavailable"},
    {"date": "2026-12-18", "dte": 223}
  ],
  "warning_details": []
}
```

Field meanings:
- `spot` — underlying mid the daemon used to pick the per-expiry ATM
  strike and compute `implied_move`. Zero when the spot probe failed
  or `--no-iv` was passed.
- `expiries[].date` — ISO date `YYYY-MM-DD`. Sorted ascending, deduped
  across exchanges (SMART, AMEX, CBOE, …) so each expiry appears once.
- `expiries[].dte` — calendar days from today (local) to the expiry.
  Same-day expiries have `dte` = 0.
- `expiries[].iv` — decimal (e.g. `0.284` = 28.4%) or `null`. Present
  when the daemon fetched IV for that expiry (default: nearest 12;
  `--all-expiries` extends).
- `expiries[].iv_status` — `ok`, `timeout`, or `unavailable`. Set
  when IV was fetched (or attempted); absent on bare rows beyond the
  default cap. Surface non-`ok` rows clearly; do not substitute a proxy.
- `expiries[].iv_source` — `live_model`, `cached`, or `unavailable`.
  Names how the IV got onto the row; cached values come from the daemon
  TTL cache, not a fresh gateway round trip.
- `expiries[].iv_quality` — `live_model`, `cached`, `reused_fallback`,
  or `unavailable`. Surface `reused_fallback` as degraded term-structure
  data; the row may still have a numeric IV, but repeated ATM IV across
  expiries should not be treated as a clean curve.
- `expiries[].implied_move` — the 1-σ expected dollar move by
  expiration, computed `spot × IV × √(DTE/365)`. The desk-standard
  "expected move" used to size event trades and pick option strikes.
  Populated only when spot and IV are both known; `null` otherwise.
- `expiries[].implied_move_pct` — `implied_move / spot` as a fraction
  (e.g. `0.0436` means 4.36% expected move by expiry).
- `warning_details` — structured data-quality warnings. `code:
  "repeated_expiry_iv"` means the same ATM IV appeared across at least
  three expiries and those rows are marked `iv_quality: "reused_fallback"`.

Empty `expiries` means the symbol has no listed options (typical for
ETFs without an option program). Surface this honestly rather than
fabricating expiries or falling back to the existing chain command.

## history

`canary history AAPL --days 90 --json`

```json
{
  "symbol": "AAPL",
  "days": 90,
  "data_type": "live",
  "as_of": "2026-05-09T14:32:11Z",
  "bars": [
    {"date": "2026-02-09", "open": 188.30, "high": 189.95, "low": 187.10, "close": 189.40, "volume": 48230100},
    {"date": "2026-02-10", "open": 189.50, "high": 191.20, "low": 188.85, "close": 190.95, "volume": 51012400}
  ]
}
```

Field meanings:
- `days` — calendar lookback requested. The actual number of bars returned
  is typically smaller (non-trading days are skipped).
- `bars[].date` — ISO date `YYYY-MM-DD`. Bars are ordered oldest → newest.
- `volume` — daily total share/contract volume.

Daily granularity only; intraday bars are not implemented.

## technical

`canary technical AAPL,MSFT,NVDA --benchmark SPY --json`

```json
{
  "benchmark": "SPY",
  "lookback_days": 420,
  "as_of": "2026-05-26T20:12:00+02:00",
  "rows": [
    {
      "symbol": "ASTS",
      "price": 42.10,
      "price_as_of": "2026-05-22",
      "bars": 252,
      "sma_50": 35.20,
      "sma_200": 27.85,
      "pct_above_50dma": 0.196,
      "pct_above_200dma": 0.512,
      "return_21d": 0.184,
      "return_63d": 0.721,
      "return_126d": 1.842,
      "benchmark_return_63d": 0.092,
      "benchmark_return_126d": 0.144,
      "rs_63d": 0.629,
      "rs_126d": 1.698,
      "atr_14": 2.15,
      "atr_pct": 0.051,
      "avg_volume_20d": 12400000,
      "avg_dollar_volume_20d": 522040000,
      "liquidity_sample_days": 20,
      "trend_state": "extended",
      "data_quality": "ok"
    }
  ]
}
```

Field meanings:

- `benchmark` — relative-strength benchmark. Defaults to `SPY`.
- `lookback_days` — calendar-day HMDS request. Default 420 is intended
  to provide enough trading bars for SMA200 and 126-bar returns.
- `price` / `price_as_of` — latest daily close and bar date.
- `sma_50`, `sma_200` — simple moving averages over the latest 50/200
  trading bars.
- `pct_above_50dma`, `pct_above_200dma` — distance from the moving
  average as decimal fractions (`0.10` = 10% above).
- `return_21d`, `return_63d`, `return_126d` — trading-bar returns as
  decimal fractions.
- `benchmark_return_63d`, `benchmark_return_126d` — benchmark returns
  over the same bar windows.
- `rs_63d`, `rs_126d` — symbol return minus benchmark return. Positive
  means the symbol outperformed the benchmark over that window.
- `atr_14`, `atr_pct` — 14-bar average true range in price units and as
  a fraction of latest close.
- `avg_volume_20d`, `avg_dollar_volume_20d` — 20-bar liquidity gate
  computed from daily bars.
- `trend_state` — `uptrend`, `recovering`, `extended`, `broken`, or
  `insufficient_data`. `extended` means price is more than 35% above
  SMA200 in the first implementation.
- `data_quality` — `ok`, `partial`, `insufficient_data`, or `error`.
  Inspect `missing_reasons` before using partial rows in a screen.

## scan

Three invocations share this result shape — preset, ad-hoc, and list-only differ in inputs.

`canary scan top-movers --json` (preset shorthand):

```json
{
  "preset": "top-movers",
  "type": "TOP_PERC_GAIN",
  "as_of": "2026-05-09T14:32:09Z",
  "rows": [
    {
      "rank": 1,
      "symbol": "NVDA",
      "currency": "USD",
      "last": 458.02,
      "prev_close": 434.50,
      "change": 23.52,
      "change_pct": 5.41,
      "volume": 12345678,
      "iv": 0.342,
      "week_52_high": 465.10,
      "week_52_low": 290.50,
      "comment": ""
    }
  ]
}
```

`canary scan --type TOP_PERC_GAIN --exchange STK.NASDAQ --limit 25 --json` (ad-hoc): same row shape, `preset` is empty.

Row fields:

- `rank` — IBKR scanner ranking (0-indexed in the response, 1-indexed in the text renderer for readability).
- `symbol` — ticker.
- `currency` — ISO-4217 code for `last`/`prev_close`/`change`/`week_52_*`. Populated from the gateway's scannerData row (the contract currency comes back alongside symbol/exchange). Omitted by daemons older than v0.13.0; consumers should treat empty as "unknown" and fall back to `$`.
- `instrument_tags` — conservative local labels for known exchange-traded products that IBKR may still return from a stock scan, e.g. `etf`, `leveraged_etp`, `single_stock_etp`, `broad_index_etf`, `sector_etp`. When a prompt asks for non-ETF single-name ideas, drop rows tagged `etf` or `leveraged_etp` before running deeper analysis. Empty/missing means "no known local tag", not proof that the row is common stock.
- `last`, `prev_close`, `change`, `change_pct`, `volume` — populated by a follow-up market-data subscribe the daemon issues per row. IBKR's scanner subscription itself returns *only* rank + symbol (by protocol design — the leaderboard is a separate service from market data), so the daemon enriches each row in parallel. Nil fields mean the gateway didn't deliver the corresponding tick within the per-row enrichment window — common off-hours, especially for IV.
- `iv` — underlying's averaged option implied volatility (from generic tick 106). Stored as a fraction: 0.234 = 23.4%. Present only when the symbol has actively-traded options *and* the gateway delivers the tick within the window.
- `week_52_high`, `week_52_low` — 52-week price range (from generic tick 165). Used to gauge where the current price sits within the year's extremes.
- `comment` — raw scanner-side comment field. Empty for most scan types; carries the IBKR-side metric only for a few specialty scans.

`type` always echoes the underlying `scanCode` so the caller can attribute rows even without `preset`. **The scanner ranks server-side; per-row data is fetched client-side.** This is by IBKR's design — the TWS Market Scanner GUI works the same way.

## scan-list

`canary scan list --json`

```json
{
  "presets": [
    { "name": "gappers", "type": "HIGH_OPEN_GAP", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "high-iv-rank", "type": "HIGH_OPT_IMP_VOLAT_OVER_HIST", "exchange": "STK.US", "limit": 20 },
    { "name": "most-active", "type": "MOST_ACTIVE", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "top-losers", "type": "TOP_PERC_LOSE", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "top-movers", "type": "TOP_PERC_GAIN", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "unusual-opt-vol", "type": "HOT_BY_OPT_VOLUME", "exchange": "STK.US.MAJOR", "limit": 20 },
    { "name": "unusual-vol", "type": "HOT_BY_VOLUME", "exchange": "STK.US.MAJOR", "limit": 20 }
  ]
}
```

User-defined `[scans.<name>]` blocks in `config.toml` replace the defaults entirely (no merge). Always run `scan list` if unsure what's configured.

## scan-params

`canary scan params --instrument STK --json` (catalog dump; use to discover valid `scanCode` and `locationCode` strings before composing an ad-hoc scan):

```json
{
  "instruments": [
    { "name": "US Stocks", "type": "STK" },
    { "name": "US Equity ETFs", "type": "ETF.EQ.US" }
  ],
  "locations": [
    { "code": "STK.US", "display_name": "US Stocks" },
    { "code": "STK.US.MAJOR", "display_name": "Listed/NASDAQ" },
    { "code": "STK.NASDAQ", "display_name": "NASDAQ" }
  ],
  "scan_types": [
    { "code": "TOP_PERC_GAIN", "display_name": "Top % Gainers", "instruments": ["STK", "ETF"] },
    { "code": "HIGH_OPT_IMP_VOLAT_OVER_HIST", "display_name": "High Option Imp Vol Over Historical", "instruments": ["STK"] }
  ],
  "as_of": "2026-05-12T06:45:00Z"
}
```

- `instruments` — instrument-group tokens. Use `instruments[].type` as the `--instrument` filter value (e.g. `STK`, `OPT`, `ETF`).
- `locations` — every `locationCode` the gateway accepts. Pass `code` as the ad-hoc scan's `--exchange`.
- `scan_types` — every `scanCode`. Pass `code` as the ad-hoc scan's `--type`. `scan_types[].instruments` lists which instrument-types the scan applies to (filter the list to scans valid for your target).
- Add `--raw` to attach the full XML in a `raw_xml` field (only when you need a less-common field like filter values or category tags).

## status

`canary status --json`

Healthy / connected:

```json
{
  "daemon_version": "v0.4.2",
  "uptime_seconds": 14400,
  "account": "U1234567",
  "gateway_host": "127.0.0.1",
  "gateway_port": 4001,
  "gateway_tls": false,
  "negotiated_tls": false,
  "port_origin": "discovered",
  "client_id": 15,
  "connected": true,
  "data_type": "live"
}
```

Degraded (TCP-reachable but gateway never completed the handshake, or the
user closed Gateway / opened TWS on a different port):

```json
{
  "daemon_version": "v0.4.2",
  "gateway_host": "127.0.0.1",
  "gateway_port": 4001,
  "port_origin": "discovered",
  "connected": false,
  "last_error": "gateway not responding to TWS handshake within 12s"
}
```

Action-relevant fields:

- `connected` — `true` only when the TWS handshake completed. `false` =
  show the error to the user.
- `last_error` — populated when the daemon attempted a connection and it
  failed. Empty during the in-flight initial handshake (the daemon may
  still be waiting); populated once the daemon gives up.
- `port_origin` — `"pinned"` (user wrote it in config), `"discovered"`
  (probe found the listener), or `"default"`. Tells you whether `canary
  status` is showing the gateway the user *intended* (pinned) or one the
  daemon picked on its own (discovered).
- `gateway_tls` vs `negotiated_tls` — disagreement means the daemon's TLS
  fallback fired (config said plain, server demanded TLS, AUTO mode let it
  upgrade). Surface this when troubleshooting handshake errors.
- `data_type` — `live`, `delayed`, `frozen`, or `delayed-frozen`. If a
  user asks about a quote and `data_type != "live"`, mention it.

- `connected_account` / `account_mode` — the one account the gateway session is
  scoped to, and its `paper`/`live`/`unknown` classification. Never a list: a
  login holding several accounts advertises all of them, and this field then
  carries the configured pin instead, or nothing at all when no pin is set.
  When `account_mode` disagrees with what the user expects, say so before
  discussing anything order-related.
- `background_tasks` — always present, `[]` means idle; non-empty lists
  daemon computes running or awaiting retry.
- `subsystems`, `data_quality`, `data_farms`, `members` — health rows the
  CLI renders; consult them when quotes or sensors look stale.
- `trading` — always present; the local order-entry readiness surface and
  the single most action-relevant object here:
  - `mode` — `disabled`/`paper`/`live` from config; `mcp_trading` reports
    the MCP exposure state.
  - `can_preview` / `can_write` — local gate results. `can_write=false`
    with `write_blockers[]` (each `{code, message, action}`) is the list
    to show the user verbatim.
  - `blocked` / `blockers[]` — current overall trading-gate state.
  - `live_override` — `ready`/`blocked` local readiness evidence; never
    submit authority.
  - `endpoint`, `gateway_host`, `gateway_port`, `account`, `client_id`
    with their `*_origin` fields — the pins actually in force and whether
    each was pinned by config or discovered.
  - `open_orders`, `last_order_event` — local journal context.
  - `paper_smoke*` — informational paper round-trip evidence (result,
    timestamp, freshness window, account/endpoint/client/version); it
    reports release-pipeline evidence and does not gate live mode.

Additional metadata fields (`alternates`, `tls_origin`, `server_version`,
`daemon_started`) are rarely actionable; show them only when the user is
debugging discovery.

## restart

`canary restart --json`

```json
{
  "action": "restarted",
  "was_running": true,
  "started": true,
  "forced": false,
  "graceful": true,
  "old_pid": 12345,
  "new_pid": 12398,
  "old_command": "/Users/me/.local/bin/canary daemon",
  "socket_path": "/run/user/501/ibkr/ibkr.sock",
  "lock_path": "/run/user/501/ibkr/ibkr.lock",
  "health": {
    "daemon_version": "v1.5.1",
    "connected": true,
    "gateway_host": "127.0.0.1",
    "gateway_port": 4001,
    "client_id": 15
  },
  "elapsed_ms": 842
}
```

If no daemon was present, `action` is `"started"`, `was_running` is
`false`, and `old_pid` / `old_command` are omitted. If `--force` was needed,
`forced` is `true` and `graceful` is `false`.

Use JSON mode for scripts, CI, and post-install checks that need to know
whether process management succeeded independently of gateway health. The
embedded `health` object is the same shape as `status.health`, so
`health.connected: false` means the daemon process is running but the gateway
is not ready.

## size

`canary size --symbol SYM --entry F --stop F [--target F] [--risk-pct 1.0] [--side long|short] [--lot 1] [--fx 1.0] --json`

```json
{
  "symbol": "AAPL",
  "side": "long",
  "entry": 207.50,
  "stop": 202.50,
  "target": 217.50,
  "risk_pct": 1.0,
  "lot": 1,
  "fx": 1.0,
  "nlv": 1250000.00,
  "base_currency": "EUR",
  "risk_base": 12500.00,
  "risk_quote": 12500.00,
  "per_share_risk": 5.0,
  "shares": 2500,
  "notional": 518750.0,
  "max_loss": 12500.0,
  "r": 2.0,
  "reward_quote": 25000.0,
  "reward_base": 25000.0,
  "breakeven_win_rate": 0.3333,
  "status": "ok"
}
```

- `nlv` is read live from `account.summary` and is in `base_currency`.
- `risk_base = nlv * risk_pct / 100` (base currency).
- `risk_quote = risk_base * fx` (the trade's quote currency). Pass `--fx` when
  the account base differs from the symbol's quote currency; default `1.0` is
  correct for same-currency trades.
- `per_share_risk = |entry - stop|`, in quote currency.
- `shares = floor(risk_quote / per_share_risk / lot) * lot`.
- `notional` and `max_loss` are in quote currency.
- `target`, `r`, `reward_quote`, `reward_base`, `breakeven_win_rate` —
  populated **only** when `--target` is supplied. Long trades require
  `target > entry`; short trades require `target < entry`.
  - `r = |target - entry| / per_share_risk` — the reward-to-risk
    multiple. The standard discretionary threshold is `r >= 2`.
  - `reward_quote = shares * |target - entry|` (quote currency); 
    `reward_base = reward_quote / fx`.
  - `breakeven_win_rate = 1 / (1 + r)` — the strategy's break-even
    hit rate at this R. Surface as a percentage when explaining to
    a user (e.g. `r = 2.0` → 33.3% breakeven).
- `status` is one of:
  - `ok` — sized within buying power.
  - `tight_risk` — `shares == 0` because the budget can't cover one lot at the
    chosen per-share risk. Suggest widening the stop, raising `--risk-pct`, or
    lowering `--lot`.
  - `exceeds_buying_power` — `notional > buying_power * fx`. Suggest trimming
    `--risk-pct` or revisiting the entry.

The CLI never derives entry, stop, or target from market data — they're the
user's trade plan. The CLI also performs no order action; this is math
against the live account snapshot.

## breadth

`canary breadth --json` — S&P 500 stocks-above-50DMA reading. The daemon
computes the S&P DJI S5FI metric locally from constituent daily closes
(IBKR doesn't redistribute the index on retail subscriptions).

```json
{
  "state": "ready",
  "value": 62.4,
  "history": [
    {"date": "2026-04-09", "value": 58.2},
    {"date": "2026-04-10", "value": 59.6}
  ],
  "source": "Computed from S&P-500 constituent daily bars (IBKR HMDS)",
  "method": "constituent-fanout-50/200dma-hl",
  "as_of": "2026-05-09T20:35:01Z",
  "data_type": "live"
}
```

Field meanings:

- `state` — `"cold"` | `"computing"` | `"ready"` | `"degraded"`. Branch on
  this, not on `value == 0`. `cold` means the engine hasn't been kicked
  yet (rare; brief window at daemon start). `computing` means a refresh
  is in flight — `value` is `0` and `history` is empty during the
  first-ever build, which takes ~60 min due to IBKR's historical-data
  pacing limit. `ready` means the value is real. `degraded` means the
  engine refused to persist because constituent coverage dropped below
  the safety threshold (the previous good value still serves).
- `value` — percentage of S&P 500 constituents trading above their own
  50-day SMA. Range `[0, 100]`. Zero is meaningful only when `state ==
  "ready"`, which is impossible in practice — interpret `value: 0` on
  any other state as "no data yet."
- `history` — trailing daily series, oldest first. Length capped by
  `--days` (default 30, max 90). Empty during cold start.
- `source`, `method` — provenance strings the renderer can display
  verbatim. Method token: `constituent-fanout-50/200dma-hl`.
- `data_type` — gateway feed state (`live` / `delayed` / `frozen` /
  `delayed-frozen`) when the headline was captured. Omitted when no
  feed notice has arrived yet.

Spec rule of thumb (apply on the consumer side, not derived on the wire):
`> 55` healthy participation; `40–55` watch; `< 40` with SPX within 3% of
its 52-week high is the classic late-cycle divergence.

## gamma

`canary gamma --json` — dealer-gamma market-structure snapshot for SPX/SPXW,
with SPY as corroborating ETF context when usable. The result is heavy (multi-minute fan-out
across hundreds of legs); the first caller of an NY trading day kicks a
background job, subsequent callers within the session receive the cached
result instantly.

**MCP params** (`canary_gamma`):
- `scope` — `"spy" | "spx" | "spy+spx"`. Default `"spy+spx"`. SPX/SPXW is
  canonical; SPY-only is a labeled proxy/context read. CLI alias is `--only`.
- `wait_ms` — integer ms to block on an in-flight compute. Default 0.
- `force` — boolean; diagnostics-only — ignore cached result. Default false.
- `include_profiles` — boolean; default false. Include full sweep profile
  arrays only when charting.

**CLI-only flags** (no MCP equivalent — text-mode rendering controls):
- `--explain` — extra methodology, per-bucket horizon breakdown, scaling caveat. JSON unchanged.
- `--diagnostics` — with `--explain`, include raw source/provenance diagnostics.
- `--no-wait` — CLI sugar for `wait_ms: 0`.
- `--json` — switch the CLI from text to JSON output.
- `--profiles` — with `--json`, include full sweep profile arrays. Default
  JSON strips them so agents and shell tooling get compact payloads.

Computing (first call of the day):

```json
{
  "status": "computing",
  "started_at": "2026-05-09T13:30:14Z",
  "eta_seconds": 180,
  "progress": 22
}
```

Cold (closed market with unusable persisted cache):

```json
{
  "status": "cold",
  "cold_reason_code": "persisted_cache_rejected",
  "cold_reason": "persisted gamma cache for spy+spx was rejected: per_index[SPX]: zero-gamma invalid result: 890 GEX legs but zero gamma_total_abs/profile/top_strikes",
  "cold_action": "Run `canary gamma --force` for a diagnostic off-hours recompute, or call again during the next U.S. equity-options session."
}
```

Ready (combined scope, subsequent calls):

```json
{
  "status": "ready",
  "started_at": "2026-05-09T13:30:14Z",
  "result": {
    "scope": "spy+spx",
    "summary": {
      "primary_statement": "Zero-gamma: SPY $581.40; SPX none in $4615.50-$6244.50 (long-gamma). No combined zero is computed across SPY/SPX price scales.",
      "zero_gamma_status": "mixed",
      "regime": "mixed",
      "confidence": "estimate",
      "not_advice": "Market-structure context only; not a trade recommendation.",
      "per_index": {
        "SPY": {"underlying": "SPY", "spot_underlying": 583.21, "data_type": "live",
                "zero_gamma": 581.40, "zero_gamma_status": "crossing",
                "regime": "transition_gamma", "leg_count": 1208,
                "priced_leg_count": 1280},
        "SPX": {"underlying": "SPX", "spot_underlying": 5430.0, "data_type": "delayed",
                "zero_gamma_status": "none_in_window",
                "regime": "long_gamma", "leg_count": 1994,
                "priced_leg_count": 2150}
      }
    },
    "gamma_total_abs": 6.0e9,
    "gamma_total_abs_convention": "sign-agnostic",
    "regime_agreement": "disagree",
    "top_strikes": [
      {"underlying": "SPX", "trading_class": "SPXW", "strike": 5400.0,
       "expiry": "2026-06-19", "right": "C",
       "abs_gex": 7.0e9, "open_interest": 12450}
    ],
    "per_index": {
      "SPY": {"scope": "spy", "spot_underlying": 583.21, "data_type": "live", "zero_gamma": 581.40, "...": "..."},
      "SPX": {"scope": "spx", "spot_underlying": 5430.0, "data_type": "delayed", "gamma_sign": "positive", "...": "..."}
    },
    "expirations": ["2026-05-16", "2026-05-23", "2026-05-30",
                    "2026-06-06", "2026-06-13", "2026-06-19"],
    "leg_count": 3202,
    "priced_leg_count": 3430,
    "derived_iv_legs": 0,
    "leg_diagnostics": {
      "total": {"priced_legs": 3430, "oi_positive_legs": 3202,
                "gamma_positive_legs": 3430,
                "abs_gex_positive_legs": 3202},
      "by_underlying": {
        "SPY": {"priced_legs": 1280, "oi_positive_legs": 1208,
                "gamma_positive_legs": 1280,
                "abs_gex_positive_legs": 1208},
        "SPX": {"priced_legs": 2150, "oi_positive_legs": 1994,
                "gamma_positive_legs": 2150,
                "abs_gex_positive_legs": 1994}
      },
      "by_trading_class": {
        "SPY": {"priced_legs": 1280, "oi_positive_legs": 1208,
                "gamma_positive_legs": 1280,
                "abs_gex_positive_legs": 1208},
        "SPXW": {"priced_legs": 1900, "oi_positive_legs": 1750,
                 "gamma_positive_legs": 1900,
                 "abs_gex_positive_legs": 1750}
      }
    },
    "warning_details": [],
    "methodology_citations": [
      "Perfiliev (2022) — BS-sweep baseline",
      "Derman / Daglish-Hull-Suo — sticky-moneyness skew dynamics"
    ],
    "params": {"expiry_count": 6, "strike_width_pct": 0.10,
               "sweep_range_pct": 0.15, "worker_count": 4},
    "source": "computed from IBKR SPX/SPXW option chains with SPY ETF context",
    "method": "bs-gamma-profile-v3-stickymoneyness-0dte-split",
    "as_of": "2026-05-09T13:32:54Z",
    "duration_ms": 158420
  }
}
```

Field meanings:

- `status` — `"cold"` | `"computing"` | `"ready"` | `"error"`. The CLI
  blocks on the compute by default; pass `--no-wait` for the polling
  shape. `cold` means no compute has been kicked this NY trading session
  and none is in flight (first caller will kick); `computing` means a job
  is in flight (use `eta_seconds` / `progress` for the renderer);
  `ready` means `result` is populated; `error` means the last compute
  failed and `error` carries the classified reason.
- `cold_reason_code` / `cold_reason` / `cold_action` — present only on
  `status: "cold"` when the daemon knows why no value is serveable (for
  example, a persisted cache existed but failed data-quality validation).
- `result.scope` — `"spy"` | `"spx"` | `"spy+spx"`. Discriminator for
  combined vs single-underlying envelopes. SPX/SPXW is the production
  S&P 500 dealer-gamma signal; SPY corroborates when fresh/rankable.
- `result.summary` — agent-preferred readout. Start here. It tells you
  which zero-gamma crossing, if any, was identified; whether the signed
  profile stayed long-/short-gamma through the swept range; confidence;
  and the non-advisory caveat.
- In combined scope, **there is no top-level combined zero-gamma price**.
  SPY and SPX use different price scales, so consume
  `result.summary.per_index.SPY` / `.SPX` (or `result.per_index`) for
  per-underlying spot, zero-gamma, swept range, and regime.
- `result.leg_count` — legs with non-zero OI-weighted GEX. This is the
  count that matters for dealer-gamma magnitude and profile.
- `result.priced_leg_count` — legs that priced / fit IV. This can exceed
  `leg_count` when IBKR supplied IV but not open interest; those legs help
  skew fitting but do not contribute to GEX.
- `result.gamma_total_abs` — sign-agnostic magnitude signal:
  `Σ |Γ| × OI × 100 × spot² × 0.01`, summed across both indices on
  combined scope. SPX dominates ~75–80% of the sum because of the S²
  scaling. **More robust than `zero_gamma` when the dealer-sign
  assumption may invert** (covered-call ETF flow, autocall barrier
  proximity). `gamma_total_abs_convention` names the sign-handling
  ("sign-agnostic" today).
- `result.regime_agreement` — on combined scope, one of
  `"agree:long-gamma"` / `"agree:short-gamma"` /
  `"agree:transition-gamma"` / `"disagree"` / `""` (no data).
  `"disagree"` means SPY and SPX modeled regimes differ; show the
  per-index details instead of forcing a single headline.
- `result.per_index` — populated only on combined scope. Each entry
  (`"SPY"`, `"SPX"`) is a fully-formed single-underlying
  `GammaZeroComputed` so renderers can recurse for per-underlying
  detail. Profiles are stripped from default CLI JSON and MCP responses;
  pass `--profiles` / `include_profiles: true` when charting.
- `result.per_index.*.data_type` — the feed clock used for both the
  underlying spot and option-model inputs: `live`, `frozen`, or `delayed`.
  After a regular-hours IBKR 354, the daemon retries that underlying once in
  delayed mode. A delayed slice is usable only when every accepted IV came
  from IBKR's delayed model-computation tick; mixed or untyped option clocks
  are refused, and `delayed-frozen` never ranks.
- `result.top_strikes` — top-N strikes by absolute gamma notional,
  merged across both indices on combined scope (sorted by `abs_gex`
  descending; SPX rows dominate by structure). Each row carries
  `underlying` (`"SPY"`/`"SPX"`) so the renderer can label per-row.
- `result.derived_iv_legs` — legs whose IV fell back to the
  Newton-Raphson BS-inversion path because the gateway never pushed a
  model-computation tick. Compare to `priced_leg_count`.
- `result.leg_diagnostics` — leg-quality funnel for the current result:
  priced legs, legs with positive open interest, legs with positive
  Black-Scholes gamma at the snapshot spot, and legs with non-zero
  OI-weighted absolute GEX. Splits are provided by underlying and by
  trading class (`SPX` vs `SPXW`) so off-hours failures can identify
  whether pricing, OI, or gamma contribution disappeared.
- `result.warning_details` — non-fatal data-quality/methodology issues
  as scoped prose: `{code, scope, severity, message, impact, action}`.
  Do not look for raw warning tokens in JSON.
- `result.methodology_citations` — short bibliography backing the
  methodology disclosure. Surface verbatim in `--explain`.

**Scaling caveat:** SPY contributes ~1/100 of SPX dollar-gamma per
equivalent leg (S² scaling). Combined `gamma_total_abs` sums the books,
but zero-gamma levels stay per-index.

**Treat the number as a regime hint, not a precise level.** Full
methodology lives in `docs/docs/internals/regime-dashboard.md`.

## regime

`canary regime --json` — single-call broad-market stress-lifecycle dashboard:
all eight indicator rows in one compact JSON envelope. The default JSON/MCP shape
leads with `summary`, `composite`, and `warning_details`, then raw
measurements, streaks, and quality provenance. Long methodology `notes`
and breadth history are omitted by default; use `canary regime --json
--explain` when a JSON consumer explicitly needs the spec prose.

**MCP params** (`canary_regime`): none — the envelope always carries
all eight indicator rows.

**CLI flags**:
- `--explain` — show per-row streak markers, quality blocks, methodology disclosures; with `--json`, include full notes/history.
- `--watch` / `--rate` — auto-poll in place.
- `--log PATH` — append each snapshot to a JSONL trace file.

```json
{
  "as_of": "2026-05-09T14:32:09Z",
  "fingerprint": {"version": "regime-fp-v2", "key": "sha256:..."},
  "lifecycle": {
    "stage": "early_warning",
    "severity": "watch",
    "readiness": "watch",
    "timing": "forward_warning",
    "confidence": "medium",
    "evidence": [
      {"source": "vol", "signal": "cluster", "bucket": "yellow",
       "timing": "forward_warning", "severity": "watch"}
    ],
    "confirmed_by": [],
    "fingerprint": {"version": "lifecycle-fp-v1", "key": "sha256:..."},
    "not_execution": "Regime read only; no orders are placed by Canary."
  },
  "summary": {
    "label": "Normal regime",
    "evidence": "4 green clusters / 1 yellow cluster / 1 unranked cluster",
    "indicator_evidence": "6 green / 1 yellow / 1 unranked",
    "punch_line": "volatility term structure, vol-of-vol, cash credit spreads, funding spread, dealer gamma, and breadth are constructive; ETF credit proxy is mixed; FX carry proxy is unavailable.",
    "confidence": "medium",
    "not_advice": "Regime read only; not investment advice or a trade recommendation."
  },
  "source_health": [
    {"source": "vol", "status": "ok", "as_of": "2026-05-09T14:32:09Z",
     "age_seconds": 4, "confidence": "high",
     "fingerprint_stability": "semantic_buckets_only"}
  ],
  "vix_term_structure": {
    "status": "ok",
    "band": "green",
    "band_reason": "<0.92 contango",
    "thresholds": {"label": "vix_term_structure_v1", "green": "VIX/VIX3M < 0.92",
                   "yellow": "0.92 <= VIX/VIX3M < 1.00", "red": "VIX/VIX3M >= 1.00",
                   "heuristic": true, "pending_backtest": true},
    "as_of": {"label": "live", "freshness": "live", "source": "Cboe VIX and VIX3M via IBKR index market data",
              "time": "2026-05-09T14:32:09Z"},
    "vix": 14.82,
    "vix3m": 16.41,
    "ratio": 0.903,
    "data_type": "live",
    "vix_prev_close": 15.04,
    "vix_change_pct": -1.46,
    "vix_quality": {"as_of": "2026-05-09T14:32:09Z", "freshness_class": "live",
                    "confidence": "firm", "source": "VIX tick"},
    "vix3m_quality": {"as_of": "2026-05-09T14:32:09Z", "freshness_class": "frozen",
                      "confidence": "firm", "source": "VIX3M tick (thin CBOE; off-hours typically frozen)"},
    "streak": {"band": "green", "sessions": 4, "since": "2026-05-06"}
  },
  "vol_of_vol": {
    "status": "ok",
    "band": "green",
    "band_reason": "<90 vol-of-vol",
    "thresholds": {"label": "vvix_daily_v1", "green": "VVIX < 90",
                   "yellow": "90 <= VVIX < 110", "red": "VVIX >= 110",
                   "heuristic": true, "pending_backtest": true},
    "as_of": {"label": "close D-1", "freshness": "daily_close",
              "source": "Cboe official VVIX daily close", "date": "2026-05-08"},
    "symbol": "VVIX",
    "last": 82.4,
    "change_20d_pct": -3.1,
    "as_of_date": "2026-05-08",
    "source": "Cboe official VVIX daily time series",
    "value_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                      "confidence": "firm", "source": "Cboe VVIX daily close"},
    "streak": {"band": "green", "sessions": 3, "since": "2026-05-07"}
  },
  "hyg_spy_divergence": {
    "status": "ok",
    "hyg_price": 78.42,
    "hyg_50dma": 78.10,
    "spy_price": 583.21,
    "spy_52w_high": 605.78,
    "spy_prev_close": 581.94,
    "spy_change": 1.27,
    "spy_change_pct": 0.218,
    "hyg_data_type": "live",
    "hyg_quality": {"as_of": "2026-05-09T14:32:09Z", "freshness_class": "live",
                    "confidence": "firm", "source": "HYG tick (ARCA)"},
    "streak": {"band": "green", "sessions": 12, "since": "2026-04-28"}
  },
  "credit_spreads": {
    "status": "ok",
    "hy_oas": 3.62,
    "ig_oas": 1.05,
    "hy_ig_spread": 2.57,
    "hy_oas_20d_change": 0.08,
    "as_of_date": "2026-05-08",
    "source": "FRED/St. Louis Fed official ICE BofA OAS CSV",
    "hy_oas_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                       "confidence": "firm", "source": "FRED BAMLH0A0HYM2 HY OAS"},
    "spread_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                       "confidence": "firm", "source": "HY OAS minus IG OAS"},
    "streak": {"band": "green", "sessions": 4, "since": "2026-05-06"}
  },
  "funding_stress": {
    "status": "ok",
    "cp_3m_rate": 5.34,
    "tbill_3m_rate": 5.20,
    "spread_bps": 14.0,
    "as_of_date": "2026-05-08",
    "source": "FRED/St. Louis Fed official Federal Reserve CP and T-bill series",
    "spread_quality": {"as_of": "2026-05-08T00:00:00Z", "freshness_class": "derived",
                       "confidence": "firm", "source": "90-day AA financial CP minus 3-month T-bill"},
    "streak": {"band": "green", "sessions": 8, "since": "2026-04-30"}
  },
  "usd_jpy": {
    "status": "unavailable",
    "symbol": "USD.JPY",
    "error_message": "USD.JPY: gateway delivered no FX tick (check IDEALPRO entitlement)"
  },
  "gamma_zero": {
    "status": "ok",
    "envelope": {"status": "ready", "result": {"...": "see gamma schema"}},
    "zero_gamma_quality": {"as_of": "2026-05-09T13:32:54Z", "freshness_class": "modelled",
                           "confidence": "proxy",
                           "source": "bs-gamma-profile-v3-stickymoneyness-0dte-split"},
    "streak": {"band": "green", "sessions": 7, "since": "2026-04-30"}
  },
  "breadth": {
    "status": "ok",
    "envelope": {"state": "ready", "pct_above_50dma": 62.4, "...": "see breadth schema"},
    "pct_above_50dma": 62.4,
    "pct_above_200dma": 71.0,
    "value_quality": {"as_of": "2026-05-09T13:00:00Z", "freshness_class": "derived",
                      "confidence": "estimate", "source": "constituent-fanout-50/200dma-hl"},
    "streak": {"band": "green", "sessions": 31, "since": "2026-04-08"}
  },
  "composite": {
    "verdict": "Normal regime",
    "green_count": 6,
    "yellow_count": 1,
    "red_count": 0,
    "ranked_count": 7,
    "unranked_count": 1,
    "cluster_green_count": 4,
    "cluster_yellow_count": 1,
    "cluster_red_count": 0,
    "cluster_ranked_count": 5,
    "cluster_unranked_count": 1
  },
  "warning_details": [],
  "spec_doc": "https://osauer.dev/canary/docs/internals/regime-dashboard.html"
}
```

Field meanings:

- `summary` is the agent-preferred readout. Start here: `label` is the
  non-advisory regime label, `evidence` is the cluster traffic-light
  balance, `indicator_evidence` is the raw row balance, `punch_line`
  explains the current read in one sentence, and `confidence` reflects
  evidence coverage rather than forecast certainty.
- `lifecycle` is the orchestration-facing broad-market state. `stage` is
  one of `quiet`, `early_warning`, `confirmed_stress`, `panic`,
  `stabilization`, `opportunity`, or `data_quality`; `timing` separates
  forward warning from contemporaneous confirmation or recovery; `evidence`
  keeps weak or unconfirmed red inputs visible; `confirmed_by` lists the
  independent confirming sources. `not_execution` is always a read-only
  boundary.
- `source_health[]` reports each broad-market source cluster's `as_of`,
  status (`ok`, `stale`, `partial`, `degraded`, `computing`,
  `unavailable`, or `error`), confidence, optional age/max-age values, and
  `fingerprint_stability: "semantic_buckets_only"`. A row may also carry
  durable `refresh_state`, `next_attempt`, and a redacted typed `last_failure`
  (`code`, `stage`, `failed_at`, `retryable`); use those fields instead of
  parsing `notes` for the failure class.
- `fingerprint` and `lifecycle.fingerprint` are semantic hashes for monitor
  dedupe; do not recompute them from raw JSON.
- Each indicator row carries a `status` field:
  `"ok"` (real fresh measurement) | `"stale"` (gateway labeled it
  delayed/frozen) | `"computing"` (heavy compute in flight; poll
  again) | `"unavailable"` (feed not entitled on this account; see
  `warning_details`) | `"error"` (`error_message` carries the reason).
- Each indicator row also carries compact agent metadata:
  `band` (`green` / `yellow` / `red` / `unranked`), `band_reason`,
  `thresholds` (`label`, per-band text, `heuristic`, `pending_backtest`),
  and `as_of` (`label`, `freshness`, `source`, optional `time` / `date`,
  optional `age_seconds`). Use `as_of.label` for the table freshness badge.
- Numerical fields are pointer-typed: `null` = "didn't arrive in the
  fetch budget," never zero-substituted.
- `fields_missing` lists advisory sub-fields (e.g. `spy_52w_high`,
  `hyg_50dma`, `series_date_mismatch`) that didn't land or aligned
  imperfectly — the row's primary measurement still landed, so dim those
  sub-cells without re-classifying the whole row as `error`.
- `warning_details` carries scoped `{message, impact, action}` prose for
  stale, computing, unavailable, and error rows. Prefer this over parsing
  row-level error strings.
- `notes` are omitted from default JSON/MCP for compactness. Use
  `--json --explain` or CLI `--explain` for full methodology prose.
- `composite` is the daemon-side rollup matching what the CLI prints
  above its indicator table. Raw row counts (`green_count`,
  `yellow_count`, `red_count`, `ranked_count`, `unranked_count`) sit
  beside cluster counts (`cluster_*`) so equity-vol or credit sub-signals
  do not double-count as independent macro confirmations. `verdict` is one of
  "Normal regime", "Elevated stress watch", "Stress signal present",
  "Broad stress regime", "Full risk-off conditions", "Insufficient
  signal — too few indicators ranked", "No ranked indicators — see
  rows below for state". Renderers showing their own band coloring
  can ignore this and re-tally from per-row `status` + measurements.
- Each row's `streak: {band, sessions, since}` counts consecutive NY
  trading sessions in the current band. Nil on computing / unavailable /
  error rows (streak freezes rather than resets). The CLI shows
  this inline ("yellow · day 3"); MCP consumers can render the same.
- Each row's `*_quality` objects (`vix_quality`, `hyg_quality`,
  `last_quality`, `zero_gamma_quality`, `value_quality`, etc.) carry
  per-scalar provenance: `freshness_class` (`live` / `frozen` /
  `derived` / `modelled`), `confidence` (`firm` / `estimate` / `proxy`),
  `as_of`, and a `source` description (e.g. `"VIX tick"`, `"SPY 252d
  max(High) fallback"`, `"perfiliev-bs-sweep-v1"`). The CLI's
  `--explain` view consumes these directly.
- `gamma_zero.envelope` and `breadth.envelope` carry the full
  [gamma](#gamma) / [breadth](#breadth) result shapes; consumers that
  already know those schemas can re-use the same renderers.
- `spec_doc` always points at the canonical methodology reference.
  Surface as a deep link when explaining a band edge to the user.

Use `canary regime --explain` to get the spec's per-indicator threshold
prose printed alongside each row, or `canary regime --json --explain` for
the full JSON methodology payload.

## market-events

`canary market-events [--symbol SYM[,SYM...]] [SYM ...] --json` — observed
single-name market-event context for held or requested stock/ETF symbols.
Omitting symbols evaluates held stock/ETF underlyings from the daemon positions
snapshot.

**MCP params** (`canary_market_events`): optional `symbol` string or `symbols`
array. Omit both to use held underlyings.

```json
{
  "kind": "ibkr.market_events",
  "schema_version": "market-events-v1",
  "as_of": "2026-06-07T06:48:00Z",
  "symbols": ["GME"],
  "flags": [
    {
      "id": "borrow_fee_extreme",
      "symbol": "GME",
      "label": "Fee extreme",
      "status": "active",
      "severity": "act",
      "role": "proposal_modifier",
      "source": "IBKR short stock availability",
      "source_url": "ftp://ftp3.interactivebrokers.com/usa.txt",
      "as_of": "2026-06-07T13:45:03Z",
      "observed_at": "2026-06-07T06:48:00Z",
      "value": 75.25,
      "unit": "pct_annualized",
      "details": ["fee_rate=75.2500%", "available=1500", "USD"]
    }
  ],
  "by_symbol": {"GME": [{"id": "borrow_fee_extreme"}]},
  "source_health": [
    {"source": "borrow_inventory", "status": "ok", "confidence": "medium",
     "fingerprint_stability": "semantic_buckets_only"},
    {"source": "borrow_fee", "status": "ok", "confidence": "medium",
     "fingerprint_stability": "semantic_buckets_only"},
    {"source": "reg_sho_threshold", "status": "ok", "confidence": "high",
     "notes": ["Nasdaq-listed threshold securities source; non-Nasdaq listing-exchange threshold feeds remain outside V1."],
     "fingerprint_stability": "semantic_buckets_only"}
  ],
  "fingerprint": {"version": "market-events-fp-v2", "key": "sha256:..."},
  "not_execution": "Market-event flags are observed context and daemon safety gates; no orders are placed by Canary."
}
```

Field meanings:

- `flags[].id` is one of `borrow_inventory_tight`, `borrow_fee_extreme`,
  `reg_sho_threshold`, `luld_pause`, or `halt_regulatory_or_news`.
- `flags[].status` describes the event (`active`, `recent`). Source freshness
  is separate in `source_health[]` as `ok`, `stale`, `unknown`, or `degraded`.
- `borrow_fee_extreme` requires observed IBKR fee-rate evidence at or above 50%
  annualized. Do not infer it from low shortable-share inventory.
- `by_symbol` is a render/index convenience over `flags[]`; do not treat a
  missing key as proof that all sources were healthy.
- Unknown/null source data is unavailable, not false or zero. Check
  `source_health[]` before relying on absence of a flag.
- `source_health[]` may carry durable `refresh_state`, `next_attempt`, and a
  redacted typed `last_failure` (`code`, `stage`, `failed_at`, `retryable`).
  Respect backoff/not-due state; do not parse `notes` for failure semantics.
- Flags are reduce-only context. Borrow flags can modify existing short
  buy-to-cover reductions; active halt/LULD blocks preview/submit;
  `reg_sho_threshold` is regulatory context. V1 never creates buy-add or
  open-exposure recommendations.

## stress

`canary stress --json` — live stateless portfolio stress read for monitor loops. It
combines broad-market weather from `canary regime` with the current portfolio
shape, then emits action/readiness/evidence only. It never selects trades,
sizes hedges, previews orders, or executes.

**MCP params** (`canary_stress`): none — the tool fetches account, positions, and
regime itself.

```json
{
  "as_of": "2026-05-31T21:11:20+02:00",
  "source_as_of": {
    "account": "2026-05-31T19:11:04Z",
    "positions": "2026-05-31T21:11:04+02:00",
    "regime": "2026-05-31T21:11:20+02:00"
  },
  "fingerprint": {"version": "stress-fp-v2", "key": "sha256:..."},
  "source_fingerprints": {
    "account": {"version": "account-fp-v1", "key": "sha256:..."},
    "positions": {"version": "positions-fp-v1", "key": "sha256:..."},
    "regime": {"version": "regime-fp-v2", "key": "sha256:..."},
    "market_events": {"version": "market-events-fp-v2", "key": "sha256:..."}
  },
  "source_health": [
    {"source": "account", "status": "ok", "age_seconds": 15,
     "max_age_seconds": 5400, "confidence": "high",
     "fingerprint_stability": "semantic_buckets_only"},
    {"source": "positions", "status": "ok", "age_seconds": 15,
     "max_age_seconds": 5400, "confidence": "high",
     "fingerprint_stability": "semantic_buckets_only"},
    {"source": "regime", "status": "stale", "confidence": "medium-low",
     "notes": ["stale clusters: breadth,credit,vol"],
     "fingerprint_stability": "semantic_buckets_only"}
  ],
  "policy": "active-v1",
  "action": "confirm_inputs",
  "market_confirmation": "blocked",
  "portfolio_fit": "high",
  "input_health": "degraded",
  "direction": "defensive",
  "severity": "watch",
  "planner_mode_hint": "confirm_data",
  "planner_readiness": "blocked",
  "summary": "Refresh or confirm degraded inputs before planning major portfolio changes.",
  "primary_drivers": ["net_delta_high", "regime_stress_confirmed"],
  "signals": [
    {"id": "net_delta_high", "direction": "defensive",
     "severity": "urgent", "metric": "net_delta_pct_nlv", "observed": 250.4,
     "threshold": 125, "unit": "pct_nlv", "evidence": "net_delta_pct_nlv 250% NLV",
     "confidence": "high"}
  ],
  "rows": [
    {"title": "Portfolio stress", "direction": "defensive", "severity": "urgent",
     "guidance": "Refresh or confirm degraded inputs before planning major portfolio changes.",
     "evidence": "2 red clusters; net delta 250% NLV"}
  ],
  "portfolio": {"base_currency": "EUR", "net_liquidation": 1250000,
                "net_delta_pct_nlv": 250.4, "option_greeks": "8/8 legs"},
  "market": {"regime_verdict": "Stress signal present",
             "regime_posture": {"label": "Stress signal present",
                                "tone": "watch",
                                "stage": "early_warning"},
             "red_clusters": 1, "red_cluster_names": ["gamma"]},
  "warnings": ["stale clusters: breadth, credit, and vol"],
  "not_execution": "Read-only stress snapshot; no orders are placed by Canary."
}
```

Field meanings:

- `action` is one of `stand_down`, `watch`, `defend`, `rebalance`, `deploy`,
  or `confirm_inputs`. Do not infer a stronger action from portfolio-only
  signals when the top-level action says otherwise.
- `market_confirmation` is `none`, `partial`, `confirmed`, or `blocked`;
  `portfolio_fit` is `low`, `medium`, `high`, or `unknown`; `input_health` is
  `ok`, `warming`, `degraded`, or `failed`. These fields explain why the
  action is actionable, staged, or data-blocked.
- `planner_mode_hint` and `planner_readiness` are posture/readiness evidence.
  They are intentionally not orders or trade recommendations.
- `signals[]` is the canonical machine evidence. Use `id`, `direction`,
  `severity`, `observed`, `threshold`, `target`, `confidence`, and
  `blocked_by`; do not parse `rows[].guidance` for automation.
- `source_fingerprints` identifies the semantic account, positions, regime, and
  market-event buckets consumed by the stress run. `fingerprint` identifies
  the whole alert posture.
- `source_health[]` is the freshness/readiness surface for orchestration. During
  regular trading/pre-market, sources are expected to be fresh on a roughly
  five-minute cadence; outside trading, the max-age window is wider. Always
  respect `status`, `confidence`, and `planner_readiness`. Durable
  `refresh_state`/`next_attempt` and redacted typed `last_failure` fields
  describe retry/backoff and failure class without requiring prose parsing.
- `market_indicators[]` lists each regime indicator with `green`, `amber`,
  `red`, `context`, or `n/a` status; context-only gamma is awareness evidence,
  not degraded input health.
- `portfolio` and `market` are compact context blocks for rendering and quick
  diagnostics. Use `canary account`, `canary positions`, and `canary regime` for
  full underlying evidence.
- `market.regime_posture` is the canonical market-regime display/policy read.
  Render its `label`/`tone`; do not derive risk-off from raw red-cluster counts.
- `not_execution` is part of the contract. The stress read does not place, preview,
  submit, modify, cancel, draft, size, or select orders.

## trading-status

`canary trading status --json` — local order-entry readiness. This surface
includes connected-gateway readiness and is deliberately separate from broker
permission: TWS / IB Gateway can still reject writes after every local gate
passes. The MCP twin is `canary_trading_status`.

```json
{
  "mode": "paper",
  "endpoint": "127.0.0.1:7497",
  "gateway_host": "127.0.0.1",
  "gateway_port": 7497,
  "port_origin": "pinned",
  "account": "DU1234567",
  "account_origin": "pinned",
  "client_id": 15,
  "client_id_origin": "pinned",
  "mcp_trading": "disabled",
  "can_preview": true,
  "can_write": true,
  "open_orders": 2,
  "last_order_event": "status-updated BUY 25 AAPL at 2026-06-11T15:02:11Z",
  "live_override": "blocked",
  "blocked": false
}
```

Field meanings:

- `mode` — `disabled` | `paper` | `live`, from `[trading].mode`. On
  `disabled` only the identity fields are populated; no gate evaluation
  runs and `blockers` stays empty.
- `endpoint` / `gateway_host` / `gateway_port` / `port_origin` — the
  broker route writes would use. `port_origin` is `pinned` | `discovered`
  | `default`; order entry requires pinned values.
- `account` / `account_origin` — pinned account evidence;
  `account_origin` is `pinned` or `auto`. `client_id` /
  `client_id_origin` (`pinned` or `default`) — the API client ID the
  daemon connects with.
- `mcp_trading` — currently `disabled`; MCP broker-write controls are
  not exposed.
- `can_preview` — order entry is enabled, the gateway is connected, and no
  blockers are active; gates `order preview` / `canary_order_preview`.
- `can_write` — true only when the full broker-write authorization
  passes (connected gateway, trading-capable build, writable order journal,
  not frozen, no blockers). When `can_write` is false while `blocked` is false,
  `write_blockers[]` carries the write-only reasons — e.g.
  `order_writes_unavailable` (read-only build) or `trading_frozen` (the
  runtime brake; cancels stay allowed while frozen).
- `blocked` — true when `blockers` is non-empty. The CLI text view exits
  1 on blocked; the JSON path exits 0 either way, so branch on this
  field, not the exit code.
- `blockers[]` — `{code, message, action}` rows naming each unmet local
  gate. Codes include `gateway_port_unpinned`,
  `gateway_account_unpinned`, `gateway_account_not_concrete` (the
  aggregate account "All" is never writable), `gateway_account_mismatch`,
  `gateway_client_id_unpinned`, `gateway_client_id_mismatch`,
  `gateway_unavailable`, `paper_endpoint_unconfirmed`, `live_endpoint_unconfirmed`,
  `order_journal_unavailable`, and `invalid_mode`. Always render
  `action` — under stress it is the difference between a dead end and
  the next command to run.
- `open_orders` (integer) / `last_order_event` — journal-derived count
  of open orders in the current broker account/mode scope and the most
  recent journal event line. Omitted when the journal is unreadable.
- `paper_smoke`, `paper_smoke_at`, `paper_smoke_max_age`,
  `paper_smoke_account`, `paper_smoke_endpoint`,
  `paper_smoke_client_id`, `paper_smoke_version` — live mode only:
  the latest paper round-trip evidence (`paper_smoke_at` is RFC 3339).
  Informational since 2026-06-10 — the smoke is enforced as a binding
  release gate, not a runtime live blocker.
- `live_override` — `blocked` | `ready`. `ready` only on live mode with
  zero blockers (the simplified pins+mode gate; there is no ack or
  typed-confirmation field). Agent-origin paper and live writes use the
  same broker-write gate; origin remains journaled audit metadata.

## orders-open

`canary orders open --json` (bare `canary orders` is the same) — locally
tracked open orders, reduced from the daemon's append-only order journal.
Broker callbacks remain authoritative for acknowledgement/fill/cancel
state; the daemon never requests a broker open-order snapshot.

```json
{
  "orders": [
    {
      "order_ref": "ibkr-20260611-150210",
      "preview_token_id": "tok-7f3a2c91",
      "reserved_order_id": 1842,
      "client_id": 15,
      "perm_id": 902117344,
      "account": "DU1234567",
      "endpoint": "127.0.0.1:7497",
      "mode": "paper",
      "symbol": "AAPL",
      "sec_type": "STK",
      "con_id": 265598,
      "exchange": "SMART",
      "currency": "USD",
      "action": "BUY",
      "order_type": "LMT",
      "tif": "DAY",
      "outside_rth": false,
      "quantity": 25,
      "limit_price": 207.85,
      "status": "Submitted",
      "lifecycle_status": "submitted",
      "filled": 0,
      "remaining": 25,
      "send_state": "broker_acknowledged",
      "last_event": "broker-acknowledged",
      "updated_at": "2026-06-11T15:02:11Z",
      "open": true,
      "modify_eligible": true,
      "cancel_eligible": true
    }
  ],
  "as_of": "2026-06-11T15:04:02Z",
  "account": "DU1234567",
  "mode": "paper",
  "last_local_event_at": "2026-06-11T15:02:11Z",
  "not_broker_statement": "local order journal only; not an IBKR Activity Statement, trade confirmation, execution report, or historical broker audit",
  "limitations": [
    "Reduced from the daemon's local append-only order journal for the current broker account/mode only.",
    "May miss manual orders, other-client orders, broker activity while the daemon was offline, and rows outside the selected account/mode scope.",
    "Broker callbacks remain authoritative when journaled; absence of a local event is not proof that no broker activity occurred."
  ]
}
```

Field meanings:

- `orders` is always present (possibly empty), sorted newest
  `updated_at` first, and scoped to the **currently connected broker
  account/mode**: paper/test journal rows are intentionally not returned
  while connected live and vice versa. This is not a historical audit
  across old accounts or modes.
- `account`, `mode`, `last_local_event_at`, `not_broker_statement`, and
  `limitations[]` disclose the local-journal scope and freshness. Treat
  these rows as daemon evidence, not a broker statement or complete
  historical audit.
- Identity trio — `order_ref` (local ref), `reserved_order_id` (IBKR API
  order ID, integer), `perm_id` (IBKR permanent ID, integer). Any of the
  three is a valid `canary order status` lookup id. `preview_token_id` is
  the redacted token identifier; the raw submit-capable token never
  appears on read surfaces.
- Contract fields (`symbol`, `sec_type`, `con_id`, `exchange`,
  `primary_exch`, `currency`, `local_symbol`, `trading_class`, `expiry`,
  `strike`, `right`, `multiplier`) are populated as journaled; option
  rows carry `expiry`/`strike`/`right`. Most are `omitempty` — absence
  means "not journaled for this order", not zero.
- `action` is `BUY` | `SELL`; `order_type` is `LMT` | `TRAIL` |
  `TRAIL LIMIT`; `tif` is `DAY` | `GTC` (GTC is accepted for broker
  trails only). `trail` has the same shape as the order-preview
  `draft.trail` block.
- `status` — the raw broker status string (e.g. `Submitted`,
  `PreSubmitted`). `lifecycle_status` — always present, the daemon's
  normalized state: `previewed`, `pending_submit`, `pre_submitted`,
  `submitted`, `partially_filled`, `filled`, `pending_cancel`,
  `cancelled`, `rejected`, `inactive`, `unknown_reconcile_required`, or
  `expired_inferred`. `orders open` returns only rows still considered
  open, so terminal states show up via `order status`, not here.
- `send_state` — write-path progress: `reserved`, `send_attempted`,
  `broker_acknowledged`, `uncertain_send` (a send may or may not have
  reached the broker — reconcile before retrying), or `terminal`.
- `filled`, `remaining`, `avg_fill_price`, `last_fill_price` —
  broker-callback fill state. `why_held` and `mkt_cap_price` are IBKR
  hold/price-cap disclosures, present only when the broker sent them.
- `open`, `modify_eligible`, `cancel_eligible` — always present.
  Modify is restricted to broker-acknowledged stock rows: DAY LMT, or
  DAY/GTC TRAIL / TRAIL LIMIT (protective trails are amended in place so
  a re-price never opens an unprotected cancel/replace window). Cancel
  requires a broker-acknowledged order ID and not `pending_cancel`.
- `purge_id`, `leg_id`, `source`, `bypass_preview` — provenance for
  purge/proposal-originated orders.

## orders-history

`canary orders history --json` — bounded local order-journal history for
the current broker account/mode. Optional filters:
`--since YYYY-MM-DD|RFC3339`, `--until YYYY-MM-DD|RFC3339`, and
`--limit N` (default 50, max 500). `--event-limit N` caps returned
lifecycle events per grouped order (default 20, max 200). Without explicit
dates the daemon returns the last 7 days. Date-only values are interpreted
as UTC days; date-only `--until` includes that full day.

This is local daemon journal evidence only. It is not an IBKR Activity
Statement, Flex query/export, trade confirmation, execution report,
commission ledger, closed-position ledger, or broker-grade historical
audit. It may miss manual orders, other-client orders, broker activity
while the daemon was offline, and rows outside the selected account/mode.

```json
{
  "orders": [
    {
      "order": { "...": "OrderView — same shape as orders open rows" },
      "events": [
        {
          "at": "2026-06-11T15:02:11Z",
          "type": "status-updated",
          "status": "Filled",
          "lifecycle_status": "filled",
          "filled": 25,
          "remaining": 0,
          "avg_fill_price": 207.85
        }
      ],
      "events_count": 1,
      "total_events_count": 1,
      "events_truncated": false
    }
  ],
  "as_of": "2026-06-19T10:16:00Z",
  "since": "2026-06-12T10:16:00Z",
  "until": "2026-06-19T10:16:00Z",
  "account": "DU1234567",
  "mode": "paper",
  "count": 1,
  "total_count": 1,
  "events_count": 1,
  "total_events_count": 1,
  "limit": 50,
  "event_limit": 20,
  "truncated": false,
  "events_truncated": false,
  "not_broker_statement": "local order journal only; not an IBKR Activity Statement, trade confirmation, execution report, or historical broker audit",
  "limitations": [
    "Reduced from the daemon's local append-only order journal for the current broker account/mode only."
  ]
}
```

Field meanings:

- `orders[]` is grouped by order, sorted by latest matching in-window
  order update newest first. Each `order` has the same shape documented under
  [orders-open](#orders-open); each `events[]` row has the same shape
  documented under [order-status](#order-status).
- `since` is inclusive and `until` is exclusive after date expansion.
- `account` / `mode` are the connected broker scope used for filtering.
- `count` is returned groups, `total_count` is matching groups before
  the group limit, `events_count` is returned local events, and
  `total_events_count` is local events before per-order event sampling for
  the returned groups.
- `truncated: true` means more matching grouped orders existed than the
  selected `limit`.
- `events_truncated: true` means at least one returned grouped order had
  more local events than `event_limit`; use each row's
  `total_events_count` before treating `events[]` as complete.
- `not_broker_statement` and `limitations[]` are part of the contract;
  surface them when users ask for official trade history, commissions,
  closed-position P&L, or audit-grade evidence.

## order-status

`canary order status <order-ref|order-id|perm-id> --json` — one
journal-backed order plus its full audit trail. The MCP twin is
`canary_order_status`.

```json
{
  "found": true,
  "order": { "...": "OrderView — same shape as orders open rows" },
  "events": [
    {
      "at": "2026-06-11T15:02:09Z",
      "type": "previewed",
      "order_ref": "ibkr-20260611-150210",
      "preview_token_id": "tok-7f3a2c91",
      "symbol": "AAPL",
      "action": "BUY",
      "order_type": "LMT",
      "quantity": 25,
      "limit_price": 207.85,
      "lifecycle_status": "previewed"
    },
    {
      "at": "2026-06-11T15:02:11Z",
      "type": "broker-acknowledged",
      "status": "Submitted",
      "lifecycle_status": "submitted",
      "send_state": "broker_acknowledged"
    }
  ],
  "as_of": "2026-06-11T15:04:02Z",
  "account": "DU1234567",
  "mode": "paper",
  "last_local_event_at": "2026-06-11T15:02:11Z",
  "not_broker_statement": "local order journal only; not an IBKR Activity Statement, trade confirmation, execution report, or historical broker audit",
  "limitations": [
    "Reduced from the daemon's local append-only order journal for the current broker account/mode only.",
    "May miss manual orders, other-client orders, broker activity while the daemon was offline, and rows outside the selected account/mode scope.",
    "Broker callbacks remain authoritative when journaled; absence of a local event is not proof that no broker activity occurred."
  ]
}
```

Field meanings:

- The lookup `id` matches `order_ref` exactly, or the decimal IBKR order
  ID / permanent ID. Matching honors the current broker account/mode
  scope, so `found: false` can mean "belongs to a different account or
  paper/live session", not only "unknown id".
- `found: false` → `order` and `events` are omitted; `as_of`, scope,
  source, and limitations remain so callers can distinguish "not locally
  known in this account/mode" from broker-grade absence.
- `account`, `mode`, `last_local_event_at`, `not_broker_statement`, and
  `limitations[]` have the same meaning as [orders-open](#orders-open).
- `order` — the same `OrderView` shape documented under
  [orders-open](#orders-open), including terminal rows that `orders
  open` no longer lists.
- `events[]` — the append-only audit rows for this order, sorted oldest
  → newest. `type` is one of `previewed`, `token-confirmed`,
  `send-attempted`, `send-error`, `broker-acknowledged`,
  `status-updated`, `modify-requested`, `cancel-requested`,
  `broker-error`, `reconciled-unknown`. Each row carries the same
  contract/draft fields as `OrderView` plus `at` (RFC 3339), `status`,
  `lifecycle_status`, `send_state`, fill fields, `exec_id`/`exec_time`
  for executions, and `message` (raw broker text on errors). Full
  preview tokens are redacted; an event never implies a broker write
  unless `type`/`send_state` explicitly say one was attempted.
- `expired_inferred` lifecycle rows are DAY stock/ETF orders whose
  effective session closed (plus an hour of callback grace) without a
  terminal broker callback. This is local calendar inference — never
  broker-confirmed — and such rows stay modify- and cancel-ineligible.
  GTC trails are exempt from calendar inference; their only self-heal is
  IBKR error 135 ("can't find order"), which maps the row to `inactive`.

## order-preview

`canary order preview buy|sell SYMBOL QTY [flags] --json` (stock/ETF) or
`canary order preview buy|sell SYMBOL YYYYMMDD C|P STRIKE QTY [flags]
--json` (single-leg option) — validate and price an order draft, then
mint a short-lived preview token. **This RPC never transmits anything to
IBKR as an order**; place/modify/cancel are separate gated RPCs. Use only
after [trading-status](#trading-status) shows the local gate ready.

```json
{
  "preview_token": "v1.eyJhbGciOi...",
  "preview_token_id": "tok-7f3a2c91",
  "preview_token_scope": "place",
  "preview_token_expires_at": "2026-06-11T15:07:09Z",
  "token_minted": true,
  "submit_eligible": true,
  "executable": true,
  "mode": "paper",
  "account": "DU1234567",
  "endpoint": "127.0.0.1:7497",
  "client_id": 15,
  "draft": {
    "action": "BUY",
    "contract": {"symbol": "AAPL", "sec_type": "STK", "currency": "USD"},
    "quantity": 25,
    "order_type": "LMT",
    "limit_price": 207.85,
    "tif": "DAY",
    "outside_rth": false,
    "strategy": "patient-limit",
    "order_ref": "ibkr-20260611-150210"
  },
  "quote": {
    "symbol": "AAPL",
    "bid": 207.84, "ask": 207.88, "last": 207.86, "midpoint": 207.86,
    "data_type": "live",
    "quote_quality": "firm",
    "spread_pct": 0.02,
    "price_at": "2026-06-11T11:02:08-04:00",
    "price_as_of": "As of: Jun 11 at 11:02:08 AM EDT",
    "as_of": "2026-06-11T15:02:09Z"
  },
  "position": {"before": 0, "after": 25, "effect": "open"},
  "notional": 5196.25,
  "max_notional": 25000,
  "what_if": {
    "status": "accepted",
    "required_for_submit": true,
    "available": true,
    "margin": {
      "currency": "USD",
      "initial_margin_before": 31000.00,
      "initial_margin_after": 32300.00,
      "maintenance_margin_before": 25000.00,
      "maintenance_margin_after": 26100.00,
      "commission": 1.05,
      "commission_currency": "USD"
    }
  },
  "as_of": "2026-06-11T15:02:09Z"
}
```

Field meanings:

- `preview_token` — the raw daemon-signed bearer token for a later
  `canary order place --preview-token` / `order modify` flow. **CLI JSON
  includes it; the MCP tool strips it** and returns only the redacted
  `preview_token_id` — agents must mint their own token through the
  origin-gated CLI path to place even a paper order.
  `preview_token_scope` is `place`, or `modify` when `--replace-order`
  bound the draft to an existing open order.
  `preview_token_expires_at` is RFC 3339; tokens are short-lived and
  single-use.
- `token_minted` vs `submit_eligible` vs `executable` — `token_minted`
  means the local preview artifact exists. `submit_eligible` is true
  only when IBKR accepted a non-transmitting WhatIf for this exact
  draft; `executable` is a legacy alias kept for older clients.
- `mode` / `account` / `endpoint` / `client_id` — the pinned route the
  token is bound to; place revalidates the binding before any socket
  write.
- `draft` — the canonical intent bound into the token: `action`,
  `contract`, `quantity`, `order_type` (`LMT` | `TRAIL` |
  `TRAIL LIMIT`), `limit_price`, `trail`, `tif` (`DAY` default; `GTC`
  accepted for trails only), `outside_rth`, `strategy`, `order_ref`,
  optional `open_close`/`source`.
  - `strategy` — `patient-limit` (default): bid/ask midpoint rounded one
    tick toward the passive side and clamped inside the spread; requires
    a fresh, live, two-sided quote and rejects stale/delayed data.
    `explicit-limit` (`--limit`): your price, works on stale or delayed
    data. `broker-trail` for TRAIL / TRAIL LIMIT drafts.
  - `trail` — `basis` (`instrument_price`; option trails trail the
    option premium, not the underlying), `offset_type` (`percent` |
    `amount`), `trailing_percent` (IBKR units: `2` means 2%, not 0.02),
    `trailing_amount`, `initial_stop_price` (0 means the daemon binds it
    from live bid/ask), `limit_offset` (TRAIL LIMIT only; nullable).
- `quote` — the market-data inputs preview pricing used. `bid`, `ask`,
  `last`, `mark`, `midpoint` are nullable: `null` means not delivered,
  never zero-substituted. `quote_quality` is `firm`, `indicative`
  (off-hours session), `wide` (spread > 2%), `stale`, `prev_close`, or
  `missing`. `stale`/`stale_reason`, `price_at`/`price_as_of`,
  `session_context`, and `warnings[]` follow the [quote](#quote)
  conventions.
- `position` — local position-effect math: `before` → `after` signed
  quantity and `effect` one of `open`, `increase`, `reduce`, `close`,
  `flip`, `open_short`. Disclosure plus local policy gate (short/flip
  and option sell-to-open policies are enforced here); broker
  permissions and margin remain authoritative.
- `notional` / `max_notional` — order notional and the configured cap it
  was checked against. The size caps (`[trading].max_notional`,
  `max_option_contracts`) bind risk-increasing orders only — `effect`
  `open`/`increase`/`flip`/`open_short`; reduce-only `close`/`reduce`
  orders are exempt, bounded by the position itself, and `max_notional`
  is omitted because no cap bound the preview (also omitted when
  uncapped).
- `what_if` — the broker preview surface. `status` is `accepted` (IBKR
  returned a successful WhatIf for this exact draft), `rejected`
  (`message`/`action`/`advanced_reject_json` carry the broker detail),
  or `unavailable` (no WhatIf path — `submit_eligible` stays false).
  `margin` fields are all nullable floats: initial/maintenance margin
  and equity-with-loan before/after, plus
  commission/min_commission/max_commission with `commission_currency`.
- `warnings[]` — structured `{code, scope, severity, message, impact,
  action}` rows, same shape as other surfaces' `warning_details`.

Place/modify/cancel require a ready broker-write route and the relevant
preview token or open-order eligibility; agent-origin and human-origin
requests share the same daemon gate and journal their origin.

## proposals-status

`canary proposals status --json` — protection-proposal engine readiness:
the auto-trade config gates, the loaded protection policy, and the
embedded trading gate. Part of the `canary_proposals` MCP surface (which
returns snapshots; status is CLI-only).

```json
{
  "kind": "ibkr.auto_trade_status",
  "as_of": "2026-06-11T15:10:00Z",
  "trading": { "...": "TradingStatus — same shape as trading-status" },
  "proposals_enabled": true,
  "fast_path_enabled": true,
  "hot_reload": true,
  "reload_interval": "30s",
  "proposal_cadence": "5m0s",
  "policy": {
    "kind": "ibkr.protection_policy_status",
    "status": "active",
    "policy_id": "protection-default",
    "policy_version": 3,
    "profile": "default",
    "fingerprint": {"version": "protection-policy-fp-v1", "key": "sha256:..."},
    "source": "file",
    "path": "/Users/me/.config/ibkr/protection-policy.toml",
    "loaded_at": "2026-06-11T12:00:04Z",
    "last_checked_at": "2026-06-11T15:09:34Z"
  },
  "blocked": false
}
```

Field meanings:

- `trading` — the full [trading-status](#trading-status) shape embedded,
  so one call answers both "is the engine on" and "could a submit
  actually route".
- `proposals_enabled` — the manual proposal-generation gate
  (`[auto_trade].proposals_enabled`). When false, a `proposals_disabled`
  blocker is added.
- `fast_path_enabled` — one-confirm preview+submit support for
  snapshot-backed proposals (currently trailing stops).
- `hot_reload`, `reload_interval`, `proposal_cadence` — policy-file hot
  reload and the background recompute cadence. Durations are Go strings
  (`"5m0s"`), not seconds.
- `policy.status` — `active` | `default` (built-in policy, no file) |
  `drift` (file changed on disk vs the loaded copy) | `error` |
  `disabled`. On `drift`/`error` the policy's own `blockers` are merged
  into the top-level list. `fingerprint` is the semantic policy hash that
  also stamps every proposal row.
- `blocked` / `blockers[]` — same `{code, message, action}` shape as
  trading status.

## proposals-list

`canary proposals list --json` (bare `canary proposals` is the same) returns
the latest daemon snapshot; `canary proposals refresh --json` recomputes
from live account/positions/regime/market-events first. Both return the
same envelope, and both record a "shown" audit event for the returned
rows. MCP: `canary_proposals` with optional `refresh`/`show` booleans.

```json
{
  "kind": "ibkr.trade_proposal_snapshot",
  "schema_version": "trade-proposal-snapshot-v2",
  "as_of": "2026-06-11T15:10:02Z",
  "revision": "sha256:9c41...",
  "account_id": "DU1234567",
  "account_mode": "paper",
  "policy_id": "protection-default",
  "policy_version": 3,
  "policy_fingerprint": {"version": "protection-policy-fp-v1", "key": "sha256:..."},
  "policy_status": { "...": "same shape as proposals-status policy" },
  "auto_trade": { "...": "same shape as proposals-status" },
  "trading": { "...": "same shape as trading-status" },
  "source_fingerprints": {
    "account": {"version": "account-fp-v1", "key": "sha256:..."},
    "positions": {"version": "positions-fp-v1", "key": "sha256:..."}
  },
  "proposals": [
    {
      "key": "trailing_stop:5e1f0a2b9c3d4e5f",
      "revision": "sha256:9c41...",
      "state": "generated",
      "bucket": "trailing_stop",
      "rank": 1,
      "symbol": "NVDA",
      "sec_type": "STK",
      "action": "SELL",
      "quantity": 120,
      "max_quantity": 120,
      "position_quantity": 120,
      "position_effect": "close",
      "order_type": "TRAIL",
      "trail": {"basis": "instrument_price", "offset_type": "amount",
                "trailing_amount": 44.03, "initial_stop_price": 396.24},
      "trail_sizing": {
        "method": "atr-spread-policy",
        "version": "stock-trail-sizing-v1",
        "data_quality": "fallback",
        "selected_by": "fallback",
        "fallback": true,
        "reference_price": 440.27,
        "reference_source": "bid",
        "policy_min_pct": 6,
        "policy_default_pct": 8,
        "policy_fallback_pct": 10,
        "policy_max_pct": 15,
        "chosen_pct": 10,
        "chosen_amount": 44.03,
        "initial_stop_price": 396.24,
        "missing_reasons": ["atr_14_unavailable"],
        "as_of": "2026-06-11T15:10:02Z"
      },
      "tif": "GTC",
      "outside_rth": false,
      "contract": {"symbol": "NVDA", "sec_type": "STK", "currency": "USD"},
      "reason": "unprotected stock position above policy threshold",
      "details": ["market value 12.4% NLV", "no open protective order"],
      "market_value_pct_nlv": 12.4,
      "created_at": "2026-06-11T15:10:02Z"
    }
  ],
  "counts": {
    "total": 3,
    "actionable": 2,
    "theta_hygiene": 1,
    "risk_reduction": 0,
    "trailing_stop": 2,
    "theta_per_day": -42.18
  }
}
```

Field meanings:

- `kind` / `schema_version` — discriminators. v2 added account/mode
  scoping; persisted v1 snapshots (no `account_mode`) fail closed at
  daemon-startup adoption.
- `revision` — `sha256:…` hash anchored to the policy fingerprint,
  account/mode scope, account+positions source fingerprints, and the
  proposal set (regime/market-event fingerprints are deliberately
  excluded so the one-confirm path doesn't false-stale). `"empty"` marks
  an engine-unavailable shell. Preview/submit require the exact
  `(key, revision)` pair; after a recompute, old pairs are refused with a
  `stale_revision` blocker — refresh and re-read before acting.
- `account_id` / `account_mode` — concrete single account (never the
  "All" aggregate) and the `paper`/`live` session the proposals were
  generated under.
- `auto_trade` / `trading` / `policy_status` — full embedded copies of
  the [proposals-status](#proposals-status) and
  [trading-status](#trading-status) shapes.
- `source_fingerprints` — semantic-bucket fingerprints of the consumed
  inputs (`account`, `positions`, `regime`, `market_events`); each entry
  is nullable.
- `market_events` — embedded [market-events](#market-events) result when
  flags exist for held symbols; proposals affected by a flag also carry
  per-row `market_flags[]`.
- `proposals[]` — always present (possibly empty).
  - `key` — stable `bucket:hash` identity per bucket+contract+action;
    `state` is `generated` | `blocked`.
  - `bucket` — `theta_hygiene` (close/reduce short-dated options),
    `risk_reduction` (trim single-name concentration), or
    `trailing_stop` (broker-side protective trail). `rank` orders rows
    within the snapshot.
  - `quantity` / `max_quantity` — proposed and maximum selectable size;
    `position_quantity` and `position_effect` (same vocabulary as
    order-preview `position.effect`) describe the held position the
    proposal acts on.
  - `order_type` / `trail` / `tif` / `outside_rth` / `contract` — the
    draft the engine would preview; trail rows carry the computed
    `initial_stop_price`. `limit_price` is nullable.
  - `trail_sizing` — stock/ETF trailing-stop sizing explanation. Percent
    fields are human units (`10` means 10%). Dynamic rows name the selected
    input (`atr`, `spread_floor`, `policy_default`, etc.); if no dynamic
    sizing could be calculated, `fallback=true`, `data_quality="fallback"`,
    `selected_by="fallback"`, and `chosen_pct` uses the policy fallback
    (10% by default) with `missing_reasons[]` explaining what was absent.
  - `reason` + `details[]` — human-readable evidence. `score`,
    `theta_per_day`, `notional`, `risk_excess_notional` /
    `risk_excess_currency`, and `market_value_pct_nlv` (nullable) are
    bucket-specific metrics, omitted when not applicable.
  - per-row `blockers[]` — why this row is not actionable (e.g.
    `stock_protection_disabled`, duplicate-protection guards) with
    remediation `action` text.
- `counts` — `total`, `actionable` (rows with zero blockers), per-bucket
  counts, `market_flags`, summed `theta_per_day`, and
  `risk_reduction_excess_notional`/`_currency`. The excess aggregate is
  omitted entirely when risk-reduction proposals span different local
  currencies — a raw cross-currency sum is not a number in any currency
  (pre-2026-06-12 snapshots used a `"MIX"` currency sentinel instead).
- `blockers[]` (snapshot level) and `loaded_from_state` — the latter is
  true when the daemon is serving a snapshot adopted from persisted
  state at startup rather than a fresh compute this session.
- Read-only contract: list/refresh never preview, submit, place, modify,
  cancel, or expose raw preview tokens.

## settings-show

`canary settings show --json` — platform settings and observed read-only
state. The MCP twin is `canary_settings` (read-only; there is intentionally
no MCP settings write tool in v1). Every leaf is a typed cell
`{value, access, source, reason}`: `access` (`read` | `write`) says
whether `canary settings set` can change it, `source` (`runtime` | `config`
| `build` | `observed`) names the owning layer, and `reason` explains
read-only cells or how to change them.

```json
{
  "kind": "ibkr.platform_settings",
  "features": {
    "purge_restore": {
      "enabled": {"value": true, "access": "write", "source": "runtime"}
    },
    "stock_protection": {
      "enabled": {"value": true, "access": "write", "source": "runtime"}
    }
  },
  "trading": {
    "freeze": {"value": false, "access": "write", "source": "runtime",
               "reason": "runtime brake: true blocks new broker writes; cancels stay allowed"},
    "mode": {"value": "paper", "access": "read", "source": "config",
             "reason": "set [trading].mode in config.toml to \"disabled\", \"paper\", or \"live\""},
    "account": {"value": "DU1234567", "access": "read", "source": "config",
                "reason": "set [gateway].account in config.toml"},
    "endpoint": {"value": "127.0.0.1:7497", "access": "read", "source": "observed",
                 "reason": "observed from daemon gateway discovery/config"},
    "client_id": {"value": 15, "access": "read", "source": "config",
                  "reason": "set [gateway].client_id in config.toml"},
    "mcp_trading": {"value": "disabled", "access": "read", "source": "build",
                    "reason": "MCP broker-write controls are not exposed"},
    "live_override": {"value": "blocked", "access": "read", "source": "config",
                      "reason": "computed from [trading].mode and active blockers; \"ready\" only on an unblocked live route"},
    "build_writes_available": {"value": true, "access": "read", "source": "build",
                               "reason": "controlled by the Canary build"},
    "limits": {
      "max_notional": {"value": 25000, "access": "write", "source": "runtime"},
      "max_option_contracts": {"value": 10, "access": "write", "source": "config"},
      "allow_stock_short": {"value": false, "access": "write", "source": "config"},
      "allow_option_sell_to_open": {"value": false, "access": "write", "source": "config"}
    }
  },
  "auto_trade": {
    "proposals_enabled": {"value": true, "access": "read", "source": "config",
                          "reason": "set [auto_trade].proposals_enabled in config.toml"},
    "fast_path_enabled": {"value": true, "access": "read", "source": "config",
                          "reason": "set [auto_trade].fast_path_enabled in config.toml"},
    "policy_file": {"value": "protection-policy.toml", "access": "read", "source": "config",
                    "reason": "set [auto_trade].policy_file in config.toml"},
    "hot_reload": {"value": true, "access": "read", "source": "config",
                   "reason": "set [auto_trade].hot_reload in config.toml"},
    "reload_interval": {"value": "30s", "access": "read", "source": "config",
                        "reason": "set [auto_trade].reload_interval in config.toml"},
    "proposal_cadence": {"value": "5m0s", "access": "read", "source": "config",
                         "reason": "set [auto_trade].proposal_cadence in config.toml"}
  },
  "market_data": {
    "quality": {
      "status": "ok",
      "summary": "observed quotes look live or usable",
      "quote_counts": {"live": 12, "frozen": 2},
      "access": "read",
      "source": "observed",
      "reason": "observed from quote, position, chain, and status surfaces; entitlements are never stored",
      "observed_at": "2026-06-11T15:12:00Z"
    }
  },
  "build": {
    "channel": {"value": "experimental-trading", "access": "read", "source": "build",
                "reason": "controlled by the Canary build"},
    "trading_writes_available": {"value": true, "access": "read", "source": "build",
                                 "reason": "controlled by the Canary build"},
    "experimental_trading_note": "experimental trading build; runtime limit overrides are writable only when [trading].mode is paper or live"
  },
  "as_of": "2026-06-11T15:12:00Z"
}
```

Field meanings:

- `features.purge_restore.enabled` / `features.stock_protection.enabled`
  — runtime user preferences (always `write`/`runtime`). Default `true`
  when never set; `canary settings set ...=null` clears the override back
  to the default.
- `trading.freeze` — the runtime trading brake. `true` blocks every new
  broker write (place/modify/purge/restore/proposal submits) while
  cancels stay allowed; it engages even when order entry is otherwise
  misconfigured. Human-only on live routes.
- `trading.mode`, `account`, `client_id`, `live_override` — read-only
  mirrors of config (the same values trading-status reports);
  `trading.mcp_trading` and `build_writes_available` mirror build
  capability; `trading.endpoint` is `observed` from discovery/config.
- `trading.limits.*` — the four runtime safety limits. `access` is
  `write` only on a trading-capable build with `[trading].mode` set to
  paper or live; otherwise `read` with the reason in each cell. `source`
  is `runtime` when a runtime override is in effect, else `config`
  (config defaults). On a live route, agent-origin sessions cannot change
  them at all — a human must edit limits from an interactive terminal.
- `auto_trade.*` — read-only config mirrors of the manual
  proposal-engine gates (the same values
  [proposals-status](#proposals-status) reports as plain
  booleans/strings).
- `market_data.quality` — observed feed quality, never stored
  entitlements: `status` is `ok` | `delayed` | `degraded` (some decision
  surface reported degraded data quality) | `unknown` (nothing observed
  yet), with `quote_counts` keyed by data_type, optional
  `data_quality[]` rows (same shape as `status.health`), and
  `observed_at` (RFC 3339, omitted until something was observed).
- `build` — `channel` is `stable` | `experimental-trading`;
  `experimental_trading_note` is display prose for the active build.
