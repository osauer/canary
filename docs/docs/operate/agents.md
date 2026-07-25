# Agentic use

Updated: 2026-07-25 09:40 CEST

`ibkr mcp` makes read/status CLI operations and preview-only stock/ETF order drafts available to MCP clients: Claude Code, Claude Desktop, or any other host that speaks the protocol. The same daemon serves the CLI and MCP, and the MCP layer is a thin adapter over the existing RPCs. Official market calendars and stock/ETF quotes are also available; quote resources can be read once or subscribed to for streaming updates.

For exact tool parameters and JSON envelopes, see the auto-generated [MCP tools reference](../reference/mcp-tools.md). For protocol mechanics, see the upstream [Model Context Protocol spec](https://modelcontextprotocol.io/).

## Setup

The Claude Code plugin is registered when you install via the marketplace. It
carries the `ibkr` skill, safety hooks, and plugin-local MCP server config for
`ibkr mcp`; install the `ibkr` binary separately. Confirm it's wired:

```sh
claude plugin details ibkr@ibkr   # Skills (1), Hooks (2), MCP servers (1)
claude mcp list                   # plugin:ibkr:ibkr connected
ibkr status                       # daemon health, gateway connection, data freshness
```

Direct skill installs also work in Claude Code when `SKILL.md` is copied under
`~/.claude/skills/<name>/`, but that creates only a skill command. Use the
plugin path for normal IBKR installs because the MCP tools and safety hooks are
plugin components.

The tools mirror the agent-appropriate CLI commands: `ibkr_status` ↔ `ibkr status`, `ibkr_calendar` ↔ `ibkr calendar`, `ibkr_gamma` ↔ `ibkr gamma`, `ibkr_market_events` ↔ `ibkr market-events`, `ibkr_order_preview` ↔ `ibkr order preview`. `ibkr_watch` maps to the enriched `ibkr watch` by default, or to read-only `ibkr watch --list` when `include_quotes` is false. Local lifecycle verbs stay outside the tool set: `setup`, `update`, `restart`, `mcp`, and `daemon`. Claude calls the tools as MCP operations rather than CLI subcommands.

## Example conversations

### "Is the market regime favorable right now?"

→ Claude invokes `ibkr_regime`.

Returns the eight-row dashboard. Each row carries raw measurements, compact band/as-of metadata, scoped warnings when data is stale or unavailable, and a `streak` field when the row is rankable. The top-level envelope also carries lifecycle stage, readiness, source health, and semantic fingerprints for monitor dedupe.

Claude composes an answer that names which indicators are in which band, calls out any in red, and flags streaks (a Day-5 stress event reads differently from a Day-1 spike). The dashboard is *information*, not a verdict; the user's risk tolerance determines what to do with it. [Concepts → Regime](../understand/concepts.md#regime) names the rows.

### "Should the canary stay quiet, watch, act, rebalance, flag opportunity, or block on data quality?"

→ Claude invokes `ibkr_canary`.

Returns a stateless market-context portfolio monitor for scheduled stress checks. The canary combines market-regime clusters, direct SPY/VIX tape shock, current exposures, concentration, positions-only held-underlying stress, option-greeks coverage, and input-health gates into `action`, `market_confirmation`, `portfolio_fit`, and `input_health`.

The tool is deliberately high-precision: a standalone pre-market SPY drawdown or VIX spike can raise `watch`, while `defend` requires confirmed market pressure, vulnerable portfolio fit, and clean enough inputs. Account-only margin or P&L facts remain evidence; they do not become a canary `defend` action by themselves. Missing, stale, degraded, warming, or computing inputs become explicit input-health rows instead of being treated as safe.

Held-underlying stress appears in `portfolio.held_stress[]` only when a material held name has a real positions-derived condition. For held-name market-structure context, use `ibkr_market_events`; the canary consumes that signal as supporting context, not as a standalone trigger. [Concepts → Canary](../understand/concepts.md#canary) lists those conditions and the fuller policy.

For a scheduler-friendly prompt that preserves action, market confirmation, portfolio fit, input health, readiness, source health, fingerprints, and warnings, use [examples/ibkr_portfolio_canary_prompt.md](https://github.com/osauer/ibkr/blob/main/examples/ibkr_portfolio_canary_prompt.md). The current tool returns the decision surface; notifications, circuit breakers, and broker-specific automation policies are intentionally left to the host or user workflow.

### "Does GME have borrow, Reg SHO, LULD, or halt context?"

→ Claude invokes `ibkr_market_events` with `{"symbol":"GME"}`.

Returns market-event flags for requested or held stock/ETF symbols: IBKR shortable-share inventory, IBKR short-stock availability fee rate, Nasdaq Reg SHO threshold-list membership, and active/recent Nasdaq LULD or regulatory/news halts. The response carries `flags[]`, `by_symbol`, `source_health[]`, `warning_details[]`, and a semantic `fingerprint`.

Claude should report active flags as context and safety gates, not as standalone trade ideas. Unknown source health means unavailable evidence, not inactive. Borrow inventory and fee stress are only proposal modifiers for existing short buy-to-cover reductions; they do not justify opening or adding long exposure. Active halt/LULD flags block protection preview/submit; recent halt/LULD flags require fresh quote context.

### "Show me my SPY positions and any options on them."

→ Claude invokes `ibkr_positions` with `{"symbol": "SPY"}`.

Returns rows for SPY stock holdings and any SPY options, with per-leg Greeks (delta/gamma/theta/vega) for the options, plus a `portfolio` block aggregating effective_delta in share-equivalents. Claude typically renders the stock holding alongside an aggregate Greek line ("you're net long ~1,500 SPY-deltas after the options"). Daily P&L is included from IBKR's `reqPnLSingle` stream: `null` when the daemon hasn't pre-warmed that contract, never zero-substituted.

If you also want context, follow-up questions naturally chain: *"and what's SPY's dealer gamma profile?"* invokes `ibkr_gamma`; *"how does that compare to where SPY closed yesterday?"* invokes `ibkr_history` + `ibkr_quote`.

### "What's on my watchlist, with prices and what I hold?"

→ Claude invokes `ibkr_watch`.

Returns one row per saved symbol with headline price and currency, previous close, absolute and percent change, day range, 52-week range, volume versus average volume, `price_as_of`, stale/session context, and compact stock holding context where the account owns the symbol. Claude should call out stale or closed-market rows instead of treating the values as fresh live prices.

The MCP tool is read-only. Claude can use the symbols for follow-up quote, history, chain, scan, gamma, or regime context, but it cannot add, remove, or clear watchlist entries through MCP. If the user asks only for the saved symbol inventory, Claude passes `{"include_quotes": false}`.

### "Why does SPY look stale at 1am ET?"

→ Claude invokes `ibkr_quote`; if the snapshot is frozen, delayed, or missing live prices, the response may include `session_context`. Claude may also invoke `ibkr_calendar` directly to answer "when does it reopen?"

Returns the official market state for the relevant supported calendar: US cash equities, US listed options regular sessions, or German Xetra cash equities. A US quote at 1am ET will normally say the regular session is closed and show the next 09:30 ET open; a US holiday shows the holiday reason and the next known open. For example, Whit Monday 2026 is closed for US equities because it is Memorial Day, while Xetra is open.

### "Are SPY dealers supporting or amplifying today's moves?"

→ Claude invokes `ibkr_gamma` (default scope = combined SPY+SPX).

Returns the signed zero-gamma price level, the dealer book's current sign (`positive` = long-gamma = stabilizing; `negative` = short-gamma = amplifying), the regime-agreement classifier between SPY and SPX (`agree:long-gamma` / `agree:short-gamma` / `agree:transition-gamma` / `disagree`), and the magnitude view via `gamma_total_abs` and `top_strikes`.

The important diagnostic is **`disagree`**: one book stabilizing while the other amplifies, indicating institutional/retail positioning divergence. Claude usually flags this prominently.

Always read `quality.rankability` before treating gamma as a market-structure signal. `rankable` means the read is fresh and covered enough; `context_only` is awareness-only; `blocked` and `unavailable` are data-quality blockers.

Do not treat missing 0DTE alone as a gamma no-vote. With healthy SPX 1-7DTE and
term coverage the result stays rankable, disclosing the missing bucket in
`quality.coverage` and `warning_details`. After the expiring SPXW series closes,
0DTE can be absent while the broader SPX surface is still usable.

When no serveable result exists, `ibkr_gamma` kicks a multi-minute background compute and returns `status: "computing"` with an ETA. During options RTH, a served result refreshes behind the last-good value after 15 minutes. See [Sensors → Gamma](../understand/sensors.md#gamma) and [Concepts → Gamma](../understand/concepts.md#gamma).

### "Find me top S&P 500 names trading above their 50-day moving average."

→ Claude invokes `ibkr_breadth` to get the index-wide reading, then `ibkr_scan` with the `top-movers` preset (or an ad-hoc scan) to return specific names.

Returns the % of S&P names above their 50-DMA (the tactical signal) and per-row scanner output enriched with last / prev_close / change_pct / volume / IV. Claude typically pairs the breadth context ("market-wide reading: 54% above 50-DMA, healthy") with the specific names that match the scan. For follow-up questions like *"show me daily bars for AAPL"*, Claude chains to `ibkr_history`. See [Concepts → Breadth](../understand/concepts.md#breadth).

### "Preview buying 10 AAPL shares."

→ Claude invokes `ibkr_trading_status`, then `ibkr_order_preview` only if the local preview gate is ready.

Returns a draft order, quote inputs, position impact, notional, warnings, and preview-token fields. `token_minted` means the local daemon created a preview artifact. `submit_eligible` means broker WhatIf accepted the exact draft and a future write path could consider the token. If broker WhatIf is unavailable or rejected, `token_minted` can still be true while `submit_eligible` and compatibility field `executable` are false. The preview itself does not place, modify, cancel, or transmit any broker order.

### "Why is my AAPL stop showing red?"

→ Claude invokes `ibkr_orders_open` and `ibkr_positions`, then explains the mismatch.

A protective stop that no longer matches its position (a partial sale in TWS is the usual cause) is classified critical, with the consequence stated plainly: triggering would close the shares still held and open the excess in the opposite direction.

The fix lives on the paired device. The app offers one guided action that reduces the stop to the held quantity through the normal preview and confirm steps, and the daemon re-checks the live position at both steps.

The order journal behind these reads reconciles itself against the broker's open-order list after each reconnect and every 30 minutes, so a cancel that happened while the daemon was offline closes its row as `closed_reconciled` instead of lingering as a stale open order.

## What Claude can't do here

The MCP interface intentionally has no trade-execution tool. Claude can:

- ✅ tell you what you own
- ✅ read your local saved-symbol watchlist
- ✅ tell you the market state
- ✅ size a trade (`ibkr_size`: pure math against your NLV, never proposes an order)
- ✅ preview a locally gated stock/ETF LMT draft without broker submission
- ❌ place an order
- ❌ cancel an order
- ❌ modify a position

If you ask Claude to "buy 100 shares of AAPL," it can preview a non-submitting draft only if you explicitly ask for preview. It cannot submit that order, and won't try to. This is a hard architectural boundary: the bundled daemon does not expose broker-write paths to MCP, regardless of what Claude asks.

Streaming quote resources are separate from tools. MCP clients discover the `ibkr://quote/{symbol}` template via `resources/templates/list`, then read one snapshot or subscribe for coalesced tick frames that stop when the client unsubscribes or closes the MCP session. The [MCP resources reference](../reference/mcp-resources.md) has the exact method names and payload shapes.

Other things outside the scope today:

- **Option streaming** (continuous option contract ticks). Option snapshots are available through chains and option quotes, but the MCP streaming resource is stock/ETF only.
- **Non-equity asset classes** (futures, FX spot, crypto, bonds). Equity, ETF, Xetra cash-equity, and regular-session US listed-options calendars are covered; everything else is out of scope or partial context today.
- **Other indices' breadth or constituents** (NDX, RUT, sector-specific). S&P 500 only.

## Tips for getting good answers

- **Ask the question, don't name the tool.** "How does my portfolio look?" works better than "Run ibkr_positions." Claude picks the right tool based on the question; naming the tool just adds friction.
- **Chain follow-ups freely.** Each tool call is cheap (cached when possible). "And what about gamma for those?" or "How did that look yesterday?" generate natural follow-up tool calls.
- **For the dashboard, ask "how does the market regime look?"** It triggers `ibkr_regime`, which returns the eight-row dashboard in one call. Faster than asking about each indicator separately.
- **For scheduled stress checks, ask for the canary.** "How does market weather interact with my portfolio right now?" triggers `ibkr_canary`, which returns the whole decision surface plus evidence rows in one call, so the assistant never composes its own escalation ladder.
- **For sizing, give Claude the full plan.** "I want to enter AAPL at 180 with a stop at 175 and a target at 195, risking 1% of NLV" lets `ibkr_size` return the R-multiple, breakeven win rate, and share count in one round-trip.

## Reference

- [MCP tools reference](../reference/mcp-tools.md): auto-generated table of every tool, parameters, descriptions.
- [MCP resources reference](../reference/mcp-resources.md): streaming stock/ETF quote resource semantics.
- [Concepts](../understand/concepts.md): the mental model for regime / gamma / breadth.
- [Updating](../start/updating.md): keeping the binary + constituent list current.
- [Model Context Protocol spec](https://modelcontextprotocol.io/): the upstream protocol.
