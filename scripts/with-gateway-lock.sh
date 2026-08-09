#!/usr/bin/env bash
#
# wire smoke, and release smokes each spawn daemons against the same
#                            sized for a worst-case off-hours release smoke)
#     fd the shell already holds. perl exiting does NOT release the lock:
#     hold the lock after the gate finishes. The lock lifetime is exactly
set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "usage: with-gateway-lock.sh <command> [args...]" >&2
    exit 2
fi

LOCKFILE="${IBKR_GATEWAY_LOCK_FILE:-${TMPDIR:-/tmp}/ibkr-gateway.lock}"
WAIT="${IBKR_GATEWAY_LOCK_WAIT:-900}"
if [[ ! "$WAIT" =~ ^[0-9]+$ ]]; then
    echo "with-gateway-lock: invalid IBKR_GATEWAY_LOCK_WAIT: $WAIT" >&2
    exit 2
fi

exec 9>>"$LOCKFILE"

perl -e '
    use Fcntl qw(:flock);
    my ($timeout, $lockfile) = @ARGV;
    open(my $fh, "<&=", 9) or die "with-gateway-lock: cannot adopt fd 9: $!\n";
    exit 0 if flock($fh, LOCK_EX | LOCK_NB);
    print STDERR "with-gateway-lock: gateway busy (another session holds $lockfile); waiting up to ${timeout}s...\n";
    my $deadline = time + $timeout;
    my $next_note = time + 30;
    until (flock($fh, LOCK_EX | LOCK_NB)) {
        die "with-gateway-lock: timed out after ${timeout}s waiting for $lockfile\n"
            if time >= $deadline;
        if (time >= $next_note) {
            printf STDERR "with-gateway-lock: still waiting for %s (%ds left)\n",
                $lockfile, $deadline - time;
            $next_note = time + 30;
        }
        sleep 1;
    }
    print STDERR "with-gateway-lock: lock acquired, continuing\n";
' "$WAIT" "$LOCKFILE"

"$@" 9>&-
