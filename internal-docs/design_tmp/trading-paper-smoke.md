# `ibkr trading paper-smoke` — the release-gate order-pipeline smoke

Updated: 2026-06-10 17:05 CEST
Status: shipped, then re-gated same day by owner decision (see Revision below)

> **Removed 2026-06-11 (live-gate simplification, see CHANGELOG):** the typed
> `live/<account>` confirmation and the `allow_live` / `live_ack_*` config
> keys referenced throughout this document no longer exist. This doc is kept
> as the historical design record of the paper-smoke itself, which is
> unchanged: a release-pipeline gate, informational in trading status.
>
> **Storage revision 2026-07-20:** current evidence is a versioned state
> document in `$XDG_STATE_HOME/ibkr/daemon.db`; its MAC key is the separate
> private `order-preview-key-v2`. References below to
> `trading-readiness.json` and `order-preview-key` describe the retired
> pre-cutover implementation only and are not runtime fallbacks.

## Revision 2026-06-10 — release-time gate, no human ceremony

Owner decision after shipping: the runtime gate stack was too deep (TWS API
toggle + build flag + config pins/acks + human-certified evidence). The smoke
itself stays — it is the only end-to-end proof the order pipeline works — but
it moves from a runtime live precondition to a **release-pipeline quality
gate**:

- `make release` runs the smoke automatically at version bump and aborts the
  release on failure. It requires a reachable **paper** session at release
  time, by construction.
- `CheckPaperSmoke` no longer contributes live blockers; trading status still
  reports the evidence fields informationally (including `unsigned` for
  hand-edited files).
- The human-origin-only restriction on the producer and the repo-hook agent
  block are removed: with nothing unlocked by the evidence at runtime, the
  restriction only obstructed release automation. The paper order itself
  rides the agent-open paper route.
- Runtime live enablement = TWS-side API toggle + trading-capable binary +
  config (pinned endpoint/account, `mode=live`, `allow_live`, both live
  acks). The agent-origin live block and typed `live/<account>` confirmation
  are unchanged.

Sections below describe the original human-certified design and remain as
the implementation record; read them through the revision above.

Original status: reviewed — implementing with review deltas (limit offset 0.98 not 0.75;
single ack-wait knob + detached fixed-budget cancel phase; JSON+base64 MAC
payload instead of pipe-join; `--symbol` dropped; smoke-run mutex; direct
broker cancel fallback for the not-yet-acknowledged case since
`orderViewCancelEligible` requires broker ack)

Contract per `docs/templates/daemon-cli-trading-contract.md`.

## Scope

- **Goal:** every live-trading blocker tells the user to run
  `ibkr trading paper-smoke`, but the command does not exist —
  `SavePaperSmoke` has zero production callers and the de-facto live switch is
  hand-writing `~/.local/state/ibkr/trading-readiness.json` (unaudited plain
  JSON). Implement the producer: a daemon-owned paper order round-trip
  (place 1-share far-off-market LMT → broker ack → cancel → cancel confirmed)
  whose evidence is written by the daemon and MAC'd so it cannot be
  hand-forged. Additionally, make SPA live writes possible at all: the daemon
  now requires a typed `live/<account>` confirmation from human origins, and
  the SPA never collects one.
- **User-facing command/tool/API:** `ibkr trading paper-smoke
  [--timeout 30s] [--json]`; new daemon RPC `trading.paper_smoke`;
  SPA proposals-submit / order-modify / purge live flows gain a typed
  live-confirmation input. **No MCP tool** — agents must not be able to mint
  the last live precondition.
- **Owner layer:** daemon owns the smoke lifecycle and the evidence file
  (only the daemon can author valid evidence); CLI is the renderer and the
  only intended invoker; RPC carries params/result; SPA/HTTP only forward the
  typed phrase for ordinary live writes (not paper-smoke, which stays
  CLI-only).
- **Existing behavior and artifact:** `internal/cli/trading.go` has only
  `status`. `tradingReadinessStore.SavePaperSmoke` is dead code.
  `CheckPaperSmoke` (called from `tradingStatus` for live mode) validates
  account family / endpoint host / client ID / version / freshness of plain
  JSON with no integrity check.

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback or unavailable state |
|---|---|---|---|---|
| Paper-smoke evidence | daemon `trading.paper_smoke` run (place→ack→cancel→confirm observed in-daemon) | daemon.db `trading_readiness_v1` state document with `paper_smoke{…}` + `mac` | `ibkr trading status` (`PaperSmoke*` fields) | missing/stale/mismatch stays informational; release smoke remains binding |
| Evidence integrity | HMAC-SHA256 with the private `order-preview-key-v2`, domain-separated prefix `ibkr-paper-smoke-v1\|` | `paper_smoke.mac` (base64url) plus daemon.db signer generation | status `unsigned` when invalid | missing/invalid MAC is disclosed; there is no JSON file fallback |
| Smoke invocation origin | invoking adapter (CLI `DetectWriteOrigin`) | `origin` on `TradingPaperSmokeParams` | order journal origin | automated origins allowed; smoke transmits only on paper |
| Live human confirmation (SPA) — **removed in v1.10.0** | ~~typed input in the PWA, verbatim `live/<account>`~~ the typed phrase, `live_confirmation` field, and `live_confirmation_required` blocker were removed end-to-end in v1.10.0; the app HTTP layer now rejects the field as an unknown field on every broker-write route | server-validated `confirm_account`/`confirm_mode` on every HTTP write (unchanged) | single-click after preview; purge keeps its typed confirm | mismatched confirm fields → write refused |
| Smoke order lifecycle | broker callbacks via order journal read model | `SendState=broker_acknowledged`, `LifecycleStatus ∈ {pre_submitted, submitted}` then `cancelled` | result fields `ack_lifecycle_status`, `cancel_lifecycle_status` | timeout → `result=failed` evidence (fail closed) |

## Mechanism

### Daemon RPC `trading.paper_smoke` (build-tag `trading`; `!trading` stub returns `ErrTradingDisabled`)

One synchronous RPC; the daemon itself observes the whole round-trip (the CLI
never relays lifecycle claims — otherwise evidence would trust the client):

1. **Origin metadata:** paper-smoke is open to automated origins and journals
   origin through the normal order path. As of 2026-06-10 the evidence is a
   release-pipeline quality gate, not a runtime live precondition; the smoke
   itself transmits only on the paper route.
2. **Route gate:** `status.Mode == paper` required (refuse on live — the smoke
   must never transmit on a live gate), status unblocked, broker write
   authorization allowed. One smoke at a time (`TryLock` → "already running").
3. **Draft:** quote `SPY` (constant — the smoke proves order plumbing, not
   symbol coverage; no `--symbol` knob until a real need appears), reference
   price = first positive of bid, last, mark, midpoint, ask; limit =
   `floorPriceToTick(reference × 0.98, max(resolvedMinTick, 0.01))` —
   2 % below stays inside default TWS API price-precaution bands (a 25 %
   offset is deterministically rejected by the ~3 % percentage constraint,
   error 109) while a 1-share DAY buy 2 % under the market is still
   effectively unfillable inside a ≤60 s window; the fill→failed path covers
   the residual. No reference price → error (no evidence change).
4. **Preview via the production path:** `s.previewOrder` (journals
   `previewed`, runs broker WhatIf, mints a real single-use token). Require
   `SubmitEligible` — WhatIf must accept; otherwise error, no evidence change.
5. **Place via the production path:** `s.placeOrder` (journals
   token-confirmed + send-attempted, transmits with the paper-gate
   `SubmitPaperOrder`). `brokerWriteMu` is held around this call only — never
   across the polls (and never via `handleOrderPlace`, which locks it
   itself). The smoke exercises exactly the code real orders use.
6. **Wait for broker ack:** poll the order read model by `order_ref` (poll
   first, then every 250 ms) until `SendState == broker_acknowledged` or
   lifecycle `pre_submitted`/`submitted`. Single knob: `timeout_ms` is the
   ack wait, default 30 s, capped 60 s. Poll deadlines are wall-clock/ctx —
   never `s.now`, which tests pin to a fixed instant.
7. **Cancel — always attempted once transmit was attempted,** even if the ack
   timed out (never leak an open smoke order), on
   `context.WithoutCancel(ctx)` with its own fixed 15 s budget so the method
   deadline or a dropped client cannot strand the order. Ack observed →
   `s.cancelOrder` (production path). Ack never observed → the read model
   refuses (`orderViewCancelEligible` requires broker ack), so fall back to a
   direct `cancelConfiguredOrder` on the reserved ID plus a manually
   journaled cancel-requested event. Then poll until lifecycle `cancelled`.
8. **Evidence:** `result=passed` only if ack **and** cancel confirmation were
   both observed. Save `{account, endpoint, endpoint_class=paper, client_id,
   version, result, at}` + MAC via `SavePaperSmoke`. Failure after transmit
   was attempted → save `result=failed` evidence (deliberately fail-closed:
   a broken order lifecycle revokes prior valid evidence). Failure before any
   transmit → error only, evidence untouched.
9. **Unexpected fill while waiting** (≈ impossible at −25 %): `result=failed`
   plus an explicit warning naming the position to close manually.

### Evidence MAC

- `orderTokenSigner` gains `signPaperSmoke(ev) string` /
  `verifyPaperSmoke(ev, mac) bool`: HMAC-SHA256 over
  `"ibkr-paper-smoke-v1." + base64url(json.Marshal(evidence))` — the same
  JSON+base64 idiom as preview tokens rather than a second ad-hoc canonical
  form (struct fields marshal in declaration order; `time.Time` round-trips
  RFC3339Nano deterministically, so verify-by-re-marshal is stable). The
  prefix domain-separates from preview-token MACs. The MAC lives at file
  level (`paper_smoke_mac`), beside — not inside — the evidence object, so
  "evidence sans MAC" is just the evidence struct.
- `tradingReadinessStore` keeps pure I/O; the store gets the signer handed in
  at construction (`installOrderTokenSigner` must therefore run before
  `installTradingReadinessStore` — today the order is reversed). A nil signer
  fails closed: `SavePaperSmoke` errors, `CheckPaperSmoke` reports
  `unsigned`.
- `CheckPaperSmoke`: evidence with missing/invalid MAC → new status
  `unsigned` (blocker `paper_smoke_unsigned`, action: rerun
  `ibkr trading paper-smoke`; hand-written evidence is not accepted).
  File version stays 1 — the `mac` field is additive.
- **Trust model (SECURITY.md note):** the key lives in the same state dir a
  same-uid process can read, so the MAC is an interlock against hand-editing
  and accidental forgery, not a security boundary — exactly the origin-gating
  trust model.

### CLI

- `ibkr trading paper-smoke` subcommand: flags `--timeout` (broker-ack wait,
  default 30 s), `--json`. Sends `env.Origin`; renders gate echo, order
  ref/ID, limit, ack + cancel status, evidence age window, and — on pass —
  the note that evidence is bound to this binary version (every
  `make install` requires a rerun); failures render reason + action
  prominently since a failed run deliberately revokes prior valid evidence;
  exit 0 on pass, 1 on fail.
- `cmd/ibkr/main.go`: paper-smoke gets a long unary budget (120 s);
  daemon-side method deadline 100 s (under the CLI ceiling so classified
  errors reach the user — same pattern as `scan`).
- catalog entry + docs regen.

### SPA / HTTP live confirmation

> **Removed in v1.10.0.** The typed `live/<account>` phrase, the
> `live_confirmation` request field, and the `live_confirmation_required`
> blocker were removed end-to-end; the app HTTP layer strict-decodes every
> broker-write body and rejects the field as unknown. Server-validated
> `confirm_account`/`confirm_mode` remain. (The daemon-side agent-origin
> live block was likewise removed on 2026-06-20 — origin is audit/policy
> metadata now; see `docs/design/agent-origin-gating.md`.) The bullets below
> describe the v1.9.0 mechanism for historical context only.

- HTTP: `orderModifyRequest` and `purgeActionRequest` gain
  `live_confirmation` mapped onto the rpc params (proposals submit already
  embeds `rpc.TradeProposalSubmitParams`, which has the field). Origin stays
  server-assigned (`human-paired-device`); the phrase remains client-typed.
- SPA (`web/app/app.js`): when `trading.mode === "live"`, proposals submit
  and order modify prompt for the typed phrase `live/<account>` (the existing
  `window.prompt` idiom used by purge); purge already makes the user type
  `live/<account>` when live — that typed string is now also sent as
  `live_confirmation`. Paper flows are unchanged. `app_compat_test.go`
  contracts updated.

## Safety Invariants

- Paper-smoke is deliberately open to automated origins: since 2026-06-10
  the evidence is a release-pipeline quality gate (`make release` aborts
  without a passing round-trip), not a runtime live precondition, so an
  origin restriction would only obstruct release automation. The order
  transmits on the paper route, which is open to agents by policy.
- The smoke only ever transmits on the paper route; a live-routed gate
  refuses before any broker call.
- Evidence is daemon-authored only: valid MAC requires the daemon's key, and
  the daemon only signs after observing the round-trip itself.
- Preview tokens are not submit eligibility; the smoke checks both
  (`SubmitEligible` gate before place).
- Trading capability and live acks remain operator-owned config/build state;
  the smoke adds evidence, never flips config.
- Nil/zero handling unchanged; quote warnings ride along into the result.
- Journal records the full smoke lifecycle with `source="paper-smoke"` and
  the caller's origin — the audit trail the hand-written JSON era lacked.

## Before/After Artifact

Before:

```sh
ibkr trading status --json      # live blockers say: run `ibkr trading paper-smoke`
ibkr trading paper-smoke        # → unknown subcommand
```

After:

```sh
make install && ibkr restart --timeout 15s
ibkr trading paper-smoke --json # paper round-trip + MAC'd evidence
ibkr trading status --json      # paper_smoke fields populated from evidence
```

If no paper gateway is reachable, the exact missing artifact is named in the
completion message.

## Verification

- Narrow unit/package tests:
  - handler matrix with fake broker hooks (`orderPlaceBroker` etc.): origin
    refusal; live-route refusal; pass path writes MAC'd evidence; ack timeout
    → direct-cancel fallback still attempted + `failed` evidence;
    cancel-confirm timeout → `failed`; pre-transmit failure leaves evidence
    untouched; fill → failed; concurrent run refused.
    `newOrderPreviewTestServer` builds no readiness store — smoke tests wire
    store+signer in a temp dir; fake brokers append lifecycle events
    synchronously (poll-first makes that deterministic).
  - signer: sign/verify round-trip, tamper detection, domain separation.
  - `CheckPaperSmoke`: unsigned/forged/legacy-v1-file → `unsigned` blocker.
  - CLI: render + exit codes; catalog/docs.
  - SPA: `app_compat_test.go` broker-write confirmation contracts (the typed live-confirmation flow was removed in v1.10.0).
- Generated docs needed: yes (`make docs-regen` — new subcommand, new env
  none, config unchanged).
- Static gate: `make check`. Tests: `make test` (incl. `-tags trading` leg).
- Live gate: `make smoke`, then the real artifact: `ibkr trading paper-smoke`
  against paper TWS with output pasted.

## Rollback Notes

- Files to revert: new `internal/daemon/trading_paper_smoke{,_disabled}.go`,
  edits to `trading_readiness.go`, `order_preview.go` (signer methods),
  `trading_status.go` (unsigned status), rpc params/result + method, CLI
  `trading.go`/catalog/main.go, HTTP request structs, `app.js`, docs.
- Runtime state touched: daemon.db `trading_readiness_v1`; the SQLite cutover
  deliberately resets prior smoke evidence and rotates the token/MAC key.
  Rolling forward requires a new paper smoke only when release evidence is
  needed; never restore the retired JSON file as a fallback.
- User-facing behavior that changes: live blockers' instruction finally
  works; hand-written evidence stops being accepted (deliberate); SPA live
  writes become possible via typed confirmation (previously impossible).
