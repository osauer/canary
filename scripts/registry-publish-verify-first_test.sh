#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

mkdir -p "$tmpdir/bin"

cat >"$tmpdir/bin/curl" <<'SH'
#!/usr/bin/env bash
if [ "${TEST_CURL_FAIL:-0}" = "1" ]; then
  exit 22
fi
printf '%s\n' "$*" >>"$TEST_CURL_LOG"
cat "$TEST_REGISTRY_PAYLOAD"
SH
cat >"$tmpdir/bin/gh" <<'SH'
#!/usr/bin/env bash
exit 1
SH
cat >"$tmpdir/bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"$tmpdir/fallback" <<'SH'
#!/usr/bin/env bash
printf 'fallback\n' >>"$TEST_FALLBACK_LOG"
if [ -n "${TEST_FALLBACK_RESULT_PAYLOAD:-}" ]; then
	cp "$TEST_FALLBACK_RESULT_PAYLOAD" "$TEST_REGISTRY_PAYLOAD"
fi
SH
chmod +x "$tmpdir/bin/curl" "$tmpdir/bin/gh" "$tmpdir/bin/sleep" "$tmpdir/fallback"

printf '%s\n' 'verified mcpb bytes' >"$tmpdir/canary-v9.8.7.mcpb"
python3 - "$tmpdir" <<'PY'
import copy
import hashlib
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
version = "9.8.7"
digest = hashlib.sha256((root / f"canary-v{version}.mcpb").read_bytes()).hexdigest()
server = {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
    "name": "io.github.osauer/canary",
    "title": "Canary MCP",
    "description": "fixture",
    "version": version,
    "websiteUrl": "https://osauer.dev/canary/",
    "repository": {
        "url": "https://github.com/osauer/canary",
        "source": "github",
        "id": "1234071553",
    },
    "packages": [
        {
            "registryType": "mcpb",
            "identifier": (
                "https://github.com/osauer/canary/releases/download/"
                f"v{version}/canary-v{version}.mcpb"
            ),
            "fileSha256": digest,
            "transport": {"type": "stdio"},
        }
    ],
}

def write(name, payload):
    (root / name).write_text(json.dumps(payload), encoding="utf-8")

write("server.json", server)
write("exact.json", {"server": server, "_meta": {"fixture": True}})

mismatch = copy.deepcopy(server)
mismatch["name"] = "io.github.osauer/ibkr"
write("mismatch.json", {"server": mismatch})
write("malformed.json", {"server": "conflict 409 already exists"})

wrong_digest = copy.deepcopy(server)
wrong_digest["packages"][0]["fileSha256"] = "0" * 64
write("wrong-digest.json", {"server": wrong_digest})
write("bad-expected.json", wrong_digest)
PY

: >"$tmpdir/fallback.log"
: >"$tmpdir/curl.log"
PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/exact.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/server.json" "$tmpdir/fallback" \
  >"$tmpdir/exact.out" 2>&1
grep -Fq 'published exact version 9.8.7' "$tmpdir/exact.out"
test ! -s "$tmpdir/fallback.log"
grep -Fq \
  'https://registry.modelcontextprotocol.io/v0.1/servers/io.github.osauer%2Fcanary/versions/9.8.7' \
  "$tmpdir/curl.log"

: >"$tmpdir/fallback.log"
cp "$tmpdir/mismatch.json" "$tmpdir/registry.json"
PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/registry.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_RESULT_PAYLOAD="$tmpdir/exact.json" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/server.json" "$tmpdir/fallback" \
  >"$tmpdir/mismatch.out" 2>&1
grep -Fq 'exact registry record was malformed or mismatched' "$tmpdir/mismatch.out"
grep -Fxq 'fallback' "$tmpdir/fallback.log"
grep -Fq 'post-publish registry verification succeeded' "$tmpdir/mismatch.out"

: >"$tmpdir/fallback.log"
cp "$tmpdir/mismatch.json" "$tmpdir/registry.json"
if PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/registry.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/server.json" "$tmpdir/fallback" \
  >"$tmpdir/post-publish-absent.out" 2>&1; then
  echo "registry-publish-verify-first test: zero-exit fallback passed without exact registry evidence" >&2
  exit 1
fi
grep -Fxq 'fallback' "$tmpdir/fallback.log"
grep -Fq 'fallback returned success but exact io.github.osauer/canary@9.8.7 is not verifiable' \
  "$tmpdir/post-publish-absent.out"

: >"$tmpdir/fallback.log"
cp "$tmpdir/malformed.json" "$tmpdir/registry.json"
if PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/registry.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/server.json" "$tmpdir/fallback" \
  >"$tmpdir/malformed.out" 2>&1; then
  echo "registry-publish-verify-first test: malformed registry response passed" >&2
  exit 1
fi
grep -Fq 'exact registry record was malformed or mismatched' "$tmpdir/malformed.out"

: >"$tmpdir/fallback.log"
cp "$tmpdir/exact.json" "$tmpdir/registry.json"
if PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_CURL_FAIL=1 \
  TEST_REGISTRY_PAYLOAD="$tmpdir/registry.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/server.json" "$tmpdir/fallback" \
  >"$tmpdir/unavailable.out" 2>&1; then
  echo "registry-publish-verify-first test: unavailable registry evidence passed" >&2
  exit 1
fi
grep -Fxq 'fallback' "$tmpdir/fallback.log"
grep -Fq 'fallback returned success but exact io.github.osauer/canary@9.8.7 is not verifiable' \
  "$tmpdir/unavailable.out"

: >"$tmpdir/fallback.log"
cp "$tmpdir/wrong-digest.json" "$tmpdir/registry.json"
if PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/registry.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/server.json" "$tmpdir/fallback" \
  >"$tmpdir/wrong-digest.out" 2>&1; then
  echo "registry-publish-verify-first test: wrong registry MCPB digest passed" >&2
  exit 1
fi
grep -Fxq 'fallback' "$tmpdir/fallback.log"

: >"$tmpdir/fallback.log"
if PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/exact.json" \
  TEST_CURL_LOG="$tmpdir/curl.log" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" \
  v9.8.7 "$tmpdir/bad-expected.json" "$tmpdir/fallback" \
  >"$tmpdir/bad-expected.out" 2>&1; then
  echo "registry-publish-verify-first test: unverified expected MCPB digest passed" >&2
  exit 1
fi
test ! -s "$tmpdir/fallback.log"
grep -Fq 'fileSha256 does not match the verified MCPB' "$tmpdir/bad-expected.out"

echo "registry-publish-verify-first_test: OK"
