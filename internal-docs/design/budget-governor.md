# Budget governor (options premium at risk, reduce by rule)

Updated: 2026-09-30 10:53 CEST
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

## Basis (amendment 2026-09-23 08:37 CEST)

`basis = "declared_risk_capital"` (the default, and the only basis before this
amendment) keeps everything below. `basis = "rulebook"` measures against the
Rulebook policy in force instead, as shares of NLV from the account summary: a
line's premium at risk (the higher of price paid and value) is cut to
`option_line_act_pct`, and a shortfall of broker-reported available funds under
`cash_reserve_min_pct` is covered by selling lines in the loss-first order at
their current value. It reads no risk constitution and waits for no brake,
because the owner decided on 2026-09-23 that exposure is limited relative to
cash at all times, not only after a drawdown. The two percentage keys must be
absent under this basis. Shadow mode, protection legs, `never_skip_veto`,
`max_order_notional` and every order gate are unchanged.

Amendment 2026-09-30 (owner decision, Rulebook amendment 17): the cash reserve
is retired. The total pass now reads Rulebook rule 3, the premium budget of
the regime set in force (the latched regime stage, read as rule 3 reads it; a
carried stage uses the lower of its set and calm): it triggers when the book's
premium at risk reaches `premium_budget_act_pct` of NLV and sells lines in the
unchanged loss-first order, each contract counted at its premium at risk,
until the total is back at `premium_budget_watch_pct` (rule 1's trim
convention). The act level is the same in every set by default (35), so a
rise in volatility alone starts no sale; once the cap is reached under
confirmed stress, the cut deliberately goes to the stress budget (15). Available
funds are context only; the basis needs NLV. The
status and row fields `cash_reserve_min_pct` and `cash_shortfall_base` became
`premium_budget_watch_pct`, `premium_budget_act_pct`, `premium_pct_of_nlv` and
`premium_excess_base`, with `premium_budget_set` naming the set on the status.
The governor keeps its own protection classification, so its measured total
can differ from rule 3's where a declared or covering option is protection to
the governor but not to rule 12.

## Ranking, candidates and the review gate (amendment 2026-09-30)

Owner decisions of 2026-09-30 09:10 CEST ("GO – I love it. Build 1-3"),
recorded from the senior review of the first live governor row, which sold
the line with the largest unrealised loss. Largest loss first is exit
discipline borrowed from rule 13, not a way to choose what to sell when a
book-level budget is breached; a desk ranks by what the sale fixes.

- **G1 · Review gate.** Under `basis = "rulebook"`, while the Rulebook policy
  file in force reads `unreviewed` (`rpc.RulebookPolicyStatus.Review`, the
  file still opens with `# Canary defaults, not yet reviewed.`), the governor
  behaves as shadow whatever `mode` says: rows are listed and journaled with
  `shadow: true`, preview, submit and the queue refuse them, and the first
  blocker on every row is `rulebook_unreviewed` ("the Rulebook policy file
  still carries Canary's defaults, not yet reviewed"; action: read the file,
  set the limits you have decided, then delete its first line). The status
  keeps `mode` as configured and adds `shadow: true` and `shadow_reason:
  "rulebook_unreviewed"`; `AutomaticEligible()` is false. A configured shadow
  mode keeps `shadow_mode` as the second blocker. Once the file is reviewed,
  the configured mode applies at the next refresh (the manager rereads the
  file every 30 s). This does not reverse the owner's decision of 2026-09-28
  ("armed instead of shadow … human has to approve trades anyways"): the mode
  stays the owner's; the gate only asks that the numbers the governor sells
  against be the owner's numbers. The declared-risk-capital basis sells
  against caps the owner wrote in the protection policy and is not gated.
  The gate fails closed (reviewer decision 2026-09-30 10:52 CEST): the
  rulebook basis acts only when the Rulebook policy status reads an
  explicitly reviewed owner file (`source = file`, `status = active`, no
  review marker). Every other state holds it in shadow with the same
  `rulebook_unreviewed` blocker and a `shadow_reason` naming the case:
  `rulebook_unreviewed` (the marker), `rulebook_no_file` (the compiled
  baseline: "no Rulebook policy file; Canary's compiled defaults apply"),
  `rulebook_drift` (the file on disk is not the one in force: "the last good
  file applies") and `rulebook_error` (the file could not be read; the last
  good file, or before any good file Canary's compiled defaults, apply). Each
  blocker's action names what lifts it.
- **G2 · Ranking.** The total pass (both bases, one algorithm) sells whole
  contracts in this order: (1) relief, descending: the number of distinct
  Rulebook rows at watch or act (rules 1, 2, 4, 5, 13, 16, 18) on which the
  line (its exact leg, matched by the Rulebook's leg description) or its
  issuer (`IssuerOf`, so issuer groups count) is a measured offender; an
  offender listed as `unknown` is not relief. It reads the latest Rulebook
  result the daemon holds for the broker scope and the policy in force
  (`cachedRulebookResult`, the same read the stress page takes, within the
  75 s preview window) and never re-measures. No result: every line relieves
  nothing, the status carries `ranking_without_rulebook: true`, and the
  reason says the ranking read time value, then loss. (2) Time-value share,
  descending: extrinsic ÷ value from `risk.OptionExtrinsicPerShare`, the
  decomposition the theta bucket and rule 4 share, on the row's underlying
  price and mark (the valuation mark when no mark); unknown sorts last.
  (3) Unrealised loss, largest first (the old key). (4) Line value.
  (5) Contract id. The per-line pass runs first, unchanged; protection legs,
  unit legs and stale marks keep their treatment (never selected, and
  `strategy_workflow_required` / `fresh_option_quote_required` on the row).
  The reason names the order used.
- **G3 · Candidates and plan.** The status gains `candidates`, the first
  three ranked lines, each with its contract (con_id, symbol, sec_type,
  expiry, strike, right), `contracts` held, `unit_value_base`, `relief` (rule
  ids in rulebook order), `time_value_pct`, `unrealized_pnl_base` and a one-line
  `why` ("offends 2 open rules; 50% time value; unrealised −500"), and `plan`:
  every order the measurement needs, one line's orders together and lines in
  rank order, each with `rank`, `contract`, `contracts`, `raises_base` (at the
  line's value) and `cycle` (1 for this refresh; 2 and later where
  `max_order_notional`, unchanged, holds the rest back). Every governor row's
  `details` gain three lines before the veto sentence: its place in the plan
  ("order 1 of 2 in the plan; order 2 of 2 (2 contracts of the same line)
  follows after this fill", or "... is the next line", or ", the last"), the
  relief line ("also relieves rule 2 (line 12.5% of NLV), rule 4"; rule 2 is
  left out where its act level is the row's own reason), and one
  alternatives line naming the next two ranked lines with their `why`.
- **Ignore.** Ignoring a row (the existing ignore path, by key) takes its
  line out of the ranking and both passes: the next refresh plans from the
  next candidate. The ignored line still counts in the measured total.
- **Surfaces.** `canary proposals` prints the candidates and the plan under
  the Budget header and names the gate there; JSON and MCP
  `canary_proposals` carry both lists. Journaled events are unchanged apart
  from the new snapshot fields. The SPA is out of scope (Desk renders it).
- **Send timing after the open** is compiled, not a policy key:
  `readinessOptionsOpeningOffset` (15 minutes; stocks 5) in
  `internal/daemon/proposal_readiness.go` dates `default_send_at` and is the
  earliest time the queue and the pre-authorisation scheduler send. The
  senior review's ask for no governor sends in the first 30 minutes of a
  stress open is a later decision.

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
    in rank order (since 2026-09-30: open Rulebook rules relieved, then
    time-value share, then largest unrealised loss, then largest market value,
    then contract id; see the amendment above), whole
    contracts (needed = ceil(excess ÷ value per contract)), until within the
    cap. A line already trimmed by the per-line pass can be trimmed further.
  - `max_order_notional` caps one generated order exactly as
    `risk_reduction.max_order_notional` does; the remainder waits for the next
    cycle, which re-evaluates from the position as it then is. The row's
    reason asks to sell exactly the order's quantity; a held order also names
    the plan's number and the limit that held it (`budget.contracts_*` keep
    the plan's cuts), and the status `plan` lists the held remainder as
    cycle 2 and later.
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
| Relief per line (rank key 1) | the Rulebook result the daemon holds | `rpc.RulesResult` via `cachedRulebookResult` (scope-, connector- and policy-bound) | 75 s preview window | none current ⇒ relief 0, `ranking_without_rulebook` |
| Time-value share (rank key 2) | positions view | `risk.OptionExtrinsicPerShare` on `PositionView.Underlying`, `.Mark` | per refresh | nil ⇒ sorts last |
| Review gate (rulebook basis) | Rulebook policy file | `rpc.RulebookPolicyStatus` (`Source`, `Status`, `Review`) | manager reread every 30 s | anything but a reviewed owner file in force ⇒ shadow, `rulebook_unreviewed` first, `shadow_reason` names the case |
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
  `proposal_budget_shadow_test.go`, `proposal_budget_plan_test.go`,
  `protection_policy_budget_test.go`, `brief_capital_test.go`, and
  `internal/cli/proposals_budget_test.go`):
  three discretionary lines plus a hedge-listed put and a covering put over a
  latched, approved constitution; the per-line pass runs before the total pass
  and, with no Rulebook result and no time value, the order falls back to
  largest loss first; cap arithmetic in whole contracts
  including the zero-contract full close; an unapproved constitution, a shadow
  enforcement class and an open latch yield no rows with the typed reason; a
  partial fill re-evaluates from the new position.
- Amendment 2026-09-30 fixtures (`proposal_budget_plan_test.go`, and the CLI
  text in `proposals_budget_test.go`): an unreviewed Rulebook file holds an
  active rulebook basis in shadow with `rulebook_unreviewed` first (preview,
  queue and `AutomaticEligible()` refuse), a configured shadow adds
  `shadow_mode` second, a reviewed file restores the mode, and the declared
  basis is not gated; through a real policy manager the gate fails closed on
  no file, a file in drift, a file in error after a good load and before
  any, each with its `shadow_reason` and message; the engine reads the review state and the held Rulebook
  result (a stale one is not read); four lines where largest loss would sell
  DDD then CCC sell AAA (two open rules) then BBB (one rule, more time value)
  on both bases, issuer groups count, unknown offenders and rows outside the
  seven rules do not; without a Rulebook result time value leads and the
  status says `ranking_without_rulebook`; the tie-breaks (relief, time value
  with unknown last, loss, value, contract id); the candidates and plan JSON,
  a plan held by `max_order_notional` running into cycle 2, and the three new
  detail lines; an ignored line leaves the plan to the next candidate.
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
  heading and the candidates and plan under the Budget header; JSON carries
  `shadow`, `never_skip_veto`, `budget` and the snapshot `budget_reduction`
  status with `shadow_reason`, `candidates`, `plan` and
  `ranking_without_rulebook`; the SPA renders the rows as blocked proposals
  through the existing blocker path.
- Redacted before/after artifact: none against a live gateway (fixtures only,
  by instruction).
- Residual risk accepted by the user: the caps are measured on marks, so a wide
  or stale quote can over- or understate a line until the next refresh; the
  per-line cap uses the declared figure, not the effective one, so after a
  large drawdown the cap is looser relative to what is actually at risk than
  the percentage suggests. Both are stated on the row and in the docs.
