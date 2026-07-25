# Platform Settings

Platform settings are the narrow writable preference surface for runtime UX
choices. They are not a second config system.

## Ownership

The daemon stores runtime preferences as a versioned, compare-and-swap state
document in `$XDG_STATE_HOME/ibkr/daemon.db` (or the corresponding fallback
state root). It does not read, mirror, or fall back to
`platform-settings.json` after SQLite authority attaches. Only user
preferences owned by `ibkr` belong in this document: feature toggles, the
`trading.freeze` brake, rulebook earnings overrides, the regime/canary
forward-collection switches, and optional experimental trading-limit
overrides.

Reviewed exact-contract terminal earnings evidence is not a preference and is
not writable through `settings.update`. Its optional
`[rulebook].terminal_evidence_file` is a private startup import into a separate
typed daemon.db state document; rule snapshots serve only the committed SQLite
revision. Ownership, validation, revocation, and expiry semantics live in
the [Trading Rulebook](trading-rulebook.md).

This document owns semantics and ownership, not the key list. The writable
keys, types, and per-key descriptions are enumerated in the generated
[configuration reference](../reference/config.md) (single source: the settings
key registry in `internal/rpc`); `ibkr settings set --help` prints the same
list.

TOML/config/build still own gateway endpoint, account, client ID, trading
enablement, trading mode, and whether write-capable trading code exists. MCP
broker writes are not exposed as a local setting.

Cutover imports the validated settings document exactly, including
`trading.freeze` and any limit overrides. Trading readiness is a separate
safety proof and resets until a new paper-smoke artifact is issued.

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

## Policy

The purge/restore workflow/read surface is enabled by default. Disabling
`features.purge_restore.enabled` disables its workflow controls while
`purge.status` remains readable. Enabling it is not broker-write authority:
purge, restore preview, and restore submission remain unconditionally
unavailable until exact per-leg portfolio and account-global working-order
authority exist. Use TWS for a manual exit/restore, then refresh and reconcile
the daemon.

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
write while cancels stay allowed. Freeze and trading-limit changes are
human-only policy in disabled, paper, and live modes: missing, agent, or paired
device origins are rejected, and accepted human-terminal origins are stamped
in the atomic audit event.

`regime.journal.enabled` retains its public name but controls forward
collection of typed regime-decision events in `daemon.db`. It does not enable
a JSONL writer.

`canary.journal.enabled` similarly controls typed canary-decision events in
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
- CLI: `ibkr settings show [--json]` and
  `ibkr settings set <key>=<value>` for the writable keys above, e.g.
  `features.purge_restore.enabled=true|false|null` or
  `features.rulebook.earnings_overrides.<SYMBOL>=YYYY-MM-DD|null`.
  `ibkr settings set --help` is the authoritative key list.
- HTTP/app: `GET /api/settings`, `PATCH /api/settings`, `/api/bootstrap`, live
  snapshot, and SSE `settings` events.
- MCP: read-only `ibkr_settings`; no write tool in V1.
- SPA: Settings tab renders this contract directly and honors `access` before
  enabling controls.

When changing this surface, update daemon permissions/tests first, then adapters,
then docs. Run focused settings tests before `make check && make smoke`.
