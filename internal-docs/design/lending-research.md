# Indicative lending research adapter

The CLI `lending rates --symbols ...` and MCP `canary_lending_rates` adapt the
existing `market_events.snapshot` RPC with an explicit 1–100-symbol scope.
Shared normalization and response-scope validation live in `internal/rpc`.
Neither adapter owns acquisition or fee calculations. The daemon's existing
source/cache cadence, dated coverage, nullable rate and numeric-scale status
remain unchanged. `canary_lending_fees` continues to mean actual earned income.

The generated CLI and MCP references and discovery server card are regenerated
with `make docs-regen`; `make docs-check` detects drift. CLI and MCP tests cover
invalid scope before daemon access, the read-only RPC boundary and preservation
of unavailable evidence. Desk's research threshold and persistence labels are
presentation/playbook hypotheses, not Canary trading policy or lending yields.

Pending release note: explicit-symbol indicative borrowing-rate reads are now
available in CLI and MCP; missing or stale rates never become zero rates.

### Broader US fee discovery

Use `canary lending screen --min-rate 50 --limit 25 --exclude AAA,BBB --json`
or `canary_lending_screen` with `{"min_rate":50,"limit":25,"exclude":["AAA","BBB"]}`
to discover names beyond an existing list. The example symbols are fictional.
CLI/MCP default to 50% and 25 rows; limits are 1–100, exclusions at most 100.
The shared `lending.screen` RPC defaults to 50 rows and accepts a nonnegative
minimum annualized borrower percentage. These are research filters, not risk policy.

The daemon scans its existing IBKR US short-stock bulk feed, reusing acquisition
cadence, durable cache, calendar and failure backoff. There is no per-symbol
broker fan-out, independent download or unverified historical-rate fallback.
Only usable, published, finite, nonnegative USD rates qualify; results sort by
fee descending, then symbol. Names come from the provider and do not establish
exact broker identity, exchange listing, liquidity or lending eligibility.

`total` counts parsed USD records, `usable` counts valid published USD rates,
`matching` counts those meeting the filter after exclusions, and `truncated`
means more matches exist than returned rows. `skipped_rows` reports malformed
feed rows omitted by the source parser. `status: unavailable` is distinct from
an observed zero-match screen. Both source and receipt clocks, plus source health,
remain visible. Stale/failed feeds never rank last-good quotes as current.
This covers the US feed, not every market; distressed and OTC names may appear.
No enrollment, watchlist mutation, policy change or order is authorized.

### Lending price and liquidity context

`canary lending market --symbols AAA,BBB --json` and the read-only
`canary_lending_market` MCP tool return matching cached context for 1–100 explicit
USD names in the borrowing feed. Missing exact contract IDs stay unavailable.
The daemon queues at most 150 names, follows requested names for 15 minutes,
and refreshes one at a time on the background lane, with a 35-second bound and
five-minute retry/receipt interval. No quote fan-out occurs inside the screen read.
A name IBKR does not recognise (no security definition, inactive, or the history
service's "Unknown contract") shows unavailable and is not asked again until the
broker session changes, and for at least 30 minutes: delisted names the feed still
lists otherwise drew a broker WARN every five minutes each.
History reuses the existing durable cache without adding chart-refresh interests.

Rows carry last completed close or a recent actual trade (including delayed-feed
labels), change from prior close, session share volume, average daily dollar
turnover over all 20 completed sessions, and completed-close YTD price change
from the exact prior year-end session. YTD excludes dividends; missing year-end,
latest session or any volume baseline stays missing. Field dates and pending,
partial, unavailable and expiry remain explicit. Filters are research preferences,
not trading policy or a liquidity guarantee. This does not screen liquidity across
the full borrowing feed; consumers filter their retained rows only.
