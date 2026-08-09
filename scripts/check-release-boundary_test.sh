#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-boundary.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-boundary-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

# The checked-in controller is the canonical positive witness.
"$checker" "$repo_root" >/dev/null

# Exercise the highest-risk invariant against a complete working-tree fixture:
# the version tag must be pushed atomically with the reviewed main candidate.
fixture="$test_root/repo"
mkdir -p "$fixture"
git -C "$repo_root" ls-files | while IFS= read -r path; do
	[ -f "$repo_root/$path" ] || continue
	mkdir -p "$fixture/$(dirname "$path")"
	cp "$repo_root/$path" "$fixture/$path"
done
python3 - "$fixture/Makefile" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = "git push --no-follow-tags --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)"
new = "git push --no-follow-tags origin $(RELEASE_VERSION)"
if old not in text:
    raise SystemExit("atomic release push witness missing")
path.write_text(text.replace(old, new, 1))
PY
if "$checker" "$fixture" >/dev/null 2>&1; then
	echo "check-release-boundary test: non-atomic tag publication passed" >&2
	exit 1
fi

echo "check-release-boundary test: OK"
