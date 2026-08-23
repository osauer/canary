# Releases and support

Updated: 2026-08-23

Every release publishes two binaries per platform, under two different names. One is read-only. The other can send orders to your broker. That difference is compiled in rather than configured, so the filename you download decides it.

[Updating](../start/updating.md) covers how to install a new version. This page covers what you are installing, how to check it is genuine, and how long it keeps getting fixes.

## What a release publishes

Assets hang off each tag on the [releases page](https://github.com/osauer/canary/releases).

| Asset | What it is |
|---|---|
| `canary-vX.Y.Z-<os>-<arch>.tar.gz` | Standard read-only build, one per platform. |
| `canary-trading-vX.Y.Z-<os>-<arch>.tar.gz` | Broker-write capable build, same platforms. |
| `canary-vX.Y.Z.mcpb` | Claude Desktop MCP Bundle for that version. |
| `canary.mcpb` | The same bundle bytes under a stable latest-download name. |
| `SHA256SUMS` | One SHA-256 line for every tarball and bundle above. |
| `SHA256SUMS.asc` | PGP detached signature over `SHA256SUMS`. |
| `SHA256SUMS.ed25519` | Compact Ed25519 signature over `SHA256SUMS` for current updaters. |

The platforms are `darwin-arm64`, `darwin-amd64`, `linux-amd64`, and `linux-arm64`. There is no Windows build, because the daemon uses `setsid`, `flock`, and AF_UNIX sockets. WSL works.

A standard tarball holds the `canary` binary, `LICENSE`, and `README.md`, and `install.sh` rejects an archive containing anything else. A trading tarball adds `TRADING-WARNING.md`, which links to the security policy and the order-preview guide at that exact tag.

A release that publishes an MCP Bundle also carries MCP Registry metadata recording that bundle's SHA-256 as `fileSha256`. The registry hash is a discovery-time integrity hint; signed `SHA256SUMS` remains the release-level trust anchor. [Packaging and distribution](../internals/packaging.md) describes the bundle and plugin formats.

## The standard build and the trading build

The standard read-only artifact uses the canonical `canary` filename. Getting the broker-write-capable variant takes a deliberate `canary-trading` download.

**Standard build.** Broker-write handlers are not compiled in. The retained
order, proposal, and opportunity reads remain available, while broker actions
fail closed before a wire write. The bundled MCP surface has no preview or
order-entry tools in either build.

**Trading build.** This binary can place, modify, cancel, or exercise only the
constrained actions retained by v3, once mode, route, account, freeze, journal,
and fresh review contracts all pass. Trading builds are experimental and
provided as-is. [Orders and the trading build](../operate/orders.md) has the
configuration and gates in full.

To see which one you are running, `canary settings` prints a `Build` row reading either `stable` or `experimental-trading`, followed by a build note.

The normal install paths land on the standard build:

- `install.sh` downloads `canary-<version>-<platform>.tar.gz` and nothing else.
- `canary update` matches that exact filename, so it cannot install a trading build over your binary.
- The MCP Bundles are assembled from the standard tarballs' binaries.
- `go install github.com/osauer/canary/v2/cmd/canary@latest` stays on the
  maintained v2 Go-module line and does not install product v3.

## Verifying a download

`install.sh` verifies the PGP signature. Current `canary update` binaries require the compact Ed25519 signature and never fall back to PGP. There is no override flag. The PGP path below remains the simplest manual check and keeps older installations able to update directly.

1. Import the signing key and check its fingerprint against the table below.

```sh
curl -fsSL https://github.com/osauer.gpg | gpg --import
gpg --fingerprint oliver.sauer@gmail.com
```

A fingerprint that does not match means you have the wrong key, and nothing after this step is worth doing. From a cloned repository at a commit you trust, `gpg --import internal/update/release-signing-key.asc` is the equivalent path.

2. Download the tarball for your platform plus both checksum files. Set `VERSION` to the tag you want and `PLAT` to your platform.

```sh
VERSION=vX.Y.Z
PLAT=darwin-arm64
BASE=https://github.com/osauer/canary/releases/download/$VERSION
curl -fLO $BASE/canary-$VERSION-$PLAT.tar.gz
curl -fLO $BASE/SHA256SUMS
curl -fLO $BASE/SHA256SUMS.asc
```

3. Verify the signature over `SHA256SUMS`, then the tarball's hash against it.

```sh
gpg --verify SHA256SUMS.asc SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing
```

On Linux, use `sha256sum` in place of `shasum -a 256`. The first command must report a good signature and the second must print `OK`. Either one failing means the release is corrupted or tampered with.

The order matters. Signing `SHA256SUMS` is what binds the hash list to a key an attacker does not have. Checking a hash on its own proves only that the tarball matches the list published beside it, and whoever could replace one could replace both.

### Release-signing keys

| | |
|---|---|
| Owner | Oliver Sauer (`oliver.sauer@gmail.com`) |
| Algorithm | Ed25519 |
| Fingerprint | `D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D` |
| Compatibility | embedded in legacy binaries; retained at `internal/update/release-signing-key.asc` for `install.sh` and manual verification |
| Also published at | `https://github.com/osauer.gpg` |

The current updater also embeds the dedicated compact-signature key at `internal/update/release-signing-key.ed25519.pem`. Its PKIX public-key SHA-256 is `c9a8685a83c8d8584c1469f6f03973943e439f4aa2485468ffcda5a5db8c5578`; the private key is held in the maintainer's macOS login Keychain rather than in a file.

Current binaries carry the compact public key and use it to check the next release with no network bootstrap step to interpose on. Legacy binaries carry the PGP key; release assets retain both signatures so those binaries can still update directly. Rotating the compact identity requires another staged transition.

This defends against an attacker who compromises the GitHub account and swaps the tarball and `SHA256SUMS` together: without the private key, the signature they can produce will not verify. It does not defend against theft of that private key, which is what the revocation certificate is for, nor against a compromised dependency at build time, which `govulncheck` covers separately.

The `.mcpb` container itself is not code-signed. Treat bundle integrity as checksum-based unless `mcpb verify canary-vX.Y.Z.mcpb` succeeds.

## Version numbers

Releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html), and every entry in [CHANGELOG.md](../../../CHANGELOG.md) uses Keep a Changelog categories.

- **Patch** (`v2.3.0` to `v2.3.1`): fixes and internal changes.
- **Minor** (`v2.2.1` to `v2.3.0`): new commands, tools, or fields, backward compatible.
- **Major**: a breaking change to a published contract.

Canary now has two related version contracts:

- Product releases version the signed binaries, MCPB, plugin, app, daemon state,
  CLI, and MCP surface. Product v3 is a major reduction of those surfaces.
- The public Go module remains `github.com/osauer/canary/v2`. A root `v3.0.0`
  product tag is not a valid `/v2` module version, so Go ignores it for
  `/v2/...@latest`; the newest maintained v2 tag remains the module and
  source-built CLI release. Move to `/v3` only if the public Go API itself takes
  another approved breaking change.

`canary version` prints the version, commit, build date, and runtime, plus a trust line saying whether the binary is a stamped release build or something compiled locally.

## Which versions are supported

Product v3 and later release from `main`. While v3 stabilizes, maintained v2
releases cut only from `release/2.x` and receive safety, security, data-loss,
and broker-compatibility fixes. Those fixes are forward-ported to `main`;
feature work does not land only on N-1.

Retiring v2 requires at least 90 days of overlap, one maintenance release,
proven v2-to-v3 migration and rollback, and a new explicit operator decision.
Time alone does not end support.

`canary update` selects the newest stable release on the installed product
major. It does not silently move a v2 installation to v3; development builds
follow the newest stable product line.

## Reporting a security problem

Report privately rather than opening a public issue. The channel is a draft advisory through [GitHub Private Vulnerability Reporting](https://github.com/osauer/canary/security/advisories/new). Plain English is fine, and a proof of concept is appreciated but not required. Without a GitHub account, open a normal issue titled `security: request private channel` carrying no details, and you will get a one-time reporting address back.

The policy commits to acknowledgement within 7 days, triage within 30 days for most reports, and a patched release before the advisory becomes public. Responses are best-effort.

**In scope**: the daemon, the CLI, the stdio MCP server, the Claude Code plugin, the `pkg/ibkr` wire-protocol implementation, `install.sh`, and the published release artifacts.

**Out of scope**: bugs in IBKR's own TWS and IB Gateway software, which go to IBKR directly; vulnerabilities in upstream Go modules, which go to the upstream maintainer and reach this project as a re-release once the fix lands; and denial of service against the local daemon by someone who already has a shell as your user.

Reports that break a stated property come first: a place, modify, cancel, or trade reaching the gateway from a default build, a daemon listener on anything other than its Unix socket, or data leaving the machine beyond what [PRIVACY.md](../../../PRIVACY.md) documents.
