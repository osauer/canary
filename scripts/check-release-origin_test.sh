#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-origin.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-origin-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

reset_fixture() {
	rm -rf "$test_root/repo"
	git init -q "$test_root/repo"
}

expect_pass() {
	local label="$1"
	if ! "$checker" "$test_root/repo" >/dev/null 2>&1; then
		echo "check-release-origin test: $label unexpectedly failed" >&2
		exit 1
	fi
}

expect_fail() {
	local label="$1"
	if "$checker" "$test_root/repo" >/dev/null 2>&1; then
		echo "check-release-origin test: $label unexpectedly passed" >&2
		exit 1
	fi
}

reset_fixture
git -C "$test_root/repo" remote add origin https://github.com/osauer/canary.git
expect_pass "canonical HTTPS origin"

reset_fixture
git -C "$test_root/repo" remote add origin git@github.com:osauer/canary.git
expect_pass "canonical SSH origin"

reset_fixture
git -C "$test_root/repo" remote add origin https://github.com/other/canary.git
expect_fail "forked fetch origin"

reset_fixture
git -C "$test_root/repo" remote add origin https://github.com/osauer/canary.git
git -C "$test_root/repo" remote set-url --push origin https://github.com/other/canary.git
expect_fail "forked push origin"

reset_fixture
git -C "$test_root/repo" remote add origin https://github.com/osauer/canary.git
git -C "$test_root/repo" remote set-url --add --push origin https://github.com/osauer/canary.git
git -C "$test_root/repo" remote set-url --add --push origin git@github.com:osauer/canary.git
expect_fail "multiple push destinations"

reset_fixture
git -C "$test_root/repo" remote add origin https://github.com/osauer/canary.git
git -C "$test_root/repo" config \
	url.https://example.invalid/fork/.insteadOf \
	https://github.com/osauer/canary
expect_fail "insteadOf fetch rewrite"

reset_fixture
git -C "$test_root/repo" remote add origin https://github.com/osauer/canary.git
git -C "$test_root/repo" config \
	url.https://example.invalid/fork/.pushInsteadOf \
	https://github.com/osauer/canary
expect_fail "pushInsteadOf rewrite"

echo "check-release-origin test: OK"
