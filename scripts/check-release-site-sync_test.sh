#!/usr/bin/env bash
# Regression fixture for check-release-site-sync.sh.
#
# v2.3.1 shipped with all three MCP discovery JSONs still stamped 2.3.0: the
# stamp loop sat *after* the patch-release early return, so no patch cut ever
# reached it. These cases pin the split — discovery stamps gate on every
# release, the static-site push stays non-patch-only.

set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/check-release-site-sync.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-site-sync-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

fail() {
	echo "check-release-site-sync test: $1" >&2
	exit 1
}

# Minimal tree carrying only what the per-release stamp checks read. Each file
# gets its own version argument so a single stale copy can be simulated. The
# issue template takes the v-prefixed form because that is what a reporter
# sees in the field.
seed_docs() {
	root="$1"
	canonical="$2"
	wellknown="$3"
	card="$4"
	template="$5"
	mkdir -p "$root/docs/.well-known/mcp" "$root/.github/ISSUE_TEMPLATE"
	printf '{\n  "name": "ibkr",\n  "version": "%s"\n}\n' "$canonical" \
		> "$root/docs/mcp-server.json"
	printf '{\n  "name": "ibkr",\n  "version": "%s"\n}\n' "$wellknown" \
		> "$root/docs/.well-known/mcp/server.json"
	printf '{\n  "serverInfo": {\n    "version": "%s"\n  }\n}\n' "$card" \
		> "$root/docs/.well-known/mcp/server-card.json"
	printf '      placeholder: "%s"\n' "$template" \
		> "$root/.github/ISSUE_TEMPLATE/bug_report.yml"
}

# A patch release whose discovery stamps all match passes without needing any
# git state — the early return fires after the stamp loop, not before it.
ok="$test_root/ok"
mkdir -p "$ok"
seed_docs "$ok" 2.3.1 2.3.1 2.3.1 v2.3.1
(cd "$ok" && "$script" v2.3.1 >/dev/null) \
	|| fail "a patch release with matching discovery stamps should pass"

# The v2.3.1 regression itself, once per file so the loop cannot silently
# narrow to the canonical copy and let a generated one drift.
for stale_file in canonical wellknown card template; do
	case "$stale_file" in
		canonical) versions="2.3.0 2.3.1 2.3.1 v2.3.1" ;;
		wellknown) versions="2.3.1 2.3.0 2.3.1 v2.3.1" ;;
		card) versions="2.3.1 2.3.1 2.3.0 v2.3.1" ;;
		template) versions="2.3.1 2.3.1 2.3.1 v2.3.0" ;;
	esac
	# shellcheck disable=SC2086 # deliberate split into three positional args
	stale="$test_root/stale-$stale_file"
	mkdir -p "$stale"
	seed_docs "$stale" $versions
	if (cd "$stale" && "$script" v2.3.1 >/dev/null 2>&1); then
		fail "a patch release passed with a stale $stale_file discovery stamp"
	fi
	# The hint has to fix the file it names. A shared "run make docs-regen"
	# survived several cuts here because only pass/fail was ever asserted:
	# the card's serverInfo.version is written by no generator, so following
	# it left the gate failing with the identical message.
	(cd "$stale" && "$script" v2.3.1 >/dev/null 2>"$test_root/hint-$stale_file") || true
	case "$stale_file" in
		card)
			grep -q 'serverInfo' "$test_root/hint-$stale_file" \
				|| fail "a stale card hint does not name serverInfo"
			if grep -q 'docs-regen' "$test_root/hint-$stale_file"; then
				fail "a stale card hint still points at a generator that never writes it"
			fi
			;;
		wellknown)
			grep -q 'docs-regen' "$test_root/hint-$stale_file" \
				|| fail "a stale generated copy should be fixed by docs-regen"
			;;
		canonical)
			grep -q 'canonical' "$test_root/hint-$stale_file" \
				|| fail "a stale canonical stamp should say it is the canonical one"
			;;
		template)
			grep -q 'placeholder' "$test_root/hint-$stale_file" \
				|| fail "a stale issue-template hint does not name the placeholder"
			if grep -q 'docs-regen' "$test_root/hint-$stale_file"; then
				fail "a stale placeholder hint points at a generator that never writes it"
			fi
			;;
	esac
done

# The issue template ships on every release, so a stale placeholder must fail
# a non-patch cut too — not just the patch path that returns early above.
stale_minor="$test_root/stale-minor-template"
mkdir -p "$stale_minor"
seed_docs "$stale_minor" 2.4.0 2.4.0 2.4.0 v2.3.1
if (cd "$stale_minor" && "$script" v2.4.0 >/dev/null 2>&1); then
	fail "a minor release passed with a stale issue-template placeholder"
fi

# A missing docs/ tree is a hard error, not a silent skip.
bare="$test_root/bare"
mkdir -p "$bare"
if (cd "$bare" && "$script" v2.3.1 >/dev/null 2>&1); then
	fail "a missing docs/ tree should not pass"
fi

# Version-shape validation still rejects junk before touching the tree.
if (cd "$ok" && "$script" 2.3.1 >/dev/null 2>&1); then
	fail "a version without the v prefix should be rejected"
fi

echo "check-release-site-sync test: all cases passed"
