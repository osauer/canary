# Cash sweep (idle cash into same-currency bills)

Updated: 2026-09-30 21:32 CEST
Status: Phase B installed (daemon v3.14.0-33, d819dfc4); post-install proof
step 1 passed on 2026-09-30 at d819dfc4: a US bill resolved as BILL by
symbol with a live quote (see "Post-install findings", F1, F3 and F4). A4
answered (false, F2); the remaining proof steps are in progress.

This record follows `.agents/docs/risk-policy-contract.md`. It records the
owner's decisions of 2026-09-30 09:10 CEST (S1–S6), the reviewer's decisions
on the open items of 2026-09-30 09:30 CEST (O1–O7) and the owner's decisions
of 2026-09-30 12:35 CEST (P1, verbatim: "no shadow, armed. Human need to
approve anyway. Do all follow-ups.") and 12:45 CEST (the order path may be
built), and creates none. Items marked **(A)** are broker assumptions to
verify, never facts.

P1 reading: the owner's approval of each order is the last line; software
gates ahead of it that only restate a review (the tax gate) are unwanted, so
the tax review becomes advisory, and `instrument_support_required` is
retired. Technical blockers that stop a malformed order (no live quote, a
size or price off the bill's grid, a closed session) are not gates and stay.
The mode default stays `shadow` in code; `mode = "active"` is the owner's
line to write, and so is `cash_sweep` under `pre_authorised`.

## Decision

- Goal: idle cash earns a near-risk-free return in its own currency. Protected:
  the settlement float, currency neutrality (the sweep never converts) and the
  full veto window.
- Owner: the desk owner. Decisions: S1 instruments, S2 bucket and authority,
  S3 band numbers, S4 reserve accounting, S5 assumptions and tax, S6 phases.
- Authoritative: the protection policy file (`protection-policy-fp-v1`); code
  `internal/daemon/proposal_cash_sweep.go`, `protection_policy.go`,
  `internal/rpc/cash_sweep.go`.
- Reading of S2: `cash_sweep` is the first bucket that buys.
  `close_reduce_only` stays true for every other bucket; a sweep BUY is
  admitted only for the closed vocabulary in its own currency (O1).

### Decisions on the open items (2026-09-30 09:30 CEST)

| # | Decision | Where it lives |
|---|---|---|
| O1 | Confirmed. The `close_reduce_only` carve-out exists for the `cash_sweep` bucket only, only for the row's own resolved bill (a vocabulary instrument in the row's own currency), only up to `free` cash in face value and in cost at the preview's limit, and within `max_order_notional`. It is a typed, tested exception, not a general relaxation; it is also the only way a BOND passes the preview's security-type check. | `closeReduceOnlyException`, `cashSweepOpenException`, `cashSweepBondAdmitted`, the effect and security-type checks in `proposalPreviewSafetyBlockers` (`cash_sweep_orders.go`) |
| O2 | `min_maturity_days` per currency, default 28 (the four-week US bill); rung targets spread evenly from `min_maturity_days` to `max_maturity_days`. EUR default min 28, max 182. | `defaultCashSweepCurrency`, `cashSweepRungTargets` |
| O3 | Accepted. The compiled EUR default declares `fallback = "etf"` with no symbol; the template comment shows the owner-approved example ETF on Xetra; the status reads `needs_your_number` for the fallback symbol only, and bills still plan. | `writeCashSweepTemplate`, `missingNumbers` |
| O4 | Superseded by P1 (2026-09-30 12:35 CEST): `tax_reviewed_at` is advisory. Unset, every row carries the detail line "tax treatment not yet confirmed (tax_reviewed_at unset)" and the status `tax_reviewed: false`; nothing is blocked. | `cashSweepRow`, `cashSweepPlanFor` |
| O5 | Accepted: `max_order_notional` has no default (`needs_your_number`). | `protectionCashSweepPolicy.missingNumbers` |
| O6 | Accepted: `keep_cash` 5,000 per currency. | `defaultCashSweepCurrency` |
| O7 | Out of scope (USD balance as FX exposure is a separate decision). | — |

R2 (ETF margin tripping the Rulebook-basis reserve) is out of scope and moot
for the governor: rule 3 is the premium budget since Rulebook amendment 17,
and no cash reserve remains to trip.

## Meaning

- Base and unit: per currency, in that currency. Only the `max_order_notional`
  comparison is in base currency, at the ledger rate.
- Vocabulary (closed): `us_tbill`, `de_bubill`, `fr_btf`, `uk_tbill`,
  `ca_tbill`, `etf` (requires `etf_symbol`, `etf_exchange`), `none`. Validation
  refuses an instrument outside its currency. Compiled defaults: USD
  `us_tbill`; EUR `de_bubill`, `fr_btf`, `fallback = "etf"` with no symbol
  (`needs_your_number`, O3); GBP `uk_tbill`; CAD `ca_tbill`; CHF, JPY and any
  unlisted currency `none`.
- Per currency: `keep_cash` 5,000, `min_tranche` 1,000, `min_maturity_days` 28,
  `max_maturity_days` 91 (EUR 182, ceiling 397), `ladder_rungs` 4. Bucket:
  `enabled`, `mode`, `max_order_notional` (no default), `tax_reviewed_at`.
- Cash: `cash` is the lower of trade-date and settled cash; settled cash is
  proven by the broker's per-currency `SettledCash` observation. The journal
  estimate cannot prove actual settlement dates, holiday calendars or
  account-wide fill coverage; without the broker observation the sweep holds
  at `settlement_unknown` (A4 is currently false). `committed` is working BUY
  orders with a fixed finite limit plus authorised (armed, held or sending)
  queued orders at their finite worst price. Unknown bounds or nonfinite
  totals hold the sweep; `free = cash − committed − keep_cash`;
  `cash_like = cash + cash equivalents` when both are known.
- Band: invest when `free > min_tranche`: one BUY in the bill's whole order
  units on its size grid, capped by `max_order_notional`. Redeem when
  `cash − committed < keep_cash`: one SELL of the nearest maturity (or the
  ETF) covering the gap, held to `max_order_notional` (the next cycle sells
  the rest), skipped when a held bill pays out first. Otherwise nothing. An unauthorised proposal or queue
  entry is never a commitment. Maturities return to cash; the band re-sweeps.
- Ladder: rung targets spread evenly from `min_maturity_days` to
  `max_maturity_days` (O2). A tranche goes to the rung holding least face
  value, into the bill maturing nearest its target, never earlier than
  `min_maturity_days` and never later than `max_maturity_days`.
- Cash equivalents: vocabulary-issuer bills in that currency with ≤ 397 days
  left, plus the declared ETF by ConID, at broker market value.
- Classification: invest rows BUY `open`/`increase` a cash equivalent; redeem
  rows SELL `reduce`/`close`. Nothing converts or leaves the vocabulary.
- Enforcement: advisory. Shadow lists and journals (`shadow_mode`); active
  rows are ordinary proposals under every gate, freeze included, sent on the
  owner's approval or, when `cash_sweep` is pre-authorised, by the daemon
  after the full veto window. An unset `tax_reviewed_at` adds a detail line
  only (P1). Every row sets `NeverSkipVeto`.
- Unknown posture, per currency, generating nothing: `cash_unavailable`,
  `settlement_unknown` (no broker per-currency `SettledCash`, or unbounded
  commitments), `equivalents_unclassified`, `needs_your_number`. An
  invest verdict with no bill to name reads `universe_unavailable` (no
  candidate list) or `instrument_unresolved` (no candidate confirmed), with
  the evidence. A missing bill line is inferred only from a completed contract
  search, so the ETF fallback still never acts. Nil is unavailable, never
  zero.
- Assumptions (S5, extended by Phase B): (A1) IBKR pays about benchmark −
  0.5% above 10,000 per currency, nothing below; (A2) T-bill initial margin
  about 1%, ETF 25–50%; (A3) German tax measures bill rolls in EUR, FX
  component taxable; (A4) `$LEDGER` CashBalance is trade-date, and
  `$LEDGER:ALL` carries a per-currency `SettledCash` field that is settled
  cash in that currency (verified false 2026-09-30, see "Post-install
  findings"; absent means the sweep holds); (A5) each instrument's quantity unit and price convention (table
  under Phase B as built), minimum, session and T+1 settlement; (A6) a held
  bill's issuer is read from its identifier: US Treasury bill CUSIPs
  (912794–912797), else the ISIN's country (DE, FR, GB, CA); (A7) IBKR answers
  a BILL or BOND contract-details request (the identifier as the symbol, as IBKR
  documents bonds; `secIdType`/`secId` only as the fallback, refuted as the
  first form for US bill CUSIPs 2026-09-30) with `bondContractData` frames, sends bond prices per 100 of face and yields in
  percent (ticks 50–52, delayed 103–105), and needs no generic ticks for a
  bond quote; (A8) a bond line's contract details carry its minimum size and
  size increment in order units, its minimum tick per 100 of face, and its
  liquid (else trading) hours in its time zone, and a LMT DAY order by
  contract id on SMART, carrying the line's own security type (A10), is how
  IBKR takes a bill order; without hours, the
  assumed weekday sessions of the conventions table apply; (A9) IBKR's
  WhatIf for a bill buy reports an initial-margin change (and no order cost)
  whose ratio to the order's value lies between 0.005 and 1.2: about 0.01
  where a bill is margined at one percent (the owner's margin account), about
  1.0 in a cash account; a 1,000-fold unit error lands near 10 or near
  0.00001, outside the band either way (reviewer decision 2026-09-30 15:45
  CEST); (A10) IBKR lists US Treasury bills as secType `BILL` (the TWS
  API's security-type vocabulary has BILL beside BOND, and held bills
  arrive as BILL), so `us_tbill` is asked as BILL only; a German, French, UK
  or Canadian bill is asked as BILL first and BOND second, and the row's
  `bill.sec_type` records which one resolved; which type each non-US
  instrument resolves as is to verify per instrument (proof steps 1–2).

## Authority And Evidence

| Concept | Authoritative source | Typed field/contract | Freshness/finality | Fallback or blocker |
|---|---|---|---|---|
| Numbers, instruments, mode | protection policy file | `protectionCashSweepPolicy`, `[buckets.cash_sweep.currency.<CCY>]` | hot reload, version bump | absent or disabled ⇒ silent |
| Cash per currency | `$LEDGER:ALL` CashBalance | `rpc.CurrencyExposure.CashCcy` + `CashObserved`; the base row in `AccountResult.BaseCurrencyLedger` | per account refresh (one-shot request only) | `cash_unavailable` |
| Settled cash | broker per-currency ledger observation (A4 false in the current gateway) | `rpc.CurrencyExposure.SettledCashCcy`, `cashSweepLedgerRow.Settled`; `settled_cash_source: broker` | per account refresh | `settlement_unknown`; journal estimates never admit orders |
| Commitments | broker open-order inventory, queued authorisations | `cashSweepCommitments` | per refresh | `settlement_unknown` |
| Held equivalents | positions view and its `bonds` section | `rpc.PositionsResult.Bonds` (`classifyBondPositions`), ETF by ConID (not yet) | per refresh, `Stale` honoured | `equivalents_unclassified` |
| USD bill universe | TreasuryDirect securities API (public, no key) | `billUniverse`, daemon.db `cash_sweep_us_bill_universe_v1` | daily; served up to 48 h | `universe_unavailable` |
| EUR, GBP, CAD universe | owner's `isins` per currency | `protectionCashSweepCurrency.ISINs` | hot reload | `universe_unavailable` |
| Bill lines, quotes | IBKR BILL or BOND contract details (A10), quotes | `ibkr.BondContractDetails`, `bondDirectory`, `rpc.TradeProposalCashSweepBill` | details cached a day, quotes a minute | `instrument_unresolved`, `fresh_bill_quote_required` |
| Order grid, session | the line's contract details, re-read by contract id on the preview's own session | `ibkr.BondOrderRules`, `rpc.OrderBondTerms`, `rpc.BondSession` | per preview | `contract_unresolved`, `bond_order_invalid`, `market_closed` |
| Status | proposal snapshot | `rpc.TradeProposalSnapshot.CashSweep` (`cash_sweep`) | per refresh | — |
| Row arithmetic, flags | proposal | `rpc.TradeProposal.CashSweep`, `.Shadow`, `.NeverSkipVeto`, `AutomaticEligible()` | per refresh | shadow ⇒ `shadow_mode` |
| Cash-like figures | brief, rule 14 | `BriefReadySection.Cash`, rule 14 notes | per brief / per Rulebook read | nil when unavailable |

Post-trade truth: fills and maturities arrive through the position stream and
ledger; ConID is the reconciliation key. Rule 14's figure is unchanged by a
sweep; its evidence gains cash and equivalents per currency. Rule 3 is the
premium budget since Rulebook amendment 17; its measure reads no cash.

## Exceptions And Change Control

- Only the owner, by editing the file and bumping `policy_version`; the
  fingerprint change is the audit record.
- An edit can change numbers or set `none`; it cannot add an instrument or a
  conversion.
- Rollback: `enabled = false` or remove the table; rows leave next refresh,
  held bills mature to cash.
- The table is pointer-typed: a file without it keeps its fingerprint. A
  written currency table takes the compiled default for every key it leaves
  out; a currency without a table follows the compiled default at evaluation.

## Operating Cadence

- Daily: `canary brief` cash row per currency (cash, equivalents, sum), while
  the sweep is enabled.
- Pre-trade: `canary proposals` "Cash sweep" section with band figures and
  rows under their own heading; preview refuses shadow rows.
- EOD: outcomes and events carry `shadow`; maturities show in the ledger.
- Weekly: shadow rows against broker cash interest (A1) argue for active mode.
- Stale ledger or quote blocks the row; nothing silently passes.
- Mode is the owner's (P1 reads "armed"); the tax review is advisory.

## Verification

- Fixtures (synthetic books): invest, redeem and hold at each band edge;
  commitments; an unauthorised queue entry ignored; rung choice;
  `max_order_notional` hold; CHF `none`; EUR fallback only after an empty
  search; journal gap.
- Invariants: no conversion; no BUY outside the vocabulary or maturity cap,
  above free cash (in face and in cost) or off the bill's grid; no
  invest/redeem alternation on unchanged inputs; absent from `canary policy
  default protection`; shadow refused by preview and submit; an unresolved
  row never `AutomaticEligible()`; no RPC caller can preview a BOND.
- Adversarial: ETF symbol and exchange are policy data matched by ConID; no
  broker text enters a decision.
- Parity: CLI text, JSON `cash_sweep`, MCP `canary_proposals`, SPA blocker path.
- Phase B: the post-install proof below (resolve and quote, the positions
  section, the SettledCash tag, one whatIf preview, the grid and the hours);
  the first live orders add a redacted BUY, SELL and maturity artifact per
  currency.

Phase A tests: `internal/daemon/protection_policy_cash_sweep_test.go`,
`internal/daemon/proposal_cash_sweep_test.go` (including a 2,000-book random
invariant run), `internal/cli/proposals_cash_sweep_test.go`,
`internal/risk/rulebook_cash_like_test.go`. Phase B tests (synthetic
identifiers): `pkg/ibkr/bond_test.go` (frame decoding and fail-closed
prefixes, identifier check digits, the ISIN request and a definition
rejection, yield ticks, no generic ticks for bonds, SettledCash in the typed
ledger), `internal/daemon/cash_sweep_phase_b_test.go` (settled cash
precedence, `cash_like`, held-bill classification, nearest-bill selection,
fail-closed resolution, listed EUR bills, `isins` validation, TreasuryDirect
parsing and aging, directory caching, positions classification, the bond
check), `internal/cli/cash_sweep_phase_b_test.go`,
`internal/mcp/market_bond_test.go`. Order path tests: `pkg/ibkr/bond_order_test.go`
(the grid from contract details, quantity and price checks, construction
refusals, `ValidateOrder` and the protobuf encoder for BOND, the hours
parser, no generic ticks on the exact order session),
`internal/daemon/cash_sweep_orders_test.go` (sizing on the grid, sessions,
redemptions, bond valuation in commitments and settlement, readiness and the
scheduler's session wait, the pre-authorised vocabulary, the end-to-end BOND
preview and its refusals, BOND refused without a sweep row's terms, the
unit check's ratio band (0.01 and 1.0 pass; 1,000-fold errors either way and
0.001 are refused) and its latch),
`internal/daemon/cash_sweep_orders_trading_test.go` (a pre-authorised buy
waits the full window and the session, then reaches the broker as one BOND
LMT DAY order with its grid), `internal/app/alerts/presentation_test.go`.

## Phase A as built

What the code does where the record above left room; each is a reading of the
design, not a new threshold.

1. **Quantity unit.** A bill line is unknown until Phase B, so an invest row's
   quantity is face value in whole units of its currency
   (`quantity_unit = face_value`; an ETF buy is a cash amount). A5 replaces
   this with the bill's own unit and minimum in the paper proof.
2. **Cap below the tranche.** When `max_order_notional` holds one order below
   `min_tranche`, the currency holds (the "`max_order_notional` hold" fixture);
   above it, the order is held to the cap and the next cycle sweeps the rest.
3. **Pending redemptions.** Unsettled sale proceeds of cash equivalents count
   toward `keep_cash` on the redeem side, so a redemption is not sold again
   while `cash` (the lower of trade-date and settled) still waits for it. The
   invest side is unchanged, so the band keeps its dead zone.
4. **Settlement estimate.** The journal's previous-weekday window is an
   unverified estimate, never settled-cash authority. T+2 products, settlement
   holidays and unobserved fills can make it overstate settled cash. Without
   a broker per-currency `SettledCash` observation the sweep holds at
   `settlement_unknown`. A broker-supported complete settlement projection
   is needed before a journal fallback can admit an order.
5. **Fills and orders Canary cannot value** (a bond fill, a currency
   conversion, a buy without a price bound, a fill without a currency) make the
   affected currency `settlement_unknown` rather than guessing.
6. **Held equivalents in Phase A.** There is no bill classifier and no ConID
   for the declared ETF yet, so any bond or bill holding, or a holding carrying
   the declared ETF's symbol, makes its currency `equivalents_unclassified`.
   The symbol only ever blocks; it never admits.
7. **Base currency cash.** `CurrencyExposure` leaves the base row out so FX
   consumers never count it; the base row is served as
   `AccountResult.BaseCurrencyLedger`. `CashEquivalentsCcy` is not on the
   account RPC: the account summary reads no positions, so equivalents are
   served where positions are read (the brief's cash row, rule 14's notes and
   the sweep status).
8. **Opt-in surfaces.** The brief's `cash` row and rule 14's notes appear only
   while `[buckets.cash_sweep]` is enabled, so nobody else's brief or Rulebook
   changes.
9. **One rung** targets `max_maturity_days`.

## Phase B read-only half as built

Built on `feat/cash-sweep-b` from Canary `33f76ced`. Nothing here builds or
sends an order; each item is a reading of the design or of P1, not a new
threshold.

1. **Tax review advisory (P1).** `tax_reviewed_at` unset adds the row detail
   "tax treatment not yet confirmed (tax_reviewed_at unset)" and the status
   `tax_reviewed: false`; `tax_review_required` is gone. `canary policy
   status` lists the advisory line while it is unset.
2. **`cash_like`** per currency on the status: `cash` (the lower of
   trade-date and settled) plus `cash_equivalents`, absent unless both are
   known.
3. **Settled cash.** `SettledCash` joined the typed `$LEDGER` allowlist
   (`ibkr.CurrencyLedger.SettledCash`, `SettledCashObserved`) and reaches
   `CurrencyExposure.settled_cash_ccy` and the base ledger row. When present
   it is settled cash (`settled_cash_source: broker`); pending redemptions
   still come from the journal, and without it every unsettled net sale
   proceed (trade-date − settled, at least zero) counts toward `keep_cash`,
   which can only hold a redemption back. The journal estimate cannot admit
   orders. Post-install, A4 is false (F2): no gateway row reaches this path,
   so the sweep holds at `settlement_unknown`. A bare `SettledCash_<CCY>` in the streaming map
   is reqAccountUpdates' account-level figure and never reads as a
   currency's settled cash.
4. **Bonds at the broker (read-only).** `pkg/ibkr/bond_frames.go` decodes
   `bondContractData` (message 18) in the negotiated server version's layout
   (IBKR API 10.37: a message version before 164, trading hours from 188,
   size rules from 164): the identity (the request id, the typed fields
   through the minimum tick, a positive contract id, a currency that is
   empty or a three-letter code) must decode; an empty or unreadable
   maturity or currency is a gap the line carries (F4). Size rules,
   `secIdList`, the long name and the hours are kept only when the whole
   frame decodes. An ordinary `contractData` frame (message 10) answering
   the request is a line too, bond-only fields empty. A line without a
   currency takes the one asked. `Connector.BondContractDetails` asks by
   ISIN, CUSIP or contract id, epoch-bound, ending on the gateway's
   rejection. An identifier is asked as the symbol first (IBKR's documented
   bond form: symbol, type, SMART, currency, nothing else), then by
   `secIdType`/`secId`, then by symbol with no exchange, each only when the
   one before found no line (F1, F4). While a form is in flight every frame
   that names its request id is logged at INFO (WARN for a contract frame
   that is not a line) and recorded, reduced to identifiers. Bond quotes
   ride the subscription manager's short-lived hold (no standing line) with no
   generic ticks; yield ticks 50–52 and 103–105 are stored as yields, never
   prices. `canary market --symbol <ISIN|CUSIP> --type BILL|BOND` (text,
   `--json`) and MCP `canary_market` with `bond_identifier` (asked as BOND)
   run the same read: one line, one quote; a gap is `resolved`/`quoted`
   false with a reason. The requested type is asked first; an identifier of
   a vocabulary bill is then asked as the bill's own types (A10), and the
   result's `sec_types` and `sec_types_note` say so. The currency defaults to USD for a CUSIP, else the ISIN's issuer
   country; an `XS` ISIN needs `--currency`. `--json` carries `attempts`:
   each form sent (`form`, `req_id`, `sec_type`, `symbol` or
   `sec_id_type`/`sec_id`, `exchange`, `currency`), its `outcome` (`line`,
   `no_line`, `rejected`, `failed`) with IBKR's `code` and `message`, and
   `frames`: every frame that named the request (`msg_id`, `kind`, raw
   `fields` count, `layout`, `con_id`, `sec_type`, identifiers, `exchange`,
   `currency`, `maturity` as sent, `line`, `complete`, `note`), with the
   negotiated `server_version` and `lookup_as_of`.
5. **Positions.** Held BOND rows stay in `stocks` with their valuation, so
   Desk, the SPA, portfolio aggregates and the Rulebook see no change; the new
   `bonds` section classifies each by `con_id` as `bill` (zero coupon, at most
   397 days from issue to maturity, or a Treasury bill CUSIP when the issue
   date is missing), `bond`, or `unresolved` with a reason, with maturity,
   days to maturity, coupon, ISIN/CUSIP and currency. `canary positions`
   prints them under *Bills & bonds* and keeps unclassified rows in the stock
   table. Contract details are read by contract id, cached a day (failures
   retried after ten minutes), and a cold read waits at most 1.5 s before the
   lookup finishes detached. `positionWireSecType` keeps a held BILL or BOND
   row's own type, so any proposal built on a held bill or bond outside a
   sweep row names it and preview refuses it (`unsupported_security_type`);
   none is generated today.
6. **Held-bill equivalents.** A classified bill is a cash equivalent when its
   issuer is a vocabulary issuer of the row's currency (A6); its face value is
   quantity × the instrument's assumed face per unit (A5). Any other bond is
   not an equivalent; an unresolved row makes its currency
   `equivalents_unclassified`, as in Phase A.
7. **USD universe.** TreasuryDirect's
   `https://www.treasurydirect.gov/TA_WS/securities/Bill?format=json` is read
   once a day while the sweep is enabled and USD declares `us_tbill`, retried
   every 15 minutes after a failure, kept in daemon.db
   (`public-treasurydirect:bills` / `cash_sweep_us_bill_universe_v1`) and
   served up to 48 hours, so one missed daily read does not stop the sweep;
   older, or never read, it is `universe_unavailable` with the last error. An
   offline or test state database never reads the network. Records that are
   not bills, lack a valid CUSIP or a maturity (announced reopenings) are
   skipped; a reopened CUSIP counts once.
8. **Bill selection.** For an invest verdict: USD candidates are issued bills
   maturing inside `[min_maturity_days, max_maturity_days]`; EUR, GBP and CAD
   candidates are the owner's `isins` (validated: check digit, an instrument
   the currency declares, once each; refused for USD), resolved to learn their
   maturity. Candidates are tried nearest the rung's target first, at most
   three per cycle, inside a 10-second budget: contract details must name one
   BILL or BOND line (A10) in the currency (for USD, maturing on the list's date) and the
   quote must carry a price. The first confirmed bill is the row's `bill`
   (ISIN/CUSIP, contract id, maturity, days, price per 100 of face and its
   source, the quote, min size, assumed unit and convention); the contract is
   the bill's contract id. A quote that is not a live bid or ask keeps the row
   and adds `fresh_bill_quote_required`. Nothing confirmed reads
   `instrument_unresolved` with evidence per candidate; no row exists.
9. **Row identity.** Superseded by the order path (item 3 below): the key
   binds the bill's contract id, and the quantity counts the bill's order
   unit.
10. **Not built.** The ETF fallback (no ETF resolution; the completed-search
    input stays empty) and the mode default.

Instrument conventions (A5, A10), explicit constants in
`internal/daemon/cash_sweep_instruments.go`, each an assumption to verify; the
order path sizes, prices and times orders by them. The assumed session applies
only when the line's contract details carry no liquid or trading hours; it is
weekdays only (holidays not modelled: on one the preview's live-quote
requirement refuses instead):

| Instrument | IBKR secType, in order (A10) | Quantity unit | Face per unit | Price | Assumed session |
|---|---|---|---|---|---|
| `us_tbill` | BILL | `face_1000` | 1,000 USD | per 100 of face | 08:00–17:00 America/New_York |
| `de_bubill` | BILL, then BOND | `face_1` | 1 EUR | per 100 of face | 09:00–17:30 Europe/Berlin |
| `fr_btf` | BILL, then BOND | `face_1` | 1 EUR | per 100 of face | 09:00–17:30 Europe/Paris |
| `uk_tbill` | BILL, then BOND | `face_1` | 1 GBP | per 100 of face | 08:00–16:30 Europe/London |
| `ca_tbill` | BILL, then BOND | `face_1` | 1 CAD | per 100 of face | 08:00–17:00 America/Toronto |
| `etf` | — (STK) | `shares` | — | per share | its exchange calendar |

A held line is asked by contract id as its position's own type first, then
the instrument's. The invest row's contract, its preview draft and its order
carry the type the line resolved as (`bill.sec_type`); a redemption carries
the position's type.

A working bond order or a bond fill in the journal is valued at its
currency's bill convention (USD 1,000 face per unit, EUR, GBP and CAD 1), the
same assumption; a bond in any other currency stays unvalued (unknown).

### Post-install proof (read-only, one whatIf)

Run by the owner after install, on the live session, with no order sent: the
whatIf preview in step 6 transmits nothing, and nothing here submits. Record
redacted evidence here (identifiers of public government bills are fine;
never an account id, a balance or an order reference).

1. USD bill: pick one CUSIP from TreasuryDirect's list maturing 28–91 days
   out; `canary market --symbol <CUSIP> --type BILL` and again with `--json`.
   Expect `resolved: true`, one line, `sec_type: BILL` (A10), `class: bill`,
   the list's maturity,
   `min_size`/`size_increment`, and a quote with bid/ask per 100 of face and
   yields in percent (A7), `fresh: true` during the session (a quote without
   the gateway's feed-type notice reads stale; record which). Note whether
   the quantity unit matches `face_1000` (A5). `--type BOND` for the same
   CUSIP asks BOND, then BILL, and its `Asked` line says so; a gap line
   names every attempt with IBKR's code and text. If BILL also finds no
   line, A10 is refuted for US bills: record the gap line. Either way,
   record from `--json` each attempt's `outcome`, `code` and `frames`
   (`kind`, `fields`, `layout`, `line`, `note`): they say which frames IBKR
   answered each form with and why each became a line or did not (F4).
   Passed 2026-09-30 at d819dfc4: a US bill resolved as BILL by symbol with
   a live quote.
2. EUR bill: pick one German Bubill ISIN (`DE…`) maturing 28–182 days out;
   `canary market --symbol <ISIN> --type BILL` (currency inferred EUR; BILL,
   then BOND) and `--json`. Same expectations; record `sec_type` (which type
   the Bubill resolved as, A10) and whether one unit is 1 EUR of face.
   Repeat for one listed bill of every other declared currency when one is
   configured (BTF, UK and Canadian bills), recording each `sec_type`.
3. Positions: `canary positions` and `canary positions --json`. Every held
   BILL or BOND row appears in `bonds` with its `con_id`, class, maturity and
   currency (none `unresolved` after a second read), and still in `stocks`.
4. SettledCash tag: `canary account --json` shows `settled_cash_ccy` on each
   `currency_exposure` row and on `base_currency_ledger`; with the sweep
   enabled, `canary proposals list --json` shows
   `cash_sweep.currencies[].settled_cash_source: "broker"`. If the tag is
   absent, A4 is false and the sweep holds; record that.
   Answered 2026-09-30: absent (F2).
5. Freshness: outside a bill's session the check reads `fresh: false` with a
   reason, and a sweep row carries `fresh_bill_quote_required`.
6. One whatIf preview of a USD bill row, never a submit: with the sweep
   enabled and `mode = "active"` (and `cash_sweep` not pre-authorised),
   during the bill's session, `canary proposals preview KEY REVISION` for the
   USD invest row. Expect `accepted: true`, a BILL LMT DAY draft for the
   row's contract id (`cash_sweep.bill.sec_type: BILL`), `quantity` in `face_1000` units, `bond.face_value` =
   quantity × 1,000, a limit on the line's tick, `notional` = face × limit /
   100, and a WhatIf verdict; record the WhatIf's commission and margin
   change. A WhatIf that reads the quantity 1,000 times larger (or smaller)
   than the face value refutes A5 for USD; the preview then reads
   `bill_unit_mismatch`. Record the ratio of the margin change to the
   order's value: about 0.01 is the margin account's one-percent bill margin
   (A2, A9); a ratio outside [0.005, 1.2] on a correct unit refutes A9. Do
   not submit.
7. Size and tick read back: `canary market --symbol <CUSIP> --type BILL
   --json` for that bill shows `min_size`, `size_increment` and `min_tick`;
   they must equal the draft's `bond.min_size`, `bond.size_increment` and
   `bond.min_tick`, and `min_size` should read 1 for `face_1000` (a reading
   of 1,000 or more says IBKR counts USD bills in face value, refuting A5).
   Repeat the read for one listed EUR bill: `min_size` of 1,000 or similar
   fits `face_1`; a `min_size` of 1 there says IBKR counts EUR bills in
   thousands, and EUR orders must wait for a change to the convention.
8. Bond session hours: the row's `cash_sweep.session` names its `source`;
   record whether it is `liquid_hours` or `trading_hours` (A8) and the
   windows for the next business day, against IBKR's published hours for
   US Treasury bills and for the EUR bill. `assumed` means the contract
   details carried no hours; record that too. Outside the session the row's
   readiness reads `market_closed` with the next open.

### Post-install findings (2026-09-30)

F1, proof step 1, USD bill lookup. `canary market --symbol <CUSIP> --type
BOND` for two outstanding 13-week bills on TreasuryDirect's list
(912797SK4, 912797UM7) read "Resolved no (0 lines) · contract details:
IBKR lists no such bond line". The installed build sent one
reqContractDetails per bill: conId 0, no symbol, secType `BOND`, exchange
`SMART`, currency `USD`, `secIdType` `CUSIP`, `secId` the CUSIP. The daemon
log shows IBKR's answer to each (19:24:16 and 19:24:32 CEST, reproduced
19:39:20; no symbol alias because the request carried none): code 200, "No
security definition has been found for the request". IBKR documents a bond contract as the
CUSIP or ISIN in the symbol field (secType BOND, SMART, the currency), and
ties CUSIP data to its CUSIP market-data subscription, the likely reason the
`secIdType` CUSIP form finds nothing here. Code 200 is a definition verdict;
a missing Treasury trading permission rejects an order, not a contract
search. Fix: an identifier is asked as the symbol first and by
`secIdType`/`secId` only when that finds no line; a line naming another
identifier of the requested type is dropped; when neither form finds a line
the gap line names the request and each form's answer with IBKR's code and
text. Rerun step 1 after the reinstall; if the
symbol form also reads code 200, the next suspects are bond market-data or
the CUSIP subscription (Client Portal, Settings, Market Data
Subscriptions), which the gap line will show.

F3, proof step 1 rerun after the F1 fix (installed as v3.14.0-29,
2026-09-30 19:58 CEST, read-only). With Bonds trading permission on the
account, `canary market --type BOND` for 912797SK4 and 912797UM7, by CUSIP
and by ISIN (US912797SK41, US912797UM78), still read no line: by symbol the
search ended without a line, and by `secIdType` IBKR answered code 200 "No
security definition has been found for the request". Every request named
secType `BOND` on SMART in USD. The TWS API lists US Treasury bills as
secType `BILL`, and Canary's own non-issuer vocabulary in
`internal/daemon/rulebook.go` already names BILL beside BOND because held
bills arrive that way: a bill asked as BOND finds nothing. Fix (A10): the
vocabulary carries each instrument's security types (`us_tbill` BILL; the
other bills BILL, then BOND); a lookup asks every form of the first type
before the next, and the line keeps the type it resolved as; the row, its
preview draft, the WhatIf and the order carry it, and every bill path tests
BILL or BOND through one helper (`ibkr.IsBillOrBond`). `canary market
--type BILL` asks BILL; `--type BOND` for a vocabulary bill's identifier
asks BOND, then the bill's own types, and says so. Rerun steps 1–2 after
the reinstall.

F4, proof step 1 rerun after the F3 fix (installed as v3.14.0-30,
2026-09-30 20:22–20:24 CEST, read-only). `canary market --type BILL` for
the bills of F3, by CUSIP and by ISIN, still read no line: "BILL by symbol:
the search ended without a line; BILL by secIdType CUSIP/ISIN: IBKR 200".
The daemon log carries code 200 only for the `secIdType` requests; the
symbol requests logged neither an error nor a line. The negotiated server
version is 203, so contract data arrives as text frames (protobuf contract
data starts at 205), and the request matched IBKR's official 10.37 encoder
field for field. IBKR answered the failed `secIdType` searches with code
200, not with the end marker alone, so the symbol form most likely drew
frames the lookup did not keep, and nothing recorded them: the bondContractData decoder
refused any frame without a YYYYMMDD maturity or a three-letter currency,
silently, while IBKR documents that bond market-data licensing leaves only
a few fields of a bond description populated (the minimum tick, the
exchange, the short name). Not settled live yet: which frames IBKR sends
for a bill asked by symbol. Fix: a frame is a line when its identity
decodes (request id, typed fields through the minimum tick, a positive
contract id); a missing or unreadable maturity or currency is a gap the
line names, the currency asked stands in, and the sweep still refuses a
line without a maturity or whole-frame size rules. An ordinary contractData
frame answering the request is decoded in full (bond-only fields empty). A
third form asks by symbol with no exchange; the request is encoded exactly
as the documented bond form (no SMART default, no primary exchange). Every
frame that names a bond request's id, of any message, is logged at INFO
with its message id, raw field count and identifiers, a contract frame that
is not a line at WARN with the reason, and the same record reaches
`canary market --json` as `attempts`. Rerun step 1 with `--json` after the
reinstall and record the attempts; if the symbol form still ends without a
line with no contract frame recorded, IBKR sends the end marker alone for
it, and the next suspect is the bond market-data or CUSIP subscription.

F2, proof step 4, SettledCash (A4). The account-summary request names
`$LEDGER:ALL` and no `SettledCash` tag. IBKR's `$LEDGER` cash-balance set
(CashBalance, TotalCashBalance, AccruedCash, market values by class,
NetLiquidationByCurrency, UnrealizedPnL, RealizedPnL, ExchangeRate and
kin) carries no SettledCash, and the live `canary account --json` shows no
`settled_cash_ccy` on any row although the parser admits a per-currency
SettledCash in both wire dialects. `SettledCash` itself is an account-level
tag: one figure in the base currency covering every currency, so it cannot
be one currency's settled cash and is not requested; reqAccountUpdates
already streams it under the base currency's suffix, and the legacy ledger
scan no longer reads that bare key as the base currency's settled cash.
A4 is false. The safety review rejects the journal derivation as settled-cash
authority: it lacks actual settlement dates, holiday calendars and complete
account-wide fills. Every currency without broker per-currency settled cash
therefore stays `settlement_unknown`; daemon age never removes that hold.

## Phase B order path as built

Built on `feat/cash-sweep-b` on the read-only half (owner decisions of
2026-09-30 12:35 and 12:45 CEST). Each item is a reading of the design or of
P1, not a new threshold; no new policy number exists.

1. **BOND in the broker protocol** (`pkg/ibkr/bond_order.go`). One order
   shape: LMT DAY for a contract id on SMART. `BondOrderRules` (minimum tick,
   minimum size, size increment) come from the line's contract details (a
   complete frame; a line without them cannot be ordered);
   `NewBondLimitOrder` refuses at construction a quantity below the minimum
   or off the size step (the increment, else the minimum) and a price off the
   minimum tick. `ValidateOrder` refuses any BOND order without the grid or
   off it, so neither the protobuf nor the legacy encoder can send one; the
   protobuf encoder admits BOND only as LMT DAY. The WhatIf and submit
   builders carry the grid with the contract. An exact-session bond quote asks
   for no generic ticks; `BondContractDetailsForSession` reads the line on the
   preview's own socket. `ParseTradingHours` reads IBKR's hours list (dated
   and undated closes, CLOSED days, several spans a day); any malformed
   segment refuses the list.
2. **Sizing.** An invest row buys the planned cash at the higher of par and
   the bill's quoted price, in whole order units (A5), rounded down to the
   grid: neither its face value nor its cost passes the free cash. A tranche
   below the bill's minimum holds the currency with the reason. A redemption
   is held to `max_order_notional` at its mark like a buy (the brief's "bounded
   by max_order_notional per order"; Phase A left sales uncapped), reads its
   held bill's line by contract id and rounds its sale up to the step, or down
   when up would pass the position or the cap; a line it cannot read blocks
   the row (`bill_contract_rules_unavailable`), a sale the grid cannot fit (a
   position below the bill's minimum) blocks it (`below_minimum_increment`).
3. **Row identity and binding.** An invest row's key binds the bill's
   contract id, so a preview, prepared submit or pre-authorised record of a
   key buys the bill the owner saw; a new bill is a new row with a new window.
   Sweep rows bind their key, whole order units and position effect to the
   snapshot revision. Cash movements below one order unit leave it stable;
   a changed order requires a new automatic notice and full veto window.
   Automatic submission also pins the quantity named in that notice. The
   persisted public revision chains semantic sweep transitions, so an order
   disappearing after a fill and later recurring at the same capped size
   receives a fresh episode, notice and window; old terminal records cannot
   prevent that future sweep. Equivalent effective terms retain the window.
   The preview binds to the row's own terms: the key, the quantity
   capped by the free cash at preview (the O1 exception, in face and in cost
   at the draft's limit, and `max_order_notional` in base), and a prepared
   submit's exact comparison of the reviewed terms (contract id, quantity).
4. **Preview admission.** `rpc.OrderPreviewParams.Bond` is daemon-internal
   (`json:"-"`): the proposal engine sets the bill's conventions for a sweep
   row, and a BOND preview without them is refused, so no RPC caller can
   preview a bond. The preview re-reads the line by contract id on its own
   session, checks it is still a bill of the row's instrument, refuses a
   closed session before any quote (`market_closed`), prices a patient limit
   on the line's tick from a live two-sided quote read during the preview (a
   buy at the mid rounded down, never below the bid; a sell at the mid
   rounded up, never above the ask), values the order at face × price / 100
   for `[trading].max_notional` and FX, builds it through
   `NewBondLimitOrder` (`bond_order_invalid` otherwise), and runs WhatIf.
   Every existing gate still decides: trading mode and freeze, the
   agent/human authority of the submit, position and base-currency
   authority, the risk limits, the token and the journal. A bond short or
   flip is refused outright. `proposalPreviewSafetyBlockers` admits BOND only
   for a sweep row's own bill: the invest row through the typed exception, a
   redemption as a SELL that reduces or closes the held bill.
5. **Netting.** A sweep row nets like a reduction: an order working at the
   broker for its exact bill and side, or one Canary sent and the broker has
   not acknowledged, holds the preview until it fills or is cancelled.
   Working bond buys count as committed cash at their limit (A5 by currency),
   bond fills in the settlement window are valued the same way, and a bill
   sold inside the window counts as a pending redemption toward `keep_cash`.
6. **Session and readiness.** A row's `cash_sweep.session` is its bill's
   liquid (else trading) hours from contract details, else the assumed
   session of the conventions table. Readiness reads it: `market_closed`
   with the next open outside it; a stale quote
   (`fresh_bill_quote_required`) is `quote_unusable`, a transient wait, not a
   refusal. `instrument_support_required` is retired; a row exists only when
   its bill resolved and quoted in the current cycle, and the declared ETF
   (matched by contract id, which Canary does not resolve) reads
   `instrument_unresolved` instead of a row.
7. **Pre-authorised path.** `cash_sweep` joins the pre-authorised vocabulary
   and `automaticBucketFor`. Every sweep row is `NeverSkipVeto`: the full
   veto window always, even under the latched brake; each order is bounded by
   `max_order_notional` (the exception) and waits for its bill's session
   (due at the open plus five minutes). Automation pauses on policy drift or
   error, and on config defaults, exactly as for the other buckets. Its
   notice has its own copy (`protection_auto_cash_sweep`, no "now" variant).
   Nothing enables it: the owner writes `pre_authorised`.
8. **Unit check against the broker** (reviewer decisions 2026-09-30 15:25
   and 15:45 CEST). A wrong face unit is caught by the broker's own figure,
   not a policy gate: an invest preview divides the accepted WhatIf's
   initial-margin change (IBKR's WhatIf sends no order cost for a bond) by
   the order's expected value, face × limit / 100 at the assumed unit, in the
   margin currency (the account base or the bill's currency). A ratio outside
   [0.005, 1.2] (A9), or no readable figure or one in a third currency,
   refuses the preview with `bill_unit_mismatch` naming both figures, the
   unit and both bounds, and latches
   the currency's instrument: its invest rows carry the blocker (so no
   submit, prepared submit or pre-authorised record passes) until a preview
   of it checks clean, which clears the latch. Every submit previews and
   checks again. The latch is daemon memory; a restart forgets it.
   Redemptions are not checked: their quantity is the broker's own position
   count.

Not built: ETF resolution by contract id and the fallback's completed-search
input (the ETF neither invests nor classifies); a spread limit for bills (no
policy number exists; inventing one needs an owner decision).

## Implementation plan

Line references at Canary `a53daed0`.

| File | Change |
|---|---|
| `internal/daemon/protection_policy.go` | bucket pointer; types, validation, defaults and needs-number appended; Phase B vocabulary `:64–85`, `:658` |
| `internal/daemon/proposal_cash_sweep.go` (new) | pure plan and rows; commitments, settlement and classification inputs; the typed O1 exception |
| `internal/daemon/proposal_budget.go` | `shadowProposalBlockers` names the sweep's own shadow blocker |
| `internal/daemon/proposal_engine.go` | refresh appends sweep rows and status; counts; clone; effect checks take the O1 exception; Phase B: `positionWireSecType` (BOND becomes STK today), sec types |
| `internal/rpc/cash_sweep.go` (new), `opportunities.go` | status, row and state vocabulary; snapshot, counts and proposal fields |
| `internal/rpc/rpc.go`, `internal/daemon/handlers.go`, `cash_sweep_account.go` (new) | `CashObserved`, `BaseCurrencyLedger`; Phase B bond rows, `positionSecType` |
| `internal/rpc/brief.go`, `internal/daemon/brief.go` | Ready `cash` row |
| `internal/risk/rulebook.go`, `rulebook_cash_like.go` (new), `internal/daemon/rulebook.go` | rule 14 notes |
| `internal/daemon/policy_files.go`, `policy_file_status.go` | commented template block; needs-your-number lines |
| `internal/cli/proposals.go`, `proposals_cash_sweep.go` (new), `brief.go`, `internal/mcp/tools.go` | section, brief line, description |
| `internal/daemon/proposal_automatic.go` | Phase B: pre-authorised vocabulary, bond session (built) |
| `pkg/ibkr/connection.go`, `place_order_proto.go` | Phase B: bond contract handler; BOND LMT DAY (built) |
| `pkg/ibkr/bond_order.go`, `internal/daemon/order_preview_bond.go`, `cash_sweep_orders.go` (new) | Phase B order path: the grid, the one order shape, the hours; the BOND preview; sizing, sessions, the O1 exception, netting |

Phase A (built):

1. rpc types, status states, blocker codes.
2. Schema, defaults, validation, fingerprint.
3. Cash presence flag and commitments.
4. Pure planner: band, ladder, equivalents.
5. Rows with `instrument_support_required`; engine wiring.
6. Brief and rule 14 cash-like figures.
7. CLI section and JSON; MCP description.
8. Template, `make docs-regen`, this record.
9. Tests; `make check`.

Phase B:

1. Read-only: BOND contract details by ISIN/CUSIP, quotes (built).
2. `msgBondContractData`; bond quotes; position classification; bill
   selection; SettledCash; advisory tax review (built on
   `feat/cash-sweep-b`: `pkg/ibkr/bond.go`, `internal/rpc/bond.go`,
   `internal/daemon/bond_directory.go`, `cash_sweep_bills.go`,
   `cash_sweep_universe.go`, `cash_sweep_instruments.go`,
   `internal/cli/market_bond.go`, `positions_bonds.go`).
3. Order path (built, above); `instrument_support_required` retired.
4. Post-install proof (above); then the first live orders, each on the
   owner's approval.

## Risks and open decisions (ranked)

| # | Item | Handling |
|---|---|---|
| O1 | First buying bucket; `close_reduce_only` carve-out | decided: typed exception limited to vocabulary, currency and free cash |
| R1 | Broker per-currency settled cash is absent (A4 false) | the sweep stays `settlement_unknown`; the journal estimate cannot certify settlement and is not an order-authority fallback |
| R2 | ETF margin (A2) lowers available funds | out of scope; moot for the governor (rule 3 is the premium budget since Rulebook amendment 17) |
| O2 | Rung 1 under four weeks | decided: `min_maturity_days` 28 |
| O3 | EUR fallback symbol, exchange | decided: owner writes; `needs_your_number`; bills still plan |
| O4 | German tax (A3) | superseded by P1: advisory detail line and `tax_reviewed: false` |
| O5 | `max_order_notional` | decided: owner number; `needs_your_number` |
| O6 | `keep_cash` 5,000 earns nothing (A1) | decided: 5,000; owner reviews per currency |
| R3 | Bill minimums, spreads (A5) | built: orders on the contract's grid only, patient limits from a live two-sided quote; a tranche below the minimum holds; no spread limit (owner number) |
| R4 | TreasuryDirect unreachable | list served up to 48 h; then `universe_unavailable` for USD only |
| R5 | Issuer read from identifiers (A6) | only classifies held bills as equivalents; buys use only TreasuryDirect CUSIPs and the owner's ISINs |
| R6 | Quantity unit and price convention (A5, A7) | the order path sizes and prices by them; a wrong EUR, GBP or CAD unit (IBKR counting in thousands) would make an order 1,000 times its intended face: the post-install proof (steps 6–7) checks it before any order, and the owner's approval of the previewed quantity and WhatIf is the last line |
| R7 | Bond session hours (A8) | contract liquid or trading hours when sent, else the assumed weekday session; a holiday reads open and the live-quote requirement refuses |
| R8 | The unit check's figure (A9) | the WhatIf's initial-margin change ÷ the order's value must lie in [0.005, 1.2], admitting the margin account's one-percent bill margin and a cash account; a bill margined below 0.5% would read `bill_unit_mismatch` and stay blocked (fail-closed), which the post-install proof step 6 shows |
| O7 | USD balance as FX exposure | out of scope; the sweep never converts |
