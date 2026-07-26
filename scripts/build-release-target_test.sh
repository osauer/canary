#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-target-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_root/repo/cmd/canary" "$test_root/repo/docs/docs/operate" "$test_root/fake-bin" "$test_root/dist"
cp "$repo_root/scripts/build-release-target.sh" "$test_root/repo/build-release-target.sh"
printf '%s\n' 'MIT fixture' > "$test_root/repo/LICENSE"
printf '%s\n' '# Security fixture' > "$test_root/repo/SECURITY.md"
printf '%s\n' '# Trading preview fixture' > "$test_root/repo/docs/docs/operate/orders.md"
printf '%s\n' '# ibkr' '' '## Safety' > "$test_root/repo/README.md"
printf '%s\n' 'package main' 'func main() {}' > "$test_root/repo/cmd/canary/main.go"
printf '%s\n' '#!/bin/sh' 'set -eu' 'out=' 'while [ "$#" -gt 0 ]; do' '  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi' 'done' 'test -n "$out"' 'printf "%s\n" "fixture binary" > "$out"' 'chmod 0755 "$out"' > "$test_root/fake-bin/go"
chmod 0755 "$test_root/fake-bin/go"

(cd "$test_root/repo" && PATH="$test_root/fake-bin:$PATH" ./build-release-target.sh darwin-arm64 v1.2.3 '-s -w' "$test_root/dist")

readonly_list="$(tar -tzf "$test_root/dist/canary-v1.2.3-darwin-arm64.tar.gz" | sort)"
trading_list="$(tar -tzf "$test_root/dist/canary-trading-v1.2.3-darwin-arm64.tar.gz" | sort)"

expected_readonly="$(printf '%s\n' \
	'canary-v1.2.3-darwin-arm64/' \
	'canary-v1.2.3-darwin-arm64/LICENSE' \
	'canary-v1.2.3-darwin-arm64/README.md' \
	'canary-v1.2.3-darwin-arm64/canary')"
expected_trading="$(printf '%s\n' \
	'canary-trading-v1.2.3-darwin-arm64/' \
	'canary-trading-v1.2.3-darwin-arm64/LICENSE' \
	'canary-trading-v1.2.3-darwin-arm64/README.md' \
	'canary-trading-v1.2.3-darwin-arm64/TRADING-WARNING.md' \
	'canary-trading-v1.2.3-darwin-arm64/canary')"

[ "$readonly_list" = "$expected_readonly" ] || {
	echo "build-release-target test: canonical read-only archive has unexpected entries:" >&2
	printf '%s\n' "$readonly_list" >&2
	exit 1
}
[ "$trading_list" = "$expected_trading" ] || {
	echo "build-release-target test: canonical trading archive has unexpected entries:" >&2
	printf '%s\n' "$trading_list" >&2
	exit 1
}
if printf '%s\n' "$readonly_list" | grep -q 'TRADING-WARNING.md'; then
	echo "build-release-target test: read-only archive contains the trading warning" >&2
	exit 1
fi
warning="$(tar -xOzf "$test_root/dist/canary-trading-v1.2.3-darwin-arm64.tar.gz" 'canary-trading-v1.2.3-darwin-arm64/TRADING-WARNING.md')"
printf '%s\n' "$warning" | grep -Fq 'blob/v1.2.3/SECURITY.md'
printf '%s\n' "$warning" | grep -Fq 'blob/v1.2.3/docs/docs/operate/orders.md'
printf '%s\n' "$warning" | grep -Fq 'github.com/osauer/canary/blob/v1.2.3/'

if find "$test_root/dist" -maxdepth 1 -type f -name 'ibkr-*.tar.gz' | grep -q .; then
	echo "build-release-target test: retired ibkr release archive was produced" >&2
	exit 1
fi

echo "build-release-target test: OK"
