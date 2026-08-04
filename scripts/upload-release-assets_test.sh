#!/usr/bin/env bash

set -euo pipefail

source_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-upload-assets-test.XXXXXX")"
bin="$test_root/bin"
dist="$test_root/dist"
uploader="$source_root/scripts/upload-release-assets.sh"
version="v1.2.3"

cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$bin" "$dist"
for name in one.tar.gz two.tar.gz three.tar.gz SHA256SUMS SHA256SUMS.asc; do
	printf '%s payload\n' "$name" >"$dist/$name"
done

cat >"$bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = "api" ]; then
	printf 'x\n' >>"$TEST_API_CALLS"
	calls="$(wc -l <"$TEST_API_CALLS" | tr -d '[:space:]')"
	if [ "$calls" -lt "${TEST_DRAFT_APPEARS_ON:-1}" ]; then
		exit 0
	fi
	cat "$TEST_DRAFT_IDS"
	exit 0
fi
if [ "$1" = "release" ] && [ "$2" = "upload" ]; then
	[ "$3" = "$TEST_VERSION" ] || exit 95
	asset="$4"
	[ "$5" = "--repo" ] && [ "$6" = "github.com/osauer/canary" ] || exit 94
	printf '%s\n' "${asset##*/}" >>"$TEST_UPLOADED"
	if [ "${asset##*/}" = "${TEST_FAIL_ASSET:-}" ]; then
		exit 1
	fi
	exit 0
fi
exit 97
EOF
chmod 0755 "$bin/gh"

run_uploader() {
	PATH="$bin:/usr/bin:/bin" \
		TEST_VERSION="$version" \
		TEST_DRAFT_IDS="$test_root/draft-ids" \
		TEST_UPLOADED="$test_root/uploaded" \
		TEST_API_CALLS="$test_root/api-calls" \
		TEST_DRAFT_APPEARS_ON="${TEST_DRAFT_APPEARS_ON:-1}" \
		TEST_FAIL_ASSET="${TEST_FAIL_ASSET:-}" \
		RELEASE_UPLOAD_JOBS="${RELEASE_UPLOAD_JOBS:-2}" \
		"$uploader" "$version" \
		"$dist/one.tar.gz" "$dist/two.tar.gz" "$dist/three.tar.gz" \
		"$dist/SHA256SUMS" "$dist/SHA256SUMS.asc"
}

fail() {
	echo "upload-release-assets test: $1" >&2
	exit 1
}

if "$uploader" "$version" 2>/dev/null; then
	fail "missing asset arguments passed"
fi
if "$uploader" not-a-version "$dist/one.tar.gz" 2>/dev/null; then
	fail "malformed version passed"
fi

printf '%s\n' 4242 >"$test_root/draft-ids"

: >"$test_root/uploaded"
run_uploader >/dev/null || fail "canonical parallel upload failed"
[ "$(sort "$test_root/uploaded" | tr '\n' ' ')" = "SHA256SUMS SHA256SUMS.asc one.tar.gz three.tar.gz two.tar.gz " ] ||
	fail "canonical upload did not cover the exact asset set"

# A failure in any parallel job must fail the batch, including one that is
# reaped before later jobs finish.
: >"$test_root/uploaded"
if TEST_FAIL_ASSET=one.tar.gz run_uploader >/dev/null 2>&1; then
	fail "a failed upload did not fail the batch"
fi
: >"$test_root/uploaded"
if TEST_FAIL_ASSET=SHA256SUMS.asc run_uploader >/dev/null 2>&1; then
	fail "a failed final upload did not fail the batch"
fi

# The list endpoint can lag the create that staged the draft. An empty read is
# retried until it appears; anything else is decided on the first read.
: >"$test_root/uploaded"
: >"$test_root/api-calls"
printf '%s\n' 4242 >"$test_root/draft-ids"
TEST_DRAFT_APPEARS_ON=3 run_uploader >/dev/null || fail "a lagging draft read was not retried"
[ "$(sort "$test_root/uploaded" | tr '\n' ' ')" = "SHA256SUMS SHA256SUMS.asc one.tar.gz three.tar.gz two.tar.gz " ] ||
	fail "retried draft read did not upload the exact asset set"
[ "$(wc -l <"$test_root/api-calls" | tr -d '[:space:]')" -eq 3 ] ||
	fail "retry did not poll until the draft appeared"

# Fail-closed on the draft: no draft, or more than one, must never upload.
: >"$test_root/uploaded"
: >"$test_root/draft-ids"
if run_uploader >/dev/null 2>&1; then
	fail "absent draft passed"
fi
[ ! -s "$test_root/uploaded" ] || fail "absent-draft case uploaded something"

: >"$test_root/uploaded"
: >"$test_root/api-calls"
printf '%s\n' 4242 4343 >"$test_root/draft-ids"
if run_uploader >/dev/null 2>&1; then
	fail "ambiguous drafts passed"
fi
[ ! -s "$test_root/uploaded" ] || fail "ambiguous-draft case uploaded something"
[ "$(wc -l <"$test_root/api-calls" | tr -d '[:space:]')" -eq 1 ] ||
	fail "ambiguous drafts were retried instead of failing on the first read"

printf '%s\n' 4242 >"$test_root/draft-ids"
: >"$test_root/uploaded"
if RELEASE_UPLOAD_JOBS=0 run_uploader >/dev/null 2>&1; then
	fail "zero job count passed"
fi
if RELEASE_UPLOAD_JOBS="2; rm -rf /" run_uploader >/dev/null 2>&1; then
	fail "injected job count passed"
fi

: >"$test_root/uploaded"
ln -s "$dist/one.tar.gz" "$dist/link.tar.gz"
if PATH="$bin:/usr/bin:/bin" TEST_VERSION="$version" \
	TEST_DRAFT_IDS="$test_root/draft-ids" TEST_UPLOADED="$test_root/uploaded" \
	TEST_API_CALLS="$test_root/api-calls" \
	"$uploader" "$version" "$dist/link.tar.gz" >/dev/null 2>&1; then
	fail "symlinked asset passed"
fi
[ ! -s "$test_root/uploaded" ] || fail "symlink case uploaded something"

echo "upload-release-assets test: OK"
