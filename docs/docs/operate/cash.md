# Cash management

Updated: 2026-10-06 14:12 CEST

Canary 3.18 and earlier read the cash sweep at `[buckets.cash_sweep]` and
have no currency leveling; this page describes the versions after it.

The cash sweep and currency leveling manage cash rather than protect
positions: the sweep buys bills of the same currency with idle cash, and
leveling converts cash to repay a borrowed currency. They live in their own
`[cash]` section of the protection policy file, with their own authority.
Protection stays close-or-reduce only; a cash row's order is admitted by its
own typed bounds, described below, never as an exception to protection.

```toml
[cash]
pre_authorised = []   # ["cash_sweep"] lets the daemon place the sweep's bill orders itself

[cash.sweep]
# the cash sweep, below

[cash.leveling]
# currency leveling, below
```

`pre_authorised` in `[cash]` is empty by default. Listing `cash_sweep`, then
raising `policy_version`, lets the daemon place the sweep's bill buys and
redemptions itself after the notice and the full veto window, each held to
the sweep's order cap in force; the window is the one in `[authority]
veto_window`, shared with protection ([Pre-authorised
buckets](protection.md#pre-authorised-buckets)). Currency leveling cannot be
listed: you approve each repayment.

A file written before cash management had its own section keeps working:
Canary reads `[buckets.cash_sweep]` as `[cash.sweep]` and `cash_sweep` in
`[authority] pre_authorised` as the `[cash]` entry. At its next start the
daemon moves them, after a backup: it rewrites those headers and that one
entry, keeps every value and comment and changes no setting. The same start
writes `[cash.leveling]`, off, into a file without it and raises
`policy_version` by one; `canary policy ensure --dry-run` shows both
beforehand. A file that writes the sweep in both places is refused until one
is removed.

## Cash sweep

The cash sweep puts idle cash to work in bills of the same currency and never
converts one currency into another. Its rows buy under the `[cash]`
authority, not as protection: a `cash_sweep` buy is admitted only as the
row's own resolved bill, a vocabulary instrument in the row's own currency,
for no more face value than the free cash it was planned against, costing no
more than that free cash at the preview's limit, and within the sweep's order
cap in force. Apart from the trading-cap exemption described under
[Reserve and order sizing](#reserve-and-order-sizing), nothing else is relaxed.

It is off until you write the table. In `active` mode a row is an ordinary
proposal: `canary proposals preview` previews its bill as a limit order of
the bill's own security type (BILL for a US Treasury bill, BILL or BOND for
the others; DAY, regular hours) through every gate any proposal meets (trading freeze,
authority, the bill's session, a live two-sided quote read during the preview,
the order cap in force of `[order_limits]` unless the bill exemption applies, broker WhatIf), and it is sent only when you approve
it, or by the daemon after the full veto window when you list `cash_sweep`
under `[cash] pre_authorised`. Without that listing, your approval of each
order is the last step.

```toml
[cash.sweep]
enabled = true
mode = "active"              # shadow (default) or active
reserve_floor_base = 10000.0 # reserve kept as cash: the largest of this,
reserve_pct_nlv = 10.0       #   this percent of NLV, and planned needs
min_order_notional = 20000.0 # smallest buy, in base currency
max_order_notional = 50000.0 # largest order: the larger of this
max_order_pct_nlv = 10.0     #   and this percent of NLV
order_step_base = 1000.0     # order grid: buys round down, redemptions up
keep_cash = 5000.0           # settlement float in each currency's own unit
bills_exempt_from_trading_max_notional = true
no_buy_while_borrowed = true # no bill buys while any currency is borrowed
# tax_reviewed_at = 2026-01-01   # advisory: until written, rows say the tax treatment is not yet confirmed
```

Every sizing number is read from the file only. Canary's own values exist
solely to be written into it: the daemon writes a missing one at its next
start, and until then the sweep holds at `needs_your_number`, naming the key,
and never falls back to a compiled value.

Per currency, in that currency: **cash** is the lower of trade-date cash and
the broker's observed per-currency settled cash
(`settled_cash_source: broker`). The account-wide `SettledCash` figure does
not establish a currency's settled cash. Without a per-currency observation,
the sweep holds at `settlement_unknown`; waiting after a daemon restart does
not clear it. The order journal lacks verified settlement dates, holiday
calendars and complete account-wide fill coverage, so its estimate cannot
authorise a sweep. An optional authenticated read-only Web API connection can
provide the missing observation; see [Settlement evidence](cash-ledger.md).
**Committed** is working buy orders with a fixed finite limit plus armed, held
or sending queued buys at their finite worst price. An unknown principal or
commission bound or a nonfinite total holds the sweep. Outstanding buy records
currently lack fee envelopes, so new sweeps wait while such buys remain.
A prepared, unarmed queue entry or an
unapproved proposal never counts. A currency conversion of either side
working at the broker makes every currency's commitments unknown, so the
sweep holds until it fills or is cancelled. **Kept** is the currency's `keep_cash` and,
in the base currency, the reserve (below): the larger of the two. **Free** is
cash − committed − kept − any reserve shortfall this currency keeps for the base
currency. When free exceeds the smallest order (`min_order_notional` at the
ledger rate) the sweep buys one order, held to the order cap in force at the
ledger rate; a cap that holds the order below the smallest order holds the
currency. Exact buy previews also require principal plus the broker's maximum
same-currency commission to fit free cash. The cap is per order; no aggregate
percentage or daily budget exists. When cash less commitments falls below
kept it sells the nearest maturity (or the declared ETF) to cover the gap,
held to the cap like a buy (the next cycle sells the rest), unless a held bill
pays out before a sale today would settle; unsettled proceeds of such a sale
count toward kept so it is not sold twice. A sale restores cash, so it is
never held to, or raised to, the smallest order: it sells what the gap needs on
the bill's size grid. Otherwise nothing happens.

A currency without its own table follows Canary's default declaration: USD
`us_tbill`; EUR `de_bubill` and `fr_btf` with an `etf` fallback; GBP
`uk_tbill`; CAD `ca_tbill`; every other currency `none`, which keeps its cash
as cash. The ladder defaults to four rungs from `min_maturity_days = 28` to
`max_maturity_days = 91` (EUR 182, at most 397). `keep_cash` has no compiled
default: a currency table's own value wins, else the `[cash.sweep]` table's. `min_tranche`
is retired in favour of `min_order_notional`; an old file that still writes it
keeps reading, and the value then raises that currency's smallest buy. The
fallback ETF acts only after a completed contract search finds no bill line;
until you write its `etf_symbol` and `etf_exchange` the status names them under
`needs_your_number`, and the bills still plan.

### Reserve and order sizing

Owner decisions of 2026-10-05 18:35 CEST. All figures are in the account's
base currency at the ledger rate.

- **Reserve kept as cash** is the largest of `reserve_floor_base`,
  `reserve_pct_nlv` percent of net liquidation value, and the cash planned
  exercises or withdrawals need. Canary records no planned draw today: an
  approved exercise goes to the broker at once, and a withdrawal is recorded
  only after it happens. That term is therefore 0, and the status says so in
  `planned_needs_reason`. Working and armed buys are not added, because each
  currency's cash already has them deducted as committed.
- **Where it is held.** The reserve is held in the base currency first: the
  base currency keeps the larger of its `keep_cash` and the reserve. The part
  base cash cannot hold (`reserve_shortfall_base`) is kept in the other
  currencies before they invest, the largest free cash first. If base cash is
  unknown, the other currencies do not invest. A shortfall never makes another
  currency sell; the base currency redeems its own bills to restore the
  reserve. Every other currency keeps its own `keep_cash`.
- **Order bounds.** A buy is at least `min_order_notional` and at most the
  larger of `max_order_notional` and `max_order_pct_nlv` percent of NLV. A
  sale is never held to the minimum.
- **Order grid.** Every order is sized on a grid of `order_step_base`
  (1,000 in base currency, converted at the ledger rate): a buy rounds its
  amount down to a whole step, a redemption rounds its target up. The reserve
  and the order cap follow net liquidation value, so without the grid the
  quantity would change on every refresh and an approval would go stale; with
  it the quantity changes only when free cash crosses a step, about a 10,000
  move in NLV at 10% (owner decision 2026-10-06 08:22 CEST).
- **Fail closed.** When a percentage is above 0 and net liquidation value
  cannot be read, every currency holds with nothing bought or sold, and no row
  carries a trading-cap exemption.
- **Trading-cap exemption.** With `bills_exempt_from_trading_max_notional =
  true`, a sweep bill order may pass the order cap in force (`[order_limits]`)
  up to the sweep's order cap in force, never beyond it. The order must be a BILL or BOND
  buy that opens or increases, or a sale that reduces or closes, of a vocabulary
  bill in the order's own currency, with no conversion. Stocks, ETFs, the
  sweep's fallback ETF, conversions and anything over the sweep's cap keep the
  order cap in force. The row's order terms carry the limit as
  `trading_cap_exempt_up_to_base`, and both preview and submit check it.
  Without the key, the exemption is off.

- **No buys while borrowed.** Owner decision of 2026-10-05 21:24 CEST. With
  `no_buy_while_borrowed = true`, the sweep buys no bill in any currency while
  any currency's cash is negative by more than 1 unit of that currency. A
  negative balance is a margin loan, and its interest usually costs more than
  a bill earns. Cash here is the same figure as each currency's **cash**: the
  lower of trade-date and settled cash. Every invest row stays listed,
  blocked by `currency_borrowed`, for example "USD is borrowed: −20,000 USD;
  bill buys wait until it is repaid". Repay the debit by converting another
  currency or depositing; the sweep never converts, and
  [currency leveling](#currency-leveling), when on, proposes the conversion
  for a debit beyond its band. Redemptions are not held:
  selling bills to cover cash is still allowed. A listed currency whose cash
  cannot be read means the sweep cannot prove that nothing is borrowed, so
  buys hold with `borrowing_unknown` and the status names the currency. With
  `false`, buys go ahead and `canary policy check` warns while a currency is
  borrowed. The status's `borrowing` block carries `state` (`clear`,
  `borrowed` or `unknown`), `no_buy_while_borrowed`, `holds_buys`,
  `tolerance_units`, `borrowed` (`currency`, `cash`, `borrowed`,
  `borrowed_base`), `unknown` (`currency`, `reason`), `message` and
  `action`; a held currency carries the blocker in `blockers`.

Worked check, with illustrative figures. NLV 200,000 EUR, EUR cash 60,000, USD cash 6,000, `keep_cash`
5,000: the reserve is 20,000 EUR (10% of NLV), held in EUR, and EUR invests
one order of 40,000 (60,000 − 20,000, under the 50,000 cap). USD free cash
is 1,000 USD, below 20,000 EUR, so USD stays cash. With USD cash at −20,000
instead, the EUR order is listed but held by `currency_borrowed`. At NLV
1,200,000 with 1,000,000 cash: reserve 120,000, and orders up to 120,000 each.

The status's `sizing` block and every row's `cash_sweep.sizing` carry the
figures, with stable field names: `base_currency`, `net_liquidation_base`,
`reserve_base`, `reserve_bound` (`reserve_floor_base`, `reserve_pct_nlv` or
`planned_needs`), `reserve_floor_base`, `reserve_pct_nlv`,
`reserve_pct_nlv_base`, `planned_needs_base`, `planned_needs_known`,
`planned_needs_reason`, `reserve_shortfall_base`, `min_order_base`,
`max_order_base`, `max_order_bound` (`max_order_notional` or
`max_order_pct_nlv`), `max_order_notional_base`, `max_order_pct_nlv`,
`order_step_base` and `trading_max_notional_exempt`. Each currency's status and row carry
`reserve_held`, the part of the reserve kept in that currency in its own unit.
`max_order_notional_base` on the status and the row is the cap in force. A row
detail says it in words, for example "kept as cash: 20000 EUR (10% of NLV
200000 EUR), held in EUR; orders from 20000 EUR to 50000 EUR
(max_order_notional)".

At its start the daemon adds each missing sizing key to an existing
`[cash.sweep]` section: it backs the file up, writes only the missing keys at
the values above, keeps every value you wrote and raises `policy_version` by
one; run `canary policy ensure --dry-run` to list them beforehand. A file
without the table is left alone, and the sweep stays off.

```toml
[cash.sweep.currency.EUR]
instruments = ["de_bubill", "fr_btf"]
fallback = "etf"
etf_symbol = "AAA"       # your fallback ETF
etf_exchange = "IBIS"
keep_cash = 8000
```

A key you leave out of a currency table takes that currency's default. An
instrument outside the vocabulary, a bill of another currency, or any
conversion fails validation.

Which bill a buy names: for USD, the outstanding bill from TreasuryDirect's
public list (read once a day and kept in daemon state) that matures nearest
the rung's target inside `min_maturity_days`–`max_maturity_days`; for EUR,
GBP and CAD, the nearest of the bills you list by ISIN:

```toml
[cash.sweep.currency.EUR]
isins = ["DE000BU0ZZ19", "FR0128ZZZZ13"]   # your bills; each of a declared instrument
```

Canary names a bill only after the broker resolves it to one line that
carries its size rules and minimum tick, and quotes it. IBKR lists US
Treasury bills as security type BILL, so Canary asks for a US bill as BILL
and for a German, French, UK or Canadian bill as BILL first and BOND second
(an assumption checked per instrument after install); the row records which
type resolved. A currency with no
list to choose from reads `universe_unavailable` (TreasuryDirect unreachable
for two days, or no `isins` written); one whose candidates do not resolve,
carry no size rules or no price reads `instrument_unresolved` with the
evidence per candidate. `canary market --symbol <ISIN|CUSIP> --type BILL`
(or `--type BOND`, which for a bill's identifier asks BILL as well and says
so) runs the same resolution and quote as a read-only check, naming every
attempt with IBKR's code and text when none finds a line, and `canary positions`
lists held bills and bonds in their own section with class, maturity and
currency.

An order counts the bill's own unit: Canary assumes a US Treasury bill is
bought in bonds of 1,000 USD face (`face_1000`) and a German, French, UK or
Canadian bill in single units of face (`face_1`), priced per 100 of face.
The buy is the planned cash at the higher of par and the quoted price, in
whole units rounded down to the bill's minimum size and size step, so neither
its face value nor its cost passes the free cash; a tranche too small for the
bill's minimum holds the currency and says so. A redemption rounds its sale up
to the held bill's size step, or down when up would pass the position or
the order cap. The row's `cash_sweep`
block carries `quantity_unit`, `face_value`, `estimated_cost` and the
`session` its order fills in: the bill's liquid hours from its contract
details, else assumed weekday hours (US bills 08:00–17:00 New York, Bubills
and BTFs 09:00–17:30 Frankfurt and Paris, UK bills 08:00–16:30 London,
Canadian bills 08:00–17:00 Toronto; holidays are then not modelled, and the
live-quote requirement refuses instead). The units, the price convention and
the hours are assumptions the post-install proof checks. The preview prices a
patient limit on the bill's minimum tick (a buy at the mid rounded down, never
below the bid), and Canary refuses to build any bond order off the bill's size
or price grid.

A row carries a blocker only when its order cannot be priced or sized:
`fresh_bill_quote_required` (the bill's quote, or a held bill's mark, is not
live), `bill_contract_rules_unavailable` or `below_minimum_increment` (a
redemption the held bill's size grid cannot fit), and `bill_unit_mismatch`:
a buy's preview divides the broker's WhatIf initial-margin change (the figure
IBKR returns for a bond) by the order's value at the assumed unit, and when
the ratio falls outside 0.005 to 1.2 (a bill margined at one percent reads
about 0.01, a cash account about 1.0, a unit 1,000 times off near 10 or near
0.00001), the preview is refused and that bill instrument's buys stay
blocked, for every submit, until a preview checks clean. A stale quote's
readiness is `quote_unusable`. Outside the bill's session the row's readiness
is `market_closed` with the session's next open, and the preview refuses with
`market_closed` before any quote. An
order already working for the same bill and side, or sent and not yet
acknowledged, holds a new preview until it fills or is cancelled; working
bond buys count as committed cash, and a bill sold inside the settlement
window counts toward `keep_cash` until it settles.

The snapshot's `cash_sweep` status lists every currency the account ledger
reports, with its figures and a state:

| State | Meaning |
|---|---|
| `invest` / `redeem` | the band asks for a buy or a sale; the row follows |
| `hold` | inside the band, or the reason says why no order follows |
| `no_instrument` | the currency declares `none` |
| `cash_unavailable` | no current ledger cash for the currency (never read as zero) |
| `settlement_unknown` | the broker supplied no per-currency settled cash, or working/armed queued buy commitments have no fixed finite bound; a journal estimate does not clear this state |
| `equivalents_unclassified` | a bond or bill holding whose contract details cannot be read, or a declared-ETF holding |
| `needs_your_number` | a sizing number (`max_order_notional`, `max_order_pct_nlv`, `min_order_notional`, `reserve_floor_base`, `reserve_pct_nlv`, `order_step_base`, or the currency's `keep_cash`), `no_buy_while_borrowed`, or the symbol of an ETF-only declaration, is not written; the reason names the key |
| `universe_unavailable` | no list of bills to choose from (see above) |
| `instrument_unresolved` | no candidate bill was confirmed by contract details and a quote; `evidence` says why |

`canary proposals list` shows the sweep under its own *Cash sweep* heading,
and `--details` adds one band line per currency; JSON carries a `cash_sweep`
block on each row, and `counts.cash_sweep` and `counts.cash_sweep_shadow`
count the rows.
An invest row's key names its bill, so a preview or submit buys the bill you
saw, never another one a later cycle names. Every row carries
`never_skip_veto`. `mode = "active"` makes the rows ordinary proposals under
every gate, freeze included; shadow rows carry `shadow_mode` and preview and
submit refuse them. Without `tax_reviewed_at` each
row carries the line "tax treatment not yet confirmed" and the status
`tax_reviewed: false`; it blocks nothing. Each currency's status carries
`cash_like`, cash plus cash equivalents, when both are known. While the sweep
is enabled, `canary brief` adds a `cash` row (cash, cash equivalents and
their sum per currency) and rule 14's
evidence gains the same figures; the rule's own figure is unchanged, because
the sweep never converts. Set `enabled = false`, or remove the table, and
raise `policy_version` to stop it: rows leave on the next refresh and held
bills mature to cash.

## Currency leveling

A currency whose cash is negative is a margin loan: IBKR charges debit
interest on it and never converts on its own. Currency leveling repays such
a loan back to between zero and a small cushion, never further, from the
currencies whose cash earns least, and only from a currency whose cash earns
less than the loan costs. The cash sweep still never converts; leveling
converts, under `[cash.leveling]`.

A conversion is admitted under the `[cash]` authority only as a
`currency_leveling` row's own pair, contract and side, on IDEALPRO, IBKR's
currency market, that reduces a negative currency balance and, at the live
quote's far side, brings in no more than its share of the target, zero to
the cushion, and spends no more than its allotment of the paying currency.

It is off until you switch it on. The daemon writes the table, off, at its
next start; once you set `enabled = true` and raise `policy_version`, each
repayment is an ordinary proposal:
its conversions preview as CASH limit DAY orders on IDEALPRO through every
gate any proposal meets, and they are sent only when you approve the
repayment. Leveling is never pre-authorised.

```toml
[cash.leveling]
enabled = false
trigger_base = 10000.0  # the band: repay only below minus this, in base currency
cushion_base = 250.0    # land at most this far above zero; a paying currency keeps this much
max_slippage_bp = 2.0   # limit at most this far from the live mid
payback_days = 30       # a conversion must earn back its worst-case cost within this many days

[cash.leveling.currency.USD]
deliberate_carry = true  # keep USD negative on purpose; never repaid, never pays
```

Every number is read from the file only: a missing one holds leveling; the
status's `needs_your_number` names it.

**Which cash.** Leveling reads each currency's trade-date cash from the
broker's ledger, not the sweep's lower of trade-date and settled cash. A
conversion changes trade-date cash at once, while settled cash follows two
days later, so planning on settled cash would propose the same conversion
again until it settles.

**Interest rates.** What each currency costs and earns comes from the
broker's own daily statements, which Canary already keeps for currency
reporting; there is no rate setting. A loan rate is the interest charged over
the currency's last ten statement days with a negative settled balance, a
cash rate the interest paid over its last ten days with a positive one, each
divided by balance times calendar days and annualised over 365 days; only
the last 180 days count, and each rate shows the last day it read. A
currency not borrowed lately takes its cash rate as the floor of what its
loan costs; one that held no cash lately takes its loan rate as the ceiling
of what its cash earns. A cash rate is a blend: IBKR pays nothing on the
first slice of a balance. Without the statements, leveling holds and says so.

**Which loan, and who pays.** Loans are repaid in order of their rate,
dearest first: when cash runs short, each unit of it saves most where the
loan costs most. A currency may pay a loan only when its cash earns less
than the loan costs, and only down to its cushion after working and armed
buys; the sweep's reserve and `keep_cash` can pay too. Every conversion must earn back its worst-case cost, IBKR's
commission (at least USD 2) plus the slippage bound, in interest saved within
`payback_days`. Of the ways to repay a loan from the currencies allowed to
pay, leveling takes the one that repays it in full and saves the most within
that window: the cheapest currency that covers the loan alone, or several,
cheapest first, when a cheaper one cannot cover it alone and the split saves
more than its extra commission. What one loan takes is set aside before the
next is planned, so two repayments never spend the same cash.

**One approval per loan.** A loan's conversions form one repayment, listed
together and approved as a whole. The daemon's bundle calls
(`trade.proposals.prepare_bundle`, `trade.proposals.submit_bundle`) preview
every conversion, then check every one again before sending any, send them
cheapest currency first and stop at the first refusal. A conversion that is
one of several refuses a prepare or submit of its own
(`conversion_bundle_needs_one_approval`). In the trading build you approve a
repayment of a single conversion like any proposal, with `canary proposals
submit KEY REVISION`; a repayment of several conversions only through the
daemon's bundle calls.

**How it sizes.** Each conversion is sized inside its share of that target at
both edges of the price bound and takes the middle, keeping the previous
size while it stays inside, so an ordinary rate move does not change the
order. A repayment's conversions together are held to the order cap in force
of `[order_limits]`, and the next cycle repays the rest. Each row names the
pair's exact contract, and its key binds it.

**When it holds.** A currency holds, with the reason on its line, while its
cash is unavailable; while the ledger is older than a fill Canary saw; while
the broker's open-order list cannot be read; while a conversion in either
currency works at the broker or waits for its acknowledgement; while the
order cap in force or the interest rates cannot be read; while no currency
may pay (each earns at least what the loan costs, holds no more than its
cushion, or has a buy whose cost has no bound); while no conversion pays
back within the window; while the pair's contract cannot be resolved; and
for a currency you mark `deliberate_carry`. A currency with no pair Canary
trades (outside EUR, GBP, AUD, NZD, USD, CAD, CHF and JPY) holds too.

**How the preview prices it.** IDEALPRO trades from Sunday 17:15 to Friday
17:00 New York time with a break from 17:00 to 17:15; outside those hours
the preview refuses with `market_closed` before reading a quote, and the
row's readiness says when it opens. The preview reads a live bid and ask on
its own broker session and sends a limit `max_slippage_bp` from the mid,
toward the mid on the pair's tick. A delayed, stale, one-sided or crossed
quote refuses; a market wider than the bound refuses with
`fx_spread_beyond_bound`, so no fill is worse than the bound. A preview,
prepared submit or submit refuses with `conversion_beyond_target`,
`conversion_funding_short`, `conversion_terms_drift` or
`conversion_already_working` when the conversion has left its bounds since
the row was built.

**Beside the cash sweep.** A conversion of either side working at the broker
holds the sweep, because it moves cash between currencies the sweep counts.
A debit inside the band stays, and with `no_buy_while_borrowed` the sweep's
bill buys wait until it is repaid; `canary policy check` names such a debit
(`leveling_debit_inside_band`).

**Tax.** Each conversion is a foreign-exchange transaction in a base-currency
account. Repaying a borrowed currency from the base currency buys that
currency and spends it at once; paying from a foreign currency you hold, or
selling one to repay a borrowed base currency, can realise a gain or loss.
Each repayment names the currency that pays. Canary keeps no tax setting for
it; ask your adviser how your conversions are taxed.

`canary proposals list` shows leveling under its own *Currency leveling*
heading with one line per currency and one per repayment, with what it saves
and can cost within the window; JSON carries a `currency_leveling` status on
the snapshot, with its `bundles`, a `currency_leveling` block on each row,
and `counts.currency_leveling` counts the rows. Set `enabled = false`, or
remove the table, and raise `policy_version` to stop it.
