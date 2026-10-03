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
