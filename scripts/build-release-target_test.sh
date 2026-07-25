#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-release-target-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_root/repo/cmd/ibkr" "$test_root/repo/docs/docs/operate" "$test_root/fake-bin" "$test_root/dist"
cp "$repo_root/scripts/build-release-target.sh" "$test_root/repo/build-release-target.sh"
printf '%s\n' 'MIT fixture' > "$test_root/repo/LICENSE"
printf '%s\n' '# Security fixture' > "$test_root/repo/SECURITY.md"
printf '%s\n' '# Trading preview fixture' > "$test_root/repo/docs/docs/operate/orders.md"
printf '%s\n' '# ibkr' '' '## Safety' > "$test_root/repo/README.md"
printf '%s\n' 'package main' 'func main() {}' > "$test_root/repo/cmd/ibkr/main.go"
printf '%s\n' '#!/bin/sh' 'set -eu' 'out=' 'while [ "$#" -gt 0 ]; do' '  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi' 'done' 'test -n "$out"' 'printf "%s\n" "fixture binary" > "$out"' 'chmod 0755 "$out"' > "$test_root/fake-bin/go"
chmod 0755 "$test_root/fake-bin/go"

(cd "$test_root/repo" && PATH="$test_root/fake-bin:$PATH" ./build-release-target.sh darwin-arm64 v1.2.3 '-s -w' "$test_root/dist")

readonly_list="$(tar -tzf "$test_root/dist/ibkr-v1.2.3-darwin-arm64.tar.gz")"
trading_list="$(tar -tzf "$test_root/dist/ibkr-trading-v1.2.3-darwin-arm64.tar.gz")"
printf '%s\n' "$readonly_list" | grep -qx 'ibkr-v1.2.3-darwin-arm64/ibkr'
if printf '%s\n' "$readonly_list" | grep -q 'TRADING-WARNING.md'; then
	echo "build-release-target test: read-only archive contains the trading warning" >&2
	exit 1
fi
printf '%s\n' "$trading_list" | grep -qx 'ibkr-trading-v1.2.3-darwin-arm64/TRADING-WARNING.md'
warning="$(tar -xOzf "$test_root/dist/ibkr-trading-v1.2.3-darwin-arm64.tar.gz" 'ibkr-trading-v1.2.3-darwin-arm64/TRADING-WARNING.md')"
printf '%s\n' "$warning" | grep -Fq 'blob/v1.2.3/SECURITY.md'
printf '%s\n' "$warning" | grep -Fq 'blob/v1.2.3/docs/docs/operate/orders.md'

echo "build-release-target test: OK"
