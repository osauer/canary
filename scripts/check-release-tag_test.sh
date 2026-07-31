#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-tag.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-tag-test.XXXXXX")"
remote="$test_root/remote.git"
work="$test_root/work"

cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

git init --bare -q "$remote"
git init -q "$work"
git -C "$work" config user.name "Canary Release Test"
git -C "$work" config user.email "release-test@example.invalid"
git -C "$work" remote add origin "$remote"
mkdir -p "$work/.claude-plugin"
printf '%s\n' '{"name":"canary"}' >"$work/.claude-plugin/plugin.json"

printf '%s\n' base >"$work/evidence.txt"
git -C "$work" add evidence.txt .claude-plugin/plugin.json
git -C "$work" commit -q -m base
base_commit="$(git -C "$work" rev-parse HEAD)"
printf '%s\n' candidate >>"$work/evidence.txt"
git -C "$work" commit -q -am candidate
candidate_commit="$(git -C "$work" rev-parse HEAD)"
git -C "$work" tag -a v1.2.3 -m v1.2.3
git -C "$work" push -q origin HEAD:main v1.2.3

(
	cd "$work"
	"$checker" v1.2.3 >/dev/null
)

git -C "$work" tag v1.2.4
git -C "$work" push -q origin v1.2.4
if (
	cd "$work"
	"$checker" v1.2.4 >/dev/null 2>&1
); then
	echo "check-release-tag test: lightweight tag passed" >&2
	exit 1
fi

git -C "$work" tag -a v1.2.5 "$base_commit" -m v1.2.5
git -C "$work" push -q origin v1.2.5
(
	cd "$work"
	"$checker" v1.2.5 >/dev/null
)

git -C "$work" tag -a v1.2.6 -m v1.2.6
git --git-dir="$remote" update-ref refs/tags/v1.2.6 "$base_commit"
if (
	cd "$work"
	"$checker" v1.2.6 >/dev/null 2>&1
); then
	echo "check-release-tag test: divergent remote tag passed" >&2
	exit 1
fi

git -C "$work" tag -a v1.2.7 -m v1.2.7
if (
	cd "$work"
	"$checker" v1.2.7 >/dev/null 2>&1
); then
	echo "check-release-tag test: missing remote tag passed" >&2
	exit 1
fi

git -C "$work" tag -a v1.2.9 -m local-v1.2.9
git -C "$work" tag -a remote-object-v1.2.9 -m different-remote-message
git -C "$work" push -q origin remote-object-v1.2.9
different_remote_object="$(git -C "$work" rev-parse refs/tags/remote-object-v1.2.9)"
git --git-dir="$remote" update-ref refs/tags/v1.2.9 "$different_remote_object"
if (
	cd "$work"
	"$checker" v1.2.9 >/dev/null 2>&1
); then
	echo "check-release-tag test: different remote tag object at the same commit passed" >&2
	exit 1
fi

if (
	cd "$work"
	"$checker" 'v1.2.3;true' >/dev/null 2>&1
); then
	echo "check-release-tag test: malformed version passed" >&2
	exit 1
fi

git -C "$work" tag -a canary--v1.2.3 -m canary--v1.2.3
git -C "$work" push -q origin canary--v1.2.3
(
	cd "$work"
	if [ "$("$checker" --plugin-ref v1.2.3)" != "refs/tags/canary--v1.2.3" ]; then
		echo "check-release-tag test: plugin ref derivation mismatch" >&2
		exit 1
	fi
	"$checker" --plugin-local v1.2.3 >/dev/null
	"$checker" --plugin v1.2.3 >/dev/null
)

wrong_plugin_object="$(git -C "$work" rev-parse refs/tags/v1.2.5)"
git -C "$work" tag -a v1.2.8 -m v1.2.8
git -C "$work" push -q origin v1.2.8
git -C "$work" tag -a canary--v1.2.8 -m canary--v1.2.8
git --git-dir="$remote" update-ref refs/tags/canary--v1.2.8 "$wrong_plugin_object"
if (
	cd "$work"
	"$checker" --plugin v1.2.8 >/dev/null 2>&1
); then
	echo "check-release-tag test: divergent annotated plugin tag passed" >&2
	exit 1
fi

printf '%s\n' '{"name":"../unsafe"}' >"$work/.claude-plugin/plugin.json"
git -C "$work" commit -q -am unsafe-plugin-name
git -C "$work" tag -a v1.2.10 -m v1.2.10
git -C "$work" push -q origin v1.2.10
if (
	cd "$work"
	"$checker" --plugin-ref v1.2.10 >/dev/null 2>&1
); then
	echo "check-release-tag test: unsafe plugin name passed" >&2
	exit 1
fi

if [ "$(git -C "$work" rev-parse refs/tags/v1.2.3^{commit})" != "$candidate_commit" ]; then
	echo "check-release-tag test: release tag drifted" >&2
	exit 1
fi

echo "check-release-tag test: OK"
