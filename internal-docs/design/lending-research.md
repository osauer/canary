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
