#!/usr/bin/env bash
set -euo pipefail

payload="$(cat)"
command_line="unknown"

block() {
  printf 'Canary CLI safety hook blocked command: %s\n\n%s\n' "$command_line" "$1" >&2
  exit 2
}

payload_mentions_cli() {
  printf '%s' "$payload" | grep -Eq '(^|[^[:alnum:]_-])(ibkr|canary)([^[:alnum:]_-]|$)'
}

if ! command -v jq >/dev/null 2>&1; then
  if payload_mentions_cli; then
    command_line="${payload//$'\n'/ }"
    command_line="${command_line:0:240}"
    block "jq is required so the project hook can inspect agent tool payloads before broker-adjacent Canary CLI commands run."
  fi
  exit 0
fi

command_line="$(
  printf '%s' "$payload" | jq -r '
    def as_command:
      if type == "array" then map(tostring) | join(" ")
      elif type == "string" then .
      else ""
      end;

    [
      (.tool_input.command? | as_command),
      (.tool.input.command? | as_command),
      (.input.command? | as_command),
      (.arguments.command? | as_command),
      (.params.command? | as_command),
      (.command? | as_command),
      (.tool_input.args? | as_command),
      (.args? | as_command),
      (.argv? | as_command)
    ]
    | map(select(length > 0))
    | .[0] // ""
  ' 2>/dev/null || true
)"

command_line="${command_line#"${command_line%%[![:space:]]*}"}"
command_line="${command_line%"${command_line##*[![:space:]]}"}"
# The raw line feeds the command-position scanner, which needs the quoting
# intact to tell data from code. The has_re gates below then run with quotes
# stripped so trivially quoted invocations cannot slip past the verb
# matching. `ibkr` is retained only as a retired anti-bypass spelling.
command_line_raw="$command_line"
command_line="${command_line//\'/}"
command_line="${command_line//\"/}"

has_re() {
  printf '%s' "$command_line" | grep -Eq "$1"
}

# A CLI name is an invocation only where a segment actually runs it. A path
# argument that merely ends in the name — `git -C /Users/osauer/dev/ibkr status`
# — is not, and neither is quoted data: `grep -n "retired\|ibkr" f` and a
# commit message carrying "; ibkr" reach an exec in no shell, yet a
# quote-blind scan promoted them to command position and broke read-only
# tooling. The scanner therefore walks the raw line under shell quoting
# rules: quoted and escaped separators stay data, a quoted name at command
# position ("ibkr" status) still matches, `$(`/backtick substitutions open
# command position even inside double quotes, and `sh -c` payloads re-enter
# as nested command lines. Environment prefixes, leading flags, and wrapper
# words do not consume the position, so neither `FOO=1 canary …` nor
# `env ibkr …` can hide an invocation.
scan_flush() {
  local t="$scan_token" had="$scan_has_token"
  local sep_chars=$' \t\n;&|(){}`'
  scan_token=''
  scan_has_token=0
  if [[ -z "$t" && "$had" -eq 0 ]]; then
    return 0
  fi
  if [[ "$scan_expect" -ne 1 ]]; then
    return 0
  fi
  if [[ "$t" == [[:alpha:]_]*=* || "$t" == -* ]]; then
    return 0
  fi
  if [[ "$scan_shellc" -eq 1 && "$t" == *["$sep_chars"]* ]]; then
    scan_shellc=0
    scan_command_names "$t" "$((scan_depth + 1))"
    scan_expect=0
    return 0
  fi
  scan_shellc=0
  case "${t##*/}" in
  sh | bash | zsh | dash)
    scan_shellc=1
    return 0
    ;;
  command | builtin | exec | env | time | nohup | nice | ionice | stdbuf | sudo | doas | xargs)
    return 0
    ;;
  ibkr | canary)
    printf '%s\n' "${t##*/}"
    ;;
  esac
  scan_expect=0
}

scan_sep() {
  scan_expect=1
  scan_shellc=0
}

scan_command_names() {
  local scan_line="$1" scan_depth="${2:-0}"
  if [[ "$scan_depth" -gt 4 ]]; then
    return 0
  fi
  local scan_token='' scan_has_token=0 scan_expect=1 scan_shellc=0
  local scan_state='' scan_stack='' rest top
  local i=0 n=${#scan_line} ch nx
  while [[ "$i" -lt "$n" ]]; do
    ch="${scan_line:$i:1}"
    if [[ "$scan_state" == "'" ]]; then
      if [[ "$ch" == "'" ]]; then
        scan_state=''
      else
        scan_token+="$ch"
      fi
      i=$((i + 1))
      continue
    fi
    if [[ "$scan_state" == '"' ]]; then
      case "$ch" in
      '"')
        scan_state=''
        ;;
      '\')
        nx="${scan_line:$((i + 1)):1}"
        if [[ -n "$nx" ]]; then
          scan_token+="$nx"
          i=$((i + 1))
        fi
        ;;
      '`')
        scan_flush
        scan_sep
        scan_stack+='G'
        scan_state=''
        ;;
      '$')
        if [[ "${scan_line:$((i + 1)):1}" == '(' ]]; then
          scan_flush
          scan_sep
          scan_stack+='d'
          scan_state=''
          i=$((i + 1))
        else
          scan_token+="$ch"
        fi
        ;;
      *)
        scan_token+="$ch"
        ;;
      esac
      i=$((i + 1))
      continue
    fi
    case "$ch" in
    "'")
      scan_state="'"
      scan_has_token=1
      ;;
    '"')
      scan_state='"'
      scan_has_token=1
      ;;
    '\')
      nx="${scan_line:$((i + 1)):1}"
      if [[ -n "$nx" ]]; then
        if [[ "$nx" != $'\n' ]]; then
          scan_token+="$nx"
        fi
        i=$((i + 1))
      fi
      ;;
    ' ' | $'\t')
      scan_flush
      ;;
    '$')
      if [[ "${scan_line:$((i + 1)):1}" == '(' ]]; then
        scan_flush
        scan_sep
        scan_stack+='p'
        i=$((i + 1))
      else
        scan_token+="$ch"
      fi
      ;;
    '(')
      scan_flush
      scan_sep
      scan_stack+='p'
      ;;
    ')')
      scan_flush
      scan_sep
      if [[ -n "$scan_stack" ]]; then
        rest="${scan_stack%?}"
        top="${scan_stack#"$rest"}"
        scan_stack="$rest"
        if [[ "$top" == 'd' ]]; then
          scan_state='"'
          scan_has_token=1
        fi
      fi
      ;;
    '`')
      scan_flush
      scan_sep
      top=''
      if [[ -n "$scan_stack" ]]; then
        rest="${scan_stack%?}"
        top="${scan_stack#"$rest"}"
      fi
      if [[ "$top" == 'g' ]]; then
        scan_stack="$rest"
      elif [[ "$top" == 'G' ]]; then
        scan_stack="$rest"
        scan_state='"'
        scan_has_token=1
      else
        scan_stack+='g'
      fi
      ;;
    $'\n' | ';' | '&' | '|' | '{' | '}')
      scan_flush
      scan_sep
      ;;
    *)
      scan_token+="$ch"
      ;;
    esac
    i=$((i + 1))
  done
  scan_flush
}

cli_command_names() {
  scan_command_names "$command_line_raw" 0
}

# The write gates below stay deliberately broad: a name anywhere on the line
# still reaches them, because a false block costs a retry and a false allow
# reaches a broker. Only the retired-name rejection and the executable this
# hook itself runs are narrowed to command position.
#
# The word regex alone misses a name opening a substitution or subshell, where
# the preceding character is `(` rather than space or slash, so command
# position is a second way in — never a way out.
cli_mentioned() {
  has_re '(^|[[:space:]/])(ibkr|canary)([[:space:]]|$)' || [[ -n "$(cli_command_names)" ]]
}

if [[ -z "$command_line" ]] || ! cli_mentioned; then
  exit 0
fi

if cli_command_names | grep -qx 'ibkr'; then
  block "The retired ibkr executable is not callable. Use canary; old command spellings are rejected instead of treated as compatibility aliases."
fi

shell_composition() {
  [[ "$command_line" == *$'\n'* || "$command_line" =~ [\;\&\|\<\>\`] || "$command_line" =~ \$\( ]]
}

read_only_help_command() {
  if ! has_re '(^|[[:space:]/])(ibkr|canary)([[:space:]]+[^;&<>`$()]*)?[[:space:]]+(--help|-h|help)([[:space:]]|$)'; then
    return 1
  fi
  if [[ "$command_line" =~ [\;\&\<\>\`] || "$command_line" =~ \$\( ]]; then
    return 1
  fi
  local first_cli_match=""
  if [[ "$command_line" =~ (^|[[:space:]/])(ibkr|canary)([[:space:]]|$) ]]; then
    first_cli_match="${BASH_REMATCH[0]}"
  fi
  local after_first_cli="${command_line#*"$first_cli_match"}"
  if printf '%s' "$after_first_cli" | grep -Eq '(^|[[:space:]/])(ibkr|canary)([[:space:]]|$)'; then
    return 1
  fi
  if [[ "$command_line" == *"|"* ]] &&
    ! printf '%s' "$command_line" | grep -Eq '\|[[:space:]]*(cat|head|tail|sed|awk|grep|rg|jq|less|more|wc)([[:space:]]|$)'; then
    return 1
  fi
  return 0
}

command_cli() {
  local name
  name="$(cli_command_names | head -1)"
  if [[ -z "$name" ]]; then
    return 1
  fi
  printf '%s' "$name"
}

trading_status_json() {
  local cli
  cli="$(command_cli)" || return 1
  "$cli" trading status --json 2>/dev/null
}

broker_route_ready_filter='
  def paper_ready:
    (.mode == "paper")
    and ((.blocked // false) == false)
    and (
      (.gateway_port == 4002)
      or (.gateway_port == 7497)
      or ((.account // "" | ascii_upcase | startswith("DU")))
    )
    and ((.live_override // "blocked") != "ready");
  def live_ready:
    (.mode == "live")
    and ((.blocked // false) == false)
    and ((.live_override // "blocked") == "ready");
  def only_freeze_blocker:
    ((.write_blockers // []) | length > 0)
    and all(.write_blockers[]?; .code == "trading_frozen");
  (paper_ready or live_ready)
  and (
    (.can_write == true)
    or ($allow_cancel == true and only_freeze_blocker)
  )
'

broker_writes_ready() {
  local allow_cancel="${1:-false}"
  local status
  status="$(trading_status_json)" || return 1
  printf '%s' "$status" | jq -e --argjson allow_cancel "$allow_cancel" "$broker_route_ready_filter" >/dev/null
}

broker_write_status_summary() {
  local status
  if ! status="$(trading_status_json)"; then
    printf 'trading.status unavailable'
    return
  fi
  printf '%s' "$status" | jq -r '
    "mode=\(.mode // "unknown") account=\(.account // "unknown") endpoint=\(.endpoint // "unknown") can_write=\(.can_write // false) blocked=\(.blocked // false) live_override=\(.live_override // "unknown") blockers=\((.write_blockers // .blockers // []) | map(.code) | join(","))"
  ' 2>/dev/null || printf 'trading.status unreadable'
}

allow_broker_write_or_block() {
  local allow_cancel="${1:-false}"
  if broker_writes_ready "$allow_cancel"; then
    exit 0
  fi
  block "Broker-adjacent Canary CLI writes are allowed only when trading.status is paper/write-ready on a paper-looking route or live/write-ready with live_override=ready. Disabled, blocked, frozen, unknown, and route-mismatched states remain blocked. Current: $(broker_write_status_summary)"
}

broker_write_command() {
  has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+proposals[[:space:]]+(preview|submit|ignore|request-stop)([[:space:]]|$)' ||
    {
      has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+proposals[[:space:]]+reduce([[:space:]]|$)' &&
		has_re '(^|[[:space:]])--submit(=|[[:space:]]|$)'
	} ||
	has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+opportunities[[:space:]]+(preview|exercise|ignore)([[:space:]]|$)' ||
	has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+strategies[[:space:]]+(close|reduce)([[:space:]]|$)' ||
	has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+order[[:space:]]+(preview|place|modify|cancel)([[:space:]]|$)' ||
	has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+(submit|place|transmit|modify|cancel|close)([[:space:]]|$)'
}

state_write_command() {
  has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+settings[[:space:]]+set([[:space:]]|$)' ||
    has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+daemon[[:space:]]+(purge|reset|wipe)([[:space:]]|$)'
}

cancel_command() {
  has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+order[[:space:]]+cancel([[:space:]]|$)' ||
    has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+cancel([[:space:]]|$)'
}

if read_only_help_command; then
  exit 0
fi

if shell_composition && { broker_write_command || state_write_command; }; then
  block "Run broker-adjacent Canary CLI write commands directly, without shell composition, pipes, redirection, command substitution, or chained commands."
fi

if has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+settings[[:space:]]+set([[:space:]]|$)'; then
  block "Runtime settings writes, including trading.freeze and trading limit changes, must be run by the user from an interactive session."
fi

if has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+daemon[[:space:]]+(purge|reset|wipe)([[:space:]]|$)'; then
  block "Daemon destructive maintenance must be run by the user, not by an agent session."
fi

if has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+(order|orders|trading|trade|trades|proposals|opportunities)([[:space:]]|$)' &&
  has_re '(^|[[:space:]])(--help|-h|help)([[:space:]]|$)'; then
  exit 0
fi

# The plural `orders` noun is a read-only journal surface end to end (open/
# history listings); only the singular `order` verb tree can reach broker
if has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+orders([[:space:]]|$)'; then
  exit 0
fi

if has_re '(^|[[:space:]/])(ibkr|canary)[[:space:]]+(order[[:space:]]+status|trading[[:space:]]+status|proposals[[:space:]]+(status|refresh|list)|opportunities[[:space:]]+(status|refresh|list))([[:space:]]|$)'; then
  exit 0
fi

if broker_write_command; then
  if cancel_command; then
    allow_broker_write_or_block true
  fi
  allow_broker_write_or_block false
fi

exit 0
