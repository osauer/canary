# Risk Constitution (risk-policy.toml)

Updated: 2026-10-07 08:42 CEST
Status: phase 1 implemented 2026-07-12 (advisory/shadow only); 2026-10-05 adds
[order_limits], the one pre-trade hard gate in this file (see Order limits
below); 2026-10-07 adds the delta-reducing exit exemption to its two caps
(see Delta-reducing exits below); v2 adds
[recon] 2026-07-13 (internal-docs/design/post-trade-truth.md); v3 2026-07-18 adds
statement-authoritative flows and the clean-report auto-extend. Interview
decisions approved by the operator on 2026-07-12; every numerical threshold
remains unapproved until the operator writes it into the policy file.

The machine-readable policy is the constitution. `~/.config/ibkr/policies/
risk-policy.toml` is the single authority over personal capital numbers;
code owns schema, validation, calculation, and non-overridable invariants;
daemon runtime state owns observed and derived facts; `canary policy show
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
5. **Resumption (operator decisions 2026-09-23):** with
   `drawdown.release = "automatic"` the brake releases itself on a fresh,
   finite, same-account equity observation when
   approved drawdown is strictly below the existing block threshold. The
   policy manager must be active, the base currency proven and matching, and
   equity and reconciliation clocks current. Missing, stale, future or
   out-of-order evidence cannot release it. Recovery preserves the adjusted
   peak, flows and loss history; a renewed breach creates a new episode.
   SQLite commits the release and `drawdown_latch_recovered` event together;
   a failed commit keeps the brake. This applies to existing confirmed and
   provisional latches. Statement replay may still dissolve an original
   breach explained by a withdrawal. `reset-drawdown` remains an optional
   human decision to accept losses and rebase the peak, never a routine chore.
   Release is opt-in (amended 2026-09-23 22:45 CEST): `manual`, the default
   when the key is absent, keeps the original rule that only a journaled human
   reset clears a confirmed latch, so an upgrade never unlatches a brake by
   itself. This follows v3.10.0, which shipped every automation inactive by
   default. An unset key stays out of the constitution fingerprint.
6. **Exceptions:** one-shot overrides (human-only, single control, reason,
   hard expiry, journaled with fingerprint) for time-bounded exceptions;
   fingerprinted revisions for durable change.
7. **Stale/unreconciled data:** posture follows enforcement class.
   Advisory/shadow: unknown + disclosure, never a silent pass. Promoted
   hard (future): fail closed for risk increases.
8. **Cadence:** routine brief viewing and clean monthly evidence are automated,
   never human attestations. Reconciliation lapses flow through the staleness
   posture; only exceptional repair returns to the operator.
9. **Sibling pins (operator decisions 2026-08-14/15):** a changed sibling
   policy is bookkeeping by default — the sibling file edit is the human act,
   and pin re-approval is a ritual on a single-trader desk. Default-mode
   human surfaces (policy show text, brief row, narrative, SPA) render pins
   only when a live identity is unreadable; the typed JSON keeps the rows in
   every mode. The v4-gated `inventory.require_signoff = true` key restores
   the strict posture (visible section, attention row, drift nudge, blocked
   monthly pulse) for regulated environments.

## Order limits (owner decision 2026-10-05 19:56 CEST)

The four per-order gates that `config.toml` `[trading]` held
(`max_notional`, `max_option_contracts`, `allow_stock_short`,
`allow_option_sell_to_open`) and the runtime `canary settings` override of
them are risk limits, so they move here as `[order_limits]`, and the notional
cap scales with the book. Implemented 2026-10-05 20:28 CEST.

1. **Keys.** `max_order_floor_base`, `max_order_pct_nlv`,
   `max_order_ceiling_base` (base currency, percent, base currency),
   `max_option_contracts`, `allow_stock_short`, `allow_option_sell_to_open`.
   Cap in force = min(ceiling, max(floor, pct / 100 × NLV)). Worked check:
   NLV 240,000 EUR gives 12,000 (pct binds); 150,000 gives 10,000 (floor);
   2,500,000 gives 100,000 (ceiling); NLV unreadable gives 10,000 (floor,
   flagged in the summary and the refusal).
2. **Read from the file only.** Compiled numbers exist solely to be written
   into the file (`order_limits_policy_file.go`). A missing key, a missing
   table or a missing constitution refuses every order preview with the
   `order_risk_limit` blocker naming the key; proposal rows read blocked with
   the same blocker. Unlike every other control here, an absent value fails
   closed: the gate cannot judge an order without it.
3. **NLV.** The last current account read of the selected account in its base
   currency (`recordOrderLimitsNLV`), at most 15 minutes old; a preview reads
   the account again past 5 minutes. Older, missing, other-account or
   other-currency readings bind the floor. A `capital.base_currency` that
   differs from the order's account base currency refuses every order.
4. **Enforcement points.** `validateOrderRiskAuthority` at preview, at
   admission (`bindPreviewOrderRiskAuthority`, with the limits in force then)
   and at the final wire guard, with the limits read before the control lease.
   The blocker code stays `order_risk_limit`; refusals name the cap in force
   and how it is bound. Proposal readiness, the plausibility rules and the
   protective-exit readiness use the same evaluation.
5. **Exemptions, against the cap in force.** Protective stock stops within
   the long position (protective-stop-guard.md), same-currency sweep bills
   within the sweep's own cap when `bills_exempt_from_trading_max_notional`
   is true (cash-sweep.md), and, since 2026-10-07, closes and reductions that
   lower the absolute net delta of their underlying, which also pass
   `max_option_contracts` (Delta-reducing exits below).
6. **Exception path.** The runtime override is replaced by the one-shot
   override (decision 6 above): `canary policy override --control
   order_limits.max_order_floor_base --reason ... --hours N` lifts the floor to
   the ceiling until it expires. It fits because it is already human-only,
   reasoned, bounded by `override.max_duration_hours`, journaled with the
   fingerprint and self-expiring; it carries no value, so its meaning is fixed
   to "the ceiling", which the file still bounds. No other `[order_limits]` key
   accepts an override; changing them is a revision.
7. **Migration.** `canary policy ensure` writes each missing key from
   `config.toml` `[trading]` as written (the compiled 10,000 / 5 / false /
   false where a key is absent), plus pct 5.0 and ceiling 100,000, raises
   `policy_version`, and backs the file up when daemon start (or a reviewed plan)
   applies it;
   a written key is never changed. A new constitution file is written with the
   table filled the same way. The `[trading]` keys stay parseable (pointers,
   no compiled default), are never read for a decision, print as `retired` in
   `policy show --explain`, and `policy check` warns
   (`retired_trading_gate`) while one remains with a different value;
   `order_limits_missing` is the error for a missing key.
8. **Surfaces.** `rpc.TradingStatus.OrderLimits` and
   `rpc.RiskPolicyResult.OrderLimits` (`risk.OrderLimitsInForce`: cap, bound
   floor/pct_nlv/ceiling/override, NLV used or why not, summary, missing
   keys); the explain view prints the keys and an `order_limits.cap_in_force`
   row; the settings view reports the limits read-only with source `policy`.

## Authority

| Concept | Authoritative source | Typed field/contract | Renderer/tool | Fallback or unavailable state |
|---|---|---|---|---|
| Capital numbers, ladder, override cap, process cadence, sibling pins | `risk-policy.toml` (no embedded default) | `risk.Constitution` | `canary policy show [--explain]` | missing file/key ⇒ `unapproved`, never a code value |
| Single-issuer concentration: the issuer cap and trim level, illiquid bands, hedge credit, takeover gap, issuer groups, clusters, delta-swing and loss-budget watches | `rulebook-policy.toml` (Rulebook rule 1 and rules 16-18, amendment 15 of the Rulebook design) | `risk.RulebookPolicy`, `RulesResult` rows 1 and 16-18 | `canary rules`, `canary rules policy`, stress concentration row, risk-reduction bucket | the stress read and the protection policy define no concentration threshold of their own; rule 18 needs the effective risk capital above and reads unknown without it |
| Net market exposure: the whole book's signed stock-equivalent exposure with hedges, and its regime-banded watch and act levels | `rulebook-policy.toml` (Rulebook rule 15, amendments 16 and 17 of the Rulebook design) | `risk.RulebookPolicy` regime sets, `RulesResult` row 15, `StressPortfolioSummary.NetExposure` | `canary rules`, `canary rules policy`, stress exposure row and `net_delta_high` | the stress read defines no net-exposure measure or level of its own; it takes rule 15's verdict (an act under the confirmed regime set is urgent); without a rule 15 measurement the exposure row is a data-quality watch |
| Margin headroom: the broker's excess liquidity as a share of NLV (the worse of current and look-ahead), and its watch and act levels | `rulebook-policy.toml` (Rulebook rule 19, amendments 18 and 19 of the Rulebook design) | `risk.RulebookPolicy` margin-headroom keys, `RulesResult` row 19, `StressPortfolioSummary.MarginHeadroom` | `canary rules`, `canary rules policy`, stress margin row and `margin_cushion_low` | the stress read defines no margin-cushion level of its own; it takes rule 19's verdict (watch is a watch, act an act); without a rule 19 measurement the margin row is a data-quality watch |
| Per-order limits: notional cap floor, NLV share and ceiling, option contracts, stock short and option sell-to-open permissions | `risk-policy.toml` `[order_limits]` (no embedded default) | `risk.ConstitutionOrderLimits`, `risk.OrderLimitsInForce` | `canary policy show [--explain]`, `canary trading status`, settings view (read-only) | missing key or file ⇒ every order preview refused (`order_risk_limit` naming the key); NLV not current ⇒ floor |
| Delta-reducing exit: the underlying's signed net dollar delta before an order and per unit of each contract it touches, in base currency | one current `positions.list` read of the order's account, with the deltas the daemon's verdicts use (`positionDollarDelta`, `positionBaseRate`) | `deltaReductionEvidence` (daemon-internal; `measureDeltaReduction`, `deltaReducingExit`) | the refusal text of `order_risk_limit`; `policy check` rules `lot_above_trading_max`, `order_cap_splits_reduction`, `reduction_cap_above_order_cap` | positions not current, another account, a stale row, a leg without delta, spot or FX rate, or a contract not held ⇒ not measured ⇒ the caps apply and the refusal says why |
| Schema, validation, evaluation, explain text | code | `internal/risk/constitution*.go` | all | n/a |
| Policy identity | manager | `rpc.RiskPolicyResult.PolicyFingerprint` (`risk-constitution-fp-v1`) | policy show, journals | absent |
| Adjusted peak, drawdown tier, latch, flows, overrides | daemon runtime state | daemon.db `risk_capital` state document plus `capital_events` | policy show | unseeded ⇒ tier `unknown`; storage failure ⇒ unavailable |
| Governance evidence | daemon | daemon.db `risk_policy_events` plus append-only event payload carrying the fingerprint | phase-3 replay | storage failure is disclosed; no file fallback |
| Equity observation | account summary success path (`handleAccountSummary`) | `AccountResult.NetLiquidation/AsOf` | policy show `input_health` | persisted last reading, disclosed stale |
| Preview cause | `riskPolicyPreviewWarnings` | `DataWarning{Code:"capital_drawdown", Scope:"risk_policy"}` | order preview | absent when policy nil or tier ok/unknown |

Manager semantics: protection-policy manager vocabulary (strict unknown-key
rejection, version-bump-required drift detection, last-good retained on
error) with two deliberate differences — **no embedded default** (missing
file is status `absent`) and **no trading blockers** (v1 is advisory; a
broken constitution is disclosed loudly, it does not stop trading — that
posture flips per control if a control is promoted to hard). `[order_limits]`
is that promotion for the per-order gates: the last good policy's table
judges every order, and without one every order preview is refused.

Cash-flow adjustment: capital events (deposit/withdrawal/reconcile) are
operator-declared journal facts. Adjusted equity = equity − cumulative
declared flows; the peak tracks adjusted equity, so a deposit is not a fake
peak and a withdrawal is not a fake drawdown. A deposit whose effective
time precedes the recorded peak corrects the peak downward (never-inflate).
Since phase 3a (risk-policy v2, internal-docs/design/post-trade-truth.md) the
reconcile event is a human sign-off against a specific, fully resolved
`canary recon` report — bare attestation is retired, and the `[recon]`
policy keys define what counts as a matching exception.

Since risk-policy v3 (2026-07-18):
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

Since the two-stage latch (operator sign-off 2026-08-10):
`drawdown_block_latched` journals carry `provisional: true` plus the frozen
engagement equity in state; when statement coverage first reaches the latch
day, `IncorporateStatementSnapshotForScope` journals exactly one
`drawdown_latch_dissolved` or `drawdown_latch_promoted` with the replayed
consumed share and the statement flows value-dated through the latch day.
Pre-two-stage latches carry no provisional mark; with automatic release, current verified recovery can release them too. Promotion
does not re-alert: the engagement alert copy already describes the
unconfirmed state, and the brief, CLI, and report surfaces carry the stage.
The post-latch Flex recheck is a bounded backoff (half-hourly for the first
two hours, two-hourly through the first day, then six-hourly) that stops
when retained coverage reaches the latch day; it honors the fetch state's
own failure and pacing schedule.

## Delta-reducing exits (owner decision 2026-10-07 08:13 CEST)

The owner's decision: "tighter order cap must not block exits if they reduce
delta. That's a (design-)bug". Before it, `validateOrderRiskAuthority` held
every stock, ETF and option order, apparent exits included, to the notional
cap in force and every option order to `max_option_contracts`, excusing only
the protective stock stop and the sweep bill; a hand exit, an option close, a
strategy close, a reduction proposal or a budget-governor cut above either cap
was refused or had to be split, and a tighter cap made that worse. Implemented
2026-10-07 (`internal/daemon/delta_reduction.go`).

1. **The rule.** An order passes the notional cap and the option-contract cap
   when it reduces delta: it only closes or shrinks an existing position (it
   never opens a position, never adds to one, never flips to the other side),
   and it lowers the absolute net delta of its underlying, measured with the
   same position deltas Canary already uses for its daemon-side risk verdicts.
   If any delta the test needs is unknown or stale, the order does not qualify
   and the cap applies (fail closed).
2. **The measure.** `positionDollarDelta` (handlers.go), which rule 15's net
   exposure (`mapRuleNames` → `GroupDollarDeltaBase`) and the portfolio reduce
   sweep (`netPortfolioDollarDelta`) already read: shares × mark for a stock
   or ETF, delta × contracts × multiplier × model spot (else previous close)
   for an option, converted to base with `positionBaseRate`. The underlying
   is the order's contract symbol; its legs are every held equity row that
   quotes as a stock and every option row with that symbol. Bills, bonds and
   conversions carry no equity delta and are neither legs nor candidates. No
   second kind of delta is computed.
3. **The test.** `measureDeltaReduction` reads one `positions.list` result of
   the order's own account and mode (`currentPortfolioAuthority`), sums the
   legs' base dollar deltas into the net before, and keeps the per-unit
   delta and held quantity of each contract the order touches.
   `deltaReducingExit` then applies the order's quantity change (every leg of
   a strategy combo) and passes when |after| < |before|; the measured held
   quantity must equal the exact position evidence the order was judged
   against. Any stale row, missing delta, spot or FX rate, non-current or
   other-account read, duplicate or unheld contract leaves the evidence not
   current, and the refusal names the reason.
4. **Enforcement points.** Preview (`previewOrder`, `previewStrategyOrder`)
   and admission (`bindPreviewOrderRiskAuthority`) each read the positions
   when, and only when, the order is a close or reduction that a cap would
   otherwise refuse (`captureDeltaReductionEvidence`); any other order reads
   nothing. The first-byte wire guard reuses the admission reading with the
   re-read position and issues no broker request, as it does for the
   protective exit. The strategy path lifts the per-leg contract cap the same
   way. Proposal rows are not re-judged: reduce, sweep, governor and
   risk-reduction rows all go through the preview.
5. **Unchanged.** Freeze, account and route pins, previews and WhatIf, the
   sell-as-short re-read (`allow_stock_short`) and the sell-to-open re-read
   (`allow_option_sell_to_open`), origin gating, owner approval, the drawdown
   brake, sell-only and the governor. The protective stock exit keeps its own
   exemption: it reads the open-order inventory, not deltas, also passes the
   short re-read, and admits an over-hedged stock's stop that this rule would
   not, so it is not a special case and was not folded in. The sweep bill
   exemption is unchanged.
6. **What still bounds cost and risk.** A qualifying order can only shrink a
   line the book carries, so its worst case is that whole line in one order
   instead of several: the preview prices it from a live quote inside the
   strategy's limit (nothing goes out at market), the broker WhatIf must
   accept it, the position and the measurement are read again at admission,
   the wire guard re-checks the position, and the owner approves each order.
   What remains is the price impact of one large exit against its limit,
   which the owner accepts by approving a preview that shows the whole
   quantity.
7. **Policy check.** `lot_above_trading_max` now reports an option line above
   the cap only when its single-contract close would not lower the
   underlying's absolute delta or the delta cannot be measured (the book
   carries `Underlying` and `DollarDeltaBase` per line, from the same
   measure); `order_cap_splits_reduction` counts a whole option-line exit only
   when the cap still binds it; `cap_above_trading_max` keeps the cash sweep
   and hands `risk_reduction` and `budget_reduction` to the new info rule
   `reduction_cap_above_order_cap`, which says that such a bucket's rows pass
   only as delta-reducing exits.
8. **Open owner questions**, recorded, not decided here. (a) The rule is per
   underlying: closing a long index put lowers the index's own absolute
   delta and passes, though at book level it removes a hedge; rule 15's net
   exposure still watches that. (b) Bills and bonds carry no equity delta,
   so a bond or bill sale above the cap keeps it (fail closed) unless the
   sweep bill exemption covers it; whether a bond sale should pass as a
   risk-reducing exit is open. (c) A stock exit above the cap now passes the
   cap but still needs `allow_stock_short`, and an option sell-to-close
   still needs `allow_option_sell_to_open`, so with both false the owner's
   large exits remain refused by the re-reads, not by the caps.

## Safety invariants

- Account/route/client pins, WhatIf, preview tokens, journal integrity,
  freeze, and agent-origin gating have **no keys in this schema**; no
  revision or override can express a change to them.
- All four policy write methods are human-origin-only (`originIsHuman`);
  agent sessions read but never operate this surface.
- Apart from `[order_limits]`, nothing in this feature reads or writes
  `submit_eligible`, blockers, freeze, pins, or tokens; the capital controls
  are advisory/shadow end to end. `[order_limits]` only refuses: no key or
  override can authorize an order or relax freeze, pins, tokens or origin
  gating.
- Data absence never renders ok: `unapproved`, `unknown`, stale, and
  unreconciled are distinct disclosed states (never-false-pass).
- `block_enforcement = "hard"` is rejected by schema v1; promotion requires
  a schema revision after the phase-2/3 shadow evidence, as a deliberate
  human decision.

## Files

```
internal/risk/constitution.go          schema, validation, fingerprint, EvaluateCapital
internal/risk/constitution_explain.go  ConstitutionLimits (single copy of meanings)
internal/rpc/brief.go                  methods, params, result types (consolidated there in v3)
internal/daemon/risk_policy_manager.go TOML manager (absent/active/drift/error)
internal/daemon/risk_capital_state.go  peak/latch/events/overrides + journals
internal/daemon/risk_policy_handlers.go RPC handlers + preview cause
internal/daemon/recon_auto_extend.go   v3 clean-report auto-extend (startup + post-ingest only)
internal/risk/order_limits.go          [order_limits] schema, validation, cap in force
internal/daemon/order_limits.go        NLV reading, limits in force, floor override, blocker
internal/daemon/order_limits_policy_file.go  policy ensure writes [order_limits]
internal/daemon/delta_reduction.go     delta-reducing exit exemption: candidate, measurement, decision
internal/cli/policy.go                 canary policy show/capital-event/override/reset-drawdown/correct-peak
internal/daemon/policy_files.go        operator template (all material keys commented out),
                                       written when the file is missing; `canary policy default constitution`
```

Runtime state and governance evidence live in
`~/.local/state/ibkr/daemon.db`. The former JSON/JSONL artifacts are sealed
cutover inputs, not live fallbacks.

## Deferred (explicitly not in phase 1)

MCP `canary_policy` tool; SPA card; push alerts; Flex/Activity ingestion;
promotion of any control to hard; automated reports (phase 4); capital
allocation responses (phase 5). The stress policy fingerprint label is
`stress-policy-fp-v1` (originally `risk-policy-fp-v1`) to keep identities
unambiguous; fingerprint keys are unchanged.

## Rollback

Revert the files above. The risk-capital document and governance events remain
inside daemon.db and may be ignored by older feature code, but daemon.db must
not be deleted or replaced; the operator's TOML remains separate policy
authority. User-visible change on rollback: `canary policy` disappears and
previews lose the advisory `capital_drawdown` cause; no trading-path behavior
changes either way.

`[order_limits]` (2026-10-05) is the exception: rolling it back changes the
trading path. An older binary refuses a constitution that carries the table
(strict unknown-key rejection keeps the last good policy, or none on a fresh
start) and reads the per-order gates from `config.toml` `[trading]` again,
with its compiled defaults where a key is absent. Before rolling back,
restore the backup `policy ensure` wrote, or delete the table and raise
`policy_version`, and write the intended `[trading]` keys.
