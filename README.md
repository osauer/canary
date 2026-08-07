# Canary - IBKR MCP server for TWS and IB Gateway

[![ci](https://github.com/osauer/canary/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/osauer/canary/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/osauer/canary?display_name=tag&sort=semver)](https://github.com/osauer/canary/releases/latest)
[![go.mod](https://img.shields.io/github/go-mod/go-version/osauer/canary)](go.mod)
[![Go reference](https://pkg.go.dev/badge/github.com/osauer/canary/v2.svg)](https://pkg.go.dev/github.com/osauer/canary/v2)
[![license](https://img.shields.io/github/license/osauer/canary)](LICENSE)

**[Documentation](https://osauer.dev/canary/docs/)** · [MCP tools](docs/docs/reference/mcp-tools.md) · [MCP resources](docs/docs/reference/mcp-resources.md) · [Configuration](docs/docs/reference/config.md) · [Sensors](docs/docs/understand/sensors.md) · [Rulebook](internal-docs/design/trading-rulebook.md) · [Trading policy](docs/docs/understand/policy.md) · [Storage](docs/docs/internals/storage.md) · [Architecture](docs/docs/internals/architecture.md) · [Platform settings](internal-docs/design/platform-settings.md) · [Working with agents](docs/docs/operate/agents.md) · [Mobile app](web/app/README.md)

**A local command line, MCP server, and risk desk for Interactive Brokers.**

`canary` turns your local IB Gateway or TWS session into structured account and market context for the terminal, Claude Desktop, Claude Code, Cursor, Continue, Zed, and other MCP hosts. It is the local `canary mcp` TWS bridge for portfolio review, exposure mapping, options diagnostics, market-regime checks, scanner-driven research, watchlist monitoring, and position-sizing math.

For MCP users, `canary mcp` exposes the same typed reads as the command line, including Canary's assembled daily brief. The bundled MCP surface cannot place, modify, cancel, or transmit broker orders; it can analyze the book, size plans, and draft preview-only stock/ETF limit orders.

Use it from a shell:

```sh
canary status
canary positions --by underlying
canary regime
canary stress
canary market-events --symbol GME --json
canary watch IBM --add
canary watch
canary calendar --market us --date 2026-05-25
canary quote SPY --watch
canary size --symbol AAPL --entry 207.50 --stop 202.50 --risk-pct 1
canary settings show
```

Or connect it to Claude Desktop, Claude Code, Cursor, Continue, Zed, or any MCP host and ask:

> "What's in my IBKR account?"
>
> "Review my portfolio and rank the risks I should look at today."
>
> "Show my AAPL exposure, including option deltas."
>
> "How does the market regime look today?"
>
> "Should I hold, watch, de-lever, or liquidate risk?"
>
> "Is Xetra open on Whit Monday?"
>
> "If I buy 100 MSFT at 418 with a stop at 408, what's my EUR risk?"

Your account data stays on the machine running IB Gateway or TWS unless you choose to send it to an MCP host. The project ships as one Go binary with a CLI, a local MCP server, and a Go library. No Python runtime, Java runtime, or hosted service is required.

**Contents** — [Install](#install) · [What you get](#what-you-get) · [Pick your path](#pick-your-path) · [How it works](#how-it-works) · [Configure](#configure) · [Safety](#safety) · [Other install paths](#other-install-paths) · [Troubleshooting](#troubleshooting)

## Install

**Prerequisites.** A running [IB Gateway](https://www.interactivebrokers.com/en/trading/ibgateway-stable.php) 10.37+ or TWS (paper or live) on the same machine. Auto-discovered on the four standard ports. An **IBKR Pro** account (IBKR Lite cannot use the TWS API).

### Claude Desktop

Download the latest MCP Bundle:

<https://github.com/osauer/canary/releases/latest/download/canary.mcpb>

Open the `.mcpb` file with Claude Desktop, drag it into Claude Desktop, or use Settings -> Extensions -> Advanced settings -> Install Extension. Quit Claude completely and relaunch it after installation.

The MCPB bundles the `canary` binary for macOS and Linux, runs it locally through stdio, and does not require a separate shell install. Windows Claude Desktop is not supported because `canary` has no native Windows daemon; WSL works through the shell install path below.

### Shell, Cursor, Continue, Zed, and generic MCP hosts

```sh
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh
canary setup claude-desktop
```

The installer downloads the release for your OS and architecture, verifies the checksum, installs `canary` in `~/.local/bin`, and adds that directory to your shell rc when needed. On macOS, it also clears Gatekeeper quarantine.

`canary setup claude-desktop` writes the legacy MCP server entry to Claude Desktop. Prefer the MCPB path above for Claude Desktop unless you specifically want one shared shell-managed binary. Skip the setup command if you only want the shell tool.

For v1.0.0+ releases, the installer, `canary update`, and the MCPB release asset are covered by the signed `SHA256SUMS` file. The MCP Registry metadata also carries the MCPB file SHA-256. [Other install paths.](#other-install-paths)

## What you get

- **Account and positions.** Net liquidation, buying power, cash, margin, daily P&L, positions, option Greeks, per-underlying grouping, and portfolio-level delta/theta/gamma/vega rollups. Every result names one account and says whether its data is current and complete. A missing value stays missing instead of looking like zero, and an unresolved multi-account login is refused instead of combined. Multi-currency accounts include FX exposure.
- **Quotes and history.** Snapshot quotes, coalesced stock/ETF streaming, daily OHLCV bars, previous close, day change, and data freshness (`live`, `frozen`, `delayed`, `delayed-frozen`).
- **Official market calendars.** US cash equities, US listed options regular sessions, and German Xetra cash equities with holidays, early closes, next open/close, and quote `session_context` when calendar state explains stale or missing data.
- **Local watchlist.** Add/remove/clear symbols offline, list them as JSON, show an enriched quote monitor with price, currency, changes, ranges, volume, timestamps, and held-stock context, or poll the saved list with `canary watch --watch`.
- **Options.** Expiry lists with ATM IV and implied move, strike grids with call/put quotes, deltas, and open interest. Option snapshots are supported; option streaming is not exposed.
- **Scanners.** Built-in market scans for movers, losers, unusual volume, gaps, high IV rank, and option volume. Agents can also compose ad-hoc scans without writing config.
- **Position sizing.** Fixed-fractional sizing against live NLV, with optional target, R-multiple, and breakeven win rate. Pure math; never an order ticket.
- **Market breadth.** S&P 500 participation from constituent daily bars: percent above 50-DMA, percent above 200-DMA, and fresh 52-week highs/lows. A fresh cache is instant; first-ever cold start can take about an hour because of IBKR pacing.
- **Dealer gamma.** Production-ready SPX/SPXW-canonical zero-gamma and concentration view, with SPY used as corroborating context when its option surface is usable. A fresh, rankable SPX result is the stable headline signal; SPY-only is a labeled proxy. Treat the signed level as a regime hint, not a precise trading level.
- **Risk regime.** One call returns the eight-row dashboard: VIX term structure, VVIX, HYG/SPY divergence, HY/IG OAS, funding spread, USD/JPY weekly move, SPX-canonical dealer gamma, and S&P 500 breadth. Heavy rows report `computing` instead of pretending stale data is fresh.
- **Portfolio stress.** `canary stress` and MCP `canary_stress` produce a stateless `market regime × portfolio shape` monitor with `action`, `market_confirmation`, `portfolio_fit`, `input_health`, planner readiness, stable fingerprints, and supporting `signals[]`. It also emits bounded `portfolio.held_stress[]` rows for material held underlyings when existing positions data shows held-name daily P&L shock, near-expiry held-option delta concentration, or held-name liquidity degradation. Account-only risk stays evidence, not a DEFEND trigger; DEFEND requires confirmed market pressure, vulnerable portfolio fit, and clean enough inputs. Use `canary stress --details` for the full evidence rows.
- **Daily brief.** `canary brief` and MCP `canary_brief` return the daemon's same written Review/Ready briefing: what changed since the last close, what matters before the next one, and which inputs could not be read. The MCP tool is a pure read. It never signs off, stamps an artefact, or advances process state.
- **Market-event flags.** `canary market-events` and MCP `canary_market_events` annotate held or requested stock/ETF symbols with borrow inventory, extreme borrow fee, Nasdaq Reg SHO threshold-list, LULD pause, and regulatory/news halt context. Flags are context and proposal gates: active halt/LULD can block protection proposals, borrow stress can strengthen short buy-to-cover context, and unknown sources remain unknown rather than false. Borrow-fee output also discloses global versus exact-held-short coverage; the narrow TWS `FEE_RATE` fallback remains scale-unverified and cannot create or clear the 50% flag.
- **Protection proposals and order views.** The daemon maintains trailing-stop, theta-hygiene, and risk-reduction proposals with per-row blockers; `canary proposals` and MCP `canary_proposals` read them. Order state is a local journal read (`canary orders open`, `canary orders history`, `canary order status`) that reconciles itself against the broker's open-order list after each reconnect and every 30 minutes, closing rows the broker no longer reports as `closed_reconciled`. A protective stop that no longer matches its position is flagged critical with the exact reduce-to-position quantity. These are reads; acting on any of it stays behind the separate gated order path.
- **Platform settings.** `canary settings show` and MCP `canary_settings` report runtime preferences, trading/build capability, account mode, and compact observed market-data quality with `access`, `source`, and read-only reasons. `features.purge_restore.enabled` controls the workflow/read surface while `purge status` stays readable; it never authorizes broker submission, which is currently unavailable and must be handled manually in TWS.

Every data command supports `--json`. `canary restart --json` is also useful for scripts: it reports whether a daemon was already running, old/new PIDs, whether `--force` was used, the post-start `status.health` snapshot, and any app process it refreshed. Lifecycle commands such as `setup`, `update`, `restart`, `mcp`, and `daemon` are for local operation and transport setup.

For schemas and edge cases, see the [agent skill schema notes](skills/canary/schemas.md), [MCP tools reference](docs/docs/reference/mcp-tools.md), [MCP resources reference](docs/docs/reference/mcp-resources.md), [configuration reference](docs/docs/reference/config.md), and [concept docs](docs/docs/understand/concepts.md).

For ready-to-run prompts, see [examples/canary_portfolio_analysis_prompt.md](examples/canary_portfolio_analysis_prompt.md) for portfolio review, [examples/canary_portfolio_stress_prompt.md](examples/canary_portfolio_stress_prompt.md) for scheduled stress checks, and [examples/canary_fresh_ideas_screen_prompt.md](examples/canary_fresh_ideas_screen_prompt.md) for a fresh-ideas screening session.

## Pick your path

### Claude Desktop, Cursor, Continue, Zed

`canary mcp` starts a local stdio MCP server. MCP hosts can call the same account, watchlist, quote, calendar, position, scanner, sizing, regime, stress, daily-brief, and preview-only order-draft tools that the CLI exposes as JSON. The order preview surface can mint a local non-submitting preview token, but it cannot place, modify, cancel, or transmit broker orders. Watchlist access through MCP can return either the saved symbols or enriched quote rows; local lifecycle verbs such as `setup`, `update`, `restart`, `mcp`, `daemon`, and `version` stay outside the MCP tool set.

The server also exposes quotes for stocks and ETFs as an MCP resource:

- `canary://quote/{symbol}`

`resources/read` returns one snapshot for that URI; `resources/subscribe` delivers coalesced ticks via `notifications/resources/updated` until you `resources/unsubscribe` or close the stdio. The resource shape is documented in [docs/docs/reference/mcp-resources.md](docs/docs/reference/mcp-resources.md).

For Claude Desktop, the recommended install path is the `.mcpb` asset from the latest release. For other clients, paste this into the client's MCP config (path varies):

```json
{
  "mcpServers": {
    "canary": {
      "command": "/ABSOLUTE/PATH/TO/canary",
      "args": ["mcp"]
    }
  }
}
```

The `command` must be the absolute path. `~` is not expanded by `exec` and `$PATH` is not consulted. `which canary` gives you the right value. After upgrading the binary, fully quit and relaunch the client — it caches the spawned server process. MCPB installs carry their own embedded binary; reinstall the new `.mcpb` release to update that path.

`claude.ai` (web) accepts only remote MCP servers and cannot reach a local IB Gateway. Use Desktop.

Logs (macOS, Claude Desktop): `~/Library/Logs/Claude/mcp-server-canary.log`.

### Claude Code

Inside a standalone Claude Code session:

```
/plugin marketplace add osauer/canary
/plugin install canary@canary
```

Or — for **Claude for Mac**'s embedded Claude Code pane, which doesn't expose `/plugin` slash commands — from a regular terminal:

```sh
claude plugin marketplace add osauer/canary
claude plugin install canary@canary
```

The plugin carries a skill, Claude Code MCP server config for `canary mcp`, a `PreToolUse` hook that permits read/preview order commands, blocks shell command chaining around broker-adjacent writes, and refuses broker-write verbs unless the daemon reports a paper or live write-ready trading state (failing closed for broker-adjacent `canary` commands if `jq` is missing from PATH), plus a `SessionStart` hint when the binary isn't installed. The skill's `allowed-tools` pre-allows read and preview-only patterns once the skill activates. For a global allowlist that fires *before* the skill activates, merge `settings/canary.settings.json` into `~/.claude/settings.json` by hand — it is permissions-only: read/preview patterns are allowed, and destructive daemon maintenance carries explicit deny rules while broker writes remain decided by the hook and daemon gates.

**The plugin doesn't ship the binary.** It carries the skill, hooks, MCP launcher config, and manifest — you still need the `canary` binary from [Install](#install). The MCP launcher looks at `CANARY_BIN`, the plugin's local development `bin/canary`, `PATH`, `~/.local/bin/canary`, Homebrew, and `/usr/local/bin/canary`. The binary and plugin have independent release cadences and independent update paths:

```sh
# Binary release (new MCP tool descriptions are baked into the binary):
curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh

# Plugin release (new skill commands, settings, hooks):
claude plugin update canary@canary
```

Restart the host (Claude for Mac, standalone Claude Code session, Cursor, …) after either update so it respawns the MCP server subprocess with the new descriptions and reloads the skill at the next session start.

### The shell

```sh
$ canary account --json | jq '.net_liquidation, .base_currency'
$ canary watch IBM --add
$ canary watch --list --json
$ canary watch --json | jq '.rows[] | {sym: .symbol, price: .price, chg: .change_pct, as_of: .price_as_of}'
$ canary quote AAPL,MSFT --json | jq '.[] | {sym: .symbol, price: .price, chg: .change_pct}'
$ canary quote MBG --market de --json | jq '{sym: .symbol, ccy: .contract.currency, last: .last}'
$ canary calendar --market us-options --date 2026-11-27 --json | jq '.session'
$ canary positions --by underlying --json | jq '.portfolio.effective_delta'
$ canary stress --json | jq '{action, market_confirmation, portfolio_fit, held_stress: .portfolio.held_stress}'
$ canary market-events --symbol GME --json | jq '{flags, source_health, fingerprint}'
$ canary settings show --json | jq '.features.purge_restore.enabled'
$ canary chain NVDA --json | jq '.expiries[] | select(.iv > 0.6)'
$ canary size --symbol AAPL --entry 207.50 --stop 202.50 --risk-pct 1
```

`canary --help` lists subcommands; `canary <cmd> --help` lists flags. `canary status` first if anything looks off.

### Mobile app

`canary app` serves a paired PWA for iPhone-sized checks when you are away from the desk. It has five tabs. Monitor is a fixed grid of instrument windows covering every regime cluster the daemon ranks plus the rulebook, protection, and stress, each printing its reading and the level it would trip at. Brief carries the daemon-composed daily briefing. Alerts is the annunciator log. Orders is a read-only view of the local order journal. Settings holds account and process state and can toggle the purge/restore workflow preference. Broker submission remains unavailable and requires manual TWS action. Start it on the Mac running TWS or IB Gateway, then run `canary app pair` and scan the QR code. `canary app status` is the local operator check for app liveness, daemon alert-producer coverage, and app-owned dispatcher health.

For access away from the LAN without router setup, run `canary app --remote` to use the Cloudflare Worker relay at `remote.osauer.dev`, then run `canary app pair` as usual.

See [web/app/README.md](web/app/README.md) for the short operator notes; the original MVP design is preserved as a historical record in [internal-docs/design/mobile-app-mvp.md](internal-docs/design/mobile-app-mvp.md).

### Go and other agent SDKs

`pkg/ibkr` speaks the TWS API protocol directly:

```go
import "github.com/osauer/canary/v2/pkg/ibkr"

cfg := ibkr.DefaultConfig()    // 127.0.0.1:4001
cfg.Port = 4002                // paper

c := ibkr.NewConnector(&ibkr.ConnectorConfig{
    ServiceName:       "myapp",
    PreferredClientID: 15,
    BaseConfig:        cfg,
})
if err := c.Start(ctx); err != nil { return err }

snap, err := c.RequestAccountSummary(ctx, 5*time.Second)
if err != nil { return err }
if snap.NetLiquidation != nil {
    fmt.Printf("NLV: %.2f %s\n", *snap.NetLiquidation, snap.Currency)
}
```

From Python, TypeScript, or Rust, shell out to the CLI: subprocess in, JSON out. Wrap each `canary <cmd> --json` invocation as a function and register it with your model's tool-call API.

## How it works

`canary` runs local commands against one background daemon.

When you run a CLI command or an MCP tool, it connects to the daemon over a Unix socket. The daemon keeps the IB Gateway or TWS connection open, caches contract details, manages quote subscriptions, and returns JSON responses. It starts on first use and exits after 15 minutes of inactivity unless you run it in the foreground.

```text
CLI or MCP host -> local canary daemon -> IB Gateway or TWS -> your account data
```

Use `canary restart` after upgrading, changing daemon-loaded config, or when you want to clear stale gateway connection state. It sends SIGTERM, waits for cleanup, starts a fresh daemon, reports the new process, then refreshes any already-running `canary app` host while preserving app flags such as `--remote`. If no daemon was running, it starts one and says so; if no app host was running, it leaves the app stopped. `canary restart --force` escalates to SIGKILL only after the graceful timeout. This restarts the shared daemon used by CLI and MCP tool calls; it does not restart the `canary mcp` stdio process itself, which is owned by the MCP host. Fully relaunch the host when you need it to respawn MCP from a new binary or bundle.

`canary stop` puts the local processes down without a kill command. It stops the app first and the daemon second, so a running app cannot start a replacement daemon mid-stop; `--app` or `--daemon` stops just one. Before stopping a daemon that still has work in flight it names that work and asks, and `--yes` answers for scripts. `--force` escalates a process that ignored SIGTERM to SIGKILL once `--timeout` has passed. An app supervised by launchd is unloaded rather than signalled, because KeepAlive would otherwise restart it, and the reply says how to bring it back. MCP servers are counted and named with their AI client but never signalled: each belongs to the client that started it and exits with it.

This means your shell, Claude Desktop, Claude Code, Cursor, and other MCP clients can share one IBKR connection and one client ID. Tool calls stay fast because the gateway session is already open.

`pkg/ibkr` is a clean-room Go implementation of the TWS protocol. Unrestricted order methods are disabled in default builds; the separate trading build and the narrow all-build paper-order wrappers have distinct gates described in [docs/docs/internals/protocol.md](docs/docs/internals/protocol.md). Public package documentation lives in [pkg/ibkr/doc.go](pkg/ibkr/doc.go).

## Configure

For normal read-only use, no config file is required. The daemon TCP-probes `4001` (Gateway live), `4002` (Gateway paper), `7496` (TWS live), `7497` (TWS paper), picks the first responder, and falls over to alternates if the first one accepts TCP but never completes the handshake. The account is auto-detected via `managedAccounts`. Default client ID is `15`.

**TOML config** (`config.toml`) means "active local overrides." Create it only when you want to pin something: anything present is binding, anything omitted stays auto-detected, and unknown keys fail at startup with a message that names them. The safety-pinned pre-rename namespace remains `$XDG_CONFIG_HOME/ibkr/config.toml`, falling back to `~/.config/ibkr/config.toml`, so an upgrade cannot create a second configuration authority. For example, this read-only config pins TWS live and leaves everything else automatic:

```toml
[gateway]
port = 7496
```

Every section and key — `[gateway]`, `[daemon]`, `[trading]`, `[rulebook]`, `[auto_trade]`, `[opportunities]`, `[spx]`, `[scans.<name>]` — is enumerated with types, defaults, and semantics in the generated [configuration reference](docs/docs/reference/config.md), alongside the public `CANARY_*` variables and the broker-wire `IBKR_*` diagnostics. `canary status` shows what the daemon ended up using and where each value came from (`pinned` or `discovered`).

**Runtime platform preferences** are daemon-owned, live in the safety-pinned `$XDG_STATE_HOME/ibkr/daemon.db`, and change without a restart. Feature toggles and rulebook earnings overrides are available through `canary settings set`, the SPA Settings tab, or `PATCH /api/settings`; the `trading.freeze` brake and experimental trading-limit overrides require `canary settings set` from an interactive human terminal. The writable keys are listed in the [configuration reference](docs/docs/reference/config.md); ownership and semantics live in the [platform-settings design](internal-docs/design/platform-settings.md).

**Trading policies** turn the desk's risk decisions into repeatable checks. The personal risk constitution stays at `~/.config/ibkr/policies/risk-policy.toml`; this safety-pinned path prevents a rename from hiding an approved policy. It has no embedded default: missing material decisions remain `unapproved`. Protection proposals (`protection-policy.toml`) and option-exercise opportunities (`opportunity-policy.toml`) do have conservative embedded defaults, printable with `canary policy default <protection|opportunity>`. The [trading-policy reference](docs/docs/understand/policy.md) explains who decides, what is advisory today, how controls change, and why local policy records are not broker execution evidence; every editable engine key remains enumerated in the generated [configuration reference](docs/docs/reference/config.md).

**Trading config is opt-in and experimental.** Stable `canary` releases are read-only. Trading builds, when built or published separately, are experimental and provided as-is for explicit operator testing. Keep trading config inactive as `~/.config/ibkr/config.toml.trading`; it has no effect with that suffix. To activate it, a human or explicitly instructed local agent removes the `.trading` suffix so the file becomes `~/.config/ibkr/config.toml`, verifies the pinned account and endpoint, then runs `canary restart`. The example template lives at [examples/config.toml.trading](examples/config.toml.trading).

References:

- [Configuration reference](docs/docs/reference/config.md) for TOML sections and `IBKR_*` environment variables.
- [Trading policy](docs/docs/understand/policy.md) for who decides risk boundaries, how Canary evaluates them, what is advisory today, and which actions still require a human.
- [Trading Rulebook](internal-docs/design/trading-rulebook.md) for the compiled advisory
  discipline model, its evidence contract, ownership, freshness, and limits.
- [Storage](docs/docs/internals/storage.md) for how the daemon preserves state and evidence with SQLite, including data relationships, query boundaries, durability, recovery, and current limits.
- [Sensors](docs/docs/understand/sensors.md) for Gamma, Regime, Stress, Rulebook, market-event authority, freshness, last-good behavior, and fail-closed checks.
- [Experimental trading config](docs/docs/operate/orders.md) for the inactive `config.toml.trading` pattern and release-channel expectations.
- [Concepts](docs/docs/understand/concepts.md) for breadth, gamma, and regime interpretation.
- [Working with agents](docs/docs/operate/agents.md) for Claude and MCP workflows.
- [Packaging and distribution](docs/docs/internals/packaging.md) for packaging notes.
- [Privacy](PRIVACY.md) for data locality and local files.

### Adding scanners

Two paths, depending on who's calling:

**Humans — add a preset to `config.toml`.** Use this when you want a stable shorthand you'll call by name:

```toml
[scans.tech-gainers]
type     = "TOP_PERC_GAIN"
exchange = "STK.NASDAQ"
limit    = 25
```

Then `canary scan tech-gainers`. **Caveat:** writing **any** `[scans.*]` block makes the seven built-in defaults disappear — the `[scans]` table is replace-not-merge. Copy the defaults from [internal/config/config.go](internal/config/config.go) into your file if you want to keep them. Run `canary restart` for new presets to be visible.

**Agents — use the ad-hoc form, no config write needed:**

```
canary scan --type TOP_PERC_GAIN --exchange STK.NASDAQ --limit 25 --json
canary scan --type TOP_PERC_GAIN --exchange STK.EU.IBIS --instrument STOCK.EU --limit 25 --json
```

Ad-hoc rows are capped at 50 (vs. preset's user-set limit) to keep an agent from accidentally pulling thousands.

**Finding the right `scanCode` and `locationCode`.** The TWS Market Scanner UI hides these strings behind human labels. Dump your gateway's actual catalog with:

```
canary scan params --instrument STK [--json]
```

The catalog varies by gateway version and by your market-data subscriptions — `scanCode`s like `HIGH_OPT_IMP_VOLAT_OVER_HIST` require US options data, and European stock locations often require `--instrument STOCK.EU` instead of the US default `STK`. `--instrument STK` narrows to US stock scans; omit for everything. Add `--raw` to get the full XML (~200 KB–2 MB) if you need a less-common field. There's no need to memorize the values — the catalog is the source of truth.

## Safety

`canary` is the stable no-broker-write binary line. It exposes read tools plus preview-only order drafts; preview tokens are local artifacts and are not broker orders. Experimental trading builds are separate, as-is, and not part of the stable update or MCP marketplace path. The Go library is not itself a complete no-write sandbox: its default build disables unrestricted order and exercise methods but retains narrowly paper-gated order wrappers for daemon use. The stable binary's no-write posture is enforced across these layers:

1. Default `pkg/ibkr` builds return `ErrTradingDisabled` from unrestricted place/cancel and option-exercise methods before any wire write. Raw unrestricted methods require `-tags trading`; the all-build paper wrappers validate a concrete paper account and connection but do not grant application authority.
2. The daemon's write-handler dispatch returns `ErrTradingDisabled` for place/cancel RPCs ([internal/daemon/trading_disabled.go](internal/daemon/trading_disabled.go)); preview can mint a token but reports `submit_eligible=false` unless broker WhatIf is accepted.
3. The bundled [settings/canary.settings.json](settings/canary.settings.json) allowlists read/preview `canary` patterns only and explicitly denies destructive daemon maintenance. Broker writes are not hard-denied there; the project hook and daemon gates decide them.
4. The plugin's `PreToolUse` hook blocks shell chaining around broker-adjacent writes and refuses broker-write patterns unless the daemon reports a paper or live write-ready trading state, failing closed for broker-adjacent `canary` commands if `jq` is missing from PATH.
5. A unit test in `internal/mcp` allows only preview/read-model order tools and refuses unallowlisted order/trade/cancel/submit/place tool names.

Stable releases keep broker-write RPC handlers unavailable. Preview-only CLI, JSON, and MCP additions may appear as documented minor additions.

## Other install paths

- **`go install`**: `go install github.com/osauer/canary/v2/cmd/canary@latest`. Requires Go 1.26+.
- **Claude Desktop MCPB**: download `canary.mcpb` from the latest [release](https://github.com/osauer/canary/releases/latest/download/canary.mcpb) and open it with Claude Desktop. The release also publishes `canary-vX.Y.Z.mcpb` for registry integrity and reproducible manual verification.
- **Different install dir**: `CANARY_INSTALL_DIR=/usr/local/bin sh install.sh`. The installer won't touch your shell rc when you override; manage PATH yourself.
- **Inspect the installer first**: `curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh -o install.sh && less install.sh && sh install.sh`.
- **Manual download**: pick a tarball from the latest [release](https://github.com/osauer/canary/releases/latest). Each contains `canary` plus `LICENSE` and `README.md`. Verify `SHA256SUMS.asc` against the release-signing key, then verify the tarball against `SHA256SUMS`; see [SECURITY.md](SECURITY.md#release-integrity-v100).
- **Local build**: `git clone … && make install`.
- **Self-update**: `canary update` fetches the next stable release, verifies the PGP signature on `SHA256SUMS`, SHA-verifies the tarball, and atomically replaces `~/.local/bin/canary`. Prior bytes exist only in transaction staging and are deleted after publication; forward-only daemon state migrations make executable rollback unsafe. See [docs/docs/start/updating.md](docs/docs/start/updating.md) for headless flag matrix, daemon-restart semantics, `canary restart`, and how the runtime S&P-500 constituent refresh works.

Windows is not supported — the daemon uses Unix-only primitives (setsid, flock, AF_UNIX sockets). WSL works.

## Testing

```sh
make commit-check # fast intermediate gate for the exact staged tree
make check      # gofmt + go vet + staticcheck + govulncheck + plugin/parity checks
make test       # check + unit tests + hermetic daemon/CLI lifecycle integration
make test-integration-live # strict live-Gateway integration; absence fails
```

`make commit-check` is for frequent checkpoint commits. It classifies the
staged paths, verifies that exact tree in an isolated worktree, and falls back
to full `make check` for unknown or authority-sensitive changes. Its exact-tree
cache is never final, CI, or release evidence.

`make check` is the binding gate. It fails on stdlib vulnerabilities, so an outdated Go toolchain is a build failure. The lint/vuln tools are pinned in `go.mod` and run via `go tool`, so CI and local checks use the same versions. The gate also checks that MCP tools, streaming resources, generated references, and plugin metadata stay aligned with the CLI commands.

`make test` runs the hermetic lifecycle inventory under `test/integration/`
without probing a Gateway. The live inventory is deliberately separate:
`IBKR_TEST_PORT=4002 make test-integration-live` requires a reachable,
fully-handshaken Gateway and fails rather than reporting green skips. A direct
`go test ./...` retains optional-live behavior for ordinary Go tooling.

No mock daemons. `pkg/ibkr/protocoltest/` is a wire-level encoder/decoder spec used by unit tests. Behavioural verification runs against a real IB Gateway.

## Troubleshooting

**"gateway not responding to TWS handshake within 12s".** The gateway accepts your TCP connection but never replies to the v100 handshake. Almost always the API socket is disabled. Launch TWS once, accept "Enable ActiveX and Socket Clients", quit TWS, restart Gateway. The flag carries over via shared `~/Jts/<userdir>/ibg.xml`. It also silently un-ticks itself when more than one of TWS / IB Gateway / IBKR Desktop is launched against the same login — if it keeps coming back, run only one of them.

**"no IBKR listener found on 127.0.0.1 ports ...".** Auto-discovery probed all four standard ports and got nothing. The error message tells you which case you're in: if TWS / IB Gateway / IBKR Desktop is running, the API socket is closed (checkbox unchecked, login pending, or non-default socket port — pin it in `[gateway]`); if nothing is running, just start one and the daemon reconnects automatically. On a non-loopback host, set `host = "192.168.x.y"` explicitly — auto-discovery only probes loopback.

**"none of N discovered endpoint(s) completed TWS handshake".** Both Gateway and TWS are running, both accept TCP, but neither completes the API handshake. Usually a stale Gateway window from earlier in the day plus a freshly logged-in TWS. The status output names every endpoint that was tried. Quit the one you don't need.

**`daemon socket did not appear`.** The daemon crashed during startup. Check the safety-pinned `~/.local/state/ibkr/ibkr-daemon.log`. Common causes: gateway not running, configured `client_id` already in use, wrong port. Orphaned sockets from crashed daemons are handled automatically.

**Quotes time out.** Strict live entitlements, market closed. The daemon defaults to `SetMarketDataType(2)` (frozen), which returns the last-known price; with `live` only, snapshots stay empty out of trading hours. Loosen the gateway's market-data permissions.

**`use of closed network connection` during handshake.** IB Gateway rate-limits fast handshake retries. Wait ~30 seconds before restarting.

**CLI vs daemon version skew warning.** Run `canary restart`. It stops the old daemon and starts a new one from the current binary.

**Capturing the wire protocol for diagnostics.** Set `IBKR_WIRE_INTERCEPTOR=1` to enable the in-process recorder; pair with `IBKR_WIRE_LOG_PATH=/path/to/wire.jsonl` to also persist every frame as JSON-lines. `IBKR_WIRE_RING_SIZE=N` sizes the in-memory ring (default 256). For raw bytes, `IBKR_PACKET_LOG_TEMPLATE=/path/to/packets.bin` enables the lower-level packet logger. All four are off by default. Captured frames carry account-sensitive data — see [SECURITY.md §Diagnostic data sensitivity](SECURITY.md#diagnostic-data-sensitivity) before sharing logs.

## Disclaimer & trademarks

This project is an **independent, third-party client** for Interactive Brokers' [publicly documented TWS API](https://interactivebrokers.github.io/). It is not built, endorsed, sponsored, or supported by Interactive Brokers Group, Inc., or any of its affiliates.

- "Interactive Brokers", "IBKR", "TWS", and "IB Gateway" are trademarks or registered trademarks of Interactive Brokers Group, Inc. or its affiliates. They are used here nominatively, solely to identify the brokerage and the API this project connects to.
- `pkg/ibkr` is a clean-room Go re-implementation of the TWS wire protocol. **No code, libraries, or jars distributed by Interactive Brokers are included or redistributed in this project.**
- This project does not redistribute IBKR market data. Data is read from a gateway you run locally, against your own account. The daemon keeps local operational caches and journals on your machine, and data leaves it only through features you explicitly enable — the remote app relay, Web Push — or an MCP host you choose to connect. [PRIVACY.md](PRIVACY.md) is the authoritative list.
- Connecting to IBKR via the TWS API requires an **IBKR Pro** account; IBKR Lite does not include API access.
- Nothing here is investment advice. Use at your own risk; the MIT license's AS IS clause applies in full.

If you are Interactive Brokers and have a concern with anything in this repository, please open a GitHub issue and we will respond promptly.

## License

MIT. See [LICENSE](LICENSE).
