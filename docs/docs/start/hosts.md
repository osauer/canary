# Connect an MCP host

Updated: 2026-08-09

`canary mcp` is a local MCP server that speaks JSON-RPC over stdin and stdout. Your host starts it as a child process, and it exits when that parent goes away. It opens no network listener of its own: each request dials the daemon's Unix socket, and the daemon is the only thing holding the gateway connection.

That means the wiring is the same everywhere. Point the host at the absolute path of the `canary` binary, pass the single argument `mcp`, and restart the host.

Install first. [Install and first run](install.md) covers prerequisites, paths, and the `canary status` check that tells you whether the gateway half of this is working.

## Claude Desktop

Two ways in, and they update differently.

**The bundle.** Open [`canary.mcpb`](https://github.com/osauer/canary/releases/latest/download/canary.mcpb) with Claude Desktop. It carries its own binaries for macOS and Linux on both architectures, behind a launcher script that picks the matching one and exits with a clear error on anything else. The [Claude Desktop install page](../../claude-desktop-interactive-brokers/index.html) has the click-by-click version. Update it by reinstalling the bundle, not with `canary update`.

**The shell-managed binary.** Use this when you want one binary shared with the terminal and other hosts:

```sh
canary setup claude-desktop
```

That writes an `mcpServers.canary` entry into `~/Library/Application Support/Claude/claude_desktop_config.json`, recording the resolved (symlink-free) path of the binary that ran the command. It backs the old file up to `claude_desktop_config.json.bak-<timestamp>` first, keeps unrelated top-level keys, and overwrites any previous `canary` entry. If the existing file is not valid JSON it stops and changes nothing. macOS is the only target: anywhere else the command exits with an error pointing at the generic snippet below.

Then fully quit Claude Desktop with Command-Q and reopen it. Closing the window is not enough.

## Claude Code

```
/plugin marketplace add osauer/canary
/plugin install canary@canary
```

The Claude for Mac embedded pane has no `/plugin` slash commands, so do it from a regular terminal instead:

```sh
claude plugin marketplace add osauer/canary
claude plugin install canary@canary
```

The plugin carries the skill, the safety hooks, and its own MCP server config. It does not carry the binary. Its launcher looks for one in this order and runs the first executable it finds: `CANARY_BIN`, the plugin's local `bin/canary`, `PATH`, `~/.local/bin/canary`, `/opt/homebrew/bin/canary`, `/usr/local/bin/canary`. With none of them present it prints install instructions and fails, which is the usual cause of a plugin that installs cleanly and then has no working tools.

Binary and plugin release on separate cadences, so update them separately. [Working with agents](../operate/agents.md) covers what to do once the tools are live.

## Cursor, Zed, and any other stdio host

Paste this into the host's MCP configuration:

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

`which canary` gives you the value for `command`. It has to be absolute: the host runs it through `exec`, which does not expand `~` and does not consult `$PATH`. Config file locations and the surrounding key names differ per host, so check your host's own MCP documentation for where the file lives; the `command` and `args` above are what it needs to contain.

A browser-based assistant cannot use this. The server communicates over the stdio of a process on your machine, so the host has to be running on that machine too.

## Confirm it connected

1. Ask the host to list its MCP servers, or look for the `canary_*` tools in its tool list. In Claude Code, `claude mcp list` reports the server state directly.
2. Run `canary status` in a terminal. It separates the two failure classes. No `canary_*` tools in the host at all is a wiring problem, because the tool list comes from the server process. Tools present but every call failing, with `OFFLINE` in the status output, is a gateway problem.
3. On Claude Desktop, read `~/Library/Logs/Claude/mcp-server-canary.log`. A wrong or relative `command` path shows up there as a spawn failure.

After upgrading the binary, fully quit and relaunch the host. Hosts keep the server subprocess alive across chats and will happily run the old binary until they are restarted. `canary restart` does not help here: it restarts the shared daemon, not the stdio process your host owns. [Updating](updating.md) has the rest.

## Trimming the tool surface

```sh
canary mcp --profile monitor
```

The `monitor` profile exposes exactly two tools, `canary_brief` and `canary_status`. It exists for scheduled low-token checks. Register it as a second server entry if you want both the full surface and a cheap one.

## What the agent can then see

Thirteen tools cover the daily brief, account and positions, named-symbol technical analysis, rulebook verdict, protection proposals, option-exercise opportunities, settings, trading readiness, and read-only order-journal views. Local lifecycle verbs (`setup`, `update`, `restart`, `mcp`, `daemon`, `version`) and human policy/reconciliation writes remain CLI-only. The [MCP tools reference](../reference/mcp-tools.md) is generated from the registry and lists every parameter.

The bundled MCP surface has no order-entry or preview tools. A unit test enforces that boundary against the tool registry by name. [Orders and the trading build](../operate/orders.md) owns the confirmed CLI and app paths.
