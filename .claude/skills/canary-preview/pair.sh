#!/bin/sh
# Mint a pairing URL for an isolated Canary preview instance; print only the URL.
set -eu
port="${1:?usage: pair.sh <port>}"
canary app pair --addr "127.0.0.1:${port}" --public-url "http://127.0.0.1:${port}" --json | jq -r .url
