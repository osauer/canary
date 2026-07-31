#!/usr/bin/env bash

# Verify an existing GitHub release as a complete publication of the local
# release set. Asset count alone is not authority: metadata, names,
# GitHub digests, checksums, and the detached signature must all agree.

set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: $0 vX.Y.Z dist-dir" >&2
	exit 2
fi

version="$1"
dist_dir="$2"
if ! [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
	echo "check-github-release: version must look like vX.Y.Z (got $version)" >&2
	exit 1
fi
if [ ! -d "$dist_dir" ] || [ -L "$dist_dir" ]; then
	echo "check-github-release: dist directory is missing or unsafe: $dist_dir" >&2
	exit 1
fi
for command in gh python3 gpg; do
	if ! command -v "$command" >/dev/null 2>&1; then
		echo "check-github-release: $command is required" >&2
		exit 1
	fi
done

script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(CDPATH= cd -- "$script_dir/.." && pwd)"
release_key="$repo_root/internal/update/release-signing-key.asc"
updater_contract="$repo_root/internal/update/keyring.go"
expected_signing_fingerprint="D98426D48FED85EFA33904694D922A4F922B7D7D"
if [ ! -f "$release_key" ] || [ -L "$release_key" ]; then
	echo "check-github-release: canonical release-signing key is missing or unsafe" >&2
	exit 1
fi
if [ ! -f "$updater_contract" ] || [ -L "$updater_contract" ]; then
	echo "check-github-release: updater signing contract is missing or unsafe" >&2
	exit 1
fi
updater_fingerprint="$(
	sed -n 's/^var ReleaseSigningKeyFingerprint = "\([0-9A-F]*\)"$/\1/p' \
		"$updater_contract"
)"
if [ "$updater_fingerprint" != "$expected_signing_fingerprint" ]; then
	echo "check-github-release: updater signing fingerprint does not match the pinned Canary release identity" >&2
	exit 1
fi

work="$(mktemp -d "${TMPDIR:-/tmp}/canary-github-release-check.XXXXXX")"
keyring_dir=""
cleanup() {
	rm -rf "$work"
	if [ -n "$keyring_dir" ]; then
		if command -v gpgconf >/dev/null 2>&1; then
			gpgconf --homedir "$keyring_dir" --kill gpg-agent >/dev/null 2>&1 || true
		fi
		rm -rf "$keyring_dir"
	fi
}
trap cleanup EXIT HUP INT TERM

release_json="$work/release.json"
download_dir="$work/download"
notes_source="$work/notes-source"
expected_notes="$work/expected-notes.md"
keyring_dir="$(mktemp -d /tmp/canary-github-release-gpg.XXXXXX)"
mkdir -p "$download_dir" "$notes_source"
chmod 0700 "$keyring_dir"
if ! gpg --homedir "$keyring_dir" --batch --quiet --import "$release_key" \
	</dev/null >/dev/null 2>&1; then
	echo "check-github-release: could not import the canonical release-signing key" >&2
	exit 1
fi
imported_fingerprints="$(
	gpg --homedir "$keyring_dir" --batch --with-colons --fingerprint 2>/dev/null |
		awk -F: '
			$1 == "pub" { want_primary = 1; next }
			want_primary && $1 == "fpr" { print $10; want_primary = 0 }
		'
)"
if [ "$imported_fingerprints" != "$expected_signing_fingerprint" ]; then
	echo "check-github-release: canonical key file does not contain exactly the pinned Canary release identity" >&2
	exit 1
fi

gh api --hostname github.com -X GET \
	"repos/osauer/canary/releases/tags/$version" >"$release_json"
gh release download "$version" --repo github.com/osauer/canary \
	--dir "$download_dir" --pattern SHA256SUMS --pattern SHA256SUMS.asc

python3 "$script_dir/materialize-release-tag-file.py" \
	"$version" CHANGELOG.md "$notes_source/CHANGELOG.md"
python3 "$script_dir/materialize-release-tag-file.py" \
	"$version" .github/release-notes-template.md \
	"$notes_source/release-notes-template.md"
"$script_dir/render-release-notes.sh" "$version" \
	"$notes_source/CHANGELOG.md" "$notes_source/release-notes-template.md" \
	"$expected_notes"

python3 - "$version" "$dist_dir" "$download_dir" "$release_json" "$expected_notes" <<'PY'
import hashlib
import json
import re
import sys
from pathlib import Path

version, dist_arg, download_arg, json_arg, expected_notes_arg = sys.argv[1:]
dist = Path(dist_arg)
download = Path(download_arg)
release_path = Path(json_arg)
expected_notes_path = Path(expected_notes_arg)
targets = ("darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64")
payloads = {
    f"{prefix}-{version}-{target}.tar.gz"
    for target in targets
    for prefix in ("canary", "canary-trading")
}
payloads.update({f"canary-{version}.mcpb", "canary.mcpb"})
expected = payloads | {"SHA256SUMS", "SHA256SUMS.asc"}

try:
    release = json.loads(release_path.read_text(encoding="utf-8"))
except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
    raise SystemExit(f"check-github-release: malformed release JSON: {error}")
if type(release) is not dict:
    raise SystemExit("check-github-release: release JSON must be an object")
if release.get("tag_name") != version:
    raise SystemExit("check-github-release: release tag_name mismatch")
if release.get("draft") is not False:
    raise SystemExit("check-github-release: release is draft or lacks draft=false")
if release.get("prerelease") is not False:
    raise SystemExit("check-github-release: release is prerelease or lacks prerelease=false")
if not isinstance(release.get("name"), str) or not release["name"].strip():
    raise SystemExit("check-github-release: release title is missing")
try:
    expected_body = expected_notes_path.read_text(encoding="utf-8")
except (OSError, UnicodeDecodeError) as error:
    raise SystemExit(f"check-github-release: cannot read expected release notes: {error}")
if release.get("body") != expected_body:
    raise SystemExit("check-github-release: release body differs from the immutable tag")
if not isinstance(release.get("published_at"), str) or not release["published_at"]:
    raise SystemExit("check-github-release: release is not published")

assets = release.get("assets")
if type(assets) is not list:
    raise SystemExit("check-github-release: assets must be an array")
by_name = {}
for asset in assets:
    if type(asset) is not dict or type(asset.get("name")) is not str:
        raise SystemExit("check-github-release: malformed asset record")
    name = asset["name"]
    if name in by_name:
        raise SystemExit(f"check-github-release: duplicate asset {name}")
    if asset.get("state") != "uploaded":
        raise SystemExit(f"check-github-release: asset {name} is not uploaded")
    digest = asset.get("digest")
    if type(digest) is not str or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None:
        raise SystemExit(f"check-github-release: asset {name} lacks a canonical sha256 digest")
    by_name[name] = digest.removeprefix("sha256:")
if set(by_name) != expected:
    missing = sorted(expected - set(by_name))
    unexpected = sorted(set(by_name) - expected)
    raise SystemExit(
        f"check-github-release: asset inventory mismatch missing={missing} unexpected={unexpected}"
    )

remote_sums = download / "SHA256SUMS"
remote_signature = download / "SHA256SUMS.asc"
local_sums = dist / "SHA256SUMS"
local_signature = dist / "SHA256SUMS.asc"
for path in (remote_sums, remote_signature, local_sums, local_signature):
    if not path.is_file() or path.is_symlink():
        raise SystemExit(f"check-github-release: missing or unsafe file {path}")
if remote_sums.read_bytes() != local_sums.read_bytes():
    raise SystemExit("check-github-release: remote SHA256SUMS differs from local file")
if remote_signature.read_bytes() != local_signature.read_bytes():
    raise SystemExit("check-github-release: remote SHA256SUMS.asc differs from local file")

sum_entries = {}
for number, line in enumerate(remote_sums.read_text(encoding="utf-8").splitlines(), 1):
    match = re.fullmatch(r"([0-9a-f]{64})[ \t]+[*]?([A-Za-z0-9._-]+)", line)
    if match is None:
        raise SystemExit(f"check-github-release: malformed SHA256SUMS line {number}")
    digest, name = match.groups()
    if name in sum_entries:
        raise SystemExit(f"check-github-release: duplicate checksum entry {name}")
    sum_entries[name] = digest
if set(sum_entries) != payloads:
    raise SystemExit("check-github-release: SHA256SUMS payload inventory is not exact")
if sum_entries[f"canary-{version}.mcpb"] != sum_entries["canary.mcpb"]:
    raise SystemExit(
        "check-github-release: stable MCPB alias differs from the versioned bundle"
    )

def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()

local_payloads = {
    path.name
    for path in dist.iterdir()
    if path.is_file() and (path.name.endswith(".tar.gz") or path.name.endswith(".mcpb"))
}
if local_payloads != payloads:
    raise SystemExit("check-github-release: local payload inventory is not exact")

for name in sorted(payloads):
    path = dist / name
    if not path.is_file() or path.is_symlink():
        raise SystemExit(f"check-github-release: missing local payload {name}")
    actual = sha256(path)
    if sum_entries[name] != actual:
        raise SystemExit(f"check-github-release: local payload checksum mismatch for {name}")
    if by_name[name] != actual:
        raise SystemExit(f"check-github-release: GitHub digest mismatch for {name}")
if by_name["SHA256SUMS"] != sha256(remote_sums):
    raise SystemExit("check-github-release: GitHub digest mismatch for SHA256SUMS")
if by_name["SHA256SUMS.asc"] != sha256(remote_signature):
    raise SystemExit("check-github-release: GitHub digest mismatch for SHA256SUMS.asc")
if by_name["SHA256SUMS.asc"] != sha256(local_signature):
    raise SystemExit("check-github-release: local SHA256SUMS.asc digest mismatch")
PY

gpg --homedir "$keyring_dir" --batch --no-auto-key-retrieve \
	--verify "$download_dir/SHA256SUMS.asc" "$download_dir/SHA256SUMS" \
	>/dev/null 2>&1 || {
	echo "check-github-release: remote checksum signature did not verify against the pinned Canary release identity" >&2
	exit 1
}

echo "check-github-release: OK version=$version assets=12"
