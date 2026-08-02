#!/usr/bin/env bash

# Table-driven test for check-changelog-issue-refs.sh. Each case builds a real
# tagged repository rather than simulating git output, so the fixture stays
# honest if git's formatting shifts.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-changelog-issue-refs.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/canary-issue-refs-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT HUP INT TERM

fails=0
git_q() { git -c user.email=t@example.com -c user.name=t -C "$1" "${@:2}"; }

# Builds a repo tagged v1.0.0, then one commit carrying $subject, then a
# changelog whose topmost entry body is $entry.
seed() {
	local dir="$1" subject="$2" entry="$3"
	rm -rf "$dir"
	mkdir -p "$dir"
	git_q "$dir" init -q
	echo base > "$dir/f.txt"
	git_q "$dir" add f.txt
	git_q "$dir" commit -q -m "base"
	git_q "$dir" tag v1.0.0
	echo next > "$dir/f.txt"
	git_q "$dir" commit -q -am "$subject"
	printf '# Changelog\n\n## v1.1.0 — 2026-08-02\n\n### Fixed\n\n- %s\n\n## v1.0.0 — 2026-08-01\n\n- older (#99)\n' \
		"$entry" > "$dir/CHANGELOG.md"
}

run_case() {
	local name="$1" want="$2" subject="$3" entry="$4"
	local dir="$work/$name" got=0
	seed "$dir" "$subject" "$entry"
	# A non-zero exit is the expected result in most rows, so it must be
	# captured rather than allowed to trip `set -e`.
	(cd "$dir" && RELEASE_VERSION=v1.1.0 CHANGELOG_PATH=CHANGELOG.md "$checker" >"$work/$name.out" 2>&1) || got=$?
	if [ "$got" != "$want" ]; then
		echo "FAIL $name: exit $got, want $want" >&2
		sed 's/^/  /' "$work/$name.out" >&2
		fails=$((fails + 1))
		return
	fi
	echo "ok   $name"
}

# A closing reference that the entry names is the passing shape.
run_case named 0 'fix: thing

Fixes #12' 'thing no longer breaks (#12)'

# The defect this gate exists for: the fix closes the issue on push, so the
# issue reads "closed" while nothing tells users which release carries it.
run_case unnamed 1 'fix: thing

Fixes #12' 'thing no longer breaks'

# GitHub closes on several spellings and is case-insensitive.
run_case closes-keyword 1 'fix: thing

Closes #34' 'thing no longer breaks'
run_case resolved-keyword 1 'fix: thing

resolved #56' 'thing no longer breaks'

# A bare mention is a cross-reference, not a claim this release fixes it, so it
# must not force a changelog entry.
run_case bare-mention 0 'fix: thing

context, see #78' 'thing no longer breaks'

# Substring safety: #1 named must not satisfy a requirement for #12, and #123
# in the entry must not satisfy #12 either.
run_case no-prefix-match 1 'fix: thing

Fixes #12' 'thing no longer breaks (#1)'
run_case no-superstring-match 1 'fix: thing

Fixes #12' 'thing no longer breaks (#123)'

# Naming it under an older heading is not naming it in this release.
older_entry_dir="$work/older-heading"
seed "$older_entry_dir" 'fix: thing

Fixes #12' 'thing no longer breaks'
printf '# Changelog\n\n## v1.1.0 — 2026-08-02\n\n- unrelated\n\n## v1.0.0 — 2026-08-01\n\n- older (#12)\n' \
	> "$older_entry_dir/CHANGELOG.md"
if (cd "$older_entry_dir" && RELEASE_VERSION=v1.1.0 CHANGELOG_PATH=CHANGELOG.md "$checker" >/dev/null 2>&1); then
	echo "FAIL older-heading: an older entry satisfied this release" >&2
	fails=$((fails + 1))
else
	echo "ok   older-heading"
fi

# The repository carries a separately named plugin tag alongside each release
# tag. A newer plugin tag on a later commit must not become the range start, or
# commits between the release tag and it drop out of the check unnoticed.
# The closing reference has to sit BETWEEN the release tag and the plugin tag,
# which is the only arrangement where an unfiltered describe hides it.
plugin="$work/plugin-tag"
seed "$plugin" 'fix: thing

Fixes #12' 'thing no longer breaks'
git_q "$plugin" tag canary--v1.0.1
echo more > "$plugin/f.txt"
git_q "$plugin" commit -q -am 'chore: unrelated'
if (cd "$plugin" && RELEASE_VERSION=v1.1.0 CHANGELOG_PATH=CHANGELOG.md "$checker" >/dev/null 2>&1); then
	echo "FAIL plugin-tag: a plugin tag shifted the range and hid a closing reference" >&2
	fails=$((fails + 1))
else
	echo "ok   plugin-tag"
fi

# An untagged repository has no range and must not block a first release.
first="$work/first-release"
rm -rf "$first"; mkdir -p "$first"
git_q "$first" init -q
echo base > "$first/f.txt"
git_q "$first" add f.txt
git_q "$first" commit -q -m 'fix: thing

Fixes #12'
printf '# Changelog\n\n## v1.0.0 — 2026-08-02\n\n- first\n' > "$first/CHANGELOG.md"
if (cd "$first" && RELEASE_VERSION=v1.0.0 CHANGELOG_PATH=CHANGELOG.md "$checker" >/dev/null 2>&1); then
	echo "ok   first-release"
else
	echo "FAIL first-release: an untagged repo should not block" >&2
	fails=$((fails + 1))
fi

if [ "$fails" -gt 0 ]; then
	echo "$fails changelog-issue-refs case(s) failed" >&2
	exit 1
fi
echo "check-changelog-issue-refs test: OK"
