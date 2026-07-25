# Canary SPA Authority Matrix Template

Updated: 2026-06-06 06:35 CEST

Use this before changing Canary SPA screens, labels, fixtures, live snapshots,
SSE data, app routes, or browser-visible trading/risk posture.

## Required Reading

- Root `AGENTS.md`
- `web/app/AGENTS.md`
- `internal-docs/guides/canary-spa-dev.md`
- `internal-docs/design/platform-settings.md` when settings/config/state surfaces are
  involved

## Matrix

| UI concept | Human label | Authoritative source | RPC/app snapshot path | Fixture/test path | Stale/error behavior | Rendered QA gate |
|---|---|---|---|---|---|---|
|  |  |  |  |  |  |  |

## Semantic Checks

- `Daily P/L` means start-of-trading-day P/L.
- `Open P/L` or `Unrealized P/L` means current open-position cost-basis P/L.
- Quote move/percent move is underlying market movement, not position P/L.
- Avoid `total P/L` unless the total is explicitly defined and reconciled.
- Degraded readiness, unavailable rows, stale data, and warning details should
  show product posture from the canonical backend/app snapshot, not CSS-only
  overrides.

## Embedded App Workflow

Source edits under `web/app` are not visible in the normal browser preview until
the embedded binary and app host are refreshed.

```sh
make app-check
make app-refresh
ibkr app pair --public-url http://127.0.0.1:8765 --json
```

Open the returned `.url` in the Codex Browser. For rendered behavior:

```sh
make app-refresh-smoke APP_SMOKE_BROWSER=webkit
```

Before completion, run the root gate from `AGENTS.md`. If in-app Browser
screenshots fail and Playwright/WebKit is used as fallback, report that fallback.

## Completion Artifact

- Pairing URL command output:
- Browser/DOM evidence:
- Console errors:
- Mobile/responsive viewport checked:
- Root gate result:
