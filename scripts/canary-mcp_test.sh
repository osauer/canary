#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
launcher="$repo_root/scripts/canary-mcp.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-mcp-launcher-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

make_binary() {
	local path="$1" identity="$2"
	mkdir -p "$(dirname "$path")"
	cat > "$path" <<EOF
#!/bin/sh
printf '%s:%s\n' '$identity' "\${1:-}"
EOF
	chmod 0755 "$path"
}

run_launcher() {
	local home="$test_root/home"
	HOME="$home" \
	PATH="$test_root/path:/usr/bin:/bin" \
	CLAUDE_PLUGIN_ROOT="$test_root/plugin" \
	CANARY_BIN="${CANARY_BIN_UNDER_TEST:-}" \
		bash "$launcher"
}

make_binary "$test_root/override/canary" "CANARY_BIN"
make_binary "$test_root/plugin/bin/canary" "bundled-canary"
make_binary "$test_root/path/canary" "path-canary"

got="$(CANARY_BIN_UNDER_TEST="$test_root/override/canary" run_launcher)"
[ "$got" = "CANARY_BIN:mcp" ] || {
	echo "canary-mcp test: CANARY_BIN did not win launcher precedence" >&2
	exit 1
}

got="$(CANARY_BIN_UNDER_TEST="" run_launcher)"
[ "$got" = "bundled-canary:mcp" ] || {
	echo "canary-mcp test: bundled canary did not precede PATH canary" >&2
	exit 1
}

rm "$test_root/plugin/bin/canary"
got="$(CANARY_BIN_UNDER_TEST="" run_launcher)"
[ "$got" = "path-canary:mcp" ] || {
	echo "canary-mcp test: PATH canary was not used" >&2
	exit 1
}

rm "$test_root/path/canary"
make_binary "$test_root/override/ibkr" "IBKR_BIN"
make_binary "$test_root/plugin/bin/ibkr" "bundled-ibkr"
make_binary "$test_root/path/ibkr" "path-ibkr"
if output="$(run_launcher 2>&1)"; then
	echo "canary-mcp test: legacy ibkr fallback unexpectedly succeeded" >&2
	exit 1
fi
for required in \
	"Canary Claude Code plugin could not find an executable canary binary." \
	"raw.githubusercontent.com/osauer/canary/main/install.sh" \
	"CANARY_BIN=/absolute/path/to/canary"; do
	if [[ "$output" != *"$required"* ]]; then
		echo "canary-mcp test: canonical error copy missing $required" >&2
		exit 1
	fi
done
for forbidden in IBKR_BIN "/bin/ibkr"; do
	if [[ "$output" == *"$forbidden"* ]]; then
		echo "canary-mcp test: canonical error copy retained legacy fallback $forbidden" >&2
		exit 1
	fi
done

echo "canary-mcp test: OK"
