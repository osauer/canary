#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-source.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-source-test.XXXXXX")"
repo="$test_root/repo"
remote="$test_root/remote.git"

cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

git init -q "$repo"
git -C "$repo" config user.name "Canary Source Test"
git -C "$repo" config user.email "source-test@example.invalid"
printf '%s\n' tracked >"$repo/tracked.txt"
printf '%s\n' '/dist/' >"$repo/.gitignore"
git -C "$repo" add tracked.txt .gitignore
git -C "$repo" commit -q -m tagged
git -C "$repo" tag -a v1.2.3 -m v1.2.3
git -C "$repo" branch -M main
git -C "$repo" branch release/2.x
git -C "$repo" tag -a v2.8.5 -m v2.8.5
git init --bare -q "$remote"
git -C "$repo" remote add origin "$remote"
git -C "$repo" push -q origin main release/2.x v1.2.3 v2.8.5

(
	cd "$repo"
	"$checker" --mode tag v1.2.3 >/dev/null
	"$checker" --mode controller v1.2.3 >/dev/null
	mkdir -p dist
	printf '%s\n' ignored >dist/server.json
	"$checker" --mode tag v1.2.3 >/dev/null
	"$checker" --mode controller v1.2.3 >/dev/null
)

git -C "$repo" checkout -q release/2.x
(
	cd "$repo"
	"$checker" --mode tag v2.8.5 >/dev/null
	"$checker" --mode controller v2.8.5 >/dev/null
)
git -C "$repo" checkout -q main

expect_dirty_rejection() {
	local label="$1"
	if (
		cd "$repo"
		"$checker" --mode tag v1.2.3 >/dev/null 2>&1
	); then
		echo "check-release-source test: $label source passed" >&2
		exit 1
	fi
}

printf '%s\n' changed >>"$repo/tracked.txt"
expect_dirty_rejection "tracked"
git -C "$repo" restore tracked.txt

printf '%s\n' staged >>"$repo/tracked.txt"
git -C "$repo" add tracked.txt
expect_dirty_rejection "staged"
git -C "$repo" restore --staged tracked.txt
git -C "$repo" restore tracked.txt

printf '%s\n' untracked >"$repo/untracked.go"
expect_dirty_rejection "untracked"
rm "$repo/untracked.go"

printf '%s\n' next >>"$repo/tracked.txt"
git -C "$repo" commit -q -am next
git -C "$repo" push -q origin main
if (
	cd "$repo"
	"$checker" --mode tag v1.2.3 >/dev/null 2>&1
); then
	echo "check-release-source test: HEAD away from tag passed" >&2
	exit 1
fi
(
	cd "$repo"
	"$checker" --mode controller v1.2.3 >/dev/null
)
if (
	cd "$repo"
	"$checker" --mode controller v2.8.5 >/dev/null 2>&1
); then
	echo "check-release-source test: advanced main passed as v2 controller" >&2
	exit 1
fi
git -C "$repo" checkout -q release/2.x
(
	cd "$repo"
	"$checker" --mode controller v2.8.5 >/dev/null
)
git -C "$repo" checkout -q main

# The registry workflow's release-publication checkout: HEAD is the tag while
# origin/main has already moved on. Tag mode is the only anchor that holds.
git -C "$repo" checkout -q --detach v1.2.3
(
	cd "$repo"
	"$checker" --mode tag v1.2.3 >/dev/null
)
if (
	cd "$repo"
	"$checker" --mode controller v1.2.3 >/dev/null 2>&1
); then
	echo "check-release-source test: tag checkout passed as controller" >&2
	exit 1
fi
git -C "$repo" checkout -q main

printf '%s\n' unpushed >>"$repo/tracked.txt"
git -C "$repo" commit -q -am unpushed
if (
	cd "$repo"
	"$checker" --mode controller v1.2.3 >/dev/null 2>&1
); then
	echo "check-release-source test: unpushed controller passed" >&2
	exit 1
fi

if (
	cd "$repo"
	"$checker" --mode tag 'v1.2.3/../../unsafe' >/dev/null 2>&1
); then
	echo "check-release-source test: unsafe version passed" >&2
	exit 1
fi

# An anchor nobody named is an anchor nobody proved: every malformed mode
# argument must fail rather than fall back to a default.
expect_usage_rejection() {
	local label="$1"
	shift
	if (
		cd "$repo"
		"$checker" "$@" >/dev/null 2>&1
	); then
		echo "check-release-source test: $label passed" >&2
		exit 1
	fi
}

expect_usage_rejection "bare version" v1.2.3
expect_usage_rejection "legacy --controller" --controller v1.2.3
expect_usage_rejection "unknown mode" --mode origin v1.2.3
expect_usage_rejection "empty mode" --mode "" v1.2.3
expect_usage_rejection "missing version" --mode tag

echo "check-release-source test: OK"
