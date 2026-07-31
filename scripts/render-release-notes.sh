#!/usr/bin/env bash

# Render the canonical GitHub release body from an explicit CHANGELOG and
# release-notes template. Callers choose the authority for those two inputs;
# release publication uses the release source, while verification materializes
# both blobs from the immutable release tag.

set -euo pipefail

if [ "$#" -ne 4 ]; then
	echo "usage: $0 vX.Y.Z CHANGELOG.md release-notes-template.md output" >&2
	exit 2
fi

version="$1"
changelog="$2"
template="$3"
output="$4"

if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "render-release-notes: version must look like vX.Y.Z (got $version)" >&2
	exit 1
fi
for input in "$changelog" "$template"; do
	if [ ! -f "$input" ] || [ -L "$input" ]; then
		echo "render-release-notes: input is missing or unsafe: $input" >&2
		exit 1
	fi
done

output_dir="$(dirname -- "$output")"
if [ ! -d "$output_dir" ] || [ -L "$output_dir" ]; then
	echo "render-release-notes: output directory is missing or unsafe: $output_dir" >&2
	exit 1
fi
if { [ -e "$output" ] || [ -L "$output" ]; } \
	&& { [ ! -f "$output" ] || [ -L "$output" ]; }; then
	echo "render-release-notes: output is unsafe: $output" >&2
	exit 1
fi

highlights="$(mktemp "$output_dir/canary-release-highlights.XXXXXX")"
rendered="$(mktemp "$output_dir/canary-release-notes.XXXXXX")"
cleanup() {
	rm -f "$highlights" "$rendered"
}
trap cleanup EXIT HUP INT TERM

awk -v ver="$version" '
	/^## v[0-9]/ {
		if (in_ver) exit
		in_ver = ($0 ~ "^## " ver " ")
		next
	}
	in_ver && /^### What.s new$/ { in_new=1; next }
	in_ver && in_new && /^###/ { exit }
	in_new
' "$changelog" >"$highlights"

awk -v ver="$version" -v hf="$highlights" '
	{ gsub(/__VERSION__/, ver) }
	/__HIGHLIGHTS__/ {
		while ((getline line < hf) > 0) print line
		close(hf)
		next
	}
	{ print }
' "$template" >"$rendered"

awk -v ver="$version" '
	/^## v[0-9]/ {
		in_section = ($0 ~ "^## " ver " ")
		skip=0
		if (in_section) next
	}
	in_section && /^### What.s new$/ { skip=1; next }
	in_section && skip && /^### / { skip=0 }
	in_section && !skip
' "$changelog" >>"$rendered"

mv -f "$rendered" "$output"
rendered=""
