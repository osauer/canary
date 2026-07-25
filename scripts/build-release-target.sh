#!/usr/bin/env bash
#
# build-release-target.sh - build and package one release target.
#
# Called by `make release-binaries` via xargs -P so the OS/arch matrix
# can compile in parallel. Each target produces TWO artefacts:
#   dist/ibkr-vX.Y.Z-<os>-<arch>.tar.gz          read-only (no broker writes)
#   dist/ibkr-trading-vX.Y.Z-<os>-<arch>.tar.gz  broker-write capable
#
# The read-only artefact keeps the historical name so existing links and
# install.sh keep resolving to the safe variant; trading is an explicit
# download (the release notes carry the warning).

set -euo pipefail

target="${1:?usage: build-release-target.sh <os-arch> <version> <ldflags> <dist-dir>}"
version="${2:?release version required}"
ldflags="${3:?release ldflags required}"
dist_dir="${4:?dist dir required}"

os="${target%-*}"
arch="${target#*-}"

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

build_variant() {
	local prefix="$1" tags="$2"
	local base="${prefix}-${version}-${target}"
	local stage="${dist_dir}/${base}"

	rm -rf "$stage"
	mkdir -p "$stage"
	GOOS="$os" GOARCH="$arch" go build -trimpath -buildvcs=false ${tags:+-tags "$tags"} -ldflags "$ldflags" -o "$stage/ibkr" ./cmd/ibkr
	cp LICENSE README.md "$stage/"
	if [ -n "$tags" ]; then
		cat > "$stage/TRADING-WARNING.md" << 'WARN'
# Broker-write capable build

This binary can place, modify, and cancel orders with your broker once the
trading gates in `~/.config/ibkr/config.toml` are configured. If you only
want market data, dashboards, and previews, download the standard `ibkr`
artefact instead — it is the same tool without order transmission compiled
in. Start with the bundled [README safety section](README.md#safety), then
read the release-pinned security and trading-preview documents below before
enabling trading.
WARN
		printf '\n- [Security policy](https://github.com/osauer/ibkr/blob/%s/SECURITY.md)\n- [Trading preview and execution guide](https://github.com/osauer/ibkr/blob/%s/docs/docs/operate/orders.md)\n' "$version" "$version" >> "$stage/TRADING-WARNING.md"
		for required in "README.md#safety" "blob/$version/SECURITY.md" "blob/$version/docs/docs/operate/orders.md"; do
			if ! grep -F "$required" "$stage/TRADING-WARNING.md" >/dev/null; then
				echo "trading warning missing release-safe reference: $required" >&2
				exit 1
			fi
		done
	fi
	( cd "$dist_dir" && tar -czf "$base.tar.gz" "$base" )
	archive="$dist_dir/$base.tar.gz"
	for required_path in "$base/ibkr" "$base/LICENSE" "$base/README.md"; do
		if ! tar -tzf "$archive" | grep -Fqx "$required_path"; then
			echo "release archive missing required path: $required_path" >&2
			exit 1
		fi
	done
	if [ -n "$tags" ]; then
		if ! tar -tzf "$archive" | grep -Fqx "$base/TRADING-WARNING.md"; then
			echo "trading release archive is missing TRADING-WARNING.md" >&2
			exit 1
		fi
	else
		if tar -tzf "$archive" | grep -Fq 'TRADING-WARNING.md'; then
			echo "read-only release archive unexpectedly contains TRADING-WARNING.md" >&2
			exit 1
		fi
	fi
	rm -rf "$stage"
}

echo "==> ${os}/${arch} (read-only + trading)"
build_variant "ibkr" ""
build_variant "ibkr-trading" "trading"
