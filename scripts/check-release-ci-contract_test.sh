#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
checker="$repo_root/scripts/check-release-ci-contract.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/canary-release-ci-contract-test.XXXXXX")
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

# The real manifest, workflows, and waiter invocation form the positive case.
"$checker" "$repo_root" >/dev/null

fixture="$test_root/repo"
mkdir -p "$fixture/scripts" "$fixture/.github"
cp "$repo_root/Makefile" "$fixture/Makefile"
cp "$repo_root/scripts/release-ci-contract.json" "$fixture/scripts/"
cp "$repo_root/scripts/release-ci-legacy-contracts.json" "$fixture/scripts/"
cp -R "$repo_root/.github/workflows" "$fixture/.github/"

restore_after_case() {
	path=$1
	backup="$test_root/backup"
	cp "$fixture/$path" "$backup"
	trap 'cp "$backup" "$fixture/$path"; rm -rf "$test_root"' EXIT HUP INT TERM
}

finish_case() {
	cp "$backup" "$fixture/$path"
	trap 'rm -rf "$test_root"' EXIT HUP INT TERM
}

# Contract/workflow drift must fail before release evidence is awaited.
restore_after_case scripts/release-ci-contract.json
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
finish_case

# The binding waiter must use the exact candidate SHA, never a literal or a
# branch approximation.
restore_after_case Makefile
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
finish_case

# The binding check job runs reduction-metrics-check against the locked
# baseline commit, so a shallow checkout cannot provide its evidence.
restore_after_case .github/workflows/ci.yml
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
finish_case

# Every job that invokes the historical regression spine needs the commits it
# replays, independently of the separate full-history requirement in check.
restore_after_case .github/workflows/ci.yml
python3 - "$fixture/.github/workflows/ci.yml" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = """      - uses: actions/checkout@v4
        with:
          # The regression spine proves historical fix commits are ancestors.
          fetch-depth: 0
"""
if old not in text:
    raise SystemExit("full-history regression-spine checkout witness missing")
path.write_text(text.replace(old, "      - uses: actions/checkout@v4\n", 1))
PY
if "$checker" "$fixture" >/dev/null 2>"$test_root/spine-error"; then
	echo "check-release-ci-contract test: shallow regression-spine checkout passed" >&2
	exit 1
fi
if ! grep -q "full history for regression-spine-check" "$test_root/spine-error"; then
	echo "check-release-ci-contract test: shallow regression-spine checkout reported the wrong failure" >&2
	cat "$test_root/spine-error" >&2
	exit 1
fi
finish_case

# The release contract must prove every cross-compile target and build mode,
# not merely recognize the job's display name.
restore_after_case .github/workflows/ci.yml
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
finish_case

# Cross-compilation is independent evidence. Serializing it behind check adds
# the two job durations to the release critical path without strengthening the
# exact-SHA join performed by release-ci-wait.
restore_after_case .github/workflows/ci.yml
python3 - "$fixture/.github/workflows/ci.yml" <<'PY'
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
old = "  cross-compile:\n    name: cross-compile release matrix\n    runs-on: ubuntu-latest\n"
if old not in text:
    raise SystemExit("parallel cross-compile witness missing")
path.write_text(text.replace(old, old + "    needs: check\n", 1))
PY
if "$checker" "$fixture" >/dev/null 2>&1; then
	echo "check-release-ci-contract test: serialized cross-compile passed" >&2
	exit 1
fi
finish_case

echo "check-release-ci-contract test: OK"
