# Releases and support

Updated: 2026-07-25 11:34 CEST

Every release publishes two binaries per platform, under two different names. One is read-only. The other can send orders to your broker. That difference is compiled in rather than configured, so the filename you download decides it.

[Updating](../start/updating.md) covers how to install a new version. This page covers what you are installing, how to check it is genuine, and how long it keeps getting fixes.

## What a release publishes

Assets hang off each tag on the [releases page](https://github.com/osauer/ibkr/releases).

| Asset | What it is |
|---|---|
| `ibkr-vX.Y.Z-<os>-<arch>.tar.gz` | Standard read-only build, one per platform. |
| `ibkr-trading-vX.Y.Z-<os>-<arch>.tar.gz` | Broker-write capable build, same platforms. |
| `ibkr-vX.Y.Z.mcpb` | Claude Desktop MCP Bundle for that version. |
| `ibkr.mcpb` | The same bundle bytes under a stable latest-download name. |
| `SHA256SUMS` | One SHA-256 line for every tarball and bundle above. |
| `SHA256SUMS.asc` | PGP detached signature over `SHA256SUMS`. |

The platforms are `darwin-arm64`, `darwin-amd64`, `linux-amd64`, and `linux-arm64`. There is no Windows build, because the daemon uses `setsid`, `flock`, and AF_UNIX sockets. WSL works.

A standard tarball holds the `ibkr` binary, `LICENSE`, and `README.md`, and `install.sh` rejects an archive containing anything else. A trading tarball adds `TRADING-WARNING.md`, which links to the security policy and the order-preview guide at that exact tag.

A release that publishes an MCP Bundle also carries MCP Registry metadata recording that bundle's SHA-256 as `fileSha256`. The registry hash is a discovery-time integrity hint; signed `SHA256SUMS` remains the release-level trust anchor. [Packaging and distribution](../internals/packaging.md) describes the bundle and plugin formats.

## The standard build and the trading build

The read-only artifact keeps the historical filename, so old links and `install.sh` keep resolving to the safe variant. Getting the other one takes a deliberate download.

**Standard build.** Order transmission is not compiled in. The CLI keeps `ibkr order place`, `ibkr order modify`, and `ibkr order cancel` in every build; in a standard build the daemon's write handlers are compiled out behind the `trading` build tag and return `ErrTradingDisabled`, so the verbs exist and fail closed before anything reaches the broker. The bundled MCP surface has no order-entry tools in either build.

**Trading build.** This binary can place, modify, and cancel orders once `[trading].mode` and the pinned gateway endpoint and account are set and cross-checked against the connected session. Every write still needs a submit-eligible preview token, which is an invariant rather than a switch. Purge and restore sit outside that gate: they never mint a token, and their broker submission is disabled outright. Trading builds are experimental and provided as-is. [Order previews and the trading build](../operate/orders.md) has the configuration and the gates in full.

To see which one you are running, `ibkr settings` prints a `Build` row reading either `stable` or `experimental-trading`, followed by a build note.

Four install paths all land on the standard build:

- `install.sh` downloads `ibkr-<version>-<platform>.tar.gz` and nothing else.
- `ibkr update` matches that exact filename, so it cannot install a trading build over your binary.
- The MCP Bundles are assembled from the standard tarballs' binaries.
- `go install github.com/osauer/ibkr/v2/cmd/ibkr@vX.Y.Z` compiles without the tag.

## Verifying a download

`install.sh` and `ibkr update` verify signature and checksum for you, and from v1.0.0 onward both refuse a release whose `SHA256SUMS.asc` is missing or does not verify. There is no override flag. Verify by hand when you took a tarball straight off the releases page.

1. Import the signing key and check its fingerprint against the table below.

```sh
curl -fsSL https://github.com/osauer.gpg | gpg --import
gpg --fingerprint oliver.sauer@gmail.com
```

A fingerprint that does not match means you have the wrong key, and nothing after this step is worth doing. From a cloned repository at a commit you trust, `gpg --import internal/update/release-signing-key.asc` is the equivalent path.

2. Download the tarball for your platform plus both checksum files. Set `VERSION` to the tag you want and `PLAT` to your platform.

```sh
VERSION=v2.3.1
PLAT=darwin-arm64
BASE=https://github.com/osauer/ibkr/releases/download/$VERSION
curl -fLO $BASE/ibkr-$VERSION-$PLAT.tar.gz
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

### The release-signing key

| | |
|---|---|
| Owner | Oliver Sauer (`oliver.sauer@gmail.com`) |
| Algorithm | Ed25519 |
| Fingerprint | `D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D` |
| Embedded in | every `ibkr` binary from v1.0.0 onward, at `internal/update/release-signing-key.asc` |
| Also published at | `https://github.com/osauer.gpg` |

Because the public key ships inside the binary you already run, `ibkr update` checks the next release against a key it carries, with no network bootstrap step to interpose on. The key has no expiry, since rotating it means shipping a new binary with a new key embedded. A revocation certificate is held offline.

This defends against an attacker who compromises the GitHub account and swaps the tarball and `SHA256SUMS` together: without the private key, the signature they can produce will not verify. It does not defend against theft of that private key, which is what the revocation certificate is for, nor against a compromised dependency at build time, which `govulncheck` covers separately.

The `.mcpb` container itself is not code-signed. Treat bundle integrity as checksum-based unless `mcpb verify ibkr-vX.Y.Z.mcpb` succeeds.

## Version numbers

Releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html), and every entry in [CHANGELOG.md](../../../CHANGELOG.md) uses Keep a Changelog categories.

- **Patch** (`v2.3.0` to `v2.3.1`): fixes and internal changes.
- **Minor** (`v2.2.1` to `v2.3.0`): new commands, tools, or fields, backward compatible.
- **Major**: a breaking change to a published contract.

So far, "published contract" has meant the Go library. v2.0.0, the only major bump since v1.0.0, modernized the `pkg/ibkr` API and moved the module path to `github.com/osauer/ibkr/v2`. Users of the binary, the MCP Bundle, and the plugin were unaffected. Go developers importing the library had to update their imports.

`ibkr version` prints the version, commit, build date, and runtime, plus a trust line saying whether the binary is a stamped release build or something compiled locally.

## Which versions are supported

One person maintains this. There is no funded support program, no long-term-support line, and no maintenance branch: every tag sits on `main`, and fixes go into the next release rather than back into an older one. In practice the latest release is the supported one.

The written policy in [SECURITY.md](../../../SECURITY.md) says the same thing about security specifically: only the latest minor release receives security fixes. An older minor may get a best-effort backport for a critical issue when the patch is low-risk. Otherwise the answer is to upgrade.

To find out where you stand, `ibkr update --check` prints your installed version beside the latest published one and exits 0 either way, installing nothing.

## Reporting a security problem

Report privately rather than opening a public issue. The channel is a draft advisory through [GitHub Private Vulnerability Reporting](https://github.com/osauer/ibkr/security/advisories/new). Plain English is fine, and a proof of concept is appreciated but not required. Without a GitHub account, open a normal issue titled `security: request private channel` carrying no details, and you will get a one-time reporting address back.

The policy commits to acknowledgement within 7 days, triage within 30 days for most reports, and a patched release before the advisory becomes public. Responses are best-effort.

**In scope**: the daemon, the CLI, the stdio MCP server, the Claude Code plugin, the `pkg/ibkr` wire-protocol implementation, `install.sh`, and the published release artifacts.

**Out of scope**: bugs in IBKR's own TWS and IB Gateway software, which go to IBKR directly; vulnerabilities in upstream Go modules, which go to the upstream maintainer and reach this project as a re-release once the fix lands; and denial of service against the local daemon by someone who already has a shell as your user.

Reports that break a stated property come first: a place, modify, cancel, or trade reaching the gateway from a default build, a daemon listener on anything other than its Unix socket, or data leaving the machine beyond what [PRIVACY.md](../../../PRIVACY.md) documents.
