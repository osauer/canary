#!/usr/bin/env bash
#
# registry-publish-verify-first.sh - wait for the Actions OIDC publisher to
# make exact verified release metadata visible, then run the supplied fallback.

set -euo pipefail

if [[ "$#" -lt 3 ]]; then
    echo "usage: registry-publish-verify-first.sh <vX.Y.Z> <expected-server.json> <fallback-command> [args...]" >&2
    exit 2
fi

release_version="$1"
expected_server_source="$2"
shift 2
if [[ ! "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
    echo "registry-publish: release version must look like vX.Y.Z (got $release_version)" >&2
    exit 2
fi
if [[ ! -f "$expected_server_source" || -L "$expected_server_source" ]]; then
    echo "registry-publish: expected server JSON is missing or unsafe: $expected_server_source" >&2
    exit 2
fi

expected_version="${release_version#v}"
server_name="io.github.osauer/canary"
interval_seconds=15
wait_seconds=240
max_attempts=$((wait_seconds / interval_seconds + 1))
registry_query_state=""

for command in curl python3; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "registry-publish: $command is required for exact registry verification" >&2
        exit 2
    fi
done

work="$(mktemp -d "${TMPDIR:-/tmp}/canary-registry-verify.XXXXXX")"
expected_server_json="$work/server.json"
cleanup() {
    rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

python3 - "$expected_server_source" "$expected_server_json" "$server_name" "$expected_version" <<'PY'
import hashlib
import json
import re
import sys
from pathlib import Path

source_path = Path(sys.argv[1])
output_path = Path(sys.argv[2])
expected_name = sys.argv[3]
required_version = sys.argv[4]

def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result

def reject_constant(value):
    raise ValueError(f"non-finite JSON number {value!r}")

try:
    payload = json.loads(
        source_path.read_text(encoding="utf-8"),
        object_pairs_hook=unique_object,
        parse_constant=reject_constant,
    )
except (OSError, UnicodeDecodeError, ValueError) as error:
    raise SystemExit(f"registry-publish: expected server JSON is unreadable: {error}")
if type(payload) is not dict:
    raise SystemExit("registry-publish: expected server JSON must be an object")
if payload.get("name") != expected_name:
    raise SystemExit(
        f"registry-publish: expected server JSON name must be exactly {expected_name}"
    )
version = payload.get("version")
if (
    type(version) is not str
    or re.fullmatch(
        r"[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?", version
    ) is None
    or version != required_version
):
    raise SystemExit(
        "registry-publish: expected server JSON version does not match the release"
    )

repository = payload.get("repository")
expected_repository = {
    "url": "https://github.com/osauer/canary",
    "source": "github",
    "id": "1234071553",
}
if type(repository) is not dict or repository != expected_repository:
    raise SystemExit(
        "registry-publish: expected server JSON repository identity is not canonical"
    )

packages = payload.get("packages")
if type(packages) is not list or len(packages) != 1 or type(packages[0]) is not dict:
    raise SystemExit(
        "registry-publish: expected server JSON must contain exactly one package"
    )
package = packages[0]
if set(package) != {"registryType", "identifier", "fileSha256", "transport"}:
    raise SystemExit("registry-publish: expected server JSON package fields are not exact")
if package.get("registryType") != "mcpb":
    raise SystemExit(
        "registry-publish: expected server JSON package registryType must be mcpb"
    )
if package.get("transport") != {"type": "stdio"}:
    raise SystemExit(
        "registry-publish: expected server JSON package transport must be exact stdio"
    )
expected_identifier = (
    "https://github.com/osauer/canary/releases/download/"
    f"v{version}/canary-v{version}.mcpb"
)
if package.get("identifier") != expected_identifier:
    raise SystemExit(
        "registry-publish: expected server JSON package identifier is not canonical"
    )

mcpb_path = source_path.parent / f"canary-v{version}.mcpb"
if not mcpb_path.is_file() or mcpb_path.is_symlink():
    raise SystemExit(
        f"registry-publish: verified MCPB is missing or unsafe: {mcpb_path}"
    )
try:
    mcpb_digest = hashlib.sha256(mcpb_path.read_bytes()).hexdigest()
except OSError as error:
    raise SystemExit(f"registry-publish: could not hash verified MCPB: {error}")
if package.get("fileSha256") != mcpb_digest:
    raise SystemExit(
        "registry-publish: expected server JSON fileSha256 does not match the verified MCPB"
    )

try:
    output_path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
except OSError as error:
    raise SystemExit(f"registry-publish: could not snapshot expected server JSON: {error}")
PY

registry_url="https://registry.modelcontextprotocol.io/v0.1/servers/io.github.osauer%2Fcanary/versions/$expected_version"
deadline=$((SECONDS + wait_seconds))

query_exact_registry() {
    local response

    registry_query_state="unavailable"
    if ! response="$(curl -fsS --connect-timeout 5 --max-time 10 "$registry_url" 2>/dev/null)"; then
        return 1
    fi
    registry_query_state="malformed"
    if ! printf '%s' "$response" | python3 -c '
import json
import sys
from pathlib import Path

expected_path = Path(sys.argv[1])

def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key {key!r}")
        result[key] = value
    return result

def reject_constant(value):
    raise ValueError(f"non-finite JSON number {value!r}")

expected = json.loads(
    expected_path.read_text(encoding="utf-8"),
    object_pairs_hook=unique_object,
    parse_constant=reject_constant,
)
payload = json.load(
    sys.stdin,
    object_pairs_hook=unique_object,
    parse_constant=reject_constant,
)
if type(payload) is not dict or type(payload.get("server")) is not dict:
    raise SystemExit("registry response must contain one typed server object")
server = payload["server"]
canonical = lambda value: json.dumps(
    value, ensure_ascii=False, sort_keys=True, separators=(",", ":")
)
if canonical(server) != canonical(expected):
    raise SystemExit("registry server does not match verified expected metadata")
' "$expected_server_json" 2>/dev/null; then
        return 1
    fi
    registry_query_state="exact"
    return 0
}

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
    if query_exact_registry; then
        echo "registry-publish: Actions OIDC workflow published exact version $expected_version; registry verification succeeded without local login."
        exit 0
    else
        case "$registry_query_state" in
        malformed)
            echo "registry-publish: poll $attempt/$max_attempts: exact registry record was malformed or mismatched; retrying"
            ;;
        *)
            echo "registry-publish: poll $attempt/$max_attempts: registry query failed; retrying"
            ;;
        esac
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

fallback_status=0
"$@" || fallback_status=$?
if [[ "$fallback_status" -ne 0 ]]; then
    exit "$fallback_status"
fi

# A zero-exit fallback is not publication evidence. Poll typed registry state
# again so publisher stderr, local command behavior, and eventual consistency
# cannot promote an absent or wrong Canary version into release success.
post_attempt=1
post_attempts=3
while [[ "$post_attempt" -le "$post_attempts" ]]; do
    if query_exact_registry; then
        echo "registry-publish: local fallback published exact version $expected_version; post-publish registry verification succeeded."
        exit 0
    fi
    case "$registry_query_state" in
    malformed)
        echo "registry-publish: post-publish poll $post_attempt/$post_attempts: exact registry record was malformed or mismatched" >&2
        ;;
    *)
        echo "registry-publish: post-publish poll $post_attempt/$post_attempts: registry query failed" >&2
        ;;
    esac
    if [[ "$post_attempt" -lt "$post_attempts" ]]; then
        sleep "$interval_seconds"
    fi
    post_attempt=$((post_attempt + 1))
done

echo "registry-publish: fallback returned success but exact $server_name@$expected_version is not verifiable" >&2
exit 1
