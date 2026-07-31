#!/usr/bin/env bash

# release_smoke_wait_for_task_absent polls a typed status provider until the
# named background task is absent. The provider is a shell function or command
# that receives one variable name and writes the JSON payload with printf -v.
# Malformed or unavailable status is never interpreted as settled.
release_smoke_wait_for_task_absent() {
    local provider="${1:?status provider required}"
    local task_name="${2:?background task name required}"
    local attempts="${3:?poll attempt count required}"
    local sleeper="${4:-sleep}"
    local payload=""
    local attempt

    if [[ ! "$attempts" =~ ^[1-9][0-9]*$ ]]; then
        echo "release-smoke-wait: attempts must be a positive integer" >&2
        return 2
    fi

    for ((attempt = 1; attempt <= attempts; attempt++)); do
        payload=""
        if "$provider" payload && release_smoke_status_task_absent "$task_name" "$payload"; then
            return 0
        fi
        if (( attempt < attempts )); then
            "$sleeper" 1
        fi
    done
    return 1
}

release_smoke_status_task_absent() {
    local task_name="$1"
    local payload="$2"
    printf '%s' "$payload" | python3 -c '
import json, sys
try:
    status = json.load(sys.stdin)
except Exception:
    sys.exit(1)
tasks = status.get("background_tasks") if isinstance(status, dict) else None
if not isinstance(tasks, list):
    sys.exit(1)
for task in tasks:
    if not isinstance(task, dict) or not isinstance(task.get("name"), str):
        sys.exit(1)
    if task["name"] == sys.argv[1]:
        sys.exit(1)
sys.exit(0)
' "$task_name" 2>/dev/null
}
