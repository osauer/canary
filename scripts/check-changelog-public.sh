#!/bin/sh
# check-changelog-public.sh — keep CHANGELOG.md source reader-facing.
set -eu

cd "$(dirname "$0")/.."

path=${CHANGELOG_PATH:-CHANGELOG.md}

expected=$(cat <<'EOF'
# Changelog
All notable changes to this project are documented here. The project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html), and release entries follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories (Added / Changed / Deprecated / Removed / Fixed / Security).
EOF
)

actual=$(awk '
  /^## v[0-9]/ { exit }
  NF { print }
' "$path")

if [ "$actual" != "$expected" ]; then
  echo "check-changelog-public: $path preamble contains non-public or unexpected prose" >&2
  echo "                        keep maintainer guidance in internal-docs/," >&2
  echo "                        not in CHANGELOG.md before the first release entry" >&2
  exit 1
fi

comment=$(grep -n '<!--' "$path" || true)
if [ -n "$comment" ]; then
  echo "check-changelog-public: $path contains HTML comments; remove template guidance before commit" >&2
  echo "$comment" >&2
  exit 1
fi

leak=$(grep -nE 'Entries tier by audience|Shape is enforced|lint-enforced|session-internal|No internal finding|make changelog-(lint|stub)' "$path" || true)
if [ -n "$leak" ]; then
  echo "check-changelog-public: $path contains maintainer-process wording" >&2
  echo "$leak" >&2
  exit 1
fi

echo "check-changelog-public: OK"
