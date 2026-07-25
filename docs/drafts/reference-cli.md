# CLI reference

Status: planned. This file is the brief for the page, not the page itself.

**This page should be generated, not written.** `internal/cli` already holds a
command registry with a name, summary, and usage string for all 36 subcommands
(`internal/cli/cli.go`), plus flag and guard metadata in
`internal/cli/catalog.go`. A `scripts/docgen/cli-ref` generator emitting
`docs/reference/cli.md` would follow the same pattern as `config-ref` and
`mcp-tools`, and `make docs-check` would then keep it honest for free.

**What the generated page needs**

- One entry per subcommand: name, summary, usage line, flags.
- The guard class for each command, so a reader can see at a glance which
  commands only read, which touch local state, and which need confirmation.
- Which commands have an MCP counterpart and which are CLI-only. The parity
  test in the MCP package already knows this.
- A short hand-written preamble covering `--json`, `--watch`, `NO_COLOR`, and
  `IBKR_COLOR`. Everything else comes from the registry.

**Sequencing.** Build the generator first, then flip this manifest entry from
planned to published. Doing it in the other order produces a page that goes
stale the first time a flag changes.

**Boundaries to keep**

- The generated page must state the gated status of `ibkr order` rather than
  listing it as an ordinary command.
