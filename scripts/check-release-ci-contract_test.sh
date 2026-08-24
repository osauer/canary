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
	git -C "$repo_root" ls-files --cached --others --exclude-standard | while IFS= read -r path; do
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

# The binding check job runs reduction-metrics-check against the locked
# baseline commit, so a shallow checkout cannot provide its evidence.
copy_fixture shallow-checkout
python3 - "$fixture/.github/workflows/ci.yml" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = """      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
"""
if old not in text:
    raise SystemExit("full-history check checkout witness missing")
path.write_text(text.replace(old, "      - uses: actions/checkout@v4\n", 1))
PY
if "$checker" "$fixture" >/dev/null 2>&1; then
	echo "check-release-ci-contract test: shallow check checkout passed" >&2
	exit 1
fi

# The release contract must prove every cross-compile target and build mode,
# not merely recognize the job's display name.
copy_fixture incomplete-cross-compile
python3 - "$fixture/.github/workflows/ci.yml" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = "for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do"
if old not in text:
    raise SystemExit("cross-compile target witness missing")
path.write_text(text.replace(old, "for target in darwin-arm64 darwin-amd64 linux-amd64; do", 1))
PY
if "$checker" "$fixture" >/dev/null 2>&1; then
	echo "check-release-ci-contract test: incomplete cross-compile matrix passed" >&2
	exit 1
fi

echo "check-release-ci-contract test: OK"
