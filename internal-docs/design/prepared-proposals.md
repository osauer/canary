# Prepared proposal handoff

A backend can prepare one exact close/reduce proposal, retain its private
reference, obtain human confirmation of the returned preview, and submit that
same preview. The daemon owns the binding and all current trading checks.

| Operation | CLI | RPC |
| --- | --- | --- |
| Prepare | `canary proposals prepare KEY REVISION --json` | `trade.proposals.prepare` |
| Submit | `canary proposals submit KEY REVISION --prepared-ref-stdin --json` | `trade.proposals.submit` with `prepared_ref` |
| Passive receipt | `canary proposals prepared-status --prepared-ref-stdin --json` | `trade.proposals.prepared_status` |

Prepare supports the existing optional quantity selection and timeout. It uses
full proposal revalidation, ordinary order preview and broker WhatIf. Success
requires a durable binding in daemon.db before the reference is returned.
The response extends the existing sanitized preview with `prepared_ref` and
`preparation`: ID, key, revision, draft fingerprint, original expiry, state and
nullable consumed flag. The raw order token stays inside Canary.

`prepared_ref` is an authorizing capability for a backend. Keep it out of
browser payloads, URLs, logs and command-line arguments. Bind the human approval
to the exact preview, account/mode, key/revision, preparation ID, fingerprint and
expiry. Only prepare returns the private reference; submit and status never echo
it. These methods have no MCP surface.

Submit accepts the same key/revision and the reference on standard input, with
no quantity override or client-supplied draft. The existing fast-path submit
configuration and request gate remain required, but a prepared submission does
full current revalidation. It checks the proposal terms, current policy and
scope, duplicate protection, option economics, origin and freeze, then enters
the existing order placement path with the original token. Portfolio and risk
authority, signer/authority generations, mode/pins, broker readiness and the
first-byte write guards still apply. It does not preview or renew the token.
The original token expiry is at most ten minutes and may be shortened by option
economic evidence. Changed or expired evidence requires a newly reviewed
preparation, not an automatic replacement preview.

An immutable preparation document links the proposal to its signed draft; the
existing journal owns execution. Its atomic token tombstone and pre-transmit
intent select one winner across concurrent requests and survive restart. A
consumed reference never sends again, including after an uncertain transport
result, freeze, expiry or response loss. The submit response adds preparation
metadata and the existing `OrderStatusResult` receipt. A replay returns
`accepted=false` and a consumed blocker with that receipt. Acceptance means the
existing transport result, not a broker acknowledgement or fill.

Status reads local durable state only: no preview, refresh, broker query or
reconciliation. Its envelope is `preparation`, optional `order`, optional
`blockers`, and `as_of`. States are `prepared`, `expired`, `consumed`, or
`unavailable`. Prepared means unconsumed, not currently eligible. If journal
consumption cannot be established, consumed is null; if the reference cannot
safely be resolved or its broker scope differs, preparation is omitted. Missing
or unreadable authority must never be interpreted as unused. Callers preserve
uncertain outcomes and require deliberate review before any new attempt.

Existing submit without a prepared reference keeps its one-confirm behavior.
This addition does not activate policy, automation, a trading build or live
execution. Synthetic tests cover exact draft identity, refusal gates, one-time
redemption, restart, lost/uncertain outcomes, persistence failure and passive
receipts in both build variants.
