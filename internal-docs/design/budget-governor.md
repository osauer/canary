# Budget governor (options premium at risk, reduce by rule)

Updated: 2026-09-21 15:38 CEST
Status: implemented in shadow on branch `p1-governor` (Desk product view "The
Bounded Desk", Phase 1 · Reduce by rule, row 1.3 and the capital-row
extension). The bucket ships absent from the embedded default and disabled;
the caps are proposals the owner confirms by writing them into the protection
policy file. The code carries no default that acts.

This record follows `.agents/docs/risk-policy-contract.md`. It records human
policy decisions; it creates none.

## Decision

- Goal and protected asset or behavior: keep the money tied up in long option
  premium inside a declared share of the risk capital, and when the risk
  constitution's drawdown brake is engaged, turn the excess into close-or-reduce
  proposals by rule instead of by mood. Protected asset: the declared risk
  capital; protected behavior: no discretionary-scale reduction ever skips the
  owner's veto window.
- Policy owner: the desk owner (single-trader desk).
- Human decision or approval reference (2026-09-21, quoted from the brief):
  - **D1** — "the risk constitution's floor and declared risk capital stand as
    written".
  - **D3** — "reduce-only automation (built by another agent as pre-authorised
    buckets with a thirty-minute veto)".
  - **D4** — the options mandate "a bit more aggressive" than proposed, which
    the product manager set as: "total premium at risk at most 40% of the
    declared risk capital, at most 15% per line. These caps are proposals the
    owner confirms by writing them into the policy file; the code carries no
    default that acts."
- Current authoritative document/code/fingerprint: the protection policy file
  (`[buckets.budget_reduction]`, part of `protection-policy-fp-v1`) for the
  caps and the mode; `risk-policy.toml` (`risk-constitution-fp-v1`) for the
  declared risk capital, floor and drawdown ladder; code:
  `internal/daemon/proposal_budget.go`, `internal/daemon/protection_policy.go`,
  `internal/rpc/opportunities.go`, `internal/rpc/brief.go`.
- Status: implemented (shadow); activation is the owner's policy edit.

## Meaning

- Capital or exposure base: the constitution's `capital.declared_risk_capital`
  in `capital.base_currency`. Not NLV, not buying power, not the effective
  risk capital (min(declared, equity − floor)); the caps are stated against the
  declared figure because that is the number the owner wrote. The brief's
  capital row now carries `declared_risk_capital_base`,
  `effective_risk_capital_base`, `protected_floor_base`, `warn_pct` and
  `block_pct` so the reader sees all three bases side by side.
- Aggregation unit: per line = one held long option contract (exact ConID);
  total = every long option leg Canary does not derive as protection, in base
  currency. Protection legs are outside both, so a book that is 45% premium at
  risk on the brief row (which counts every long leg) can measure 38% for the
  governor; each row and the snapshot status state the measured value.
- Risk-increasing, risk-reducing, and unknown classifications: every row is a
  SELL of a long option, position effect `reduce` or `close`; the engine's
  double check of position effect (proposal and preview) applies. A leg is
  protection when the standing option-purpose derivation says so
  (`optionExitPurpose`), or it is a hedge-listed long put (`SPY`, `SPX`,
  `SPXW`, `QQQ`, `IWM`), or it covers a stock of its own underlying
  (`optionExitCallHedge` / `optionExitPutHedge`). A leg of a multi-leg unit or
  an ambiguous underlying is measured but never trimmed as a single order: its
  row carries `strategy_workflow_required`. Everything else is discretionary
  premium and eligible.
- Measurement horizon and market-session assumptions: current position marks
  (`market_value_base`); a stale row is measured but its order is blocked with
  `fresh_option_quote_required`. No intraday history and no Greeks enter the
  measurement.
- Threshold or rule, including units:
  - `premium_at_risk_pct_of_risk_capital` — total measured premium ÷ declared
    risk capital × 100, in (0, 100].
  - `per_line_pct_of_risk_capital` — one line's market value ÷ declared risk
    capital × 100, in (0, 100], at most the total.
  - Per line first: a line above the per-line cap is reduced to the cap in
    whole contracts (keep = floor(cap ÷ value per contract)); when the cap
    rounds to zero contracts the row is a full close.
  - Then the total: while the projected sum exceeds the total cap, reduce lines
    in order of largest unrealised loss first, then largest market value, whole
    contracts (needed = ceil(excess ÷ value per contract)), until within the
    cap. A line already trimmed by the per-line pass can be trimmed further.
  - `max_order_notional` caps one generated order exactly as
    `risk_reduction.max_order_notional` does; the remainder waits for the next
    cycle, which re-evaluates from the position as it then is.
- Enforcement class: advisory. `mode = "shadow"` lists and journals rows that
  no surface can preview or submit (`shadow_mode`); `mode = "active"` makes
  them ordinary proposals under every existing gate. Neither mode places an
  order; the pre-authorisation scheduler (D3) is the only automatic path, and
  it must honour `never_skip_veto`.
- Unknown, stale, partial, and unavailable-data posture: no constitution, an
  unapproved capital section, a shadow drawdown enforcement class, or no
  latched or breached block tier generates nothing and a typed status names
  why (`constitution_unapproved`, `enforcement_shadow`, `not_latched`). A base
  currency mismatch between the constitution and the account is
  `currency_mismatch`. A leg without a base market value is excluded and
  counted; when no leg can be measured the status is `unmeasurable`. Nil is
  unavailable, never zero.

## Authority And Evidence

| Concept | Authoritative source | Typed field/contract | Freshness/finality | Fallback or blocker |
|---|---|---|---|---|
| Caps, mode, order notional cap | protection policy file | `protectionBudgetPolicy` (`[buckets.budget_reduction]`) | hot reload, version-bump discipline | table absent or `enabled = false` ⇒ bucket silent, no status |
| Declared risk capital, floor, ladder, enforcement class | `risk-policy.toml` | `risk.Constitution` | manager reread every 30 s | absent/unapproved ⇒ `constitution_unapproved` |
| Latch and tier | daemon `risk_capital` state | `rpc.CapitalStateReport.BlockLatched`, `.Tier` | live | not latched and not block ⇒ `not_latched` |
| Leg purpose | standing option-purpose derivation + rulebook hedge list | `optionExitPurpose`, `optionExitStrategyScope`, `risk.RulebookPolicy.IsHedgeSymbol` | per refresh | protection ⇒ never selected; unit leg ⇒ `strategy_workflow_required` |
| Line value and loss | positions view | `PositionView.MarketValueBase`, `.UnrealizedPnLBase` | per refresh; `Stale` honoured | nil base value ⇒ excluded and counted |
| Measured state | proposal snapshot | `rpc.TradeProposalSnapshot.BudgetReduction` (`budget_reduction`) | per refresh | — |
| Per-row arithmetic | proposal | `rpc.TradeProposal.Budget` (`budget`) | per refresh | — |
| Shadow and veto flags | proposal | `rpc.TradeProposal.Shadow`, `.NeverSkipVeto`, `(TradeProposal).AutomaticEligible()` | per refresh | shadow ⇒ `shadow_mode` on preview and submit |
| Capital-row figures | brief | `rpc.BriefCapitalRow` (`warn_pct`, `block_pct`, `protected_floor_base`, `declared_risk_capital_base`, `effective_risk_capital_base`), `rpc.BriefMoneyCoverageRow.PctOfRiskCapital` | per brief | nil when the constitution is absent or unapproved |

Post-trade truth is unchanged: fills reach the book through the ordinary
position stream, and the next cycle measures the position as it then is. There
is no governor-owned journal beyond the proposal snapshot and its events, which
carry `shadow` on every generated row.

## Exceptions And Change Control

- Who may grant an exception: the owner, by editing the protection policy file
  (raise a cap, disable the bucket, switch the mode) and bumping
  `policy_version`. The constitution's one-shot override mechanism has no key
  for this bucket.
- Required reason, scope, expiry, and audit record: the policy edit is the
  record; the fingerprint change shows in every later snapshot and event.
- Whether exceptions can increase risk: no row of this bucket can open,
  increase or flip exposure; an exception can only make it propose less.
- Rollback trigger and operator action: set `enabled = false` or remove the
  table and bump `policy_version`; existing rows leave at the next refresh.
- Policy version/fingerprint transition: the bucket is a pointer-typed table,
  so a file without it keeps its previous protection-policy fingerprint
  byte-for-byte, and the embedded default is unchanged.

## Operating Cadence

- Pre-open or daily review artifact: `canary brief` capital and premium rows
  (the ladder, money at risk (max) of declared, premium as % of risk capital).
- Pre-trade artifact and acknowledgement: `canary proposals list` — shadow rows
  under their own heading with cap, measured value, excess and order; `canary
  proposals preview` refuses them with `shadow_mode`.
- Post-trade/EOD reconciliation artifact: the proposal outcome marks and
  events, which now carry `shadow`; unchanged reconciliation otherwise.
- Weekly review and breach follow-up: the owner reads the shadow rows against
  what they did by hand; a persistent difference is the argument for or against
  `mode = "active"` and for the two cap values.
- Missed-run or stale-report escalation: a stale mark blocks the row
  (`fresh_option_quote_required`); a missing constitution reads
  `constitution_unapproved`. Nothing silently passes.
- Shadow/manual observation period before hard enforcement: shadow first, by
  policy default; active is a deliberate edit; there is no hard enforcement in
  this design at all — rows go through the veto window, always in full.

Why `never_skip_veto`: the pre-authorised buckets (D3) may shorten or skip the
thirty-minute veto when the brake is latched, because a stop is a stop. A
reduction to budget is a discretionary-scale action — it sells premium the
owner chose to hold, at a size a policy number set — so it always waits the full
window even under the latch. The scheduler reads the flag on the proposal; the
bucket sets it on every row.

## Verification

- Fixture scenarios (all synthetic books; `internal/daemon/proposal_budget_test.go`,
  `proposal_budget_shadow_test.go`, `protection_policy_budget_test.go`,
  `brief_capital_test.go`, and `internal/cli/proposals_budget_test.go`):
  three discretionary lines plus a hedge-listed put and a covering put over a
  latched, approved constitution; the per-line pass runs before the total pass
  and the order is largest loss first; cap arithmetic in whole contracts
  including the zero-contract full close; an unapproved constitution, a shadow
  enforcement class and an open latch yield no rows with the typed reason; a
  partial fill re-evaluates from the new position.
- Property/invariant tests: every row is a SELL with effect reduce or close and
  quantity within the position; a protection leg is never selected; the bucket
  is absent from `canary policy default protection` and disabled when the table
  is written without `enabled`; shadow rows are refused by preview and submit
  and `AutomaticEligible()` is false for them; the capital-row figures are nil
  when unapproved and filled otherwise with `pct_of_risk_capital` correct.
- Adversarial free-text and prompt-injection cases: the bucket reads numbers
  from the two policy files and typed position fields only; no symbol, order
  reference or broker text enters a decision.
- Cross-surface parity checks: CLI text lists shadow rows under their own
  heading; JSON carries `shadow`, `never_skip_veto`, `budget` and the snapshot
  `budget_reduction` status; the SPA renders the rows as blocked proposals
  through the existing blocker path.
- Redacted before/after artifact: none against a live gateway (fixtures only,
  by instruction).
- Residual risk accepted by the user: the caps are measured on marks, so a wide
  or stale quote can over- or understate a line until the next refresh; the
  per-line cap uses the declared figure, not the effective one, so after a
  large drawdown the cap is looser relative to what is actually at risk than
  the percentage suggests. Both are stated on the row and in the docs.
