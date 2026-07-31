#!/usr/bin/env bash

set -euo pipefail

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
script="$repo_root/scripts/registry-publish-with-login.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-registry-publish-test.XXXXXX")"
bin="$test_root/bin"
publisher="$test_root/mcp-publisher"

cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$bin"
printf '%s\n' 'verified mcpb bytes' >"$test_root/canary-v9.8.7.mcpb"
python3 - "$test_root" <<'PY'
import copy
import hashlib
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
version = "9.8.7"
digest = hashlib.sha256((root / f"canary-v{version}.mcpb").read_bytes()).hexdigest()
server = {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
    "name": "io.github.osauer/canary",
    "title": "Canary MCP",
    "description": "fixture",
    "version": version,
    "websiteUrl": "https://osauer.dev/canary/",
    "repository": {
        "url": "https://github.com/osauer/canary",
        "source": "github",
        "id": "1234071553",
    },
    "packages": [
        {
            "registryType": "mcpb",
            "identifier": (
                "https://github.com/osauer/canary/releases/download/"
                f"v{version}/canary-v{version}.mcpb"
            ),
            "fileSha256": digest,
            "transport": {"type": "stdio"},
        }
    ],
}

def write(name, payload):
    (root / name).write_text(json.dumps(payload), encoding="utf-8")

write("server.json", server)
write("exact.json", {"server": server, "_meta": {"fixture": True}})

absent = copy.deepcopy(server)
absent["version"] = "9.8.6"
write("absent.json", {"server": absent})
write("malformed.json", {"server": "409 already exists"})

mutations = {}
wrong_repository = copy.deepcopy(server)
wrong_repository["repository"]["id"] = "999"
mutations["wrong-repository"] = wrong_repository

extra_package = copy.deepcopy(server)
extra_package["packages"].append(copy.deepcopy(extra_package["packages"][0]))
mutations["extra-package"] = extra_package

wrong_type = copy.deepcopy(server)
wrong_type["packages"][0]["registryType"] = "npm"
mutations["wrong-type"] = wrong_type

wrong_transport = copy.deepcopy(server)
wrong_transport["packages"][0]["transport"] = {"type": "sse"}
mutations["wrong-transport"] = wrong_transport

wrong_identifier = copy.deepcopy(server)
wrong_identifier["packages"][0]["identifier"] = "https://evil.invalid/canary.mcpb"
mutations["wrong-identifier"] = wrong_identifier

wrong_digest = copy.deepcopy(server)
wrong_digest["packages"][0]["fileSha256"] = "0" * 64
mutations["wrong-digest"] = wrong_digest

for name, mutated in mutations.items():
    write(f"{name}.json", {"server": mutated})

bad_expected = copy.deepcopy(server)
bad_expected["packages"][0]["fileSha256"] = "f" * 64
write("bad-expected.json", bad_expected)
PY

cat >"$bin/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [ "${TEST_CURL_FAIL:-0}" = "1" ]; then
	exit 22
fi
printf '%s\n' "$*" >>"$TEST_CURL_LOG"
cat "$TEST_REGISTRY_PAYLOAD"
SH
cat >"$bin/sleep" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"$publisher" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$TEST_PUBLISHER_LOG"
case "$1" in
login)
	printf '%s\n' device-code-login
	exit 0
	;;
publish)
	count=0
	if [ -s "$TEST_PUBLISH_COUNT" ]; then
		count="$(cat "$TEST_PUBLISH_COUNT")"
	fi
	count=$((count + 1))
	printf '%s\n' "$count" >"$TEST_PUBLISH_COUNT"
	case "$TEST_PUBLISH_MODE" in
	success)
		printf '%s\n' published
		exit 0
		;;
	conflict)
		echo 'HTTP 409 conflict while writing unrelated audit record' >&2
		exit 23
		;;
	already_exists)
		echo 'unrelated package already exists' >&2
		exit 24
		;;
	auth_then_success)
		if [ "$count" -eq 1 ]; then
			echo 'JWT token expired; login required' >&2
			exit 41
		fi
		printf '%s\n' published-after-login
		exit 0
		;;
	*)
		exit 97
		;;
	esac
	;;
esac
exit 98
SH
chmod 0755 "$bin/curl" "$bin/sleep" "$publisher"

run_publish() {
	: >"$test_root/publisher.log"
	: >"$test_root/publish-count"
	: >"$test_root/curl.log"
	PATH="$bin:/opt/homebrew/bin:/usr/bin:/bin" \
		TEST_REGISTRY_PAYLOAD="$TEST_REGISTRY_PAYLOAD" \
		TEST_CURL_FAIL="${TEST_CURL_FAIL:-0}" \
		TEST_CURL_LOG="$test_root/curl.log" \
		TEST_PUBLISHER_LOG="$test_root/publisher.log" \
		TEST_PUBLISH_COUNT="$test_root/publish-count" \
		TEST_PUBLISH_MODE="$TEST_PUBLISH_MODE" \
		MCP_REGISTRY_AUTO_LOGIN="${TEST_AUTO_LOGIN:-1}" \
		MCP_REGISTRY_LOGIN_METHOD="${TEST_LOGIN_METHOD:-github}" \
		"$script" "$publisher" "${TEST_SERVER_JSON:-$test_root/server.json}"
}

TEST_PUBLISH_MODE=success TEST_REGISTRY_PAYLOAD="$test_root/exact.json" \
	run_publish >"$test_root/success.out" 2>&1
grep -Fq 'registry verified exact io.github.osauer/canary@9.8.7' "$test_root/success.out"
grep -Fq \
	'https://registry.modelcontextprotocol.io/v0.1/servers/io.github.osauer%2Fcanary/versions/9.8.7' \
	"$test_root/curl.log"

for mutation in wrong-repository extra-package wrong-type wrong-transport wrong-identifier wrong-digest; do
	if TEST_PUBLISH_MODE=success TEST_REGISTRY_PAYLOAD="$test_root/$mutation.json" \
		run_publish >"$test_root/$mutation.out" 2>&1; then
		echo "registry-publish-with-login test: $mutation registry metadata passed" >&2
		exit 1
	fi
done

if TEST_PUBLISH_MODE=success TEST_REGISTRY_PAYLOAD="$test_root/exact.json" \
	TEST_SERVER_JSON="$test_root/bad-expected.json" \
	run_publish >"$test_root/bad-expected.out" 2>&1; then
	echo "registry-publish-with-login test: expected digest not backed by the local MCPB passed" >&2
	exit 1
fi
test ! -s "$test_root/publisher.log"

if TEST_PUBLISH_MODE=conflict TEST_REGISTRY_PAYLOAD="$test_root/absent.json" \
	TEST_AUTO_LOGIN=0 run_publish >"$test_root/unrelated-409.out" 2>&1; then
	echo "registry-publish-with-login test: unrelated 409/conflict passed without typed registry proof" >&2
	exit 1
fi

if TEST_PUBLISH_MODE=already_exists TEST_REGISTRY_PAYLOAD="$test_root/absent.json" \
	TEST_AUTO_LOGIN=0 run_publish >"$test_root/unrelated-already-exists.out" 2>&1; then
	echo "registry-publish-with-login test: unrelated already-exists prose passed without typed registry proof" >&2
	exit 1
fi

TEST_PUBLISH_MODE=conflict TEST_REGISTRY_PAYLOAD="$test_root/exact.json" \
	TEST_AUTO_LOGIN=0 run_publish >"$test_root/typed-conflict.out" 2>&1
grep -Fq 'requiring typed registry proof' "$test_root/typed-conflict.out"

if TEST_PUBLISH_MODE=success TEST_REGISTRY_PAYLOAD="$test_root/absent.json" \
	run_publish >"$test_root/post-publish-absent.out" 2>&1; then
	echo "registry-publish-with-login test: zero-exit publish passed while exact registry entry was absent" >&2
	exit 1
fi

if TEST_PUBLISH_MODE=success TEST_REGISTRY_PAYLOAD="$test_root/exact.json" \
	TEST_CURL_FAIL=1 run_publish >"$test_root/unavailable.out" 2>&1; then
	echo "registry-publish-with-login test: unavailable registry evidence passed" >&2
	exit 1
fi

if TEST_PUBLISH_MODE=success TEST_REGISTRY_PAYLOAD="$test_root/malformed.json" \
	run_publish >"$test_root/malformed.out" 2>&1; then
	echo "registry-publish-with-login test: malformed registry evidence passed" >&2
	exit 1
fi

TEST_PUBLISH_MODE=auth_then_success TEST_REGISTRY_PAYLOAD="$test_root/exact.json" \
	run_publish >"$test_root/login.out" 2>&1
grep -Fxq 'login github' "$test_root/publisher.log"
test "$(grep -c '^publish ' "$test_root/publisher.log")" -eq 2
grep -Fq 'published-after-login' "$test_root/login.out"

echo "registry-publish-with-login_test: OK"
