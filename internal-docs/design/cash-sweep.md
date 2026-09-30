# Cash sweep (idle cash into same-currency bills)

Updated: 2026-09-30 13:14 CEST
Status: Phase B read-only half implemented on feat/cash-sweep-b; order path
pending owner authorisation; post-install proof pending.

This record follows `.agents/docs/risk-policy-contract.md`. It records the
owner's decisions of 2026-09-30 09:10 CEST (S1–S6), the reviewer's decisions
on the open items of 2026-09-30 09:30 CEST (O1–O7) and the owner's decision
of 2026-09-30 12:35 CEST (P1, verbatim: "no shadow, armed. Human need to
approve anyway. Do all follow-ups."), and creates none. Items marked **(A)**
are broker assumptions to verify, never facts.

P1 reading: the owner's approval of each order is the last line; software
gates ahead of it that only restate a review (the tax gate) are unwanted, so
the tax review becomes advisory. Every follow-up of Phase A's read-only side
is built here; the order path stays a separate, owner-authorised task, so
every row still carries `instrument_support_required`. The mode default
stays `shadow` in code; `mode = "active"` is the owner's line to write.

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
| O1 | Confirmed. The `close_reduce_only` carve-out exists for the `cash_sweep` bucket only, only for an instrument in the closed vocabulary in the row's own currency, only up to `free` cash. It is a typed, tested exception, not a general relaxation; Phase A rows still carry `instrument_support_required`, so no preview or submit can pass. | `closeReduceOnlyException`, `cashSweepOpenException`, the two effect checks in `proposalPreviewSafetyBlockers` |
| O2 | `min_maturity_days` per currency, default 28 (the four-week US bill); rung targets spread evenly from `min_maturity_days` to `max_maturity_days`. EUR default min 28, max 182. | `defaultCashSweepCurrency`, `cashSweepRungTargets` |
| O3 | Accepted. The compiled EUR default declares `fallback = "etf"` with no symbol; the template comment shows the owner-approved example ETF on Xetra; the status reads `needs_your_number` for the fallback symbol only, and bills still plan. | `writeCashSweepTemplate`, `missingNumbers` |
| O4 | Superseded by P1 (2026-09-30 12:35 CEST): `tax_reviewed_at` is advisory. Unset, every row carries the detail line "tax treatment not yet confirmed (tax_reviewed_at unset)" and the status `tax_reviewed: false`; nothing is blocked. | `cashSweepRow`, `cashSweepPlanFor` |
| O5 | Accepted: `max_order_notional` has no default (`needs_your_number`). | `protectionCashSweepPolicy.missingNumbers` |
| O6 | Accepted: `keep_cash` 5,000 per currency. | `defaultCashSweepCurrency` |
| O7 | Out of scope (USD balance as FX exposure is a separate decision). | — |

R2 (ETF margin tripping the Rulebook-basis reserve) is out of scope and moot
for the governor: rule 3 is becoming a premium budget in a parallel change.

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
  the ledger's `SettledCash` per currency (A4), else derived from Canary's
  order journal; `committed` is working BUY orders plus authorised (armed,
  held or sending) queued orders; `free = cash − committed − keep_cash`;
  `cash_like = cash + cash equivalents` when both are known.
- Band: invest when `free > min_tranche`: one BUY in whole face units, capped
  by `max_order_notional`. Redeem when `cash − committed < keep_cash`: one SELL
  of the nearest maturity (or the ETF) covering the gap, skipped when a held
  bill pays out first. Otherwise nothing. An unauthorised proposal or queue
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
  rows are ordinary proposals under every gate, freeze included. An unset
  `tax_reviewed_at` adds a detail line only (P1).
  `instrument_support_required` stays on every row until the order path is
  authorised. Every row sets `NeverSkipVeto`.
- Unknown posture, per currency, generating nothing: `cash_unavailable`,
  `settlement_unknown` (no ledger `SettledCash` and a journal gap in the
  settlement window), `equivalents_unclassified`, `needs_your_number`. An
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
  cash in that currency (verify post-install; absent, the journal derivation
  stays); (A5) each instrument's quantity unit and price convention (table
  under Phase B as built), minimum, session and T+1 settlement; (A6) a held
  bill's issuer is read from its identifier: US Treasury bill CUSIPs
  (912794–912797), else the ISIN's country (DE, FR, GB, CA); (A7) IBKR answers
  a BOND contract-details request by `secIdType`/`secId` with
  `bondContractData` frames, sends bond prices per 100 of face and yields in
  percent (ticks 50–52, delayed 103–105), and needs no generic ticks for a
  bond quote.

## Authority And Evidence

| Concept | Authoritative source | Typed field/contract | Freshness/finality | Fallback or blocker |
|---|---|---|---|---|
| Numbers, instruments, mode | protection policy file | `protectionCashSweepPolicy`, `[buckets.cash_sweep.currency.<CCY>]` | hot reload, version bump | absent or disabled ⇒ silent |
| Cash per currency | `$LEDGER:ALL` CashBalance | `rpc.CurrencyExposure.CashCcy` + `CashObserved`; the base row in `AccountResult.BaseCurrencyLedger` | per account refresh (one-shot request only) | `cash_unavailable` |
| Settled cash | `$LEDGER:ALL` SettledCash (A4), else the order journal | `rpc.CurrencyExposure.SettledCashCcy`, `cashSweepLedgerRow.Settled`, `cashSweepSettlement`; `settled_cash_source` | per account refresh | journal fallback; `settlement_unknown` when both are missing |
| Commitments | broker open-order inventory, queued authorisations | `cashSweepCommitments` | per refresh | `settlement_unknown` |
| Held equivalents | positions view and its `bonds` section | `rpc.PositionsResult.Bonds` (`classifyBondPositions`), ETF by ConID (not yet) | per refresh, `Stale` honoured | `equivalents_unclassified` |
| USD bill universe | TreasuryDirect securities API (public, no key) | `billUniverse`, daemon.db `cash_sweep_us_bill_universe_v1` | daily; served up to 48 h | `universe_unavailable` |
| EUR, GBP, CAD universe | owner's `isins` per currency | `protectionCashSweepCurrency.ISINs` | hot reload | `universe_unavailable` |
| Bill lines, quotes | IBKR BOND contract details, quotes | `ibkr.BondContractDetails`, `bondDirectory`, `rpc.TradeProposalCashSweepBill` | details cached a day, quotes a minute | `instrument_unresolved`, `fresh_bill_quote_required`; `instrument_support_required` until the order path |
| Status | proposal snapshot | `rpc.TradeProposalSnapshot.CashSweep` (`cash_sweep`) | per refresh | — |
| Row arithmetic, flags | proposal | `rpc.TradeProposal.CashSweep`, `.Shadow`, `.NeverSkipVeto`, `AutomaticEligible()` | per refresh | shadow ⇒ `shadow_mode` |
| Cash-like figures | brief, rule 14 | `BriefReadySection.Cash`, rule 14 notes | per brief / per Rulebook read | nil when unavailable |

Post-trade truth: fills and maturities arrive through the position stream and
ledger; ConID is the reconciliation key. Rule 14's figure is unchanged by a
sweep; its evidence gains cash and equivalents per currency. Rule 3 is left
to its parallel change.

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
- Invariants: no conversion; no BUY outside the vocabulary or maturity cap; no
  invest/redeem alternation on unchanged inputs; absent from `canary policy
  default protection`; shadow refused by preview and submit; Phase A rows never
  `AutomaticEligible()`.
- Adversarial: ETF symbol and exchange are policy data matched by ConID; no
  broker text enters a decision.
- Parity: CLI text, JSON `cash_sweep`, MCP `canary_proposals`, SPA blocker path.
- Phase B read-only: the post-install proof below (resolve and quote, the
  positions section, the SettledCash tag); the order path adds a redacted
  BUY, SELL and maturity artifact per currency.

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
`internal/mcp/market_bond_test.go`.

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
4. **Settlement window.** From the start (UTC) of the previous weekday; fills
   since then are unsettled (T+1, A5). Holidays are not modelled; they can only
   make the window too long, which lowers settled cash. A journal gap is an
   unreadable journal or a daemon started inside the window; the journal also
   cannot see fills Canary never observed (other clients, the mobile app),
   which is R1's residual.
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
   which can only hold a redemption back. The journal derivation stays the
   fallback (`journal`); `settlement_unknown` needs both to be missing.
4. **Bonds at the broker (read-only).** `pkg/ibkr/bond.go` decodes
   `bondContractData` (message 18) with the strict cursor: the identity
   prefix (through the minimum tick, a positive contract id, a three-letter
   currency, a YYYYMMDD maturity) must decode; size rules, `secIdList` and
   the long name are kept only when the whole frame decodes.
   `Connector.BondContractDetails` asks by ISIN, CUSIP (`secIdType`/`secId`)
   or contract id, epoch-bound, ending on the gateway's rejection. Bond quotes
   ride the subscription manager's short-lived hold (no standing line) with no
   generic ticks; yield ticks 50–52 and 103–105 are stored as yields, never
   prices. `canary market --symbol <ISIN|CUSIP> --type BOND` (text,
   `--json`) and MCP `canary_market` with `bond_identifier` run the same
   read: one line, one quote; a gap is `resolved`/`quoted` false with a
   reason. The currency defaults to USD for a CUSIP, else the ISIN's issuer
   country; an `XS` ISIN needs `--currency`.
5. **Positions.** Held BOND rows stay in `stocks` with their valuation, so
   Desk, the SPA, portfolio aggregates and the Rulebook see no change; the new
   `bonds` section classifies each by `con_id` as `bill` (zero coupon, at most
   397 days from issue to maturity, or a Treasury bill CUSIP when the issue
   date is missing), `bond`, or `unresolved` with a reason, with maturity,
   days to maturity, coupon, ISIN/CUSIP and currency. `canary positions`
   prints them under *Bills & bonds* and keeps unclassified rows in the stock
   table. Contract details are read by contract id, cached a day (failures
   retried after ten minutes), and a cold read waits at most 1.5 s before the
   lookup finishes detached. `positionWireSecType` keeps BOND a bond, so any
   proposal built on a held bond names BOND and preview refuses it
   (`unsupported_security_type`); none is generated today.
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
   BOND line in the currency (for USD, maturing on the list's date) and the
   quote must carry a price. The first confirmed bill is the row's `bill`
   (ISIN/CUSIP, contract id, maturity, days, price per 100 of face and its
   source, the quote, min size, assumed unit and convention); the contract is
   the bill's contract id. A quote that is not a live bid or ask keeps the row
   and adds `fresh_bill_quote_required`. Nothing confirmed reads
   `instrument_unresolved` with evidence per candidate; no row exists.
9. **Row identity.** The row key still uses the planned instrument, so it is
   stable when the EUR bill alternates between issuers; `cash_sweep.instrument`
   names the resolved bill's instrument, which stays inside the O1 exception.
   The quantity stays face value in whole units; at a price at or below par
   the cost never exceeds the free cash it was planned against.
10. **Not built.** The ETF fallback (no ETF resolution; the completed-search
    input stays empty), the mode default, and everything under *Order path*.

Instrument conventions (A5), explicit constants in
`internal/daemon/cash_sweep_instruments.go`, each an assumption to verify; no
order uses them yet:

| Instrument | Quantity unit | Face per unit | Price |
|---|---|---|---|
| `us_tbill` | `face_1000` | 1,000 USD | per 100 of face |
| `de_bubill`, `fr_btf` | `face_1` | 1 EUR | per 100 of face |
| `uk_tbill` | `face_1` | 1 GBP | per 100 of face |
| `ca_tbill` | `face_1` | 1 CAD | per 100 of face |
| `etf` | `shares` | — | per share |

### Post-install proof (read-only)

Run by the owner after install, on the live session, with no order. Record
redacted evidence here (identifiers of public government bills are fine;
never an account id, a balance or an order reference).

1. USD bill: pick one CUSIP from TreasuryDirect's list maturing 28–91 days
   out; `canary market --symbol <CUSIP> --type BOND` and again with `--json`.
   Expect `resolved: true`, one line, `class: bill`, the list's maturity,
   `min_size`/`size_increment`, and a quote with bid/ask per 100 of face and
   yields in percent (A7), `fresh: true` during the session (a quote without
   the gateway's feed-type notice reads stale; record which). Note whether
   the quantity unit matches `face_1000` (A5).
2. EUR bill: pick one German Bubill ISIN (`DE…`) maturing 28–182 days out;
   `canary market --symbol <ISIN> --type BOND` (currency inferred EUR) and
   `--json`. Same expectations; note whether one unit is 1 EUR of face.
3. Positions: `canary positions` and `canary positions --json`. Every held
   BOND row appears in `bonds` with its `con_id`, class, maturity and
   currency (none `unresolved` after a second read), and still in `stocks`.
4. SettledCash tag: `canary account --json` shows `settled_cash_ccy` on each
   `currency_exposure` row and on `base_currency_ledger`; with the sweep
   enabled, `canary proposals list --json` shows
   `cash_sweep.currencies[].settled_cash_source: "broker"`. If the tag is
   absent, A4 is false and the journal fallback stays; record that.
5. Freshness: outside a bill's session the check reads `fresh: false` with a
   reason, and a sweep row carries `fresh_bill_quote_required`.

### Order path (separate task, owner-authorised)

What the order-path task must add before any sweep row can be previewed or
submitted:

1. BOND in `pkg/ibkr/place_order_proto.go` (LMT DAY; contract by contract id)
   and in proposal preview admission (`proposalSupportedSecType` refuses BOND
   today).
2. The quantity conversion from face value to the broker's order unit (A5 as
   proven), rounding to `min_size` and `size_increment`
   (`below_minimum_increment`), and a limit price per 100 of face from a fresh
   quote within the typed O1 exception's bound.
3. A sweep preview bound to the row's own terms (the rows stay out of the
   snapshot revision).
4. Valuing working bond buys in commitments and bond fills in the journal
   settlement (both read unknown today), and duplicate-order netting for
   redeem rows.
5. Bond session and trading-hours gates from the contract's hours.
6. Lifting `instrument_support_required` per instrument after the proof, and
   deciding whether `cash_sweep` joins the pre-authorised vocabulary
   (`automaticBucketFor`; P1 says the owner approves each order).
7. ETF resolution by contract id and the fallback's completed-search input.

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
| `internal/daemon/proposal_automatic.go` | Phase B: pre-authorised vocabulary, bond session |
| `pkg/ibkr/connection.go`, `place_order_proto.go` | Phase B: bond contract handler; BOND LMT DAY |

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
3. Post-install proof (above); then the order path as a separate,
   owner-authorised task (above).
4. Lift `instrument_support_required` per instrument after that task.

## Risks and open decisions (ranked)

| # | Item | Handling |
|---|---|---|
| O1 | First buying bucket; `close_reduce_only` carve-out | decided: typed exception limited to vocabulary, currency and free cash |
| R1 | Settled cash observed only if A4 holds | the ledger's SettledCash when sent, else the journal derivation (fills Canary never observed stay invisible there); `settlement_unknown` when both are missing |
| R2 | ETF margin (A2) lowers available funds | out of scope; moot for the governor (rule 3 becomes a premium budget in a parallel change) |
| O2 | Rung 1 under four weeks | decided: `min_maturity_days` 28 |
| O3 | EUR fallback symbol, exchange | decided: owner writes; `needs_your_number`; bills still plan |
| O4 | German tax (A3) | superseded by P1: advisory detail line and `tax_reviewed: false` |
| O5 | `max_order_notional` | decided: owner number; `needs_your_number` |
| O6 | `keep_cash` 5,000 earns nothing (A1) | decided: 5,000; owner reviews per currency |
| R3 | Bill minimums, spreads (A5) | order path: `below_minimum_increment`; limit orders only |
| R4 | TreasuryDirect unreachable | list served up to 48 h; then `universe_unavailable` for USD only |
| R5 | Issuer read from identifiers (A6) | only classifies held bills as equivalents; buys use only TreasuryDirect CUSIPs and the owner's ISINs |
| R6 | Quantity unit and price convention (A5, A7) | labels only until the post-install proof; the order path converts |
| O7 | USD balance as FX exposure | out of scope; the sweep never converts |
