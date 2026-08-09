#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
checker="$repo_root/scripts/check-release-ci-contract.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/canary-release-ci-contract-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

# The real manifest, workflows, and waiter invocation form the positive case.
"$checker" "$repo_root" >/dev/null

copy_fixture() {
	fixture="$test_root/$1"
	mkdir -p "$fixture"
	git -C "$repo_root" ls-files | while IFS= read -r path; do
		[ -f "$repo_root/$path" ] || continue
		mkdir -p "$fixture/$(dirname "$path")"
		cp "$repo_root/$path" "$fixture/$path"
	done
}

# Contract/workflow drift must fail before release evidence is awaited.
copy_fixture manifest-drift
python3 - "$fixture/scripts/release-ci-contract.json" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
doc = json.loads(path.read_text())
doc["workflows"][0]["jobs"][0] += " drift"
path.write_text(json.dumps(doc))
PY
if "$checker" "$fixture" >/dev/null 2>&1; then
	echo "check-release-ci-contract test: manifest drift passed" >&2
	exit 1
fi

# The binding waiter must use the exact candidate SHA, never a literal or a
# branch approximation.
copy_fixture wrong-sha
python3 - "$fixture/Makefile" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = '-sha "$$(git rev-parse HEAD)"'
if old not in text:
    raise SystemExit("release waiter SHA witness missing")
path.write_text(text.replace(old, '-sha deadbeef', 1))
PY
if "$checker" "$fixture" >/dev/null 2>&1; then
	echo "check-release-ci-contract test: inexact candidate SHA passed" >&2
	exit 1
fi

echo "check-release-ci-contract test: OK"
