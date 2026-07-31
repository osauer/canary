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

echo "release-smoke_test: OK"
