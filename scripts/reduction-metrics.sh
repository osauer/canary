#!/bin/sh

set -eu

baseline_ref=c3ec1d81c8537c8b982791e6fdc78f3da4e2c28e
baseline_files=1148
baseline_loc=370751
floor_files=574
floor_loc=185375
stretch_files=459
stretch_loc=148300

usage() {
	cat <<'EOF'
usage: scripts/reduction-metrics.sh [--json] [--verify-baseline] [REF]

Measure a committed Canary tree against the approved v3 reduction baseline.
REF defaults to HEAD. Tests, scripts, configuration, comments, and textual test
fixtures count as maintained source. Blank lines, prose, binaries, generated
outputs, dependency locks, and the public-web HTML surface do not count as LOC.
Every Git tree entry still counts toward the independent file target.
EOF
}

json=false
verify_baseline=false
ref=HEAD
while [ "$#" -gt 0 ]; do
	case "$1" in
	--json)
		json=true
		;;
	--verify-baseline)
		verify_baseline=true
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*)
		echo "reduction-metrics: unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	*)
		if [ "$ref" != HEAD ]; then
			echo "reduction-metrics: only one REF may be supplied" >&2
			exit 2
		fi
		ref=$1
		;;
	esac
	shift
done

repo=$(git rev-parse --show-toplevel)
commit=$(git -C "$repo" rev-parse --verify "$ref^{commit}")
scratch=$(mktemp -d "${TMPDIR:-/tmp}/canary-reduction.XXXXXX")
trap 'rm -rf "$scratch"' EXIT HUP INT TERM

git -C "$repo" archive --format=tar "$commit" | tar -xf - -C "$scratch"
git -C "$repo" ls-tree -r --name-only "$commit" >"$scratch/.tracked-files"

files=$(wc -l <"$scratch/.tracked-files" | tr -d '[:space:]')
loc=0
source_files=0

while IFS= read -r path; do
	file=$scratch/$path
	[ -f "$file" ] || continue
	[ -L "$file" ] && continue

	include=false
	case "$path" in
	docs/*.html | internal/breadth/spx/members_data.go | */package-lock.json)
		continue
		;;
	esac

	case "$path" in
	*.go | *.js | *.mjs | *.ts | *.tsx | *.sh | *.bash | *.zsh | *.py | *.css | *.html | *.toml | *.yaml | *.yml | *.json | *.sql | *.mod | *.rules | *.webmanifest | Makefile)
		include=true
		;;
	*/testdata/*.jsonl | */testdata/*.xml | */testdata/*.tsv | */testdata/*.txt | */testdata/*.head | */testdata/*.hex | */testdata/*.fields | */testdata/*.asc | */testdata/sample-sha256sums | scripts/upgrade-fixtures/generators/*.txt | scripts/product-identity-allowlist.tsv | examples/config.toml.trading)
		include=true
		;;
	esac

	[ "$include" = true ] || continue
	lines=$(awk 'NF { count++ } END { print count + 0 }' "$file")
	loc=$((loc + lines))
	source_files=$((source_files + 1))
done <"$scratch/.tracked-files"

file_reduction=$(awk -v base="$baseline_files" -v current="$files" 'BEGIN { printf "%.2f", (base-current)*100/base }')
loc_reduction=$(awk -v base="$baseline_loc" -v current="$loc" 'BEGIN { printf "%.2f", (base-current)*100/base }')

floor_status=miss
[ "$files" -le "$floor_files" ] && [ "$loc" -le "$floor_loc" ] && floor_status=pass
stretch_status=miss
[ "$files" -le "$stretch_files" ] && [ "$loc" -le "$stretch_loc" ] && stretch_status=pass

if [ "$verify_baseline" = true ]; then
	if [ "$commit" != "$baseline_ref" ] || [ "$files" -ne "$baseline_files" ] || [ "$loc" -ne "$baseline_loc" ]; then
		echo "reduction-metrics: baseline mismatch: commit=$commit files=$files loc=$loc" >&2
		exit 1
	fi
fi

if [ "$json" = true ]; then
	printf '{"commit":"%s","files":%d,"maintained_source_files":%d,"maintained_nonblank_loc":%d,"file_reduction_pct":%s,"loc_reduction_pct":%s,"floor_50":"%s","stretch_60":"%s"}\n' \
		"$commit" "$files" "$source_files" "$loc" "$file_reduction" "$loc_reduction" "$floor_status" "$stretch_status"
	exit 0
fi

printf 'commit                    %s\n' "$commit"
printf 'tracked files             %7d  reduction %6s%%  floor <= %d  stretch <= %d\n' "$files" "$file_reduction" "$floor_files" "$stretch_files"
printf 'maintained nonblank LOC   %7d  reduction %6s%%  floor <= %d  stretch <= %d\n' "$loc" "$loc_reduction" "$floor_loc" "$stretch_loc"
printf 'maintained source files   %7d\n' "$source_files"
printf '50%% floor                  %s\n' "$floor_status"
printf '60%% stretch                %s\n' "$stretch_status"
