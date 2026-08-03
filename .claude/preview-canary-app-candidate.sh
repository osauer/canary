#!/bin/sh
# Candidate-build twin of preview-canary-app.sh: serves the SPA embedded in a
# worktree binary so unlanded UI work can be verified in the preview pane.
# Same isolation contract: assigned port, own state dir, never the shared LAN
# host on 0.0.0.0:8765.
# Point CANARY_CANDIDATE_BIN at the worktree binary under test.
set -eu
port="${PORT:-8768}"
bin="${CANARY_CANDIDATE_BIN:?set CANARY_CANDIDATE_BIN to the candidate canary binary under test}"
exec "$bin" app \
  --addr "127.0.0.1:${port}" \
  --public-url "http://127.0.0.1:${port}" \
  --state-dir "/tmp/ibkr-preview-app-state-candidate-${port}"
