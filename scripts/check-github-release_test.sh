#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null

original_path="$PATH"
source_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-github-release-test.XXXXXX")"
repo="$test_root/repo"
checker="$repo/scripts/check-github-release.sh"
dist="$test_root/dist"
remote="$test_root/remote"
bin="$test_root/bin"
real_bin="$test_root/real-bin"
version="v1.2.3"
expected_fingerprint="D98426D48FED85EFA33904694D922A4F922B7D7D"
attacker_home=""

cleanup() {
	rm -rf "$test_root"
	if [ -n "$attacker_home" ]; then
		if command -v gpgconf >/dev/null 2>&1; then
			gpgconf --homedir "$attacker_home" --kill gpg-agent >/dev/null 2>&1 || true
		fi
		rm -rf "$attacker_home"
	fi
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$repo/scripts" "$repo/internal/update" "$repo/.github" \
	"$dist" "$remote" "$bin" "$real_bin"
cp "$source_root/scripts/check-github-release.sh" "$repo/scripts/"
cp "$source_root/scripts/materialize-release-tag-file.py" "$repo/scripts/"
cp "$source_root/scripts/render-release-notes.sh" "$repo/scripts/"
cp "$source_root/internal/update/release-signing-key.asc" "$repo/internal/update/"
cp "$source_root/internal/update/keyring.go" "$repo/internal/update/"
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
git -C "$repo" init -q
git -C "$repo" config user.name "Canary Test"
git -C "$repo" config user.email "test@canary.invalid"
git -C "$repo" add .
git -C "$repo" commit -qm fixture
git -C "$repo" tag -a "$version" -m fixture

for target in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
	for prefix in canary canary-trading; do
		name="$prefix-$version-$target.tar.gz"
		printf '%s\n' "$name payload" >"$dist/$name"
	done
done
printf '%s\n' versioned-mcpb >"$dist/canary-$version.mcpb"
cp "$dist/canary-$version.mcpb" "$dist/canary.mcpb"

write_sums() (
	cd "$dist"
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
write_sums
cp "$dist/SHA256SUMS" "$remote/SHA256SUMS"
printf '%s\n' fixture-signature >"$remote/SHA256SUMS.asc"
cp "$remote/SHA256SUMS.asc" "$dist/SHA256SUMS.asc"

"$repo/scripts/render-release-notes.sh" "$version" \
	"$repo/CHANGELOG.md" "$repo/.github/release-notes-template.md" \
	"$test_root/expected-notes.md"

python3 - "$version" "$dist" "$remote" "$test_root/release.json" \
	"$test_root/expected-notes.md" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

version, dist_arg, remote_arg, output_arg, notes_arg = sys.argv[1:]
dist = Path(dist_arg)
remote = Path(remote_arg)
names = [path.name for path in dist.iterdir() if path.is_file()]
names.append("SHA256SUMS.asc")
assets = []
for name in sorted(set(names)):
    path = remote / name if name == "SHA256SUMS.asc" else dist / name
    assets.append(
        {
            "name": name,
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

cat >"$bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = "api" ]; then
	[ "$*" = "api --hostname github.com -X GET repos/osauer/canary/releases/tags/$TEST_VERSION" ] || exit 96
	cat "$TEST_RELEASE_JSON"
	exit 0
fi
if [ "$1" = "release" ] && [ "$2" = "download" ]; then
	[ "$3" = "$TEST_VERSION" ] || exit 95
	destination=""
	repo_count=0
	sum_pattern_count=0
	signature_pattern_count=0
	shift 3
	while [ "$#" -gt 0 ]; do
		case "$1" in
			--dir) destination="$2"; shift 2 ;;
			--repo)
				[ "$2" = github.com/osauer/canary ] || exit 94
				repo_count=$((repo_count + 1))
				shift 2
				;;
			--pattern)
				case "$2" in
				SHA256SUMS) sum_pattern_count=$((sum_pattern_count + 1)) ;;
				SHA256SUMS.asc) signature_pattern_count=$((signature_pattern_count + 1)) ;;
				*) exit 93 ;;
				esac
				shift 2
				;;
			*) shift ;;
		esac
	done
	[ "$repo_count" -eq 1 ] \
		&& [ "$sum_pattern_count" -eq 1 ] \
		&& [ "$signature_pattern_count" -eq 1 ] \
		&& [ -n "$destination" ] || exit 92
	cp "$TEST_REMOTE_ASSETS/SHA256SUMS" "$destination/SHA256SUMS"
	cp "$TEST_REMOTE_ASSETS/SHA256SUMS.asc" "$destination/SHA256SUMS.asc"
	exit 0
fi
exit 97
EOF
cat >"$bin/gpg" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
for argument in "$@"; do
	if [ "$argument" = "--with-colons" ]; then
		printf '%s\n' 'pub:-:255:22:4D922A4F922B7D7D:0::::::scSC:::::ed25519:::0:'
		printf 'fpr:::::::::%s:\n' "${TEST_GPG_FINGERPRINT:-D98426D48FED85EFA33904694D922A4F922B7D7D}"
		exit 0
	fi
done
for argument in "$@"; do
	if [ "$argument" = "--verify" ]; then
		exit "${TEST_GPG_EXIT:-0}"
	fi
done
exit 0
EOF
chmod 0755 "$bin/gh" "$bin/gpg"
cp "$bin/gh" "$real_bin/gh"

run_checker() {
	PATH="$bin:/usr/bin:/bin" \
		TEST_RELEASE_JSON="$test_root/release.json" \
		TEST_REMOTE_ASSETS="$remote" \
		TEST_VERSION="$version" \
		TEST_GPG_FINGERPRINT="${TEST_GPG_FINGERPRINT:-$expected_fingerprint}" \
		TEST_GPG_EXIT="${TEST_GPG_EXIT:-0}" \
		"$checker" "$version" "$dist"
}

run_checker_real_gpg() {
	PATH="$real_bin:$original_path" \
		GNUPGHOME="$attacker_home" \
		TEST_RELEASE_JSON="$test_root/release.json" \
		TEST_REMOTE_ASSETS="$remote" \
		TEST_VERSION="$version" \
		"$checker" "$version" "$dist"
}

run_checker >/dev/null
cp "$test_root/release.json" "$test_root/release.canonical.json"
cp "$dist/canary.mcpb" "$test_root/stable-mcpb.canonical"
cp "$dist/SHA256SUMS" "$test_root/SHA256SUMS.canonical"

for mutation in draft wrong_tag empty_title missing_body wrong_body missing_asset wrong_digest; do
	cp "$test_root/release.canonical.json" "$test_root/release.json"
	python3 - "$test_root/release.json" "$mutation" <<'PY'
import json
import sys
from pathlib import Path

path = Path(sys.argv[1])
mutation = sys.argv[2]
document = json.loads(path.read_text(encoding="utf-8"))
if mutation == "draft":
    document["draft"] = True
elif mutation == "wrong_tag":
    document["tag_name"] = "v9.9.9"
elif mutation == "empty_title":
    document["name"] = ""
elif mutation == "missing_body":
    del document["body"]
elif mutation == "wrong_body":
    document["body"] += "\nUntrusted install guidance.\n"
elif mutation == "missing_asset":
    document["assets"].pop()
elif mutation == "wrong_digest":
    document["assets"][0]["digest"] = "sha256:" + "0" * 64
path.write_text(json.dumps(document), encoding="utf-8")
PY
	if run_checker >/dev/null 2>&1; then
		echo "check-github-release test: $mutation release passed" >&2
		exit 1
	fi
done

cp "$test_root/release.canonical.json" "$test_root/release.json"
printf '%s\n' divergent-stable-alias >"$dist/canary.mcpb"
write_sums
cp "$dist/SHA256SUMS" "$remote/SHA256SUMS"
python3 - "$test_root/release.json" "$dist" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

release_path = Path(sys.argv[1])
dist = Path(sys.argv[2])
document = json.loads(release_path.read_text(encoding="utf-8"))
for asset in document["assets"]:
    if asset["name"] in {"canary.mcpb", "SHA256SUMS"}:
        asset["digest"] = (
            "sha256:" + hashlib.sha256((dist / asset["name"]).read_bytes()).hexdigest()
        )
release_path.write_text(json.dumps(document), encoding="utf-8")
PY
if run_checker >/dev/null 2>&1; then
	echo "check-github-release test: divergent stable MCPB alias passed" >&2
	exit 1
fi
cp "$test_root/stable-mcpb.canonical" "$dist/canary.mcpb"
cp "$test_root/SHA256SUMS.canonical" "$dist/SHA256SUMS"
cp "$test_root/SHA256SUMS.canonical" "$remote/SHA256SUMS"
cp "$test_root/release.canonical.json" "$test_root/release.json"

if TEST_GPG_EXIT=1 run_checker >/dev/null 2>&1; then
	echo "check-github-release test: invalid checksum signature passed" >&2
	exit 1
fi

cp "$dist/SHA256SUMS.asc" "$test_root/local-signature.canonical"
printf '%s\n' wrong-local-signature >"$dist/SHA256SUMS.asc"
if run_checker >/dev/null 2>&1; then
	echo "check-github-release test: mismatched local signature passed" >&2
	exit 1
fi
cp "$test_root/local-signature.canonical" "$dist/SHA256SUMS.asc"

printf '%s\n' unexpected >"$dist/unexpected.mcpb"
if run_checker >/dev/null 2>&1; then
	echo "check-github-release test: extra local MCPB passed" >&2
	exit 1
fi
rm "$dist/unexpected.mcpb"

if TEST_GPG_FINGERPRINT=0000000000000000000000000000000000000000 \
	run_checker >/dev/null 2>&1; then
	echo "check-github-release test: wrong imported signing identity passed" >&2
	exit 1
fi

# A valid signature from an unrelated key must fail even if that key would be
# trusted by an ambient operator keyring. This fixture uses real GnuPG: first
# prove the attacker signature is cryptographically valid in its own keyring,
# then prove the checker rejects it while using only Canary's pinned key.
attacker_home="$(mktemp -d /tmp/canary-wrong-signer-gpg.XXXXXX)"
chmod 0700 "$attacker_home"
gpg --homedir "$attacker_home" --batch --pinentry-mode loopback --passphrase '' \
	--quick-generate-key 'Wrong Valid Release Signer <wrong@example.invalid>' \
	ed25519 sign 0 >/dev/null 2>&1
gpg --homedir "$attacker_home" --batch --yes --armor --detach-sign \
	--output "$remote/SHA256SUMS.asc" "$remote/SHA256SUMS"
cp "$remote/SHA256SUMS.asc" "$dist/SHA256SUMS.asc"
gpg --homedir "$attacker_home" --batch \
	--verify "$remote/SHA256SUMS.asc" "$remote/SHA256SUMS" >/dev/null 2>&1
python3 - "$test_root/release.json" "$remote/SHA256SUMS.asc" <<'PY'
import hashlib
import json
import sys
from pathlib import Path

release_path = Path(sys.argv[1])
signature_path = Path(sys.argv[2])
document = json.loads(release_path.read_text(encoding="utf-8"))
for asset in document["assets"]:
    if asset["name"] == "SHA256SUMS.asc":
        asset["digest"] = "sha256:" + hashlib.sha256(signature_path.read_bytes()).hexdigest()
        break
else:
    raise SystemExit("fixture release lacks SHA256SUMS.asc")
release_path.write_text(json.dumps(document), encoding="utf-8")
PY
wrong_signer_log="$test_root/wrong-signer.log"
if run_checker_real_gpg >"$wrong_signer_log" 2>&1; then
	echo "check-github-release test: valid signature from wrong signer passed" >&2
	exit 1
fi
if ! grep -Fq \
	'check-github-release: remote checksum signature did not verify against the pinned Canary release identity' \
	"$wrong_signer_log"; then
	echo "check-github-release test: wrong-signer fixture failed before the pinned-identity check" >&2
	sed -n '1,20p' "$wrong_signer_log" >&2
	exit 1
fi

echo "check-github-release test: OK"
