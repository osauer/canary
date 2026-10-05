#!/usr/bin/env bash
# SessionStart hook for Claude Code and Codex: one line saying what main's CI
# last did, so a red main is seen when work starts instead of days later.
# Prints nothing when gh, jq or the remote is unavailable; never fails the
# session. Cancelled and skipped runs are neither green nor red.
set -u

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
command -v gh >/dev/null 2>&1 || exit 0
command -v jq >/dev/null 2>&1 || exit 0
repo="$(git -C "$root" remote get-url origin 2>/dev/null | sed -E 's#^.*github\.com[:/]##; s#\.git$##')"
[ -n "$repo" ] || exit 0

runs="$(gh run list --repo "$repo" --branch main --limit 25 --json conclusion,createdAt,url,name 2>/dev/null)" || exit 0
[ -n "$runs" ] && [ "$runs" != "[]" ] || exit 0

printf '%s' "$runs" | jq -r --arg repo "$repo" '
  map(select(.conclusion == "success" or .conclusion == "failure" or .conclusion == "timed_out")) as $d
  | if ($d | length) == 0 then "main CI (\($repo)): no completed run yet"
    elif $d[0].conclusion == "success" then
      "main CI (\($repo)): green — \($d[0].name) \($d[0].createdAt[0:16]) UTC"
    else
      ([ $d | to_entries[] | select(.value.conclusion == "success") | .key ] | first // ($d | length)) as $n
      | "main CI (\($repo)): RED for \($n) run(s) since \($d[$n-1].createdAt[0:16]) UTC — \($d[0].name) \($d[0].conclusion) \($d[0].url)"
    end' 2>/dev/null
