#!/usr/bin/env bash

# Prove that the named annotated release/plugin tag exists both locally and on
# origin, has the exact tag object, and peels to the release tag's immutable
# commit. The current recovery controller may intentionally be newer than it.

set -euo pipefail

mode="release"
if [ "$#" -eq 2 ] && {
	[ "$1" = "--plugin" ] || [ "$1" = "--plugin-local" ] || [ "$1" = "--plugin-ref" ]
}; then
	mode="${1#--}"
	shift
fi
if [ "$#" -ne 1 ]; then
	echo "usage: $0 [--plugin|--plugin-local|--plugin-ref] vX.Y.Z" >&2
	exit 2
fi

version="$1"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "check-release-tag: version must look like vX.Y.Z (got $version)" >&2
	exit 1
fi

release_commit="$(git rev-parse --verify "refs/tags/${version}^{commit}")" || {
	echo "check-release-tag: cannot resolve local release tag $version" >&2
	exit 1
}

tag_name="$version"
if [ "$mode" = "plugin" ] || [ "$mode" = "plugin-local" ] || [ "$mode" = "plugin-ref" ]; then
	if ! command -v python3 >/dev/null 2>&1; then
		echo "check-release-tag: python3 is required to resolve the plugin name" >&2
		exit 1
	fi
	plugin_manifest="$(git show "refs/tags/${version}:.claude-plugin/plugin.json")" || {
		echo "check-release-tag: cannot read plugin manifest from $version" >&2
		exit 1
	}
	plugin_name="$(
		printf '%s' "$plugin_manifest" | python3 -c '
import json
import re
import sys

try:
    document = json.load(sys.stdin)
except (UnicodeDecodeError, json.JSONDecodeError) as error:
    print(f"cannot read plugin manifest: {error}", file=sys.stderr)
    raise SystemExit(1)
name = document.get("name") if type(document) is dict else None
if type(name) is not str or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*", name) is None:
    print("plugin manifest has an unsafe or missing top-level name", file=sys.stderr)
    raise SystemExit(1)
print(name)
'
	)" || {
		echo "check-release-tag: cannot derive the canonical plugin tag" >&2
		exit 1
	}
	tag_name="${plugin_name}--${version}"
fi
if [ "$mode" = "plugin-ref" ]; then
	printf 'refs/tags/%s\n' "$tag_name"
	exit 0
fi

tag_ref="refs/tags/$tag_name"
tag_type="$(git cat-file -t "$tag_ref" 2>/dev/null || true)"
if [ "$tag_type" != "tag" ]; then
	echo "check-release-tag: $tag_ref must be an annotated local tag" >&2
	exit 1
fi
local_commit="$(git rev-parse --verify "${tag_ref}^{commit}")" || {
	echo "check-release-tag: cannot peel local $tag_ref" >&2
	exit 1
}
local_object="$(git rev-parse --verify "$tag_ref")" || {
	echo "check-release-tag: cannot resolve local $tag_ref object" >&2
	exit 1
}
if [ "$local_commit" != "$release_commit" ]; then
	echo "check-release-tag: local $tag_ref peels to $local_commit, expected release commit $release_commit" >&2
	exit 1
fi
if [ "$mode" = "plugin-local" ]; then
	echo "check-release-tag: OK mode=$mode tag=$tag_name sha=$release_commit"
	exit 0
fi

remote_lines="$(git ls-remote --exit-code origin "$tag_ref" "${tag_ref}^{}")" || {
	echo "check-release-tag: cannot resolve $tag_ref on origin" >&2
	exit 1
}
remote_evidence="$(
	printf '%s\n' "$remote_lines" |
		awk -v plain="$tag_ref" -v peeled="${tag_ref}^{}" '
			NF != 2 {
				bad = 1
				next
			}
			$2 == plain {
				plain_count++
				object = $1
				next
			}
			$2 == peeled {
				peeled_count++
				commit = $1
				next
			}
			{
				bad = 1
			}
			END {
				if (bad || plain_count != 1 || peeled_count != 1) {
					exit 1
				}
				print object ":" commit
			}
		'
)" || {
	echo "check-release-tag: origin returned malformed or non-annotated tag evidence for $tag_ref" >&2
	exit 1
}
remote_object="${remote_evidence%%:*}"
remote_commit="${remote_evidence#*:}"
if [ "$remote_object" != "$local_object" ]; then
	echo "check-release-tag: origin $tag_ref object is $remote_object, expected local annotated tag object $local_object" >&2
	exit 1
fi
if [ "$remote_commit" != "$release_commit" ]; then
	echo "check-release-tag: origin $tag_ref peels to $remote_commit, expected release commit $release_commit" >&2
	exit 1
fi

echo "check-release-tag: OK mode=$mode tag=$tag_name sha=$release_commit"
