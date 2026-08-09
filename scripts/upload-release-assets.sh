#!/usr/bin/env bash
#
# upload-release-assets.sh - upload a locally verified asset set to the
# STAGED DRAFT release for a tag, in bounded parallel batches.
# Fail-closed on the draft: this refuses to run unless exactly one draft
# release carries the tag, so it can never mutate a published release. The
# caller's draft verification and the publish flip stay the only path that
#   upload-release-assets.sh <vX.Y.Z> <asset-path> [asset-path...]

set -euo pipefail

if [ "$#" -lt 2 ]; then
	echo "usage: $0 vX.Y.Z asset-path [asset-path...]" >&2
	exit 2
fi

version="$1"
shift
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "upload-release-assets: version must look like vX.Y.Z (got $version)" >&2
	exit 2
fi
if ! command -v gh >/dev/null 2>&1; then
	echo "upload-release-assets: gh CLI is required" >&2
	exit 2
fi

jobs="${RELEASE_UPLOAD_JOBS:-4}"
if ! [[ "$jobs" =~ ^[1-9][0-9]*$ ]] || [ "$jobs" -gt 12 ]; then
	echo "upload-release-assets: RELEASE_UPLOAD_JOBS must be 1-12 (got $jobs)" >&2
	exit 2
fi

for asset in "$@"; do
	if [ ! -f "$asset" ] || [ -L "$asset" ]; then
		echo "upload-release-assets: asset is missing or unsafe: $asset" >&2
		exit 1
	fi
	case "${asset##*/}" in
	"" | */* | *[!A-Za-z0-9._-]*)
		echo "upload-release-assets: unsafe asset name: ${asset##*/}" >&2
		exit 1
		;;
	esac
done

# The release list is eventually consistent with the `gh release create` that
# staged this draft moments earlier: v2.7.1's first publication attempt read
# back zero drafts and aborted with the tag already pushed. Only an empty read
# waits — one draft proceeds and two still fail closed, both immediately.
draft_ids=""
draft_count=0
for delay in 0 1 2 3 4; do
	[ "$delay" -eq 0 ] || sleep "$delay"
	draft_ids="$(
		gh api --hostname github.com "repos/osauer/canary/releases?per_page=100" \
			--jq ".[] | select(.draft == true and .tag_name == \"$version\") | .id"
	)"
	draft_count="$(printf '%s' "$draft_ids" | grep -c . || true)"
	[ "$draft_count" -eq 0 ] || break
done
if [ "$draft_count" -ne 1 ]; then
	echo "upload-release-assets: expected exactly one staged draft for $version, found $draft_count" >&2
	exit 1
fi

echo "upload-release-assets: uploading $# assets to the $version draft ($jobs at a time)"

pids=()
failed=0

# A failed upload must not be masked by the ones that follow it, so every
# job is reaped and its status folded in; the batch fails if any did.
reap() {
	local pid="$1" status=0
	wait "$pid" || status=$?
	if [ "$status" -ne 0 ]; then
		failed=1
	fi
}

for asset in "$@"; do
	gh release upload "$version" "$asset" --repo github.com/osauer/canary &
	pids+=("$!")
	if [ "${#pids[@]}" -ge "$jobs" ]; then
		reap "${pids[0]}"
		pids=(${pids[@]+"${pids[@]:1}"})
	fi
done

for pid in ${pids[@]+"${pids[@]}"}; do
	reap "$pid"
done

if [ "$failed" -ne 0 ]; then
	echo "upload-release-assets: at least one asset upload failed for $version" >&2
	exit 1
fi

echo "upload-release-assets: all assets uploaded to the $version draft"
