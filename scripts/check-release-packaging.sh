#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."
./scripts/with-release-tag-checkout_test.sh
./scripts/build-release-target_test.sh
./scripts/build-release-artifacts_test.sh
./scripts/build-mcpb_test.sh
./scripts/install_test.sh
./scripts/check-changelog-entry_test.sh
./scripts/registry-publish-with-login_test.sh
./scripts/registry-publish-verify-first_test.sh
./scripts/check-release-origin_test.sh
./scripts/check-release-source_test.sh
./scripts/check-release-tag_test.sh
./scripts/materialize-release-tag-file_test.sh
./scripts/render-release-notes_test.sh
./scripts/check-github-release_test.sh
./scripts/prune-github-release-drafts_test.sh
./scripts/upload-release-assets_test.sh
./scripts/github-release-state_test.sh
./scripts/hydrate-github-release-assets_test.sh
./scripts/check-release-ci-contract.sh
./scripts/check-release-ci-contract_test.sh
./scripts/materialize-release-ci-contract_test.sh
./scripts/check-release-payload-inventory_test.sh
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
grep -Fq 'blob/$version/PRIVACY.md' scripts/build-mcpb.sh || {
	echo "check-release-packaging: MCP bundle privacy policy is not pinned to the release tag" >&2
	exit 1
}

echo "check-release-packaging: OK"
