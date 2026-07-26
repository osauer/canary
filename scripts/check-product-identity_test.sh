#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-product-identity.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-product-identity-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_root/scripts" "$test_root/internal/update"
cp "$repo_root/scripts/product-identity-allowlist.tsv" "$test_root/scripts/product-identity-allowlist.tsv"
cat > "$test_root/go.mod" <<'EOF'
module github.com/osauer/canary/v3
EOF
cat > "$test_root/pins.go" <<'EOF'
package fixture

const socket = "ibkr.sock"
const namespace = "ibkr"
const brokerEnv = "IBKR_PORT"
const brokerSource = "ibkr_wsh"
EOF
git -C "$test_root" init -q
git -C "$test_root" add .
"$checker" "$test_root" >/dev/null

assert_rejected() {
	local detector="$1" text="$2"
	printf '%s\n' "$text" > "$test_root/stale.txt"
	git -C "$test_root" add stale.txt
	if "$checker" "$test_root" >/dev/null 2>"$test_root/.git/check-error"; then
		echo "check-product-identity test: stale $detector passed" >&2
		exit 1
	fi
	if ! grep -q "retired $detector" "$test_root/.git/check-error"; then
		echo "check-product-identity test: stale $detector reported the wrong failure" >&2
		cat "$test_root/.git/check-error" >&2
		exit 1
	fi
	git -C "$test_root" rm -q -f stale.txt
}

assert_rejected old-module-or-repository 'module github.com/osauer/ibkr/v2'
assert_rejected old-site 'website = "https://osauer.dev/ibkr/"'
assert_rejected old-mcp-server '"name": "io.github.osauer/ibkr"'
assert_rejected old-mcp-resource 'uri = "ibkr://quote/{symbol}"'
assert_rejected old-product-path 'exec = "bin/ibkr"'
assert_rejected old-cli-command 'run `ibkr status --json`'
assert_rejected old-cli-argv 'exec.Command("ibkr", "status")'
assert_rejected old-product-name '"name": "ibkr"'
assert_rejected old-mcp-tool 'tool = "ibkr_status"'

cat > "$test_root/internal/update/daemon_test.go" <<'EOF'
package update

// pre-upgrade process quiescence only: ibkr daemon
EOF
git -C "$test_root" add internal/update/daemon_test.go
"$checker" "$test_root" >/dev/null

cat >> "$test_root/internal/update/daemon_test.go" <<'EOF'
// a second stale use exceeds the reviewed exception: ibkr status
EOF
git -C "$test_root" add internal/update/daemon_test.go
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-product-identity test: exception max_hits was not enforced" >&2
	exit 1
fi

echo "check-product-identity test: OK"
