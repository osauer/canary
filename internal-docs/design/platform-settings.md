# Platform Settings

Platform settings are the narrow writable preference surface for runtime UX
choices. They are not a second config system.

## Ownership

The daemon stores runtime preferences as a versioned, compare-and-swap state
document in `$XDG_STATE_HOME/ibkr/daemon.db` (or the corresponding fallback
state root). It does not read, mirror, or fall back to
`platform-settings.json` after SQLite authority attaches. Only user
preferences owned by Canary belong in this document: feature toggles, the
`trading.freeze` brake, rulebook earnings overrides, the regime/stress
forward-collection switches. The trading-limit overrides
(`trading.limits.*`) are retired since 2026-10-05 20:28 CEST: the order limits
are the risk constitution's `[order_limits]` (see [Risk constitution](risk-policy.md#order-limits-owner-decision-2026-10-05-1956-cest)).
A stored override still decodes so an old document loads and is never read;
setting one is refused with a pointer to the one-shot policy override.

Reviewed exact-contract terminal earnings evidence is not a preference and is
not writable through `settings.update`. Its optional
`[rulebook].terminal_evidence_file` is a private startup import into a separate
typed daemon.db state document; rule snapshots serve only the committed SQLite
revision. Ownership, validation, revocation, and expiry semantics live in
the [Trading Rulebook](trading-rulebook.md).

This document owns semantics and ownership, not the key list. The writable
keys, types, and per-key descriptions are enumerated in the generated
[configuration reference](../reference/config.md) (single source: the settings
key registry in `internal/rpc`); `canary settings set --help` prints the same
list.

TOML/config/build still own gateway endpoint, account, client ID, trading
enablement, trading mode, and whether write-capable trading code exists. MCP
broker writes are not exposed as a local setting.

Cutover imports the validated settings document exactly, including
`trading.freeze` and any limit overrides. Trading readiness is a separate
safety authority and remains unavailable until startup completes.

The owner [watchlist](watchlist.md) is a separate typed preference document in
the same daemon.db authority. Its CAS/request receipts preserve intentional empty
lists and current-state retry semantics; it is not a broker or risk control.

## Contract

Every returned setting field carries:

- `access`: `read` or `write`
- `source`: `runtime`, `config`, `build`, or `observed`
- `reason` when a read-only value needs operator context

`settings.update` and `PATCH /api/settings` accept only writable fields. Unknown
fields and read-only writes return 400. `null` clears a runtime override and
reveals the underlying default/config value again.

An accepted semantic update commits the SQLite state document and its typed
`platform_settings_update` audit event in one compare-and-swap transaction
before the daemon publishes the new in-memory/settings-event view. The audit
payload carries sorted changed keys, canonical before/after values, expected
and new revisions, old/new trading-control generations, and the normalized
write origin. A semantic no-op advances neither the revision nor the audit
stream. Revision conflicts, duplicate audit identities, or critical database
errors roll back both records; no legacy-file write or fallback follows.

## Cash sweep ordering

`cash_sweep.currency_priority` is a runtime ordering preference: `usd_first`,
`balanced`, `eur_first`, or `null` to restore the protection policy/default.
It affects only the final currency ordering and priority ranks. It never writes
`protection.toml`, opts into its reserve design, changes the cushion, funding
checks, floors, caps, policy fingerprint, trading-control generation or AUTO.
The legacy TOML priority keeps its reserve opt-in semantics. USD first is the
ordering default when neither source supplies a preference. Existing native cash
is used; the preference authorizes no FX conversion or broker action.

The settings document is version 4. Version 3 is strictly decoded and upgraded
in memory without an eager database rewrite; its controls survive the next write.

The dedicated non-catalogue `settings.cash_sweep.get` and
`settings.cash_sweep.set_priority` RPCs and Go client methods expose only this
preference. A save requires `currency_priority`, `expected_revision` and an
immutable `request_id`. Canary always audits this narrow client's origin as
agent; it claims no human-terminal authority. Each accepted request records its
canonical terms, digest, before value and saved revision in the existing
append-only event table. Changed settings and the receipt commit together.
A semantic no-op records a receipt under a revision fence without rewriting
state. Reusing an ID with changed terms fails. Retrying an old ID returns its
saved revision plus the current settings; it never reapplies the old choice.
Cash/funding readiness remains a separate proposal observation.

## Cash policy settings

Added 2026-10-06 14:52 CEST. `policy.cash.get`, `policy.cash.check` and
`policy.cash.apply` edit the protection policy file's `[cash.leveling]` and
`[cash.sweep]` keys for Desk's Settings → Cash management
(docs/docs/operate/cash.md, "Changing these from Desk"). Like the priority
RPCs they are outside every catalogue: not in MCP, the CLI or an agent grant.
Get returns each key in scope with its value, source (`file`,
`canary_default`, `not_written`), written default, bounds, help and facts; the
revision is the hash of the file's bytes. Its findings are `canary policy
check`'s findings over the file (over the draft, for check) for the rules that
name a cash key; a rule whose CLI text names file keys or cents writes the
sentence the screen shows beside it (`screen` on the finding, in the screen's
labels and whole units), and the snapshot carries that sentence in place of
the CLI text (2026-10-07). Check runs the loader's validation
on the draft and returns canonical terms and their digest; it writes nothing.
Apply takes those terms with a confirmation reference (Desk action id,
credential, envelope; audited, not verifiable here), audits its origin as
agent and refuses every other origin. A reference may rely on an earlier save
instead of the device (`confirmed_by`; owner decision 2026-10-06 15:31 CEST):
Canary accepts it only for a save with no consequence at the broker, only on a
save it recorded with a fresh confirmation by the same credential, and only
inside the window it works out itself: that receipt's time plus `[cash]
confirmation_window` in force now (file-only, written as `"10m"` by the
startup migration; `"0s"` refuses every reliance). Desk binds the reliance to
its console session; the apply result returns the window's end
(`confirmed_until`). The receipt and the provenance comment name the save
relied on. Under the protection policy manager's
file lock it rechecks the receipt, the revision and the validation, writes
only the changed lines with their provenance after a backup, raises
`policy_version` by one, reloads, and records a `cash_policy_saved` receipt in
the event table. A retried request id with the same digest writes nothing; with
other terms it fails `request_reused`. The receipt follows the write: when it
cannot be recorded the call fails, and the file's provenance still names the
save.

## Policy

`display.date_format` is presentation-only and defaults to `us`. The closed
values are `us`, `eu`, `us_weekday`, and `eu_weekday`; the weekday variants
spell out the session day. The SPA applies the preference to absolute calendar
dates across every tab while preserving relative freshness labels. The stored
value never rewrites a typed timestamp, changes a timezone, or participates in
market-session, risk, alert, or trading decisions.

Stock/ETF protection proposals are enabled by default. Disabling
`features.stock_protection.enabled` blocks stock/ETF protection proposal actions
with a `stock_protection_disabled` blocker, while proposal/status surfaces
remain readable. The setting cannot enable broker writes, option protection, or
policy-disabled buckets.

The advisory trading rulebook is enabled by default. Disabling
`features.rulebook.enabled` hides the SPA card, empties `rules.snapshot`, and
stops advisory `rule_*` preview warnings; it cannot affect broker-write gating
in either direction. `features.rulebook.earnings_overrides` is authoritative
over fetched earnings dates for rules 6-8. Override patches merge per symbol:
a null symbol value clears that symbol, null on the whole map clears all, and
unmentioned symbols survive.

`trading.freeze` is the runtime trading brake: `true` blocks every new broker
write while cancels stay allowed. Freeze changes are
human-only policy in disabled, paper, and live modes: missing, agent, or paired
device origins are rejected, and accepted human-terminal origins are stamped
in the atomic audit event.

`regime.journal.enabled` retains its public name but controls forward
collection of typed regime-decision events in `daemon.db`. It does not enable
a JSONL writer.

`stress.journal.enabled` similarly controls typed stress-decision events in
`daemon.db`. It defaults to `true`; disabling it stops future collection
without deleting existing evidence.

`history.rotation.enabled` and `history.rotation.keep_raw_months` are retired.
There are no live decision JSONL files or rotation worker after cutover.
Compatibility fields may remain in the typed response while clients migrate,
but they are not writable preferences and have no runtime effect.

Trading mode is never writable here. Stable builds expose trading and limits as
read-only. Experimental trading builds may edit safety limits only after
`[trading].mode` is set to `paper` or `live` in TOML.

Market-data settings never store subscription entitlements. The settings surface
shows a compact observed-quality summary from live quote/status data; row-level
truth remains on quote, chain, position, and status responses.

## Surfaces

- Daemon RPC: `settings.get`, `settings.update`; both must work without gateway
  connectivity.
- CLI: `canary settings show [--json]` and
  `canary settings set <key>=<value>` for the writable keys above, e.g.
  `features.stock_protection.enabled=true|false|null` or
  `features.rulebook.earnings_overrides.<SYMBOL>=YYYY-MM-DD|null`.
  `canary settings set --help` is the authoritative key list.
- HTTP/app: `GET /api/settings`, `PATCH /api/settings`, `/api/bootstrap`, live
  snapshot, and SSE `settings` events.
- MCP: read-only `canary_settings`; no write tool in V1.
- SPA: Settings tab renders this contract directly and honors `access` before
  enabling controls.

When changing this surface, update daemon permissions/tests first, then adapters,
then docs. Run focused settings tests before `make check && make smoke`.
