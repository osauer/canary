#!/usr/bin/env bash
#
# build-release-target.sh - build and package one release target.
# Called by `make release-binaries` via xargs -P so the OS/arch matrix
#   dist/canary-vX.Y.Z-<os>-<arch>.tar.gz          read-only (canonical)
#   dist/canary-trading-vX.Y.Z-<os>-<arch>.tar.gz  broker-write capable

set -euo pipefail

target="${1:?usage: build-release-target.sh <os-arch> <version> <ldflags> <dist-dir>}"
version="${2:?release version required}"
ldflags="${3:?release ldflags required}"
dist_dir="${4:?dist dir required}"

os="${target%-*}"
arch="${target#*-}"

# Variant capability is pinned by the explicit -tags flags below. Ambient
# GOFLAGS build tags would silently apply to the read-only build (which
# passes no -tags of its own) and could ship a broker-write-capable binary
if [[ "${GOFLAGS:-}" == *"-tags"* ]]; then
	echo "release build refuses ambient GOFLAGS carrying build tags (GOFLAGS=${GOFLAGS})" >&2
	exit 1
fi

for source_path in LICENSE README.md SECURITY.md docs/docs/operate/orders.md; do
	if [ ! -f "$source_path" ]; then
		echo "release source is missing required file: $source_path" >&2
		exit 1
	fi
done
if ! grep -Eq '^## Safety([[:space:]]|$)' README.md; then
	echo "release source README is missing the linked Safety section" >&2
	exit 1
fi

package_variant() {
	local prefix="$1" binary_name="$2" compiled_binary="$3" warning="$4"
	local base="${prefix}-${version}-${target}"
	local stage="${dist_dir}/${base}"
	local archive archive_inventory

	rm -rf "$stage"
	mkdir -p "$stage"
	install -m 0755 "$compiled_binary" "$stage/$binary_name"
	cp LICENSE README.md "$stage/"
	if [ "$warning" = "trading" ]; then
		cat > "$stage/TRADING-WARNING.md" << 'WARN'
# Broker-write capable build

This binary can place, modify, and cancel orders with your broker once the
trading gates in `~/.config/ibkr/config.toml` are configured. If you only
want market data, dashboards, and previews, download the standard `canary`
artefact instead — it is the same tool without order transmission compiled
in. Start with the bundled [README safety section](README.md#safety), then
read the release-pinned security and trading-preview documents below before
enabling trading.
WARN
		printf '\n- [Security policy](https://github.com/osauer/canary/blob/%s/SECURITY.md)\n- [Trading preview and execution guide](https://github.com/osauer/canary/blob/%s/docs/docs/operate/orders.md)\n' "$version" "$version" >> "$stage/TRADING-WARNING.md"
		for required in "README.md#safety" "blob/$version/SECURITY.md" "blob/$version/docs/docs/operate/orders.md"; do
			if ! grep -F "$required" "$stage/TRADING-WARNING.md" >/dev/null; then
				echo "trading warning missing release-safe reference: $required" >&2
				exit 1
			fi
		done
	fi
	( cd "$dist_dir" && tar -czf "$base.tar.gz" "$base" )
	archive="$dist_dir/$base.tar.gz"
	# Buffer the complete inventory before testing membership. Piping tar into
	# grep -q is racy under pipefail: grep may close early after a match, making
	# GNU tar report SIGPIPE and turning a present entry into a false failure.
	archive_inventory="$(tar -tzf "$archive")"
	for required_path in "$base/$binary_name" "$base/LICENSE" "$base/README.md"; do
		if ! grep -Fqx "$required_path" <<< "$archive_inventory" >/dev/null; then
			echo "release archive missing required path: $required_path" >&2
			exit 1
		fi
	done
	assert_archive_capability "$archive" "$base" "$binary_name" "$warning"
	if grep -Fqx "$base/ibkr" <<< "$archive_inventory" >/dev/null; then
		echo "release archive contains retired executable entry: $base/ibkr" >&2
		exit 1
	fi
	if [ "$warning" = "trading" ]; then
		if ! grep -Fqx "$base/TRADING-WARNING.md" <<< "$archive_inventory" >/dev/null; then
			echo "trading release archive is missing TRADING-WARNING.md" >&2
			exit 1
		fi
	else
		if grep -F 'TRADING-WARNING.md' <<< "$archive_inventory" >/dev/null; then
			echo "read-only release archive unexpectedly contains TRADING-WARNING.md" >&2
			exit 1
		fi
	fi
	rm -rf "$stage"
}

# assert_archive_capability capability-checks the exact binary inside the
# check works for every cross-compiled target. The trading archive must
# carry the trading build tag; the canonical read-only archive must not.
assert_archive_capability() {
	local archive="$1" base="$2" binary_name="$3" warning="$4"
	local check_dir="$build_root/capability-$base"
	rm -rf "$check_dir"
	mkdir -p "$check_dir"
	tar -xzf "$archive" -C "$check_dir" "$base/$binary_name"
	local settings
	settings="$(go version -m "$check_dir/$base/$binary_name")"
	if [ "$warning" = "trading" ]; then
		if ! grep -Eq 'build[[:space:]]+-tags=.*trading' <<< "$settings"; then
			echo "trading archive $archive lacks the trading build tag in its binary" >&2
			exit 1
		fi
	elif grep -Eq 'build[[:space:]]+-tags=.*trading' <<< "$settings"; then
		echo "read-only archive $archive carries a trading-tagged binary" >&2
		exit 1
	fi
	rm -rf "$check_dir"
}

build_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-target.XXXXXX")"
cleanup() {
	rm -rf "$build_root"
}
trap cleanup EXIT HUP INT TERM

echo "==> ${os}/${arch} (canonical read-only + trading)"
GOFLAGS="" GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -ldflags "$ldflags" -o "$build_root/canary" ./cmd/canary
GOFLAGS="" GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false -tags trading -ldflags "$ldflags" -o "$build_root/canary-trading" ./cmd/canary

package_variant "canary" "canary" "$build_root/canary" ""
package_variant "canary-trading" "canary" "$build_root/canary-trading" "trading"
