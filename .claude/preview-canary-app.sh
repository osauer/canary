#!/bin/sh
# Launcher for the Claude preview pane's isolated Canary app instance.
# The preview harness assigns PORT (launch.json: autoPort true), so several
# sessions can run previews concurrently — each gets its own port and its
# own state dir, mirroring the app-lifecycle-smoke isolation pattern.
# The shared LAN host on 0.0.0.0:8765 is a separate process; never bind it here.
#
# Pair a fresh preview browser against the assigned port:
#   canary app pair --addr 127.0.0.1:$PORT --public-url http://127.0.0.1:$PORT --json
set -eu
port="${PORT:-8766}"
exec /Users/osauer/.local/bin/canary app \
  --addr "127.0.0.1:${port}" \
  --public-url "http://127.0.0.1:${port}" \
  --state-dir "/tmp/ibkr-preview-app-state-${port}"
