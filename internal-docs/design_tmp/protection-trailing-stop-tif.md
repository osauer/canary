# Policy-driven TIF for protective trailing stops

Updated: 2026-06-10 11:54 CEST
Status: implemented

Contract per `docs/templates/daemon-cli-trading-contract.md`.

## Scope

- **Goal:** the trailing-stop protection bucket can place GTC stops instead of
  DAY. A DAY protective stop silently dies at every session close (17:30 CEST
  for Xetra names) — absent exactly when the overnight gap it exists to cover
  opens up. Default stays DAY; GTC is an explicit policy decision.
- **User-facing command/tool/API:** protection-policy TOML key
  `[buckets.trailing_stop] tif = "DAY"|"GTC"`; `ibkr proposals …` (proposal
  TIF + Details caveat); `ibkr order preview --tif GTC` (TRAIL/TRAIL LIMIT
  only); MCP `ibkr_order_preview` `tif` enum.
- **Owner layer:** the policy file owns the choice; the proposal engine
  threads it; preview and the protobuf wire validator enforce the
  GTC-implies-trail rule; CLI/MCP only describe it.
- **Existing behavior:** TIF was pinned to DAY at five layers (MCP enum, CLI
  help, daemon preview, proposal preview params + drift gate, proto wire
  validator).

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback / unavailable |
|---|---|---|---|---|
| Bucket TIF | protection-policy TOML | `protectionTrailPolicy.TIF`, resolved via `effectiveTIF()` | proposal `tif` + Details line; CLI/SPA render | unset → DAY (pre-tif files unchanged, fingerprint-stable via `json:",omitempty"`) |
| TIF on a draft | proposal generation / preview params | `rpc.TradeProposal.TIF`, `rpc.OrderDraft.TIF` | `ibkr proposals`, `ibkr orders`, SPA | empty journal/proposal TIF → DAY |
| GTC-implies-trail rule | daemon preview validator + proto wire validator | `errBadRequest` / `unsupportedPlaceOrderProtoValue` | preview errors | LMT and modify paths stay DAY-only |
| Stale GTC row closure | broker error 135 on a write | `OrderLifecycleInactive` via `brokerErrorProvesOrderGone` | `ibkr orders`, order status | without a 135 reply the row stays open (see residual risks) |

## Safety invariants

- GTC is accepted for TRAIL/TRAIL LIMIT only, enforced at preview and again at
  the protobuf wire validator; LMT, purge legs, and the modify path remain
  DAY-only. Broker WhatIf stays the final arbiter of venue support.
- DAY remains the default at every layer; a policy file without `tif` and the
  embedded default keep their previous fingerprints (`omitempty` + unset).
- `trailing_stop.tif` outside DAY/GTC fails policy validation (even with the
  bucket disabled) → policy status `error` → fail-closed blockers. No silent
  unknown→DAY mapping is reachable.
- Proposal-vs-preview drift gate: non-DAY/GTC preview TIF → `unsupported_tif`;
  preview TIF ≠ proposal TIF → `tif_drift`.
- DAY-expiry caveat is carried in proposal Details and now rendered by the
  CLI text view (which previously dropped Details entirely).
- GTC rows are deliberately excluded from calendar DAY-expiry inference
  (`inferDayOrderExpiry`); their only local self-heal is the broker's
  error-135 "can't find order" reply, which now maps the row to terminal
  `inactive` regardless of sticky earlier Status.

## Residual risks (accepted)

- ~~A GTC stop with a missed terminal callback stays "open" locally until
  some write attempt elicits error 135.~~ Resolved 2026-07-19: the daemon now
  requests a broker open-order snapshot (`reqAllOpenOrders`) ~45 s after each
  (re)connect and every 30 minutes (`internal/daemon/order_reconcile.go`).
  Journal rows absent from a COMPLETE snapshot — current account/mode scope
  only, PermID set, quiet past a 10-minute grace window — are closed with a
  terminal `reconciled-absent` event mapping to `closed_reconciled`. The
  final broker state (cancelled vs filled outside the daemon's view) stays
  unknown locally; broker statements remain authoritative. The error-135
  heal remains as the immediate path when a write touches a gone order.
- IBKR cancels GTC orders broker-side on corporate actions/symbol events; the
  snapshot reconcile now catches these within one sweep, and the 135 heal
  still applies on any earlier write attempt.
- The duplicate-protective blocker is contract+side keyed, not
  quantity-aware: a standing GTC stop for a smaller quantity blocks
  re-protection after the position grows. With DAY this self-resolved at the
  session close; with GTC, cancel + re-propose.
- A GTC premium trail on a long option will eventually execute from theta
  decay alone; the proposal Details say so. Option trails stay disabled by
  default.

## Before/After artifact

Before: `ibkr order preview --json --order-type TRAIL --trail-percent 8 --tif GTC sell SYM N`
fails with "order preview supports DAY time-in-force only"; proposals carry
`"tif": "DAY"` unconditionally.

After: same preview succeeds (TRAIL/TRAIL LIMIT only); with
`[buckets.trailing_stop] tif = "GTC"` (policy_version bumped) proposals carry
`"tif": "GTC"` plus a persistence Details line, and the proposal fast path
previews/submits the GTC draft end-to-end.

## Verification

- Unit: policy parse/validate/fingerprint stability; generation TIF +
  Details; preview-params carry; drift gate; preview TIF gate; proto TIF ×
  order-type matrix; GTC zombie heal via 135; GTC fast-path preview
  end-to-end. GTC exclusion from expiry inference was already covered.
- `make check` (includes docs-regen drift gate), `make test`, `make smoke`.
- Paper E2E: GTC policy file → `ibkr proposals` → preview/submit → GTC
  trailing stop visible via `ibkr orders`.

## Rollback

- Revert: `internal/rpc/rpc.go` (GTC const), `internal/daemon/{protection_policy,proposal_engine,order_preview,order_read_model}.go`,
  `pkg/ibkr/place_order_proto.go`, `internal/cli/{order,catalog,proposals}.go`,
  `internal/mcp/tools.go` + regenerated docs.
- **Downgrade also requires removing `tif` from the policy file**: an older
  binary rejects it as an unknown key → policy status `error` → proposals
  blocked (fail closed, not silent).
- Runtime state: none beyond journal entries; any GTC order already placed
  keeps working at the broker after a rollback and must be cancelled manually
  if unwanted.
