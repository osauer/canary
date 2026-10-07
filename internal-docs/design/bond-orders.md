# Bond orders (government and investment-grade bonds by identifier)

Updated: 2026-10-07 19:45 CEST
Status: built on branch `bond-trading-feature-8531ae`; not installed. Proof
pending: one read-only preview per issuer row after install.

This record follows `.agents/docs/daemon-cli-trading-contract.md` and
`.agents/docs/risk-policy-contract.md`. It records the owner's decisions of
2026-10-06 and creates none.

## Owner decisions

- B1 (2026-10-06 20:17 CEST): "We should trade government bonds plus
  investment grade. On government, all five." The five governments are the
  United States, Germany, France, the United Kingdom and Canada. This
  replaces the owner's government-only answer given a few minutes earlier.
- B2 (2026-10-06 20:17 CEST): a bond buy may mature at most 30 years ahead.
  The number lives in `risk-policy.toml` `[order_limits]
  max_bond_maturity_years`.
- B3 (2026-10-06, about 20:37 CEST): a non-government bond proves investment grade
  only by being on the ECB's list of eligible marketable assets that day
  ("ECB list only").
- B4 (2026-10-06, about 20:44 CEST): a UK gilt or a Government of Canada bond is
  proven by OpenFIGI, with its maturity and coupon matching IBKR's own
  description of the same line ("OpenFIGI + IBKR agree"). Any disagreement or
  outage refuses the buy.
- B5 (2026-10-06, about 20:54 CEST): the owner holds a bond ratings
  subscription on the IBKR account. IBKR documents the contract-details
  ratings field as not delivered for bonds. Canary now keeps the field and
  shows it in `canary market --type BOND`, so the first read after install
  proves whether ratings arrive. Ratings decide nothing until the owner names
  the agency and the lowest admitted rating; B3 stays the investment-grade
  proof.
- B6 (2026-10-07 08:27 CEST): while `max_bond_maturity_years` is missing,
  only bond and bill buys are refused; every other order, exits and stops
  included, is judged as usual. This departs from the general rule that any
  missing `[order_limits]` key refuses every preview; the desk settings
  review raised it against the owner's 08:13 ruling that a tighter cap must
  not block delta-reducing exits.
- B7 (2026-10-07 19:39 CEST): TWS's API precautions for bonds are bypassed
  ("Bond (Bills) order size ... nominal par value" notice, "Don't display
  again"; "Bypass negative yield to worst confirmation for API orders":
  "yes"). Until then TWS held every bond WhatIf behind a modal dialog, and
  the WhatIf timed out with nothing sent. Canary now makes the negative-yield
  check itself: a bond or bill buy is refused when its limit price is at
  least 100 plus the coupon for the years left (code `bond_negative_yield`;
  exact at a coupon date, close between them), cash sweep bills included.
- Size: no separate face cap. The order cap in force (`[order_limits]`
  floor, percent of NLV and ceiling) bounds every bond order, as it bounds
  every other order.

## Scope

- Goal: buy and sell one bill or bond by ISIN or CUSIP, with a face amount,
  through the same gated preview and place path a stock order uses.
- Command: `canary order preview buy|sell <ISIN|CUSIP> <FACE> --type
  BOND|BILL --currency CCY`, then `canary order place --preview-token`.
  `FACE` is the nominal amount in the bond's currency. The daemon computes the
  order quantity from it.
- Owner layer: daemon (`internal/daemon`), contract (`internal/rpc`),
  limit (`internal/risk` `[order_limits]`), CLI renderer.
- Enforcement class: pre-trade hard gate. Every order still needs an
  explicit transaction-specific instruction from the owner.
- Not in scope: MCP stays read-only; the app gains no new order entry; no
  modify of a working bond order (cancel and preview again); no explicit
  limit price; no rule measuring interest-rate or credit risk across the
  book (unapproved, see "What still bounds risk").

## Admission of a buy

A buy is admitted only when every step holds. A sell needs only steps 1, 5
and 6, because selling a held line reduces risk. It may never exceed the
held face less every other sale of the line working at the broker: the
broker's complete open-order list is read at preview, place and the wire
guard, and a sale is refused while that list is unavailable.

1. Identity. IBKR resolves the identifier to one line on the preview's
   broker session (BOND first, then BILL, or the reverse for `--type
   BILL`). The line's identifier channels must contain the requested ISIN or
   CUSIP and none may contradict it. The contract id, security type and grid
   come from that line.
2. Issuer evidence, from one source per issuer, keyed by the exact
   identifier:

   | Issuer | Source | Admits | Refuses |
   |---|---|---|---|
   | US Treasury (CUSIP 912…) | TreasuryDirect securities search by CUSIP | Bill, Note, Bond, CMB | TIPS, FRN |
   | Any ISIN on the ECB list | ECB eligible marketable assets, daily file | Asset type bond, MTN, bill, covered bond; coupon zero or fixed | ABS, multi-cédulas, variable coupon |
   | UK gilt, UK Treasury bill | OpenFIGI by ISIN, checked against IBKR's description | Ticker UKT (gilt stock) or UKTB (GBP bill) | UKTI (index-linked) |
   | Government of Canada | OpenFIGI by ISIN, checked against IBKR's description | Ticker CAN or CTB, security type CANADIAN | CANRRB (real return) |

   Class: an ECB issuer group IG2 (central government), TreasuryDirect, and
   the UK and Canada rows are `government`. Any other ECB row is
   `investment_grade`. The Eurosystem's general framework admits marketable
   assets from credit quality step 3 (BBB-) up. Unverified: whether any asset
   that the 2020 pandemic measures kept eligible after a downgrade below BBB-
   is still on the list. Anything else is refused with the reason, for
   example a USD corporate bond that is not on the ECB list. Evidence is keyed
   by the identifier the owner named, never by another identifier the
   broker's line also carries.
3. Not indexed. IBKR flags a factor-priced line with notice 2130 ("trading
   on the basis of currency price with factor"). Such a line is refused even
   when a source admits it, because its price is per 100 of indexed
   principal and the order's value would be understated. The ECB codes
   inflation-linked bonds as fixed coupon (read 2026-10-06: OAT€i
   FR0013327491 and FR0010447367, BTP€i IT0005138828, Bund€i DE0001030575 all
   `CD4`), so on that path the notice is the only signal. Live reads of
   2026-10-06, about 21:25 CEST: IBKR sent 2130 for the German, French and Italian
   linkers and not for the nominal OAT FR0011461037. The daemon remembers
   every contract it has seen flagged, so a later lookup without the notice
   still refuses. Unproven: that IBKR flags every linker on its first lookup
   of a session.
4. Maturity and coupon come from the evidence. The maturity must lie after
   the next seven days and no later than `max_bond_maturity_years` from
   today. Where IBKR sends a maturity or the UK and Canada rule compares
   descriptions, a disagreement refuses.
5. Currency. `--currency` is required for a bond. It must equal the
   evidence's currency (US Treasury USD; ECB denomination; UKT and UKTB GBP;
   CAN and CTB CAD) and IBKR's line currency when sent. Only USD, EUR, GBP
   and CAD are admitted, the currencies whose IBKR unit is known.
6. Quantity. One order unit is 1,000 of face in USD and 1 of face in EUR,
   GBP and CAD (assumption A5 of the cash sweep). USD proven live
   2026-10-07 19:39 CEST: a WhatIf for 1 unit of a 7-year Treasury at 99.14
   raised initial margin by 44.33 EUR on a 930.29 EUR order, ratio 0.048;
   a 1-of-par unit would have moved it by cents. EUR, GBP and CAD wait for
   an accepted WhatIf in their markets' hours. The face must
   be a whole number of units on the line's size grid. A buy's WhatIf
   initial-margin change must lie within the sweep's unit band (0.005 to 1.2
   of the order's value). The band catches the realistic error, a unit of 1
   read as 1,000 or the reverse; it does not catch an error of 10 to 60
   times in a margin account.

## Order and value

- Shape: the cash sweep's one bond order, a DAY limit on the line's minimum
  tick, priced as a patient limit from a live two-sided quote inside the
  line's own trading hours. `ibkrlib.NewBondLimitOrder` refuses anything off
  the grid.
- Value for the order cap: face × price / 100, plus for a buy an accrued
  interest bound of one year's coupon (face × coupon / 100). Accrued
  interest never exceeds one coupon period, so the bound holds for any
  payment frequency. The preview shows both parts.
- The draft carries the evidence (issuer, class, source, as-of, maturity,
  coupon) inside `OrderBondTerms`. The signed token binds it, and place
  re-checks the maturity cap against the policy in force.

## Contract changes

- `rpc.OrderPreviewParams.BondOrder` (`identifier`, `face`) is the one
  serialised bond input. `OrderPreviewParams.Bond` stays `json:"-"`: no RPC
  caller sets bond terms, conventions or evidence; the daemon builds them.
  The cash-sweep invariant "no RPC caller can preview a BOND" becomes "no
  RPC caller can author bond terms".
- `[order_limits] max_bond_maturity_years`: daemon start writes 30 (B2) after
  a backup; while it is missing only bond buys are refused (B6). An older
  binary refuses a file that carries it, as for every earlier key. Surfaces
  name it "Bond maturity, buys" with "at most 30 years" (CLI) and "bonds ≤ 30
  years" (app); a missing limit reads "not set", never 0 (desk settings
  review, 2026-10-07).

## Measurement fixes that ship with it

- A held bill or bond no longer enters the portfolio's equity delta.
  `addStock` counted quantity × price per 100, which overstates a EUR bond by
  about 100× and understates a USD bond by 10×.
- A bill or bond buy no longer warns that it adds to rule 15's net exposure:
  rule 15 never counts bonds. The premium budget and margin warnings still
  apply to every buy.
- A position change in an order, proposal or opportunity preview prints every
  digit; a face quantity of 10,000 read "1e+04".

## Technical numbers in code (review M4)

- A buy maturing within 7 days is refused (settlement and redemption
  overlap).
- The ECB list serves until it is 4 days past its publication date, which
  carries Friday's list over a long weekend; B3's "that day" reads as "the
  newest list published".
- A TreasuryDirect or OpenFIGI answer serves 24 hours; the preview shows when
  it was read.

The owner acknowledged all three on 2026-10-06 at 21:34 CEST ("Keep in
code") after the review asked for a decision or a policy key.

## Bond risk

The stress read's "US equity/options exposure" row takes IBKR's gross
position value, which includes bonds, so a bond-heavy book can reach its
watch level from bonds alone. Asked about it on 2026-10-06 at 21:34 CEST, the
owner asked for a proper treatment of bond risk instead (interest rate,
issuer credit with ratings, and the rest): [bond-risk.md](bond-risk.md).
Until it is built the row is unchanged.

## What still bounds risk

Admitting these orders loosens the sweep-only rule for BOND previews. After
the change:

- Each order: the order cap in force, the 30-year maturity limit, government
  or ECB-eligible issuers only, fixed or zero coupon only, no short, sells
  at most the held face, and the owner's per-transaction instruction.
- The book: nothing caps total bond exposure, duration or issuer
  concentration. Repeated orders could move most of the account into
  30-year bonds; a one-point rise in yields would then cost about 17% of
  that value, and an investment-grade issuer default could lose the whole
  position. Cash, margin (rule 19 excess liquidity) and the Rulebook's buy
  warnings are the only other limits. A rate-risk or concentration rule is
  unapproved and needs an owner decision.

## Sources and failure

| Source | Read | Kept | On failure |
|---|---|---|---|
| TreasuryDirect search | per CUSIP, at preview | 24 h in memory | buy refused, reason named |
| ECB eligible assets | daily file (about 1 MB compressed), on first need | until a newer file; at most 4 days old | buy refused, reason named |
| OpenFIGI mapping | per ISIN, at preview, no key | 24 h in memory | buy refused, reason named |

No source fills an identity field the broker owns, and no source text is
an instruction. The evidence decides admission only; IBKR's line decides
contract id, grid and hours.

## Verification

- Tests: synthetic fixtures for each source (no account data); a regression
  test per refusal row above; the delta fix with a test that fails on the
  old code; a CLI golden render of a bond preview.
- Gates: `make test` (both build modes), `make check`, `make docs-regen`.
- Live: after install, one read-only preview per issuer row (US note, Bund,
  OAT, gilt, Canada, one ECB-listed corporate) with the token discarded; the
  first real order only on the owner's instruction.
