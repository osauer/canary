#!/usr/bin/env bash
# lib-daemon-control.sh — shared helpers for the smoke scripts to spin
# up an isolated `canary daemon` under /tmp without colliding with the
# user's canonical daemon. Sourced by release-verify.sh, release-smoke.sh,
# and wire-smoke.sh.
#
# Provides:
#   stop_existing_daemons <label>
#       SIGTERM (with SIGKILL fallback) every running canonical
#       `canary daemon` or still-running pre-upgrade `ibkr daemon` process. The IBKR
#       gateway accepts one connection per client ID,
#       so running two daemons with the same ID makes the second fail
#       with "code 326 / client id already in use" — this aborted the
#       v0.16.0 release on first run before the workaround was added.
#       Survivors auto-spawn on the next CLI call, so the cost is one
#       bounce. <label> is the prefix for the user-facing banner
#       ("release-verify", "release-smoke", "wire-smoke", …).
#
#   kill_daemon_from_lockfile <lockfile>
#       SIGTERM the daemon whose PID is recorded in <lockfile>; wait up
#       to 3s for graceful exit; SIGKILL stragglers. Silent no-op when
#       the lockfile is unreadable (daemon already exited cleanly).

stop_existing_daemons() {
    local label="${1:-smoke}"
    local pids candidate_pids pid cmd
    candidate_pids="$(pgrep -f '(^|/)(canary|ibkr)[[:space:]]+daemon([[:space:]]|$)' 2>/dev/null || true)"
    pids=""
    for pid in $candidate_pids; do
        cmd="$(ps -o command= -p "$pid" 2>/dev/null || true)"
        if is_product_daemon_command "$cmd"; then
            pids="${pids:+$pids }$pid"
        fi
    done
    if [[ -z "$pids" ]]; then
        return 0
    fi
    echo "${label}: stopping pre-existing daemon(s) so they don't race the smoke daemon for the gateway client-ID slot:"
    for pid in $pids; do
        cmd="$(ps -o command= -p "$pid" 2>/dev/null || echo '?')"
        echo "  pid=$pid cmd=$cmd"
    done
    for pid in $pids; do
        kill -TERM "$pid" 2>/dev/null || true
    done
    # Wait up to 5s for graceful exit before escalating.
    local exited=""
    for _ in $(seq 1 50); do
        local remaining=""
        for pid in $pids; do
            if kill -0 "$pid" 2>/dev/null; then
                remaining="$remaining $pid"
            fi
        done
        if [[ -z "$remaining" ]]; then
            exited=1
            break
        fi
        sleep 0.1
    done
    if [[ -z "$exited" ]]; then
        for pid in $pids; do
            kill -KILL "$pid" 2>/dev/null || true
        done
    fi
    # TWS-side cool-down. Killing the daemon closes the TCP connection,
    # but TWS retains the client-ID slot for ~1-3s after the FIN before
    # accepting a new connection with the same ID — without this pause
    # the smoke daemon races TWS and hits code=326 "client id already
    # in use" on the very next handshake. Observed on the v0.27.12
    # release-verify attempt. The CLI's autospawn behaviour (an MCP
    # request can respawn the daemon mid-kill) makes this race common
    # rather than rare; 5s is conservative — TWS typically clears in
    # 1-2s but a busy gateway can stretch it.
    sleep 5
}

is_product_daemon_command() {
    local command="${1:-}"
    local program subcommand
    read -r program subcommand _ <<<"$command"
    program="${program##*/}"
    [[ "$program" == "canary" || "$program" == "ibkr" ]] && [[ "$subcommand" == "daemon" ]]
}

kill_daemon_from_lockfile() {
    local lockfile="$1"
    if [[ ! -r "$lockfile" ]]; then
        return 0
    fi
    local pid
    pid="$(tr -d '[:space:]' < "$lockfile" 2>/dev/null || true)"
    if [[ -z "$pid" || "$pid" -le 0 ]] 2>/dev/null; then
        return 0
    fi
    kill -TERM "$pid" 2>/dev/null || true
    # Wait up to 3s for graceful exit before escalating.
    for _ in $(seq 1 30); do
        if ! kill -0 "$pid" 2>/dev/null; then break; fi
        sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
}
