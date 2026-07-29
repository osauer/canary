#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-boundary.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-boundary-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_root/scripts" "$test_root/.github/workflows"
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	git tag -a v1.2.3 -m fixture
	git push origin v1.2.3
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) -C . _release-run RELEASE_PIPELINE_ENTRY=release
EOF
cat > "$test_root/scripts/package.sh" <<'EOF'
#!/bin/sh
printf '%s\n' package
EOF
chmod 0755 "$test_root/scripts/package.sh"

"$checker" "$test_root" >/dev/null

if make -s -f "$test_root/Makefile" _release-publish >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal publication helper invocation passed" >&2
	exit 1
fi
if make -s -f "$test_root/Makefile" _release-run >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal pipeline body invocation passed" >&2
	exit 1
fi

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
