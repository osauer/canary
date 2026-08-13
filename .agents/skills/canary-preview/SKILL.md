---
name: "canary-preview"
description: "Bring up the Canary SPA quickly in a Codex browser panel on the isolated read-only loopback host at 127.0.0.1:8766. Use when asked to open, show, load, or preview the Canary or mobile app in Codex. Never use this for the shared LAN host on 0.0.0.0:8765 or macOS `open`."
---

# Canary preview

Open the isolated Canary SPA at `http://127.0.0.1:8766/`. Never adopt, kill,
restart, or bind the shared LAN host on `0.0.0.0:8765`; it belongs to the
phone-paired app.

1. Reuse an already-open tab or preview process for that exact URL. Do not
   launch a duplicate.
2. Otherwise start this direct command in a long-running terminal session and
   keep it alive:

   ```sh
   canary app --addr 127.0.0.1:8766 --public-url http://127.0.0.1:8766 --state-dir /tmp/ibkr-codex-preview-app-state-8766 --preview-read-grant
   ```

3. Open `http://127.0.0.1:8766/` in a Codex browser panel. Prefer the Codex app
   open-panel tool; do not load browser automation merely to show the page.
4. Stop when the panel is open. No pairing, navigation beyond `/`, page reads,
   screenshots, or console inspection. Report success in one line.

The `--preview-read-grant` flag exposes only read routes to an unpaired
loopback browser. Never submit broker writes from a browser.

The preview serves the installed binary, not loose source. After editing
`web/app`, run `make install`, stop only this skill's long-running preview
session, and launch it again. A blank page with no console errors usually means
the isolated server is no longer running; restart this preview session before
debugging the SPA.
