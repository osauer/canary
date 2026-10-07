# Currency leveling (repay a borrowed currency by converting)

Updated: 2026-10-06 18:40 CEST
Status: built on branch `currency-auto-level`, including the plan for
several currencies, not merged, not installed. Nothing has been previewed or
sent at the broker. The commands Desk's one-approval repayment card calls
(`proposals prepare-bundle`, `submit-bundle`, `bundle-status`) are built on
the `cash-settings` branch with the card, not merged; proof pending: the
first real repayment.

This record follows `.agents/docs/risk-policy-contract.md`. It records the
owner's decisions and creates none.

## Owner decisions

- 2026-10-05 21:28 CEST: build currency auto-level off by default, in a
  separate worktree and branch; do not merge or push it that day; commit on
  the branch only. Never install, restart the daemon, write
  `~/.config/ibkr` or place orders.
- 2026-10-05 22:02 CEST, verbatim: "with live I meant: no shadow. Do the
  real thing. But stay in the worktree." The whole order path is built;
  there is no shadow mode.
- 2026-10-06, settings walk-through (05:44–06:15 CEST):
  - five settings: `enabled`, `trigger_base`, `cushion_base`,
    `max_slippage_bp`, `deliberate_carry`. Removed: `mode` (no shadow; every
    conversion waits for approval anyway), `min_conversion_base` (the band is
    the floor), `max_order_base` (the order cap in force of
    `[order_limits]` already limits every order), `tax_reviewed_at` (no tax
    setting; the note below stays);
  - band 10,000 EUR; cushion 250 EUR; limit within 2 bp of the mid;
  - no deliberate-carry currency;
  - off by default for every install; the owner switches their own file on;
  - not pre-authorisable;
  - `no_buy_while_borrowed` lands first (it is on `origin/main` since
    2026-10-06 06:05 CEST); this branch is rebased onto it;
  - Desk approves a row only when it names the exact contract id, so rows
    carry the pair's resolved contract id;
  - cash management (the sweep and leveling) moves out of the protection
    policy into its own section, on this branch, in a separate commit.
- 2026-10-06 06:38 CEST, answers on three or more currencies (quoted under
  "Several currencies"), and 07:11 CEST, verbatim: "all six confirmed, build
  it". Confirmed: rates from the broker's statements with no rate settings; a
  sixth setting `payback_days`, 30; the band no longer the smallest
  conversion; the order cap holds a loan's conversions together; a paying
  currency keeps the cushion; a repayment of several conversions waits for
  Desk's one-approval card while single conversions work as before.

## Why

A negative currency balance is a margin loan; IBKR never converts on its
own. When a stock is bought in a currency the account does not hold, the
account borrows that currency and pays debit interest until the owner
converts. The cash sweep deliberately never converts (its protected goal is
currency neutrality, `cash-sweep.md`), so the loan stays until converted by
hand.

## Meaning

- Base and unit: every policy number is in base currency at the ledger rate
  (`ExchangeRate`, base per unit). Amounts on a row are in their own
  currency.
- **Balance read: trade-date cash** (`$LEDGER` CashBalance per currency,
  through `cashSweepLedgerAt`). Not the sweep's lower of trade-date and
  settled cash: a conversion moves trade-date cash the moment it fills,
  while settled cash follows two days later (FX spot settles T+2). Planning
  on settled cash would propose the same conversion again until it settles
  and convert beyond zero. Trade-date cash is also where a stock buy's debit
  is heading.
- Band (`trigger_base`): a currency is levelled only when its trade-date
  cash is below minus the band. A debit inside the band is left alone; the
  plausibility rule `leveling_debit_inside_band` names it.
- Target: back between zero and `cushion_base`, never more. Each
  conversion of a repayment gets its share of that band: its planned value
  plus a share of half the cushion as its ceiling, minus the same as its
  floor, so the repayment lands between zero and the cushion whatever order
  its conversions fill in. A conversion's quantity is chosen inside its
  share at both edges of the price bound, at the middle, and the previous
  generation's quantity is kept while it stays inside, so a ledger-rate
  update does not change the row, its revision or a review in progress.
- Paying ("Several currencies" below): the currencies whose cash earns less
  than the loan costs, cheapest first, chosen by what they save within
  `payback_days` after costs. A paying currency spends at most its trade-date
  cash less what working buy orders and armed queued buys already hold of it
  (principal at their fixed limit; a buy without a price bound holds) less
  the cushion it keeps. Each conversion carries its allotment of that cash;
  what one loan takes is set aside before the next. Leveling never borrows
  one currency to repay another.
- Size: a repayment's conversions together are held to the order cap in
  force of `[order_limits]`; the next cycle repays the rest. The band says
  only when a loan is worth repaying; whether a conversion is worth its cost
  is the payback test.
- `deliberate_carry = true` in `[cash.leveling.currency.<CCY>]`
  leaves that currency negative on purpose; it is never repaid and never
  pays.
- Pair: IDEALPRO's conventional major pair (`canonicalOrderFXContract`: EUR,
  GBP, AUD, NZD, USD, CAD, CHF, JPY); quantity counts the pair's first
  currency. Repaying USD from EUR is `SELL EUR.USD`; repaying EUR from USD is
  `BUY EUR.USD`. Any other currency holds with "no pair Canary can trade".
  After planning, the engine resolves the pair's contract id and minimum
  tick once per daemon (contract details) and binds the id to the row and
  its key; a pair that does not resolve holds.
- Unknown posture, per currency, generating nothing: `cash_unavailable`; a
  ledger older than a confirmed fill; the open-order list unreadable; a
  conversion already working in either currency; the order cap in force
  unreadable; the interest rates unreadable or the loan's unmeasured; the
  ledger without a USD rate (the commission minimum is in USD); no currency
  allowed to pay; nothing that pays back; a number not written
  (`needs your number`).

## Several currencies (built 2026-10-06, confirmed 07:11 CEST)

The first build paid every loan from the base currency, and a borrowed base
currency from the foreign currency with the most cash. It also had a defect:
two loans were each sized against the whole base cash, so approving both
rows could overdraw it. The owner's answers of 2026-10-06 06:38 CEST set the
rule for three or more currencies:

- pay from "the currency with the lowest interest rate compared to loan
  currency, if it has sufficient cash", and split when no single currency
  has enough but together they do;
- loans in order of "the highest net interest cost. We must not sell a
  high-interest currency to fund a low or zero interest loan. The net effect
  is important for decision.";
- several paying currencies for one loan, "but one approval";
- the sweep's kept cash may be spent, "with interest calculation".

### Rates come from the broker's statements

No rate settings. Canary already keeps IBKR's daily statements for currency
reporting (`fxStatements`). Each carries, per currency, the interest
accrued that day (`InterestAccruals`) and the settled cash it accrued on
(`CashReport`, `endingSettledCash`).

- Loan rate: interest charged over the latest 10 statement days on which the
  currency's settled cash was negative, divided by balance times days,
  annualised on 365 days. Cash rate: the same over days with positive settled
  cash. Only days within the last 180 count; each rate shows the date of its
  latest day. Counting calendar days lets currencies with 360- and 365-day
  conventions compare directly.
- One side unobserved: in one currency, a loan costs at least what cash
  earns. An unobserved loan rate is taken as that currency's cash rate (the
  loan ranks lower and fewer conversions qualify); an unobserved cash rate
  as its loan rate (the currency looks dearer to spend). Neither observed:
  the loan holds ("no interest for USD in the statements yet") and the
  currency does not pay.
- Statements unavailable: the whole bucket holds.
- Checked 2026-10-06 against the retained daily statements: in each
  currency the loan rate comes out about 2 points above the cash rate,
  consistent with IBKR's tiered schedule. Cash rates are blends: IBKR pays
  nothing on a first slice of a balance, so the rate on the last unit spent
  is higher than the blend (A6).

### The plan

Each generation, after the holds that already apply (a ledger older than a
fill, a conversion working, the order cap unreadable):

1. Loans: currencies whose trade-date cash is below minus the band, except
   deliberate carry. Ranked by loan rate, highest first; at equal rates the
   larger loan first.
2. Payers: currencies with trade-date cash above the cushion after working
   and armed buys; the sweep's kept cash counts. A payer keeps the cushion.
   A loan or a deliberate-carry currency never pays.
3. For each loan in rank order, the payers allowed to repay it: cash rate
   below the loan rate, and an IDEALPRO pair for the two currencies. A
   currency that earns more than the loan costs never repays it.
4. Payback: a conversion must earn back its worst-case cost within
   `payback_days`. Saving = amount × (loan rate − cash rate) × days / 365;
   cost = the commission bound (A1) plus the slippage bound
   (`max_slippage_bp`). With 30 days and 2 bp, a spread under about 0.25
   points never pays back; a 3.7-point spread pays back from about 600 EUR.
5. Canary compares every combination of the allowed payers (the five
   cheapest at most), spending the cheapest first: each gives all it can and
   the last gives the rest. The last conversion is never smaller than what
   pays back; the one before it gives up the difference. A combination with
   any conversion that does not pay back is dropped. Canary takes the one
   that repays the loan in full and saves the most over the window after
   costs; fewer conversions on a tie. If none repays in full, the best
   partial one; if none pays back, the loan holds and the status says why.
6. The order cap in force holds a loan's conversions together: one approval
   moves at most one order cap. The cheapest payers go first; the rest waits
   for the next cycle.
7. What a loan takes is deducted from its payers before the next loan. Each
   row carries its allotment, and preview and submit check the conversion
   against it (what it brings in within the loan's share of the target, what
   it spends within the payer's allotment). Rows approved in any order
   cannot overdraw a payer; this removes the defect.
8. Each conversion is sized as today, inside its share of the safe band at
   the far edge of the price bound, keeping the previous quantity while it
   stays inside.

### Why this order

- Rate, not size times rate. When cheap cash is short, each euro saves most
  where the loan rate is highest. With 20,000 EUR of 0% cash, a 100,000 EUR
  loan at 5.1% and a 20,000 EUR loan at 5.6%: repaying the smaller, dearer
  loan saves 1,120 EUR a year, the same cash on the larger one 1,020.
- Cheapest payer first. In total the account saves the rate of every loan
  repaid less the rate of every currency spent. Spending the lowest-earning
  cash first gives the most, and with loans in rate order every pairing it
  makes has a positive spread.
- One payer or several. A split costs one more commission minimum (about
  USD 2; the slippage bound is the same either way). It is chosen when the
  cheaper currency saves more than that within the window: 5,000 EUR of 0%
  cash instead of 1.4% cash saves about 5.75 EUR in 30 days, so Canary
  splits; for 1,000 EUR it saves about 1.15 EUR, so one payer covers the
  loan. This follows "the net effect is important" over the literal "if it
  has sufficient cash".
- The band says when a loan is worth acting on; payback says whether each
  conversion is worth its cost. The band is no longer the smallest
  conversion.

### One approval per loan

- A loan's conversions form a bundle: one row per conversion, each one
  order under every existing gate, sharing a bundle id (the loan currency
  and the payers' pairs with their contract ids), the leg number and count,
  the rates with their dates, and the bundle's saving and worst-case cost.
- A one-conversion bundle is an ordinary row; Desk approves it as before.
- A bundle of several needs one approval for all of them
  (`proposal_currency_leveling_bundle.go`). `trade.proposals.prepare_bundle`
  prepares every conversion through the ordinary prepare path and retains
  their preparations behind one private reference (`canarypb1.…`, like a
  prepared proposal's, never logged or sent to a browser).
  `trade.proposals.submit_bundle` records the submission, then, under the
  broker write lock, runs every
  conversion's full prepared-submit check first with nothing sent, then
  sends them through their original preview tokens, cheapest payer first,
  and stops at the first refusal. Because every check runs before any send,
  no conversion sees a sibling as already working. After sending, the bucket
  holds as before until the conversions finish and the ledger is newer than
  the fills; the next cycle plans what remains. A partly sent bundle is safe:
  each conversion stays within its own allotment and share of the target.
- A conversion that is one of several refuses a prepare, prepared submit or
  fast-path submit of its own (`conversion_bundle_needs_one_approval`); a
  plain preview stays allowed, to inspect it.
- The prepared bundle carries its exact terms (`canary.leveling_bundle`,
  version 1): the broker scope, the loan, every conversion's identity (key,
  revision, preparation, draft fingerprint, preview token id, order
  reference, contract, side, quantity, limit and the quote it was bounded
  from) and the figures at each limit, rounded against the owner: what it
  pays (at most, for a buy) and receives (at least, for a sell), what its
  payer keeps at least, where the loan lands at least, and the cushion it
  never ends above (rounded up). Their digest is of
  the exact bytes; `submit_bundle` refuses another (`prepared_terms_mismatch`)
  before any check.
- A reference is used once. `submit_bundle` records its submission, with the
  owner's confirmation for audit only, before it checks anything again and
  before it waits for the broker write lock, so a second call is refused as
  `prepared_reference_consumed`, with no outcome, whatever the first did and
  whatever else the second names. A reference Canary cannot read answers
  `prepared_reference_unavailable`, also with no outcome: the status read
  says what the bundle did. Before review B1 and M1 (fixed 2026-10-06 21:08
  CEST) other terms after a sent bundle, or an unreadable reference, answered
  `not_sent`, and a submission waiting for the lock was not yet recorded.
- Every conversion is reported in send order with its outcome, which the
  order journal proves: `sent`; `refused` when nothing reached the broker
  (the place was refused before its attempt was staged, or failed with
  nothing written); `not_sent` when it was never tried; `unknown` when the
  attempt may have reached the broker. The bundle reads `sent`,
  `partly_sent`, `not_sent` or `unknown`, with the count sent, and stops after
  a conversion that is not sent. Until 2026-10-06 18:40 CEST a transport
  failure read as refused, a first refusal read as partly sent, and a second
  submission read "nothing was sent".
- `trade.proposals.prepared_bundle_status` reads the same outcomes back from
  the bundle's record, each conversion's preparation and the order journal,
  and sends nothing; a bundle being sent, or waiting for the broker write
  lock, reads `unknown` with why, never `prepared`.
- Desk reaches the broker only through the CLI: `canary proposals
  prepare-bundle`, `submit-bundle --stdin` and `bundle-status
  --bundle-ref-stdin`. None is an MCP tool or in an agent grant; the broker
  hook treats `submit-bundle` as a write. The CLI waits for them longer than
  the daemon's 150 s, so its caller never reads a send still running as
  finished.
- Desk shows every leveling repayment, one conversion or several, only as
  its repayment card with one approval (owner answer 2026-10-06 18:00 CEST);
  Canary keeps its single path for a one-conversion repayment.

### Settings after the change

| Key | Written | Meaning |
|---|---|---|
| `enabled` | `false` | unchanged |
| `trigger_base` | 10000.0 | the band: a smaller loan is left alone; no longer the smallest conversion |
| `cushion_base` | 250.0 | a repaid loan lands between zero and this; a payer keeps this much |
| `max_slippage_bp` | 2.0 | unchanged; also the cost bound in the payback test |
| `payback_days` | 30 | new: a conversion must earn back its worst-case cost within this many days (1 to 365) |
| `currency.<CCY>.deliberate_carry` | not written (false) | unchanged: never repaid, never pays |

### Worked examples

Synthetic books, EUR base, illustrative rates (loan / cash): EUR 3.4 / 1.4,
USD 5.1 / 3.3, CHF 1.5 / 0.0, AUD 5.6 / 3.6, GBP 5.5 / 3.5, JPY 1.6 / 0.0;
EUR.USD 1.17. The planner's own results; the tests check the same books
(`TestCurrencyLevelingTwoLoansFourCurrencies`, `TestCurrencyLevelingBorrowedBase`,
`TestCurrencyLevelingSplitsAcrossThree`).

1. One loan. EUR +60,000, USD −20,000 (−17,094 EUR). EUR pays 17,219 EUR
   in one conversion, as before the several-currency plan; saves about 52 EUR in 30 days
   against at most about 5 EUR.
2. Two loans, four other currencies. EUR +30,000, CHF +8,000, AUD +15,000,
   USD −30,000, JPY −2,000,000. USD (5.1%) goes first: CHF pays 8,310 EUR
   (all but the cushion) and EUR the remaining 17,456; two conversions, one
   approval; saves about 88 EUR in 30 days against at most 9. JPY (1.6%)
   holds: EUR would save 0.2 points, about 2 EUR in 30 days against at most
   4, and AUD earns 3.6%, more than the loan costs.
3. Borrowed base. EUR −15,000, USD +40,000, CHF +12,000. CHF pays
   12,590 EUR. USD earns 3.3% against the 3.4% loan, too thin to pay back,
   so about 2,400 EUR stays borrowed inside the band, and
   `leveling_debit_inside_band` names it.
4. No single payer covers. EUR +9,000, CHF +9,000, GBP +9,000, USD −25,000.
   CHF pays 9,380 EUR, EUR 8,750, GBP 3,363; three conversions, one
   approval.

### Confirmed against the earlier settings (2026-10-06 07:11 CEST)

1. Rates from the statements, no rate settings; leveling then needs Flex
   reporting on.
2. A sixth setting, `payback_days`, 30.
3. The band stops being the smallest conversion; payback takes that role.
4. The order cap holds a loan's conversions together, not each one.
5. A paying currency keeps the cushion.
6. A bundle of several conversions waits for Desk's one-approval card;
   single conversions work as before.

Paying from a foreign currency the account holds (CHF or USD above)
disposes of that currency, which the tax note covers; paying from the base
currency does not. The status shows which currency pays.

## Order path

- The proposal engine attaches the row's terms as
  `rpc.OrderPreviewParams.FX` (`json:"-"`): no RPC caller can preview a
  conversion. `normalizePreviewContract` admits CASH only as a conventional
  pair on IDEALPRO; `validatePreviewFXParams` admits only a new LMT DAY order
  without a limit, trail, bound or outside-hours flag.
- Session: IDEALPRO is open from Sunday 17:15 to Friday 17:00 New York
  time, closed daily 17:00–17:15 (`idealproSessionAt`, the hours the
  regime's USD.JPY read uses). A closed session refuses before any quote and
  drives the row's readiness. Holidays are not modelled; the live-quote
  requirement refuses instead.
- Quote and limit: a live bid and ask both received during the preview, on
  the preview's own broker session. The limit is the mid moved
  `max_slippage_bp` against the order (a sell below the mid, a buy above),
  snapped to the pair's tick toward the mid. The order is marketable while
  the touch lies inside the bound; a wider market refuses
  (`fx_spread_beyond_bound`), so no fill is worse than the bound. A delayed,
  stale, one-sided or crossed quote refuses.
- The second typed exception to `authority.close_reduce_only`
  (`currencyLevelingReduceException`, `currency_leveling_orders.go`): a
  currency_leveling row on the pair's exact contract, its own side,
  `reduce`, a negative borrowed balance, a target above that balance and no
  larger than the policy cushion, an allotment no larger than the funding
  currency's free cash. At preview, prepare and submit it checks the draft
  against the row's terms, at the live quote's far side: the conversion can
  bring in no more than its share of the target (`conversion_beyond_target`),
  spends no more than its allotment (`conversion_funding_short`), and is the
  row's order on the row's contract within the slippage bound
  (`conversion_terms_drift`). The pair's own position effect is ignored: it
  says nothing about a currency balance. A CASH order outside the exception
  is refused (`unsupported_security_type`). The order cap in force is the
  preview's own gate.
- Netting at preview and submit: a working CASH order in either currency
  from any client, or a Canary CASH send the broker has not acknowledged,
  holds the row (`conversion_already_working`).
- Encoder: the protobuf `placeOrder` admits CASH only as LMT DAY on IDEALPRO
  without outside-hours (`pkg/ibkr/place_order_proto.go`), so WhatIf and the
  real place see the same shape.
- Desk: its proposal matcher (`execution_proposals.go`) requires the row's
  contract id to equal the preview's; rows carry it, so Desk can prepare and
  confirm a conversion on the companion. Verified by reading Desk's
  matcher, not by running Desk.

## Risks of going live without an observation phase, and how they are handled

| Risk | Handling |
|---|---|
| A conversion that filled is not yet in the ledger, so a second one is proposed | Rows hold while a confirmed fill is newer than the ledger, while any CASH order works at the broker, and while the open-order list is unreadable; preview and submit hold beside a working or unacknowledged conversion; the exception re-checks against the current ledger at every step |
| Overshoot past zero: a conversion creates the opposite debit | Quantity sized inside the safe band at the far edge of the price bound; the exception refuses a fill that could pass the cushion |
| The funding currency becomes the new debit, then the next cycle converts back | Funding is trade-date cash less working and armed buys; unbounded commitments hold; the exception re-checks free cash at the limit |
| The pair's position effect (open_short for a sell with no FX position) is meaningless for a currency balance | The exception judges only the ledger terms; the generic close-reduce path never admits CASH |
| Generic preview pricing uses a 0.01 tick for non-options, 100 pips on EUR.USD | A dedicated FX limit on the contract's minimum tick, bounded by `max_slippage_bp` |
| Weekend, the 17:00 New York break, or a wide market | Session refusal before any quote; wide quote refusal |
| The sweep invests EUR a working `SELL EUR.USD` is about to spend | Any working CASH order, of either side, makes the sweep's commitments unknown, so the sweep holds until it fills or is cancelled |
| A quantity that follows every ledger-rate update churns the row's revision | The quantity is kept while it stays inside the safe band |
| Leveling becomes an automatic money mover | Never pre-authorised or queued; each repayment is approved by the owner |
| Two rows approved in any order overdraw a currency both spend | What one loan takes is set aside before the next is planned; each row carries its allotment and the exception refuses a spend beyond it |
| A repayment of several conversions lands in part | Every conversion's checks run before any is sent; a later refusal leaves earlier ones inside their own share of the target and allotment, and the next cycle plans the rest |
| Selling a currency that earns more than the loan costs | A currency pays only while its cash rate is below the loan rate, and a conversion must earn back its worst-case cost within `payback_days` |
| Rates stale or wrong | Read from the broker's own statements; each rate shows its last day; only the last 180 days count; an unmeasured side stands in from the other side in the safe direction; without statements leveling holds |

## Interplay with the cash sweep

- The sweep still never converts. A working conversion holds the sweep. The
  sweep's `no_buy_while_borrowed` reads the lower of trade-date and settled
  cash; after leveling repays a debit, trade-date cash is about +cushion at
  once and settled cash follows at T+2, so the hold releases once the
  conversion settles.
- A debit inside leveling's band but beyond 1 unit keeps
  `no_buy_while_borrowed` holding bill buys, with nothing proposed to clear
  it. `leveling_debit_inside_band` names it; the owner converts by hand,
  lowers the band, or marks the currency deliberate carry. The sweep's
  messages now say "the sweep never converts" instead of "Canary does not
  convert".

## Tax note

In a EUR-based account every conversion is a foreign-exchange transaction.
For a German private investor, foreign currency counts as an asset: selling
one within a year of acquiring it can realise a gain or loss under §23 EStG
(private sale transactions; first in, first out; tax-free when all such
gains in a year stay under 1,000 EUR). Repaying a borrowed currency from the
base currency buys that currency and spends it at once, so little arises;
selling a currency the account holds, to repay a borrowed base currency, is
where a gain or loss can arise. IBKR withholds no German tax. This is the
general mechanism, not advice; the owner chose (2026-10-06 06:04 CEST) to
keep no tax setting in Canary.

## Policy

`[cash.leveling]` in `protection-policy.toml`, under the `[cash]` section
and its authority (owner decision 2026-10-06 06:08 CEST). `policy ensure`
writes the whole table, off, into a file without it, or the missing numbers
into an existing table, and raises `policy_version`. Values are read from
the file only:

| Key | Written | Meaning |
|---|---|---|
| `enabled` | `false` | off until the owner switches it on |
| `trigger_base` | 10000.0 | the band: a smaller loan is left alone |
| `cushion_base` | 250.0 | the balance a repayment lands at most; a paying currency keeps this much |
| `max_slippage_bp` | 2.0 | the limit's distance from the live mid (at most 100); also the price part of the payback test's cost |
| `payback_days` | 30 | a conversion must earn back its worst-case cost within this many days (1 to 365) |
| `currency.<CCY>.deliberate_carry` | not written (false) | keep a currency negative on purpose: never repaid, never pays |

Interest rates are not settings: they come from the broker's statements
("Several currencies").

Worked example (a synthetic book, none of it an account's; EUR.USD 1.17 and
the rates USD loan 5.1%, EUR cash 1.4% are illustrative): EUR cash +60,000,
USD cash −20,000, which is −17,094 EUR, beyond the 10,000 EUR band. The safe
band runs from 17,098 EUR (back to zero at 1.17 × (1 − 2 bp)) to 17,340 EUR
(the 250 EUR cushion at 1.17 × (1 + 2 bp)); the row sells 17,219 EUR.USD,
about 20,146 USD, leaving USD about +146 and EUR about +42,781. Within 30
days it saves about 52 EUR against a worst-case cost of about 5 EUR. Its
line reads: "Convert 17,219 EUR into about 20,146 USD: USD cash is −20,000
USD, a margin loan beyond the 10,000 EUR band; this brings it to about +146
USD".

## Assumptions (A), to verify at the first conversion

- (A1) IBKR's IDEALPRO commission is 0.2 bp of value, at least USD 2; under
  about USD 20,000–25,000 an order is an odd lot, about 1 pip worse.
- (A2) Spot settles T+2; interest is charged on settled balances.
- (A3) The ledger's per-currency CashBalance is trade-date cash (as the
  sweep's A4).
- (A4) Contract details carry the pair's contract id and minimum tick
  (IDEALPRO majors 0.00005); without a tick the limit snaps to 0.00001,
  toward the mid.
- (A5) IBKR accepts a CASH LMT DAY order on IDEALPRO through the protobuf
  `placeOrder` and answers its WhatIf; the first preview proves it, and the
  owner's approval of that preview is the last line.
- (A6, for "Several currencies") `InterestAccruals.interestAccrued` per
  currency is broker credit and debit interest only, accrued daily
  (weekends included) on settled cash; a blended rate is close enough to
  rank currencies and to test payback.
- (A7, for "Several currencies") IDEALPRO quotes a direct pair for every two
  currencies the plan pairs; a pair that does not resolve removes that payer
  for that loan.

## Verification

- Unit tests: `proposal_currency_leveling_test.go` (worked example, band,
  cushion bound for every conversion and the whole repayment, deliberate
  carry on both sides, unknown inputs, rate stand-ins, working conversions,
  rates deciding (a currency earning more than the loan costs, a spread too
  thin to pay back), two loans across four currencies, a borrowed base
  currency, a three-way split, splitting only when it pays, the dearer loan
  first, a shared payer never overdrawn, the repayment held to the cap,
  quantity kept, an unresolved pair, missing numbers including
  `payback_days`, validation, `ensure` writing `payback_days`),
  `currency_leveling_rates_test.go` (rates from statements: loan and cash
  sides, the balance floor, a re-fetched day, gaps, the lookback; stand-in
  direction), `currency_leveling_orders_test.go` (FX limit bounds, IDEALPRO
  hours, admission only for a leveling row, the exception's boundaries
  including the contract id and allotment, an end-to-end preview through the
  engine, refusals, never pre-authorised, the sweep holding beside a
  conversion, commitments, quantity stability, engine rows, bundles and
  their revision, pair resolution and its cache),
  `currency_leveling_bundle_trading_test.go` (trading build: a conversion of
  a bundle refuses a single prepare and submit; the bundle prepares both,
  sends both in order, cannot be sent twice; a stale second conversion, a
  wrong revision or an expired bundle sends nothing),
  `internal/flexstmt/fx_test.go` (interest evidence read independent of the
  attribution gates), `pkg/ibkr/place_order_cash_test.go` (encoder shape),
  policy check cases, `ensure` tests and the CLI goldens
  (`proposals_list_leveling*`).
- Live proof, pending: the first preview of a real conversion (A4, A5), then
  the first conversion on the owner's approval.
