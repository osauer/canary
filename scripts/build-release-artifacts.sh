#!/usr/bin/env bash
# Assemble release assets while cwd is an isolated checkout of the exact tag.

set -euo pipefail

mode="${1:?usage: build-release-artifacts.sh <all|mcpb|checksums> <version> <dist-dir> <targets> [jobs] [strip-ldflags]}"
version="${2:?release version required}"
dist_dir="${3:?dist dir required}"
targets="${4:?release targets required}"
jobs="${5:-1}"
strip_ldflags="${6:--s -w}"
ownership_marker=".canary-release-output"

case "$mode" in all|mcpb|checksums) ;; *) echo "build-release-artifacts: invalid mode: $mode" >&2; exit 2 ;; esac
case "$version" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "build-release-artifacts: invalid version: $version" >&2; exit 2 ;; esac
case "$dist_dir" in /*) ;; *) echo "build-release-artifacts: dist directory must be absolute" >&2; exit 2 ;; esac

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
owner_repo="$(git -C "$script_dir" rev-parse --show-toplevel)"
owner_repo="$(cd "$owner_repo" && pwd -P)"
expected_dist="$owner_repo/dist"
dist_leaf="${dist_dir##*/}"
dist_parent="${dist_dir%/*}"
if [ "$dist_leaf" != "dist" ] || [ ! -d "$dist_parent" ]; then
	echo "build-release-artifacts: dist directory must be the caller repository output: $expected_dist" >&2
	exit 2
fi
dist_dir="$(cd "$dist_parent" && pwd -P)/$dist_leaf"
if [ "$dist_dir" != "$expected_dist" ]; then
	echo "build-release-artifacts: dist directory must be the caller repository output: $expected_dist" >&2
	exit 2
fi
if [ -L "$dist_dir" ]; then
	echo "build-release-artifacts: refusing symlink dist directory: $dist_dir" >&2
	exit 2
fi
case "$jobs" in ''|*[!0-9]*) echo "build-release-artifacts: jobs must be a positive integer" >&2; exit 2 ;; esac
if [ "$jobs" -lt 1 ]; then
	echo "build-release-artifacts: jobs must be a positive integer" >&2
	exit 2
fi

ownership_record() {
	printf '%s\n' \
		'format=canary-release-output-v1' \
		"repository=$owner_repo" \
		"path=$expected_dist"
}

require_owned_dist() {
	local marker="$dist_dir/$ownership_marker" expected actual
	if [ ! -d "$dist_dir" ] || [ -L "$dist_dir" ]; then
		echo "build-release-artifacts: existing output is not an owned directory: $dist_dir" >&2
		exit 1
	fi
	if [ ! -f "$marker" ] || [ -L "$marker" ]; then
		echo "build-release-artifacts: existing output lacks a regular ownership marker: $marker" >&2
		exit 1
	fi
	expected="$(ownership_record)"
	actual="$(cat "$marker")"
	if [ "$actual" != "$expected" ]; then
		echo "build-release-artifacts: existing output ownership marker does not match this repository" >&2
		exit 1
	fi
}

if [ -e "$dist_dir" ]; then
	require_owned_dist
elif [ "$mode" != "all" ]; then
	echo "build-release-artifacts: owned output does not exist; run all mode first" >&2
	exit 1
fi

tag_commit="$(git rev-parse --verify "refs/tags/$version^{commit}")"
head_commit="$(git rev-parse HEAD)"
if [ "$head_commit" != "$tag_commit" ] || [ -n "$(git status --porcelain)" ]; then
	echo "build-release-artifacts: source must be a clean checkout of $version" >&2
	exit 1
fi
release_date="$(git show -s --format=%cI HEAD)"
release_ldflags="$strip_ldflags -X main.version=$version -X main.commit=$head_commit -X main.date=$release_date"
artifact_dir="$dist_dir"
stage_dir=""
cleanup() {
	if [ -n "$stage_dir" ] && [ -d "$stage_dir" ]; then
		rm -rf -- "$stage_dir"
	fi
}
trap cleanup EXIT HUP INT TERM

build_mcpb() {
	./scripts/build-mcpb.sh "$version" "$artifact_dir" "$targets"
}

build_checksums() {
	local release_assets=() target prefix asset
	for target in $targets; do
		for prefix in canary canary-trading; do
			asset="${prefix}-${version}-${target}.tar.gz"
			if [ ! -f "$artifact_dir/$asset" ]; then
				echo "build-release-artifacts: missing $artifact_dir/$asset" >&2
				exit 1
			fi
			release_assets+=("$asset")
		done
	done
	for asset in "canary-$version.mcpb" canary.mcpb; do
		if [ ! -f "$artifact_dir/$asset" ]; then
			echo "build-release-artifacts: missing $artifact_dir/$asset" >&2
			exit 1
		fi
		release_assets+=("$asset")
	done

	expected_tarballs="$(printf '%s\n' "${release_assets[@]}" | grep -E '\.tar\.gz$' | sort)"
	actual_tarballs="$(find "$artifact_dir" -maxdepth 1 -type f -name '*.tar.gz' -exec basename {} \; | sort)"
	if [ "$actual_tarballs" != "$expected_tarballs" ]; then
		echo "build-release-artifacts: release tarball inventory is not exact" >&2
		diff -u <(printf '%s\n' "$expected_tarballs") <(printf '%s\n' "$actual_tarballs") >&2 || true
		exit 1
	fi
	expected_mcpbs="$(printf '%s\n' "${release_assets[@]}" | grep -E '\.mcpb$' | sort)"
	actual_mcpbs="$(find "$artifact_dir" -maxdepth 1 -type f -name '*.mcpb' -exec basename {} \; | sort)"
	if [ "$actual_mcpbs" != "$expected_mcpbs" ]; then
		echo "build-release-artifacts: release MCPB inventory is not exact" >&2
		diff -u <(printf '%s\n' "$expected_mcpbs") <(printf '%s\n' "$actual_mcpbs") >&2 || true
		exit 1
	fi
	(
		cd "$artifact_dir"
		shasum -a 256 "${release_assets[@]}" > SHA256SUMS
	)
	command -v gpg >/dev/null 2>&1 || {
		echo "build-release-artifacts: gpg not on PATH" >&2
		exit 1
	}
	fingerprint_file="internal/update/release-signing-key.fingerprint"
	if [ ! -f "$fingerprint_file" ] || [ -L "$fingerprint_file" ]; then
		echo "build-release-artifacts: the tag's release-signing fingerprint is missing or unsafe" >&2
		exit 1
	fi
	expected_fp="$(tr -d '[:space:]' < "$fingerprint_file")"
	if [ -z "$expected_fp" ] || ! gpg --list-secret-keys --with-colons "$expected_fp" >/dev/null 2>&1; then
		echo "build-release-artifacts: the tag's release signing key is unavailable" >&2
		exit 1
	fi
	echo "==> signing SHA256SUMS with the key pinned by $version"
	(
		cd "$artifact_dir"
		gpg --batch --yes --local-user "$expected_fp" --armor --detach-sign --output SHA256SUMS.asc SHA256SUMS
		gpg --verify SHA256SUMS.asc SHA256SUMS >/dev/null 2>&1
	)
	compact_public_key="internal/update/release-signing-key.ed25519.pem"
	if [ ! -f "$compact_public_key" ] || [ -L "$compact_public_key" ]; then
		echo "build-release-artifacts: the tag's compact release-signing public key is missing or unsafe" >&2
		exit 1
	fi
	go run ./scripts/release-sign-ed25519 \
		-public "$compact_public_key" \
		-input "$artifact_dir/SHA256SUMS" \
		-output "$artifact_dir/SHA256SUMS.ed25519"
	go run ./scripts/release-sign-ed25519 \
		-public "$compact_public_key" \
		-input "$artifact_dir/SHA256SUMS" \
		-verify "$artifact_dir/SHA256SUMS.ed25519"
}

case "$mode" in
	all)
		stage_dir="$(mktemp -d "$owner_repo/.canary-release-output.XXXXXX")"
		artifact_dir="$stage_dir"
		ownership_record > "$artifact_dir/$ownership_marker"
		printf '%s\n' $targets | xargs -P "$jobs" -I {} ./scripts/build-release-target.sh {} "$version" "$release_ldflags" "$artifact_dir"
		build_mcpb
		build_checksums
		if [ -e "$dist_dir" ]; then
			require_owned_dist
			rm -rf -- "$dist_dir"
		fi
		mv "$stage_dir" "$dist_dir"
		stage_dir=""
		;;
	mcpb)
		build_mcpb
		;;
	checksums)
		build_checksums
		;;
esac
