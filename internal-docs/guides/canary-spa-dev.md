# Canary SPA Development

Updated: 2026-06-05 17:42 CEST

This guide covers the fast path for changing and verifying the Canary SPA served
by `ibkr app`.

## Mental Model

The app is not served from loose source files during normal local preview. It is
embedded in the `ibkr` binary and served by the long-running `ibkr app` process.
After changing `web/app`, install a new binary and restart the app host before
trusting the browser.

Use:

```sh
make app-refresh
```

This runs the install/restart path and prints a fresh local pairing URL for
`http://127.0.0.1:8765`.

## Browser Preview

The root URL is authenticated. For a fresh browser context, create a local
pairing URL:

```sh
ibkr app pair --public-url http://127.0.0.1:8765 --json
```

Open the returned `.url`. A successful pairing redirects to
`http://127.0.0.1:8765/` with title `Canary · IBKR`.

Keep the shared host on the LAN-capable default bind so the same process can
serve the Codex Browser on `127.0.0.1` and a phone on the Mac's LAN URL. Use
loopback-only hosts only for deliberately isolated tests.

## Fast Gates

During SPA iteration:

```sh
make app-check
```

For rendered behavior:

```sh
make app-refresh-smoke APP_SMOKE_BROWSER=webkit
```

Before completion, still run the repo gate from `AGENTS.md` when the task is
expected to be done end to end.

## Browser Debugging

Use the in-app Browser for visible QA and interaction. If the Browser's
page-evaluate sandbox is missing a browser global such as `fetch`, split the
debugging path:

- read rendered DOM values from the Browser;
- read raw live data with CLI/API commands;
- compare the intended concept rather than only rendered text.

If in-app Browser screenshots time out, Playwright/WebKit screenshots are a
valid fallback. Mention the fallback in the final note so reviewers know which
surface produced the image.

## P/L Semantics

Be explicit in UI copy and tests:

- `Daily P/L` is start-of-trading-day P/L.
- `Open P/L` or `Unrealized P/L` is current open-position cost-basis P/L.
- `Realized P/L` is separate; a daily attribution can include realized moves,
  so unrealized-only totals can be misleading.
- Quote absolute and percent move describe market price movement, not position
  P/L.
- Avoid `total P/L` unless the UI defines the total and reconciles to that
  definition.

When the UI compares account Daily P/L to grouped underlying winners/losers,
the grouped view should use the same daily concept, with tolerance for live tick
timing and formatting.

## Regime Posture

When a regime label appears benign but readiness is degraded, indicators are
yellow, or data is unavailable, ask a trading/risk agent for two answers:

- what posture should the UI show for a human operator?
- why is the data missing or degraded?

Prefer a canonical backend posture/data-quality fix over CSS-only coloring.

## Live Smoke Failures

Live smoke failures are artifacts, not annoyances. Capture the exact assertion,
wire/log path, and the relevant command output. Do not hide a failure by
rerunning silently.

A rerun can be useful while diagnosing external gateway nondeterminism, but the
first failure must still be reported.
