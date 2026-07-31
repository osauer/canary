#!/usr/bin/env bash

# Resolve whether an exact Canary GitHub release exists without promoting
# network/auth/API failures into "absent". Only HTTP 200 and 404 are typed
# states; every other response is unknown and fails closed.

set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: $0 vX.Y.Z" >&2
	exit 2
fi

version="$1"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "github-release-state: version must look like vX.Y.Z (got $version)" >&2
	exit 2
fi
if ! command -v gh >/dev/null 2>&1; then
	echo "github-release-state: gh CLI is required" >&2
	exit 2
fi

response="$(mktemp "${TMPDIR:-/tmp}/canary-github-release-state.XXXXXX")"
cleanup() {
	rm -f -- "$response"
}
trap cleanup EXIT HUP INT TERM

status=0
gh api --hostname github.com --include -X GET \
	"repos/osauer/canary/releases/tags/$version" >"$response" 2>/dev/null || status=$?

status_lines="$(LC_ALL=C sed -n 's/\r$//; /^HTTP\//p' "$response")"
status_line="$(LC_ALL=C sed -n '1{s/\r$//;p;}' "$response")"
if [ "$(printf '%s\n' "$status_lines" | sed '/^$/d' | wc -l | tr -d '[:space:]')" -ne 1 ] \
	|| [ "$status_line" != "$status_lines" ]; then
	echo "github-release-state: GitHub response did not contain one strict leading HTTP status" >&2
	exit 1
fi

case "$status_line:$status" in
	HTTP/1.1\ 200\ *:0|HTTP/2.0\ 200\ *:0)
		printf 'existing\n'
		;;
	HTTP/1.1\ 404\ *:[1-9]*|HTTP/2.0\ 404\ *:[1-9]*)
		printf 'absent\n'
		;;
	*)
		echo "github-release-state: GitHub release state is unavailable or ambiguous" >&2
		exit 1
		;;
esac
