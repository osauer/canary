#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-gate-telemetry-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM
records="$test_root/records.jsonl"
touch "$records"

run="$repo_root/scripts/run-gate-with-telemetry.sh"
"$run" --gate probe --class manual --output "$records" -- sh -c 'exit 0' >/dev/null
if "$run" --gate probe --class manual --output "$records" -- sh -c 'exit 7' >/dev/null 2>&1; then
	echo "gate telemetry test: failing command passed" >&2
	exit 1
fi
"$run" --gate probe --class manual --output "$records" --skip-marker 'wire-smoke: SKIP:' -- \
	sh -c 'echo "wire-smoke: SKIP: no gateway"' >/dev/null
"$run" --gate probe --class manual --output "$records" -- sh -c 'exit 0' >/dev/null

python3 - "$records" <<'PY'
import json
import sys

rows = [json.loads(line) for line in open(sys.argv[1], encoding="utf-8")]
assert len(rows) == 4, rows
assert [row["outcome"] for row in rows] == ["pass", "fail", "skip", "pass"]
for row in rows:
    assert "command" not in row
    assert "output" not in row
    assert "error" not in row
PY

summary=$(python3 "$repo_root/scripts/summarize-gate-telemetry.py" "$records")
printf '%s\n' "$summary" | grep -Eq '^probe manual 4 2 1 1 1 [0-9]+ [0-9]+$' || {
	echo "gate telemetry test: unexpected summary" >&2
	printf '%s\n' "$summary" >&2
	exit 1
}

echo "gate telemetry test: OK"
