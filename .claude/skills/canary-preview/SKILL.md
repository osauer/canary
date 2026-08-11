---
name: canary-preview
description: Bring up the Canary SPA in the Claude Code preview pane. Use when asked to open, show, load, or preview the canary/mobile app in the preview panel/pane. Never use this for the shared LAN host (0.0.0.0:8765) or macOS `open`.
---

Isolated Canary SPA instance in the preview pane — never the shared LAN host.
One tool call, nothing else:

1. `preview_start` launch config `canary-app`.

The instance runs with `--preview-read-grant`, so the tab the harness opens
renders the app read-only immediately — no pairing, no navigate. Done when the
`preview_start` result shows the tab open at the assigned port (autoPort —
8766 only when free). That is the whole proof — no `read_page`, no screenshot,
no console check; the user sees the pane. Report success in one line.

Only when a session must exercise paired-device action flows in the preview,
pair on top: `sh .claude/skills/canary-preview/pair.sh <port>` prints a
pairing URL; `navigate` the tab to it. Never submit broker writes from the
browser regardless of pairing — browser QA is read-only by project rule.

Guardrails:
- Never adopt, kill, or bind the shared `0.0.0.0:8765` host — it is phone-paired
  over LAN; touching it breaks pairing.
- Blank preview with zero console errors = no server running (the Launch panel
  falls back to a static `file://` load whose absolute-path assets can't
  resolve). Start `canary-app`; don't debug the SPA.
- After editing `web/app`: `make install`, restart the preview server — the
  instance serves the installed binary, not loose source.
