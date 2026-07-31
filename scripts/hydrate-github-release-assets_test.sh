#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

source_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-hydrate-release-test.XXXXXX")"
repo="$test_root/repo"
remote="$test_root/remote"
fake_bin="$test_root/bin"
version="v1.2.3"

cleanup() {
	rm -rf -- "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$repo/scripts" "$repo/internal/update" "$repo/.github" "$remote" "$fake_bin"
cp "$source_root/scripts/hydrate-github-release-assets.sh" "$repo/scripts/"
cp "$source_root/scripts/check-github-release.sh" "$repo/scripts/"
cp "$source_root/scripts/materialize-release-tag-file.py" "$repo/scripts/"
cp "$source_root/scripts/render-release-notes.sh" "$repo/scripts/"
cp "$source_root/internal/update/release-signing-key.asc" "$repo/internal/update/"
cp "$source_root/internal/update/keyring.go" "$repo/internal/update/"
git -C "$repo" init -q
git -C "$repo" config user.name "Canary Test"
git -C "$repo" config user.email "test@canary.invalid"
cat >"$repo/CHANGELOG.md" <<'EOF'
# Changelog

## v1.2.3 (2026-07-31)

### What's new

- Exact release authority.

### Fixed

- Recovery remains tag-bound.
EOF
cat >"$repo/.github/release-notes-template.md" <<'EOF'
# Canary __VERSION__

__HIGHLIGHTS__

## Details
EOF
git -C "$repo" add .
git -C "$repo" commit -qm fixture
git -C "$repo" tag -a "$version" -m fixture
"$repo/scripts/render-release-notes.sh" "$version" \
	"$repo/CHANGELOG.md" "$repo/.github/release-notes-template.md" \
	"$test_root/expected-notes.md"

for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
	for prefix in canary canary-trading; do
		name="$prefix-$version-$target.tar.gz"
		printf '%s\n' "$name payload" >"$remote/$name"
	done
done
printf '%s\n' versioned-mcpb >"$remote/canary-$version.mcpb"
cp "$remote/canary-$version.mcpb" "$remote/canary.mcpb"
(
	cd "$remote"
	shasum -a 256 \
		canary-"$version"-darwin-arm64.tar.gz \
		canary-trading-"$version"-darwin-arm64.tar.gz \
		canary-"$version"-darwin-amd64.tar.gz \
		canary-trading-"$version"-darwin-amd64.tar.gz \
		canary-"$version"-linux-amd64.tar.gz \
		canary-trading-"$version"-linux-amd64.tar.gz \
		canary-"$version"-linux-arm64.tar.gz \
		canary-trading-"$version"-linux-arm64.tar.gz \
		canary-"$version".mcpb canary.mcpb >SHA256SUMS
)
printf '%s\n' fixture-signature >"$remote/SHA256SUMS.asc"

python3 - "$version" "$remote" "$test_root/release.json" \
	"$test_root/expected-notes.md" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

version, remote_arg, output_arg, notes_arg = sys.argv[1:]
remote = Path(remote_arg)
assets = []
for path in sorted(remote.iterdir()):
    if path.is_file():
        assets.append(
            {
                "name": path.name,
                "state": "uploaded",
                "digest": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
            }
        )
document = {
    "tag_name": version,
    "draft": False,
    "prerelease": False,
    "name": version,
    "body": Path(notes_arg).read_text(encoding="utf-8"),
    "published_at": "2026-07-31T00:00:00Z",
    "assets": assets,
}
Path(output_arg).write_text(json.dumps(document), encoding="utf-8")
PY
cp "$test_root/release.json" "$test_root/release.canonical.json"

cat >"$fake_bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = api ]; then
	[ "$*" = \
		"api --hostname github.com -X GET repos/osauer/canary/releases/tags/$TEST_VERSION" ] \
		|| exit 98
	cat "$TEST_RELEASE_JSON"
	exit 0
fi
if [ "$1" = release ] && [ "$2" = download ]; then
	[ "$3" = "$TEST_VERSION" ] || exit 97
	destination=""
	repo_count=0
	pattern_count=0
	seen_patterns="|"
	shift 3
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--dir)
			destination="$2"
			shift 2
			;;
		--repo)
			[ "$2" = github.com/osauer/canary ] || exit 96
			repo_count=$((repo_count + 1))
			shift 2
			;;
		--pattern)
			pattern="$2"
			case "$seen_patterns" in
			*"|$pattern|"*) exit 94 ;;
			esac
			case "$pattern" in
			"canary-$TEST_VERSION-darwin-arm64.tar.gz" | \
				"canary-trading-$TEST_VERSION-darwin-arm64.tar.gz" | \
				"canary-$TEST_VERSION-darwin-amd64.tar.gz" | \
				"canary-trading-$TEST_VERSION-darwin-amd64.tar.gz" | \
				"canary-$TEST_VERSION-linux-amd64.tar.gz" | \
				"canary-trading-$TEST_VERSION-linux-amd64.tar.gz" | \
				"canary-$TEST_VERSION-linux-arm64.tar.gz" | \
				"canary-trading-$TEST_VERSION-linux-arm64.tar.gz" | \
				"canary-$TEST_VERSION.mcpb" | canary.mcpb | \
				SHA256SUMS | SHA256SUMS.asc) ;;
			*) exit 93 ;;
			esac
			seen_patterns="${seen_patterns}${pattern}|"
			pattern_count=$((pattern_count + 1))
			shift 2
			;;
		*)
			exit 92
			;;
		esac
	done
	[ "$repo_count" -eq 1 ] && [ -n "$destination" ] || exit 95
	case "$pattern_count" in
	2)
		case "$seen_patterns" in
		*"|SHA256SUMS|"*) ;;
		*) exit 91 ;;
		esac
		case "$seen_patterns" in
		*"|SHA256SUMS.asc|"*) ;;
		*) exit 90 ;;
		esac
		;;
	12) ;;
	*) exit 89 ;;
	esac
	old_ifs="$IFS"
	IFS='|'
	for pattern in $seen_patterns; do
		[ -n "$pattern" ] || continue
		cp "$TEST_REMOTE_ASSETS/$pattern" "$destination/"
	done
	IFS="$old_ifs"
	exit 0
fi
exit 88
SH
cat >"$fake_bin/gpg" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
for argument in "$@"; do
	if [ "$argument" = --with-colons ]; then
		printf '%s\n' 'pub:-:255:22:4D922A4F922B7D7D:0::::::scSC:::::ed25519:::0:'
		printf '%s\n' 'fpr:::::::::D98426D48FED85EFA33904694D922A4F922B7D7D:'
		exit 0
	fi
	if [ "$argument" = --verify ]; then
		exit 0
	fi
done
exit 0
SH
chmod 0755 "$fake_bin/gh" "$fake_bin/gpg"

hydrate() {
	PATH="$fake_bin:/usr/bin:/bin" \
		TEST_RELEASE_JSON="$test_root/release.json" \
		TEST_REMOTE_ASSETS="$remote" \
		TEST_VERSION="$version" \
		"$repo/scripts/hydrate-github-release-assets.sh" "$version" "$repo/dist"
}

hydrate >/dev/null
[ -f "$repo/dist/.canary-release-output" ] || {
	echo "hydrate-github-release-assets test: ownership marker missing" >&2
	exit 1
}
[ -f "$repo/dist/SHA256SUMS" ] && [ ! -L "$repo/dist/SHA256SUMS" ] || {
	echo "hydrate-github-release-assets test: exact asset set was not installed" >&2
	exit 1
}

printf '%s\n' victim-original >"$test_root/victim"
rm "$repo/dist/SHA256SUMS"
ln -s "$test_root/victim" "$repo/dist/SHA256SUMS"
hydrate >/dev/null
[ "$(cat "$test_root/victim")" = victim-original ] || {
	echo "hydrate-github-release-assets test: wrote through an existing asset symlink" >&2
	exit 1
}
[ -f "$repo/dist/SHA256SUMS" ] && [ ! -L "$repo/dist/SHA256SUMS" ] || {
	echo "hydrate-github-release-assets test: did not replace symlinked output safely" >&2
	exit 1
}

printf '%s\n' preserve-on-verification-failure >"$repo/dist/sentinel"
python3 - "$test_root/release.json" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
document = json.loads(path.read_text(encoding="utf-8"))
document["assets"][0]["digest"] = "sha256:" + "0" * 64
path.write_text(json.dumps(document), encoding="utf-8")
PY
if hydrate >"$test_root/output" 2>&1; then
	echo "hydrate-github-release-assets test: installed assets after failed verification" >&2
	exit 1
fi
[ -f "$repo/dist/sentinel" ] || {
	echo "hydrate-github-release-assets test: modified owned output before verification" >&2
	exit 1
}
cp "$test_root/release.canonical.json" "$test_root/release.json"

rm -rf -- "$repo/dist"
mkdir "$repo/dist"
printf '%s\n' unowned >"$repo/dist/sentinel"
if hydrate >"$test_root/output" 2>&1; then
	echo "hydrate-github-release-assets test: replaced unowned output" >&2
	exit 1
fi
[ "$(cat "$repo/dist/sentinel")" = unowned ] || {
	echo "hydrate-github-release-assets test: changed unowned output" >&2
	exit 1
}

rm -rf -- "$repo/dist"
mkdir "$test_root/symlink-target"
printf '%s\n' symlink-target >"$test_root/symlink-target/sentinel"
ln -s "$test_root/symlink-target" "$repo/dist"
if hydrate >"$test_root/output" 2>&1; then
	echo "hydrate-github-release-assets test: accepted symlink dist" >&2
	exit 1
fi
[ "$(cat "$test_root/symlink-target/sentinel")" = symlink-target ] || {
	echo "hydrate-github-release-assets test: changed symlink target" >&2
	exit 1
}

echo "hydrate-github-release-assets test: PASS"
