# Connect an MCP host

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone with ibkr installed who now wants an agent to use it.

**Questions the page has to answer**

- What `ibkr mcp` is: a local stdio MCP server, one process, no network
  listener, no hosted component.
- The per-host wiring for Claude Desktop, Claude Code, Cursor, Zed, and the
  generic stdio case. Config snippets that can be copied.
- How to tell the host actually connected, and what to check when it did not.
- What the agent can see, in one paragraph, with a link to the MCP tools
  reference for the full list.
- Why Windows Claude Desktop is not supported, and that WSL works.

**Draw from**

- `README.md`, "Pick your path".
- `docs/reference/mcp-tools.md` and `docs/reference/mcp-resources.md`.
- `ibkr setup claude-desktop` behaviour in `cmd/ibkr/setup.go`.

**Boundaries to keep**

- The bundled MCP surface has no order-entry tools. Say "order-entry tools",
  never "order tools". The preview-only draft tool exists in every variant.
- Do not claim MCP exposure for CLI-only surfaces such as `ibkr policy` and
  `ibkr recon`.
