#!/usr/bin/env bash
# The OpenD witness: this tree's build against the process simulator, in a scratch
# world with its own HOME, XDG roots, config and socket, captured into <outdir>.
# It fails closed: it refuses to start unless every path is inside its scratch
# directory, and it fails if the operator's socket, stop marker or policy directory
# changed while it ran. It never runs stop, restart or anything else that manages a
# daemon by name, and it dials only the simulator.
set -u

refuse() {
	echo "opend-witness: refused: $*" >&2
	exit 1
}

[ $# -eq 1 ] || refuse "usage: scripts/opend-witness.sh <outdir>"
out=$1
repo=$(cd "$(dirname "$0")/.." && pwd)
bin=$repo/bin/canary
[ -x "$bin" ] || refuse "no $bin: run make build first"

operator_home=$HOME
operator_state() {
	local path
	for path in "$operator_home/.cache/ibkr/ibkr.sock" "$operator_home/.cache/ibkr/ibkr.sock.stopped" "$operator_home/.config/ibkr/policies"; do
		ls -lid "$path" 2>/dev/null || echo "absent $path"
	done
}
before=$(operator_state)

dir=$(mktemp -d /tmp/opend-witness.XXXXXX) || refuse "no scratch directory"
case "$dir/" in
"$operator_home"/*) refuse "the scratch directory $dir is inside the operator's home" ;;
esac
mkdir -p "$out" "$dir/home"

go -C "$repo" build -o "$dir/opend-sim" ./cmd/opend-sim || exit 1
"$dir/opend-sim" -clock now >"$dir/sim.addr" 2>"$dir/sim.err" &
sim=$!
daemon=
cleanup() {
	[ -n "$daemon" ] && kill "$daemon" 2>/dev/null
	sleep 0.5
	kill "$sim" 2>/dev/null
}
trap cleanup EXIT
for _ in $(seq 1 100); do [ -s "$dir/sim.addr" ] && break; sleep 0.05; done
addr=$(cat "$dir/sim.addr")
[ -n "$addr" ] || refuse "the simulator printed no address: $(cat "$dir/sim.err")"
host=${addr%:*}
port=${addr##*:}

cat >"$dir/config.toml" <<EOF
[broker]
kind = "opend"

[opend]
host = "$host"
port = $port
account = "1000001"
EOF

export HOME="$dir/home" XDG_CONFIG_HOME="$dir/config" XDG_STATE_HOME="$dir/state" XDG_CACHE_HOME="$dir/cache" XDG_RUNTIME_DIR="$dir/run"
export CANARY_CONFIG="$dir/config.toml" CANARY_SOCKET="$dir/canary.sock" COLUMNS=80 NO_COLOR=1
for path in "$HOME" "$XDG_CONFIG_HOME" "$XDG_STATE_HOME" "$XDG_CACHE_HOME" "$XDG_RUNTIME_DIR" "$CANARY_CONFIG" "$CANARY_SOCKET"; do
	case "$path" in
	"$dir"/*) ;;
	*) refuse "$path is outside the scratch directory $dir" ;;
	esac
done
mkdir -p "$XDG_RUNTIME_DIR"

"$bin" daemon --foreground --config "$CANARY_CONFIG" --socket "$CANARY_SOCKET" --log stderr >"$dir/daemon.log" 2>&1 &
daemon=$!
for _ in $(seq 1 200); do [ -S "$CANARY_SOCKET" ] && break; sleep 0.05; done
[ -S "$CANARY_SOCKET" ] || refuse "the daemon did not serve $CANARY_SOCKET: $(tail -3 "$dir/daemon.log")"
for _ in $(seq 1 200); do
	"$bin" status --json 2>/dev/null | grep -q '"connected": *true' && break
	sleep 0.1
done

run() {
	local name=$1
	shift
	{
		printf '$ canary %s\n\n' "$*"
		"$bin" "$@" 2>&1
		printf '\n[exit %s]\n' "$?"
	} >"$out/$name.txt"
}
sleep 3
run status status
run status-json status --json
run account account
run positions positions
run brief brief
run edge edge
run trading-status trading status
run stress stress
run rules rules
sleep 2
run status-after-reads status

# The MCP tools through `canary mcp`, as an MCP client drives them.
{
	printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"witness","version":"1"}}}'
	printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
	id=2
	for tool in canary_status canary_account canary_positions canary_brief canary_edge canary_recon_status canary_reporting canary_lending_fees; do
		printf '{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"%s","arguments":{}}}\n' "$id" "$tool"
		id=$((id + 1))
		sleep 3
	done
	sleep 2
} | "$bin" mcp >"$out/mcp.jsonl" 2>"$out/mcp.err"

# What the stack wrote, and that the home directory took nothing.
(cd "$dir" && find . -path ./state -prune -o -type f -print | sort) >"$out/files.txt"
echo "home entries: $(find "$dir/home" -mindepth 1 | wc -l | tr -d ' ')" >>"$out/files.txt"
echo "sim address: $addr" >>"$out/files.txt"
tail -40 "$dir/daemon.log" >"$out/daemon-tail.log"
echo "$dir" >"$out/dir.txt"

after=$(operator_state)
if [ "$before" != "$after" ]; then
	echo "opend-witness: FAIL: the operator's paths changed during the run" >&2
	diff <(echo "$before") <(echo "$after") >&2
	exit 1
fi
echo "opend-witness: OK: operator paths unchanged; captures in $out"
