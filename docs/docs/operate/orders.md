# Order previews and the trading build

Updated: 2026-07-25 09:22 CEST

Stable `canary` is read-only. It can read account and market data, compute risk context, size positions, and preview stock/ETF LMT order drafts without broker submission. It does not place, modify, cancel, or transmit broker orders.

Trading builds are a separate experimental path. They are provided as-is for explicit operator testing, not as the default product and not as an unattended automation surface.

For the fields a preview returns, see [Working with agents](agents.md).

## Read-only default

Read-only use needs no config file. With no `config.toml`, the daemon probes the standard local TWS and IB Gateway ports, auto-detects the account from `managedAccounts`, and uses client ID `15`.

Only create `~/.config/ibkr/config.toml` when you want active local overrides. Any value in that file is binding. Anything omitted stays auto-detected.

## Inactive trading config

Keep trading configuration inactive as:

```text
~/.config/ibkr/config.toml.trading
```

That file is documentation and staging only. The daemon does not load it.

To activate it, remove the `.trading` suffix:

```sh
mv ~/.config/ibkr/config.toml.trading ~/.config/ibkr/config.toml
canary restart
canary trading status
```

Before doing that, verify the pinned account, endpoint, and client ID. Trading config must not rely on account auto-detection.

## Required pins

When trading is enabled, the daemon expects these values to be pinned:

- `[gateway].port`
- `[gateway].account`
- `[gateway].client_id`
- `[trading].mode = "paper"` or `"live"`; absent or `"disabled"` means no order entry

Placing, modifying, and cancelling an order each require a submit-eligible preview token. That is an invariant, not a config switch. Purge and restore are the one surface outside it: they never mint a preview token, and their broker submission is disabled outright today, so the only way to run a purge is manually in TWS.

Paper mode should use a paper endpoint or account, such as TWS paper on `7497` or a `DU...` account.

Live mode requires a live-looking endpoint and account, port `4001`/`7496` with a non-`DU` account. The pinned account must match what the connected TWS session advertises, and that check runs in paper mode too, not only live. That is the whole config gate. Pin the account and endpoint deliberately for the session in front of you.

The former `allow_live` / `live_ack_account` / `live_ack_endpoint` keys were removed 2026-06-11 because they restated the gateway pins from the same file. They now fail config load with a targeted error if left in place.

Paper-smoke evidence is reported in trading status as context but does not gate live mode. The release pipeline enforces the smoke instead: `make release` runs it at version bump and aborts on failure.

## Protection market flags

Protection proposals consume daemon market-event flags as context and safety gates. The proposal snapshot carries `source_fingerprints.market_events`, a top-level `market_events` snapshot, proposal `market_flags`, and `counts.market_flags` so UI and agents can tell when active flag changes require proposal revalidation.

Active `halt_regulatory_or_news` and active `luld_pause` are hard blockers for preview/submit. Recent halt/LULD flags remain visible as warning tags and should be paired with fresh quote context before acting.

Borrow flags are modifier-only. `borrow_inventory_tight` and `borrow_fee_extreme` can strengthen context for a proposal that buys to cover an existing short, but they must not create standalone long sells or buy-add proposals. `reg_sho_threshold` is regulatory context unless paired with an existing reduce/cover proposal.

User-facing proposal copy should render any reducing short `BUY` as `Buy to cover`. Market-event squeeze-like context remains observational in Underlyings; the separate Opportunities panel is limited to daemon-calculated option-exercise opportunities and does not create buy-add or buy-to-open recommendations.

## Release channel

Every release publishes both builds side by side. The standard artifact keeps the plain `canary-vX.Y.Z-<os>-<arch>.tar.gz` name and is read-only; the broker-write build is named `canary-trading-...` and carries a `TRADING-WARNING.md`.

`canary update` and `install.sh` match the standard filename exactly, so neither can move you onto a trading build, whatever else is attached to the release. Getting one is a deliberate download.

MCP order writes stay out of both builds. There are no order-entry tools on the MCP surface, and adding any would need its own review, nonce, audit, and human-confirmation model first.
