#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-artifacts-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

owner="$test_root/owner"
source_checkout="$test_root/source"
dist="$owner/dist"
targets="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
mkdir -p "$owner/scripts" "$owner/internal/update" "$owner/fake-bin"
cp "$repo_root/scripts/build-release-artifacts.sh" "$owner/scripts/"
printf '%s\n' 'package update' 'const ReleaseSigningKeyFingerprint = "FIXTURE-FINGERPRINT"' > "$owner/internal/update/keyring.go"

cat > "$owner/fake-bin/gpg" <<'EOF'
#!/bin/sh
set -eu
case " $* " in
	*" --list-secret-keys "*) exit 0 ;;
	*" --verify "*) exit 0 ;;
esac
out=""
while [ "$#" -gt 0 ]; do
	if [ "$1" = "--output" ]; then
		out="$2"
		break
	fi
	shift
done
[ -n "$out" ] || exit 2
printf '%s\n' 'fixture detached signature' > "$out"
EOF
chmod 0755 "$owner/fake-bin/gpg"

cat > "$owner/scripts/build-release-target.sh" <<'EOF'
#!/bin/sh
set -eu
target="$1"
version="$2"
dist="$4"
for prefix in canary canary-trading; do
	printf '%s\n' "$prefix fixture" > "$dist/$prefix-$version-$target.tar.gz"
done
EOF
chmod 0755 "$owner/scripts/build-release-target.sh"

cat > "$owner/scripts/build-mcpb.sh" <<'EOF'
#!/bin/sh
set -eu
version="$1"
dist="$2"
printf '%s\n' 'mcpb fixture' > "$dist/canary-$version.mcpb"
cp "$dist/canary-$version.mcpb" "$dist/canary.mcpb"
EOF
chmod 0755 "$owner/scripts/build-mcpb.sh"

git init --quiet "$owner"
git -C "$owner" config user.name "Release Fixture"
git -C "$owner" config user.email "release-fixture@example.invalid"
git -C "$owner" add .
git -C "$owner" commit --quiet -m fixture
git -C "$owner" tag v1.2.3
git clone --quiet "$owner" "$source_checkout"
git -C "$source_checkout" checkout --quiet --detach v1.2.3

run_build() {
	(
		cd "$source_checkout"
		PATH="$owner/fake-bin:$PATH" \
			"$owner/scripts/build-release-artifacts.sh" "$@"
	)
}

assert_rejected_without_deletion() {
	local label="$1" candidate="$2" sentinel="$3"
	shift 3
	if run_build all v1.2.3 "$candidate" "$targets" 1 '-s -w' >/dev/null 2>&1; then
		echo "build-release-artifacts test: accepted unsafe $label output" >&2
		exit 1
	fi
	[ -e "$sentinel" ] || {
		echo "build-release-artifacts test: deleted $label sentinel" >&2
		exit 1
	}
}

fake_home="$test_root/home"
ancestor="$test_root"
other_repo="$test_root/other-repo"
arbitrary="$test_root/arbitrary"
mkdir -p "$fake_home" "$other_repo/dist" "$arbitrary"
git init --quiet "$other_repo"
touch "$fake_home/sentinel" "$ancestor/ancestor-sentinel" "$other_repo/dist/sentinel" "$arbitrary/sentinel"
assert_rejected_without_deletion home "$fake_home" "$fake_home/sentinel"
assert_rejected_without_deletion ancestor "$ancestor" "$ancestor/ancestor-sentinel"
assert_rejected_without_deletion other-repo "$other_repo/dist" "$other_repo/dist/sentinel"
assert_rejected_without_deletion arbitrary-absolute "$arbitrary" "$arbitrary/sentinel"

mkdir -p "$test_root/symlink-victim"
touch "$test_root/symlink-victim/sentinel"
ln -s "$test_root/symlink-victim" "$dist"
assert_rejected_without_deletion symlink "$dist" "$test_root/symlink-victim/sentinel"
rm "$dist"

mkdir -p "$dist"
touch "$dist/sentinel"
assert_rejected_without_deletion unowned-directory "$dist" "$dist/sentinel"
rm -rf "$dist"

run_build all v1.2.3 "$dist" "$targets" 1 '-s -w'
[ -f "$dist/.canary-release-output" ] || {
	echo "build-release-artifacts test: owned output marker missing" >&2
	exit 1
}

printf '%s\n' stale > "$dist/stale"
run_build all v1.2.3 "$dist" "$targets" 1 '-s -w'
[ ! -e "$dist/stale" ] || {
	echo "build-release-artifacts test: owned output was not replaced" >&2
	exit 1
}

expected="$(printf '%s\n' \
	'canary-v1.2.3-darwin-arm64.tar.gz' \
	'canary-trading-v1.2.3-darwin-arm64.tar.gz' \
	'canary-v1.2.3-darwin-amd64.tar.gz' \
	'canary-trading-v1.2.3-darwin-amd64.tar.gz' \
	'canary-v1.2.3-linux-amd64.tar.gz' \
	'canary-trading-v1.2.3-linux-amd64.tar.gz' \
	'canary-v1.2.3-linux-arm64.tar.gz' \
	'canary-trading-v1.2.3-linux-arm64.tar.gz' \
	'canary-v1.2.3.mcpb' \
	'canary.mcpb')"
actual="$(awk '{print $2}' "$dist/SHA256SUMS")"
[ "$actual" = "$expected" ] || {
	echo "build-release-artifacts test: checksum inventory mismatch" >&2
	diff -u <(printf '%s\n' "$expected") <(printf '%s\n' "$actual") >&2 || true
	exit 1
}
[ -s "$dist/SHA256SUMS.asc" ] || {
	echo "build-release-artifacts test: detached signature was not produced" >&2
	exit 1
}

# Any additional MCPB would be selected by a broad release glob. Checksums mode must
# reject it instead of signing an unintended publication asset.
printf '%s\n' 'stale' > "$dist/old-name.mcpb"
if run_build checksums v1.2.3 "$dist" "$targets" 1 '-s -w' >/dev/null 2>&1; then
	echo "build-release-artifacts test: unexpected MCPB asset was signed" >&2
	exit 1
fi
rm "$dist/old-name.mcpb"

# A stale historical trading archive would be an unintended fourth authority
# shape. Checksums mode must reject it rather than silently sign it.
printf '%s\n' 'stale' > "$dist/ibkr-trading-v1.2.3-darwin-arm64.tar.gz"
if run_build checksums v1.2.3 "$dist" "$targets" 1 '-s -w' >/dev/null 2>&1; then
	echo "build-release-artifacts test: unexpected legacy trading tarball was signed" >&2
	exit 1
fi
rm "$dist/ibkr-trading-v1.2.3-darwin-arm64.tar.gz"

rm "$dist/.canary-release-output"
ln -s "$test_root/symlink-victim/sentinel" "$dist/.canary-release-output"
if run_build all v1.2.3 "$dist" "$targets" 1 '-s -w' >/dev/null 2>&1; then
	echo "build-release-artifacts test: accepted symlink ownership marker" >&2
	exit 1
fi
[ -e "$test_root/symlink-victim/sentinel" ] || {
	echo "build-release-artifacts test: altered symlink-marker target" >&2
	exit 1
}

echo "build-release-artifacts test: OK"
