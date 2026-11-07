# Releases and support

Updated: 2026-11-07 08:23 CET

Every release publishes two binaries per platform, under two different names. One is read-only. The other can send orders to your broker. That difference is compiled in rather than configured, so the filename you download decides it.

[Updating](../start/updating.md) covers how to install a new version. This page covers what you are installing, how to check it is genuine, and how long it keeps getting fixes.

The notes below cover each version whose installation changes a file on disk, what the first start does, or what runs on its own. The [changelog](https://github.com/osauer/canary/blob/main/CHANGELOG.md) has every other change.

## Moving to 3.34

If your protection policy lists nothing under `[authority].pre_authorised` or
`[cash].pre_authorised`, nothing changes. If it lists a bucket, Canary places
none of that bucket's orders on its own after the upgrade until a standing
authority is armed: a one-time confirmation, Protection or Fully armed, signed
on a paired device. Until then each order waits for your confirmation, and
`canary proposals status` lists the orders that are waiting. Only Canary Desk,
a private project, can sign that confirmation, so an install without Desk
confirms each order by hand. An order already at the broker stays there. After
a crash, the first start rebuilds a database it cannot verify rather than
reusing it, and trading authority stays intact.

## Moving to 3.33

The first start writes two tables into a risk policy (`risk-policy.toml`) that
already has a written `[mandate]`: `[mandate.regime_gate]` (`breadth_min_pct =
50.0`, `half_scale_pct = 50.0`, `stale_after_sessions = 1`, `confirm_closes =
2`) and `[mandate.reduction]` (`cheap_leg_give_pct_of_loss = 2.0`). It writes
each table once, keeps one you wrote, and raises `policy_version`. Rule 20 runs
in alert mode until your rulebook policy names `modes.earnings_premium_freeze`.
Strategy one's screen (`canary strategy-one`) sizes its trade plans by the
regime gate. No strategy that places orders reads the gate, so it changes no
order.

## Moving to 3.32

The database now stores each distinct statement row once. The first start
after the upgrade removes the duplicate copies and compacts `daemon.db`, so it
can take noticeably longer on a large database. Corrections and the original
reports are kept.

## Moving to 3.31

When `opportunity-policy.toml` has no `[add]` table, the first start writes the
call and put horizons and delta bands Add uses, at Canary's values. Edit them
there; an edit applies at the next reload. The daemon also keeps the database
log, `daemon.db-wal`, to 64 MiB after each reset.

## Moving to 3.30

The daemon deletes the policy backups it made more than 30 days ago, keeping
the newest five for each file. Backups you made yourself are never touched.
`canary app` deletes temporary files left by a save that a crash or power loss
cut off. A `[mandate.index_hedge]` table you deleted is no longer written back.

## Moving to 3.29

From 3.29.1, the first start writes `[mandate.iv_fallback]` (`short_band_pct =
65.0`, `month_band_pct = 15.0`, `long_band_pct = 8.0`, `measured_pct = 5.0`,
`range_pct = 20.0`) and `[mandate.index_hedge] max_contracts = 25` into a risk
policy that already has a written `[mandate]`, after a backup and with a higher
`policy_version`. A table you wrote is kept. The daemon also checks at each
start, and then daily, whether the account may trade futures, with a preview of
one Micro E-mini S&P 500 contract that sends no order.

## Moving to 3.28

The policy migration writes `option_name_act_pct = 15` into the rulebook
policy. While the risk policy carries a `[mandate]` table, a risk-adding option
buy that would take one name's premium at risk past 15% of net liquidation
value is refused. Installing does not arm automatic trading.

## Moving to 3.27

Canary writes the worst-price allowances for percentage reductions into the
risk policy's `[order_limits]` when the keys are missing, `reduce_allowance_pct` (1%
for stocks and ETFs) and `reduce_option_allowance_pct` (5% for options), and
raises `policy_version`. Limits you wrote are kept.

## Moving to 3.26

Nothing changes on upgrade. `canary status --json` gains a `gateway_setup`
object (`state`, `notes`, `as_of`); clients that ignore unknown fields need
no change. When the open-order read loses its answer, the daemon redials its
broker connection, at most once an hour and never while an order is being
placed or changed.

## Moving to 3.25

Nothing changes on upgrade until you write it. A `[mandate.strategies.<id>]`
entry written `stage = "live"` now serves its month's row, as the core's
does; a file that writes an entry live on a rule without rows, or two
strategies on one fund line, is refused and the last policy stays. While an
entry is live, a hand buy of its fund line is refused (sales stay open);
write the entry `suspended` to trade the line by hand. A standing authority
armed before you edit the register sends no strategy buys until you confirm it
again on your device; its sales and protection still go. The
scorecard's v1 fields announced to go in 3.25 (`mean_z`, `clusters`,
`p_ic_above_*`, `clusters_to_establish`) are still served in 3.25.

## Moving to 3.24

Nothing changes on upgrade until you write it. `[mandate.sleeves]
owner_stop_pct` is optional; absent, the owner sleeve, the part of the book
you trade yourself, has no stop of its own. Desk's request to send a strategy's
order (`desk.execution.submit`) is served only to a controller presenting a
standing authority you confirmed on your device; without one, a live
strategy's row waits for your confirmation. A queued authorisation, an order you confirm
ahead to be sent at the next open, now refuses the core's row, which is sent in session at
the live mid.

Clients that read the scorecard: the schema is `canary-scorecard-v2`. The
measure is the directional score (`directional_score`, `p_score_above_0`,
`units`, `effective_units`, `weeks`, `futility_e_value`, `placebo`,
`slippage`); `mean_z`, `clusters`, `p_ic_above_*` and `clusters_to_establish`
stay for one release and go in 3.25. `canary policy check` reads the
mandate's realised figures from a 3.24 daemon; against an older daemon it
names the missing field as the reason for the skip.

## Moving to 3.23

An absent `[mandate.core]` stays inactive, and an absent stage or
`stage = "shadow"` keeps the core in shadow. An existing `stage = "live"`
declaration now takes effect: earlier builds accepted it but limited the core
to shadow. On upgrade it reserves its configured allocation and turns each monthly decision into one
`core_rebalance` proposal row for your device confirmation, bound to the
core's contract and sized by the rule, the band, the minimum and the
mandate's headroom; the daemon never sends it by itself and the bucket cannot
be pre-authorised. A `[mandate.strategies.<id>]` entry names a fund, a signal
from the compiled library and your numbers, and the evaluator runs it in
shadow at most; `canary strategy ID` shows what it would trade. A register
entry may write `kill_drawdown_k`, the drawdown bound in units of sleeve
capital.

Clients that read `canary core --json` or `strategy.target`: the sizing's
allocation is named `allocation_pct` where `sizing.frozen_on` and
`sizing.current_on` said `core_vol_pct`.

## Moving to 3.22

Nothing changes on upgrade until you write it. The mandate (`[mandate]`), the
strategy register (`[mandate.core]`, `[mandate.strategies.<id>]`) and TWS
order control (`trading.tws_order_control.enabled`, which waits for a paper
proof) are optional; absent, the daemon measures nothing new and refuses
nothing new.

Two things change on their own. The option-chain recorder now runs without a
switch and records the option chains of held option underlyings once a
session, 30 minutes before the US close, inside the market-data line and
pacing limits; `chains.recorder.enabled=false` stops it. Every runtime
setting has a class the daemon enforces by the write's origin: the trading
freeze and TWS order control take a human terminal only, the owner's settings
a human terminal or the paired console, and an agent session may change only
the research chain list.

`cash_interest_rate_upper` and `cash_interest_valid_through` are retired; a
sweep policy that still writes them loads, is named by `canary policy check`,
and the broker's own rate from the Flex statement is used instead. Stops
Canary places from this release on are born in a broker one-cancels-all
group; a stop placed earlier is shrunk or cancelled before a sale of the same
line sends. Canary now polls the account's executions once a minute and keeps
a quote per fill in daemon.db; `canary reporting costs` reads them.

## Moving to 3.21

This release hardens compatibility with TWS / IB Gateway 10.50. Existing TWS
connections keep their tested protocol. The optional
`canary status --gateway-diagnostics` probe reads supported API settings on a
separate connection; unsupported settings remain unknown. No broker settings or
existing policies change on upgrade.

Order reads can now report `deactivated`. It is a nonterminal state that stays
visible, blocks competing orders and does not count as active protection. Clients
that exhaustively match lifecycle values should accept this new value.

Trading builds add owner-confirmed order queues and whole-position stock trailing
protection. Existing orders do not acquire a mandate on upgrade. Standard binaries
and MCP remain read-only; diagnostics are exposed through the CLI.

## Moving to 3.18

Review the [full changelog](https://github.com/osauer/canary/blob/main/CHANGELOG.md).
The per-order limits move from `config.toml` `[trading]` to the risk
constitution's [`[order_limits]`](../understand/policy.md#order-limits), and
the order-value cap now scales with net liquidation value between a floor and a
ceiling. `max_notional` becomes `max_order_floor_base`; the other three keys
keep their names. The `[trading]` keys still parse but no longer apply, and the
runtime `trading.limits.*` settings are retired. The cash sweep's `min_tranche`
is deprecated in favour of `min_order_notional`; a written one still raises
that currency's smallest buy until you delete it.

Policy files need no action on upgrade. From 3.18.1 the daemon migrates them at
its first start, after an owner-only backup of each file it changes.
`[order_limits]` takes your `max_notional` as the floor, `max_order_floor_base`
(10,000 if you never set it), with a 5% of net liquidation scale and a 100,000
base-currency ceiling, or the floor if that is higher. Your order cap can
therefore rise to 5% of net liquidation value, up to the ceiling; to return to
a fixed cap, set `max_order_ceiling_base` equal to `max_order_floor_base` and
raise `policy_version`.

An existing sweep table gains its sizing defaults with
`no_buy_while_borrowed = true` and
`bills_exempt_from_trading_max_notional = true`. The second lets a
same-currency bill order exceed the order cap, up to the sweep's own cap: the
larger of `max_order_notional` (written as 50,000) and `max_order_pct_nlv`
(written as 10%) of net liquidation value, with no ceiling. 3.18.1 also applies
the format conversions earlier releases left optional.

Installing does not change the files: `canary policy ensure --dry-run` shows
the change before the new daemon starts, and
[`canary policy check`](../understand/policy.md#check-a-policy-for-plausibility)
reads the result against the account afterwards. 3.18.0 needed
`canary policy ensure --apply-plan` for all of this; until it ran, protection
proposals read blocked and trading builds refused order previews.

In trading builds the protective stop guard shrinks or cancels stock and ETF
sell stops that Canary placed when the position falls below them; stops placed
by hand are reported and never changed. A proposal you prepared with
`canary proposals prepare` before the upgrade must be prepared again.

## Moving to 3.16

Version 3.16 adds [FX contribution](../start/reporting.md#fx-contribution) and
[earned stock-lending fees](../understand/edge.md#stock-lending-income).
Extend the existing Flex query with the documented sections to supply the
needed evidence. Missing sections and partial periods remain explicit; lending
payment and P/L linkage still need account-specific broker commissioning.

Cash sweeps now require broker-observed settled cash per currency. Account-wide
figures and diagnostic Flex estimates cannot authorise a sweep. An optional
[Web API ledger](../operate/cash-ledger.md) can read an existing session.
Whole-order minimums, fixed-quantity fee bounds and net sale proceeds are shown
separately from advisory purchase value. `min_net_gain` remains readable but
is advisory; missing cash-interest assumptions no longer block an order.

Existing private policy settings are unchanged. The sweep stays off by default;
review the [sweep controls](../operate/cash.md#cash-sweep) and commission
each bill route before activation. Installation grants no broker-write authority.

## Moving to 3.15

Review the [full changelog](https://github.com/osauer/canary/blob/main/CHANGELOG.md).
Rule 3 now measures long option premium at risk as a share of net liquidation
value instead of available funds, and rule 15's net exposure bands move into
the regime tables. A policy file that still sets `cash_reserve_min_pct` or the
top-level `net_exposure_watch_pct` and `net_exposure_act_pct` loads with a
note; `canary rules policy reset KEY` removes each one, and the regime tables
carry the new keys at their defaults. Rule 19, margin headroom, starts in alert
mode. Check `canary rules policy` for the values in force.

The cash sweep is absent and off by default. A sweep order needs your approval,
or a pre-authorisation that keeps the full veto window. At a confirmed-stress
open, budget, theta and issuer-trim option reductions wait 30 minutes after the
open; stops and loss exits keep 15.

## Moving to 3.14

Review the [full changelog](https://github.com/osauer/canary/blob/main/CHANGELOG.md).
Nothing converts at upgrade. With directional-option exits enabled
(`[buckets.trailing_stop.options] enabled`), the loss exit now keeps working
below `min_dte` until expiry, and an in-the-money long option is proposed for a
DAY limit close from the Rulebook's expiry act level. Leave the bucket off, or
review it, if that is not what you want.

Proposals that would sell the same contract now appear as one row, and
reductions wait while a same-side order for that contract is working. A queued
authorisation needs a Desk and companion that know its terms; restart
consuming clients after upgrading Canary. A queued send passes the same account
pins, journal, daemon authorisation and trading freeze as any other send.

Optional `[gateway] restart_time` and `restart_timezone` let status tell a
declared daily Gateway restart from an unexpected outage. They are diagnostic
only and never pause reconnection.

## Moving to 3.13

Review the [full changelog](https://github.com/osauer/canary/blob/main/CHANGELOG.md).
Rule 1 now measures modelled loss across an issuer's positions, with hedge and
liquidity treatment. Rules 16–18 add delta swings, cluster stress and loss
budgets. Rules 16 and 17 start in tracking mode; rule 18 starts in alert mode.
Check `canary rules policy` for the values and modes actually in force. Existing
automatic permissions are retained; changed risk calculations can change the
proposals produced by already-enabled workflows.

Existing policy files remain readable and are not rewritten at startup. A v5
constitution receives the reminder fix without conversion. New templates use
`canary.*` kinds, and constitution/opportunity format 2 separates file semantics
from owner revisions and makes working opportunity settings explicit. See
[policy identity and permission](../understand/policy.md#identity-revisions-and-permission).

Install the compatible reader first. To review an optional conversion, save
`canary policy ensure --dry-run --json` locally, inspect its exact changes, then
use `canary policy ensure --apply-plan FILE` for the files you approve. The plan
can contain private policy values. Application checks the reviewed input/output
hashes, backs up originals and preserves effective settings; changed files need
a new preview. This process does not choose risk limits or trading grants.

Proven existing actions retain their review, veto window and consumed state.
An old action without enough evidence to prove identity continuity requires
fresh review; an unproven automatic revision follows the existing notice and
veto rules. Preserve daemon/app state and notification receipts. Each alert
source establishes its own baseline without pushing a backlog of old conditions.

After upgrading Canary, restart consuming clients such as Desk. A Go client
also needs the matching published dependency when it consumes new fields.
Service health, broker readiness and device notification receipts are separate
checks; a successful installation does not prove a trade or a displayed banner.

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
order, proposal, and opportunity reads remain available, and so do previews:
a preview runs the broker WhatIf, which creates no order, and mints a token
this build cannot submit. Broker actions fail closed before a wire write. The bundled MCP surface has no preview or
order-entry tools in either build.

**Trading build.** This binary can place, modify, cancel, or exercise only the
constrained actions retained by v3, once mode, route, account, freeze, journal,
and fresh review contracts all pass. Trading builds are experimental and
provided as-is. [Gated orders and the trading build](../operate/orders.md) has the
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

Releases follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Each entry in [CHANGELOG.md](../../../CHANGELOG.md) opens with a short "What's new" summary, followed by Keep a Changelog categories such as Added, Changed and Fixed.

- **Patch** (`v3.34.0` to `v3.34.1`): fixes and internal changes.
- **Minor** (`v3.33.1` to `v3.34.0`): new commands, tools, or fields, backward compatible.
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

Product v3 and later release from `main`. While v3 stabilises, maintained v2
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
