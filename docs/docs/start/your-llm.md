# Connect your Interactive Brokers account to the LLM you already use

Updated: 2026-11-05 08:40 CET

Canary doesn't come with a model and doesn't need one. It runs a small server on your machine that any MCP host on that machine can start, and the model is whichever one the host is set up with. Once connected, you can ask about your own positions in that app.

## What you need

- Canary installed and connected to your gateway. [Install and first run](install.md) covers this.
- A desktop or terminal app on the same machine that supports MCP over stdio.

A chat in a browser tab can't do this, because the host has to run on the same machine as the server.

## Claude Desktop and Claude Code

**Claude Desktop.** Open [`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb) with Claude Desktop. The [Claude Desktop install page](../../claude-desktop-interactive-brokers/index.html) has the click-by-click version.

**Claude Code.** Run these in a terminal. The plugin doesn't carry the `canary` binary. Install that too, or the plugin installs cleanly and its tools won't work.

```sh
claude plugin marketplace add osauer/canary
claude plugin install canary@canary
```

Claude may not be offered where you are: in November 2026 Anthropic's [list of supported regions](https://www.anthropic.com/supported-countries) did not include Hong Kong, Macau or mainland China. The CLI and paired app work without it, and other MCP hosts can use the same tools with a model that is available where you are.

## Cursor, Continue, Zed and other hosts

Add this to the host's MCP configuration, then fully quit and reopen the host:

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

`which canary` prints the path. It has to be absolute, because the host doesn't search your `PATH`. The file's location and the surrounding key names differ per host, so check its MCP documentation; `command` and `args` are what it needs. [Connect an MCP host](hosts.md) has the rest, including how to tell a wiring fault from a gateway fault.

## Where your account data goes

Canary has no sign-up and sends no telemetry, and the server runs on your machine. Tool results go to your host and to whichever model it uses, so with a hosted model your positions and balances reach that provider under its terms. A model on your own machine means no model provider receives them, but check what the host itself sends. Canary also asks Nasdaq for earnings dates by ticker, which can show that you hold a name. [Privacy](https://github.com/osauer/canary/blob/main/PRIVACY.md) lists every connection.

## What to ask

- "What needs my attention today?" Canary answers with the daily brief and names anything stale or missing.
- "Which of my positions break my limits?" This reads the rulebook against your limits, and says so if they are still Canary's unreviewed defaults.
- "What's my exposure by underlying?" This reads your positions grouped by underlying, as `canary positions --by underlying` shows them.
- "Is Hong Kong open now, and when does the lunch break end?" This reads the official exchange calendar.
- "How did prices move after my trades this past year?" Canary Edge answers from your own history once [broker reporting](reporting.md) is set up. It describes the past and doesn't forecast.

## Orders

The MCP tools only read. None of them can preview or send an order, in any build. [Agentic trading](agentic-trading.md) explains what an agent can do and how an order gets sent.
