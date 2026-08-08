# Working with agents

Updated: 2026-08-04 22:57 CEST

`canary mcp` makes read/status CLI operations and preview-only stock/ETF order drafts available to MCP clients: Claude Code, Claude Desktop, or any other host that speaks the protocol. The same daemon serves the CLI and MCP, and the MCP layer is a thin adapter over the existing RPCs. Official market calendars and stock/ETF quotes are also available; quote resources can be read once or subscribed to for streaming updates.

For exact tool parameters and JSON envelopes, see the auto-generated [MCP tools reference](../reference/mcp-tools.md). For protocol mechanics, see the upstream [Model Context Protocol spec](https://modelcontextprotocol.io/).

## Setup

The Claude Code plugin is registered when you install via the marketplace. It
carries the `canary` skill, safety hooks, and plugin-local MCP server config for
`canary mcp`; install the `canary` binary separately. Confirm it's wired:

```sh
claude plugin details canary@canary   # Skills (1), Hooks (2), MCP servers (1)
claude mcp list                       # plugin:canary:canary connected
canary status                         # daemon health, gateway connection, data freshness
```

Direct skill installs also work in Claude Code when `SKILL.md` is copied under
`~/.claude/skills/<name>/`, but that creates only a skill command. Use the
plugin path for normal Canary installs because the MCP tools and safety hooks are
plugin components.

The tools mirror the agent-appropriate CLI commands: `canary_status` ↔ `canary status`, `canary_brief` ↔ `canary brief`, `canary_calendar` ↔ `canary calendar`, `canary_gamma` ↔ `canary gamma`, `canary_market_events` ↔ `canary market-events`, `canary_order_preview` ↔ `canary order preview`. `canary_watch` maps to the enriched `canary watch` by default, or to read-only `canary watch --list` when `include_quotes` is false. Local lifecycle verbs stay outside the tool set: `setup`, `update`, `restart`, `mcp`, and `daemon`. Claude calls the tools as MCP operations rather than CLI subcommands.

## Example conversations

### "What do I need to know before the open?"

→ Claude invokes `canary_brief`.

The daemon returns the same Review/Ready briefing served to the terminal and paired app. Review covers what changed since the last regular close; Ready covers today's market, risk capacity, protection work, process clocks, and missing inputs. The MCP tool is read-only and cannot sign off or stamp the brief.

### "Is the market regime favorable right now?"

→ Claude invokes `canary_regime`.

Returns the eight-row dashboard. Each row carries raw measurements, compact band/as-of metadata, scoped warnings when data is stale or unavailable, and a `streak` field when the row is rankable. The top-level envelope also carries lifecycle stage, readiness, source health, and semantic fingerprints for monitor dedupe.

Claude composes an answer that names which indicators are in which band, calls out any in red, and flags streaks (a Day-5 stress event reads differently from a Day-1 spike). The dashboard is *information*, not a verdict; the user's risk tolerance determines what to do with it. [Concepts → Regime](../understand/concepts.md#regime) names the rows.

### "Should the monitor stay quiet, watch, act, rebalance, flag opportunity, or block on data quality?"

→ Claude invokes `canary_stress`.

Returns a stateless market-context portfolio monitor for scheduled stress checks. The stress read combines market-regime clusters, direct SPY/VIX tape shock, current exposures, concentration, positions-only held-underlying stress, option-greeks coverage, and input-health gates into `action`, `market_confirmation`, `portfolio_fit`, and `input_health`.

The tool is deliberately high-precision: a standalone pre-market SPY drawdown or VIX spike can raise `watch`, while `defend` requires confirmed market pressure, vulnerable portfolio fit, and clean enough inputs. Account-only margin or P&L facts remain evidence; they do not become a `defend` action by themselves. Missing, stale, degraded, warming, or computing inputs become explicit input-health rows instead of being treated as safe.

Held-underlying stress appears in `portfolio.held_stress[]` only when a material held name has a real positions-derived condition. For held-name market-structure context, use `canary_market_events`; the stress read consumes that signal as supporting context, not as a standalone trigger. [Concepts → Stress](../understand/concepts.md#stress) lists those conditions and the fuller policy.

For a scheduler-friendly prompt that preserves action, market confirmation, portfolio fit, input health, readiness, source health, fingerprints, and warnings, use [examples/canary_portfolio_stress_prompt.md](https://github.com/osauer/canary/blob/main/examples/canary_portfolio_stress_prompt.md). The current tool returns the decision surface; notifications, circuit breakers, and broker-specific automation policies are intentionally left to the host or user workflow.

### "Does GME have borrow, Reg SHO, LULD, or halt context?"

→ Claude invokes `canary_market_events` with `{"symbol":"GME"}`.

Returns market-event flags for requested or held stock/ETF symbols: IBKR shortable-share inventory, IBKR short-stock availability fee rate, Nasdaq Reg SHO threshold-list membership, and active/recent Nasdaq LULD or regulatory/news halts. The response carries `flags[]`, `by_symbol`, `source_health[]`, `warning_details[]`, and a semantic `fingerprint`.

Claude should report active flags as context and safety gates, not as standalone trade ideas. Unknown source health means unavailable evidence, not inactive. Borrow inventory and fee stress are only proposal modifiers for existing short buy-to-cover reductions; they do not justify opening or adding long exposure. Active halt/LULD flags block protection preview/submit; recent halt/LULD flags require fresh quote context.

### "Show me my SPY positions and any options on them."

→ Claude invokes `canary_positions` with `{"symbol": "SPY"}`.

Returns rows for SPY stock holdings and any SPY options, with per-leg Greeks (delta/gamma/theta/vega) for the options, plus a `portfolio` block aggregating effective_delta in share-equivalents. Claude typically renders the stock holding alongside an aggregate Greek line ("you're net long ~1,500 SPY-deltas after the options"). Daily P&L is included from IBKR's `reqPnLSingle` stream: `null` when the daemon hasn't pre-warmed that contract, never zero-substituted.

If you also want context, follow-up questions naturally chain: *"and what's SPY's dealer gamma profile?"* invokes `canary_gamma`; *"how does that compare to where SPY closed yesterday?"* invokes `canary_history` + `canary_quote`.

### "What's on my watchlist, with prices and what I hold?"

→ Claude invokes `canary_watch`.

Returns one row per saved symbol with headline price and currency, previous close, absolute and percent change, day range, 52-week range, volume versus average volume, `price_as_of`, stale/session context, and compact stock holding context where the account owns the symbol. Claude should call out stale or closed-market rows instead of treating the values as fresh live prices.

The MCP tool is read-only. Claude can use the symbols for follow-up quote, history, chain, scan, gamma, or regime context, but it cannot add, remove, or clear watchlist entries through MCP. If the user asks only for the saved symbol inventory, Claude passes `{"include_quotes": false}`.

### "Why does SPY look stale at 1am ET?"

→ Claude invokes `canary_quote`; if the snapshot is frozen, delayed, or missing live prices, the response may include `session_context`. Claude may also invoke `canary_calendar` directly to answer "when does it reopen?"

Returns the official market state for the relevant supported calendar: US cash equities, US listed options regular sessions, or German Xetra cash equities. A US quote at 1am ET will normally say the regular session is closed and show the next 09:30 ET open; a US holiday shows the holiday reason and the next known open. For example, Whit Monday 2026 is closed for US equities because it is Memorial Day, while Xetra is open.

### "Are SPY dealers supporting or amplifying today's moves?"

→ Claude invokes `canary_gamma` (default scope = combined SPY+SPX).

Returns the signed zero-gamma price level, the dealer book's current sign (`positive` = long-gamma = stabilizing; `negative` = short-gamma = amplifying), the regime-agreement classifier between SPY and SPX (`agree:long-gamma` / `agree:short-gamma` / `agree:transition-gamma` / `disagree`), and the magnitude view via `gamma_total_abs` and `top_strikes`.

The important diagnostic is **`disagree`**: one book stabilizing while the other amplifies, indicating institutional/retail positioning divergence. Claude usually flags this prominently.

Always read `quality.rankability` before treating gamma as a market-structure signal. `rankable` means the read is fresh and covered enough; `context_only` is awareness-only; `blocked` and `unavailable` are data-quality blockers.

Do not treat missing 0DTE alone as a gamma no-vote. With healthy SPX 1-7DTE and
term coverage the result stays rankable, disclosing the missing bucket in
`quality.coverage` and `warning_details`. After the expiring SPXW series closes,
0DTE can be absent while the broader SPX surface is still usable.

When no serveable result exists, `canary_gamma` kicks a multi-minute background compute and returns `status: "computing"` with an ETA. During options RTH, a served result refreshes behind the last-good value after 15 minutes. See [Sensors → Gamma](../understand/sensors.md#gamma) and [Concepts → Gamma](../understand/concepts.md#gamma).

### "Preview buying 10 AAPL shares."

→ Claude invokes `canary_trading_status`, then `canary_order_preview` only if the local preview gate is ready.

Returns a draft order, quote inputs, position impact, notional, warnings, and preview-token fields. `token_minted` means the local daemon created a preview artifact. `submit_eligible` means broker WhatIf accepted the exact draft and a future write path could consider the token. If broker WhatIf is unavailable or rejected, `token_minted` can still be true while `submit_eligible` and compatibility field `executable` are false. The preview itself does not place, modify, cancel, or transmit any broker order.

### "Why is my AAPL stop showing red?"

→ Claude invokes `canary_orders_open` and `canary_positions`, then explains the mismatch.

A protective stop that no longer matches its position (a partial sale in TWS is the usual cause) is classified critical, with the consequence stated plainly: triggering would close the shares still held and open the excess in the opposite direction.

The fix lives on the paired device. The app offers one guided action that reduces the stop to the held quantity through the normal preview and confirm steps, and the daemon re-checks the live position at both steps.

The order journal behind these reads reconciles itself against the broker's open-order list after each reconnect and every 30 minutes, so a cancel that happened while the daemon was offline closes its row as `closed_reconciled` instead of lingering as a stale open order.

## What Claude can't do here

The MCP interface intentionally has no trade-execution tool. Claude can:

- ✅ tell you what you own
- ✅ read your local saved-symbol watchlist
- ✅ tell you the market state
- ✅ size a trade (`canary_size`: pure math against your NLV, never proposes an order)
- ✅ preview a locally gated stock/ETF LMT draft without broker submission
- ❌ place an order
- ❌ cancel an order
- ❌ modify a position

If you ask Claude to "buy 100 shares of AAPL," it can preview a non-submitting draft only if you explicitly ask for preview. It cannot submit that order, and won't try to. This is a hard architectural boundary: the bundled daemon does not expose broker-write paths to MCP, regardless of what Claude asks.

Streaming quote resources are separate from tools. MCP clients discover the `canary://quote/{symbol}` template via `resources/templates/list`, then read one snapshot or subscribe for coalesced tick frames that stop when the client unsubscribes or closes the MCP session. The [MCP resources reference](../reference/mcp-resources.md) has the exact method names and payload shapes.

Other things outside the scope today:

- **Option streaming** (continuous option contract ticks). Option snapshots are available through chains and option quotes, but the MCP streaming resource is stock/ETF only.
- **Non-equity asset classes** (futures, FX spot, crypto, bonds). Equity, ETF, Xetra cash-equity, and regular-session US listed-options calendars are covered; everything else is out of scope or partial context today.
- **Other indices' breadth or constituents** (NDX, RUT, sector-specific). S&P 500 only.

## Tips for getting good answers

- **Ask the question, don't name the tool.** "How does my portfolio look?" works better than "Run canary_positions." Claude picks the right tool based on the question; naming the tool just adds friction.
- **Chain follow-ups freely.** Each tool call is cheap (cached when possible). "And what about gamma for those?" or "How did that look yesterday?" generate natural follow-up tool calls.
- **For the dashboard, ask "how does the market regime look?"** It triggers `canary_regime`, which returns the eight-row dashboard in one call. Faster than asking about each indicator separately.
- **For scheduled stress checks, ask for the stress read.** "How does market weather interact with my portfolio right now?" triggers `canary_stress`, which returns the whole decision surface plus evidence rows in one call, so the assistant never composes its own escalation ladder.
- **For the daily desk, ask what needs attention before the open.** That triggers `canary_brief`, so Claude reads Canary's assembled Review/Ready result instead of rebuilding it from several smaller tools.
- **For sizing, give Claude the full plan.** "I want to enter AAPL at 180 with a stop at 175 and a target at 195, risking 1% of NLV" lets `canary_size` return the R-multiple, breakeven win rate, and share count in one round-trip.

## Reference

- [MCP tools reference](../reference/mcp-tools.md): auto-generated table of every tool, parameters, descriptions.
- [MCP resources reference](../reference/mcp-resources.md): streaming stock/ETF quote resource semantics.
- [Concepts](../understand/concepts.md): the mental model for regime / gamma / breadth.
- [Updating](../start/updating.md): keeping the binary + constituent list current.
- [Model Context Protocol spec](https://modelcontextprotocol.io/): the upstream protocol.
