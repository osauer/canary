# Writing a risk policy

Updated: 2026-10-08 19:39 CEST

The personal risk policy is one TOML file you write by hand. It holds the
capital numbers, drawdown ladder, exception cap, reconciliation tolerances, and
cadence declarations the daemon evaluates against your account, and the
per-order limits every order preview and broker send must pass.
[Trading policy](policy.md) covers who owns each control and what the system
does with the result; this page covers how to write those decisions down.

Every capital number in it is yours. There is no embedded default and no
recommended value, and nothing here proposes one. A key you do not write does
not exist, and the control that depends on it stays `unapproved`. The
per-order limits in `[order_limits]` are the exception: Canary writes their
values ([Order limits](policy.md#order-limits)), and a key missing from that
table refuses every order preview.

## Where the file lives

The daemon reads one path, a constant rather than a config key, so the file
that governs risk numbers cannot be relocated:

```text
~/.config/ibkr/policies/risk-policy.toml
```

When no file exists, the installer and each daemon start write a skeleton
there, and `canary policy default constitution` prints the same file. Every
capital key arrives commented out, the base currency and the `[inventory]`
pins included; the only values filled in are the structural envelope below
and `[order_limits]`. The capital placeholders carry no recommendation; the
`[order_limits]` values are Canary's starting limits and apply until you change
them. The file opens
with `# Canary defaults, not yet reviewed.` until you delete that line. A file
written before `[order_limits]` existed gains the table at the next daemon
start, after a backup.

Only the envelope is fixed:

```toml
kind = "canary.risk_policy"
schema_version = 2
policy_id = "risk-constitution"
policy_version = 1
```

`kind` is `canary.risk_policy` and `schema_version` 2; files with the legacy
`ibkr.risk_policy` kind or schema 1 still read (see
[Identity, revisions and permission](policy.md#identity-revisions-and-permission)).
`policy_id` is any non-empty identity string and `policy_version` any positive
integer you raise on each revision.

## What each section governs

| Section | Keys | What the numbers govern |
|---|---|---|
| `[capital]` | `base_currency`, `protected_floor`, `declared_risk_capital`, `max_equity_age_minutes`, `max_unreconciled_days` | The currency every figure is stated in, equity that is never risk capital, the money you have authorized to be at risk, and how stale an equity reading or a reconciliation may get |
| `[drawdown]` | `warn_consumed_pct`, `block_consumed_pct`, `block_enforcement`, `release` | Two tiers, each a percent of declared risk capital consumed from the cash-flow-adjusted equity peak, the enforcement class of the block tier, and how a latched brake clears (`manual` by default, or `automatic`) |
| `[override]` | `max_duration_hours` | The longest a one-shot exception may live |
| `[recon]` | `amount_tolerance_pct`, `amount_tolerance_min`, `date_window_business_days`, `max_report_age_days`, `max_equity_divergence_pct` | Which statement-versus-declared-event differences you want to look at, and how old the statement evidence may be |
| `[cadence]` | `morning.class`, `eod.class`, `weekly.class` | Which routine reviews get completion journaling |
| `[inventory]` | `rulebook`, `protection`, `stress` pins; `require_signoff` | The sibling policy versions this constitution was approved against, identity only; whether a changed sibling blocks governance evidence until the pin is updated (default off: disclosure only) |
| `[position_add]` (optional) | `admission_contract`, `max_stock_pct_nlv`, `max_underlying_stock_pct_nlv` | Owner-chosen stock allocation ceilings for opening or increasing long stock positions; no defaults. The explicit `stock-entry-v1` admission contract activates cash, allocation and portfolio-risk checks for stock increases, including ordinary orders. |
| `[portfolio_plan]` (optional) | `contract`, `valid_until`, `targets` | Advisory selection of one next portfolio action. Each exact-stock target specifies bands as a share of NLV, entry regimes, priority, price ceiling and reason. No defaults, cash reservation or execution authority. |
| `[mandate]` (optional) | `max_loss_pct`, `vol_target_pct`, `tax_reserve_base`, `started_at`, `gap_shock_pct`, `gap_room_pct`, optional `index_gap_shock_pct`; `[mandate.sleeves]` `owner_vol_pct`, `core_vol_pct`, `overlay_vol_pct`, optional `owner_stop_pct` | The mandate: the largest loss of the risk base (equity after the tax reserve, cash-flow adjusted from the start date) you tolerate, the volatility budget at full room that sizes the sleeve allocations, and the one-name gap stress (a broad index fund or index takes `index_gap_shock_pct` instead when written). Written, the daemon measures the book's loss measure (every bought option at its premium at risk, protection included, plus the cash loss to the stop of every share line a working stop covers; an unstopped line counts nothing and is named) against the loss room (`max_loss_pct` of the budget base, less the drawdown taken), measures the book's ex-ante volatility as the 1-day swing, and enforces sleeve allocations on additions (`canary mandate`; details below). Reductions, closes, cancels and hedges retain their existing gates. Absent, nothing changes. |
| `[mandate.iv_fallback]` (optional) | `short_band_pct`, `month_band_pct`, `long_band_pct`, `measured_pct`, `range_pct` | What an option without its own implied volatility may do to the verdict. Before a session's first quote, after a restart or outside an option's market hours, the position row carries no volatility; Canary then prices the option at the volatility its own mark implies. Written, the verdict runs the book with every such option moved down and up by its band (under 8 days to expiry, 8 to 35 days, beyond), and judges half the resulting range of the S&P-fall figure against the loss room: within `measured_pct` the verdict reads as measured, within `range_pct` only when it is the same at both edges, beyond it unknown. `canary mandate` lists the options, their bands and the range. Absent, or with a key missing, one such option leaves the verdict unknown, as before. Beside a written `[mandate]`, daemon start writes this table once with 65, 15 and 8 for the bands and 5 and 20 for the tiers (owner decision 2026-10-29 07:06 CET), after a backup and with a higher `policy_version`; a table you wrote is kept, and one you delete afterwards is not written again. |
| `[mandate.index_hedge]` (optional) | `max_contracts` | The Reduce index hedge: Micro E-mini S&P 500 futures sold until a 10% S&P fall leaves the book about even ([Protection](../operate/protection.md)). The table's presence is the permission to place one: absent, or without `max_contracts`, the plan still prices the hedge and placing it is refused with the table named. `max_contracts` is the most contracts one hedge may hold after it is placed, a hard stop against a wrong size, never the target. There is no second switch. Beside a written `[mandate]`, daemon start writes this table once with `max_contracts = 25` (owner decision 2026-10-29 07:06 CET), as it writes `[mandate.iv_fallback]`; delete it to keep the hedge unplaceable. The table only permits placing: a hedge is sold only when you ask for it and confirm the order on your device. |
| `[mandate.core]` (optional) | `rule`, `isin`, `currency`, `scale_pct`, `band_pct`, `min_order_base`, `max_value_pct`; optional `con_id`, `exchange`, `kill_drawdown_k` (needed once `stage` is `live`) | The core sleeve: one broad index fund held while its month-end close is above its 10-month average, sized to `[mandate.sleeves]` `core_vol_pct` of the budget. The fund is named by `isin` and `currency`: Canary uses the line IBKR lists under that ISIN in that currency, reads its daily closes on the listing's primary exchange and judges sessions and decision days on that exchange's calendar; `canary core` shows the contract and venue. If IBKR lists more than one line, or a different line than last time, the core holds until you choose one with `con_id`; a written `exchange` must be one IBKR lists the line on. Written, the daemon decides its target each month and `canary core` shows the decision, the target shares, the holding and what the rule would trade. While `stage` is unwritten or `shadow` it proposes nothing; written `live` with `spec_hash`, each month's decision becomes at most one `core_rebalance` proposal row you confirm on your device (a DAY limit at the live mid, sized to the frozen target and the mandate's headroom, served for the first five sessions after the decision that the US also trades; a sale to zero continues until sold), never pre-authorised: sent on your device confirmation, or by an authorized controller under the device-confirmed standing mandate, while a row served for your review (the core holds) is yours alone; `canary core` shows the pending row and the last decision. Protection's automatic sales leave the core's line out, and the mandate's forced reduction still covers it. A missing key holds the core and is named there, `kill_drawdown_k` included while the core is live (its bound is in units of the sleeve invested at its stage; 1 or more refuses the file). Absent, nothing changes. |
| `[mandate.strategies.<id>]` (optional) | `rule`, `spec_hash`, `sleeve`, `risk_allocation_pct` (not for the overlay), `stage`, `scale_pct`; for the rule `timed-holding-v1`: `isin`, `currency`, `signal`, `band_pct`, `min_order_base`, `max_value_pct`; optional `con_id`, `exchange`, `kill_drawdown_k` | A later machine strategy of the strategy register, one table per strategy: the compiled rule it runs, the lab spec it cites, its sleeve and risk allocation, its ladder stage (`shadow`, `live` or `suspended`) and the scale it sizes at. Under `timed-holding-v1` the entry names its fund by ISIN and currency (the account's base currency), a signal from the library (`trend-sma10m`: held while the month-end close is above its 10-month average) and the core's numbers, and the evaluator decides it as it decides the core; `canary strategy ID` shows the decision, the target, the paper position and what the rule would trade. A missing key holds that strategy and is named there, `kill_drawdown_k` included while the entry is live (its bound is in units of the sleeve invested at its stage; 1 or more refuses the file). Absent, nothing changes. |
| `[order_limits]` | `max_order_floor_base`, `max_order_pct_nlv`, `max_order_ceiling_base`, `max_option_contracts`, `allow_stock_short`, `allow_option_sell_to_open`, `max_bond_maturity_years`, `reduce_allowance_pct`, `reduce_option_allowance_pct` | The per-order notional cap, which scales with net liquidation value between the floor and the ceiling, the option contract cap, whether an order may open a stock short or sell an option to open, the longest maturity a bond or bill buy may have, and how far beyond the bid or ask a percent reduction's limit may sit (stocks and ETFs; options, at least one tick). Every order preview and broker send must pass them, apart from the protective-stop and sweep-bill exemptions ([Order limits](policy.md#order-limits)) |

The verdict reads within while the loss measure is inside the loss room and no name breaches its gap cap, reduce over the room or on a gap breach, and unknown while anything is missing. Sleeve allocations are enforced on additions. A sleeve must fit its allocation both before and after an addition, measured both by its contribution to the book's risk and by its standalone risk. Unknown or stale evidence, an addition that creates or deepens a gap-cap breach, a stock order that would take the loss measure over the loss room, and the maximum-loss boundary prevent additions. Forced reduction cuts the name with the most premium at risk first, its largest position within it, until the book fits 95% of the loss room; it keeps waiting for your approval on the device. Other order gates remain binding.

Risk-adding option purchases also count premium committed by pending buy
orders against the existing name and portfolio limits. An uncertain send or
unresolved fill keeps that capacity unavailable; a new timestamp alone does
not prove that holdings include the fill. If Canary cannot bound a pending
combo's bought-option premium, the next risk-adding option purchase waits for
clear order and position evidence. The existing protective-put entry exemption
is unchanged.

Machine allocations reserve risk only at the effective live scale of a rule that sends orders; a live strategy on hold retains its reservation while holding positions. The core's rule sends since 3.23 (its monthly rows), so a core written `live` reserves its scale; a core in shadow, as the live file runs it, and the overlay without a strategy reserve nothing. The owner receives the remainder; `owner_vol_pct` remains recorded-only. The core contract belongs to the core sleeve regardless of who buys it.

`owner_stop_pct` (optional) gives the owner's sleeve a drawdown stop of its own inside the whole book's maximum loss: the sleeve's equity-like measure is the book's cash-flow-adjusted risk base less the machine sleeves' marked P&L since the start, kept against its own high-water mark. When its drawdown reaches the stop, the forced reduction cuts the owner's sleeve, reduce-only, to half its risk at the stop, through the same batch, veto window and pre-authorisation as a budget breach, and the owner's sleeve adds nothing until the drawdown is back under the stop's release, five points under the stop (a 25% stop releases under 20%); the target is pinned for the engagement, so a drawdown oscillating around the stop cuts the sleeve once, and a new engagement after a release pins half the use at that time. Machine sleeves are untouched. `canary mandate` shows the sleeve's drawdown, high-water mark and stop state. Absent, nothing changes.

The schema bounds the shape of these numbers, never the level. Percentages must
sit in `(0, 100]`, `warn_consumed_pct` must be below `block_consumed_pct`,
`declared_risk_capital` must be positive, `protected_floor` must not be
negative, `max_order_floor_base` must not exceed `max_order_ceiling_base`, and
`max_bond_maturity_years` must lie between 1 and 100.
Choosing the values inside those bounds is your decision alone.

`canary policy show --explain` is the field inventory: it prints every key with
its meaning, current value, source (`file`, `default`, or `unapproved`), and
enforcement class (`hard` for `[order_limits]`), followed by the order cap in
force (source `in force`). The [configuration reference](../reference/config.md)
covers `config.toml`, the protection and opportunity policies, and runtime
settings; the risk policy is not among them.

Effective risk capital is the lesser of `declared_risk_capital` and equity above
`protected_floor`, so a deposit raises equity without raising the budget; only a
revision does that. If `capital.base_currency` differs from the account's base
currency, the equity observation is reported as unusable for capital math rather
than converted.

The brief's capital row (`ready.capital` in `canary brief --json`) carries the
figures this file declares beside the measured state, so a reader can place
the consumed share against the ladder without opening the file: `warn_pct` and
`block_pct` from `[drawdown]`, `protected_floor_base` and
`declared_risk_capital_base` from `[capital]`, and
`effective_risk_capital_base`, the money at risk (max) —
min(declared, equity − floor). All five are nil while no constitution is
active or any material key is unapproved; a partially written policy renders
as undecided, never as the subset of numbers that happen to exist. The
premium-at-risk row gains `pct_of_risk_capital`, long-option market value as a
percent of the declared figure, nil when either side is missing. The
[budget reduction](../operate/protection.md#budget-reduction) bucket measures
against the same declared figure.

## What the file cannot do

Both schema versions accept `shadow` (the default when the key is empty) and
`advisory` for `drawdown.block_enforcement`. They reject `"hard"` with a
targeted error, so the drawdown ladder itself cannot block an order today.
The mandate, allocation and `[order_limits]` gates can refuse orders. A warn or block tier
attaches an advisory `capital_drawdown` cause to a risk-increasing order
preview and leaves submit eligibility untouched; close and reduce previews, and
a long put on the Rulebook's hedge index list, never carry it. Cadence classes
accept `advisory` only.

The schema has no key for account or route pins, freeze, preview tokens, broker
WhatIf, or origin gating, so no revision can express a change to them, and an
override naming a key outside the constitution is refused with "safety
invariants have no keys and cannot be overridden". Unknown keys fail the load
outright rather than being ignored. `trading.freeze` is a runtime setting, not
policy: it changes only through `canary settings set` from an interactive human
terminal, and agent and paired-device origins are rejected. The per-order
limits are no longer runtime settings; they are this file's `[order_limits]`.

## Making a revision effective

1. Edit the file and raise `policy_version`. Identical content at the accepted
   version keeps running. Changed content at the same or a lower version
   reports `drift`, and the daemon keeps the last accepted version in memory.
2. Wait. The manager rereads the file every 30 seconds.
3. Run `canary policy show`. The header gives the status, policy id, version,
   short fingerprint, and path; add `--explain` for the limit rows or `--json`
   for the typed result. It works without gateway connectivity, falling back to
   the persisted last equity reading.
   - `error` means bad syntax, an unknown key, or failed validation. The last
     good policy stays active and the message names the problem.
   - `absent` means the file is missing. A previously loaded policy stays
     active; deleting the file is not how you retire one, and the next daemon
     start writes the placeholder skeleton again.
4. Read the "Waiting on your decisions" list. Any material key missing there
   makes the whole capital tier `unapproved`, not only the control that needs
   it.

Raising `policy_version` past certain points enlarges the decision set.
`recon.max_equity_divergence_pct` is accepted only at version 3 or above and
becomes material there. The version-4 `[cadence.nudges]` and `[cadence.monthly]`
tables are different: every cadence value has a built-in default — the nudge
clock is the machine's own timezone, the reconcile warning starts 2 days out,
and the monthly pulse falls on the first working day at 09:00 — so the keys
are optional overrides, never approval material. A version bump made for an
unrelated edit can therefore move a complete policy to `unapproved` only
through the non-cadence keys it newly requires.

## Sibling policy changes and sign-off

The `[inventory]` pins record which sibling-policy versions (rulebook,
protection, stress) this constitution was written against. When a sibling
changes — a protection-policy edit bumps its own version, for example — the
pins no longer match the live identities. What that mismatch means is governed
by `inventory.require_signoff` (version 4 or above, optional):

- **Off, the default.** The change is bookkeeping and the human surfaces stay
  quiet: `canary policy show` and the brief mention pins only when a live
  policy identity cannot be read (a real data gap), and the monthly pulse
  completes on its own. The typed JSON keeps the pin rows in every mode so
  tooling has the record. On a single-trader desk the sibling file edit is
  itself the human decision; nothing about it needs re-announcing.
- **On (`require_signoff = true`).** The mismatch is a governance blocker:
  the brief flags it for attention, a policy-drift nudge is raised, and the
  monthly pulse stays blocked until you review the changed sibling and update
  its `[inventory]` pin. Set this in environments where an explicit recorded
  sign-off of every sibling revision is required.

Adding or changing the key is itself a policy edit and becomes effective the
same way as any other revision (see "Making a revision effective" above).

Evaluation covers one selected account: the ladder measures that account's
current equity against its own cash-flow-adjusted peak. Paper and live state are
kept separate, and switching accounts cannot reuse another account's peak,
drawdown latch, or reconciliation clock. A policy revision still changes the
answer for everything held in the selected account at once; no position carries
the policy that was in force when it was opened.

## Bounded exceptions

```sh
canary policy override --control <key> --reason "..." --hours <n>
```

`--hours` must be positive and at most `override.max_duration_hours`; until you
declare that cap, overrides are unavailable. `--control` must name a key that
`--explain` lists. Each grant is journaled with its reason and the policy
fingerprint and expires on its own. A durable change is a version bump, not a
longer override.

Two overrides reach evaluation. One on `capital.max_unreconciled_days` can
extend that deadline, never shorten it. One on
`order_limits.max_order_floor_base` lifts the floor to
`max_order_ceiling_base`, so the order cap in force is the ceiling until it
expires; the other `[order_limits]` keys refuse an override. Others are
recorded and displayed.

A latched drawdown block is not an override case. It engages provisionally:
the broker statement covering the latch day releases it automatically when a
confirmed withdrawal explains the drop, and confirms it otherwise. How a
confirmed latch clears is yours to choose with `release` under `[drawdown]`:

- `manual` (the default): it stays on until you run
  `canary policy reset-drawdown --reason "..."`, which re-bases the adjusted
  peak and measures future drawdown from that new baseline.
- `automatic`: it also clears when fresh, verified drawdown is strictly below
  the block threshold, preserving the peak and loss history. Missing or stale
  equity, unresolved policy, and overdue reconciliation never clear it, and
  the reset command remains available when you want to re-base.

An upgrade never changes this for you: a policy without the key keeps manual
release.

`canary policy` has one MCP counterpart, the read-only plausibility check
`canary_policy_check`. Its governance verbs (`capital-event`, `override`,
`reset-drawdown`, `correct-peak`) are human-origin only, so an agent session
can read the policy result and never operate this file.

## Opening or adding stock

`canary add plan SYMBOL --currency CCY --limit PRICE --max` requests the
maximum whole-share addition for one order. Use `--quantity N` instead to check
an exact addition. Choose one explicitly. The same command opens a selected
watchlist instrument: Canary reads the actual holding, including confirmed zero.
MCP `canary_add` exposes planning only with the same explicit sizing choice.

The plan separates stock allocation room, the next-order allowance before
broker costs, and the exact checked order. It shows cash deductions, allocation
changes, risk checks and working stop coverage. Each order uses its own broker
fee and margin simulation. An exact-quantity request does not estimate a maximum
using that smaller order's fee. If a bounded Max search cannot establish the
maximum, it retains useful evidence and explains which quantity passed a check;
no order is selected from that incomplete result.

`canary add preview` prepares the exact order through the existing review path.
Submission needs separate owner confirmation. Max never creates a recurring
purchase or unattended submission. Existing stops retain their quantity; their
before/after share coverage is shown, and any change needs a separate review.

The optional `[position_add]` table belongs in this risk policy. Both
`max_stock_pct_nlv` and `max_underlying_stock_pct_nlv` are required percentages
in `(0,100]`, chosen by you. They cap all stock market value and stock in the
selected underlying respectively, including pending buys.

**Activation is a separate policy decision:** `admission_contract =
"stock-entry-v1"` makes the pass bands of Rulebook rules 1 (issuer concentration),
3 (premium budget/sell-only), 15 (net exposure), 18 (issuer loss budget) and 19
(margin headroom) mandatory for long-stock increases. This includes ordinary
stock BUYs and applies regardless of those rules' advisory display modes.
Allocation percentages alone do not activate these new admission checks. No
policy values or admission approval are installed automatically.

Purchases must fit settled cash in their own currency after fees, commitments
and currency floats. The account cash reserve is funded once across measured
cash currencies; another currency can cover the reserve, but cannot fund this
purchase. The exact broker margin simulation must retain the approved headroom
floor, including look-ahead where available. Missing evidence holds the order.

This increment supports stock additions and openings only. Unknown pending
activity, queued instructions, pending non-stock orders, or corporate debt
without combined issuer mapping can hold the plan. Bonds and option orders
will have separate sizing rules in later increments.
