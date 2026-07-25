# Risk Constitution (risk-policy.toml)

Updated: 2026-07-18 17:31 CEST
Status: phase 1 implemented 2026-07-12 (advisory/shadow only); v2 adds
[recon] 2026-07-13 (docs/design/post-trade-truth.md); v3 2026-07-18 adds
statement-authoritative flows and the clean-report auto-extend
(docs/design/operator-ergonomics.md, implementation record). Interview
decisions approved by the operator on 2026-07-12; every numerical threshold
remains unapproved until the operator writes it into the policy file.

The machine-readable policy is the constitution. `~/.config/ibkr/policies/
risk-policy.toml` is the single authority over personal capital numbers;
code owns schema, validation, calculation, and non-overridable invariants;
daemon runtime state owns observed and derived facts; `ibkr policy show
--explain` renders the whole contract. A prose constitution is optional and
must not duplicate numbers.

## Approved decisions (2026-07-12)

1. **Capital anchor:** an internal protected equity floor (EUR) inside the
   account. External wealth is unobservable and therefore not the anchor.
2. **Effective risk capital** = min(declared_risk_capital, equity −
   protected_floor). Buying power is never the risk budget.
3. **No auto-increase:** deposits, profits, and live events never raise
   declared risk capital; only a fingerprinted policy revision does.
4. **Drawdown ladder:** two tiers, both % of declared risk capital consumed
   from the cash-flow-adjusted equity peak. Warn = advisory, self-clearing.
   Block targets risk-increasing orders only; reductions, closes, cancels,
   and rulebook-hedge-classified entries stay exempt. Block ships
   shadow-first.
5. **Resumption:** a block breach latches in daemon state regardless of
   mark recovery; clearing requires a journaled human reset with reason,
   which re-bases the peak. Re-stating declared risk is a policy revision.
6. **Exceptions:** one-shot overrides (human-only, single control, reason,
   hard expiry, journaled with fingerprint) for time-bounded exceptions;
   fingerprinted revisions for durable change.
7. **Stale/unreconciled data:** posture follows enforcement class.
   Advisory/shadow: unknown + disclosure, never a silent pass. Promoted
   hard (future): fail closed for risk increases.
8. **Cadence:** morning/eod/weekly artefacts are declared and journaled,
   advisory only in v1; reconciliation lapses flow through the staleness
   posture rather than a separate gate.

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback or unavailable state |
|---|---|---|---|---|
| Capital numbers, ladder, override cap, cadence declarations, sibling pins | `risk-policy.toml` (no embedded default) | `risk.Constitution` | `ibkr policy show [--explain]` | missing file/key ⇒ `unapproved`, never a code value |
| Schema, validation, evaluation, explain text | code | `internal/risk/constitution*.go` | all | n/a |
| Policy identity | manager | `rpc.RiskPolicyResult.PolicyFingerprint` (`risk-constitution-fp-v1`) | policy show, journals | absent |
| Adjusted peak, drawdown tier, latch, flows, overrides, artefacts | daemon runtime state | daemon.db `risk_capital` state document plus `capital_events` | policy show | unseeded ⇒ tier `unknown`; storage failure ⇒ unavailable |
| Governance evidence | daemon | daemon.db `risk_policy_events` plus append-only event payload carrying the fingerprint | phase-3 replay | storage failure is disclosed; no file fallback |
| Equity observation | account summary success path (`handleAccountSummary`) | `AccountResult.NetLiquidation/AsOf` | policy show `input_health` | persisted last reading, disclosed stale |
| Preview cause | `riskPolicyPreviewWarnings` | `DataWarning{Code:"capital_drawdown", Scope:"risk_policy"}` | order preview | absent when policy nil or tier ok/unknown |

Manager semantics: protection-policy manager vocabulary (strict unknown-key
rejection, version-bump-required drift detection, last-good retained on
error) with two deliberate differences — **no embedded default** (missing
file is status `absent`) and **no trading blockers** (v1 is advisory; a
broken constitution is disclosed loudly, it does not stop trading — that
posture flips per control if a control is promoted to hard).

Cash-flow adjustment: capital events (deposit/withdrawal/reconcile) are
operator-declared journal facts. Adjusted equity = equity − cumulative
declared flows; the peak tracks adjusted equity, so a deposit is not a fake
peak and a withdrawal is not a fake drawdown. A deposit whose effective
time precedes the recorded peak corrects the peak downward (never-inflate).
Since phase 3a (risk-policy v2, docs/design/post-trade-truth.md) the
reconcile event is a human sign-off against a specific, fully resolved
`ibkr recon` report — bare attestation is retired, and the `[recon]`
policy keys define what counts as a matching exception.

Since risk-policy v3 (2026-07-18, docs/design/operator-ergonomics.md):
statement-confirmed post-genesis flows are the authoritative cumFlows
input; declarations are optional provisional bridge entries covering only
the fetch lag (matched ones are superseded by the statement value); peak
corrections key off statement value dates, exactly once per line id; and
reconcile evidence is either a human sign-off or an automatic clean-report
extension — a report with zero unresolved exceptions, statements and a
same-day equity pair fresh within `recon.max_report_age_days`, and
divergence within `recon.max_equity_divergence_pct`, journaled as origin
`daemon-auto` with the report id. Declared vs statement cumFlows are
displayed side by side until R5.

## Safety invariants

- Account/route/client pins, WhatIf, preview tokens, journal integrity,
  freeze, and agent-origin gating have **no keys in this schema**; no
  revision or override can express a change to them.
- All five policy write methods are human-origin-only (`originIsHuman`);
  agent sessions read but never operate this surface.
- Nothing in this feature reads or writes `submit_eligible`, blockers,
  freeze, pins, or tokens. v1 is advisory/shadow end to end.
- Data absence never renders ok: `unapproved`, `unknown`, stale, and
  unreconciled are distinct disclosed states (never-false-pass).
- `block_enforcement = "hard"` is rejected by schema v1; promotion requires
  a schema revision after the phase-2/3 shadow evidence, as a deliberate
  human decision.

## Files

```
internal/risk/constitution.go          schema, validation, fingerprint, EvaluateCapital
internal/risk/constitution_explain.go  ConstitutionLimits (single copy of meanings)
internal/rpc/risk_policy.go            methods, params, result types
internal/daemon/risk_policy_manager.go TOML manager (absent/active/drift/error)
internal/daemon/risk_capital_state.go  peak/latch/events/overrides/artefacts + journals
internal/daemon/risk_policy_handlers.go RPC handlers + preview cause
internal/daemon/recon_auto_extend.go   v3 clean-report auto-extend (startup + post-ingest only)
internal/cli/policy.go                 ibkr policy show/capital-event/override/reset-drawdown/artefact
examples/risk-policy.toml              operator template (all material keys commented out)
```

Runtime state and governance evidence live in
`~/.local/state/ibkr/daemon.db`. The former JSON/JSONL artifacts are sealed
cutover inputs, not live fallbacks.

## Deferred (explicitly not in phase 1)

MCP `ibkr_policy` tool; SPA card; push alerts; Flex/Activity ingestion;
promotion of any control to hard; automated reports (phase 4); capital
allocation responses (phase 5). The canary fingerprint label was renamed
`canary-policy-fp-v1` (was `risk-policy-fp-v1`) to keep identities
unambiguous; fingerprint keys are unchanged.

## Rollback

Revert the files above. The risk-capital document and governance events remain
inside daemon.db and may be ignored by older feature code, but daemon.db must
not be deleted or replaced; the operator's TOML remains separate policy
authority. User-visible change on rollback: `ibkr policy` disappears and
previews lose the advisory `capital_drawdown` cause; no trading-path behavior
changes either way.
