#!/usr/bin/env bash

# Delete leftover DRAFT releases carrying this exact tag name so a fresh
# draft-then-publish attempt is idempotent. Drafts are pipeline staging that
# was never public; published releases are never touched — a published
# release for the tag makes this script a no-op and the caller's later
# create fails loudly instead.

set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: $0 vX.Y.Z" >&2
	exit 2
fi

version="$1"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "prune-github-release-drafts: version must look like vX.Y.Z (got $version)" >&2
	exit 2
fi
if ! command -v gh >/dev/null 2>&1; then
	echo "prune-github-release-drafts: gh CLI is required" >&2
	exit 2
fi

draft_ids="$(
	gh api --hostname github.com "repos/osauer/canary/releases?per_page=100" \
		--jq ".[] | select(.draft == true and .tag_name == \"$version\") | .id"
)"

if [ -z "$draft_ids" ]; then
	exit 0
fi

for draft_id in $draft_ids; do
	if ! [[ "$draft_id" =~ ^[0-9]+$ ]]; then
		echo "prune-github-release-drafts: unexpected draft id: $draft_id" >&2
		exit 1
	fi
	echo "prune-github-release-drafts: deleting stale draft $draft_id for $version"
	gh api --hostname github.com -X DELETE "repos/osauer/canary/releases/$draft_id" >/dev/null
done
