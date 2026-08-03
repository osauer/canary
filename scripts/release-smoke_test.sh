#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."
. ./scripts/lib-release-smoke-wait.sh

fail() {
    echo "release-smoke_test: $*" >&2
    exit 1
}

payloads=()
payload_index=0
provider_calls=0
sleep_calls=0

sequence_provider() {
    local output_var="$1"
    local value="${payloads[$payload_index]}"
    payload_index=$((payload_index + 1))
    provider_calls=$((provider_calls + 1))
    printf -v "$output_var" '%s' "$value"
}

no_sleep() {
    sleep_calls=$((sleep_calls + 1))
}

payloads=(
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[]}'
)
release_smoke_wait_for_task_absent sequence_provider breadth-spx 5 no_sleep ||
    fail "active -> active -> absent did not settle"
[[ "$provider_calls" -eq 3 && "$sleep_calls" -eq 2 ]] ||
    fail "sequence calls provider=$provider_calls sleep=$sleep_calls, want 3/2"

payloads=('{"background_tasks":[]}')
payload_index=0
provider_calls=0
sleep_calls=0
release_smoke_wait_for_task_absent sequence_provider breadth-spx 5 no_sleep ||
    fail "already-absent task did not settle immediately"
[[ "$provider_calls" -eq 1 && "$sleep_calls" -eq 0 ]] ||
    fail "immediate calls provider=$provider_calls sleep=$sleep_calls, want 1/0"

payloads=('not-json' '{"connected":true}' '{"background_tasks":"none"}')
payload_index=0
provider_calls=0
sleep_calls=0
if release_smoke_wait_for_task_absent sequence_provider breadth-spx 3 no_sleep; then
    fail "malformed status was accepted as settled"
fi
[[ "$provider_calls" -eq 3 ]] || fail "malformed provider calls=$provider_calls, want 3"

payloads=(
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[{"name":"breadth-spx"}]}'
)
payload_index=0
provider_calls=0
sleep_calls=0
downstream_calls=0
if release_smoke_wait_for_task_absent sequence_provider breadth-spx 3 no_sleep; then
    downstream_calls=$((downstream_calls + 1))
fi
[[ "$downstream_calls" -eq 0 ]] ||
    fail "permanently active task invoked downstream work"

payloads=(
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[{"name":"breadth-spx"}]}'
)
payload_index=0
provider_calls=0
sleep_calls=0
release_smoke_settle_or_fail sequence_provider breadth-spx 2 regime no_sleep ||
    fail "a still-draining fan-out blocked the release"
[[ "$provider_calls" -eq 3 ]] ||
    fail "still-draining provider calls=$provider_calls, want 3 (2 polls + 1 readability probe)"

payloads=('not-json' 'not-json' 'not-json')
payload_index=0
provider_calls=0
sleep_calls=0
if release_smoke_settle_or_fail sequence_provider breadth-spx 2 regime no_sleep; then
    fail "unreadable status was accepted as a slow fan-out"
fi

payloads=(
    '{"background_tasks":[{"name":"breadth-spx"}]}'
    '{"background_tasks":[]}'
)
payload_index=0
provider_calls=0
sleep_calls=0
release_smoke_settle_or_fail sequence_provider breadth-spx 5 regime no_sleep ||
    fail "a fan-out that drained in budget did not settle"
[[ "$provider_calls" -eq 2 ]] ||
    fail "drained provider calls=$provider_calls, want 2 (no readability probe)"

# Multi-task settle: one remaining watched task must hold the wait, and only
# the joint absence of every watched task settles it. Unwatched tasks
# (open-orders here) never hold anything.
payloads=(
    '{"background_tasks":[{"name":"breadth-spx"},{"name":"gamma-zero"}]}'
    '{"background_tasks":[{"name":"gamma-zero"},{"name":"open-orders"}]}'
    '{"background_tasks":[{"name":"open-orders"}]}'
)
payload_index=0
provider_calls=0
sleep_calls=0
release_smoke_wait_for_task_absent sequence_provider "breadth-spx gamma-zero" 5 no_sleep ||
    fail "joint absence of both watched tasks did not settle"
[[ "$provider_calls" -eq 3 && "$sleep_calls" -eq 2 ]] ||
    fail "multi-task calls provider=$provider_calls sleep=$sleep_calls, want 3/2"

payloads=(
    '{"background_tasks":[{"name":"gamma-zero"}]}'
    '{"background_tasks":[{"name":"gamma-zero"}]}'
    '{"background_tasks":[{"name":"gamma-zero"}]}'
)
payload_index=0
provider_calls=0
sleep_calls=0
release_smoke_settle_or_fail sequence_provider "breadth-spx gamma-zero" 2 chain no_sleep ||
    fail "a still-draining multi-task fan-out blocked the release"
[[ "$provider_calls" -eq 3 ]] ||
    fail "multi-task still-draining provider calls=$provider_calls, want 3 (2 polls + 1 readability probe)"

present="$(release_smoke_status_tasks_present "breadth-spx gamma-zero regime-prewarm" \
    '{"background_tasks":[{"name":"gamma-zero"},{"name":"open-orders"}]}')"
[[ "$present" == "gamma-zero" ]] ||
    fail "tasks-present reported '$present', want 'gamma-zero'"

if release_smoke_status_tasks_present "breadth-spx" 'not-json' >/dev/null; then
    fail "tasks-present accepted malformed status"
fi

echo "release-smoke_test: OK"
