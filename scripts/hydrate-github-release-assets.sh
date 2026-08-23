#!/usr/bin/env bash

# Download an existing GitHub release into a fresh private staging directory,
# verify its exact signed asset set, then replace only Canary-owned dist output.

set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 vX.Y.Z /absolute/repository/dist" >&2
	exit 2
fi

version="$1"
dist_dir="$2"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "hydrate-github-release-assets: version must look like vX.Y.Z (got $version)" >&2
	exit 2
fi
case "$dist_dir" in
/*) ;;
*)
	echo "hydrate-github-release-assets: dist directory must be absolute" >&2
	exit 2
	;;
esac

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repo_root="$(git -C "$script_dir" rev-parse --show-toplevel)"
repo_root="$(CDPATH= cd -- "$repo_root" && pwd -P)"
expected_dist="$repo_root/dist"
dist_leaf="${dist_dir##*/}"
dist_parent="${dist_dir%/*}"
if [ "$dist_leaf" != "dist" ] || [ ! -d "$dist_parent" ]; then
	echo "hydrate-github-release-assets: dist must be this repository's output: $expected_dist" >&2
	exit 2
fi
dist_dir="$(CDPATH= cd -- "$dist_parent" && pwd -P)/$dist_leaf"
if [ "$dist_dir" != "$expected_dist" ]; then
	echo "hydrate-github-release-assets: dist must be this repository's output: $expected_dist" >&2
	exit 2
fi

ownership_marker=".canary-release-output"
ownership_record() {
	printf '%s\n' \
		'format=canary-release-output-v1' \
		"repository=$repo_root" \
		"path=$expected_dist"
}
require_owned_dist() {
	local marker="$dist_dir/$ownership_marker" expected actual
	if [ ! -d "$dist_dir" ] || [ -L "$dist_dir" ]; then
		echo "hydrate-github-release-assets: existing output is not an owned directory" >&2
		exit 1
	fi
	if [ ! -f "$marker" ] || [ -L "$marker" ]; then
		echo "hydrate-github-release-assets: existing output lacks a regular ownership marker" >&2
		exit 1
	fi
	expected="$(ownership_record)"
	actual="$(cat "$marker")"
	if [ "$actual" != "$expected" ]; then
		echo "hydrate-github-release-assets: existing output ownership marker does not match this repository" >&2
		exit 1
	fi
}

if [ -e "$dist_dir" ] || [ -L "$dist_dir" ]; then
	require_owned_dist
fi

stage_dir="$(mktemp -d "$repo_root/.canary-release-output.XXXXXX")"
cleanup() {
	if [ -n "${stage_dir:-}" ] && [ -d "$stage_dir" ]; then
		rm -rf -- "$stage_dir"
	fi
}
trap cleanup EXIT HUP INT TERM
ownership_record >"$stage_dir/$ownership_marker"

set --
for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
	set -- "$@" \
		--pattern "canary-$version-$target.tar.gz" \
		--pattern "canary-trading-$version-$target.tar.gz"
done
set -- "$@" \
	--pattern "canary-$version.mcpb" \
	--pattern canary.mcpb \
	--pattern SHA256SUMS \
	--pattern SHA256SUMS.asc \
	--pattern SHA256SUMS.ed25519
gh release download "$version" --repo github.com/osauer/canary \
	--dir "$stage_dir" "$@"

"$script_dir/check-github-release.sh" "$version" "$stage_dir"

if [ -e "$dist_dir" ]; then
	require_owned_dist
	rm -rf -- "$dist_dir"
fi
mv "$stage_dir" "$dist_dir"
stage_dir=""

echo "hydrate-github-release-assets: OK version=$version assets=13"
