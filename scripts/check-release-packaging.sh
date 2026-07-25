#!/usr/bin/env bash

set -euo pipefail

cd "$(dirname "$0")/.."
./scripts/with-release-tag-checkout_test.sh
./scripts/build-release-target_test.sh
./scripts/release-paper-smoke_test.sh

for path in SECURITY.md docs/docs/operate/orders.md; do
	grep -Fq "blob/__VERSION__/$path" .github/release-notes-template.md || {
		echo "check-release-packaging: release notes do not pin $path to the release tag" >&2
		exit 1
	}
done
if grep -Eq 'github\.com/osauer/ibkr/blob/(main|master)/' .github/release-notes-template.md; then
	echo "check-release-packaging: release notes contain a moving branch link" >&2
	exit 1
fi
grep -Fq 'blob/$version/PRIVACY.md' scripts/build-mcpb.sh || {
	echo "check-release-packaging: MCP bundle privacy policy is not pinned to the release tag" >&2
	exit 1
}

echo "check-release-packaging: OK"
