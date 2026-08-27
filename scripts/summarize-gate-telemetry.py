#!/usr/bin/env python3

"""Summarize Canary's redacted local gate records."""

from __future__ import annotations

from collections import defaultdict
import json
from pathlib import Path
import re
import subprocess
import sys


TOKEN = re.compile(r"^[a-z0-9][a-z0-9._-]{0,63}$")
OUTCOMES = {"pass", "fail", "skip"}
REQUIRED = {
    "schema",
    "timestamp",
    "sha",
    "gate",
    "change_class",
    "duration_ms",
    "outcome",
    "failure_code",
    "exit_code",
}


def default_path() -> Path:
    result = subprocess.run(
        ["git", "rev-parse", "--git-path", "canary-gate-telemetry.jsonl"],
        check=True,
        stdout=subprocess.PIPE,
        text=True,
    )
    return Path(result.stdout.strip())


def load(path: Path) -> list[dict[str, object]]:
    records: list[dict[str, object]] = []
    for number, raw in enumerate(path.read_text().splitlines(), 1):
        try:
            record = json.loads(raw)
        except json.JSONDecodeError as error:
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: {error}")
        if not isinstance(record, dict) or set(record) != REQUIRED:
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: unexpected schema")
        if record["schema"] != 1:
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: unsupported schema")
        if not all(
            isinstance(record[key], str) and TOKEN.fullmatch(record[key])
            for key in ("gate", "change_class", "failure_code")
        ):
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: unsafe token")
        if record["outcome"] not in OUTCOMES:
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: invalid outcome")
        if not isinstance(record["duration_ms"], int) or record["duration_ms"] < 0:
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: invalid duration")
        if not isinstance(record["exit_code"], int):
            raise SystemExit(f"gate-telemetry-summary: {path}:{number}: invalid exit code")
        records.append(record)
    return records


def percentile(values: list[int], numerator: int, denominator: int) -> int:
    ordered = sorted(values)
    index = max(0, (len(ordered) * numerator + denominator - 1) // denominator - 1)
    return ordered[index]


def main() -> int:
    if len(sys.argv) > 2:
        raise SystemExit("usage: scripts/summarize-gate-telemetry.py [JSONL]")
    path = Path(sys.argv[1]) if len(sys.argv) == 2 else default_path()
    if not path.exists():
        print(f"gate-telemetry-summary: no records at {path}")
        return 0
    records = load(path)
    if not records:
        print(f"gate-telemetry-summary: no records at {path}")
        return 0

    grouped: dict[tuple[str, str], list[dict[str, object]]] = defaultdict(list)
    for record in records:
        grouped[(str(record["gate"]), str(record["change_class"]))].append(record)

    print("gate class runs pass fail skip recovered median_ms p90_ms")
    for (gate, change_class), rows in sorted(grouped.items()):
        counts = {outcome: 0 for outcome in OUTCOMES}
        durations = []
        failed_shas: set[str] = set()
        recovered_shas: set[str] = set()
        for row in rows:
            outcome = str(row["outcome"])
            sha = str(row["sha"])
            counts[outcome] += 1
            durations.append(int(row["duration_ms"]))
            if outcome == "fail":
                failed_shas.add(sha)
            elif outcome == "pass" and sha in failed_shas:
                recovered_shas.add(sha)
        print(
            gate,
            change_class,
            len(rows),
            counts["pass"],
            counts["fail"],
            counts["skip"],
            len(recovered_shas),
            percentile(durations, 1, 2),
            percentile(durations, 9, 10),
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
