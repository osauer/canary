# Privacy

Last reviewed: 2026-08-09

`canary` is a local Interactive Brokers client. The default/read-only binary and
the MCP surface cannot submit broker orders. A separately published
trading-capable binary, local trading builds, and the paired PWA can expose
gated place/modify/cancel or protection-action paths when the operator enables
trading explicitly. The project does not collect telemetry or send account data
to the maintainer.

## What data the tool can access

When you run `canary`, it talks to the IB Gateway or TWS instance that you run locally. Depending on the command, the local daemon can read:

- account identifiers, balances, cash, margin, buying power, and P&L
- positions, market values, option Greeks, option chains, open interest, and quotes
- historical daily bars, quotes, option data, and market-event inputs used by
  daemon-owned sensors
- local configuration paths and daemon health information

Trading is disabled by default. In a trading-capable build, broker writes remain
behind the mode and route pins, preview/WhatIf eligibility, runtime freeze,
journal, and daemon authorization described in
[SECURITY.md](./SECURITY.md#agent-origin-write-gating-trading-builds). The MCP
surface remains read-oriented; agent writes use the gated CLI.

## Where data goes

By default, data stays on your machine:

- CLI output is written to your terminal.
- MCP tool results are sent over stdio to the local MCP client that launched `canary mcp`.
- The daemon listens on a local Unix-domain socket, not a public TCP port.
- The daemon talks to IB Gateway or TWS over the configured gateway host, normally loopback.
- The paired PWA can be used over the LAN. If the user explicitly enables the
  remote relay or Web Push, the relevant app traffic or redacted alert payloads
  pass through those configured third-party services.

If you paste output into a chat, connect the MCP server through a remote tunnel, or use a third-party MCP host, that host may receive the account and market data returned by the local tool. Review that host's privacy and retention policy before enabling access.

## Local files

`canary` may write local operational files:

The on-disk namespace deliberately remains `ibkr`. Reusing the existing config,
state, socket, and supervisor identities prevents an upgrade from starting a
second daemon authority or silently abandoning safety state. It is an internal
continuity contract, not the product or command name.

- daemon logs under `~/.local/state/ibkr/` by default
- updater and transport scratch under `~/.cache/ibkr/`; daemon business state,
  market observations, membership, regime, breadth, and gamma authority live in
  `daemon.db` or refreshable memory rather than cache files
- optional user configuration under `~/.config/ibkr/config.toml`
- daemon state, orders, proposals, opportunities, decisions, observations, and
  statement projections in `~/.local/state/ibkr/daemon.db` by default
- paired-app device, alert, delivery, and relay state under the configured app
  state directory
- optional diagnostic wire logs only when explicitly enabled with the diagnostic environment variables documented in [SECURITY.md](./SECURITY.md#diagnostic-data-sensitivity)

These files can contain account-sensitive information. Protect your local user account and avoid sharing logs without redaction.

## Network access

Normal operation requires access to your configured IB Gateway or TWS API
socket. The project makes these additional outbound connections:

- `install.sh` and `canary update` contact GitHub Releases when the user asks
  to install or update. The paired app also checks GitHub Releases after app
  bootstrap, cached for six hours, so it can show a newer stable version. These
  requests disclose release/platform metadata, not broker account data.
- The rulebook's automatic earnings refresh queries Nasdaq once per held or
  requested underlying that needs current evidence. Each request discloses the
  normalized ticker symbol, so a third party can infer that the name is being
  evaluated and it may be a current holding. The request does not include
  account identifiers, quantities, balances, cost basis, or P&L. The
  subscription-gated Wall Street Horizon corroboration path remains inside the
  configured IBKR connection.
- Market-event refreshes download Nasdaq Reg SHO and trading-halt feeds and
  IBKR's public short-stock inventory. Those are whole-market files or feeds;
  held symbols are matched locally and are not sent as query parameters.
- Regime refreshes download public series from FRED, CBOE, the Federal Reserve,
  and the US Treasury. The requests contain no portfolio or account fields.
- The S&P 500 constituent refresher downloads Wikipedia's public constituent
  list on its schedule unless disabled in config. Holdings are matched locally.
- Configured IBKR Flex reconciliation contacts IBKR's Flex Statement service
  using the user's query credential and receives the requested broker report.
  That report is exchanged with IBKR, not with this project's maintainer.
- When the user enables `canary app --remote`, authenticated app HTTP and SSE
  traffic passes through the configured Cloudflare relay. This can include
  portfolio/account fields returned to the paired phone; the relay does not own
  the local device grant, daemon credentials, or broker credentials.
- When Web Push is enabled, the browser's push service receives a redacted
  alert payload plus normal delivery metadata. Web Push does not use the remote
  relay.

Like any HTTPS/FTP client, these connections also disclose ordinary network
metadata such as source IP, timing, user agent, and the requested endpoint to
the destination and intervening infrastructure.

## Deleting data

To remove local runtime state, quit MCP clients, stop the daemon, and delete the relevant local directories:

```sh
pkill -f 'canary daemon'
rm -rf ~/.cache/ibkr
rm -rf ~/.local/state/ibkr
```

Delete `~/.config/ibkr/config.toml` if you also want to remove local configuration.

## Contact

Report security issues privately through GitHub Private Vulnerability Reporting as described in [SECURITY.md](./SECURITY.md). For privacy or data-handling questions, open a GitHub issue without account-specific details.
