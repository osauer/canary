# MCP resources reference

Last reviewed: 2026-06-11 08:17 CEST

These are the non-tool resources `ibkr mcp` exposes to MCP clients. Tools are documented separately in [MCP tools reference](mcp-tools.md).

## `ibkr://quote/{symbol}`

Live stock / ETF quote resource.

Use this when an MCP client wants to keep watching one equity or ETF quote without repeatedly calling `ibkr_quote`. The template is discovered through `resources/templates/list`.

Example URI:

```text
ibkr://quote/AAPL
```

`resources/read` returns a one-off quote snapshot with the same JSON shape as `ibkr quote AAPL --json`, including the enriched headline `price`, `price_source`, previous close, change, ranges, volume/average volume, 20-day average volume/dollar volume, `price_as_of`, stale flags, and optional `session_context`.

`resources/subscribe` returns `{}` and then streams coalesced tick frames through `notifications/resources/updated` until the client calls `resources/unsubscribe` or closes the MCP session. Notification payloads embed a JSON frame in `params.contents[].text`; the frame shape matches `ibkr quote AAPL --watch --json`.

Only stock and ETF symbols are supported. Option contract streaming is not exposed as an MCP resource today; use `ibkr_chain` or CLI option quotes for option snapshots.
