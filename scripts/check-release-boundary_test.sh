#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-boundary.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-boundary-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

# The checked-in controller is the canonical positive witness.
"$checker" "$repo_root" >/dev/null

fixture="$test_root/repo"
mkdir -p "$fixture/.github"
cp "$repo_root/Makefile" "$fixture/Makefile"
cp -R "$repo_root/scripts" "$fixture/scripts"
cp -R "$repo_root/.github/workflows" "$fixture/.github/"

expect_rejected() {
	local mode="$1" relative backup="$test_root/$1.backup"
	case "$mode" in
	non-atomic) relative=Makefile ;;
	pages-all-branches) relative=.github/workflows/pages-deploy.yml ;;
	registry-main | registry-v2-main) relative=.github/workflows/registry-publish.yml ;;
	*) echo "check-release-boundary test: unknown mutation $mode" >&2; exit 2 ;;
	esac
	cp "$fixture/$relative" "$backup"
	python3 - "$fixture" "$mode" <<'PY'
import sys
from pathlib import Path

root, mode = Path(sys.argv[1]), sys.argv[2]
mutations = {
    "non-atomic": ("Makefile", "git push --no-follow-tags --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)", "git push --no-follow-tags origin $(RELEASE_VERSION)"),
    "pages-all-branches": (".github/workflows/pages-deploy.yml", "    branches: [main]", "    branches: [main, release/2.x]"),
    "registry-main": (".github/workflows/registry-publish.yml", '-sha "$release_sha" -branch "$RELEASE_BRANCH" -event push', '-sha "$release_sha" -branch main -event push'),
    "registry-v2-main": (".github/workflows/registry-publish.yml", "            v2.*) release_branch=release/2.x ;;", "            v2.*) release_branch=main ;;"),
}
relative, old, new = mutations[mode]
path = root / relative
text = path.read_text()
if old not in text:
    raise SystemExit(f"{mode} witness missing")
path.write_text(text.replace(old, new, 1))
PY
	if "$checker" "$fixture" >/dev/null 2>&1; then
		echo "check-release-boundary test: $mode mutation passed" >&2
		exit 1
	fi
	cp "$backup" "$fixture/$relative"
}

expect_rejected non-atomic
expect_rejected pages-all-branches
expect_rejected registry-main
expect_rejected registry-v2-main

echo "check-release-boundary test: OK"
