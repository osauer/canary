#!/usr/bin/env bash

# The published inventory gate must accept exactly the canonical release
# matrix and reject every way an assembled output can be short, padded, or
# unsigned. The narrowed-matrix case is the one that matters most: before this
# gate existed, `make release RELEASE_TARGETS=linux-amd64` assembled two
# tarballs plus the bundles, and every later step treated that as complete.

set -euo pipefail

cd "$(dirname "$0")/.."
checker="$PWD/scripts/check-release-payload-inventory.sh"
version=v9.9.9

test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-payload-inventory-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

fail() {
	echo "check-release-payload-inventory_test: $*" >&2
	exit 1
}

# Build an output directory holding one tarball pair per named target, plus
# the two bundles, plus a matching signed SHA256SUMS.
build_dist() {
	local dist="$1"
	shift
	rm -rf "$dist"
	mkdir -p "$dist"
	local target prefix asset
	for target in "$@"; do
		for prefix in canary canary-trading; do
			asset="$prefix-$version-$target.tar.gz"
			printf 'payload %s\n' "$asset" >"$dist/$asset"
		done
	done
	printf 'bundle\n' >"$dist/canary-$version.mcpb"
	printf 'bundle\n' >"$dist/canary.mcpb"
	(
		cd "$dist"
		# shellcheck disable=SC2035
		shasum -a 256 *.tar.gz *.mcpb >SHA256SUMS
	)
	printf 'signature\n' >"$dist/SHA256SUMS.asc"
}

canonical_dist="$test_root/canonical/dist"
build_dist "$canonical_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
"$checker" "$version" "$canonical_dist" >/dev/null ||
	fail "the canonical release matrix was rejected"

# The defect: a caller-narrowed RELEASE_TARGETS produces a self-consistent but
# partial output — every file present is correct and correctly checksummed.
for narrowed in "linux-amd64" "darwin-arm64 linux-amd64" "darwin-arm64 darwin-amd64 linux-amd64"; do
	narrowed_dist="$test_root/narrowed/dist"
	# shellcheck disable=SC2086
	build_dist "$narrowed_dist" $narrowed
	if "$checker" "$version" "$narrowed_dist" >/dev/null 2>&1; then
		fail "narrowed matrix [$narrowed] was accepted as a complete release"
	fi
done

# Ten files is not ten *canonical* files: padding a narrowed build back up to
# the expected count must not buy it a pass.
padded_dist="$test_root/padded/dist"
build_dist "$padded_dist" linux-amd64
for spare in canary-$version-linux-amd64.copy1.tar.gz canary-$version-linux-amd64.copy2.tar.gz \
	canary-$version-linux-amd64.copy3.tar.gz canary-$version-linux-amd64.copy4.tar.gz \
	canary-$version-linux-amd64.copy5.tar.gz canary-$version-linux-amd64.copy6.tar.gz; do
	printf 'padding\n' >"$padded_dist/$spare"
done
(
	cd "$padded_dist"
	# shellcheck disable=SC2035
	shasum -a 256 *.tar.gz *.mcpb >SHA256SUMS
)
if "$checker" "$version" "$padded_dist" >/dev/null 2>&1; then
	fail "a padded ten-file output was accepted as the canonical matrix"
fi

# A stale wider build left beside a fresh one is also not the exact set.
extra_dist="$test_root/extra/dist"
build_dist "$extra_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
printf 'stale\n' >"$extra_dist/canary-v9.9.8-linux-amd64.tar.gz"
if "$checker" "$version" "$extra_dist" >/dev/null 2>&1; then
	fail "a stale extra tarball was accepted"
fi

# SHA256SUMS is the updater's evidence; it must cover exactly the canonical
# payloads even when every file happens to be on disk.
short_sums_dist="$test_root/short-sums/dist"
build_dist "$short_sums_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
grep -v 'linux-arm64' "$short_sums_dist/SHA256SUMS" >"$short_sums_dist/SHA256SUMS.trimmed"
mv "$short_sums_dist/SHA256SUMS.trimmed" "$short_sums_dist/SHA256SUMS"
if "$checker" "$version" "$short_sums_dist" >/dev/null 2>&1; then
	fail "a SHA256SUMS missing a canonical payload was accepted"
fi

dup_sums_dist="$test_root/dup-sums/dist"
build_dist "$dup_sums_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
grep 'linux-arm64' "$dup_sums_dist/SHA256SUMS" | head -n 1 >>"$dup_sums_dist/SHA256SUMS"
if "$checker" "$version" "$dup_sums_dist" >/dev/null 2>&1; then
	fail "a SHA256SUMS with a duplicated payload line was accepted"
fi

for missing in SHA256SUMS SHA256SUMS.asc; do
	missing_dist="$test_root/missing/dist"
	build_dist "$missing_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
	rm -f "$missing_dist/$missing"
	if "$checker" "$version" "$missing_dist" >/dev/null 2>&1; then
		fail "an output missing $missing was accepted"
	fi
done

empty_sig_dist="$test_root/empty-sig/dist"
build_dist "$empty_sig_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
: >"$empty_sig_dist/SHA256SUMS.asc"
if "$checker" "$version" "$empty_sig_dist" >/dev/null 2>&1; then
	fail "an empty signature was accepted"
fi

empty_payload_dist="$test_root/empty-payload/dist"
build_dist "$empty_payload_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
: >"$empty_payload_dist/canary-trading-$version-darwin-arm64.tar.gz"
if "$checker" "$version" "$empty_payload_dist" >/dev/null 2>&1; then
	fail "an empty payload was accepted"
fi

symlink_dist="$test_root/symlink/dist"
build_dist "$symlink_dist" darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
rm -f "$symlink_dist/canary.mcpb"
ln -s "canary-$version.mcpb" "$symlink_dist/canary.mcpb"
if "$checker" "$version" "$symlink_dist" >/dev/null 2>&1; then
	fail "a symlinked payload was accepted"
fi

if "$checker" "$version" "$test_root/does-not-exist/dist" >/dev/null 2>&1; then
	fail "a missing output directory was accepted"
fi

if "$checker" not-a-version "$canonical_dist" >/dev/null 2>&1; then
	fail "a malformed release version was accepted"
fi

if "$checker" "$version" relative/dist >/dev/null 2>&1; then
	fail "a relative output directory was accepted"
fi

echo "check-release-payload-inventory_test: OK"
