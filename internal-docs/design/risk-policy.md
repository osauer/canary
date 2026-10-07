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
   `config.toml` `[trading]` as written (the compiled 10,000 / 10 / false /
   false where a key is absent; the contracts were 5 until the owner decision
   2026-10-07 08:30 CEST made the compiled values the Balanced preset), plus
   pct 10.0 (5.0 before that decision) and ceiling 100,000, raises
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
9. **Bond maturity (owner decision 2026-10-06 20:17 CEST, "30 years";
   decision B2 of bond-orders.md).** `max_bond_maturity_years`, an integer in
   [1, 100], is the longest time to maturity a bond or bill buy may have.
   `OrderLimitsInForce.CheckBondMaturity` refuses a maturity after today's UTC
   date plus that many years (`time.AddDate`; the limit date itself passes),
   an unreadable maturity and incomplete limits, naming the key. Sells are
   never judged by it. While the key is missing only bond and bill buys are
   refused: `MissingKeysForEveryOrder` leaves it out of the completeness test
   and `BondMaturityUnset` reports it (owner decision 2026-10-07 08:27 CEST,
   after the desk settings review found that a key governing only bond buys
   would otherwise block exits). Daemon start writes 30 into a table that lacks it,
   after a backup, and raises `policy_version`; a written value is never
   changed. Its JSON tag omits the key while unset, so a file written before
   the key existed keeps both fingerprints until the key is written.
   `trading.status`, `risk_policy.snapshot`, the settings view
   (`trading.limits.max_bond_maturity_years`, read-only) and the explain view
   state it.

## Presets and Desk's order-cap saves (owner decisions 2026-10-07 08:30 and 08:33 CEST)

Design: Desk `output/presets/2026-10-07-presets-design-final.md`. Three
presets size five keys across two files, the reserve pair of `[cash.sweep]`
and `max_order_floor_base`, `max_order_pct_nlv`, `max_option_contracts` here:
Cautious 15,000 / 15% / 5,000 / 5% / 5, Balanced 10,000 / 10% / 10,000 / 10% /
10, Aggressive 10,000 / 5% / 15,000 / 20% / 100. Balanced is the compiled
defaults by construction (`cash_policy_presets.go` reads
`cashSweepWrittenDefaults` and `order_limits_policy_file.go`). The ceiling,
the permissions and the bond maturity are covered by no preset; `Custom` is
any override of a covered key, derived by value on every `policy.cash.get`,
never stored as a label. A fresh desk reads Balanced; while `[cash.sweep]`
writes neither reserve key the stance follows the caps and the note says the
sweep is not set up yet.

1. **One write path.** `policy.cash.check` and `policy.cash.apply` take the
   three cap keys as `order_limits.<key>` changes (platform-settings.md). The
   constitution is written first, under the risk policy manager's `fileMu`
   (which `reload` holds too), with a backup, its changed lines' provenance
   (`set in Desk <time> from the Cautious preset, confirmed …; was …`) and
   `policy_version` raised by one; the manager reloads it at once. Then the
   protection file follows under its own lock. A failure after the first
   write is reported as a partial save (`partial` on the result and the
   receipt); nothing is rolled back, since old bytes would read as drift. The
   `canary policy preset` command of the design is not built: Desk is the one
   path, and the files stay editable by hand.
2. **Device, verified here.** A change to any cap key, in either direction
   and with no window, is accepted only with a confirmation Canary verifies
   itself against `[desk_device]`: the public key of the credential Desk
   enrolled, pinned by the owner once, by hand, from the line Desk's
   Settings shows (`companion = "<key id>:<base64url P-256 point>"`, the key
   id being base64url of the first 16 bytes of the point's SHA-256 as Desk
   derives it; `passkey = "<credential id>:<point>"`). A malformed line never
   refuses the constitution: the loader keeps the policy and cap changes are
   refused with the line's error. The envelope carries the device's signature over Desk's
   digest, which chains to these exact terms and the review the owner saw
   (`cash_policy_device.go`); a reliance (`confirmed_by`), an unsigned
   envelope, an unknown key or a broken chain is refused with
   `confirmation_unverifiable` and nothing is written. Cash-only saves are
   verified when the envelope carries a signature and a key is pinned, and
   kept for audit otherwise, as before. No pairing or key exchange runs
   between Desk and Canary; the table is in the fingerprint, so changing it
   is a revision.
3. **Journal.** Each constitution revision written this way is a governance
   event in `risk_policy_events`, kind `order_limits_revision`, with the
   request and Desk action ids, the credential and whether it was verified,
   the versions, the fingerprint and each cap before and after.
4. **Consequences.** Each cap key that rises carries one sentence at today's
   NLV (one per quantity: the floor and the share describe the one cap), so a
   loosening needs the device by the existing rule as well; a tightening
   needs it by rule 2. `policy.cash.check` returns `preset_from`,
   `preset_to` and `device_required`; the terms and the companion's review
   fields are unchanged (`expected_revision` now names both files' bytes,
   `<protection digest>+<constitution digest>`).
5. **Restore.** Receipts carry `preset_before` and `preset_after`; while the
   files read a preset, `policy.cash.get` serves `restore`, the owner's values
   before the most recent save from Custom to a preset, withdrawn once the
   files' revision is not the one Canary's latest save wrote.
6. **Out-of-process writes.** `canary policy ensure --apply-plan` takes the
   daemon's instance lock and refuses while a daemon holds it.

What still bounds cost and risk under Aggressive, the only preset that
loosens: the 100,000 ceiling (and with it the override's reach), unchanged
under every preset; every other gate (freeze, pins, WhatIf, preview tokens,
journal integrity, origin gating, the drawdown brake, the Rulebook, sell-only
and the governor); the owner's approval of every order, a daemon-sent order
existing only under a pre-authorised bucket, which no preset lists; the
sweep's own bounds, which no preset covers; and the device for every cap
change. Worst case at a synthetic 240,000 book: one wrong new order of 48,000
instead of 24,000, an opening option order of 100 contracts instead of 10,
idle cash down to 12,000 from 24,000. Delta-reducing exits pass the caps under
the 08:13 decision, so no preset makes the book less able to cut risk.

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

## Delta-reducing exits (owner decision 2026-10-07 08:13 CEST; open questions decided 09:51 CEST)

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
   and it lowers the absolute net delta of its underlying and of the whole
   book (owner decision 2026-10-07 09:51 CEST, "Both"), measured with the
   same position deltas Canary already uses for its daemon-side risk verdicts.
   If any delta the test needs is unknown or stale, the order does not qualify
   and the cap applies (fail closed). An exit that would leave a short option
   uncovered keeps the cap (owner decision 2026-10-07 09:51 CEST, "Keep the
   cap"; item 9 below).
2. **The measure.** `positionDollarDelta` (handlers.go), which rule 15's net
   exposure (`mapRuleNames` → `GroupDollarDeltaBase`) and the portfolio reduce
   sweep (`netPortfolioDollarDelta`) already read: shares × mark for a stock
   or ETF, delta × contracts × multiplier × model spot (else previous close)
   for an option, converted to base with `positionBaseRate`. The underlying
   is the order's contract symbol; its legs are every held equity row that
   quotes as a stock and every option row with that symbol. Bills, bonds and
   conversions carry no equity delta and are neither legs nor candidates;
   any other non-equity row the positions file among the stocks (a future,
   index, CFD, fund or warrant) carries delta the daemon does not measure, so
   it keeps the cap and the refusal names the line (the policy check's book
   lists it as an unmeasured line and judges no exit while one exists). No
   second kind of delta is computed: `deltaLegBase` is the one per-row
   measurement, and the policy check's book reads it too, with no fallback
   the gate lacks (a row without its own FX rate is unmeasured for both).
3. **The test.** `measureDeltaReduction` reads one `positions.list` result of
   the order's own account and mode (`currentPortfolioAuthority`), sums the
   legs' base dollar deltas into the net before, and keeps the per-unit
   delta and held quantity of each contract the order touches.
   `deltaReducingExit` then applies the order's quantity change (every leg of
   a strategy combo) and passes when |after| < |before|; the measured held
   quantity must equal the exact position evidence the order was judged
   against. Any stale row, missing delta, spot or FX rate, non-current or
   other-account read, duplicate or unheld contract leaves the evidence not
   current, and the refusal names the reason in plain words (the line as the
   Rulebook describes it, never a contract id or a raw error). Two exits of
   one line judged against the same position would together flip it, so the
   exemption also needs the broker's complete open-order inventory
   (`captureProtectiveExitInventory`, now read for every candidate from the
   cached protection snapshot): this order and every other working order in
   its direction on the contract (per leg for a combo, hand orders included)
   must stay within the held quantity; an unavailable inventory keeps the
   cap and the refusal says so. The cached snapshot is served unchanged for
   up to `protectionOrderSnapshotRefreshEvery` (45 s) and nothing refreshes
   it when Canary places an order, so the count also takes Canary's own
   journal rows (`journalOrderViewsForInventory`): an open row in scope that
   no snapshot row pairs and that moved after the snapshot counts, a paired
   row never twice; only when the journal cannot be read is the inventory
   read from the broker afresh. An unpaired row counts when it moved after
   the snapshot's request could have begun: the snapshot carries only its
   completion time, so the margin is the flight's own budget
   (`journalInventoryMargin` = `orderReconcileSnapshotWait`, 15 s), and the
   broker's acknowledgement lag never reads as settled. A snapshot row the
   broker already stamped with a PermID pairs with the journal row that has
   not seen it yet, by session order id and client id
   (`openOrderSnapshotEventMatches`), so it is not counted twice. The broker
   reports no legs for a working combo (BAG): a combo Canary placed names
   its legs in the journal (`journalInventory.legsOf`) and counts only
   against those legs, so a put-spread close beside Canary's own working
   call-spread close passes; a hand combo counts against every option leg of
   the underlying whatever the exit's direction, its units a lower bound
   (`workingOrderIdentity.competesWith`). The journal is folded whole for
   this read, as every other order read does; there is no bounded
   open-orders read model to serve it and none was added for this alone. The refusal reads "another working
   order already sells 700 of the 1,000 SYNB shares you hold; with this one,
   more would be sold than you hold. Cancel it first", counts with thousands
   separators, no "); " inside a clause (Desk reads that as a sentence break).
4. **Enforcement points.** Preview (`previewOrder`, `previewStrategyOrder`)
   and admission (`bindPreviewOrderRiskAuthority`) each read the positions
   when, and only when, the order is a close or reduction that a cap would
   otherwise refuse (`captureDeltaReductionEvidence`); any other order reads
   nothing, and a candidate the caps admit carries an `Unread` mark. Admission
   reads against the larger of the signed and the current (FX re-read)
   notional, so a cap that binds only after FX drift still finds its
   measurement. The first-byte wire guard reuses the admission reading with
   the re-read position and issues no broker request, as it does for the
   protective exit; a cap that first binds there (the book's NLV moved) is
   refused with "the order cap did not apply when you previewed this order;
   preview it again". The strategy path lifts the per-leg contract cap the
   same way and reads the inventory per leg. Proposal rows are not re-judged:
   reduce, sweep, governor and risk-reduction rows all go through the preview.
5. **Unchanged.** Freeze, account and route pins, previews and WhatIf, the
   sell-as-short re-read (`allow_stock_short`) and the sell-to-open re-read
   (`allow_option_sell_to_open`), origin gating, owner approval, the drawdown
   brake, sell-only and the governor. The protective stock exit keeps its own
   exemption: it reads the open-order inventory, not deltas, also passes the
   short re-read, and admits an over-hedged stock's stop that this rule would
   not, so it is not a special case and was not folded in. The sweep bill
   exemption is unchanged.
6. **What still bounds cost and risk.** A qualifying order can only shrink a
   line the book carries, and with every other working order in its
   direction it stays within that line, so its worst case is that whole line
   in one order instead of several: the preview prices it from a live quote
   inside the strategy's limit (nothing goes out at market), the broker
   WhatIf must accept it, the position, the measurement and the inventory are
   read again at admission, and the wire guard re-checks the position. Who
   decides to send it is unchanged by this rule, and differs by path: a hand
   order, a proposal submit and a single reduce are approved per order
   (device confirmation or the human CLI); a portfolio reduce sweep
   (`reduce-portfolio submit`) is approved as one basket of up to 25 orders;
   a queued authorisation (`daemon-owner-queued`) was signed per order by
   the owner ahead of time; a bucket listed in the protection policy's
   `[authority] pre_authorised` (`trailing_stop`, `option_loss_exit`,
   `option_profit_trail`, `budget_reduction`) is sent by the daemon
   (`daemon-preauthorised`) after a notice and the veto window with no
   per-order approval, so for those buckets the exemption now admits an
   option exit above the caps on the standing policy alone (their stock
   stops were already exempt as protective exits); the protective stop guard
   (`daemon-protective-guard`) only shrinks or cancels Canary's own stock
   stops; the cash sweep's bills never qualify. The owner decided this
   (2026-10-07 12:28 CEST, "Yes, pass"): a pre-authorised bucket's option
   exit passes the caps in one order, unattended, when it meets every exit
   condition. What remains is the price impact of one large exit against its
   limit, unattended for those buckets, bounded by the line, both deltas,
   the cover rule, a limit inside its bounds and the veto window; the owner
   accepts it by approving a preview that shows the whole quantity, or, for
   a pre-authorised bucket, by listing the bucket.
7. **Policy check.** `lot_above_trading_max` now reports an option line above
   the cap only when its single-contract close would not lower the
   underlying's absolute delta or the delta cannot be measured (the book
   carries `Underlying` and `DollarDeltaBase` per line, from the same
   measure); `order_cap_splits_reduction` counts a whole option-line exit only
   when the cap still binds it; `cap_above_trading_max` keeps the cash sweep
   and hands `risk_reduction` and `budget_reduction` to the new info rule
   `reduction_cap_above_order_cap`, which says that such a bucket's rows pass
   only as delta-reducing exits.
8. **The whole book (owner decision 2026-10-07 09:51 CEST, "Both").** The
   first cut judged the underlying alone, which let a close of index puts
   that hedge a net-long book of single stocks pass (it lowers the index's
   own absolute delta while raising the book's). Now the exit must lower
   both. The book is measured in the same positions loop with the same
   per-row measure as `netPortfolioDollarDelta` (reduce_portfolio.go):
   `deltaLegBase` = `positionDollarDelta` × `positionBaseRate`, from the
   current same-account read; `BookBefore` is kept beside `NetBefore`, and
   the order's own change moves both. Fail closed: a line anywhere in the
   book that cannot be measured keeps the cap and the refusal names that
   line. Two cases do not make the book unmeasurable: bills, bonds and
   conversions carry no equity delta and count as zero (the loop skips them),
   and a stock row Canary marks stale only because it is a zero-value row
   (`zeroValueStockPositionCode`, "likely inactive or defunct") measures as
   a known zero. The refusal names both figures: "the exit does not lower the
   absolute delta of the whole book (95,400 EUR before, 135,400 EUR after)".
   Tested: the index-hedge close and the cover of a short stock in a
   net-long book keep the cap; a stock sale in a net-long book passes; one
   unmeasured line elsewhere keeps the cap; a defunct zero-value row and a
   bond row do not.
9. **Uncovered short legs (owner decision 2026-10-07 09:51 CEST, "Keep the
   cap").** Decided on the case of selling the stock under a covered call,
   with Canary's own rule for option combos as the principle: never leave a
   short leg uncovered (internal/strategy pairs opposite-signed legs of one
   underlying into a unit whose exits stay together; option_exit_units.go
   refuses a single-leg order for a unit). After the order, the underlying's
   short calls need long shares (contracts × multiplier) or long calls, and
   its short puts need short shares or long puts; an exit that takes that
   cover away (uncovered contracts after > before) does not qualify and the
   refusal says "this sale would leave 2 short calls on SYNB uncovered, so
   the order cap applies". Covered: a stock sale under covered calls, a
   buy-back of short shares that cover short puts, and the sale of the long
   leg of a same-right spread (vertical, calendar, diagonal) on its own;
   closing the spread as one combo passes. Not covered, by design, with no
   margin rule invented: a long put as cover for a short call or a long call
   for a short put (a risk reversal is one of Canary's units but not
   coverage), options of another underlying (an index hedge), and cash as
   cover for a short put (a stock exit does not change it, so it never
   triggers). Cover by expiry (owner decision 2026-10-07 12:28 CEST,
   "Expires no earlier"): a long option covers a short option of the same
   right only if it expires on or after the short one's expiry; strike does
   not matter; a near-dated long in a calendar does not count; a long with
   no known expiry covers nothing (`uncoveredShares`: the latest-expiring
   short takes its cover first, since a long that covers it covers every
   earlier short too, and shares cover any expiry and apply last). An exit
   keeps the cap when, after it, a short of a right is uncovered, the
   uncovered count has not fallen, and the order either made it so or sold
   cover of that kind (`removesCover`): so selling stock under short calls
   whose only long calls expire earlier keeps the cap, a long call expiring
   on or after the short one still covers, and selling the near long leg of
   a calendar on its own keeps the cap (the far short stays uncovered),
   while buying back the far short is judged by the delta rule alone and
   reducing or closing the calendar as one guaranteed combo, which lowers
   the uncovered count, passes. The `policy check` book carries each
   option line's right, expiry and multiplier so `exitLowersAbsoluteDelta`
   judges exactly as the gate does (`deltaReductionEvidence.judge`), minus
   the working-order inventory.
10. **Still open.** A stock exit above the cap now passes the cap but still
    needs `allow_stock_short`, and an option sell-to-close still needs
    `allow_option_sell_to_open`, so with both false the owner's large exits
    remain refused by the re-reads, not by the caps. Bills and bonds keep the
    cap under this rule (no equity delta) unless the sweep bill exemption
    covers them.

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
