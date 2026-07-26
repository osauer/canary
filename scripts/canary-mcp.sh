#!/usr/bin/env bash
set -euo pipefail

candidate_paths=()

if [[ -n "${CANARY_BIN:-}" ]]; then
	candidate_paths+=("$CANARY_BIN")
fi

if [[ -n "${CLAUDE_PLUGIN_ROOT:-}" ]]; then
	candidate_paths+=("$CLAUDE_PLUGIN_ROOT/bin/canary")
fi

if command -v canary >/dev/null 2>&1; then
	candidate_paths+=("$(command -v canary)")
fi

candidate_paths+=(
	"$HOME/.local/bin/canary"
	"/opt/homebrew/bin/canary"
	"/usr/local/bin/canary"
)

for candidate in "${candidate_paths[@]}"; do
	if [[ -x "$candidate" ]]; then
		exec "$candidate" mcp
	fi
done

cat >&2 <<'EOF'
Canary Claude Code plugin could not find an executable canary binary.

Install the CLI first, then restart Claude Code:
  curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh

For local development from a checkout:
  make install

You can also set CANARY_BIN=/absolute/path/to/canary before starting Claude Code.
EOF

exit 127
