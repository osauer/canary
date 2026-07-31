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
git init --bare -q "$remote"
git -C "$repo" remote add origin "$remote"
git -C "$repo" push -q origin main v1.2.3

(
	cd "$repo"
	"$checker" v1.2.3 >/dev/null
	"$checker" --controller v1.2.3 >/dev/null
	mkdir -p dist
	printf '%s\n' ignored >dist/server.json
	"$checker" v1.2.3 >/dev/null
	"$checker" --controller v1.2.3 >/dev/null
)

expect_dirty_rejection() {
	local label="$1"
	if (
		cd "$repo"
		"$checker" v1.2.3 >/dev/null 2>&1
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
	"$checker" v1.2.3 >/dev/null 2>&1
); then
	echo "check-release-source test: HEAD away from tag passed" >&2
	exit 1
fi
(
	cd "$repo"
	"$checker" --controller v1.2.3 >/dev/null
)

printf '%s\n' unpushed >>"$repo/tracked.txt"
git -C "$repo" commit -q -am unpushed
if (
	cd "$repo"
	"$checker" --controller v1.2.3 >/dev/null 2>&1
); then
	echo "check-release-source test: unpushed controller passed" >&2
	exit 1
fi

if (
	cd "$repo"
	"$checker" 'v1.2.3/../../unsafe' >/dev/null 2>&1
); then
	echo "check-release-source test: unsafe version passed" >&2
	exit 1
fi

echo "check-release-source test: OK"
