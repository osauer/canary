#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
. "$script_dir/lib-daemon-control.sh"

for command in \
	'/Users/test/.local/bin/canary daemon' \
	'/Users/test/.local/bin/canary daemon --socket /tmp/ibkr.sock' \
	'/Users/test/.local/bin/ibkr daemon' \
	'canary daemon --foreground' \
	'ibkr daemon --foreground'; do
	if ! is_product_daemon_command "$command"; then
		echo "lib-daemon-control test: missed daemon command: $command" >&2
		exit 1
	fi
done

for command in \
	'/Users/test/.local/bin/notcanary daemon' \
	'/Users/test/.local/bin/canary status' \
	'/Users/test/.local/bin/ibkr-daemon' \
	'echo /tmp/canary daemon' \
	'/bin/sh -c /tmp/canary daemon' \
	'echo canary daemonized' \
	'canary-helper daemon'; do
	if is_product_daemon_command "$command"; then
		echo "lib-daemon-control test: false daemon match: $command" >&2
		exit 1
	fi
done

echo "lib-daemon-control test: OK"
