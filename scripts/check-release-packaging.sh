#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."
./scripts/with-release-tag-checkout_test.sh
./scripts/build-release-target_test.sh
./scripts/build-release-artifacts_test.sh
./scripts/check-release-payload-inventory_test.sh
./scripts/build-mcpb_test.sh
./scripts/install_test.sh
./scripts/check-changelog-entry_test.sh
./scripts/registry-publish-verify-first_test.sh
./scripts/check-release-source_test.sh
./scripts/check-github-release_test.sh
./scripts/check-release-ci-contract.sh
./scripts/check-release-ci-contract_test.sh
./scripts/check-release-boundary.sh
./scripts/check-release-boundary_test.sh
./scripts/lib-daemon-control_test.sh
./scripts/release-smoke_test.sh

grep -Fq 'gh auth status --hostname github.com' scripts/release-auth-preflight.sh || {
	echo "check-release-packaging: release auth preflight does not pin github.com" >&2
	exit 1
}

for path in SECURITY.md docs/docs/operate/orders.md; do
	grep -Fq "blob/__VERSION__/$path" .github/release-notes-template.md || {
		echo "check-release-packaging: release notes do not pin $path to the release tag" >&2
		exit 1
	}
done
if grep -Eq 'github\.com/osauer/canary/blob/(main|master)/' .github/release-notes-template.md; then
	echo "check-release-packaging: release notes contain a moving branch link" >&2
	exit 1
fi
grep -Fq 'raw.githubusercontent.com/osauer/canary/main/install.sh' .github/release-notes-template.md || {
	echo "check-release-packaging: release notes do not use the canonical Canary installer" >&2
	exit 1
}
if grep -Eq 'raw\.githubusercontent\.com/osauer/ibkr/' .github/release-notes-template.md; then
	echo "check-release-packaging: release notes still install from the legacy repository" >&2
	exit 1
fi
notes_test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-notes-test.XXXXXX")"
trap 'rm -rf "$notes_test_root"' EXIT HUP INT TERM
./scripts/render-release-notes.sh v2.8.4 CHANGELOG.md .github/release-notes-template.md "$notes_test_root/v2.md"
./scripts/render-release-notes.sh v3.0.0 CHANGELOG.md .github/release-notes-template.md "$notes_test_root/v3.md"
grep -Fq 'github.com/osauer/canary/v2/cmd/canary@v2.8.4' "$notes_test_root/v2.md" || { echo "check-release-packaging: v2 Go install is not release-pinned" >&2; exit 1; }
grep -Fq 'public Go module remains on its maintained v2 line' "$notes_test_root/v3.md" || { echo "check-release-packaging: v3 does not explain the v2 Go module" >&2; exit 1; }
! grep -Fq 'github.com/osauer/canary/v2/cmd/canary@v3.' "$notes_test_root/v3.md" || { echo "check-release-packaging: invalid cross-major Go install" >&2; exit 1; }
! grep -Eq '__GO_INSTALL__|__VERSION__|__HIGHLIGHTS__' "$notes_test_root/v2.md" "$notes_test_root/v3.md" || { echo "check-release-packaging: unresolved notes marker" >&2; exit 1; }
grep -Fq 'blob/$version/PRIVACY.md' scripts/build-mcpb.sh || {
	echo "check-release-packaging: MCP bundle privacy policy is not pinned to the release tag" >&2
	exit 1
}

echo "check-release-packaging: OK"
