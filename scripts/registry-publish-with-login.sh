#!/usr/bin/env bash
#
# registry-publish-with-login.sh - publish MCP Registry metadata and recover
# from the common expired-JWT case with the publisher's interactive login flow.

set -euo pipefail

publisher="${1:?usage: registry-publish-with-login.sh <mcp-publisher> <server.json>}"
server_json="${2:?server.json path required}"
auto_login="${MCP_REGISTRY_AUTO_LOGIN:-1}"
login_method="${MCP_REGISTRY_LOGIN_METHOD:-github}"
server_name="io.github.osauer/canary"
verify_attempts=3
verify_interval_seconds=10

if [[ ! -f "$server_json" || -L "$server_json" ]]; then
    echo "registry-publish: missing $server_json" >&2
    exit 2
fi
for command in curl python3; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "registry-publish: $command is required for exact post-publish verification" >&2
        exit 2
    fi
done

work="$(mktemp -d "${TMPDIR:-/tmp}/canary-registry-publish.XXXXXX")"
tmp="$work/publisher.out"
expected_server_json="$work/server.json"
cleanup() {
    rm -rf "$work"
}
trap cleanup EXIT HUP INT TERM

expected_version="$(
    python3 - "$server_json" "$expected_server_json" "$server_name" <<'PY'
import hashlib
import json
import re
import sys
from pathlib import Path

source_path = Path(sys.argv[1])
output_path = Path(sys.argv[2])
expected_name = sys.argv[3]

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
    raise SystemExit(f"registry-publish: server JSON is unreadable: {error}")
if type(payload) is not dict:
    raise SystemExit("registry-publish: server JSON must be an object")
if payload.get("name") != expected_name:
    raise SystemExit(
        f"registry-publish: server JSON name must be exactly {expected_name}"
    )
version = payload.get("version")
if type(version) is not str or re.fullmatch(
    r"[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?", version
) is None:
    raise SystemExit("registry-publish: server JSON version must look like X.Y.Z")

repository = payload.get("repository")
expected_repository = {
    "url": "https://github.com/osauer/canary",
    "source": "github",
    "id": "1234071553",
}
if type(repository) is not dict or repository != expected_repository:
    raise SystemExit("registry-publish: server JSON repository identity is not canonical")

packages = payload.get("packages")
if type(packages) is not list or len(packages) != 1 or type(packages[0]) is not dict:
    raise SystemExit("registry-publish: server JSON must contain exactly one package")
package = packages[0]
if set(package) != {"registryType", "identifier", "fileSha256", "transport"}:
    raise SystemExit("registry-publish: server JSON package fields are not exact")
if package.get("registryType") != "mcpb":
    raise SystemExit("registry-publish: server JSON package registryType must be mcpb")
if package.get("transport") != {"type": "stdio"}:
    raise SystemExit("registry-publish: server JSON package transport must be exact stdio")
expected_identifier = (
    "https://github.com/osauer/canary/releases/download/"
    f"v{version}/canary-v{version}.mcpb"
)
if package.get("identifier") != expected_identifier:
    raise SystemExit("registry-publish: server JSON package identifier is not canonical")

mcpb_path = source_path.parent / f"canary-v{version}.mcpb"
if not mcpb_path.is_file() or mcpb_path.is_symlink():
    raise SystemExit(f"registry-publish: verified MCPB is missing or unsafe: {mcpb_path}")
try:
    mcpb_digest = hashlib.sha256(mcpb_path.read_bytes()).hexdigest()
except OSError as error:
    raise SystemExit(f"registry-publish: could not hash verified MCPB: {error}")
if package.get("fileSha256") != mcpb_digest:
    raise SystemExit(
        "registry-publish: server JSON fileSha256 does not match the verified MCPB"
    )

try:
    output_path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
except OSError as error:
    raise SystemExit(f"registry-publish: could not snapshot expected server JSON: {error}")
print(version)
PY
)"
registry_url="https://registry.modelcontextprotocol.io/v0.1/servers/io.github.osauer%2Fcanary/versions/$expected_version"

# The registry-publish GitHub Actions workflow (OIDC) may win the race while
# this script waits at the device-code prompt. Publisher prose is untrusted:
# an already-published-looking error only asks us to query typed registry
# state; the exact historical Canary record matching the verified MCPB-backed
# server document is the sole success authority.
published_already() {
    grep -Eiq 'already exists|already published|duplicate|version .+ exists|conflict|(^|[^0-9])409([^0-9]|$)' "$1"
}

registry_exact_once() {
    local response

    if ! response="$(curl -fsS --connect-timeout 5 --max-time 10 "$registry_url" 2>/dev/null)"; then
        echo "registry-publish: exact registry query failed" >&2
        return 1
    fi
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
        echo "registry-publish: exact registry record was malformed or did not match verified server/MCPB metadata" >&2
        return 1
    fi
    return 0
}

verify_exact_publication() {
    local attempt

    attempt=1
    while [[ "$attempt" -le "$verify_attempts" ]]; do
        if registry_exact_once; then
            echo "registry-publish: registry verified exact $server_name@$expected_version"
            return 0
        fi
        if [[ "$attempt" -lt "$verify_attempts" ]]; then
            sleep "$verify_interval_seconds"
        fi
        attempt=$((attempt + 1))
    done
    echo "registry-publish: exact post-publish registry verification failed" >&2
    return 1
}

status=0
"$publisher" publish "$expected_server_json" >"$tmp" 2>&1 || status=$?
if [[ "$status" -eq 0 ]]; then
    cat "$tmp"
    verify_exact_publication
    exit $?
fi
cat "$tmp" >&2

if published_already "$tmp"; then
    echo "registry-publish: publisher reported a possible existing version; requiring typed registry proof." >&2
    if verify_exact_publication; then
        exit 0
    fi
fi

if [[ "$auto_login" != "1" ]]; then
    exit "$status"
fi
if ! grep -Eiq 'unauthorized|not logged in|login|jwt|token.*expired|expired.*token|invalid.*token|token.*invalid' "$tmp"; then
    exit "$status"
fi

cat >&2 <<EOF

registry-publish: MCP Registry auth appears expired.
registry-publish: starting '$(basename "$publisher") login $login_method'.

For GitHub device flow:
  1. Open the URL printed by mcp-publisher.
  2. Enter the printed device code.
  3. Authorize the registry publisher.
  4. Leave this terminal running; publish will retry automatically.

Set MCP_REGISTRY_AUTO_LOGIN=0 to disable this retry behavior.

EOF

"$publisher" login "$login_method"
status=0
"$publisher" publish "$expected_server_json" >"$tmp" 2>&1 || status=$?
if [[ "$status" -eq 0 ]]; then
    cat "$tmp"
    verify_exact_publication
    exit $?
fi
cat "$tmp" >&2
if published_already "$tmp"; then
    echo "registry-publish: publisher reported a possible existing version after login; requiring typed registry proof." >&2
    if verify_exact_publication; then
        exit 0
    fi
fi
exit "$status"
