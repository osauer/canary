#!/usr/bin/env bash

# release_smoke_wait_for_task_absent polls a typed status provider until every
# named background task is absent. Task names arrive as ONE space-separated
# argument, so callers can settle on a whole fan-out family in a single wait.
# The provider is a shell function or command that receives one variable name
# and writes the JSON payload with printf -v. Malformed or unavailable status
# is never interpreted as settled.
release_smoke_wait_for_task_absent() {
    local provider="${1:?status provider required}"
    local task_names="${2:?background task name(s) required}"
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
        if "$provider" payload && release_smoke_status_task_absent "$task_names" "$payload"; then
            return 0
        fi
        if (( attempt < attempts )); then
            "$sleeper" 1
        fi
    done
    return 1
}

# release_smoke_settle_or_fail waits for the named background tasks to drain
# and then separates the two ways that wait can end. The smoke daemon starts
# cold, so a full S&P-500 fan-out routinely outlasts any budget a release
# should sit on; a task that is still legitimately running says the session is
# slow, not that the artifact is broken, and the caller proceeds against the
# unsettled session — naming what is still running, because the read that
# follows contends with exactly those tasks and its failure is otherwise
# indistinguishable from a broken artifact (the 2026-08-03 v2.7.0 fire aborts
# looked like a wedged gateway until the drain trace was read). A status
# surface that stays unreadable or malformed is the case this guard exists to
# catch, and it stays fatal.
release_smoke_settle_or_fail() {
    local provider="${1:?status provider required}"
    local task_names="${2:?background task name(s) required}"
    local attempts="${3:?poll attempt count required}"
    local stage="${4:?stage label required}"
    local sleeper="${5:-sleep}"
    local payload=""
    local rc=0
    local remaining=""

    release_smoke_wait_for_task_absent "$provider" "$task_names" "$attempts" "$sleeper" || rc=$?
    if (( rc == 0 )); then
        return 0
    fi
    if (( rc != 1 )); then
        return "$rc"
    fi
    if ! "$provider" payload || ! release_smoke_status_readable "$payload"; then
        echo "release-smoke: FAIL: status stayed unreadable or malformed for $attempts polls before $stage" >&2
        return 1
    fi
    remaining="$(release_smoke_status_tasks_present "$task_names" "$payload")"
    echo "    still draining after $attempts polls (remaining: ${remaining:-none visible}); proceeding to $stage against the unsettled session"
    return 0
}

release_smoke_status_readable() {
    printf '%s' "$1" | python3 -c '
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
sys.exit(0)
' 2>/dev/null
}

release_smoke_status_task_absent() {
    local task_names="$1"
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
watched = set(sys.argv[1].split())
for task in tasks:
    if not isinstance(task, dict) or not isinstance(task.get("name"), str):
        sys.exit(1)
    if task["name"] in watched:
        sys.exit(1)
sys.exit(0)
' "$task_names" 2>/dev/null
}

# release_smoke_status_tasks_present prints the comma-joined subset of the
# named tasks that are still listed in the payload, in the caller's order.
# Malformed status prints nothing and returns 1.
release_smoke_status_tasks_present() {
    local task_names="$1"
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
running = set()
for task in tasks:
    if not isinstance(task, dict) or not isinstance(task.get("name"), str):
        sys.exit(1)
    running.add(task["name"])
print(",".join(n for n in sys.argv[1].split() if n in running))
' "$task_names" 2>/dev/null
}
