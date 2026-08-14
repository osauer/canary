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
# The fake go embeds any -tags value into the fixture binary and plays it
# back for `go version -m`, so the script's capability assertion is
# exercised end-to-end without a real toolchain.
printf '%s\n' '#!/bin/sh' 'set -eu' \
	'if [ "${1:-}" = "version" ] && [ "${2:-}" = "-m" ]; then cat "$3"; exit 0; fi' \
	'out=' 'tags=' 'while [ "$#" -gt 0 ]; do' \
	'  if [ "$1" = "-o" ]; then out="$2"; shift 2;' \
	'  elif [ "$1" = "-tags" ]; then tags="$2"; shift 2;' \
	'  else shift; fi' 'done' 'test -n "$out"' \
	'printf "%s\n" "fixture binary" > "$out"' \
	'if [ -n "$tags" ]; then printf "\tbuild\t-tags=%s\n" "$tags" >> "$out"; fi' \
	'chmod 0755 "$out"' > "$test_root/fake-bin/go"
chmod 0755 "$test_root/fake-bin/go"

# Make archive listings large enough that a tar-to-grep -q pipeline reliably
# encounters SIGPIPE after grep finds an early entry. The release script must
# consume the complete listing before checking membership.
printf '%s\n' '#!/bin/sh' 'set -eu' \
	'if [ "${1:-}" = "-tzf" ]; then' \
	'  /usr/bin/tar "$@"' \
	'  i=0' \
	'  while [ "$i" -lt 20000 ]; do' \
	'    printf "canary-pipe-padding-%05d\\n" "$i"' \
	'    i=$((i + 1))' \
	'  done' \
	'  exit 0' \
	'fi' \
	'exec /usr/bin/tar "$@"' > "$test_root/fake-bin/tar"
chmod 0755 "$test_root/fake-bin/tar"

(cd "$test_root/repo" && PATH="$test_root/fake-bin:$PATH" ./build-release-target.sh darwin-arm64 v1.2.3 '-s -w' "$test_root/dist")

mkdir -p "$test_root/dist-goflags"
if (cd "$test_root/repo" && GOFLAGS="-tags=trading" PATH="$test_root/fake-bin:$PATH" ./build-release-target.sh darwin-arm64 v1.2.3 '-s -w' "$test_root/dist-goflags") 2> "$test_root/goflags-err.log"; then
	echo "build-release-target test: tag-bearing ambient GOFLAGS was not refused" >&2
	exit 1
fi
grep -q "refuses ambient GOFLAGS" "$test_root/goflags-err.log" || {
	echo "build-release-target test: GOFLAGS refusal message missing:" >&2
	cat "$test_root/goflags-err.log" >&2
	exit 1
}

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
