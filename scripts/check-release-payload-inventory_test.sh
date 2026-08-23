#!/usr/bin/env bash

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-inventory-test.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM

version=v1.2.3
for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
	for prefix in canary canary-trading; do
		printf '%s\n' "$prefix $target" >"$test_root/$prefix-$version-$target.tar.gz"
	done
done
printf '%s\n' bundle >"$test_root/canary-$version.mcpb"
cp "$test_root/canary-$version.mcpb" "$test_root/canary.mcpb"
(
	cd "$test_root"
	shasum -a 256 \
		canary-"$version"-darwin-arm64.tar.gz \
		canary-trading-"$version"-darwin-arm64.tar.gz \
		canary-"$version"-darwin-amd64.tar.gz \
		canary-trading-"$version"-darwin-amd64.tar.gz \
		canary-"$version"-linux-amd64.tar.gz \
		canary-trading-"$version"-linux-amd64.tar.gz \
		canary-"$version"-linux-arm64.tar.gz \
		canary-trading-"$version"-linux-arm64.tar.gz \
		canary-"$version".mcpb canary.mcpb >SHA256SUMS
)
printf '%s\n' pgp-signature >"$test_root/SHA256SUMS.asc"
printf '%s\n' compact-signature >"$test_root/SHA256SUMS.ed25519"

"$repo_root/scripts/check-release-payload-inventory.sh" "$version" "$test_root" >/dev/null

mv "$test_root/SHA256SUMS.ed25519" "$test_root/SHA256SUMS.ed25519.saved"
if "$repo_root/scripts/check-release-payload-inventory.sh" "$version" "$test_root" >/dev/null 2>&1; then
	echo "check-release-payload-inventory test: accepted release without compact signature" >&2
	exit 1
fi
mv "$test_root/SHA256SUMS.ed25519.saved" "$test_root/SHA256SUMS.ed25519"

printf '%s\n' unexpected >"$test_root/unexpected.mcpb"
if "$repo_root/scripts/check-release-payload-inventory.sh" "$version" "$test_root" >/dev/null 2>&1; then
	echo "check-release-payload-inventory test: accepted unexpected payload" >&2
	exit 1
fi

echo "check-release-payload-inventory test: OK"
