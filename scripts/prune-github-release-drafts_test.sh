#!/usr/bin/env bash

set -euo pipefail

source_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-prune-drafts-test.XXXXXX")"
bin="$test_root/bin"
pruner="$source_root/scripts/prune-github-release-drafts.sh"

cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$bin"
cat >"$bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$*" = "api --hostname github.com repos/osauer/canary/releases?per_page=100 --jq .[] | select(.draft == true and .tag_name == \"$TEST_VERSION\") | .id" ]; then
	cat "$TEST_DRAFT_IDS"
	exit 0
fi
if [ "$1" = "api" ] && [ "$2" = "--hostname" ] && [ "$4" = "-X" ] && [ "$5" = "DELETE" ]; then
	printf '%s\n' "$6" >>"$TEST_DELETED"
	exit 0
fi
exit 97
EOF
chmod 0755 "$bin/gh"

run_pruner() {
	PATH="$bin:/usr/bin:/bin" \
		TEST_VERSION="v1.2.3" \
		TEST_DRAFT_IDS="$test_root/draft-ids" \
		TEST_DELETED="$test_root/deleted" \
		"$pruner" v1.2.3
}

fail() {
	echo "prune-github-release-drafts test: $1" >&2
	exit 1
}

if "$pruner" 2>/dev/null; then
	fail "missing version argument passed"
fi
if "$pruner" not-a-version 2>/dev/null; then
	fail "malformed version passed"
fi

: >"$test_root/draft-ids"
: >"$test_root/deleted"
run_pruner >/dev/null || fail "no-draft case failed"
[ ! -s "$test_root/deleted" ] || fail "no-draft case deleted something"

printf '%s\n' 111 222 >"$test_root/draft-ids"
: >"$test_root/deleted"
run_pruner >/dev/null || fail "two-draft case failed"
[ "$(cat "$test_root/deleted")" = "$(printf '%s\n' \
	repos/osauer/canary/releases/111 repos/osauer/canary/releases/222)" ] ||
	fail "two-draft case deleted the wrong records"

printf '%s\n' "111; rm -rf /" >"$test_root/draft-ids"
: >"$test_root/deleted"
if run_pruner >/dev/null 2>&1; then
	fail "non-numeric draft id passed"
fi
[ ! -s "$test_root/deleted" ] || fail "non-numeric id case deleted something"

echo "prune-github-release-drafts test: OK"
