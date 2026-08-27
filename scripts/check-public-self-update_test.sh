#!/usr/bin/env bash

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
checker="$repo_root/scripts/check-public-self-update.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-public-self-update-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

fixture="$test_root/fixture"
fake_bin="$test_root/fake-bin"
archive_root="$test_root/archive/canary-v1.2.3-darwin-arm64"
mkdir -p "$fixture" "$fake_bin" "$archive_root"

cat >"$fixture/releases.json" <<'JSON'
[
  {"tag_name":"v1.2.4","draft":false,"prerelease":false},
  {"tag_name":"v1.2.3","draft":false,"prerelease":false},
  {"tag_name":"v1.2.2-rc.1","draft":false,"prerelease":true},
  {"tag_name":"v2.0.0","draft":false,"prerelease":false}
]
JSON

cat >"$archive_root/canary" <<'OLD'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
version)
	[ "${2:-}" = "--json" ] || exit 2
	cat <<'JSON'
{"program":"canary","version":"v1.2.3","commit":"1111111111111111111111111111111111111111","goos":"darwin","goarch":"arm64"}
JSON
	;;
update)
	[ "${2:-}" = "--no-restart" ] && [ "$#" -eq 2 ] || exit 2
	case "$HOME" in */home) ;; *) exit 31 ;; esac
	case "$TMPDIR" in */tmp) ;; *) exit 32 ;; esac
	case "$CANARY_INSTALL_DIR" in */install) ;; *) exit 33 ;; esac
	case "$CANARY_SOCKET" in */runtime/canary.sock) ;; *) exit 34 ;; esac
	case "$XDG_CACHE_HOME" in */cache) ;; *) exit 35 ;; esac
	case "$XDG_CONFIG_HOME" in */config) ;; *) exit 36 ;; esac
	case "$XDG_STATE_HOME" in */state) ;; *) exit 37 ;; esac
	case "$XDG_RUNTIME_DIR" in */runtime) ;; *) exit 38 ;; esac
	mkdir -p "$CANARY_INSTALL_DIR"
	cat >"$CANARY_INSTALL_DIR/canary" <<'TARGET'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = version ] && [ "${2:-}" = --json ]; then
	cat <<'JSON'
{"program":"canary","version":"v1.2.4","commit":"2222222222222222222222222222222222222222","goos":"darwin","goarch":"arm64"}
JSON
	exit 0
fi
exit 2
TARGET
	chmod 0755 "$CANARY_INSTALL_DIR/canary"
	printf '%s\n' 'fixture update installed v1.2.4 without restart'
	;;
*) exit 2 ;;
esac
OLD
chmod 0755 "$archive_root/canary"
(
	cd "$test_root/archive"
	tar -czf "$fixture/canary-v1.2.3-darwin-arm64.tar.gz" \
		canary-v1.2.3-darwin-arm64
)
archive_digest="$(shasum -a 256 "$fixture/canary-v1.2.3-darwin-arm64.tar.gz" | awk '{print $1}')"
printf '%s  %s\n' "$archive_digest" canary-v1.2.3-darwin-arm64.tar.gz \
	>"$fixture/SHA256SUMS"
printf '%s\n' fixture-signature >"$fixture/SHA256SUMS.ed25519"

cat >"$fake_bin/gh" <<'GH'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
api)
	cat "$PUBLIC_SELF_UPDATE_FIXTURE/releases.json"
	;;
release)
	[ "${2:-}" = download ] || exit 2
	shift 3
	destination=""
	patterns=()
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--repo) shift 2 ;;
		--dir) destination="$2"; shift 2 ;;
		--pattern) patterns+=("$2"); shift 2 ;;
		*) exit 2 ;;
		esac
	done
	[ -n "$destination" ] || exit 2
	for pattern in "${patterns[@]}"; do
		cp "$PUBLIC_SELF_UPDATE_FIXTURE/$pattern" "$destination/$pattern"
	done
	;;
*) exit 2 ;;
esac
GH

cat >"$fake_bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = -C ]; then
	shift 2
fi
[ "${1:-}" = rev-parse ] && [ "${2:-}" = --verify ] || exit 2
printf '%s\n' 2222222222222222222222222222222222222222
GIT

cat >"$fake_bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
env)
	case "${2:-}" in
	GOOS) printf '%s\n' darwin ;;
	GOARCH) printf '%s\n' arm64 ;;
	*) exit 2 ;;
	esac
	;;
run)
	exit 0
	;;
version)
	[ "${2:-}" = -m ] || exit 2
	printf '%s\n' "${3:-}" 'build\t-trimpath=true'
	;;
*) exit 2 ;;
esac
GO
chmod 0755 "$fake_bin/gh" "$fake_bin/git" "$fake_bin/go"

run_witness() {
	PATH="$fake_bin:/usr/bin:/bin" \
	PUBLIC_SELF_UPDATE_FIXTURE="$fixture" \
		"$checker" v1.2.4
}

positive_output="$(run_witness)"
if [ "$positive_output" != "check-public-self-update: OK previous=v1.2.3 target=v1.2.4 host=darwin/arm64 variant=standard restart=no" ]; then
	echo "check-public-self-update test: positive witness reported unexpected evidence" >&2
	printf '%s\n' "$positive_output" >&2
	exit 1
fi

printf '%064d  %s\n' 0 canary-v1.2.3-darwin-arm64.tar.gz >"$fixture/SHA256SUMS"
if run_witness >"$test_root/tampered-output" 2>&1; then
	echo "check-public-self-update test: tampered prior archive passed" >&2
	exit 1
fi
if ! grep -q 'checksum mismatch' "$test_root/tampered-output"; then
	echo "check-public-self-update test: tampered prior archive reported the wrong failure" >&2
	cat "$test_root/tampered-output" >&2
	exit 1
fi

echo "check-public-self-update test: OK"
