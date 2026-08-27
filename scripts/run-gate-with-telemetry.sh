#!/usr/bin/env bash

set -euo pipefail

usage() {
	cat <<'EOF'
usage: scripts/run-gate-with-telemetry.sh --gate NAME --class NAME [--output PATH] [--skip-marker TEXT] -- COMMAND [ARG ...]

Run one gate, preserve its exit status, and append a redacted JSON record. The
record contains no command line or command output. PATH defaults to the
worktree-specific Git metadata directory.
EOF
}

gate=
change_class=
output=
skip_marker=
while [ "$#" -gt 0 ]; do
	case "$1" in
	--gate) gate=${2:-}; shift 2 ;;
	--class) change_class=${2:-}; shift 2 ;;
	--output) output=${2:-}; shift 2 ;;
	--skip-marker) skip_marker=${2:-}; shift 2 ;;
	--) shift; break ;;
	-h | --help) usage; exit 0 ;;
	*) echo "gate-telemetry: unknown option: $1" >&2; usage >&2; exit 2 ;;
	esac
done

for value in "$gate" "$change_class"; do
	if ! [[ "$value" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]]; then
		echo "gate-telemetry: gate and class must be lowercase allowlisted tokens" >&2
		exit 2
	fi
done
if [ "$#" -eq 0 ]; then
	echo "gate-telemetry: command is required" >&2
	exit 2
fi

if [ -z "$output" ]; then
	output=$(git rev-parse --git-path canary-gate-telemetry.jsonl)
fi
parent=$(dirname "$output")
if [ ! -d "$parent" ] || [ -L "$parent" ] || [ -L "$output" ]; then
	echo "gate-telemetry: output path is missing or unsafe: $output" >&2
	exit 2
fi

run_log=$(mktemp "${TMPDIR:-/tmp}/canary-gate-output.XXXXXX")
trap 'rm -f "$run_log"' EXIT HUP INT TERM
start_ms=$(python3 -c 'import time; print(time.monotonic_ns() // 1000000)')

set +e
"$@" 2>&1 | tee "$run_log"
status=${PIPESTATUS[0]}
set -e

end_ms=$(python3 -c 'import time; print(time.monotonic_ns() // 1000000)')
duration_ms=$((end_ms - start_ms))
outcome=pass
failure_code=none
if [ "$status" -ne 0 ]; then
	outcome=fail
	failure_code=command_failed
elif [ -n "$skip_marker" ] && grep -Fq -- "$skip_marker" "$run_log"; then
	outcome=skip
	failure_code=declared_skip
fi

sha=$(git rev-parse --short=12 HEAD 2>/dev/null || printf none)
timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
record=$(printf '{"schema":1,"timestamp":"%s","sha":"%s","gate":"%s","change_class":"%s","duration_ms":%d,"outcome":"%s","failure_code":"%s","exit_code":%d}' \
	"$timestamp" "$sha" "$gate" "$change_class" "$duration_ms" "$outcome" "$failure_code" "$status")
if ! printf '%s\n' "$record" >>"$output"; then
	echo "gate-telemetry: could not append redacted record to $output" >&2
	exit 74
fi
printf 'gate-telemetry: gate=%s class=%s outcome=%s duration_ms=%d\n' \
	"$gate" "$change_class" "$outcome" "$duration_ms"
exit "$status"
