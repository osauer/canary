---
paths:
  - "internal/mcp/**"
---

# MCP tool descriptions are documentation

Adding or changing an entry in `internal/mcp/tools.go`: every `Description` string and every parameter `description` in the JSON schema is what an LLM reads to decide whether to invoke the tool. Hold them to documentation standard, not implementation comment standard:

- **Tells the model when to invoke** — the use case in the user's language ("what you own", "is the regime favorable"), not just "calls handleX RPC".
- **Tells the model when NOT to invoke** — name the overlapping tool a confused LLM might pick instead (e.g. `canary_quote` calls out "NOT for options — use `canary_chain`").
- **Parameter descriptions explain semantics, not just type** — case-sensitivity, defaults, what good values look like.

After changes run `make docs-regen` to update `docs/docs/reference/mcp-tools.md`; `make check` enforces no drift via `docs-check`.
