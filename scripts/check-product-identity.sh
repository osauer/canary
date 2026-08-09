#!/usr/bin/env bash

# Keep current product authorities on the Canary identity after the breaking
# storage/IPC identifiers, broker-source identifiers, and migration assertions

set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
allowlist="$root/scripts/product-identity-allowlist.tsv"
tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/canary-product-identity.XXXXXX")"
hits="$tmpdir/hits.tsv"
cleanup() {
	rm -rf "$tmpdir"
}
trap cleanup EXIT HUP INT TERM

: > "$hits"

if [ ! -f "$allowlist" ]; then
	echo "check-product-identity: missing scripts/product-identity-allowlist.tsv" >&2
	exit 1
fi

record() {
	local detector="$1" path="$2" line_number="$3" text="$4"
	printf '%s\t%s\t%s\t%s\n' "$detector" "$path" "$line_number" "$text" >> "$hits"
}

is_pinned_identifier() {
	case "$1" in
		ibkr_app_device|ibkr_app_session|ibkr_client_|ibkr_hmds_adjusted_ohlcv_v1|\
		ibkr_remote_route|ibkr_short_stock_availability|ibkr_tws_historical|ibkr_wsh)
			return 0
			;;
	esac
	return 1
}

old_path_re='(^|[^A-Za-z0-9_.-])(cmd/ibkr|bin/ibkr|skills/ibkr|settings/ibkr|scripts/ibkr-mcp(\.sh)?|ibkr\.mcpb|ibkr-trading|ibkr@ibkr)([^A-Za-z0-9_.-]|$)'
old_cli_re='(^|[[:space:]`"'\''(])ibkr[[:space:]]+(account|positions|quote|calendar|watch|chain|history|scan|size|technical|breadth|gamma|regime|stress|brief|rules|market-events|proposals|opportunities|backtest|settings|policy|recon|trading|orders|order|status|version|update|app|daemon|mcp|setup|restart|purge|canary|--[a-z])([^A-Za-z0-9-]|$)'
old_argv_re='(exec\.Command(Context)?\([^)]*|pattern[[:space:]]*=[[:space:]]*\[\[)["'\'']ibkr["'\''][[:space:]]*,'
old_name_re='(^|[,{[:space:]])("name"[[:space:]]*:|name[[:space:]]*=|name[[:space:]]*:)[[:space:]]*["'\'']?ibkr["'\'']?([,}[:space:]]|$)'

scan_line() {
	local path="$1" line_number="$2" text="$3" token remainder

	[[ "$text" == *"github.com/osauer/ibkr"* ]] &&
		record old-module-or-repository "$path" "$line_number" "$text"
	[[ "$text" == *"osauer.dev/ibkr"* ]] &&
		record old-site "$path" "$line_number" "$text"
	[[ "$text" == *"io.github.osauer/ibkr"* ]] &&
		record old-mcp-server "$path" "$line_number" "$text"
	[[ "$text" == *"ibkr://"* ]] &&
		record old-mcp-resource "$path" "$line_number" "$text"

	if [[ "$text" =~ $old_path_re ]]; then
		record old-product-path "$path" "$line_number" "$text"
	fi
	if [[ "$text" =~ $old_cli_re ]]; then
		record old-cli-command "$path" "$line_number" "$text"
	fi
	if [[ "$text" =~ $old_argv_re ]]; then
		record old-cli-argv "$path" "$line_number" "$text"
	fi
	if [[ "$text" =~ $old_name_re ]]; then
		record old-product-name "$path" "$line_number" "$text"
	fi

	remainder="$text"
	while [[ "$remainder" =~ (ibkr_[a-z][a-z0-9_]*) ]]; do
		token="${BASH_REMATCH[1]}"
		if ! is_pinned_identifier "$token"; then
			record old-mcp-tool "$path" "$line_number" "$text"
			break
		fi
		remainder="${remainder#*"$token"}"
	done
}

# `ls-files --cached` below lists an unmerged path once per stage, so a
# which was never wrong. Refuse the tree instead of reporting on it: mid-merge
unmerged="$(git -C "$root" ls-files --unmerged | cut -f2 | sort -u)"
if [ -n "$unmerged" ]; then
	echo "check-product-identity: refusing to scan an unmerged index; resolve and stage these paths, then re-run:" >&2
	printf '%s\n' "$unmerged" | sed 's/^/  /' >&2
	exit 1
fi

while IFS= read -r -d '' path; do
	[ -f "$root/$path" ] || continue
	case "$path" in
		CHANGELOG.md|scripts/check-product-identity.sh|scripts/check-product-identity_test.sh|scripts/product-identity-allowlist.tsv)
			continue
			;;
	esac
	while IFS=: read -r line_number text; do
		scan_line "$path" "$line_number" "$text"
	done < <(grep -IEn 'ibkr' "$root/$path" || true)
done < <(git -C "$root" ls-files --cached --others --exclude-standard -z)

failure=0
while IFS=$'\t' read -r detector path line_number text; do
	[ -n "$detector" ] || continue
	entry="$(awk -F '\t' -v detector="$detector" -v path="$path" '
		$1 == detector && $2 == path { print $3 "\t" $4; exit }
	' "$allowlist")"
	if [ -n "$entry" ]; then
		max="${entry%%$'\t'*}"
		count="$(awk -F '\t' -v detector="$detector" -v path="$path" '
			$1 == detector && $2 == path { count++ } END { print count + 0 }
		' "$hits")"
		if [[ "$max" =~ ^[0-9]+$ ]] && [ "$count" -le "$max" ]; then
			continue
		fi
	fi
	printf 'check-product-identity: retired %s in %s:%s: %s\n' \
		"$detector" "$path" "$line_number" "$text" >&2
	failure=1
done < "$hits"

if [ "$failure" -ne 0 ]; then
	echo "check-product-identity: current authorities must use Canary identities; add only reviewed, count-bounded safety or migration exceptions" >&2
	exit 1
fi

echo "check-product-identity: OK"
