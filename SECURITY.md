# Security policy

## Reporting a vulnerability

Please report security issues privately — not via public GitHub issues.

Open a draft advisory via GitHub Private Vulnerability Reporting at
<https://github.com/osauer/canary/security/advisories/new>. Plain English is
fine; a proof-of-concept is appreciated but not required. GitHub will email
the maintainer privately and let you correspond in the advisory thread
without exposing details until the fix ships.

No GitHub account? Open a regular issue titled `security: request private
channel` (no details), and the maintainer will reply with a one-time
reporting address.

## Response

- Acknowledged within 7 days.
- Investigated and triaged within 30 days for most reports.
- Verified issues get a patched release before the advisory is made public. You will be credited in the advisory and in `CHANGELOG.md` under `### Security`, unless you prefer to stay anonymous.

This is a personal open-source project, not a funded program — responses are best-effort, but reports are taken seriously.

## Supported versions

Only the latest minor release of the current stable line receives security fixes. An older minor may receive a best-effort backport for a critical issue when the patch is low-risk; otherwise upgrade to the latest release.

## Scope

**In scope** — the daemon, CLI, stdio MCP server, Claude Code plugin, the `pkg/ibkr` wire-protocol implementation, the install script, and the published release artifacts in this repository.

**Out of scope** — vulnerabilities in Interactive Brokers' TWS / IB Gateway software (please report those directly to IBKR), vulnerabilities in upstream Go modules (please notify the upstream maintainer; this project will re-release after the fix lands), and denial-of-service against the local daemon by a user who already has shell access on the same machine (the daemon is intended for single-user local use).

## Threat model

`canary` default builds are structurally no-broker-write: the order surface is limited to local preview/status reads, and the daemon cannot place, modify, cancel, or transmit broker orders unless built with the trading capability (see [README §Safety](README.md#safety)). The daemon listens only on a Unix-domain socket in the user's runtime directory, never on a TCP port. It speaks to a locally-running IB Gateway or TWS over loopback. The project sends no telemetry or account data to the maintainer, but configured features do make outbound requests: in particular, automatic earnings refresh can disclose held ticker symbols to Nasdaq, and optional remote access carries app traffic through the configured relay. [PRIVACY.md](PRIVACY.md#network-access) inventories the destinations, triggers, and disclosed data classes.

Reports that demonstrate a deviation from those properties — a successful place / modify / cancel / trade reaching the gateway, a daemon listener on a non-loopback or non-Unix socket, or undocumented or broader-than-documented data egress — take priority.

### Agent-origin write gating (trading builds)

Builds with the `trading` tag accept broker writes behind a layered gate
(see `internal-docs/design/agent-origin-gating.md`). Every write request carries an
origin for audit and any origin-specific policy; agent-origin paper and live
writes use the same structural gates as human writes: a submit-eligible
preview token per write, pinned gateway/account/mode checked against the
connected session, a trading-capable build, a writable local journal, broker
WhatIf/eligibility checks where applicable, and the runtime `trading.freeze`
switch. (The typed `live/<account>` confirmation and the config
acknowledgement keys were removed 2026-06-11 as same-file duplication that
only slowed live order entry.)

Honest limits of this interlock:

- It is a **safety interlock, not a security boundary**. Origin is asserted
  by the calling process; any same-uid process can forge it, call the daemon
  socket directly, or edit the config.
- **Cancel remains freeze-exempt** (refusing a cancel can strand a worse
  position), but note the asymmetry: cancelling a protective stop *removes*
  protection. Cancels journal their origin; a future tightening can restrict
  agent cancels to agent-placed orders.
- A paired PWA request carries the `human-paired-device` origin; the daemon
  cannot distinguish a human click from browser automation. Pairing scopes
  user-driven app access, not agent submit authority. Repo agents keep browser
  QA read-only and use the agent-origin gated CLI for an explicitly requested
  write. App writes still require the preview token and server-validated
  account/mode confirmation fields.
## Release integrity (v1.0.0+)

Every GitHub release from v1.0.0 onward ships signed checksums for the published install assets:

1. `canary-vX.Y.Z-<os>-<arch>.tar.gz` — the shell/manual binary tarballs.
2. `canary-vX.Y.Z.mcpb` — the versioned Claude Desktop MCP Bundle, when published.
3. `canary.mcpb` — stable latest-download alias for the same MCP Bundle bytes, when published.
4. `SHA256SUMS` — one line per tarball and MCPB asset with its SHA-256.
5. `SHA256SUMS.asc` — a PGP detached signature over `SHA256SUMS`, produced by the maintainer's release-signing key.

`install.sh` and `canary update` (from v1.0.0 onward) **refuse** any release that does not publish `SHA256SUMS.asc`, and refuse any release whose `SHA256SUMS.asc` does not verify against the maintainer's release-signing key. `install.sh` pins the key fingerprint during bootstrap; `canary update` verifies against the public key embedded in the running binary. There is no fallback path. A release whose signature cannot be checked is treated as a compromised release.

The MCP Registry publish metadata for an MCPB release also includes the versioned bundle URL and `fileSha256`. That registry hash is a discovery/install integrity hint; the signed `SHA256SUMS` file remains the release-level trust anchor.

The MCPB container itself is not yet code-signed. Do not treat the `.mcpb` file as signed unless `mcpb verify canary-vX.Y.Z.mcpb` succeeds. Signed checksums and the MCP Registry hash are the current integrity mechanisms for MCPB releases; self-signed bundles are not a trust upgrade.

### The maintainer's release-signing key

| | |
|---|---|
| Owner | Oliver Sauer (`oliver.sauer@gmail.com`) |
| Algorithm | Ed25519 |
| Fingerprint | `D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D` |
| Embedded in | every `canary` binary from v1.0.0 onward, at `internal/update/release-signing-key.asc` |
| Also published at | `https://github.com/osauer.gpg` (served by GitHub) |

### Verifying a release by hand

If you want to verify a downloaded tarball without trusting `canary update`:

```sh
# 1. Get the maintainer's key (one of two equivalent paths).
curl -fsSL https://github.com/osauer.gpg | gpg --import
# or, from a cloned repo at a trusted commit:
gpg --import internal/update/release-signing-key.asc

# 2. Confirm the fingerprint matches the line in this file.
gpg --fingerprint oliver.sauer@gmail.com

# 3. Download the release artefacts for your platform.
VERSION=v1.0.0
PLAT=darwin-arm64
BASE=https://github.com/osauer/canary/releases/download/$VERSION
curl -fLO $BASE/canary-$VERSION-$PLAT.tar.gz
curl -fLO $BASE/SHA256SUMS
curl -fLO $BASE/SHA256SUMS.asc

# 4. Verify the signature over SHA256SUMS, then the tarball SHA.
gpg --verify SHA256SUMS.asc SHA256SUMS
shasum -a 256 -c SHA256SUMS --ignore-missing
```

Both lines must end in `Good signature` and `OK` respectively. Either failing means the release is corrupted or tampered.

### What this does and doesn't defend against

**Defends against**: a GitHub account compromise that swaps both the tarball and `SHA256SUMS` — the attacker doesn't have the maintainer's private key, so the produced `SHA256SUMS.asc` won't verify. Also defends against MITM scenarios past github.com's TLS.

**Does not defend against**: theft of the maintainer's private key (handled via revocation — see below) and supply-chain attacks on the Go module graph at build time (separate from release integrity; tracked by `govulncheck` in `make check`).

### Key rotation and revocation

The signing key is long-lived (no expiration date) because it is embedded in every shipped binary. Rotation requires shipping a new Canary binary with the new public key embedded. A revocation certificate is held offline by the maintainer; if used, it will be published to keyservers and announced in `SECURITY.md`.

## Diagnostic data sensitivity

Two opt-in environment variables write the raw IBKR wire protocol to disk. Both are off by default and only active when explicitly set; neither is on the autospawn path. Captured frames include account IDs, contract identifiers (symbol / conid / strike / expiry), order references, P&L numbers, and execution details — anything the gateway sends. Treat the resulting files as account-sensitive.

| Variable | Effect |
|---|---|
| `IBKR_WIRE_INTERCEPTOR=1` | Activates the in-process wire recorder. Frames are mirrored into a per-process ring buffer (in-memory). |
| `IBKR_WIRE_LOG_PATH=/path/to/wire.jsonl` | When set with `IBKR_WIRE_INTERCEPTOR=1`, every frame is also appended to this file as one JSON object per line. The parent directory is created `0700` and the file `0600`. |
| `IBKR_WIRE_RING_SIZE=N` | Bound on the in-memory ring (default 256 frames). |
| `IBKR_PACKET_LOG_TEMPLATE=/path/to/packets.bin` | Independent low-level packet logger writing raw bytes (length-prefixed). Same sensitivity as the wire log; lower-level shape. |

If you share a wire log for debugging, redact the `Symbol`, `ReqID`, and `Fields` columns where they carry account-level data. The README's Troubleshooting section has the user-facing pointer; this entry is the security-side warning.
