# Cash-sweep currency priority and decision log

Owner decisions, 2026-10-02: use existing USD cash; do not propose or execute
EUR-to-USD conversions. Allocate one configurable EUR-equivalent 10,000 cushion
across native currencies. Funding allocation is delegated to engineering.

## Small configuration

Write these in the existing versioned protection policy, using its normal
review/version/fingerprint flow. Installing source never changes private policy.

```toml
[buckets.cash_sweep]
currency_priority = "usd_first"
reserve_cushion_eur = 10000.0
```

| Priority | Meaning after funding checks |
| --- | --- |
| `usd_first` | Review and process eligible USD investments first; default for a new explicit opt-in. |
| `balanced` | Review and process the largest eligible surplus in EUR-equivalent terms first. It targets no equal EUR/USD portfolio allocation. |
| `eur_first` | Review and process eligible EUR investments first. |

Liquidity restoration precedes investment within sweep rows. Other buckets
retain their risk-score precedence and automatic-execution slots. Priority
changes neither execution eligibility nor order terms. It supplies no FX
conversion, yield forecast or claim that USD always offers a better return.
Shadow/active remains the separate existing execution mode.

An old policy without either new key retains its fingerprint and behavior.
Either key explicitly opts into the new reserve design. A cushion without an
explicit priority selects USD first; a priority without an explicit cushion
selects the owner-approved 10,000 total. Invalid priorities and nonfinite or
negative amounts are refused. Zero is an explicit owner choice, never inferred.
EUR valuation currently requires an EUR-base account; another base holds.

## Allocation and unavailable evidence

In `internal/risk`, allocate the cushion proportionally to each currency's
verified additional operational/stressed funding need, measured in EUR. These
needs exclude separately reserved working-order commitments. With zero demand
in every currency, hold the cushion in EUR. Lack of a funding figure, FX rate or
calibrated scenario is unavailable, not zero.

```text
native cushion = total EUR cushion × currency's share of funding demand ÷ EUR/native rate
effective native reserve = max(existing keep_cash, funding need) + native cushion
free cash = verified cash − separately reserved commitments − effective native reserve
```

The native floor and funding need overlap, so neither is counted twice. The
cushion is additional: total EUR-equivalent reserve is the sum of native
max(floor, funding) values plus exactly the configured cushion. With fixed FX,
higher funding needs or floors cannot lower that aggregate. Funding-weighted
redistribution can still lower another currency's cushion while increasing the
required currency's cushion; the log exposes both allocations. It reallocates the accounting requirement only; it never moves
cash between currencies. A native funding deficit cannot be cleared by cash
elsewhere, a forecast sale, or a bill's collateral value.

Every relevant native reserve must be actually funded before **any** opt-in
investment. Compare broker-observed native settled cash minus commitments with
that currency's effective reserve. A USD deficit holds EUR purchases too;
earmarking USD money or expecting a bill sale cannot prove the total cushion.
Every configured currency remains relevant when its cash observation is absent.
Liquidity-restoring sales remain possible under existing sale gates. The
synthetic journal estimate never satisfies this all-currency precondition.

**Commissioning remains open.** Production has no calibrated finite-stress,
assignment/exit-horizon funding observer yet. The opt-in therefore names
`reserve_calibration_required` and emits no order proposals. It retains other
cash/settlement evidence gaps visibly. Synthetic evidence exercises the full
planner contract but cannot commission or activate the observer. Current
protected-NAV and stressed-margin checks must also pass before verified reserve
evidence can be consumed. No numeric shock or exit-time threshold is invented.
The [operational observer and frozen finite studies](cash-sweep-funding-observer.md)
now expose partial native obligations and calibration evidence. These remain
separate from admitted reserve evidence and cannot clear that commissioning hold.

EUR ≤182-day and USD ≤91-day bills, whole-order bounds, fee-inclusive funding,
advisory benefit, settlement/route/quote gates and all broker authority checks
remain separate. This design expands no broker-write authority.

## Durable decision trace

Each changed planning decision is atomically appended to `daemon.db`'s existing
append-only event log as `cash_sweep_decision`, with a compare-and-swap current
state. Scope is isolated by account and account mode. It records planning time,
policy/version/fingerprint, execution mode, currency priority, cushion,
native cash/deductions/reserve allocation, action/hold reasons and source
fingerprints. It is a planning receipt, never a claim that an order was sent.
Original Web cash time is recorded separately from TWS account/position
request-or-stream receipt clocks. TWS original field timestamps remain unknown.
Funding scenario fingerprint, evidence time/expiry and sampled planning
daemon/session epoch are recorded too. These planning-session fields describe
the sampled runtime, not a fabricated original per-field broker session receipt.

Unchanged polls, refresh clocks and provenance-only refreshes do not add events.
Changed money, reasons, policy or priority do, including A → B → A transitions.
Restart uses the stored digest. Twenty recent generations appear in the
read-only `proposals.snapshot` contract consumed by CLI, SPA and Desk; every
older event remains in SQLite. The compact UI/CLI show the five latest
generations. SQLite/audit failure holds all opt-in proposals. Existing policies
report audit unavailability without changing their previous trading contract.

The CLI exposes configuration through the existing protection-policy file and
renders the decision log under `canary proposals`. The Canary protection view
shows the same daemon-authored policy, native cash and hold reasons with a
collapsed decision log. No UI calculation or read-only RPC changes authority.

## Verification contract

Synthetic tests cover funding-weighted single-cushion allocation, overlapping
native floors, zero demand, stale/future/missing evidence, nonfinite inputs,
NAV/margin refusal, all three priorities, liquidity restoration first, no FX,
old-policy continuity, automatic slot preservation, SQLite reopen, deduplication,
policy/return transitions, recent-window retention and response clone isolation.
