#!/usr/bin/env bash

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
renderer="$repo_root/scripts/render-release-notes.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-notes-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

cat >"$test_root/CHANGELOG.md" <<'EOF'
# Changelog

## v1.2.3 (2026-07-31)

### What's new

- Exact authority.

### Fixed

- Recovery.

## v1.2.2 (2026-07-30)

### What's new

- Old.
EOF
cat >"$test_root/template.md" <<'EOF'
# Canary __VERSION__

__HIGHLIGHTS__

## Details
EOF

"$renderer" v1.2.3 "$test_root/CHANGELOG.md" "$test_root/template.md" "$test_root/notes.md"
cat >"$test_root/expected.md" <<'EOF'
# Canary v1.2.3


- Exact authority.


## Details

### Fixed

- Recovery.

EOF
diff -u "$test_root/expected.md" "$test_root/notes.md"

ln -s "$test_root/CHANGELOG.md" "$test_root/changelog-link"
if "$renderer" v1.2.3 "$test_root/changelog-link" "$test_root/template.md" \
	"$test_root/rejected.md" >/dev/null 2>&1; then
	echo "render-release-notes test: symlink changelog passed" >&2
	exit 1
fi

ln -s "$test_root/notes.md" "$test_root/output-link"
if "$renderer" v1.2.3 "$test_root/CHANGELOG.md" "$test_root/template.md" \
	"$test_root/output-link" >/dev/null 2>&1; then
	echo "render-release-notes test: symlink output passed" >&2
	exit 1
fi

echo "render-release-notes test: OK"
