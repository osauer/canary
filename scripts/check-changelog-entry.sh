#!/bin/sh
# check-changelog-entry.sh — assert the topmost CHANGELOG.md entry matches
# RELEASE_VERSION and has the required shape (heading + "What's new" stanza
# + at least one Keep-a-Changelog subsection). Runs from `make release`
# before `git tag` so a malformed entry aborts the release.
set -eu

ver=${RELEASE_VERSION:-}
[ -n "$ver" ] || {
  echo "check-changelog-entry: RELEASE_VERSION env required" >&2
  exit 1
}

cd "$(dirname "$0")/.."

path=${CHANGELOG_PATH:-CHANGELOG.md}
historical=${CHANGELOG_HISTORICAL:-0}
case "$historical" in 0|1) ;; *) echo "check-changelog-entry: CHANGELOG_HISTORICAL must be 0 or 1" >&2; exit 1 ;; esac
[ -f "$path" ] && [ ! -L "$path" ] || {
  echo "check-changelog-entry: changelog is missing or unsafe: $path" >&2
  exit 1
}

CHANGELOG_PATH="$path" ./scripts/check-changelog-public.sh

# 1) Topmost ## v entry must match RELEASE_VERSION with a timestamp suffix.
head=$(grep -m1 '^## v' "$path" || true)
case "$head" in
  "## $ver — "*) ;;
  "")
    echo "check-changelog-entry: CHANGELOG.md has no '## v' entries" >&2
    exit 1
    ;;
  *)
    echo "check-changelog-entry: topmost CHANGELOG entry is '$head'" >&2
    echo "                       expected '## $ver — <YYYY-MM-DD HH:MM TZ>'" >&2
    exit 1
    ;;
esac

# The heading is the release record, not the day the entry was first drafted.
# Requiring today's local date makes a delayed cut update the public timestamp
# instead of publishing a stale preparation date.
if [ "$historical" = 0 ]; then
  entry_date=$(printf '%s\n' "$head" | sed -n 's/^## v[^ ]* — \([0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]\) .*/\1/p')
  today=$(date +%Y-%m-%d)
  if [ "$entry_date" != "$today" ]; then
    echo "check-changelog-entry: $ver heading date is $entry_date; release date is $today" >&2
    echo "                       update the top entry when the release is actually cut" >&2
    exit 1
  fi
fi

# Storage changed from rebuildable side files to one authoritative daemon.db.
# These retired phrases carry unsafe deletion/recovery advice and must never
# return in the current release entry.
top_entry=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" ") }
  in_ver { print }
' "$path")
retired=$(printf '%s\n' "$top_entry" | grep -Ei 'history\.db|\.jsonl|history\.rotation|rotated/|history index|journal scan' || true)
if [ -n "$retired" ]; then
  echo "check-changelog-entry: $ver still describes the retired history-file architecture" >&2
  echo "                       describe daemon.db as the sole live authority instead" >&2
  exit 1
fi

# 2) Matching entry must have a non-empty `### What's new` section.
if ! awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### What.s new$/ { in_new = 1; next }
  in_ver && in_new && /^###/ { exit }
  in_new && NF { found = 1 }
  END { exit !found }
' "$path"; then
  echo "check-changelog-entry: $ver has no non-empty '### What'\''s new' section" >&2
  echo "                       (must follow the version heading; describes user-visible change)" >&2
  exit 1
fi

# 3) Matching entry must have at least one Keep-a-Changelog subsection.
has_kac=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### (Added|Changed|Deprecated|Removed|Fixed|Security)$/ { print "yes"; exit }
' "$path")
[ "$has_kac" = yes ] || {
  echo "check-changelog-entry: $ver has no ### Added/Changed/Deprecated/Removed/Fixed/Security section" >&2
  exit 1
}

# 4) If `### Engineering notes` is present, content must be <= 15 lines.
#    Long Engineering notes are almost always duplicating Changed/Fixed
#    bullets or restating commit-message context — neither earns its keep
#    in a CHANGELOG.
eng_lines=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### Engineering notes$/ { in_eng = 1; next }
  in_ver && in_eng && /^###/ { exit }
  in_ver && in_eng && /^## v[0-9]/ { exit }
  in_eng { n++ }
  END { print n+0 }
' "$path")
if [ "$eng_lines" -gt 15 ]; then
  echo "check-changelog-entry: $ver '### Engineering notes' has $eng_lines lines (limit 15)" >&2
  echo "                       trim it. If a fact fits a one-line bullet, move it to Changed/Fixed." >&2
  exit 1
fi

# 5) No internal finding IDs (F-NN, F#NN, finding-N) inside KaC bullets.
#    These are maintainer-internal handles with no value for the section's
#    reader. The finding ID belongs in the commit message or the issue
#    tracker, not in the user-facing changelog.
finding=$(awk -v ver="$ver" '
  /^## v[0-9]/ { if (in_ver) exit; in_ver = ($0 ~ "^## "ver" "); next }
  in_ver && /^### (Added|Changed|Deprecated|Removed|Fixed|Security)$/ { in_kac = 1; next }
  in_ver && in_kac && /^###/ { in_kac = 0 }
  in_ver && in_kac && /(\*\*F-[0-9]+|F#[0-9]+|finding-[0-9]+)/ { print "L"NR": "$0; exit }
' "$path")
if [ -n "$finding" ]; then
  echo "check-changelog-entry: $ver KaC bullet references an internal finding ID" >&2
  echo "                       $finding" >&2
  echo "                       Finding IDs belong in commit messages, not the CHANGELOG." >&2
  echo "                       Frame the bullet for the section's reader (what the user notices)." >&2
  exit 1
fi

echo "check-changelog-entry: $ver OK"
