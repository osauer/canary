#!/usr/bin/env bash
#
# registry-publish-verify-first.sh - wait for the Actions OIDC publisher to
# make an exact release version visible, then run the supplied local fallback.

set -euo pipefail

if [[ "$#" -lt 2 ]]; then
    echo "usage: registry-publish-verify-first.sh <vX.Y.Z> <fallback-command> [args...]" >&2
    exit 2
fi

release_version="$1"
shift
if [[ ! "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
    echo "registry-publish: release version must look like vX.Y.Z (got $release_version)" >&2
    exit 2
fi

expected_version="${release_version#v}"
server_name="io.github.osauer/canary"
registry_url="https://registry.modelcontextprotocol.io/v0/servers?search=osauer&version=latest"
interval_seconds=15
wait_seconds=240
max_attempts=$((wait_seconds / interval_seconds + 1))
deadline=$((SECONDS + wait_seconds))

workflow_status() {
    local output

    if ! output="$(python3 - "$release_version" <<'PY'
import json
import subprocess
import sys

try:
    result = subprocess.run(
        [
            "gh", "run", "list",
            "--workflow", "registry-publish.yml",
            "--branch", sys.argv[1],
            "--limit", "1",
            "--json", "status,conclusion,url",
        ],
        capture_output=True,
        text=True,
        timeout=5,
        check=False,
    )
except (OSError, subprocess.TimeoutExpired):
    sys.exit(1)

if result.returncode != 0:
    sys.exit(result.returncode)
try:
    runs = json.loads(result.stdout)
except json.JSONDecodeError:
    sys.exit(1)
if runs:
    run = runs[0]
    print(f'{run.get("status", "unknown")}/{run.get("conclusion") or "pending"} {run.get("url", "")}'.rstrip())
PY
    )"; then
        echo "registry-publish: Actions status unavailable for $release_version (continuing registry poll)"
        return
    fi

    if [[ -n "$output" ]]; then
        echo "registry-publish: Actions registry-publish for $release_version: $output"
    else
        echo "registry-publish: Actions registry-publish run for $release_version not visible yet"
    fi
}

if command -v gh >/dev/null 2>&1; then
    have_gh=1
else
    have_gh=0
    echo "registry-publish: gh not available; workflow status will be skipped (registry polling continues)"
fi

attempt=1
while [[ "$attempt" -le "$max_attempts" ]]; do
    response=""
    if response="$(curl -fsS --connect-timeout 5 --max-time 10 "$registry_url" 2>/dev/null)"; then
        observed_version=""
        if observed_version="$(printf '%s' "$response" | python3 -c '
import json
import sys

name = sys.argv[1]
expected = sys.argv[2]
payload = json.load(sys.stdin)
versions = [
    item.get("server", {}).get("version", "")
    for item in payload.get("servers", [])
    if item.get("server", {}).get("name") == name
]
print(expected if expected in versions else (versions[0] if versions else ""))
' "$server_name" "$expected_version" 2>/dev/null)"; then
            if [[ "$observed_version" == "$expected_version" ]]; then
                echo "registry-publish: Actions OIDC workflow published exact version $expected_version; registry verification succeeded without local login."
                exit 0
            fi
            echo "registry-publish: poll $attempt/$max_attempts: registry serves '${observed_version:-no matching entry}', waiting for exact version $expected_version"
        else
            echo "registry-publish: poll $attempt/$max_attempts: registry response was unreadable; retrying"
        fi
    else
        echo "registry-publish: poll $attempt/$max_attempts: registry query failed; retrying"
    fi

    if [[ "$have_gh" -eq 1 ]] && (( (attempt - 1) % 4 == 0 )); then
        workflow_status
    fi

    if [[ "$attempt" -ge "$max_attempts" ]] || [[ "$SECONDS" -ge "$deadline" ]]; then
        break
    fi

    remaining=$((deadline - SECONDS))
    delay="$interval_seconds"
    if [[ "$remaining" -lt "$delay" ]]; then
        delay="$remaining"
    fi
    if [[ "$delay" -gt 0 ]]; then
        sleep "$delay"
    fi
    attempt=$((attempt + 1))
done

echo >&2
echo "registry-publish: ==================================================================" >&2
echo "registry-publish: OIDC WORKFLOW DID NOT DELIVER $release_version." >&2
echo "registry-publish: FALLING BACK TO LOCAL PUBLISH; AN INTERACTIVE DEVICE CODE IS NOW REQUIRED." >&2
echo "registry-publish: ==================================================================" >&2
echo >&2

exec "$@"
