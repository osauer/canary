#!/usr/bin/env bash

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$repo_root/scripts/check-changelog-entry.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-changelog-entry-test.XXXXXX")"
cleanup() {
	rm -rf -- "$test_root"
}
trap cleanup EXIT HUP INT TERM

fixture="$test_root/CHANGELOG.md"
cat >"$fixture" <<'EOF'
# Changelog
All notable changes to this project are documented here. The project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html), and release entries follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories (Added / Changed / Deprecated / Removed / Fixed / Security).

## v9.8.7 — 2001-02-03 04:05 UTC

### What's new

Historical recovery uses the release tag's own public record.

### Fixed

- Preserved exact release notes during delayed recovery.
EOF

RELEASE_VERSION=v9.8.7 CHANGELOG_PATH="$fixture" CHANGELOG_HISTORICAL=1 \
	"$checker" >/dev/null

if RELEASE_VERSION=v9.8.7 CHANGELOG_PATH="$fixture" \
	"$checker" >"$test_root/output" 2>&1; then
	echo "check-changelog-entry test: current release accepted historical date" >&2
	exit 1
fi

if RELEASE_VERSION=v1.2.3 CHANGELOG_PATH="$fixture" CHANGELOG_HISTORICAL=1 \
	"$checker" >"$test_root/output" 2>&1; then
	echo "check-changelog-entry test: historical mode accepted wrong top version" >&2
	exit 1
fi

ln -s "$fixture" "$test_root/symlink.md"
if RELEASE_VERSION=v9.8.7 CHANGELOG_PATH="$test_root/symlink.md" CHANGELOG_HISTORICAL=1 \
	"$checker" >"$test_root/output" 2>&1; then
	echo "check-changelog-entry test: historical mode accepted symlink input" >&2
	exit 1
fi

if RELEASE_VERSION=v9.8.7 CHANGELOG_PATH="$fixture" CHANGELOG_HISTORICAL=yes \
	"$checker" >"$test_root/output" 2>&1; then
	echo "check-changelog-entry test: accepted invalid historical mode" >&2
	exit 1
fi

if make -s -C "$repo_root" changelog-lint \
	RELEASE_VERSION=v9.8.7 \
	CHANGELOG_PATH="$fixture" \
	CHANGELOG_HISTORICAL=1 >"$test_root/output" 2>&1; then
	echo "check-changelog-entry test: Make override bypassed current-release authority" >&2
	exit 1
fi

echo "check-changelog-entry test: PASS"
