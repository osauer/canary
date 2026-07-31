#!/usr/bin/env bash

# Bind Git transport to the same canonical repository used by the exact-SHA
# Actions contract. A forked fetch URL or an alternate/multiple push URL must
# never receive a release whose CI evidence came from osauer/canary.

set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$root"

fetch_urls=()
fetch_count=0
while IFS= read -r url; do
	fetch_urls[$fetch_count]="$url"
	fetch_count=$((fetch_count + 1))
done < <(git remote get-url --all origin)

push_urls=()
push_count=0
while IFS= read -r url; do
	push_urls[$push_count]="$url"
	push_count=$((push_count + 1))
done < <(git remote get-url --all --push origin)

if [ "$fetch_count" -ne 1 ]; then
	echo "check-release-origin: origin must have exactly one fetch URL" >&2
	exit 1
fi
if [ "$push_count" -eq 0 ]; then
	push_urls=("${fetch_urls[0]}")
elif [ "$push_count" -ne 1 ]; then
	echo "check-release-origin: origin must have at most one explicit push URL" >&2
	exit 1
fi

is_canonical() {
	local url="${1%/}"
	url="${url%.git}"
	case "$url" in
		https://github.com/osauer/canary | \
		git@github.com:osauer/canary | \
		ssh://git@github.com/osauer/canary)
			return 0
			;;
	esac
	return 1
}

if ! is_canonical "${fetch_urls[0]}"; then
	echo "check-release-origin: origin fetch URL is not canonical osauer/canary" >&2
	exit 1
fi
if ! is_canonical "${push_urls[0]}"; then
	echo "check-release-origin: origin push URL is not canonical osauer/canary" >&2
	exit 1
fi

echo "check-release-origin: OK"
