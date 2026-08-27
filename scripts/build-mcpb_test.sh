#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-mcpb-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

fixture="$test_root/repo"
dist="$test_root/dist"
fake_bin="$test_root/fake-bin"
packed_stage="$test_root/packed-stage"
targets="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
mkdir -p "$fixture/scripts" "$fixture/web/app" "$dist" "$fake_bin"
cp "$repo_root/scripts/build-mcpb.sh" "$fixture/scripts/"
printf '%s\n' 'fixture icon' > "$fixture/web/app/icon-512.png"

for target in $targets; do
	archive_root="$test_root/archive/canary-v1.2.3-$target"
	mkdir -p "$archive_root"
	cat > "$archive_root/canary" <<'EOF'
#!/bin/sh
case "${1:-}" in
	version) printf '%s\n' 'Canary CLI  v1.2.3' ;;
	*) exit 2 ;;
esac
EOF
	chmod 0755 "$archive_root/canary"
	(
		cd "$test_root/archive"
		tar -czf "$dist/canary-v1.2.3-$target.tar.gz" "canary-v1.2.3-$target"
	)
done

cat > "$fake_bin/npx" <<'EOF'
#!/bin/bash
set -euo pipefail
[ "${1:-}" = "-y" ] || exit 2
shift 2
command="${1:?missing mcpb command}"
shift
case "$command" in
	validate|info) ;;
	pack)
		stage="$1"
		bundle="$2"
		rm -rf "$MCPB_FIXTURE_PACKED_STAGE"
		mkdir -p "$MCPB_FIXTURE_PACKED_STAGE"
		cp -R "$stage/." "$MCPB_FIXTURE_PACKED_STAGE/"
		printf '%s\n' 'fixture mcpb' > "$bundle"
		;;
	unpack)
		bundle="$1"
		out="$2"
		[ -s "$bundle" ] || exit 2
		mkdir -p "$out"
		cp -R "$MCPB_FIXTURE_PACKED_STAGE/." "$out/"
		;;
	*) exit 2 ;;
esac
EOF
chmod 0755 "$fake_bin/npx"

(
	cd "$fixture"
	PATH="$fake_bin:/usr/bin:/bin" \
	MCPB_FIXTURE_PACKED_STAGE="$packed_stage" \
		./scripts/build-mcpb.sh v1.2.3 "$dist" "$targets"
)

canonical="$dist/canary-v1.2.3.mcpb"
[ -s "$canonical" ] || {
	echo "build-mcpb test: canonical versioned bundle was not produced" >&2
	exit 1
}
cmp -s "$canonical" "$dist/canary.mcpb" || {
	echo "build-mcpb test: stable bundle differs from versioned canonical asset" >&2
	exit 1
}
if compgen -G "$dist/ibkr*.mcpb" >/dev/null; then
	echo "build-mcpb test: legacy ibkr MCPB asset was produced" >&2
	exit 1
fi
[ "$(jq -r '.name' "$packed_stage/manifest.json")" = "canary" ] || {
	echo "build-mcpb test: manifest machine name is not canary" >&2
	exit 1
}
[ "$(jq -r '.display_name' "$packed_stage/manifest.json")" = "Canary" ] || {
	echo "build-mcpb test: manifest display name is not Canary" >&2
	exit 1
}
description="$(jq -r '.description' "$packed_stage/manifest.json")"
long_description="$(jq -r '.long_description' "$packed_stage/manifest.json")"
[[ "$description" == *"read-only"* ]] || {
	echo "build-mcpb test: manifest description does not state the read-only boundary" >&2
	exit 1
}
[[ "$long_description" == *"broker-reporting status"* && "$long_description" == *"retrospective Edge review"* ]] || {
	echo "build-mcpb test: manifest long description omits reporting or Edge" >&2
	exit 1
}
[[ "$long_description" == *"no resources, order previews, settings writes, or broker-write tools"* ]] || {
	echo "build-mcpb test: manifest long description does not state the adapter boundaries" >&2
	exit 1
}
if [[ "$description" =~ [0-9]+[[:space:]]+([^[:space:]]+[[:space:]]+)?tools ]] ||
	[[ "$long_description" =~ [0-9]+[[:space:]]+([^[:space:]]+[[:space:]]+)?tools ]]; then
	echo "build-mcpb test: manifest carries a hand-maintained tool count" >&2
	exit 1
fi
if [[ "$description" == *"preview-only drafts"* || "$long_description" == *"preview-only drafts"* ]]; then
	echo "build-mcpb test: manifest carries retired preview wording" >&2
	exit 1
fi
[ "$(jq -r '.server.entry_point' "$packed_stage/manifest.json")" = "server/canary" ] || {
	echo "build-mcpb test: manifest does not use the canonical server/canary entry point" >&2
	exit 1
}
[ "$(jq -r '.server.mcp_config.command' "$packed_stage/manifest.json")" = '${__dirname}/server/canary' ] || {
	echo "build-mcpb test: manifest MCP command does not use server/canary" >&2
	exit 1
}
[ -x "$packed_stage/server/canary" ] || {
	echo "build-mcpb test: canonical entry point is missing" >&2
	exit 1
}
[ ! -e "$packed_stage/server/ibkr" ] || {
	echo "build-mcpb test: legacy server/ibkr entry point was retained" >&2
	exit 1
}
for target in $targets; do
	[ -x "$packed_stage/server/bin/canary-$target" ] || {
		echo "build-mcpb test: canonical bundled binary entry is missing for $target" >&2
		exit 1
	}
done
grep -Fq 'canary-${version}-${target}/canary' "$fixture/scripts/build-mcpb.sh" || {
	echo "build-mcpb test: bundle assembly did not consume the canonical archive entry" >&2
	exit 1
}

echo "build-mcpb test: OK"
