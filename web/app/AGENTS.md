# Canary SPA

- Follow the root [AGENTS.md](../../AGENTS.md),
  [SPA guide](../../internal-docs/guides/canary-spa-dev.md) and
  [authority matrix](../../.agents/docs/spa-authority-matrix.md).
- Browser QA is read-only, including settings and pairing-sensitive workflows.
  Without pairing authorization, use `make app-smoke-read-only`, not
  `app-smoke`. For previews, use the isolated host specified by the root rules.
- Assets are embedded: verify the installed binary/host, not loose files.
  Run `make app-check`; update `assets.go` and static imports together.
  Desktop QA does not prove behavior on the paired phone.
- On macOS, launch Playwright outside the sandbox via its API/CLI wrapper.
- Preserve daemon severity vocabulary and P/L definitions. Underlying
  winners/losers use daily P/L, never quote moves or unrealized substitutes.
- `status.market_data_access` records recent route refusals, not entitlements.
  Keep cached panels visible; name the symbol and IBKR code without duplicate
  generic faults.
- Borrow source-health chips apply only to short-stock books; active held-name
  borrow flags still render. Halt/LULD/Reg SHO health stays unconditional.
- Protection stays reduce-only. Opportunities are daemon-calculated option
  exercises; submit eligibility comes from daemon snapshot and preview.
  Label reducing short buys `Buy to cover`.
