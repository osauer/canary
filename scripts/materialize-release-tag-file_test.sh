#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-tag-file-test.XXXXXX")"
fixture="$test_root/repo"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

mkdir -p "$fixture/scripts" "$test_root/output"
cp "$repo_root/scripts/materialize-release-tag-file.py" "$fixture/scripts/"
git -C "$fixture" init -q
git -C "$fixture" config user.name "Canary Test"
git -C "$fixture" config user.email "test@canary.invalid"
printf '%s\n' immutable >"$fixture/regular.txt"
ln -s regular.txt "$fixture/link.txt"
git -C "$fixture" add scripts/materialize-release-tag-file.py regular.txt link.txt
git -C "$fixture" commit -qm fixture
git -C "$fixture" tag -a v1.2.3 -m fixture

python3 "$fixture/scripts/materialize-release-tag-file.py" \
	v1.2.3 regular.txt "$test_root/output/regular.txt" >/dev/null
printf '%s\n' immutable | cmp - "$test_root/output/regular.txt"

if python3 "$fixture/scripts/materialize-release-tag-file.py" \
	v1.2.3 link.txt "$test_root/output/link.txt" >/dev/null 2>&1; then
	echo "materialize-release-tag-file test: tagged symlink passed" >&2
	exit 1
fi

ln -s "$test_root/output/regular.txt" "$test_root/output/output-link"
if python3 "$fixture/scripts/materialize-release-tag-file.py" \
	v1.2.3 regular.txt "$test_root/output/output-link" >/dev/null 2>&1; then
	echo "materialize-release-tag-file test: symlink output passed" >&2
	exit 1
fi

if python3 "$fixture/scripts/materialize-release-tag-file.py" \
	v1.2.3 ../regular.txt "$test_root/output/traversal.txt" >/dev/null 2>&1; then
	echo "materialize-release-tag-file test: traversal source passed" >&2
	exit 1
fi

echo "materialize-release-tag-file test: OK"
