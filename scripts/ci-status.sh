#!/usr/bin/env bash
# SessionStart hook for Claude Code and Codex: one line saying what main's CI
# last did, so a red main is seen when work starts instead of days later.
# Prints nothing when gh, jq or the remote is unavailable; never fails the
# session. The newest run of each workflow decides; cancelled and skipped runs
# are neither green nor red.
set -u

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
command -v gh >/dev/null 2>&1 || exit 0
command -v jq >/dev/null 2>&1 || exit 0
repo="$(git -C "$root" remote get-url origin 2>/dev/null | sed -E 's#^.*github\.com[:/]##; s#\.git$##')"
[ -n "$repo" ] || exit 0

runs="$(gh run list --repo "$repo" --branch main --limit 25 --json conclusion,status,createdAt,url,name 2>/dev/null)" || exit 0
[ -n "$runs" ] && [ "$runs" != "[]" ] || exit 0

printf '%s' "$runs" | jq -r --arg repo "$repo" '
  # Newest run per workflow decides; the newest completed run alone can hide a
  # failed or still-running sibling workflow on the same push.
  group_by(.name) | map(sort_by(.createdAt) | reverse | .[0]) as $latest
  | [ $latest[] | select(.conclusion == "failure" or .conclusion == "timed_out") ] as $red
  | [ $latest[] | select(.conclusion == null or .conclusion == "") ] as $running
  | [ $latest[] | select(.conclusion == "success") ] as $green
  | if ($red | length) > 0 then
      "main CI (\($repo)): RED — " + ($red | map("\(.name) \(.conclusion) \(.createdAt[0:16]) UTC \(.url)") | join("; "))
      + (if ($running | length) > 0 then "; running: " + ($running | map(.name) | join(", ")) else "" end)
    elif ($running | length) > 0 then
      "main CI (\($repo)): running — " + ($running | map(.name) | join(", "))
      + (if ($green | length) > 0 then "; green: " + ($green | map(.name) | join(", ")) else "" end)
    elif ($green | length) > 0 then
      "main CI (\($repo)): green — " + ($green | map("\(.name) \(.createdAt[0:16]) UTC") | join("; "))
    else "main CI (\($repo)): no completed run yet" end' 2>/dev/null
