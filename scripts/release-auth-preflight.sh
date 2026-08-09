#!/usr/bin/env bash
#
# release-auth-preflight.sh - fail-fast auth checks before the release
#   - gh CLI auth (release-publish creates the GitHub Release page); goes
#     stale between releases and used to surface only at the last legs.
#   - The registry leg's local fallback preconditions. The normal release path
#     waits for the registry-publish Actions workflow to publish via OIDC. If
#     that workflow does not deliver, registry-publish-with-login.sh is the
#     backstop. Its GitHub device-code JWTs live only ~5 minutes (observed
#     meaningless. This preflight verifies that fallback is armed (publisher
#     a browser only if the OIDC workflow fails.

set -euo pipefail

publisher="${1:?usage: release-auth-preflight.sh <mcp-publisher> [login-method]}"
login_method="${2:-github}"
auto_login="${MCP_REGISTRY_AUTO_LOGIN:-1}"

fail() { printf 'release-auth-preflight: %s\n' "$1" >&2; exit 1; }
note() { printf 'release-auth-preflight: %s\n' "$1"; }

command -v gh >/dev/null 2>&1 || fail "gh CLI not on PATH; brew install gh"
if ! gh auth status --hostname github.com >/dev/null 2>&1; then
    fail "gh auth for github.com is invalid or expired — run 'gh auth login --hostname github.com', then retry"
fi
note "gh auth OK"

command -v "$publisher" >/dev/null 2>&1 \
    || fail "mcp-publisher not found at '$publisher' — the registry-publish leg would strand"

if [ "$auto_login" != "1" ]; then
    fail "MCP_REGISTRY_AUTO_LOGIN=0 disables the local device-code fallback if Actions OIDC fails — drop the override"
fi

token_file="${XDG_CONFIG_HOME:-$HOME/.config}/mcp-publisher/token.json"

registry_jwt_remaining_minutes() {
    # Prints whole minutes of validity left on the stored registry JWT
    python3 - "$token_file" <<'PY'
import base64, json, sys, time
try:
    with open(sys.argv[1]) as f:
        jwt = json.load(f)["token"]
    payload = jwt.split(".")[1]
    claims = json.loads(base64.urlsafe_b64decode(payload + "=" * (-len(payload) % 4)))
    exp = int(claims["exp"])
except Exception as exc:
    print(f"registry token unreadable: {exc}", file=sys.stderr)
    sys.exit(1)
print(int((exp - time.time()) // 60))
PY
}

# Stored-token state is informational only: with ~5-minute JWTs no stored
# token is expected to survive until an OIDC-failure fallback, so nothing here
# gates the release.
if remaining="$(registry_jwt_remaining_minutes 2>/dev/null)"; then
    if [ "$remaining" -gt 0 ]; then
        note "stored registry JWT has ${remaining}m left — it is not used by the normal Actions OIDC path"
    else
        note "stored registry JWT expired $((-remaining))m ago (normal: registry JWTs live ~5 minutes)"
    fi
else
    note "no readable registry JWT at $token_file (normal between releases)"
fi

note "REMINDER: the device code is needed only if the Actions OIDC workflow fails; if fallback starts near the END of the pipeline, be ready to use a browser within ~1 minute"
