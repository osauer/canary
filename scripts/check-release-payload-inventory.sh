#!/usr/bin/env bash

# Prove that an assembled release output holds the exact published payload
# inventory the release contract fixes: for each canonical target one read-only
# and one trading tarball, plus the versioned and the floating MCP bundle, plus
# a signed SHA256SUMS covering exactly those ten payloads and nothing else.
#
# The matrix below is a constant, deliberately not read from RELEASE_TARGETS.
# A caller-narrowed matrix builds a smaller set that every assembly step then
# reports as complete, so the inventory proof has to come from somewhere the
# caller cannot move. check-release-boundary.sh pins this literal against the
# Makefile's RELEASE_TARGETS so the two can never drift apart.

set -euo pipefail

CANONICAL_RELEASE_TARGETS="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"

version="${1:?usage: check-release-payload-inventory.sh <vX.Y.Z> <dist-dir>}"
dist_dir="${2:?dist dir required}"

fail() {
	printf 'check-release-payload-inventory: %s\n' "$1" >&2
	exit 1
}

case "$version" in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*)
		echo "check-release-payload-inventory: invalid version: $version" >&2
		exit 2
		;;
esac
case "$dist_dir" in
	/*) ;;
	*)
		echo "check-release-payload-inventory: dist directory must be absolute" >&2
		exit 2
		;;
esac

if [ ! -d "$dist_dir" ] || [ -L "$dist_dir" ]; then
	fail "$dist_dir is missing or not a directory; release artifact assembly did not complete"
fi

expected_payloads="$(
	{
		for target in $CANONICAL_RELEASE_TARGETS; do
			printf '%s\n' \
				"canary-$version-$target.tar.gz" \
				"canary-trading-$version-$target.tar.gz"
		done
		printf '%s\n' "canary-$version.mcpb" canary.mcpb
	} | sort
)"

# On-disk payloads must be exactly the canonical set: no missing target, and
# no extra archive left over from an earlier or wider build.
actual_payloads="$(
	find "$dist_dir" -maxdepth 1 -type f \( -name '*.tar.gz' -o -name '*.mcpb' \) \
		-exec basename {} \; | sort
)"
if [ "$actual_payloads" != "$expected_payloads" ]; then
	echo "check-release-payload-inventory: published payload inventory is not the canonical release matrix ($CANONICAL_RELEASE_TARGETS)" >&2
	diff -u <(printf '%s\n' "$expected_payloads") <(printf '%s\n' "$actual_payloads") >&2 || true
	exit 1
fi

while IFS= read -r asset; do
	case "$asset" in
		""|*/*|.*|*[!A-Za-z0-9._-]*) fail "unsafe payload name: $asset" ;;
	esac
	[ ! -L "$dist_dir/$asset" ] || fail "$dist_dir/$asset is a symlink"
	[ -s "$dist_dir/$asset" ] || fail "$dist_dir/$asset is empty"
done <<EOF
$expected_payloads
EOF

for signed in SHA256SUMS SHA256SUMS.asc; do
	[ -f "$dist_dir/$signed" ] || fail "$dist_dir/$signed missing — the Canary updater requires signed checksums"
	[ ! -L "$dist_dir/$signed" ] || fail "$dist_dir/$signed is a symlink"
	[ -s "$dist_dir/$signed" ] || fail "$dist_dir/$signed is empty"
done

# SHA256SUMS must cover exactly the canonical payloads: a checksum file that
# omits a target, or lists one twice, is not evidence for the published set.
checksummed="$(awk 'NF { print $NF }' "$dist_dir/SHA256SUMS" | sort)"
if [ "$checksummed" != "$expected_payloads" ]; then
	echo "check-release-payload-inventory: SHA256SUMS does not cover exactly the canonical payload set" >&2
	diff -u <(printf '%s\n' "$expected_payloads") <(printf '%s\n' "$checksummed") >&2 || true
	exit 1
fi

sum_lines="$(grep -c '' "$dist_dir/SHA256SUMS")"
[ "$sum_lines" -eq 10 ] || fail "expected 10 checksummed payloads (8 tarballs + 2 MCPB; 12 published assets including checksums/signature), got $sum_lines"

echo "check-release-payload-inventory: OK ($version, 10 payloads, 12 published assets)"
