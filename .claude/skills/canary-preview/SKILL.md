---
name: canary-preview
description: Bring up the Canary SPA in the Claude Code preview pane. Use when asked to open, show, load, or preview the canary/mobile app in the preview panel/pane. Never use this for the shared LAN host (0.0.0.0:8765) or macOS `open`.
---

Bring up the Canary SPA in an isolated preview-pane instance — never the shared LAN host.

1. `preview_start` the `canary-app` launch config (`.claude/launch.json`): an
   isolated instance on `127.0.0.1:8766` with its own `/tmp` state dir.
2. Mint a pairing URL:
   `canary app pair --addr 127.0.0.1:8766 --public-url http://127.0.0.1:8766 --json`
3. `preview_eval` navigation to the returned `.url`
   (e.g. `window.location.href = "<url>"`).

Success = redirect to `/` with title `Canary`.

Guardrails:
- Never adopt, kill, or bind the shared `0.0.0.0:8765` host — that's a
  separate long-running process paired to a phone over LAN; touching it
  breaks pairing.
- A blank preview with zero console errors almost always means no server is
  running yet (the Launch panel falls back to a static `file://` load of
  `web/app/index.html`, whose absolute-path `/app.js` and `/styles.css`
  can't resolve). Start `canary-app` and re-pair instead of debugging the
  SPA.
- After editing `web/app` source, `make install` and restart the preview
  server before re-pairing — the preview instance serves the installed
  binary, not loose source files.
