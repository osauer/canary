#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

mkdir -p "$tmpdir/bin"

cat >"$tmpdir/bin/curl" <<'SH'
#!/usr/bin/env bash
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
SH
chmod +x "$tmpdir/bin/curl" "$tmpdir/bin/gh" "$tmpdir/bin/sleep" "$tmpdir/fallback"

cat >"$tmpdir/exact.json" <<'JSON'
{
  "servers": [
    {"server": {"name": "io.github.osauer/ibkr", "version": "9.8.7"}},
    {"server": {"name": "io.github.osauer/canary", "version": "9.8.7"}}
  ]
}
JSON
cat >"$tmpdir/retired-only.json" <<'JSON'
{
  "servers": [
    {"server": {"name": "io.github.osauer/ibkr", "version": "9.8.7"}},
    {"server": {"name": "io.github.osauer/canary", "version": "9.8.6"}}
  ]
}
JSON

: >"$tmpdir/fallback.log"
PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/exact.json" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" v9.8.7 "$tmpdir/fallback" \
  >"$tmpdir/exact.out" 2>&1
grep -Fq 'published exact version 9.8.7' "$tmpdir/exact.out"
test ! -s "$tmpdir/fallback.log"

: >"$tmpdir/fallback.log"
PATH="$tmpdir/bin:/opt/homebrew/bin:/usr/bin:/bin" \
  TEST_REGISTRY_PAYLOAD="$tmpdir/retired-only.json" \
  TEST_FALLBACK_LOG="$tmpdir/fallback.log" \
  "$repo_root/scripts/registry-publish-verify-first.sh" v9.8.7 "$tmpdir/fallback" \
  >"$tmpdir/retired-only.out" 2>&1
grep -Fq "registry serves '9.8.6'" "$tmpdir/retired-only.out"
grep -Fxq 'fallback' "$tmpdir/fallback.log"

echo "registry-publish-verify-first_test: OK"
