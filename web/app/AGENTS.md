# Canary SPA Agent Rules

These rules apply when editing files under `web/app`.

## Serving And Refreshing

The SPA is embedded in the `canary` binary and served by the long-running
`canary app` process. Source edits are not visible in the in-app Browser until a
new binary is installed and the app host is restarted.

For the shared local/phone host, prefer:

```sh
make app-refresh
```

This installs the binary, restarts `canary app`, and prints a local pairing URL
for `http://127.0.0.1:8765`. Keep the shared app host LAN-capable; do not start
it loopback-only unless deliberately testing an isolated local preview.

## Pairing And Browser Preview

The root URL is authenticated. In a fresh browser, create a local pairing URL:

```sh
canary app pair --public-url http://127.0.0.1:8765 --json
```

Open the returned `.url` in the in-app Browser. Successful pairing redirects to
`http://127.0.0.1:8765/` with title `Canary`.

Use the in-app Browser for visible local app QA. Do not use macOS `open`.

Browser QA is read-only, even when the page is paired and trading controls are
visible. Do not click or call submit, exercise, cancel, modify, purge, restore,
settings-write, or other state-changing endpoints. A paired browser is stamped
as `human-paired-device`, so browser automation would bypass agent-origin CLI
classification. Any explicitly requested agent broker write must use the gated
CLI path described by the root `AGENTS.md`.

## Browser Debugging

The in-app Browser can read rendered DOM state, click, and inspect console logs,
but its page-evaluate sandbox may not expose every browser global such as
`fetch`. When reconciling rendered values to live data:

- read visible UI state from the Browser DOM;
- read raw live data with CLI/API commands such as `canary account --json` and
  `canary positions --json`;
- compare concepts, not only text formatting.

If in-app Browser screenshots fail, a Playwright/WebKit screenshot is an
acceptable fallback for visual QA. State that fallback in the completion note.

## P/L Semantics

Be precise with financial labels:

- `Daily P/L` means start-of-trading-day account or position P/L.
- `Open P/L` and `Unrealized P/L` mean current open-position cost-basis P/L.
- `Realized P/L` is a separate concept and can make unrealized-only totals
  meaningless for daily attribution.
- Quote price move and quote percent move describe the underlying's market move;
  they are not position P/L.
- Do not use `total P/L` in UI copy unless the total is explicitly defined and
  reconciles to that definition.

For the Underlyings hero, daily winner/loser buckets should be daily P/L
attribution by underlying, not open/unrealized P/L and not a client-estimated
quote-marked value.

## Copy And Rendering Conventions

- Daemon severity vocabulary is verbatim in UI copy: `observe`/`watch`/`act`,
  `quiet`/`building`/`confirmed`, `stand down`. Do not invent synonyms.
- Use `font-variant-numeric: tabular-nums` wherever digits align in columns or
  tick over in place.

## Market Data Access

`status.market_data_access[]` names the route keys the gateway is currently
refusing market data for. It is a time-windowed observation, never an
entitlement model: do not gate, disable, hide, or blank any panel on it. A
refusal is route-keyed and expires, delayed-data fallbacks and farm outages
make the inference wrong in both directions, and a panel holding a cached
result serves it correctly while a fresh fetch for the same key would be
refused. Degrade loudly — name the symbol and the IBKR code, and keep
rendering. An empty list means nothing was refused inside the window, not
that every symbol is entitled.

Prefer naming the refusal over the symptom it causes: a symbol with an
active refusal should not also render a vaguer per-symbol quote fault, or
one cause reads as two.

## Gates

Use the narrow loop while iterating:

```sh
make app-check
```

`make check` runs `app-contract-check`: the `web/app` Go tests statically pin
every element id that `scripts/app-browser-smoke.mjs` and
`scripts/app-browser-smoke.mjs` asserts against `index.html`/`app.js`. Removing
or renaming an SPA id the scripts reference fails pre-commit — update the
script assertions in the same change, and keep the `removedSPAIDs` allowlist
in `web/app/browser_script_ids_test.go` in sync for surfaces the smoke
asserts as deliberately removed.

When rendered behavior matters, refresh the embedded app assets and smoke the
browser:

```sh
make app-refresh-smoke APP_SMOKE_BROWSER=webkit
```

Before finishing, run the repo gate required by the root `AGENTS.md`.

If live `make smoke` fails, report the exact assertion and artifact. Do not
hide the failure by retrying silently. A rerun is only useful when explicitly
diagnosing external gateway nondeterminism, and the first failure still belongs
in the completion note.

## Regime And Risk Posture

When regime label, tone, readiness, or indicator state conflict, ask a
trading/risk agent to judge the UI posture and diagnose missing data. Prefer
fixing the canonical backend posture or data-quality surface over CSS-only UI
overrides.

## Market Event Flags

Market-event UI is compact evidence, not a new action surface. Protection hero
chips show active proposal-bound categories; Underlyings hero chips show active
held-name categories and counts. Row tags should carry source/as-of detail for
halt, LULD, borrow inventory, borrow fee, and Reg SHO flags.

Use red/soft-red for hard blockers such as active halt or active LULD, amber for
execution/carry friction such as borrow tight, fee extreme, and Reg SHO,
neutral/blue for context-only future flags, and muted gray for stale/unknown
source states.

Borrow-inventory and borrow-fee *source-health* chips are relevance-gated:
they render only while the book holds short stock, because buy-to-cover
friction is the only consumer of borrow data. For an all-long book a
permanently unreachable borrow feed (e.g. FTP filtered by the local network)
is noise, not risk disclosure. Halt/LULD/Reg SHO source health stays
unconditional, and active borrow *flags* on held names still render.

V1 protection remains reduce-only: do not add buy-add controls or standalone
squeeze recommendations. The Opportunities panel is allowed for
daemon-calculated option-exercise opportunities only; it must render blockers
and keep submit unavailable unless the daemon snapshot and preview say the
exercise is submit-eligible. Option exercise exposure effect is informational
in the app; central daemon trading gates decide whether submit is allowed. When
a proposal is a reducing short `BUY`, render it as `Buy to cover`.
