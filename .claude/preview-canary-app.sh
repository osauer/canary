#!/bin/sh
# Launcher for the Claude preview pane's isolated Canary app instance.
# The preview harness assigns PORT (launch.json: autoPort true), so several
# sessions can run previews concurrently — each gets its own port and its
# own state dir, mirroring the app-lifecycle-smoke isolation pattern.
# The shared LAN host on 0.0.0.0:8765 is a separate process; never bind it here.
#
# --preview-read-grant lets the preview tab render read-only without pairing;
# actions still require a real pairing:
#   canary app pair --addr 127.0.0.1:$PORT --public-url http://127.0.0.1:$PORT --json
set -eu
port="${PORT:-8766}"
bin="${CANARY_BIN:-$(command -v canary || true)}"
[ -n "$bin" ] || bin="$HOME/.local/bin/canary"
exec "$bin" app \
  --addr "127.0.0.1:${port}" \
  --public-url "http://127.0.0.1:${port}" \
  --state-dir "/tmp/ibkr-preview-app-state-${port}" \
  --preview-read-grant
