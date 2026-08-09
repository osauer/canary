#!/usr/bin/env bash
#
# build-mcpb.sh - assemble the release MCP Bundle from the native tarballs.
# Called by `make release-mcpb` after `make release-binaries` has produced

set -euo pipefail

version="${1:?usage: build-mcpb.sh <version> <dist-dir> <targets>}"
dist_dir="${2:?dist dir required}"
targets="${3:?release targets required}"

case "$version" in
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *)
        echo "build-mcpb: version must look like vX.Y.Z (got $version)" >&2
        exit 2
        ;;
esac

semver="${version#v}"
stage="${dist_dir}/mcpb/canary"
bundle="${dist_dir}/canary-${version}.mcpb"
stable_bundle="${dist_dir}/canary.mcpb"

mcpb_package="${MCPB_PACKAGE:-@anthropic-ai/mcpb@2.1.2}"
mcpb() {
    npx -y "$mcpb_package" "$@"
}

rm -rf "$stage" "$bundle" "$stable_bundle"
mkdir -p "$stage/server/bin"
install -m 0644 web/app/icon-512.png "$stage/icon.png"

for target in $targets; do
    tarball="${dist_dir}/canary-${version}-${target}.tar.gz"
    if [[ ! -f "$tarball" ]]; then
        echo "build-mcpb: missing release tarball: $tarball" >&2
        exit 1
    fi

    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' RETURN
    tar -xzf "$tarball" -C "$tmp" "canary-${version}-${target}/canary"
    install -m 0755 "$tmp/canary-${version}-${target}/canary" "$stage/server/bin/canary-${target}"
    rm -rf "$tmp"
    trap - RETURN
done

cat > "$stage/server/canary" <<'SH'
#!/usr/bin/env sh
set -eu

server_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *)
        echo "Canary MCPB: unsupported OS $(uname -s); supported: Darwin, Linux" >&2
        exit 127
        ;;
esac

case "$(uname -m)" in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64) arch=amd64 ;;
    *)
        echo "Canary MCPB: unsupported architecture $(uname -m); supported: arm64, amd64" >&2
        exit 127
        ;;
esac

bin="$server_dir/bin/canary-$os-$arch"
if [ ! -x "$bin" ]; then
    echo "Canary MCPB: missing bundled binary $bin" >&2
    exit 127
fi

exec "$bin" "$@"
SH
chmod 0755 "$stage/server/canary"

cat > "$stage/manifest.json" <<JSON
{
  "\$schema": "https://raw.githubusercontent.com/modelcontextprotocol/mcpb/main/schemas/mcpb-manifest-v0.4.schema.json",
  "manifest_version": "0.4",
  "name": "canary",
  "display_name": "Canary",
  "version": "$semver",
  "description": "Canary's no-broker-write Interactive Brokers MCP server for account, market analysis, and preview-only drafts.",
  "long_description": "Canary packages a local no-broker-write Interactive Brokers (IBKR) MCP server for Claude Desktop and other MCPB-compatible clients. Its 13 read-only tools cover health, account, positions, named-symbol technical evidence, the daily brief, rulebook, proposals, opportunities, settings, and order-lifecycle reads. It has no resources, previews, settings writes, or broker-write tools.",
  "icon": "icon.png",
  "icons": [
    {
      "src": "icon.png",
      "size": "512x512"
    }
  ],
  "author": {
    "name": "Oliver Sauer",
    "url": "https://github.com/osauer"
  },
  "repository": {
    "type": "git",
    "url": "https://github.com/osauer/canary"
  },
  "homepage": "https://osauer.dev/canary/",
  "documentation": "https://osauer.dev/canary/docs/",
  "support": "https://github.com/osauer/canary/issues",
  "server": {
    "type": "binary",
    "entry_point": "server/canary",
    "mcp_config": {
      "command": "\${__dirname}/server/canary",
      "args": ["mcp"],
      "env": {}
    }
  },
  "tools_generated": true,
  "keywords": [
    "ibkr",
    "interactive-brokers",
    "mcp",
    "mcpb",
    "tws-api",
    "claude-desktop",
    "finance",
    "read-only"
  ],
  "license": "MIT",
  "privacy_policies": [
    "https://github.com/osauer/canary/blob/$version/PRIVACY.md"
  ],
  "compatibility": {
    "platforms": ["darwin", "linux"]
  }
}
JSON

mcpb validate "$stage/manifest.json"
mcpb pack "$stage" "$bundle"
if [[ -n "${MCPB_SIGN_CERT:-}" || -n "${MCPB_SIGN_KEY:-}" ]]; then
    if [[ -z "${MCPB_SIGN_CERT:-}" || -z "${MCPB_SIGN_KEY:-}" ]]; then
        echo "build-mcpb: set both MCPB_SIGN_CERT and MCPB_SIGN_KEY to sign the bundle" >&2
        exit 1
    fi
    sign_args=(sign --cert "$MCPB_SIGN_CERT" --key "$MCPB_SIGN_KEY")
    if [[ -n "${MCPB_SIGN_INTERMEDIATE:-}" ]]; then
        sign_args+=(--intermediate)
        # shellcheck disable=SC2206 # intentional word splitting for multiple intermediate paths
        intermediates=($MCPB_SIGN_INTERMEDIATE)
        sign_args+=("${intermediates[@]}")
    fi
    mcpb "${sign_args[@]}" "$bundle"
    mcpb verify "$bundle"
else
    echo "build-mcpb: bundle is unsigned; set MCPB_SIGN_CERT and MCPB_SIGN_KEY to produce a signed MCPB" >&2
fi
mcpb info "$bundle"
cp "$bundle" "$stable_bundle"
cmp -s "$bundle" "$stable_bundle" || {
    echo "build-mcpb: stable asset differs from versioned bundle: $stable_bundle" >&2
    exit 1
}

unpack_dir="$(mktemp -d)"
trap 'rm -rf "$unpack_dir"' EXIT
mcpb unpack "$bundle" "$unpack_dir" >/dev/null
wrapped_version="$("$unpack_dir/server/canary" version | head -n1)"
case "$wrapped_version" in
    "canary $version"*|"canary  $version"*|"Canary CLI  $version"*) ;;
    *)
        echo "build-mcpb: unpacked wrapper reports unexpected version: $wrapped_version" >&2
        exit 1
        ;;
esac

echo "build-mcpb: built $bundle"
echo "build-mcpb: copied stable asset $stable_bundle"
echo "build-mcpb: unpacked wrapper verified: $wrapped_version"
