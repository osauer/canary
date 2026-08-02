#!/usr/bin/env bash

# A fix that closes a public issue has to tell users which release carries it.
# GitHub closes the issue when `Fixes #N` lands on main, which leaves the issue
# saying "closed" and nothing saying "shipped in vX.Y.Z" — the question the
# person who found the issue actually has.
#
# Every issue-closing reference in the release range must therefore be named in
# the release's changelog entry. Deliberately local: it reads git and the
# changelog, never GitHub, so it works offline, needs no auth, and cannot fail
# a release for a network reason. The reverse direction — a fix that closed an
# issue without ever referencing it — is judgement, and belongs to the release
# skill's reconciliation sweep rather than to a gate.

set -euo pipefail

version="${RELEASE_VERSION:?RELEASE_VERSION is required}"
changelog="${CHANGELOG_PATH:-CHANGELOG.md}"
range_end="${ISSUE_REF_RANGE_END:-HEAD}"

if [ ! -f "$changelog" ]; then
	echo "changelog-issue-refs: $changelog not found" >&2
	exit 1
fi

# The candidate is not tagged yet, so the newest reachable release tag is the
# previous release. A first release has none and has no range to check. The
# match filter matters: the repository also carries a separately named plugin
# tag (`canary--vX.Y.Z`), and an unfiltered describe picks whichever is newer,
# which silently shifts the range if the two ever stop agreeing.
previous_tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' "$range_end" 2>/dev/null || true)"
if [ -z "$previous_tag" ]; then
	echo "changelog-issue-refs: no previous tag reachable; nothing to reconcile"
	exit 0
fi

# Only the keywords GitHub actually closes on. A bare "#12" in prose is a
# cross-reference, not a claim that this release fixes it.
referenced="$(git log --format=%B "$previous_tag..$range_end" |
	grep -oiE '(close[sd]?|fix(e[sd])?|resolve[sd]?)[[:space:]]+#[0-9]+' |
	grep -oE '#[0-9]+' | sort -u -V || true)"

if [ -z "$referenced" ]; then
	echo "changelog-issue-refs: no issue-closing references in $previous_tag..$range_end"
	exit 0
fi

# Only the topmost entry, which is this release's. An issue named under an
# older heading was shipped by an older release and says nothing about this one.
entry="$(awk '/^## / { if (seen) exit; seen = 1 } seen' "$changelog")"
if [ -z "$entry" ]; then
	echo "changelog-issue-refs: $changelog has no release entry to check" >&2
	exit 1
fi

missing=""
for ref in $referenced; do
	if ! printf '%s' "$entry" | grep -qE "(^|[^0-9a-zA-Z_])${ref}([^0-9]|$)"; then
		missing="$missing $ref"
	fi
done

if [ -n "$missing" ]; then
	echo "changelog-issue-refs: $version closes$missing but the changelog entry does not name them" >&2
	echo "                      commits in $previous_tag..$range_end reference these issues, so users" >&2
	echo "                      reading the entry cannot tell which release fixed what they reported." >&2
	echo "                      Name each one in the $version entry, e.g. \"(#12)\"." >&2
	exit 1
fi

echo "changelog-issue-refs: OK ($(printf '%s' "$referenced" | tr '\n' ' ')named in the $version entry)"
