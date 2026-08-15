# Gated orders and the trading build

Updated: 2026-08-14

The standard `canary` binary is read-only and compiles in no broker-write path.
The separate opt-in trading binary exposes these actions:

- preview, place, or modify a single-leg stock/ETF or option order through the
  tokenized draft path (`canary order preview` → `place`/`modify`);
- submit or reduce a daemon-owned close/reduce protection proposal;
- exercise an eligible held option when the action reduces or closes risk;
- close or reduce a grouped option strategy as one combo; and
- cancel a Canary-owned order.

`order preview` mints a signed draft token and runs the broker WhatIf; it never
transmits, and a minted token is not submit authority. `place` and `modify`
consume a submit-eligible token for that exact draft and pass the same daemon
admission gates as every other write. A proposal or opportunity preview is
candidate-specific evidence and never submit authority. The MCP server has no
preview or execution tools.

## Required authority

Trading configuration must pin `[gateway].port`, `[gateway].account`,
`[gateway].client_id`, and `[trading].mode` to `paper` or `live`. A missing or
disabled mode means no order entry. The connected account and endpoint must
match those pins in paper and live sessions.

Every broker action additionally keeps its exact candidate revision, fresh
confirmation or preflight contract, quantity and exposure limits, journal
health, daemon authorization, origin policy, and `trading.freeze` gate. An
alert, plan, proposal, preview, prior instruction, or write-ready status is
evidence, not authority for a new transaction.

Keep an inactive example at `~/.config/ibkr/config.toml.trading`; the daemon does
not load it until the `.trading` suffix is removed. Before activating it, verify
the pins and start with a paper session. `canary trading status` reports the
current boundary but cannot authorize a trade.

## Protection and exercise context

Protection proposals carry typed market-event source health and candidate
blockers. Active regulatory/news halts and LULD pauses block action. Borrow
inventory and fee flags can strengthen cover context but cannot create a new
long sell or buy-add idea. Reg SHO membership is context unless an existing
reduce/cover candidate supplies the action authority.

Option exercise is limited to daemon-owned candidates for held options. The
confirmation surface must disclose the resulting underlying exposure and block
an exercise that opens, increases, or flips risk.

## Release boundary

Each release publishes standard and `canary-trading-*` artifacts side by side.
The installer and updater select the standard artifact unless the user
deliberately installs the trading tarball. Product v3 release publication is
hermetic: it performs no broker preview or write. Gateway behavior is verified
separately by the authorized read-only smoke target.

MCP remains read-only in every build. Adding an MCP broker action would require
a separate authority, nonce, audit, confirmation, and adversarial review; no
current configuration turns one on.
