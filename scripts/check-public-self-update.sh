#!/usr/bin/env bash

# Prove the public stable update path from the prior same-major standard
# release to the just-published release. The prior executable is trusted only
# after its archive has been checked against Canary's signed checksum file.

set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: $0 vX.Y.Z" >&2
	exit 2
fi

version="$1"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "check-public-self-update: version must be a stable vX.Y.Z release (got $version)" >&2
	exit 2
fi

for command in gh git go python3; do
	if ! command -v "$command" >/dev/null 2>&1; then
		echo "check-public-self-update: $command is required" >&2
		exit 1
	fi
done

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(CDPATH= cd -- "$script_dir/.." && pwd)"
compact_key="$repo_root/internal/update/release-signing-key.ed25519.pem"
compact_verifier="$repo_root/scripts/release-sign-ed25519"
if [ ! -f "$compact_key" ] || [ -L "$compact_key" ] \
	|| [ ! -f "$compact_verifier/main.go" ] || [ -L "$compact_verifier/main.go" ]; then
	echo "check-public-self-update: compact release verifier is missing or unsafe" >&2
	exit 1
fi

target_commit="$(git -C "$repo_root" rev-parse --verify "refs/tags/$version^{commit}")" || {
	echo "check-public-self-update: local release tag is missing: $version" >&2
	exit 1
}
if ! [[ "$target_commit" =~ ^[0-9a-f]{40}$ ]]; then
	echo "check-public-self-update: release tag did not resolve to a full commit" >&2
	exit 1
fi

witness_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-public-self-update.XXXXXX")"
cleanup() {
	rm -rf "$witness_root"
}
trap cleanup EXIT HUP INT TERM

releases_json="$witness_root/releases.json"
gh api --hostname github.com -X GET \
	"repos/osauer/canary/releases?per_page=100" >"$releases_json"
previous_version="$(python3 - "$version" "$releases_json" <<'PY'
import json
import re
import sys
from pathlib import Path

target, releases_path = sys.argv[1:]
match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", target)
if match is None:
    raise SystemExit("check-public-self-update: target is not stable semver")
target_key = tuple(map(int, match.groups()))
try:
    releases = json.loads(Path(releases_path).read_text(encoding="utf-8"))
except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
    raise SystemExit(f"check-public-self-update: malformed GitHub release list: {error}")
if type(releases) is not list:
    raise SystemExit("check-public-self-update: GitHub release list must be an array")

stable = {}
for release in releases:
    if type(release) is not dict:
        continue
    if release.get("draft") is not False or release.get("prerelease") is not False:
        continue
    tag = release.get("tag_name")
    if type(tag) is not str:
        continue
    parsed = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", tag)
    if parsed is None:
        continue
    stable[tuple(map(int, parsed.groups()))] = tag

if target_key not in stable:
    raise SystemExit(f"check-public-self-update: {target} is not a published stable release")
candidates = [key for key in stable if key[0] == target_key[0] and key < target_key]
if not candidates:
    raise SystemExit(f"check-public-self-update: no prior stable v{target_key[0]} release is published")
print(stable[max(candidates)])
PY
)" || exit 1

host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
case "$host_os/$host_arch" in
	darwin/arm64 | darwin/amd64 | linux/amd64 | linux/arm64) ;;
	*)
		echo "check-public-self-update: no standard release asset for $host_os/$host_arch" >&2
		exit 1
		;;
esac

asset_name="canary-${previous_version}-${host_os}-${host_arch}.tar.gz"
download_dir="$witness_root/download"
mkdir -p "$download_dir"
gh release download "$previous_version" --repo github.com/osauer/canary \
	--dir "$download_dir" \
	--pattern "$asset_name" \
	--pattern SHA256SUMS \
	--pattern SHA256SUMS.ed25519

for downloaded in "$asset_name" SHA256SUMS SHA256SUMS.ed25519; do
	if [ ! -f "$download_dir/$downloaded" ] || [ -L "$download_dir/$downloaded" ]; then
		echo "check-public-self-update: prior release is missing safe asset $downloaded" >&2
		exit 1
	fi
done

go run "$compact_verifier" \
	-public "$compact_key" \
	-input "$download_dir/SHA256SUMS" \
	-verify "$download_dir/SHA256SUMS.ed25519" >/dev/null || {
	echo "check-public-self-update: prior checksum signature did not verify" >&2
	exit 1
}

python3 - "$download_dir/SHA256SUMS" "$download_dir/$asset_name" "$asset_name" <<'PY'
import hashlib
import re
import sys
from pathlib import Path

sums_path, archive_path, asset_name = map(Path, sys.argv[1:])
entries = {}
for number, line in enumerate(sums_path.read_text(encoding="utf-8").splitlines(), 1):
    match = re.fullmatch(r"([0-9a-f]{64})[ \t]+[*]?([A-Za-z0-9._-]+)", line)
    if match is None:
        raise SystemExit(f"check-public-self-update: malformed checksum line {number}")
    digest, name = match.groups()
    if name in entries:
        raise SystemExit(f"check-public-self-update: duplicate checksum entry {name}")
    entries[name] = digest
wanted = asset_name.name
if wanted not in entries:
    raise SystemExit(f"check-public-self-update: signed checksums omit {wanted}")
actual = hashlib.sha256(archive_path.read_bytes()).hexdigest()
if actual != entries[wanted]:
    raise SystemExit(f"check-public-self-update: checksum mismatch for {wanted}")
PY

archive_root="${asset_name%.tar.gz}"
previous_binary="$witness_root/previous-canary"
python3 - "$download_dir/$asset_name" "$archive_root/canary" "$previous_binary" <<'PY'
import os
import sys
import tarfile
from pathlib import Path

archive_arg, member_name, output_arg = sys.argv[1:]
with tarfile.open(archive_arg, mode="r:gz") as archive:
    members = [member for member in archive.getmembers() if member.name == member_name]
    if len(members) != 1 or not members[0].isfile():
        raise SystemExit("check-public-self-update: prior archive lacks one regular canonical binary")
    source = archive.extractfile(members[0])
    if source is None:
        raise SystemExit("check-public-self-update: prior canonical binary is unreadable")
    output = Path(output_arg)
    output.write_bytes(source.read())
    os.chmod(output, members[0].mode & 0o777)
PY
if [ ! -x "$previous_binary" ] || [ -L "$previous_binary" ]; then
	echo "check-public-self-update: prior canonical binary is not safely executable" >&2
	exit 1
fi

assert_version() {
	local binary="$1" expected_version="$2" expected_commit="$3" output="$4"
	"$binary" version --json >"$output"
	python3 - "$output" "$expected_version" "$expected_commit" "$host_os" "$host_arch" <<'PY'
import json
import re
import sys
from pathlib import Path

path, expected_version, expected_commit, expected_os, expected_arch = sys.argv[1:]
try:
    value = json.loads(Path(path).read_text(encoding="utf-8"))
except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
    raise SystemExit(f"check-public-self-update: malformed version evidence: {error}")
if type(value) is not dict or value.get("program") != "canary":
    raise SystemExit("check-public-self-update: version evidence is not Canary")
if value.get("version") != expected_version:
    raise SystemExit("check-public-self-update: installed version mismatch")
if expected_commit and value.get("commit") != expected_commit:
    raise SystemExit("check-public-self-update: installed commit mismatch")
if value.get("goos") != expected_os or value.get("goarch") != expected_arch:
    raise SystemExit("check-public-self-update: installed host target mismatch")
if expected_commit and value.get("vcs_state") not in (None, "", "clean"):
    raise SystemExit("check-public-self-update: installed release reports modified provenance")
commit = value.get("commit")
if expected_commit and re.fullmatch(r"[0-9a-f]{40}", commit or "") is None:
    raise SystemExit("check-public-self-update: installed release lacks a full commit stamp")
PY
}

assert_standard_binary() {
	local binary="$1" settings
	settings="$(go version -m "$binary")" || {
		echo "check-public-self-update: cannot inspect binary build settings" >&2
		exit 1
	}
	if grep -Eq 'build[[:space:]]+-tags=.*trading' <<<"$settings"; then
		echo "check-public-self-update: standard asset carries the trading build tag" >&2
		exit 1
	fi
}

assert_version "$previous_binary" "$previous_version" "" "$witness_root/previous-version.json"
assert_standard_binary "$previous_binary"

install_dir="$witness_root/install"
cache_dir="$witness_root/cache"
config_dir="$witness_root/config"
state_dir="$witness_root/state"
runtime_dir="$witness_root/runtime"
witness_home="$witness_root/home"
witness_tmp="$witness_root/tmp"
witness_socket="$runtime_dir/canary.sock"
mkdir -p "$install_dir" "$cache_dir" "$config_dir" "$state_dir" \
	"$runtime_dir" "$witness_home" "$witness_tmp"

if ! env -i \
	PATH="$PATH" \
	HOME="$witness_home" \
	TMPDIR="$witness_tmp" \
	CANARY_INSTALL_DIR="$install_dir" \
	CANARY_SOCKET="$witness_socket" \
	XDG_CACHE_HOME="$cache_dir" \
	XDG_CONFIG_HOME="$config_dir" \
	XDG_STATE_HOME="$state_dir" \
	XDG_RUNTIME_DIR="$runtime_dir" \
	"$previous_binary" update --no-restart \
	>"$witness_root/update.log" 2>&1; then
	echo "check-public-self-update: public updater failed" >&2
	sed -n '1,80p' "$witness_root/update.log" >&2
	exit 1
fi

installed_binary="$install_dir/canary"
if [ ! -x "$installed_binary" ] || [ -L "$installed_binary" ]; then
	echo "check-public-self-update: updater did not install one safe canonical binary" >&2
	exit 1
fi
if [ -e "$witness_socket" ] || [ -L "$witness_socket" ]; then
	echo "check-public-self-update: no-restart witness unexpectedly created a daemon socket" >&2
	exit 1
fi

assert_version "$installed_binary" "$version" "$target_commit" "$witness_root/installed-version.json"
assert_standard_binary "$installed_binary"

echo "check-public-self-update: OK previous=$previous_version target=$version host=$host_os/$host_arch variant=standard restart=no"
