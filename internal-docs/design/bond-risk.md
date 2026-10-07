# Bond risk (interest rate, issuer credit, concentration)

Updated: 2026-10-07 20:51 CEST
Status: phase 1 built (owner "Phase1 Go", 2026-10-07 20:43 CEST); phases 2
and 3 proposed. Every threshold below is `unapproved` until the owner sets
it.

This record follows `.agents/docs/risk-policy-contract.md`. It answers the
owner's request of 2026-10-06 21:34 CEST: "Design a proper handling solution
for bonds - they have a different risk profile for example interest rate
risk, counterparty risk (check issuer ratings!), ...". Bond orders themselves
are [bond-orders.md](bond-orders.md).

## Why bonds need their own measures

Canary's risk rules measure equity and option exposure: delta, premium,
concentration by underlying. A bond carries none of that and is invisible to
those rules today (rules 1 and 6–18 skip it). It carries other risks:

- Interest rate: a bond's price falls when yields rise, by about its modified
  duration per point. A 30-year bond loses about 15 to 17% on a one-point rise.
- Issuer credit: a downgrade widens the issuer's spread and lowers the price;
  a default loses most of the position. Government bonds of the five issuers
  Canary buys carry little of this; corporate and bank bonds carry it all.
- Concentration: one issuer's bonds can be a large share of the account
  without any rule noticing.
- Liquidity: corporate lines trade with wider spreads and larger minimums,
  and some quote only in local hours.
- Currency: a bond outside the base currency is FX exposure. Rule 14 already
  counts it through the account's currency exposure; nothing changes there.

## Meaning

- Capital base: net liquidation value in the base currency, as for every
  rule.
- Aggregation units: the held line (by contract id); the issuer (by evidence
  issuer, governments kept apart from other issuers); the currency curve
  (USD, EUR, GBP, CAD measured separately, never netted against each other).
- Risk-increasing: a bond buy. Risk-reducing: a sale of a held line. Unknown:
  a held line without maturity and coupon evidence; it is reported as
  unmeasured and never reads as a pass.
- Horizon: an instantaneous shock to today's prices. No carry and no
  roll-down.
- Enforcement: advisory first (shadow, then watch and act warnings in
  previews and the brief). Hard pre-trade gates only by a later owner
  decision.

## Measurements (pure, in `internal/risk`)

Per held line and per previewed buy:

| Measure | How | Source |
|---|---|---|
| Market value | IBKR's market value, converted to base | positions (broker) |
| Maturity, coupon | the issuer evidence of bond-orders.md, keyed by the held ISIN or CUSIP | TreasuryDirect, ECB list, OpenFIGI |
| Yield | IBKR's bid and ask yield ticks when sent, else solved from the clean price, coupon and maturity | bond quote |
| Modified duration | from the cash flows: annual coupons in EUR, semi-annual in USD, GBP and CAD (an assumption per currency, checked against the evidence where it names a frequency) | computed |
| DV01 | market value × modified duration × 0.0001, in base | computed |
| Issuer, class | evidence issuer; government or investment grade | evidence |
| Rating | IBKR's ratings field when it arrives (B5, proof pending after install); else "ECB-eligible, BBB- or better" for ECB-listed lines; else none | broker, ECB |

Book aggregates:

- Bond value as a share of NLV, in total and per currency.
- Rate shock loss: the loss on a parallel one-point rise, per currency and
  summed without offsets, as a share of NLV.
- Issuer exposure: each non-government issuer's value as a share of NLV;
  each government's value as a share of NLV, reported separately.
- Credit exposure: the value of non-government bonds as a share of NLV, and
  the loss on a spread widening applied to them only.
- Lowest rating held, and any line whose rating fell or that left the ECB
  list since it was bought.

## Proposed rules (advisory; numbers unapproved)

| Rule | Reads | Watch | Act | On act |
|---|---|---|---|---|
| Rate risk | one-point rate shock loss, % NLV | `unapproved` | `unapproved` | warn on every bond buy that adds duration; brief names the longest lines |
| Issuer concentration | largest non-government issuer, % NLV | `unapproved` | `unapproved` | warn on a buy of that issuer |
| Sovereign concentration | largest government, % NLV | `unapproved` (or none) | `unapproved` (or none) | warn on a buy of that government |
| Credit quality | lowest rating held; a downgrade below the floor; a line leaving the ECB list | rating floor `unapproved` | — | name the line and suggest review; never sell on its own |
| Bond data coverage | held lines without maturity or coupon evidence | any | — | data-quality watch, never a pass |

A buy whose post-trade figures would cross a watch or act level gets a
preview warning naming the figure, as rule 15 does today. None of these
changes submit eligibility until the owner makes one a hard gate.

## Ratings (issuer credit)

- First source: IBKR's ratings field, which the owner's subscription may
  deliver (B5). Canary keeps and shows it from this build on; the first
  `canary market --type BOND` read after install proves whether it arrives.
  If it does, the owner names the agency order (for example Moody's, then
  S&P) and the lowest rating Canary may buy, and a non-government buy then
  also needs a current rating at or above that floor.
- Floor without ratings: being on the ECB list (BBB- or better under the
  general framework) stays the investment-grade proof of B3.
- Ratings are broker text: shown and compared against the floor, never
  followed as an instruction.

## Stress

- The "US equity/options exposure" row counts equities and options only.
  Its gross figure comes from the positions themselves (sum of absolute base
  market values of non-bond rows) instead of IBKR's gross position value,
  which includes bonds.
- A new "Rates and credit" row reads the rate shock loss, the credit spread
  shock on non-government bonds, and the loss if the largest non-government
  issuer defaulted. Shock sizes and the recovery rate are `unapproved`.
- No row assumes bonds hedge equities: in an inflation shock both fall.

## Authority and evidence

| Concept | Authoritative source | Typed field | Freshness | Fallback |
|---|---|---|---|---|
| Held face and value | IBKR positions | `PositionView`, `PositionBond` | positions read | unmeasured |
| Maturity, coupon, issuer | issuer evidence (bond-orders.md) | new `PositionBond` evidence fields | 24 h; ECB list up to 4 days | unmeasured |
| Yield | IBKR bond quote | `BondQuote` yields | live or delayed as sent | solved from price |
| Rating | IBKR ratings field | `BondContract.Ratings` | per lookup | ECB floor, else none |

## Owner decisions needed (all `unapproved`)

1. Rate risk watch and act levels: loss on a one-point rise, % NLV.
2. Non-government issuer watch and act levels, % NLV.
3. Whether governments get a concentration level, and which.
4. Rating floor and agency order, once ratings are proven to arrive.
5. Stress sizes: rate shock, spread shock for non-government bonds, recovery
   rate on default.
6. The equity exposure row counting equities and options only (this record
   proposes it).
7. Whether any of these ever becomes a hard pre-trade gate.

## Phase 1 as built

- `risk.MeasureBond` solves the yield to maturity at the mark (coupon dates
  stepped back from maturity, accrued interest evenly between them, days
  over 365.25) and derives modified duration; the one-point loss reprices
  fully, convexity included. `risk.SummarizeBondBook` sums the book.
- Held lines take coupon, maturity, issuer and class from the same issuer
  evidence a buy is admitted by; a bill without evidence is measured as a
  zero with an unknown class and counts as non-government. A line IBKR has
  flagged as factor-priced (inflation-linked), an unclassified line, or one
  whose maturity disagrees with its evidence is named as not in the sums.
- Surfaces: the positions bonds section and `bond_risk` summary (CLI table,
  JSON, MCP `canary_positions`); a buy preview's yield, duration and
  one-point loss; the stress read's "Rates and credit" row (always observe)
  and, on the equity exposure row, the gross figure without bonds as
  evidence only. The brief and Desk read these daemon fields in a later
  change.
- Ratings: IBKR sends none (proven 2026-10-07), so no rating is shown.

## Phases

1. Measure and show, no thresholds: positions and the brief show each held
   bond's duration, DV01, issuer, class and rating; previews of a bond buy
   show its duration and the loss on a one-point rise; the stress row change
   and the new row run in shadow.
2. Advisory rules with the owner's numbers (decisions 1–5).
3. Hard gates only on decision 7.

## Verification

- Fixtures with known answers: a 10-year 3% annual bond at par has a
  modified duration of about 8.53; a 2-year zero at 4% about 1.92.
- Property tests: duration rises with maturity and falls with coupon; DV01
  scales with face; a missing input is unmeasured, never zero.
- A held line without evidence reads unmeasured in rules, stress and brief.
- Cross-surface parity: CLI, JSON, MCP brief and app read the same figures.
- Shadow period of two weeks with bonds held before advisory warnings.
