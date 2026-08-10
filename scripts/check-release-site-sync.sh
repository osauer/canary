#!/bin/sh
# check-release-site-sync.sh — gate the public product surfaces before a
# release. The MCP discovery JSON stamps are checked on EVERY release; the
# static site push is required only before non-patch releases.
set -eu

version=${1:-}

usage() {
  echo "usage: $0 vX.Y.Z" >&2
}

[ -n "$version" ] || {
  usage
  exit 1
}

case "$version" in
  v[0-9]*.[0-9]*.[0-9]* | v[0-9]*.[0-9]*.[0-9]*-*) ;;
  *)
    echo "release-site-check: RELEASE_VERSION must look like vX.Y.Z (got $version)" >&2
    exit 1
    ;;
esac

core=${version#v}
core=${core%%-*}
major=${core%%.*}
rest=${core#*.}
minor=${rest%%.*}
patch=${rest#*.}

if [ -z "$major" ] || [ -z "$minor" ] || [ -z "$patch" ]; then
  echo "release-site-check: cannot parse semantic version $version" >&2
  exit 1
fi
case "$major:$minor:$patch" in
  *[!0-9:]*)
    echo "release-site-check: cannot parse semantic version $version" >&2
    exit 1
    ;;
esac

if [ ! -d docs ]; then
  echo "release-site-check: docs/ missing; run from the Canary repo root" >&2
  exit 1
fi

plain=${version#v}

# Machine-readable MCP discovery metadata ships on EVERY release, patch
# included, so it is gated before the patch short-circuit below. v2.3.1 cut
# with all three files lagging at 2.3.0 because this loop used to sit after
# the early return, where no patch release ever reached it.
# One hint per file, because they are not fixed the same way. server.json is a
# copy the generator makes; the card's serverInfo.version is hand-maintained and
# written by nothing — `-write` round-trips serverInfo as opaque bytes — so the
# old shared hint sent whoever hit the card to run a generator that could not
# produce the stamp, and the gate failed again with the identical message.
for f in docs/mcp-server.json docs/.well-known/mcp/server.json docs/.well-known/mcp/server-card.json; do
  if ! grep -q "\"version\": \"$plain\"" "$f"; then
    echo "release-site-check: $f version is not $plain" >&2
    case "$f" in
    docs/mcp-server.json)
      echo "                    bump \"version\" in $f — it is the canonical stamp the others follow" >&2
      ;;
    docs/.well-known/mcp/server-card.json)
      echo "                    edit \"serverInfo\".\"version\" in $f by hand; no generator writes it" >&2
      ;;
    *)
      echo "                    bump docs/mcp-server.json, then run \`make docs-regen\` to refresh this copy" >&2
      ;;
    esac
    exit 1
  fi
done

# The issue-template placeholder is the version a bug reporter sees as the
# example to overwrite, so it ships on every release too. It has tracked the
# tag at every release since v2.3.0, but nothing gated it: `make check` has no
# view of this file at all, and a stale placeholder invites reports filed
# against a version that is no longer current. A missing file fails here the
# same way the discovery stamps above do.
template=.github/ISSUE_TEMPLATE/bug_report.yml
if ! grep -q "placeholder: \"$version\"" "$template"; then
  echo "release-site-check: $template placeholder is not $version" >&2
  echo "                    bump the \"Canary version\" placeholder by hand; no generator writes it" >&2
  exit 1
fi

if [ "$patch" -ne 0 ]; then
  echo "release-site-check: $version is a patch release; MCP discovery and issue-template stamps OK, static site push not required"
  exit 0
fi

if [ -n "$(git status --porcelain)" ]; then
  echo "release-site-check: working tree is dirty; commit the docs/ website update before releasing $version" >&2
  git status --short >&2
  exit 1
fi

head=$(git rev-parse HEAD)
main=$(git rev-parse origin/main 2>/dev/null) || {
  echo "release-site-check: origin/main is missing locally; run git fetch origin main" >&2
  exit 1
}
if [ "$head" != "$main" ]; then
  echo "release-site-check: HEAD ($head) does not match origin/main ($main)" >&2
  echo "                    push the docs/ website update before non-patch releases" >&2
  exit 1
fi

if ! grep -q "\"softwareVersion\": \"$plain\"" docs/index.html; then
  echo "release-site-check: docs/index.html softwareVersion is not $plain" >&2
  echo "                    update the osauer.dev/canary landing page for this non-patch release" >&2
  exit 1
fi

# Every public version stamp must move together: spoke-page JSON-LD alongside
# release version while only index.html was gated). The MCP discovery JSONs
# are gated above, on every release.
if ! grep -q "\"softwareVersion\": \"$plain\"" docs/interactive-brokers-mcp-server/index.html; then
  echo "release-site-check: docs/interactive-brokers-mcp-server/index.html softwareVersion is not $plain" >&2
  exit 1
fi

echo "release-site-check: $version requires and has a pushed osauer.dev/canary docs update"
