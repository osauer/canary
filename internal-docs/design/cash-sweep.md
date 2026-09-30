# Cash sweep (idle cash into same-currency bills)

Updated: 2026-09-30 10:03 CEST
Status: Phase A implemented (shadow) on branch feat/cash-sweep. Phase B ships
only after a paper-account proof.

This record follows `.agents/docs/risk-policy-contract.md`. It records the
owner's decisions of 2026-09-30 09:10 CEST (S1–S6) and the reviewer's
decisions on the open items of 2026-09-30 09:30 CEST (O1–O7), and creates
none. Items marked **(A)** are broker assumptions to verify, never facts.

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
| O4 | Accepted: `tax_reviewed_at` gates active mode (`tax_review_required`). | `cashSweepRow` |
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
- Cash: `cash` is the lower of trade-date and settled cash; `committed` is
  working BUY orders plus authorised (armed, held or sending) queued orders;
  `free = cash − committed − keep_cash`.
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
  rows are ordinary proposals under every gate, freeze included. Active without
  `tax_reviewed_at` adds `tax_review_required`. Phase A adds
  `instrument_support_required` to every row. Every row sets `NeverSkipVeto`.
- Unknown posture, per currency, generating nothing: `cash_unavailable`,
  `settlement_unknown` (journal gap in the settlement window),
  `equivalents_unclassified`, `needs_your_number`. A missing bill line is
  inferred only from a completed contract search, so Phase A never falls back
  to the ETF. Nil is unavailable, never zero.
- Assumptions (S5): (A1) IBKR pays about benchmark − 0.5% above 10,000 per
  currency, nothing below; (A2) T-bill initial margin about 1%, ETF 25–50%;
  (A3) German tax measures bill rolls in EUR, FX component taxable; (A4)
  `$LEDGER` CashBalance is trade-date, with no per-currency settled tag; (A5)
  bill quantity unit, minimum, session and T+1 settlement.

## Authority And Evidence

| Concept | Authoritative source | Typed field/contract | Freshness/finality | Fallback or blocker |
|---|---|---|---|---|
| Numbers, instruments, mode | protection policy file | `protectionCashSweepPolicy`, `[buckets.cash_sweep.currency.<CCY>]` | hot reload, version bump | absent or disabled ⇒ silent |
| Cash per currency | `$LEDGER:ALL` CashBalance | `rpc.CurrencyExposure.CashCcy` + `CashObserved`; the base row in `AccountResult.BaseCurrencyLedger` | per account refresh (one-shot request only) | `cash_unavailable` |
| Settlement, commitments | order journal, broker open-order inventory, queued authorisations | `cashSweepSettlement`, `cashSweepCommitments` | per refresh | `settlement_unknown` |
| Held equivalents | positions view | BOND rows (Phase B classifier), ETF by ConID | per refresh, `Stale` honoured | `equivalents_unclassified` |
| Bill lines, quotes | IBKR BOND contract details, quotes (Phase B) | new `pkg/ibkr` bond contract | per cycle | `instrument_support_required`, `fresh_bill_quote_required` |
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
- Shadow through Phase A and the paper proof; active after O4.

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
- Phase B: redacted paper artifact per currency: resolve, quote, BUY, SELL,
  maturity.

Phase A tests: `internal/daemon/protection_policy_cash_sweep_test.go`,
`internal/daemon/proposal_cash_sweep_test.go` (including a 2,000-book random
invariant run), `internal/cli/proposals_cash_sweep_test.go`,
`internal/risk/rulebook_cash_like_test.go`.

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

Phase B (after paper proof):

1. Read-only paper probe: BOND contract details by ISIN/CUSIP, quotes.
2. `msgBondContractData`; bond quotes; position classification.
3. BOND orders in the proto and proposal gates (`unsupported_security_type`
   still refuses BOND); duplicate-order netting for redeem rows.
4. Paper proof per currency; lift `instrument_support_required` per instrument.
5. `cash_sweep` in the pre-authorised vocabulary and `automaticBucketFor`.

## Risks and open decisions (ranked)

| # | Item | Handling |
|---|---|---|
| O1 | First buying bucket; `close_reduce_only` carve-out | decided: typed exception limited to vocabulary, currency and free cash |
| R1 | Settled cash derived, not observed (A4) | lower of trade-date and settled; `settlement_unknown` fails closed; fills Canary never observed stay invisible |
| R2 | ETF margin (A2) lowers available funds | out of scope; moot for the governor (rule 3 becomes a premium budget in a parallel change) |
| O2 | Rung 1 under four weeks | decided: `min_maturity_days` 28 |
| O3 | EUR fallback symbol, exchange | decided: owner writes; `needs_your_number`; bills still plan |
| O4 | German tax (A3) | decided: `tax_reviewed_at` before active |
| O5 | `max_order_notional` | decided: owner number; `needs_your_number` |
| O6 | `keep_cash` 5,000 earns nothing (A1) | decided: 5,000; owner reviews per currency |
| R3 | Bill minimums, spreads (A5) | Phase B: `below_minimum_increment`; limit orders only |
| O7 | USD balance as FX exposure | out of scope; the sweep never converts |
