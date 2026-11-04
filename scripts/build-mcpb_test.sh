#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
mode="${1:-fixture}"
case "$mode" in fixture|--real) ;; *) echo "usage: build-mcpb_test.sh [--real]" >&2; exit 2 ;; esac
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-mcpb-test.XXXXXX")"
test_root="$(CDPATH= cd -- "$test_root" && pwd)"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

fixture="$test_root/repo"
dist="$test_root/dist"
fake_bin="$test_root/fake-bin"
packed_stage="$test_root/packed-stage"
targets="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
mkdir -p "$fixture/scripts" "$fixture/web/app" "$fixture/tools/mcpb" "$dist" "$fake_bin"
cp "$repo_root/scripts/build-mcpb.sh" "$fixture/scripts/"
cp "$repo_root/LICENSE" "$fixture/LICENSE"
cp "$repo_root/web/app/icon-512.png" "$fixture/web/app/icon-512.png"
cp "$repo_root/tools/mcpb/"package*.json "$fixture/tools/mcpb/"

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

cat > "$test_root/fixture-mcpb" <<'EOF'
#!/bin/bash
set -euo pipefail
command="${1:?missing mcpb command}"
shift
printf '%s\n' "$command" >> "$MCPB_FIXTURE_CALLS"
case "$command" in
	validate|info|sign|verify) ;;
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
		if [ "${MCPB_FIXTURE_FAILURE:-}" = license ]; then
			printf '%s\n' 'wrong license' > "$out/LICENSE"
		fi
		if [ "${MCPB_FIXTURE_FAILURE:-}" = version ]; then
			printf '#!/bin/sh\nprintf "wrong version\\n"\n' > "$out/server/canary"
		fi
		;;
	*) exit 2 ;;
esac
EOF
chmod 0755 "$test_root/fixture-mcpb"

cat > "$fake_bin/npm" <<'EOF'
#!/bin/bash
set -euo pipefail
[ "$#" = 8 ] && [ "$1" = --prefix ] && [ "$2" = "$MCPB_FIXTURE_TOOL_DIR" ]
[ "$3 $4 $5 $6 $7 $8" = 'ci --include=dev --ignore-scripts --no-audit --no-fund --dry-run=false' ]
[ -f "$2/package.json" ] && [ -f "$2/package-lock.json" ]
[ "${MCPB_FIXTURE_FAILURE:-}" != install ] || exit 23
rm -rf "$2/node_modules"
mkdir -p "$2/node_modules/.bin"
if [ "${MCPB_FIXTURE_FAILURE:-}" != missing ]; then
	cp "$MCPB_FIXTURE_EXECUTABLE" "$2/node_modules/.bin/mcpb"
fi
EOF
chmod 0755 "$fake_bin/npm"
for fallback in npx mcpb; do
	cat > "$fake_bin/$fallback" <<'EOF'
#!/bin/sh
touch "$MCPB_FIXTURE_FALLBACK"
exit 97
EOF
	chmod 0755 "$fake_bin/$fallback"
done

run_fixture() (
	# A controller cwd and ambient npm prefix must not select the release tool.
	cd "$test_root"
	PATH="$fake_bin:/usr/bin:/bin" \
	npm_config_prefix="$test_root/controller" \
	npm_config_dry_run=true \
	MCPB_PACKAGE='must-never-be-resolved' \
	MCPB_FIXTURE_TOOL_DIR="$fixture/tools/mcpb" \
	MCPB_FIXTURE_EXECUTABLE="$test_root/fixture-mcpb" \
	MCPB_FIXTURE_CALLS="$test_root/calls" \
	MCPB_FIXTURE_FALLBACK="$test_root/fallback" \
	MCPB_FIXTURE_PACKED_STAGE="$packed_stage" \
		"$fixture/scripts/build-mcpb.sh" v1.2.3 "$dist" "$targets"
)

if [ "$mode" = --real ]; then
(
	cd "$test_root"
	# Only synthetic native binaries and a disposable installation are used.
	export npm_config_dry_run=true
	mkdir -p "$fixture/tools/mcpb/node_modules/.bin"
	printf '#!/bin/sh\nexit 97\n' > "$fixture/tools/mcpb/node_modules/.bin/mcpb"
	chmod 0755 "$fixture/tools/mcpb/node_modules/.bin/mcpb"
	MCPB_SIGN_CERT= MCPB_SIGN_KEY= "$fixture/scripts/build-mcpb.sh" v1.2.3 "$dist" "$targets"
	# Preserve the verify gate: an untrusted fixture certificate cannot produce
	# a distributable stable bundle. Do not install trust or use release keys.
	openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=CanarySyntheticFixture \
		-keyout "$test_root/key.pem" -out "$test_root/cert.pem" >/dev/null 2>&1
	mkdir -p "$test_root/untrusted"
	cp "$dist/"*.tar.gz "$test_root/untrusted/"
	if MCPB_SIGN_CERT="$test_root/cert.pem" MCPB_SIGN_KEY="$test_root/key.pem" MCPB_SIGN_INTERMEDIATE= \
		"$fixture/scripts/build-mcpb.sh" v1.2.3 "$test_root/untrusted" "$targets" >"$test_root/sign-output" 2>&1; then
		echo "build-mcpb test: untrusted signing unexpectedly passed" >&2
		exit 1
	fi
	grep -q 'Successfully signed' "$test_root/sign-output"
	grep -q 'ERROR: Extension is not signed' "$test_root/sign-output"
	[ ! -e "$test_root/untrusted/canary.mcpb" ]
	echo "build-mcpb test: real sign ran; verification rejected the untrusted fixture and stopped assembly"
	"$fixture/tools/mcpb/node_modules/.bin/mcpb" unpack "$dist/canary-v1.2.3.mcpb" "$packed_stage"
	[ "$("$fixture/tools/mcpb/node_modules/.bin/mcpb" --version)" = 2.1.2 ]
)
else
	# Stale installed tools and globally available commands cannot rescue ci.
	for failure in install missing; do
		mkdir -p "$fixture/tools/mcpb/node_modules/.bin"
		cp "$fake_bin/mcpb" "$fixture/tools/mcpb/node_modules/.bin/mcpb"
		if MCPB_FIXTURE_FAILURE="$failure" run_fixture >"$test_root/error" 2>&1; then
			echo "build-mcpb test: $failure unexpectedly passed" >&2
			exit 1
		fi
		[ ! -e "$test_root/fallback" ] && [ ! -e "$test_root/calls" ]
	done
	for failure in license version; do
		if MCPB_FIXTURE_FAILURE="$failure" run_fixture >"$test_root/error" 2>&1; then
			echo "build-mcpb test: $failure corruption unexpectedly passed" >&2
			exit 1
		fi
		grep -q "unpacked.*$failure" "$test_root/error" || { cat "$test_root/error" >&2; exit 1; }
	done
	: > "$test_root/calls"
	MCPB_SIGN_CERT=synthetic-cert MCPB_SIGN_KEY=synthetic-key run_fixture
	printf '%s\n' validate pack sign verify info unpack > "$test_root/expected-calls"
	cmp "$test_root/expected-calls" "$test_root/calls"
	[ ! -e "$test_root/fallback" ]
fi

canonical="$dist/canary-v1.2.3.mcpb"
[ -s "$canonical" ] || {
	echo "build-mcpb test: canonical versioned bundle was not produced" >&2
	exit 1
}
cmp -s "$canonical" "$dist/canary.mcpb" || {
	echo "build-mcpb test: stable bundle differs from versioned canonical asset" >&2
	exit 1
}
cmp -s "$fixture/LICENSE" "$packed_stage/LICENSE" || {
	echo "build-mcpb test: bundle does not preserve the full repository license" >&2
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
