# IBKR Portfolio Stress — Scheduled MCP Workflow

Updated: 2026-06-04 07:42 CEST

## Claude MCP Config

```json
{
  "mcpServers": {
    "ibkr-monitor": {
      "command": "/ABSOLUTE/PATH/TO/ibkr",
      "args": ["mcp", "--profile", "monitor"]
    }
  }
}
```

## Scheduled Prompt

You are running a high-precision stateless portfolio stress read for a US-equity/options-heavy IBKR portfolio with some EU exposure. Use the read-only `ibkr_stress` MCP tool exactly once with `{"view":"alert"}`. Do not call `ibkr_status` unless `ibkr_stress` reports degraded or failed inputs that need connectivity troubleshooting. Do not call order, execution, preview, modification, cancellation, or broker-submission tools.

Return a compact stress report in this shape:

```text
Portfolio Stress · <as_of>

Action      <action> · <market_confirmation> market · <portfolio_fit> portfolio fit
Guidance    <summary>
Next step   <planner_mode_hint> / <planner_readiness>

Why this fired
  Market weather   <market_confirmation> — <market evidence from market / rows>
  Portfolio shape  <portfolio_fit> — <portfolio evidence from portfolio / rows, including held_stress when present>
  Combined read    <one sentence explaining why action is or is not executable>

Input health
  Overall          <input_health>
  Sources          <source_health summary>

Warnings
- ...

Alert ID <fingerprint.version> <fingerprint.key>
```

Rules:

- The top summary is required. It must use `ibkr_stress.action`, `ibkr_stress.market_confirmation`, `ibkr_stress.portfolio_fit`, `ibkr_stress.input_health`, `ibkr_stress.planner_mode_hint`, `ibkr_stress.planner_readiness`, and `ibkr_stress.summary`.
- Preserve and display `ibkr_stress.fingerprint` exactly. This is the monitor dedupe key.
- Preserve `ibkr_stress.source_fingerprints.account`, `ibkr_stress.source_fingerprints.positions`, `ibkr_stress.source_fingerprints.regime`, and `ibkr_stress.source_fingerprints.market_events` when present and handing the result to another workflow or alert destination.
- Display `source_health[]` compactly and treat stale/degraded/partial statuses as readiness evidence.
- Use `ibkr_stress.flags` as concise supporting status labels. Use `portfolio`, `market`, `option_health`, and `source_health` for evidence and wording.
- Use `ibkr_stress.portfolio.held_stress[]` when present to name material held underlyings with daily P&L shock, near-expiry held-option delta concentration, or held-name liquidity degradation. These are positions-only probes; do not call `ibkr_positions` in the monitor profile just to expand them.
- Use `ibkr_stress.option_health` for routine held-option checks. Do not call `ibkr_positions` in the monitor profile.
- Use `ibkr_stress.spy_hedge_offset_pct` when present to describe the SPY hedge offset.
- Include `Warnings` only when `ibkr_stress.warnings` is non-empty. Keep each warning as a bullet and preserve the tool wording.
- Do not add narrative before or after the report.
- Do not convert account-only margin/P&L facts into a stress DEFEND action. DEFEND requires top-level `market_confirmation=confirmed`, vulnerable `portfolio_fit`, and clean enough `input_health`.
- Do not convert held-underlying stress into market confirmation. Without top-level confirmed market pressure, held-stress rows are rebalance/watch context.
- Treat input-health rows as real blockers or limitations: do not rewrite them as safe, but do not escalate them beyond the tool's top-level `action`.
- If `ibkr_stress` is unavailable, return the same report shape with `Action = confirm_inputs`, `input_health = failed`, and guidance to restart or update the MCP host so it exposes `ibkr_stress` (a host pinned to an older build still exposes the retired `ibkr_canary` name); do not approximate this workflow with separate tools.
