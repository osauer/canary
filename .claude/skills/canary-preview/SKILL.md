---
name: canary-preview
description: Bring up the Canary SPA in the Claude Code preview pane. Use when asked to open, show, load, or preview the canary/mobile app in the preview panel/pane. Never use this for the shared LAN host (0.0.0.0:8765) or macOS `open`.
---

Isolated Canary SPA instance in the preview pane — never the shared LAN host.
Three tool calls, nothing else:

1. `preview_start` launch config `canary-app`; take `port` from the result
   (autoPort — 8766 only when free).
2. `sh .claude/skills/canary-preview/pair.sh <port>` — prints the pairing URL,
   one line. Always run it, even when the server was `reused`; re-pairing is
   idempotent and cheaper than probing whether the tab is still paired.
3. `navigate` the preview tab to that URL.

Done when the navigate result itself shows the tab at `http://127.0.0.1:<port>`
titled `Canary`. That is the whole proof — no `read_page`, no screenshot, no
console check; the user sees the pane. Report success in one line.

Guardrails:
- Never adopt, kill, or bind the shared `0.0.0.0:8765` host — it is phone-paired
  over LAN; touching it breaks pairing.
- Blank preview with zero console errors = no server running (the Launch panel
  falls back to a static `file://` load whose absolute-path assets can't
  resolve). Start `canary-app` and re-pair; don't debug the SPA.
- After editing `web/app`: `make install`, restart the preview server, re-pair —
  the instance serves the installed binary, not loose source.
