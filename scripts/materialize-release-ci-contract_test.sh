#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-ci-materialize-test.XXXXXX")"
fixture="$test_root/repo"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

mkdir -p "$fixture/scripts" "$fixture/.github/workflows" "$test_root/output"
cp "$repo_root/scripts/materialize-release-ci-contract.py" "$fixture/scripts/"
cp "$repo_root/scripts/check-release-ci-contract.sh" "$fixture/scripts/"
chmod 0755 "$fixture/scripts/check-release-ci-contract.sh"
git -C "$fixture" init -q
git -C "$fixture" config user.name "Canary Test"
git -C "$fixture" config user.email "test@canary.invalid"

write_ci_workflow() {
	cat >"$fixture/.github/workflows/ci.yml" <<'EOF'
name: ci

on:
  push:
    branches: [main]

jobs:
  test:
    name: test
    runs-on: ubuntu-latest
EOF
}

write_pages_workflow() {
	cat >"$fixture/.github/workflows/pages-check.yml" <<'EOF'
name: pages check

on:
  push:
    branches: [main]

jobs:
  links:
    name: links
    runs-on: ubuntu-latest
EOF
}

write_contract() {
	include_pages="$1"
	if [ "$include_pages" = yes ]; then
		pages='
    },
    {
      "file": "pages-check.yml",
      "name": "pages check",
      "jobs": ["links"]'
	else
		pages=""
	fi
	cat >"$fixture/scripts/release-ci-contract.json" <<EOF
{
  "repository": "osauer/canary",
  "workflows": [
    {
      "file": "ci.yml",
      "name": "ci",
      "jobs": ["test"]$pages
    }
  ]
}
EOF
}

write_ci_workflow
write_pages_workflow
write_contract yes
git -C "$fixture" add .
git -C "$fixture" commit -qm tag-owned-contract
git -C "$fixture" tag -a v1.0.0 -m tag-owned-contract

# A later controller may shrink its own contract and workflow tree; recovery
# must still select and validate the old tag's complete authority.
rm "$fixture/.github/workflows/pages-check.yml"
write_contract no
git -C "$fixture" add -A
git -C "$fixture" commit -qm current-controller-shrank
python3 "$fixture/scripts/materialize-release-ci-contract.py" \
	v1.0.0 "$test_root/output/v1.0.0.json" >/dev/null
grep -Fq '"file": "pages-check.yml"' "$test_root/output/v1.0.0.json"

# Contractless tags fail unless their exact commit has a pinned legacy entry.
rm "$fixture/scripts/release-ci-contract.json"
git -C "$fixture" add -A
git -C "$fixture" commit -qm legacy-release
git -C "$fixture" tag -a v1.0.1 -m legacy-release
if python3 "$fixture/scripts/materialize-release-ci-contract.py" \
	v1.0.1 "$test_root/output/rejected.json" >/dev/null 2>&1; then
	echo "materialize-release-ci-contract test: unknown legacy tag passed" >&2
	exit 1
fi

legacy_sha="$(git -C "$fixture" rev-parse 'v1.0.1^{commit}')"
cat >"$fixture/scripts/release-ci-legacy-contracts.json" <<EOF
{
  "$legacy_sha": {
    "repository": "osauer/canary",
    "workflows": [
      {
        "file": "ci.yml",
        "name": "ci",
        "jobs": ["test"]
      }
    ]
  }
}
EOF
python3 "$fixture/scripts/materialize-release-ci-contract.py" \
	v1.0.1 "$test_root/output/v1.0.1.json" >/dev/null

# The legacy allowlist is commit-keyed, not version-shaped or name-based.
printf '%s\n' new-commit >"$fixture/README.md"
git -C "$fixture" add README.md scripts/release-ci-legacy-contracts.json
git -C "$fixture" commit -qm unknown-legacy-release
git -C "$fixture" tag -a v1.0.2 -m unknown-legacy-release
if python3 "$fixture/scripts/materialize-release-ci-contract.py" \
	v1.0.2 "$test_root/output/rejected.json" >/dev/null 2>&1; then
	echo "materialize-release-ci-contract test: unlisted legacy SHA passed" >&2
	exit 1
fi

# A tag-owned contract cannot omit an additional push-to-main workflow.
write_contract no
cat >"$fixture/.github/workflows/extra.yml" <<'EOF'
name: extra

on:
  push:
    branches: [main]

jobs:
  extra:
    name: extra
    runs-on: ubuntu-latest
EOF
git -C "$fixture" add scripts/release-ci-contract.json .github/workflows/extra.yml
git -C "$fixture" commit -qm mismatched-tag-contract
git -C "$fixture" tag -a v1.0.3 -m mismatched-tag-contract
if python3 "$fixture/scripts/materialize-release-ci-contract.py" \
	v1.0.3 "$test_root/output/rejected.json" >/dev/null 2>&1; then
	echo "materialize-release-ci-contract test: incomplete tag contract passed" >&2
	exit 1
fi

echo "materialize-release-ci-contract test: OK"
