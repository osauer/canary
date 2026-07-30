#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-boundary.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-boundary-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

# Fixture makes run with a scrubbed environment and cwd pinned inside the
# fixture dir. Inside the release pipeline, MAKEFLAGS carries MAKELEVEL and
# RELEASE_PIPELINE_ENTRY=release into every descendant make — on 2026-07-29
# that armed the fixture guards and the fixture's then-real `gh release
# create` executed against the repository and published a bogus release.
# Publication recipes below are therefore inert (`touch guard-leaked &&
# false`, command text kept as a trailing comment for the checker's scan),
# so even a broken guard can only write a marker and fail.
fixture_make() {
	env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL RELEASE_PIPELINE_ENTRY= \
		make -s -C "$test_root" -f Makefile "$@"
}

mkdir -p "$test_root/scripts" "$test_root/.github/workflows"
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	@touch guard-leaked && false # gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	@touch guard-leaked && false # git tag -a v1.2.3 -m fixture
	@touch guard-leaked && false # git push origin v1.2.3
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) -C . _release-run RELEASE_PIPELINE_ENTRY=release
_release-resume-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release-resume" ]; then exit 1; fi
	@touch guard-leaked && false # claude plugin tag . --push
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release-resume:
	$(MAKE) -C . _release-resume-run RELEASE_PIPELINE_ENTRY=release-resume
EOF
cat > "$test_root/scripts/package.sh" <<'EOF'
#!/bin/sh
printf '%s\n' package
EOF
chmod 0755 "$test_root/scripts/package.sh"

"$checker" "$test_root" >/dev/null

rm -f "$test_root/guard-leaked"
if fixture_make _release-publish >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal publication helper invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: _release-publish guard leaked; publication recipe executed" >&2
	exit 1
fi
if fixture_make _release-run >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal pipeline body invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: _release-run guard leaked; publication recipe executed" >&2
	exit 1
fi
if fixture_make _release-resume-run >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal resume body invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: _release-resume-run guard leaked; publication recipe executed" >&2
	exit 1
fi

# Regression for the 2026-07-29 incident: under a pipeline-shaped
# environment the guards legitimately pass, so the inert recipes are the
# last line of defense — the invocation must still fail rather than publish.
if MAKEFLAGS="RELEASE_PIPELINE_ENTRY=release" MAKELEVEL=4 RELEASE_PIPELINE_ENTRY=release \
	make -s -C "$test_root" -f Makefile _release-publish >/dev/null 2>&1; then
	echo "check-release-boundary test: pipeline-shaped env let the publication recipe succeed" >&2
	exit 1
fi
rm -f "$test_root/guard-leaked"

cat > "$test_root/scripts/rogue.sh" <<'EOF'
#!/bin/sh
git push origin v1.2.3
EOF
chmod 0755 "$test_root/scripts/rogue.sh"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: rogue script publication path passed" >&2
	exit 1
fi
rm "$test_root/scripts/rogue.sh"

cat >> "$test_root/Makefile" <<'EOF'
release-helper:
	gh release create v1.2.3
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: rogue Makefile publication target passed" >&2
	exit 1
fi

cat > "$test_root/Makefile" <<'EOF'
.PHONY: release release-publish
release-publish:
	gh release create v1.2.3
release:
	$(MAKE) release-publish
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: public release-publish authority passed" >&2
	exit 1
fi

cat > "$test_root/Makefile" <<'EOF'
_release-run:
	git tag -a v1.2.3 -m fixture
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: unguarded internal pipeline body passed" >&2
	exit 1
fi

# The pre-worktree shape — publication commands directly in `release` —
# must now fail: authority lives in the guarded worktree body.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
release:
	git tag -a v1.2.3 -m fixture
	git push origin v1.2.3
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: publication commands in release target passed" >&2
	exit 1
fi

echo "check-release-boundary test: OK"
