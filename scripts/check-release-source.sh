#!/usr/bin/env bash

# Require publication helpers to run from either the clean exact release tag
# or a clean, current origin/main recovery controller. The latter keeps current
# safety code in authority while treating an older tag only as immutable input.
# The mode is always explicit: a caller that cannot name its anchor does not
# know which commit it is trusting.

set -euo pipefail

if [ "$#" -ne 3 ] || [ "$1" != "--mode" ]; then
	echo "usage: $0 --mode tag|controller vX.Y.Z" >&2
	exit 2
fi
mode="$2"
shift 2
case "$mode" in
	tag | controller) ;;
	*)
		echo "check-release-source: mode must be tag or controller (got $mode)" >&2
		exit 2
		;;
esac

version="$1"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "check-release-source: version must look like vX.Y.Z (got $version)" >&2
	exit 1
fi

head_commit="$(git rev-parse --verify 'HEAD^{commit}')" || {
	echo "check-release-source: cannot resolve HEAD" >&2
	exit 1
}
tag_commit="$(git rev-parse --verify "refs/tags/${version}^{commit}")" || {
	echo "check-release-source: cannot resolve local release tag $version" >&2
	exit 1
}
dirty="$(git status --porcelain --untracked-files=normal)" || {
	echo "check-release-source: cannot inspect repository state" >&2
	exit 1
}
if [ -n "$dirty" ]; then
	echo "check-release-source: release controller/source is dirty" >&2
	printf '%s\n' "$dirty" >&2
	exit 1
fi

if [ "$mode" = "tag" ]; then
	if [ "$head_commit" != "$tag_commit" ]; then
		echo "check-release-source: HEAD $head_commit is not release tag $version at $tag_commit" >&2
		exit 1
	fi
	echo "check-release-source: OK mode=tag tag=$version sha=$head_commit"
	exit 0
fi

remote_lines="$(git ls-remote --exit-code --refs origin refs/heads/main)" || {
	echo "check-release-source: cannot resolve canonical controller refs/heads/main" >&2
	exit 1
}
remote_main="$(
	printf '%s\n' "$remote_lines" |
		awk '
			NF == 2 && $2 == "refs/heads/main" {
				count++
				sha = $1
				next
			}
			{
				bad = 1
			}
			END {
				if (bad || count != 1) {
					exit 1
				}
				print sha
			}
		'
)" || {
	echo "check-release-source: origin returned malformed controller evidence" >&2
	exit 1
}
if [ "$head_commit" != "$remote_main" ]; then
	echo "check-release-source: recovery controller HEAD is not exact origin/main" >&2
	exit 1
fi

echo "check-release-source: OK mode=controller controller=$head_commit tag=$version tag_sha=$tag_commit"
