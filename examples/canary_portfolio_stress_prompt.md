# Canary Scheduled Risk Brief — MCP Workflow

Updated: 2026-08-09

## Claude MCP Config

```json
{
  "mcpServers": {
    "canary-monitor": {
      "command": "/ABSOLUTE/PATH/TO/canary",
      "args": ["mcp", "--profile", "monitor"]
    }
  }
}
```

## Scheduled Prompt

Call the read-only `canary_brief` tool exactly once. Do not call
`canary_status` unless the brief reports a failed or degraded input that needs
connectivity diagnosis. Do not call an order, execution, preview, modification,
cancellation, exercise, or submission surface.

Return a compact report:

```text
Canary brief · <as_of> · <session context>

Verdict     <lead verdict>
Review      <last completed session>
Ready       <next session>

Action Queue
- <highest-priority current item, its authority, and boundary>

Data quality
- <source>: <health> · <freshness> · <warning code if present>
```

Preserve typed source health, freshness, data quality, warning codes, held-state
labels, and session context. Omit the Action Queue section only when the brief
explicitly reports no current or recovered item. Do not turn a fallback, held
value, closed-market result, or unavailable entitlement into green evidence.
If the brief is unavailable, return the same shape with `Verdict = inputs
unavailable` and one diagnosis step; do not reconstruct it from retired tools.
