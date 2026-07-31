#!/usr/bin/env bash

set -euo pipefail

root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$root/scripts/github-release-state.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-github-release-state-test.XXXXXX")"
cleanup() {
	rm -rf -- "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_root/bin"
cat >"$test_root/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$*" = "api --hostname github.com --include -X GET repos/osauer/canary/releases/tags/v2.5.4" ] || exit 97
case "${GH_FIXTURE:-}" in
existing)
	printf 'HTTP/2.0 200 OK\r\ncontent-type: application/json\r\n\r\n{}\n'
	exit 0
	;;
absent)
	printf 'HTTP/2.0 404 Not Found\r\ncontent-type: application/json\r\n\r\n{}\n'
	exit 1
	;;
network)
	exit 1
	;;
server-error)
	printf 'HTTP/2.0 500 Internal Server Error\r\n\r\n{}\n'
	exit 1
	;;
contradictory)
	printf 'HTTP/2.0 404 Not Found\r\n\r\n{}\n'
	exit 0
	;;
redirect-chain)
	printf 'HTTP/1.1 301 Moved Permanently\r\n\r\nHTTP/2.0 200 OK\r\n\r\n{}\n'
	exit 0
	;;
*)
	exit 98
	;;
esac
SH
chmod +x "$test_root/bin/gh"

run_ok() {
	local fixture="$1" expected="$2" observed
	observed="$(PATH="$test_root/bin:$PATH" GH_FIXTURE="$fixture" "$checker" v2.5.4)"
	if [ "$observed" != "$expected" ]; then
		echo "github-release-state test: $fixture returned '$observed', expected '$expected'" >&2
		exit 1
	fi
}

run_fail() {
	local fixture="$1"
	if PATH="$test_root/bin:$PATH" GH_FIXTURE="$fixture" \
		"$checker" v2.5.4 >"$test_root/output" 2>&1; then
		echo "github-release-state test: accepted unsafe $fixture response" >&2
		exit 1
	fi
}

run_ok existing existing
run_ok absent absent
run_fail network
run_fail server-error
run_fail contradictory
run_fail redirect-chain

if PATH="$test_root/bin:$PATH" GH_FIXTURE=existing \
	"$checker" not-a-version >"$test_root/output" 2>&1; then
	echo "github-release-state test: accepted malformed version" >&2
	exit 1
fi

echo "github-release-state test: PASS"
