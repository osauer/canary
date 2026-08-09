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
	local detector="$1" text="$2" target="${3:-stale.txt}"
	printf '%s\n' "$text" > "$test_root/$target"
	git -C "$test_root" add "$target"
	if "$checker" "$test_root" >/dev/null 2>"$test_root/.git/check-error"; then
		echo "check-product-identity test: stale $detector passed" >&2
		exit 1
	fi
	if ! grep -q "retired $detector" "$test_root/.git/check-error"; then
		echo "check-product-identity test: stale $detector reported the wrong failure" >&2
		cat "$test_root/.git/check-error" >&2
		exit 1
	fi
	git -C "$test_root" rm -q -f "$target"
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
assert_rejected retired-public-surface 'run `canary gamma --json`' README.md

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

git -C "$test_root" rm -q -f internal/update/daemon_test.go
"$checker" "$test_root" >/dev/null

# An unmerged index must be named as the cause. `ls-files --cached` repeats a
# conflicted path once per stage, which previously multiplied its allowlisted
git -C "$test_root" -c user.email=test@example.com -c user.name=test commit -q -m base
base_branch="$(git -C "$test_root" rev-parse --abbrev-ref HEAD)"
git -C "$test_root" checkout -q -b conflicting
printf 'const namespace = "canary"\n' > "$test_root/pins.go"
git -C "$test_root" -c user.email=test@example.com -c user.name=test commit -q -am "branch spelling"
git -C "$test_root" checkout -q "$base_branch"
printf 'const namespace = "canary-desk"\n' > "$test_root/pins.go"
git -C "$test_root" -c user.email=test@example.com -c user.name=test commit -q -am "trunk spelling"
# The identity is required even though a conflicting merge never reaches a
# no identity, so the merge failed, no conflict was produced, and the fixture
git -C "$test_root" -c user.email=test@example.com -c user.name=test merge conflicting >/dev/null 2>&1 || true
if [ -z "$(git -C "$test_root" ls-files --unmerged)" ]; then
	echo "check-product-identity test: fixture did not produce an unmerged index" >&2
	exit 1
fi
if "$checker" "$test_root" >/dev/null 2>"$test_root/.git/check-error"; then
	echo "check-product-identity test: unmerged index passed" >&2
	exit 1
fi
if ! grep -q "unmerged index" "$test_root/.git/check-error"; then
	echo "check-product-identity test: unmerged index blamed the wrong cause" >&2
	cat "$test_root/.git/check-error" >&2
	exit 1
fi
if ! grep -q "pins.go" "$test_root/.git/check-error"; then
	echo "check-product-identity test: unmerged index did not name the path" >&2
	exit 1
fi
git -C "$test_root" merge --abort

echo "check-product-identity test: OK"
