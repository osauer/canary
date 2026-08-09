#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-installer-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

fixture="$test_root/fixture"
fake_bin="$test_root/fake-bin"
mkdir -p "$fixture" "$fake_bin"

make_fixture_release() {
	local version="$1" product="$2" reported_name="$3"
	local base="${product}-${version}-darwin-arm64"
	local release_dir="$fixture/$version"
	mkdir -p "$release_dir/$base"
	cat > "$release_dir/$base/$product" <<EOF
#!/bin/sh
case "\${1:-}" in
	version) printf '%s\n' '$reported_name $version fixture' ;;
	*) printf '%s\n' '$product fixture' ;;
esac
EOF
	chmod 0755 "$release_dir/$base/$product"
	printf '%s\n' 'MIT fixture' > "$release_dir/$base/LICENSE"
	printf '%s\n' '# Canary fixture' > "$release_dir/$base/README.md"
	(
		cd "$release_dir"
		tar -czf "$base.tar.gz" "$base"
		shasum -a 256 "$base.tar.gz" > SHA256SUMS
	)
	printf '%s\n' 'fixture signature' > "$release_dir/SHA256SUMS.asc"
}

make_fixture_release v9.9.9 canary "Canary CLI "
make_fixture_release v2.3.1 ibkr "ibkr"
# Deliberately only a retired-name archive. The installer must not fall back
# to it for any version except the exact v2.3.1 publication bootstrap.
make_fixture_release v2.3.2 ibkr "ibkr"
printf '%s\n' 'fixture release key' > "$fixture/release-signing-key.asc"

cat > "$fake_bin/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in
	-s) printf '%s\n' Darwin ;;
	-m) printf '%s\n' arm64 ;;
	*) exit 2 ;;
esac
EOF

cat > "$fake_bin/gpg" <<'EOF'
#!/bin/sh
set -eu
case "$*" in
	*--with-colons*--fingerprint*)
		printf '%s\n' 'fpr:::::::::D98426D48FED85EFA33904694D922A4F922B7D7D:'
		;;
	*--import*)
		cat >/dev/null
		;;
	*--verify*) ;;
	*) exit 2 ;;
esac
EOF

cat > "$fake_bin/curl" <<'EOF'
#!/bin/sh
set -eu
out=""
write_effective=0
url=""
while [ "$#" -gt 0 ]; do
	case "$1" in
		-o)
			out="$2"
			shift 2
			;;
		-w)
			write_effective=1
			shift 2
			;;
		-*) shift ;;
		*)
			url="$1"
			shift
			;;
	esac
done
[ -n "$url" ] || exit 2
case "$url" in
	https://github.com/osauer/canary/releases/latest)
		[ "$write_effective" = "1" ] || exit 2
		printf 'https://github.com/osauer/canary/releases/tag/%s' "$CANARY_INSTALL_FIXTURE_VERSION"
		exit 0
		;;
	https://github.com/osauer/canary/releases/download/*/*)
		asset="${url##*/}"
		rest="${url#*/releases/download/}"
		version="${rest%%/*}"
		source="$CANARY_INSTALL_FIXTURE/$version/$asset"
		;;
	https://raw.githubusercontent.com/osauer/canary/*/internal/update/release-signing-key.asc)
		source="$CANARY_INSTALL_FIXTURE/release-signing-key.asc"
		;;
	*)
		printf '%s\n' "unexpected URL: $url" >&2
		exit 2
		;;
esac
[ -f "$source" ] || exit 22
if [ -n "$out" ]; then
	cp "$source" "$out"
else
	cat "$source"
fi
EOF

# Fault injection stays entirely in the fixture. Production install.sh sees an
# ordinary mv failure or TERM and carries no test-only control surface.
cat > "$fake_bin/mv" <<'EOF'
#!/bin/bash
set -euo pipefail
args=("$@")
if [[ "${args[0]:-}" == "-f" ]]; then
	args=("${args[@]:1}")
fi
[[ "${#args[@]}" -eq 2 ]] || exec /bin/mv "$@"
source_path="${args[0]}"
dest_path="${args[1]}"
case "${CANARY_INSTALL_FIXTURE_MV_MODE:-}" in
	fail_canonical)
		if [[ "$source_path" == */.canary-install.* && "$dest_path" == "$CANARY_INSTALL_FIXTURE_TARGET/canary" ]]; then
			exit 1
		fi
		;;
	signal_after_canonical)
		if [[ "$source_path" == */.canary-install.* && "$dest_path" == "$CANARY_INSTALL_FIXTURE_TARGET/canary" ]]; then
			/bin/mv "$@"
			kill -TERM "$PPID"
			exit 0
		fi
		;;
esac
exec /bin/mv "$@"
EOF
chmod 0755 "$fake_bin/uname" "$fake_bin/gpg" "$fake_bin/curl" "$fake_bin/mv"

run_installer() {
	local version="$1"
	shift
	PATH="${CANARY_INSTALL_TEST_PATH_PREFIX:-}$fake_bin:/usr/bin:/bin" \
	CANARY_INSTALL_FIXTURE="$fixture" \
	CANARY_INSTALL_FIXTURE_VERSION="$version" \
	HOME="$test_root/home" \
	SHELL=/bin/sh \
	"$@" sh "$repo_root/install.sh" >/dev/null
}

run_installer_capture() {
	local version="$1" output="$2"
	shift 2
	PATH="${CANARY_INSTALL_TEST_PATH_PREFIX:-}$fake_bin:/usr/bin:/bin" \
	CANARY_INSTALL_FIXTURE="$fixture" \
	CANARY_INSTALL_FIXTURE_VERSION="$version" \
	HOME="$test_root/home" \
	SHELL=/bin/sh \
	"$@" sh "$repo_root/install.sh" >"$output"
}

assert_no_old_name() {
	local dir="$1"
	if [ -e "$dir/ibkr" ] || [ -L "$dir/ibkr" ]; then
		echo "install test: retired executable path remains in $dir" >&2
		exit 1
	fi
}

# A fresh canonical release installs exactly one public executable.
fresh="$test_root/fresh/bin"
run_installer v9.9.9 env CANARY_INSTALL_DIR="$fresh"
[ -x "$fresh/canary" ] || {
	echo "install test: fresh install missing canonical executable" >&2
	exit 1
}
assert_no_old_name "$fresh"
[ ! -e "$fresh/canary.bak" ] || {
	echo "install test: fresh install unexpectedly created a rollback backup" >&2
	exit 1
}

# A pre-upgrade executable is transactionally retired and its public path is
pre_upgrade="$test_root/pre-upgrade/bin"
mkdir -p "$pre_upgrade"
printf '%s\n' 'old executable' > "$pre_upgrade/ibkr"
chmod 0755 "$pre_upgrade/ibkr"
cp "$pre_upgrade/ibkr" "$test_root/expected-pre-upgrade-backup"
CANARY_INSTALL_TEST_PATH_PREFIX="$pre_upgrade:" \
	run_installer v9.9.9 env CANARY_INSTALL_DIR="$pre_upgrade"
assert_no_old_name "$pre_upgrade"
[ ! -e "$pre_upgrade/canary.bak" ] && [ ! -e "$pre_upgrade/ibkr.bak" ] || {
	echo "install test: pre-upgrade migration retained a durable rollback executable" >&2
	exit 1
}

# An old custom installation that remains on PATH must not survive beside a
# outside its selected transaction root, so it fails with the exact safe
custom_legacy="$test_root/custom-legacy/bin"
split_target="$test_root/split-target/bin"
split_output="$test_root/split-target.err"
mkdir -p "$custom_legacy"
printf '%s\n' 'old custom executable' > "$custom_legacy/ibkr"
chmod 0755 "$custom_legacy/ibkr"
custom_legacy_physical=$(cd "$custom_legacy" && pwd -P)
if CANARY_INSTALL_TEST_PATH_PREFIX="$custom_legacy:" \
	run_installer v9.9.9 env CANARY_INSTALL_DIR="$split_target" 2>"$split_output"; then
	echo "install test: custom legacy executable allowed a split installation" >&2
	exit 1
fi
grep -Fq "CANARY_INSTALL_DIR=$custom_legacy_physical" "$split_output" || {
	echo "install test: custom legacy refusal omitted the safe target directory" >&2
	exit 1
}
[ -x "$custom_legacy/ibkr" ] || {
	echo "install test: custom legacy refusal mutated the old executable" >&2
	exit 1
}
[ ! -e "$split_target/canary" ] || {
	echo "install test: custom legacy refusal published a second executable" >&2
	exit 1
}
unset CANARY_INSTALL_TEST_PATH_PREFIX

# Relative PATH entries must resolve to the same physical comparison. Otherwise
# a caller launched from beside an old custom installation could still publish
relative_legacy="$test_root/relative-legacy/bin"
relative_target="$test_root/relative-target/bin"
relative_output="$test_root/relative-target.err"
mkdir -p "$relative_legacy"
printf '%s\n' 'old relative executable' > "$relative_legacy/ibkr"
chmod 0755 "$relative_legacy/ibkr"
relative_legacy_physical=$(cd "$relative_legacy" && pwd -P)
if (
	cd "$test_root"
	CANARY_INSTALL_TEST_PATH_PREFIX="relative-legacy/bin:" \
		run_installer v9.9.9 env CANARY_INSTALL_DIR="$relative_target"
) 2>"$relative_output"; then
	echo "install test: relative legacy PATH allowed a split installation" >&2
	exit 1
fi
grep -Fq "CANARY_INSTALL_DIR=$relative_legacy_physical" "$relative_output" || {
	echo "install test: relative legacy refusal omitted the resolved safe target directory" >&2
	exit 1
}
[ -x "$relative_legacy/ibkr" ] || {
	echo "install test: relative legacy refusal mutated the old executable" >&2
	exit 1
}
[ ! -e "$relative_target/canary" ] || {
	echo "install test: relative legacy refusal published a second executable" >&2
	exit 1
}

# The one-time product-rename bridge tells an existing installation to restart
# before using daemon-backed commands, so state migration happens under the
bridge_hint="$test_root/bridge-hint/bin"
bridge_output="$test_root/bridge-hint.out"
mkdir -p "$bridge_hint"
printf '%s\n' 'old executable' > "$bridge_hint/ibkr"
chmod 0755 "$bridge_hint/ibkr"
run_installer_capture v9.9.9 "$bridge_output" env CANARY_INSTALL_DIR="$bridge_hint"
grep -Fq 'canary restart' "$bridge_output" || {
	echo "install test: product-rename bridge omitted the restart step" >&2
	exit 1
}
grep -Fq 'migrates supported existing state before broker connection' "$bridge_output" || {
	echo "install test: product-rename bridge omitted the state-migration explanation" >&2
	exit 1
}

# The retired environment variable is rejected even when present but empty;
retired_env_target="$test_root/retired-env/bin"
if run_installer v9.9.9 env CANARY_INSTALL_DIR="$retired_env_target" IBKR_INSTALL_DIR= 2>/dev/null; then
	echo "install test: retired install-directory variable was accepted" >&2
	exit 1
fi
[ ! -e "$retired_env_target/canary" ] || {
	echo "install test: retired environment variable still published an executable" >&2
	exit 1
}

# An ordinary canonical update also leaves no durable rollback executable.
canonical_upgrade="$test_root/canonical-upgrade/bin"
mkdir -p "$canonical_upgrade"
printf '%s\n' 'old canary' > "$canonical_upgrade/canary"
chmod 0755 "$canonical_upgrade/canary"
cp "$canonical_upgrade/canary" "$test_root/expected-canonical-backup"
run_installer v9.9.9 env CANARY_INSTALL_DIR="$canonical_upgrade"
[ ! -e "$canonical_upgrade/canary.bak" ] && [ ! -e "$canonical_upgrade/ibkr.bak" ] || {
	echo "install test: canonical update retained a durable rollback executable" >&2
	exit 1
}
assert_no_old_name "$canonical_upgrade"

# Public executable paths fail closed when they are symlinks or special files.
non_regular="$test_root/non-regular/bin"
mkdir -p "$non_regular"
ln -s elsewhere "$non_regular/ibkr"
if run_installer v9.9.9 env CANARY_INSTALL_DIR="$non_regular" 2>/dev/null; then
	echo "install test: pre-upgrade symlink was accepted" >&2
	exit 1
fi
[ ! -e "$non_regular/canary" ] || {
	echo "install test: non-regular path rejection still published Canary" >&2
	exit 1
}

# Exact publication bootstrap: v2.3.1 may be read from its immutable old
# archive shape, but is still installed only at the canonical command path.
bootstrap="$test_root/bootstrap/bin"
run_installer v2.3.1 env CANARY_INSTALL_DIR="$bootstrap"
[ -x "$bootstrap/canary" ] || {
	echo "install test: v2.3.1 bootstrap did not install canonical executable" >&2
	exit 1
}
assert_no_old_name "$bootstrap"

# The exception is version-bounded. A later release with only an old-name
# archive must fail rather than silently falling back.
no_fallback="$test_root/no-fallback/bin"
if run_installer v2.3.2 env CANARY_INSTALL_DIR="$no_fallback" 2>/dev/null; then
	echo "install test: installer fell back to a retired asset after v2.3.1" >&2
	exit 1
fi
[ ! -e "$no_fallback/canary" ] || {
	echo "install test: failed no-fallback install published an executable" >&2
	exit 1
}

# Publication failure and interruption both restore the exact pre-upgrade
for mode in fail_canonical signal_after_canonical; do
	target="$test_root/$mode/bin"
	mkdir -p "$target"
	cp "$test_root/expected-pre-upgrade-backup" "$target/ibkr"
	if run_installer v9.9.9 env \
		CANARY_INSTALL_DIR="$target" \
		CANARY_INSTALL_FIXTURE_MV_MODE="$mode" \
		CANARY_INSTALL_FIXTURE_TARGET="$target"; then
		echo "install test: injected $mode unexpectedly passed" >&2
		exit 1
	fi
	[ ! -e "$target/canary" ] && [ ! -L "$target/canary" ] || {
		echo "install test: $mode left a canonical executable after rollback" >&2
		exit 1
	}
	cmp -s "$target/ibkr" "$test_root/expected-pre-upgrade-backup" || {
		echo "install test: $mode did not restore the pre-upgrade executable" >&2
		exit 1
	}
done

echo "install test: OK"
