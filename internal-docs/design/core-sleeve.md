# Core sleeve (the mandate's rule-based index sleeve)

Written: 2026-10-14 16:50 CEST. Revised 2026-10-14 18:30 CEST: the owner's
answers to D1 to D4, the independent design review folded in (section
"Design review"), and phase 1 built. Revised 2026-10-18: allocations replace D1
(§3, §7). Revised 2026-10-18 CEST: the owner's decision of 08:06 CEST on
the core's venue and on D5 (sections 1, 3 and 7), built on Canary branch
`core-venue`. Revised 2026-10-18 18:40 CEST: `internal/core` is the
strategy evaluator's arithmetic and the core its first instance
(`strategy-evaluator.md`); the core's verdict, goldens and parity fixture
are unchanged. Revised 2026-10-18 18:55 CEST: phase 2 built on Canary
branch `core-rows` (section "Phase 2 as built"). Revised 2026-10-19: phase
3's daemon half built on branch `phase-b` (section "Phase 3 as built").
Status: phases 1 and 2 built; phase 3's daemon half built. Phase 1 serves
the target read-only; phase 2 serves each month's decision as one proposal
row the owner confirms on a device while `[mandate.core]` is live (option
A); phase 3 lets Desk's controller send that row under the owner's standing
Fully armed mandate (option B) once Desk presents it (Desk's half is not
built). The live policy runs the core live since 21:18 CEST (v14).

Sources read, read-only: Canary main `761a1c31` (mandate increment 1, Desk
authority and execution groundwork), Canary `integration-2` `e4df5eeb`
(forced reduction and flatten to hedges, not yet on main), Desk `77c3966`
(`docs/automatic-trading.md`, `docs/mandate.md`, `automatic_controller.go`),
lab `1ef744c` (`specs/s0_core_sleeve.yaml`, `lab/strategies.py`,
`reports/calibration.md`) and the alpha review of 2026-10-14 in Desk's
private output. This record follows `.agents/docs/risk-policy-contract.md`:
it records owner decisions and proposes numbers the owner writes; it
creates no policy.

## Owner decisions this design implements

From the owner's chat of 2026-10-14; the mandate as recorded in
`mandate-risk-budget.md`, the rest as relayed in this design request:

- Mandate: "max loss 30% of equity minus a tax reserve, vol target 15%,
  sleeves owner 7 / core 6 / overlay 4 (% vol of the risk base), forced
  reduce-only to budget with a 30-minute veto; at the max loss flatten to
  hedges" (the last part: "Yes, flatten to hedges", ~16:10 CEST).
- Core instrument: "IBCF (iShares S&P 500 EUR Hedged UCITS ETF Acc, Xetra,
  ISIN IE00B3ZW0K18)". FX hedge rule: "keep USD exposure under 20% of NAV".
- ~16:10 CEST: "Start with the core sleeve": design first, independent
  safety review, then code; buying stays off until the owner arms Fully
  armed on their device.
- 06:26 CEST: "The system should run by itself within risk boundaries."
- 2026-10-18 08:06 CEST, verbatim: "Accept all three: give the machine its
  own seat (replacing D1 and largest-contributor-first), put s1 into its
  six-month shadow test, and go for the core venue fix with the core fund
  out of protection's automatic sales." The last clause is recorded here:
  the venue fix (the policy names only the fund and the currency; IBKR's
  contract record names the listing, its venue and its calendar) and D5 as
  proposed (section 7). The first clause replaces D1 (section 7 and
  `mandate-risk-budget.md`, "Risk allocations"); the second belongs to the lab.

Answers of 2026-10-14 (relayed with the build request, verbatim where
quoted):

- **D1** "No, whole book only": the sleeve shares do not bind each sleeve;
  the whole-book mandate gate decides. The core gets the room the owner's
  sleeve leaves, and the forced reduction keeps "largest risk contributor
  first". Replaced 2026-10-18 08:06 CEST by allocations (§7).
- **D2** "Yes, for the core only": if IBKR refuses Adaptive for IBCF, core
  orders may use a plain DAY limit at the mid, never crossing it. The
  2026-10-09 "no fallback" rule stays for every other automatic order.
- **D3** "Report now, futures hedge later": excess USD cash is converted
  (the cash rule in section 4); USD securities above 20% of NAV are
  reported; micro EUR/USD futures are the hedge later; never a USD loan.
- **D4** "12% for broad index funds": broad index funds take a 12% gap
  shock; single names keep 25%.
- Earlier the same day: at the maximum loss the book flattens to hedges;
  the core instrument is IBCF (Xetra, ISIN IE00B3ZW0K18).

Earlier decisions that bind this design: Disarmed, Protection and Fully
armed with device-confirmed arming; AUTO alone never creates authority;
additions use a midpoint limit, IBKR Adaptive and DAY, with no plain-limit
or market fallback (Desk `automatic-trading.md`, 2026-10-09); each drawdown
brake engagement holds a full mandate at protection until it is confirmed
again on a device (2026-10-10 07:51 CEST). Target-allocation strategies
were deferred on 2026-10-09; the 16:10 instruction starts the first one,
for one instrument.

## Recommendation in brief

1. The core holds IBCF while IBCF's month-end close is above its 10-month
   average, sized so its own volatility is at most its 6% risk allocation of the
   budget base × room and its value at most 40% of the budget base ×
   room. It rebalances monthly on the calendar of the exchange IBKR lists
   it on (London today, section 1) with a 10% no-trade band. Trend off
   sells to zero, and the cash returns to the sweep.
2. Canary computes the target as a daemon-owned verdict (`core.target`,
   parity-tested against the lab's Python) and serves each order as one
   proposal row (bucket `core_rebalance`). Desk decides nothing and sizes
   nothing.
3. Ship option A now: the owner confirms that row once on the device
   through the existing prepared-proposal path, one tap in a month that
   trades (about three a year). Ship option B next: the Desk controller
   names the same row under the Fully armed mandate. One row, two
   authorisations, as `desk-execution.md` already separates them.
4. The whole-book mandate gate stays binding for every core buy, and so does
   the core's own allocation (2026-10-18, §3); a buy is sized to leave the
   book at most at 0.95 of the budget and the core's gap loss at most at 0.95
   of its cap, the forced reduction's own target, so a core buy can never set
   up a forced sale by itself.
5. This is a beta allocation the owner chose, with a drawdown filter. It
   claims no alpha: the lab supplies the parity fixture and the kill rule,
   nothing else, and no Sharpe, IC or e-value is a test of it.
6. The owner answered D1 to D4 on 2026-10-14 (above). Phase 1 is built:
   the target is served read-only (`canary core`), with the two mandate
   fixes the review found necessary (the index gap and the gap-cap check
   on additions).

## Design review (2026-10-14) and how it is folded in

The independent review found one blocker and eight serious items. Each is
answered here; "built" means phase 1 carries it, with a test.

| # | Finding | Resolution | Where |
|---|---|---|---|
| B1 | At its target (40% of the budget base) the core's one-name 25% gap loses 10% of the base, at the 9.99% gap cap: the core breaches it, the forced reduction sells, the next month buys back: ping-pong | D4: broad index funds take a 12% gap (4.8% of the base, under half the cap). The incremental additions gate refuses an addition that creates or deepens a gap-cap breach (it checked volatility only). The core's sizing lands inside every gap cap with margin: its gap at most 0.95 of the cap. The core is not ready until `index_gap_shock_pct` is written: at 25% its target would sit at the cap's margin and a rise of about 5% would breach it | Built: `index_gap_shock_pct`, `mandate.BroadIndexInstruments`, `mandateGapBreachAdded`, `coreHeadroomFits`, `CoreUnapprovedKeys` |
| S1 | Honesty | This is a beta allocation the owner chose with a drawdown filter, no alpha claim. The lab supplies parity and the kill rule only; s0's Sharpe hypothesis is dropped as a success test; the kill rule's reference is concrete (the new spec); Desk shows no IC, e-value or "established" for the core | Lab spec `2408d09e6189`; Desk in phase 2 |
| S2 | Lab accounting | The design's 1995-2015 turnover run (five configurations) is logged with `lab prior`; the 10% band was picked on that turnover; "S&P 500 only" is recorded as hindsight-prone; a new s0 version matching the rule is registered | Built in the lab: registry seq 29-31 |
| S3 | Costs beyond commission and spread | Whipsaw, tax on realised gains, Vorabpauschale and hedge carry, each with a number or "unknown" | "Turnover and cost" below |
| S4 | The typed buy exception | Bound like `cashSweepOpenException`: to the `core_rebalance` row, its sized quantity and a cost at most the funded EUR; netted against every working IBCF order, the owner's included; 0.95 and the gap headroom re-checked at submit; held on `book_changed` | Built in phase 2: `coreOpenException` (contract, currency, the sized quantity, never more than Q − H, a limit within 10% of P_m); the netting at generation and again at preview through the fresh refresh; the headroom re-checked by the refresh that re-sizes the row and by the mandate gate at preview and submit; funding by the stock Add admission the preview runs on a stock buy while the policy carries `[position_add]` with `admission_contract = "stock-entry-v1"` |
| S5 | IBCF closes at 17:30 CET, the US at 22:00: on its own closes the mandate understates its correlation with US names | The mandate moves IBCF with SPY's local-currency returns, no FX leg (EUR-hedged class); the ±8% price cross-check uses SPY's move over the two US sessions spanning IBCF's day. Since 2026-10-18 every EUR line IBKR lists under the ISIN matches by contract, whatever its symbol (the London line closes at the same instant) | Built: `mandate.ReturnsProxies`, `Input.LineISINs`, `core.Guard` |
| S6 | Sales | A trend-off remainder does not wait a month: it continues in the next sessions until sold. A data hold stops buys and sends any sale the rule wants to the owner | Built in phase 2: a sale to zero is served past the window until the holding is zero; under a hold, the sale to zero the frozen month wants is served with the hold's reasons on it |
| S7 | Series | Requested on the listing's primary exchange, not SMART, keyed by contract id; an unheld core series never makes the book's verdict stale; freshness on that exchange's calendar; a fixed input window. Until 2026-10-18 the venue was the policy's IBIS, on which IBKR does not list the contract it pinned | Built: key `core:con:<id>`, `coreSeriesNeed`, `mandateSeriesMarket`, `core.WindowCloses` |
| S8 | Calendar | The order window skips sessions the US is closed (wide spreads on European S&P 500 fund lines); the send time is clamped before an early close, and on London's early closes, which end before the US open, the session is skipped instead | Built in phase 2 (`coreWindow`): the window skips US-closed sessions and London's early closes. The due time is built since 2026-10-18 (`core_due.go`): the row reads `opening_window` until fifteen minutes after the US cash open of its session, its default send that time; preview, prepare and submit hold it before then, and the standing-mandate controller sends it at its first ready read after it |

Notes the review raised, with their rules:

- **Adopting an existing IBCF holding.** Every share of the pinned
  contract counts as the core's holding, whoever bought it: the instrument
  is reserved to the core. An existing holding is adopted at the first
  decision; above the target beyond the band, the rule sells the excess
  (under A, the owner confirms that row).
- **The owner's hand IBCF orders.** They are the owner's call and change the
  holding the next decision corrects. Since 2026-10-18 a hand buy is the
  core's addition, judged against the core's allocation. While any IBCF order
  works, the owner's included, no core row is served, and every core row is
  netted against working IBCF orders (phase 2).
- **Option B's armed terms** name the rule, the spec hash, the scale and
  core_vol_pct, so a policy edit cannot raise automatic buying under a
  standing arming. Phase 1 already freezes each month's target as a
  ceiling: an edit within a month can only lower it.
- **Protection's scope would sell the core.** Protection proposals cover
  every held line, IBCF included; a protection sale lowers the holding and
  the next decision may buy back under the headroom. Decided 2026-10-18
  (D5, section 7): the core's contract leaves protection's automatic sales;
  the forced reduction still covers it. Built.
- **An Auto lock on IBCF stops core sales too**, not only buys: it locks
  the instrument. A trend-off sale waits while it stands (corrected in
  section 3).
- **Property test:** the target is non-increasing in drawdown (and in
  volatility): built, `TestTargetNonIncreasingInDrawdownAndVolatility`.

## 1. The rule

### Instrument and series

- Instrument: the iShares S&P 500 EUR Hedged UCITS ETF (Acc), ISIN
  IE00B3ZW0K18, in EUR, security type STK, accumulating; IBCF is the
  owner's name for it. The policy names the fund and the currency; IBKR's
  contract record names the rest (owner decision 2026-10-18 08:06 CEST).
  On 2026-10-16 IBKR listed one line under the ISIN: the London EUR line
  (trading class EUET, primary exchange `LSEETF`), under another symbol;
  no Xetra line (`canary core resolve`).
- Venue and calendar from IBKR's record (built, `core_resolve.go`). On the
  background lane, every six hours, the daemon asks IBKR for every line
  under the ISIN in the policy's currency and uses the one it lists. It
  reads the series for that contract id with IBKR's symbol on the line's
  primary exchange, and judges sessions, freshness and the decision day on
  that exchange's calendar (`marketcal` `uk_lse` for London, `de_xetra`
  for Xetra; a primary exchange without an embedded calendar holds). It
  holds, naming every candidate, when IBKR lists more than one line in the
  currency, and holds when the line differs from the one the core last
  used (kept in the database), until the owner chooses by writing
  `con_id`. A written `con_id` must be one of the ISIN's lines; a written
  `exchange` must be a venue IBKR lists the line on (the primary exchange
  or a valid exchange), else the core holds and says so, so a policy that
  still carries both keeps loading. A lookup that gets no answer keeps the
  line found before and is asked again after 15 minutes. `canary core`
  shows the contract, its venue and its calendar.
- Signal series: the line's own daily closes from IBKR historical data,
  kept in the mandate's decision-grade history store (`mandate_history.go`)
  under its own key `core:con:<contract id>`, stamped with the calendar it
  is judged on, requested on the primary exchange by contract id (never
  SMART), whether held or not, and judged fresh on that exchange's
  calendar (review S7). A series kept for another calendar is read again.
  A read IBKR refuses is named in the hold with IBKR's answer and its time
  until a read succeeds. The book's verdict never reads it, so it can
  never make that verdict stale: the mandate measures a held line of the
  fund on SPY's returns (review S5, section 3), matched by contract. A
  series whose echoed contract id is not the core's holds. The fund
  accumulates, so its close is a total-return series of the EUR-hedged
  S&P 500 with the hedge carry and fees inside: the rule judges what the
  sleeve earns.
- Why the calendar follows the listing: London and Xetra differ. In 2026
  London was closed on 4 May, 25 May and 31 August, when Xetra traded, and
  open on 1 May, when Xetra was closed. London closes judged on Xetra's
  calendar would hold every month whose 253-session window holds such a
  day ("no close on 2026-05-04", "a bar on 2026-05-01"): every month
  (`TestCoreTargetOnTheLondonLine`).
- No live fallback series. IBCF has traded since 2010, more than every
  live input needs (the store keeps 320 closes; the rule needs 253). A
  missing, stale or suspect IBCF series holds the month. The rule never
  switches to the index, because a silent series switch is a data error
  of its own. The S&P 500 serves elsewhere: SPY, already in the store as
  the index proxy, is the live cross-check for IBCF price errors, and SPY
  or the S&P 500 total return index is the lab's proxy before 2010.

### Decision date

The last session of each month on the line's exchange calendar: London
SETS (`uk_lse`) for the London line, Xetra (`de_xetra`) for a Xetra line,
both official and covered 2026-01-01 to 2028-12-31. London's last session
can differ from Xetra's: 28 August 2026 (31 August is a bank holiday);
31 December is an early close at 12:30 London and still the decision day.
The target is computed once that session's close is in the store, from
half an hour after its scheduled close (17:00 London, 18:00 Berlin, on a
regular London or Xetra day; 13:00 London on 31 December), on closes up
to and including it, and is then frozen for the month with the digest of
its window (`core_target_drift` holds a later reading that differs). A
running month is never decided. The history store keeps the same half
hour: it neither asks IBKR for a session's daily bar nor keeps one read
earlier, because a bar read while the closing auction runs carries the
last continuous print and the next read replaces it (on 2026-10-30 a read
40 seconds after London's close kept 155.62 where the close was 155.64,
and the November month held with `core_target_drift`; fixed 2026-11-16).

Fixed input window (review S7): the last 253 calendar sessions to the
decision day, 252 daily returns, for both volatilities and the month-ends.
Where the embedded calendar covers a day, it decides which days are
sessions; before its coverage the series' own dates stand. A session
without a close holds the month; it is never filled, and a bar on a day
the exchange was closed holds too. The store re-reads the whole series each
session, so a broker gap repairs itself on the next read; a persistent one
holds every month whose window contains it.

### Trend

On when P_m > SMA10. P_m is the decision close; SMA10 is the mean of the
last ten month-end closes including P_m (the lab's `core_sleeve` with
`trend = "sma10m"`, strict inequality). Not live before ten month-ends.

The 200-day average is not used. It is evaluated daily, so it flips more
often and off the monthly cadence; s0 pre-registered the 10-month rule,
and choosing between the two now would be a search.

### Volatility and size

- σ_E: the EWMA of IBCF's daily simple returns over the window, span 63
  (α = 2/64), weights (1 − α)^(n−1−i), the variance around the weighted
  mean with the bias correction (Σw)² / ((Σw)² − Σw²), × √252. This is
  the lab's `_scale_to_vol` machinery (pandas `ewm(span=63,
  adjust=True)`, `bias=False`), on the fixed window.
- σ_252: the sample standard deviation of the window's 252 daily simple
  returns × √252, the mandate's window length, so the sleeve is never over
  its risk allocation by that measure.
- σ = max(σ_E, σ_252, 15%).
- Sleeve budget S = core_vol_pct / 100 × budget_base × room, from the
  mandate verdict's capital at the decision (`budget_base`, `room`;
  `capital.known` required). Equivalently core_vol_pct / vol_target_pct ×
  `budget_annual_sd_base`. With the owner's numbers S = 0.06 × budget
  base × room: the request's "6% × risk-base high-water mark × room".
- Target notional N = S / σ when the trend is on, 0 when it is off.
- Cap: N at most `max_value_pct` / 100 × budget base × room (owner's 40).
- Target shares Q = floor(N × scale_pct / 100 / P_m): whole shares,
  rounded down. `scale_pct` is the commissioning ramp (25, 50, 100); 0
  suspends the core (the kill rule's lever).

Caps. Each follows from the formula or an existing gate; none is a new
number:

- The 15% floor is s0's no-leverage gross cap restated. With one
  instrument and sleeve capital K = S / 0.15, s0's w = min(0.15 / σ_E, 1)
  gives N = w × K = S / max(σ_E, 15%). Hence N ≤ 0.4 × budget base × room
  with the owner's 6.
- Ex ante N × σ_E ≤ S and N × σ_252 ≤ S: the sleeve's own volatility is
  within its risk allocation on both estimators at every rebalance.
- Funding: a buy is funded as an Add of that exact quantity would be
  (`position-add.md`): settled EUR after the account reserve, commitments
  and a fee bound when the row is sized, the exact WhatIf at preview,
  never a loan or a conversion, and `no_buy_while_borrowed` applies. It
  waits where an Add waits (an unacknowledged buy, uncertain broker
  inventory, a pending non-stock order).
- Mandate headroom (section 3), and the target recomputed with the
  current verdict's capital: the frozen target is a ceiling for buys,
  never a floor. Built: the served target is the lower of the frozen one
  and today's.

### Rebalance and no-trade band

The order for a session is D = Q − H, where H is the IBCF shares the
broker's current position projection confirms. A row is served only when:

- the trend is off and the sleeve holds shares (sell all), or the sleeve
  holds none and Q is positive (first buy), or |D| > band_pct / 100 × Q
  (proposed band 10%);
- |D| × mid ≥ `min_order_base` (proposed 2,500 EUR, so IBKR's per-order
  minimum commission stays near 0.1% of the order; an assumption from
  IBKR's published Xetra rates, which London's EUR line need not share,
  to calibrate from fills in W1), except the
  sell-all, which always sells every share;
- no core order is working, unacknowledged or of unknown outcome.

A sale is H − Q, never more than H; the core never shorts. A buy is capped
as above and waits when what is left is below the minimum.

### Window and order terms

Re-checked for the London line on 2026-10-18 (the owner's venue
decision): each rule below was written for Xetra, and each holds or is
restated for London, on the embedded calendars for 2026 to 2028.

- Window: the first five sessions on the line's exchange calendar after
  the decision day that the US also trades. At most
  one core order per session, with the episode
  `core:<account>:<mode>:<YYYY-MM>:<session date>`. A remainder after a
  partial fill or an expired DAY order becomes the next session's D,
  computed again from confirmed holdings; the frozen target is never
  topped up. What is left of a buy when the window ends waits for next
  month; what is left of a sale to zero does not wait (review S6): it
  continues in the next sessions until the holding is zero.
- US-closed sessions are skipped (review S8): on a US holiday European
  S&P 500 fund lines quote wide, London's as Xetra's, since their market
  makers hedge in US instruments, so no core order is sent that day, and
  the window counts only sessions both markets trade. Holds for London:
  in 2026 to 2028 it skips Independence Day (3 July 2026, 5 July 2027,
  4 July 2028) and Labor Day (7 September 2026, 6 September 2027,
  4 September 2028) in the July and September windows. London's own bank
  holidays are not sessions and never count.
- Due time: 15:45 Europe/Berlin (`send_after`), after the US cash open,
  when European S&P 500 ETF quotes are tightest. Holds for London: it is
  14:45 London all year (the UK and Germany change clocks on the same
  days), a quarter of an hour after the US open (13:30 or 14:30 London,
  the earlier in the weeks the US is on summer time and Europe is not)
  and 1 hour 45 minutes before London's 16:30 close. The row is visible
  from the open. Five sessions also give a bill redemption time to settle
  before a buy that needs its cash. Enforced since 2026-10-18 (`core_due.go`,
  at the integrator's ask after the quality loop found readiness reading
  `ready` five minutes after the London open): the due time is computed from
  the US calendar for the row's session (`coreRowDueAt`), not written as a
  key; before it the served row reads `opening_window` with the due time as
  `default_send_at` (a closed London dates its default send there too, and a
  row for a later session than today's reads the opening window until that
  session's due time), the preview, the prepared submit and the submit hold
  the row with `core_before_due_time`, the standing-mandate controller
  refuses it before journaling and sends at its first ready read after it,
  and `canary proposals` prints "opening window · default send 15:45 CET".
  The first behavioural test of the core's buy through the production write
  path (`TestDeskExecutionSubmitSendsTheCoreBuyUnderItsTypedException`,
  trading build, 2026-10-18 23:40 CEST) found the quote normaliser refusing
  the London line's row outright ("unsupported quote market uk"): the row
  names its market `uk` (`coreContractMarket`) and `normaliseStockQuoteContract`
  knew `us` and `de` only, so no preview, prepare or send of the core's row
  could have run on 2 November. It accepts `uk`, `london` and `lse` now
  (SMART with the line's venue; a bare London request defaults to LSE in
  GBP).
- Early close (review S8): on Xetra the send time is clamped to an hour
  before an early close. London's early closes end at 12:30 London (24
  and 31 December 2026 and 2027, 22 and 29 December 2028), before the US
  open, so an hour before is a time when the 15:45 reasoning fails: on
  the London line an early-close session is skipped like a US-closed one,
  not clamped. None falls inside a window in 2026 to 2028 (a window is the
  first sessions of a month); 31 December 2026 and 2027 and 29 December
  2028 are decision days, decided from 13:00 London.
- Price guard (built, `core.Guard`): a daily move beyond ±8% needs SPY's
  move over the two US sessions spanning the line's day, at half the size
  and the same sign. Holds for London unchanged: London's 16:30 close is
  17:30 CET, the same instant as Xetra's and four and a half hours before
  the US close, so the span (from the US close before the line's previous
  session to the first US close on or after its own day) is the same; the
  guard reads the line's own session dates.
- Terms: LMT, DAY, regular session; the limit is the live mid snapped to
  the tick grid without crossing the mid against the owner (a buy rounds
  down, a sale up); IBKR Adaptive at the owner's priority where Canary
  admits it; SMART with the line's primary exchange (`LSEETF` for the
  London line), as IBKR's record names it. No market order, no
  repricing, no chasing. A limit more than 10% from P_m holds the row as a
  probable data or contract error.
- Canary's Adaptive shape is US-only today: `reduceAdaptiveSupported` requires
  USD, and `validateAdaptiveOrder` in `pkg/ibkr` requires SMART. Whether IBKR
  accepts Adaptive for the fund's line routed SMART is assumption A1, to
  verify by WhatIf in paper. A refusal is decided (D2, "Yes, for the core
  only"): core orders then use a plain DAY limit at the mid, tick-snapped,
  never crossing it, never repriced; every other automatic order keeps the
  2026-10-09 "no fallback" rule.

### Trend off

Sell to zero in the window. The proceeds are ordinary EUR cash under the
cash sweep's own band (bills while the sweep is on), which is s0's
"weight to T-bills". When the trend turns on again, the frozen buy amount
becomes the sweep's planned need through the typed seam
`cashSweepPlannedNeeds` (always 0 today), so the sweep stops investing
that cash and its redemption rule frees it; the buy waits for settled EUR
inside the window.

### Turnover and cost

Estimated with the lab's own `core_sleeve` code on SPY as the proxy,
January 1995 to December 2015, before s0's holdout. Only turnover was
computed: no return, Sharpe or drawdown. One-way turnover is in units of
sleeve capital K = S / 0.15:

| Sizing | Band | Trades a year | Turnover a year | Cost a year at 10 bp a side |
|---|---|---:|---:|---:|
| s0 (σ_E, 15% floor) | none | 9.6 | 1.19 K | 12 bp of K |
| s0 | 10% | 3.0 | 1.09 K | 11 bp of K |
| this rule (max of σ_E, σ_252, 15%) | none | 9.6 | 1.06 K | 11 bp of K |
| this rule | 10% | 2.9 | 0.96 K | 10 bp of K |
| this rule | 20% | 1.8 | 0.89 K | 9 bp of K |

The trend flipped about once a year (0.5 round trips) and was on at 76%
of month-ends; most turnover is those flips. At the proposed 10% band,
expect about three orders and about 10 bp of K a year, at most 4 bp of
the budget base, at the lab's placeholder UCITS costs (5 bp commission
and 5 bp half spread). The 10% band was picked on this turnover alone
(review S2): about three orders a year against 9.6 without a band; no
return was looked at. The run is logged as prior exploration of the
`equity_timing` family (five configurations, `lab prior`, 2026-10-14).

Costs beyond commission and spread (review S3), each a number or
"unknown":

| Cost | Size | Note |
|---|---|---|
| Whipsaw | Unknown | The turnover run computed no returns. A flip sells after a fall and buys after a rise; that loss is the price of the filter, and the kill rule's (b) watches it against buy-and-hold at the same notional |
| Tax on realised gains | If German-taxed: 26.375% (plus church tax where due) on 70% of the gain, the 30% partial exemption of an equity fund, so about 18.5% of each realised gain | Every trend-off sale realises the gain since purchase and ends its deferral; the size depends on the gain, unknown until the first sale. The tax reserve carries it. To confirm with the owner's tax adviser |
| Vorabpauschale | If German-taxed: the January value × the base rate × 0.7, taxed after the same exemption: about base rate × 0.13 of the value a year (2025's base rate of 2.53% gives about 0.33%) | IBCF accumulates, so the advance lump sum applies to January holdings, pro rata in the year of purchase, at most the year's gain, credited against the sale's gain later: a timing cost. To confirm with the owner's tax adviser |
| Hedge carry | Unknown here; roughly the USD-EUR one-month rate gap a year while USD rates are higher | Inside IBCF's price, so the rule's own series and the sleeve's returns carry it; not in the SPY proxy the lab uses before 2010 |
| Fund costs | TER inside the price | Not a separate charge |

### Relation to s0 and pre-registration

The rule is s0 (`s0_core_sleeve`, hash `7c4dd5b5…`, R0, never run) with
five changes:

1. One instrument, IBCF, instead of SPY, QQQ and EFA: the owner's choice;
   s0 left the real instrument mapping to the owner.
2. The volatility target is the mandate's risk allocation S instead of a
   free-standing 15%; s0's 15% target and gross cap 1.0 become the 15%
   floor.
3. σ_252 joins σ_E, so the sleeve is within its risk allocation on the mandate's
   own window too. Cost: slower re-risking for up to a year after a
   volatility spike.
4. A 10% no-trade band and a minimum order.
5. Orders go out in the next session at a midpoint limit, not at the next
   close.

Registered (2026-10-14 18:05 CEST) as s0's new version
`2408d09e618904c84219cacf6bc0f599e1a00ee3c4b04b0effbcc4521f45a441`, with
variants v1 (this rule), v2 (s0 sizing: σ_E and the floor only) and v3
(no trend filter); the earlier version `7c4dd5b5…` is superseded before
any run (review S1, S2). The new version states the honesty plainly: a
beta allocation the owner chose with a drawdown filter, no alpha claim,
no Sharpe hypothesis and no R1 look; the lab supplies the parity fixture
and the kill rule only. It records "S&P 500 only" as hindsight-prone and
the band as picked on turnover. Its kill rule names its references
concretely: (a) drawdown in units of K beyond 1.5 times v1's design-period
maximum, a number the first design run writes into the registry before R2
(until then it cannot fire and the mandate's brake and maximum loss bind
alone); (b) a rolling 24-month net Sharpe below 0 while IBCF held
throughout at the sleeve's mean notional, on its own closes, with no
costs, is above 0.5; (c) realised costs above twice the cost model. Design
runs stay blocked until the lab harness carries EUR-priced instruments (it
refuses non-USD datasets today) and the no-trade band. The compiled rule
`core-ibcf-v1` carries the spec hash it implements, the policy names the
rule, the verdict prints both, and a mismatch holds (built).

Parity (built): one synthetic fixture (880 sessions of generated closes
on a Xetra-shaped calendar, with a calm stretch where the floor binds, a
crash and a volatility spike where the EWMA and then the 252-day window
bind, a holiday month-end and a missing bar) and its expected values per
decision (status, month-ends, trend, SMA10, σ_E, σ_252, σ, binding, and
N and Q at scales 25, 50 and 100 for a fixed synthetic budget), produced
by the lab (`lab core-fixture`, `lab/core_rule.py`) and committed byte for
byte to both repositories (`internal/core/testdata/`). Canary's Go test
matches the status, trend, binding and Q exactly and every float to 1e-9
relative; the lab's test proves its builder still produces the fixture.
A change on either side fails a test; the fixture changes only with a new
rule version. No market data or account figure enters either
repository.

Not reused: Canary's `target-band-plan-v1` (`portfolio-plan.md`). Its
targets are static owner numbers, it never sells, and it grants advisory
selection only.

## 2. Where it lives

| Concern | Owner | How |
|---|---|---|
| Rule, monthly target, evidence | Canary verdict `core.target` (built) | The strategy evaluator's arithmetic `internal/core`, the core its first instance (`strategy-evaluator.md`); a frozen monthly state document with its input digest; `canary core` and `canary_core`, read-only; listed in `TestOnlyTheDaemonEvaluates` |
| The exact order | Canary proposal engine, bucket `core_rebalance` | Sizes D; applies the band, minimum, funding and headroom; nets and merges with other rows; readiness on the line's exchange calendar |
| The decision to send | A: the owner on the device. B: the Desk controller under the Fully armed mandate | Desk never computes a size or a price |
| Execution | Canary's existing order path | Every gate, the order journal, the wire guard |
| Durable progress (B only) | Torok | The existing controller records |
| Evidence for the rule | The parity fixture and s0's kill rule (lab); scorecard sleeve `core` | The core's fills score as `core`, never as the owner's; reported, never IC- or e-value-gated, and Desk shows no IC, e-value or "established" for it (review S1) |

### The two options

| | A: monthly row, the owner confirms | B: Fully armed, automatic |
|---|---|---|
| Authority | One device confirmation of the exact previewed order | The standing mandate, device-confirmed once, and Canary's wire fence |
| Exists today | The prepared-proposal path (`trade.proposals.prepare`, device confirmation, `submit` with `prepared_ref`), every gate, Desk's proposal confirmation | Authority methods (Go client only), the execution journal and lookup, the Desk controller in tests |
| Missing | The verdict, the row and one typed buy exception | All of A and the phase 3 list below |
| Owner effort | One tap in a month that trades, about three a year | None; pause, downgrade or an Auto lock stops it |
| Blast radius of a defect | One reviewed order | Every rebalance until a control stops it |
| Ships | After phases 1 and 2 (S and M items) | After the authority path is served (an L item) |

Recommendation: A now, B next, on the same row. Under A the new buying
code meets live money with the owner as the last line, as the cash
sweep's first buys did (`cash-sweep.md`, P1); B then changes only who
presses.

Rejected:

- A pre-authorised bucket with the 30-minute veto, sent by the daemon's
  scheduler (`proposal_automatic.go`). It would arm risk-adding buys with
  a policy-file line instead of the device-confirmed Fully armed mandate
  the owner named. `core_rebalance` stays out of the pre-authorisable
  vocabulary (`validPreAuthorisedBucket`), with a test.
- Desk's manual Add with a quantity Desk computes. Desk would size an
  order, the fill would score as the owner's, and B could not reuse it.

### The one typed exception

Core buys need a third typed exception to `authority.close_reduce_only`,
after the sweep's (`cashSweepOpenException`) and currency leveling's,
bound as tightly (review S4): to the `core_rebalance` row it was sized
for, only the pinned contract id, only BUY, only that row's sized
quantity (never more than Q − H of the frozen target), at a cost at most
the funded settled EUR. It is netted against every working IBCF order,
the owner's included; at submit it re-checks the 0.95 budget headroom and
the gap headroom on the current verdict; and it holds on `book_changed`
(the book moved since the row was sized). Checked at preview, prepare and
submit. Every other buy under `close_reduce_only` is still refused. Core
sales are ordinary reductions.

### Shipping sequence

Phase 0, done 2026-10-14: the owner's answers to D1 to D4; the
independent safety review of this record (folded in above); in the lab,
the spec version and the parity fixture. Still open in the lab: EUR
instruments and the band in the harness, which the kill rule's (a) needs.

Phase 1, read-only, no change to the write path, built on Canary branch
`core-sleeve` (2026-10-14):

| # | Item | State |
|---|---|---|
| 0a | Mandate: `index_gap_shock_pct` (D4) over a compiled broad index list; the evaluator, the gap caps and the forced reduction's gap cut read each underlying's gap | Built, `627af51e` |
| 0b | Mandate: the incremental additions gate refuses an addition that creates or deepens a gap-cap breach (B1) | Built, `cdbf8ef8` |
| 0c | Mandate: IBCF measured on SPY's local-currency returns (S5) | Built, `342582ee` |
| 1 | `[mandate.core]` schema: inactive when absent; a missing key holds the core and names it, never in the constitution's approval list; fingerprinted | Built, `e0421bb1` |
| 2 | The history store keeps the core instrument's series, held or not, by contract id on the line's primary exchange, fresh on that exchange's calendar (S7) | Built, `e0421bb1`; venue and calendar from IBKR's record since `core-venue` (2026-10-18) |
| 3 | `internal/core` evaluator and the parity fixture test; data guards; the headroom bisection; the band and minimum | Built, `293fb222` |
| 4 | `core.target` verdict, frozen monthly state with `core_target_drift`, RPC, `canary core` (goldens), `canary_core`, `TestOnlyTheDaemonEvaluates` | Built, `e0421bb1` |
| 5 | Scorecard resolver: the core instrument's fills and `proposal:core_rebalance` go to sleeve `core`, reported, never gated (S1) | Moved to phase 2 |
| 6 | Desk: the core's line on Risk › Mandate (target, holding, trend, next decision), with no IC, e-value or "established" (S1) | Moved to phase 2 (Desk) |

Resolution (2026-10-14, branch `core-resolve`; its pin on IBIS is
superseded by the venue fix below): the live daemon could not
resolve Xetra funds by symbol (`symbol_inactive: no security definition`
for IBCF, SXR8, EUNL and IUSE on IBIS, IBIS2 and SMART). The owner pins the
core by contract id, found by ISIN with `canary core resolve`, which lists
every listing and every form's answer and chooses none; `con_id = 0`
holds the core with that instruction. The series read asks the pinned id
first with no symbol and then reads history for the contract as IBKR
names it (IBKR's symbol, IBIS, the listing's primary exchange), because a
contract id sent with a symbol IBKR does not use for it is refused. Why
the symbol lookups fail is for the live check: the resolver's trace shows
IBKR's answer per form.

Venue fix (2026-10-18, branch `core-venue`, owner decision 08:06 CEST):
the policy of 2026-10-16 (v11) pinned a contract id on `IBIS`; that
contract is IBKR's only line for the ISIN, the London EUR line (primary
`LSEETF`), and every series read on IBIS was refused with "No security
definition" (06:41, 06:57 and 07:13 CEST), while the hold said only "no
IBCF daily history yet". The core now finds its contract by ISIN and
currency, reads it on its primary exchange, judges it on that exchange's
calendar and names a refused read (section 1); the policy names only the
fund and the currency.

Phase 1 knows its limits: IBKR's record names the line under the ISIN
(checked every six hours), and the contract id is checked against the
series' echo; the daily contract-details check at preview is phase 2
(12a); the decision needs the decision day's own bar and waits for it;
before the embedded calendar's coverage (2026) the series' own dates
stand.

Then witness W0 (section 6).

Phase 2, option A (built 2026-10-18, branch `core-rows`; the section
"Phase 2 as built" below says exactly what each item is):

| # | Item | State |
|---|---|---|
| 7 | `core_rebalance` rows: window (skipping US-closed sessions, S8), band, minimum, headroom, netting against every working IBCF order, readiness; a trend-off remainder continues past the window (S6); a data hold stops buys and serves the sale to the owner (S6) | Built, `core_rebalance.go`. The due time (2026-10-18, `core_due.go`): readiness `opening_window` with `default_send_at` fifteen minutes after the US cash open of the row's session, the preview's `core_before_due_time` hold, the controller's first ready read after it; the owner's hand sees the same hold |
| 8 | The typed buy exception at preview, prepare and submit, bound to the row, its quantity and its funded cost, re-checking both headrooms (S4) | Built: `coreOpenException`; the headroom through the refresh that re-sizes the row at preview and submit and the mandate gate; funding through the stock Add admission where `[position_add]` is written; `book_changed` is the prepared path's `prepared_proposal_changed` |
| 9 | Core buys require a known incremental mandate evaluation (section 3) | Built before: the mandate gate refuses an addition it cannot evaluate, at preview and submit |
| 10 | The sweep's planned need from the core's pending buy | Not built: the sweep's seam `cashSweepPlannedNeeds` still reads 0; a buy whose cash sits in bills waits for settled EUR and its row says why at preview |
| 11 | Desk: the row's notice and its review through the existing prepared-proposal confirmation; the matcher accepts the row | Desk batches any BUY row as an add and prepares it as an exact proposal (`execution_batch.go`); no Desk change needed. A dedicated notice is not built |
| 12a | Items 5 and 6 above; the contract-details check of the core's line at preview (the line IBKR lists under the ISIN, EUR, its primary exchange, STK) | Item 5 built (the scorecard's `core` sleeve). Item 6 (Desk's core line) and the contract-details check are not built; the row's contract is the one the six-hourly lookup pinned and the exception binds the preview to its id, type and currency |
| 12b | D5, decided 2026-10-18: the core's contract is outside protection's automatic sales | Built, `core-venue` |

Then, optionally, the plumbing witnesses W1 and W2 below. Owner, 2026-10-18 21:05 CEST: "we NEVER want shadow, it's useless": no witness is a step the core must pass; the core is written live (risk policy v14, 21:18 CEST) and its verdict is its backtest and one look.

### Phase 2 as built (2026-10-18)

What happens while `[mandate.core]` reads `stage = "live"` with `spec_hash`
(the register resolves the core live: the catalogue's rule now sends):

- At each decision (the London line's last session of the month, from half
  an hour after its close) the daemon freezes the month as before, and the
  first background refresh after it records the month's decision beside the
  frozen target (`coreDecisionRow`): the rule's action on the holding then
  (buy, sell, sell all, none or wait with its reason, a refused buy naming
  the gate's reading), the window and the row's key. Recorded once; the row
  itself is sized afresh by every proposal generation from the current
  holding and verdict, so a partial fill becomes a smaller remainder and a
  verdict that stops reading `within` withdraws a buy.
- The proposal engine serves at most one `core_rebalance` row per generation
  (`coreRebalanceProposals`): only while the core is live, the month is
  decided, the rule wants a buy, a sell or a sale to zero with a positive
  quantity, and the window stands. The window is the first five sessions on
  the line's calendar after the decision day that the US also trades
  (`core.WindowSessions`, compiled with the rule as its input window is, not
  a policy key; London's early closes are skipped). A buy or a sale down is
  served for the window's sessions and then waits for the next decision; a
  sale to zero continues in the next sessions until sold. A row is served
  blocked, never open, while a `mandate_reduction` row of the line stands
  (buys; until 2026-10-19 an open breach episode blocked a buy too, against
  the per-sleeve rule of 2026-10-18), and while any order for
  the contract works on either side, the owner's included, or one Canary
  sent is unacknowledged. The owner's ignore applies.
- The row: BUY or SELL of the pinned contract by id, routed SMART with the
  line's primary exchange, STK in EUR, LMT DAY, quantity and maximum both
  the sized quantity, the position effect open, increase, reduce or close,
  the reference price (the held line's mark, else the last close), the
  decision's figures (`TradeProposalCore`: decision, trend, target, held,
  band, headroom cut, scale, the decision close, the verdict, the session,
  the sleeve and stage) and the terms (`CoreRowTerms`: LMT, DAY, the limit
  rule, the venue, the window, the episode `core:<account>:<mode>:<YYYY-MM>`).
  The limit itself is the preview's patient limit: the live mid snapped to
  the tick, a buy rounding down and never above the bid, a sale rounding up
  and never below the ask (D2, the plain limit; Adaptive is not used).
- The path: `trade.proposals` lists the row with its readiness on the London
  calendar; Desk batches it as an add (a buy) or a reduce (a sale) and
  prepares it as an exact proposal; the owner confirms on the device; the
  prepared submit re-resolves the row against a fresh refresh (the band, the
  minimum, the headroom and the netting again), refuses a changed row
  (`prepared_proposal_changed`), and the order preview inside it runs the
  risk limits, the drawdown brake, the mandate gate (the whole book within,
  no gap-cap breach, the core inside its allocation), the stock Add
  admission (funding from settled cash) on a stock buy while the policy
  carries `[position_add]` with `admission_contract = "stock-entry-v1"`,
  and the close-reduce check with the core's typed exception. The daemon sends
  nothing by itself: `core_rebalance` is not in the pre-authorisable
  vocabulary and `automaticBucketFor` returns nothing for it.
- The record: the proposal journal's events for the row key (previewed,
  blocked, submitted with the order reference) are read back as the last
  decision's outcome on `canary core`; a submitted row's fill resolves to
  the scorecard's `core` sleeve under `core-ibcf-v1`.
- In shadow nothing changes: no row, the shadow record as before, its intent
  now with the terms the live row would carry and for the window's first
  session (the one builder, `coreRowTerms`).

Every live register entry since 2026-10-19 (`strategy-register.md`, "Rows
for every live entry"): the same builder, terms, window, due time, readiness,
blockers and typed exception serve each live `[mandate.strategies.<id>]`
entry's month as a `strategy_rebalance` row on its own line, under its own
episode (`strategy:<id>:<account>:<mode>:<YYYY-MM>`) and sleeve; the core's
row, its key, its episode, its words and its goldens are unchanged
(`TestStrategyRowOnTheDecisionDayBesideTheCore` serves both in one
generation).

Not built in phase 2, by decision or scope: the sweep's planned need (item
10); Desk's core line and notice (items 6 and 11); the contract-details
check at preview (12a); the `send_after` due time and the window as policy
keys (compiled instead, so the live file keeps loading); option B's
automatic submission (phase 3). Writing `stage = "live"` arms none of phase
3: every row still needs a device confirmation.

Phase 3, option B: what the implementation sequence in Desk's
`automatic-trading.md` still lacks against Canary main, numbered by its
steps, plus the core's own items:

| # | Item (step) | State today | Size |
|---|---|---|---|
| 12 | Serve `desk.execution.submit` with the automatic authorisation and construct the wire fence on that path (1, 2) | Built 2026-10-19, `desk_execution_submit.go` (section "Phase 3 as built") | L |
| 13 | One admission for funds and commitments across manual, automatic and pre-authorised work, enforcing AUTO's priority there; overlap with the protection scheduler and queued reductions (2) | Not built | M |
| 14 | Pin lookup's ID-only `absent` receipt in handler tests (the contract edge Desk recorded on 2026-10-10) | Built: `TestDeskExecutionSubmitRefusesWithoutStandingAuthority` reads `absent` after an authority refusal | S |
| 15 | Contract: a proposal item may carry class `add` only for `core_rebalance`; fence action `entry` (full scope) for its buys, `reduction` (Protection) for its sales | Built: `Validate` accepts the class, the daemon classifies the row itself (`deskExecutionClass`) and refuses any other buy `class_mismatch` | S |
| 16 | Desk snapshot and executor adapters over the Go client with the private capability (3) | Interfaces and tests only | M |
| 17 | Torok lifecycle: the controller in the operating coordinator, reconciliation at startup and reconnect, controls through the daemon generation, the 15:45 due time (4) | Not registered; the due time itself is the daemon's since 2026-10-18 (readiness `opening_window`, `core_due.go`), so the controller needs no clock of its own | M |
| 18 | Device-confirmed arming in Desk (prepare, confirm) (4) | Arming refused | M |
| 19 | Adaptive for IBCF proven and admitted, or the D2 plain limit at the mid for the core only | US-only | S |
| 20 | Contract and recovery witnesses with a simulated daemon (5) | Not built | M |

Then witness W3.

### Phase 3 as built (2026-10-19, the daemon's half)

The owner's decision of 2026-10-18 21:05 CEST ("when I arm the system, our
strategies must be live") makes option B the target: Fully armed submits
the live strategies' rows with no tap per order. The daemon now serves the
path; Desk's controller does not present the mandate yet, so until it does
every row still waits for the owner's tap (option A stands unchanged).

What `desk.execution.submit` with `automatic` does (`desk_execution_submit.go`):

- Verifies the standing mandate against the daemon's own record under
  `deskAuthorityMu`: the terms the owner confirmed on a device
  (`handleDeskAuthorityConfirm`), the controller's private capability by
  its hash, the generation and preferences hash the controller reconciled
  against, the controller confirmed running in this daemon process, an
  armed scope and this account. A mismatch refuses every item
  `authority_changed` before any journaling (a lookup answers `absent`);
  the daemon invents no authority and a saved AUTO bit, a policy line or
  the register's `stage = "live"` arms nothing.
- Classifies the row itself (`deskExecutionClass`, mirroring Desk's batch
  groups): a `core_rebalance` BUY is the one `add`, its SELL a `reduce`, a
  stop or trail `protect`; a buy outside a strategy row, a sweep or a
  conversion has no class and is refused `class_mismatch`. An `add` under
  Protection is refused `authority_changed` and recorded; an entry-rule
  addition (`intent.add`) is not served (`intent_not_served`).
- Binds a strategy row's item to the row's session episode,
  `<terms.episode>:<session>` (`core:<account>:<mode>:<YYYY-MM>:<session>`),
  refusing another episode `episode_mismatch`. The journal records every
  request with its episode before anything can reach the broker, so one
  decision yields at most one request a session: a refused row is recorded
  with its reason and not retried that session, whatever the controller
  retains; the next session's row is a new episode.
- Sends through the one order path: `brokerWriteMu`, a request-scoped grant
  (`deskExecutionGrant`, like the scheduler's), the proposal engine's
  revalidation (never the cached fast path), the preview with every gate
  (risk limits, the drawdown brake, the mandate gate, the stock Add
  admission, the close-reduce check with the core's typed exception), the
  origin gate for `desk-automatic` (a grant whose fence has not expired) and
  the wire guard, which carries the mandate's fence (`binding.deskAuthority`)
  and re-reads the record under the wire lock before the first byte:
  an `entry` fence needs scope `full`, so a downgrade, disarm, brake hold,
  stop or generation change between admission and transmit refuses the
  send (`lockDeskAuthorityForWire`).
- Decides the outcome from the order journal: the engine's acceptance is
  `accepted` with the order reference; otherwise no staged attempt or a
  provably unsent one is `refused` with the gate's blockers, a staged
  attempt without a known outcome is `unknown` (resolved only by lookup),
  and a send the journal shows reached the broker is `accepted`. The
  receipt's message and the journal's request row name the mandate (and,
  for a strategy row, the register entry and sleeve); the order journal's
  origin is `desk-automatic`, journaled, read and settled as the machine's
  own.

- Sends a strategy row in session only: a row whose live readiness is not
  `ready` (its exchange closed, the opening window, a stale quote, the broker
  link down) is refused with that readiness code before journaling, so the
  session's episode stays free and the controller sends the row once it
  reads ready, at the live mid, inside the window. The served row's
  readiness answers the first three; a stale, delayed or missing quote and a
  spread the preview refuses are read by a preview run before the journal
  write (`deskExecutionStrategyNotReady`, 2026-10-18 23:30 CEST; until then
  that refusal was journaled and consumed the episode). A hard refusal (the
  row withdrawn, its revision stale) is journaled as before. The core's row reads the
  opening window until its due time, fifteen minutes after the US cash open
  of its session (`core_due.go`, "Due time" above), so the first ready read
  is at or after 15:45 Europe/Berlin. The controller never uses
  the owner-queue path; neither does the owner's hand under option A any
  more: `prepareOwnerQueue` refuses the core's row
  (`queue_bucket_not_queueable`, as its readiness already said), because a
  queue to the open would cap the row at its reference price, the series'
  last close, so a higher open leaves it unsent (found by the Desk quality
  loop, 2026-10-19), and the queue's terms carry none of the row's typed buy
  exception. Desk's closed-market routing of STK rows to the owner queue
  (`execution_batch.go`, `views/ticket/proposal.js`) therefore shows this
  refusal for a core row in the evening; the Strategies surface routes the
  core's row to an in-session review instead (Desk's work). No row field
  changes.
- The month's decision record follows the row the window serves
  (`recordCoreDecision`, found by the Canary quality loop, 2026-10-19): a
  month first recorded as "no buy" (the verdict read reduce) is re-recorded
  with the row's key when a later refresh inside the window serves the buy,
  so `canary core` reads that row's outcome; the evaluation runs each
  cadence until the recorded row was submitted, and a served row's record
  stays, with the journal's outcome, when the row is later withdrawn or its
  window ends. The sized quantity alone changes no record.

Capabilities list `submit` with authority `automatic`; the manual batch is
still not served. The Go client gains `SubmitDeskExecution`, private like
the authority methods and outside the MCP catalogue.

Tests (`desk_execution_submit_test.go`, `desk_execution_submit_trading_test.go`,
`core_rebalance_test.go`, `proposal_queue_test.go`): a strategy row not ready
is refused unjournaled and a ready one reaches the gate; the record follows a
buy served later in the window and an unchanged decision re-records nothing;
the owner queue refuses the core's row;
no mandate, another capability, a stale generation, other preferences, not
running and disarmed each refuse before journaling; a core buy under
Protection is refused and recorded once per episode, a second request
`episode_consumed`; under Fully armed the buy meets the gates and a retry
answers from the journal without a second attempt; the classification and
the episode; the origin gate needs a live grant and human origins never see
it; a served row is sent exactly once through the production write path to
the fake broker socket with the `desk-automatic` origin, the grant cleared,
a retry and a consumed episode send nothing, a wrong class is refused and
recorded, a stopped controller refuses before journaling; the binding
carries the fence to the wire and the guard refuses an `entry` fence once
the mandate reads Protection.

Not built, Desk's half (items 16 to 18 and 20): the snapshot and executor
adapters over the Go client, the controller in Torok's operating
coordinator, device-confirmed arming in Desk, the controller's recovery
witnesses; item 19 (Adaptive for IBCF) stays at D2's plain limit. Desk is
pinned to a Canary module without `SubmitDeskExecution` and its item
validator refuses a proposal `add`, so the Desk work starts with a re-pin
after this release.

## 3. The mandate

#### For your review, the kill rule and the per-sleeve buy (2026-10-19)

A review of the whole system on 2026-10-19 found three defects in this path,
fixed in v3.25.3. A row served while the entry holds (a data hold, the kill
rule; the sale to zero its frozen month wants, review S6) now carries
`core.owner_review` and goes to the owner: `desk.execution.submit` refuses it
under the standing mandate (`owner_review`, before journaling and again at
the wire on the revalidated row), the row's details say so, it stays out of
the same-contract merge (a forced reduction of the line is never covered by
it), and Desk, re-pinned on this release, shows it as the owner's to confirm;
until then the row said "for your review" while
the controller took the sale as a candidate and the daemon sent it (owner,
2026-10-19: for your review means the owner's review). The kill rule's bound
is measured against the sleeve's capital at its stage (the core's on its
line's holding marked at each decision, since every unit of its line is the
core's whoever bought it), and a live entry without a written bound holds
(`strategy-evaluator.md` §3); `[mandate.core]` writes no bound yet, so the
core holds until the owner writes one. The core's buy is judged by its own
sleeve as the gate judges an order, and an open breach episode no longer
blocks it (`mandate-risk-budget.md`, "The gate", the decision step).

### What the gate does today

`mandateBudgetGate` (`mandate_verdict.go`) runs at order preview, strategy
preview, admission and the final wire guard. A risk-adding order (anything but
a close or reduce, or a buy of a put on a hedge symbol) is refused unless the
kept verdict for the policy in force is at most five minutes old and reads
`within`, and the book with the order added stays within the whole budget and
creates or deepens no gap-cap breach. Since 2026-10-18 the order's sleeve,
with the order added, must also stay inside its allocation
(`mandate-risk-budget.md`, "Risk allocations"); every unit of the core's
contract is the core's, so a core buy, and an owner's hand buy of the core's
fund, is judged against the core's allocation.

Two gaps mattered here; both are closed on this branch. A name without a
decision-grade series now refuses the addition and fetches the series
(main `56a3c94a`), and IBCF needs none: the mandate measures it on SPY's
returns and prices an unheld IBCF order at its limit, refusing one without
a limit (review S5). And the incremental check measured volatility only,
so an addition could pass with a gap-cap breach and be sold back by the
forced reduction; it now refuses an addition that creates or deepens one
(review B1). Item 9 still requires a known incremental evaluation for a
core buy.

Gap stress (D4): a broad index fund or index on the compiled list takes
`index_gap_shock_pct` (the owner's 12) instead of `gap_shock_pct` (25).
At its 40% cap the core's gap loss is 4.8% of the budget base, under half
the 9.99% cap at full room, and both scale with room.

### The core's own risk allocation

By the formula, the sleeve's own volatility is at most S on σ_E and on σ_252,
both measured on the line's own closes, which carry what the sleeve earns. The
mandate measures the same line on SPY's returns, on the US clock of the rest
of the book (review S5); the headroom uses the mandate's measure. Phase 2 adds
the core's standalone risk (the mandate's evaluator on the core leg alone) and
its Euler contribution, which cannot exceed the standalone figure, to the
target. A holding over its risk allocation at a decision is sold down by the
rule; between decisions it is not. Since 2026-10-18 the mandate verdict serves
the core's standalone risk and its Euler use beside its allocation
(`verdict.allocations`).

### Headroom: the core never pushes the book over budget

A buy is the largest whole quantity, up to the funded D, for which the
book with the buy measures at most 0.95 × budget, carries no gap-cap
breach, keeps the core's own gap loss at most at 0.95 × its cap, and, since
2026-10-18, keeps the core inside its allocation,
found by bisection on the daemon's evaluator (built: `core.Headroom`,
`coreHeadroomFits`). 0.95 is the forced reduction's own
`mandateReductionTargetFactor`, not a new number. The forced reduction
starts above 1.0 × budget on two scheduled readings, or on a gap-cap
breach, and sizes back to 0.95, so a core buy can never start a forced
sale by itself; only the market can.

### The core's allocation (2026-10-18, replacing D1)

The core has its own allocation inside the whole-book limits
(`mandate-risk-budget.md`, "Risk allocations"), reserved as far as it runs:
its stage times its allocation, `scale_pct / 100 × core_vol_pct / 100 × budget
base × room`, while the core runs, and nothing while it sends no orders, as in
this build (phase 1 is read-only). The owner has the rest of the budget. Once
the core runs, the owner's additions stop at the owner's allocation, so the
core's allocation stays free for the core; the core's own rule sizes a buy
to the whole-book `within` and headroom above, and to its own allocation with
the buy added. The mandate gate itself admits a core buy inside the core's
allocation while another sleeve has the book over budget (owner, 2026-10-18
15:31 CEST: "yes, it should. unless you advise differently";
`mandate-risk-budget.md`, "The gate"); since 2026-10-19 the core's rule
buys inside its allocation while the book is over budget, judged by the
gate's own reading (`mandateVerdictHolds`; `mandate-risk-budget.md`, "The
gate", the decision step), where until then it bought only on a `within`
verdict. An unknown verdict admits no core buy. The core's sales (rebalance
down, trend off) always pass.

A sleeve's Euler use never exceeds its own standalone risk, and every core buy
is cut to the core's allocation on the mandate's measure (`coreHeadroomFits`),
so others' holdings can raise the core's use at most to its standalone risk.
Between decisions a smaller room shrinks the allocation while the holding
stays; that drift is the allocations' residual risk. While the core runs
nothing, `canary core` reads "no buy: the core sends no orders yet, so it has
no allocation".

### With the forced reduction (on main since `9b10b9ca`)

Since 2026-10-18 the reduction cuts the sleeve furthest over its allocation
first, and within it the largest Euler contributor (`mandate-risk-budget.md`,
"Risk allocations"). A core inside its allocation is not cut for the owner's
excess; IBCF held while the core runs nothing (allocation zero) is over its
allocation by its whole use and is cut once it is the furthest over. To
coexist:

- One row per contract and side: a reduction sale of IBCF and a core sale
  merge into one row sized by the larger need (the existing same-contract
  merge; the mandate row takes precedence).
- No core buy while any reduction row of IBCF stands, or while a sale of
  IBCF works (an open breach episode no longer blocks one, 2026-10-19: a
  core inside its allocation adds while the reduction cuts the sleeve over
  its own).
- No buy-back in the month of a forced cut: the core buys again only in
  the next window, after `within`, under the 0.95 headroom.
- At the maximum loss the room is 0, so S and the target are 0, and the
  reduction flattens to hedges. IBCF is not a hedge and is sold, which is
  also what the rule wants.

The ranking "largest risk contributor first" (06:26 CEST), which D1 kept,
was replaced on 2026-10-18 08:06 CEST: the sleeve furthest over its allocation
first.

### Drawdown brake and controls

The brake refuses risk-adding orders, so core buys stop and core sales
continue. Under B, each engagement holds a full mandate at protection
until the owner confirms full again on a device; core sales are
reductions and keep running under Protection. Freeze, MANUAL and Disarm
stop new core buys at the final gate and cancel nothing. An Auto lock on
IBCF locks the instrument, so it stops core sales as well as buys; a
trend-off sale waits while it stands (review note).

### Protection (D5, built 2026-10-18)

Protection's automatic sales leave out the core's contract (the line in
use and the one the core last used): a pre-authorised trailing stop or
budget reduction on it is still served as a row for the owner's hand, but
gets no automatic record and does not read pre-authorised, a record
waiting from before the core used the line is superseded with the reason,
and the final submit gate refuses it (`proposalEngine.automaticBucket`).
The mandate's forced reduction still submits its rows on the line under
`mandate_reduction`, and the owner's own orders and queued authorisations
are unchanged. When Desk's controller is served under Protection (phase
3, items 12 and 15), its protection sales need the same exclusion; core
sales as the rule's own rows are not protection sales.

## 4. The FX rule

### Measure

USD share u = IBKR's net liquidation in USD (the ledger's per-currency
`NetLiquidation`: USD cash plus USD securities and options at market
value) at the ledger rate, divided by the account's net liquidation. The
owner said NAV, so the base is net liquidation, not the risk base. IBCF
is EUR and hedged at fund level, so it adds nothing; its unhedged
intra-month gains are a negligible residual.

Two facts shape enforcement:

- A trade inside USD (a USD stock bought with USD cash or a USD loan)
  leaves u unchanged. u moves with USD gains and losses, with flows, and
  with conversions.
- Currency leveling's repayment of a USD loan converts EUR into USD and
  raises u; a USD loan lowers it.

### Rule

| State | Action | Who sends |
|---|---|---|
| u ≤ 20% | Nothing | — |
| u > 20% on two consecutive daily readings, with USD cash above its keep-cash and commitments | One conversion row, BUY EUR.USD, sized to the lesser of the free USD cash and (u − 15%) × net liquidation; new USD bill buys hold while u is over the cap | The owner, one tap on the leveling card |
| u > 20% from USD securities alone | Reported on Risk and in the brief; nothing automatic | D3: "Report now, futures hedge later": micro EUR/USD futures once supported; never a USD loan |

The conversion uses leveling's order path: IDEALPRO, LMT DAY within
`max_slippage_bp` of a live two-sided mid, a typed exception for
converting a positive balance down to the target, held while any CASH
order works (which holds the sweep, as today). It never takes USD cash
below its keep-cash, so it never creates a loan for leveling to repay, and
the two cannot oscillate. Leveling itself is unchanged. The 15% target and
the two-reading debounce are proposals the owner writes. Cadence: one
reading a day after the US close (22:30 Europe/Berlin), at most one
conversion row a day, for the next IDEALPRO session.

Cost: an IDEALPRO commission of about 0.2 bp (USD 2 minimum) plus at most
the slippage bound of 2 bp, a few times a year, with no carry. The
measurement is automatic; the conversion is confirmed, following
leveling's "not pre-authorisable" decision, and B never sends one (the
execution contract has no class for it). Policy home:
`[cash.currency_cap]` under the `[cash]` authority. Rulebook rule 14
stays watch-only.

## 5. Safety invariants and failure modes

Invariants, each with a synthetic test in section 6:

1. Only Canary computes the target, the quantity and the limit. Desk and
   Torok never supply a size or a price.
2. One frozen target per month, persisted with the digest of its inputs.
   A recomputation that differs holds the month (`core_target_drift`).
3. At most one core order per session, and none while a core order
   is working, unacknowledged or of unknown outcome.
4. Every quantity is the target minus the confirmed holding; a partial
   fill never makes a new target or tops up past it.
5. A core buy passes every existing gate, including the mandate gate with
   a known incremental evaluation, fits under 0.95 × budget and is funded
   from settled EUR without a loan or a conversion; otherwise it waits.
6. The core never sells more than it holds and never shorts.
7. `core_rebalance` is never pre-authorisable. It reaches the broker only
   through the owner's device confirmation (A) or the Fully armed
   mandate's wire fence (B, served since 2026-10-19; Desk does not present
   the mandate yet).
8. Missing, stale or suspect data holds. Nothing is guessed and no series
   is substituted.
9. Restrictive controls stop new core buys at the final gate and cancel
   nothing; an Auto lock on IBCF stops core sales too.
10. A core buy never by itself takes the book over 0.95 of the budget or
    any gap loss over 0.95 of its cap (property test, built), nor the core
    over its allocation (2026-10-18).

| Failure | Guard |
|---|---|
| Duplicate order | Frozen target; one episode per session (a one-use prepared reference under A; the journal's request and episode keys under B); netting against working, unacknowledged and unknown core orders; quantities from confirmed holdings |
| Partial fill | The DAY order expires; the next session's D comes from confirmed holdings, only once the position projection is newer than the last fill; the window's end drops the rest |
| Restart | Target and digest persisted; the order journal resolves unknown sends before any new core row; under B the controller reconciles before it runs |
| Stale data | A decision needs the decision day's bar; a buy needs a verdict at most five minutes old, equity within `capital.max_equity_age_minutes`, current positions and a live two-sided quote; the frozen target expires with its window |
| Whipsaw | Monthly evaluation only, no flip within a month, the band; about one flip a year at about 10 bp of K. A buffer around the average would be a new lab variant, never a live tweak |
| Market closed, holidays | The line's exchange calendar (London SETS today) sets the decision day, the window and readiness; half days follow it; a primary exchange without an embedded calendar holds; past its coverage (2028-12-31) the core holds |
| Bad ETF price | A daily move beyond ±8% that SPY's move over the two US sessions spanning IBCF's day (from the US close before its previous session to the first US close on or after its own day, review S5) does not show at half the size and the same sign, a missing session bar, a bar on a closed day, σ outside 5–100%, or a limit more than 10% from P_m holds the month for review (built but the limit check, which is phase 2) |
| Wrong contract | The contract is the one line IBKR lists under the ISIN in EUR; several lines hold and name each, a line other than the one last used holds, a written con_id must be among the ISIN's lines and a written exchange a venue IBKR lists it on; a held line of the fund under another contract id holds (built) |
| Currency mismatch | The order currency is EUR, the pin's and the account base's; funding only from settled EUR; a core order never creates a conversion or a debit; a non-EUR base holds |
| Mandate unknown at the decision | The target needs only the verdict's capital; a buy still needs a fresh `within` when it is sent; sales go |
| The owner trades IBCF by hand | The instrument is reserved to the core: its fills count as the core's, the next decision corrects the holding, and while an owner IBCF order works no core row is served (phase 2 nets every core row against it) |
| An IBCF holding exists before the core | Adopted at the first decision as the core's holding; the rule moves it to the target past the band (a sale of the excess is a row the owner confirms under A) |
| A second line of the fund under another contract id | Holds the core (built) |
| Inputs that read differently after a month was decided | `core_target_drift` holds the month (built) |
| Cash sits in bills | The planned need starts at the decision; the buy waits for settled EUR inside the five-session window |

## 6. Acceptance tests and live witness

Synthetic tests that must pass before A is offered (1–14) and before Fully
armed may include the core (15–20). Built in phase 1, each with its test: 1
(`TestParityWithTheLab`), 2 and 11
(`TestMeasureRefusesWhatIsNotAMonthEndDecision`, `TestCoreTargetHolds`), 3
(`TestCoreTargetFrozenMonthDriftAndCeiling`), 4
(`TestSizeTargetRoomScaleAndCap`), 5 and the band and minimum of 6
(`TestDecideBandMinimumAndHeadroom`,
`TestCoreTargetTrendOffSellsAllAndOverBudgetBuysInsideTheAllocation`), the headroom of 6
(`TestCoreTargetBuyCutToTheGapCapMargin`), the contract and data guards of 9
and 10 that need no order (`TestCoreTargetHolds`,
`TestGuardHoldsSuspectData`), 20 (`TestCoreHeadroomNeverBreaksTheBudget`), the
monotone target (`TestTargetNonIncreasingInDrawdownAndVolatility`), and for
the mandate fixes `TestBroadIndexTakesTheIndexGap`,
`TestMandateGateRefusesAnAdditionThatBreachesAGapCap`,
`TestMandateReductionBroadIndexCutAtTheIndexGap` and
`TestMandateMeasuresIBCFOnTheIndex`. The rest need the phase 2 rows and
exception.

1. Parity fixture, Go against the lab: trend and Q exact, floats to
   1e-9; cases where each of σ_E, σ_252 and the floor binds, a holiday
   month-end, and no target before ten month-ends.
2. A decision waits for the decision day's bar; a running month is never
   decided; a missing bar holds.
3. The frozen target survives a restart; a changed input holds with
   `core_target_drift`.
4. Unknown capital gives no target; room 0 gives Q = 0; `scale_pct` 25,
   50 and 100 scale Q.
5. Band and minimum: inside the band, no row; the sell-all and the first
   buy pass the band; a buy below the minimum waits; the sell-all ignores
   the minimum.
6. Buy sizing is the least of D, funding, the 0.95 headroom and the
   current-capital target. An unknown incremental evaluation, or a verdict
   that reads over, reduce or unknown or is stale, gives no buy; sales
   still go.
7. One order per session; a working, unacknowledged or unknown core order
   holds the next; after a partial fill the next session buys only the
   remainder; never past Q; the window's end drops the rest.
8. Merge with a mandate reduction of IBCF (one row, the larger need); no
   buy beside a reduction row or a working IBCF sale (the breach-episode
   block retired 2026-10-19); no buy-back in the month of a cut.
9. Contract guards: a mismatched contract id, ISIN, currency, exchange or
   type, a second line with the ISIN, and a non-EUR base each hold.
10. Data guards: the price jump, the missing bar, the σ bounds and the
    price distance each hold.
11. Calendar: a month-end on a holiday, a closed session, a date past the
    calendar's coverage.
12. Funding: short settled EUR waits; the planned need reaches the sweep;
    `no_buy_while_borrowed` holds the buy; no conversion is ever produced.
13. The typed exception admits only the pinned buy up to Q − H at
    preview, prepare and submit; every other buy under
    `close_reduce_only` is still refused; `core_rebalance` is refused in
    both pre-authorised lists, and the scheduler never creates a record
    for it.
14. Scorecard: core fills land in sleeve `core`, in no owner stream, and
    are not gated.
15. B: a core buy needs fence action `entry` under full scope; a core sale
    runs under Protection; a brake hold refuses the buy at the wire.
16. B: pause, downgrade, disarm and an Auto lock racing a dispatch are
    refused at the final gate; an order already sent survives.
17. B: episode consumption across a restart; a lost reply resolves by
    lookup and never sends a second order.
18. B: unsupported Adaptive gives a structured refusal and no fallback,
    unless D2 allows the plain-limit shape, which then has its own test.
19. B: a core buy and an entry-rule addition competing for the same cash
    are admitted once each in the controller's order, after protection and
    reductions.
20. Property: on synthetic books, a core buy sized under the 0.95
    headroom never by itself yields a verdict over the budget.

FX: u from ledger rows; the two-reading debounce; the conversion sized to
the target and to free USD cash; a working CASH order holds; USD bill buys
hold while over the cap; no row when USD securities alone cause the
excess.

Live witness, each step only after the one before. These are optional
plumbing checks the agent may run beside the live core, never gates: the owner
decided on 2026-10-18 21:05 CEST that there is no shadow step ("we NEVER want
shadow, it's useless"), and the core went live at 21:18 CEST (policy v14).

- **W0, shadow.** At least one month-end, two preferred, with the verdict
  served read-only in the live account and compared with the lab's
  recomputation on independent closes (Yahoo's IBCF.DE, the fund's Xetra
  line, which closes at the same instant as London's, 17:30 CET; the lab
  aligns the two on the sessions both trade): the same trend, σ within
  0.5 points, Q within 1%. The lab's R2 shadow log starts here.
- **W1, paper.** One buy and one sale through A in the paper account:
  contract resolution by ISIN, London readiness and the 15:45 due time,
  IBKR's answer on Adaptive (A1), tick snapping, settlement (A3), the
  sweep's planned need, the scorecard sleeve and the receipts.
- **W2, live under A.** `scale_pct` 25, then 50, then 100, one monthly
  window each (the lab's R3 and R4 fractions). In each window: fills within
  10 bp of the mid at sending, the holding equal to Q within one share, no
  duplicate, the standalone risk within the risk allocation.
- **W3, B.** After tests 15–20 and the authority path's own witnesses, the
  owner arms Fully armed on the device, and the first automatic window is
  watched end to end.

Suspension, by the owner setting `scale_pct` to 0: s0's kill rule (the
sleeve's drawdown beyond 1.5 times its backtest maximum at the same
volatility, or a rolling 24-month net Sharpe below 0 while the static
mix's is above 0.5), or realised costs above twice the cost model.

Assumptions to verify before the step that needs them: A1, IBKR accepts the
fund's line with Adaptive routed SMART (W1); A2, IBKR's daily bars of the line
equal the closing auction price of the line's exchange, London's for the
London line (W0, against the exchange's close); A3, IBCF settles T+2 and a
Bubill redemption settles inside the window (W1).

## 7. Owner decisions

Decided 2026-10-14 (the questions as asked, the answers verbatim):

- **D1. Do the sleeve shares bind each sleeve, or only the whole book?** "No,
  whole book only." The core uses the room the owner's sleeve leaves, and the
  forced reduction cuts the largest contributor whatever its sleeve. In a book
  whose owner sleeve fills the budget, the core holds nothing; the target
  shows it as a buy cut to no headroom. **Replaced 2026-10-18 08:06 CEST.**
  The owner, verbatim: "Accept all three: give the machine its own seat
  (replacing D1 and largest-contributor-first), put s1 into its six-month
  shadow test, and go for the core venue fix with the core fund out of
  protection's automatic sales." Under D1 the machine had only the room the
  owner's book left, so it could never count on any. Now: allocations inside
  the whole-book limits; the machine's allocation is reserved only as far as
  it runs: its stage times its allocation, nothing while it runs nothing; the
  owner has the rest; over budget, the sleeve furthest over its allocation is
  cut first; each position stays in the sleeve of whoever opened it, and the
  core's fund stays reserved to the core (`mandate-risk-budget.md`, "Risk
  allocations").
- **D2. If IBKR refuses Adaptive for IBCF, may core orders use a plain
  DAY limit at the mid?** "Yes, for the core only." Tick-snapped, never
  crossing the mid against the owner, never repriced; the 2026-10-09 "no
  fallback" rule stays for every other automatic order. If IBKR accepts
  Adaptive, the predicate widens, with a test.
- **D3. When USD securities alone exceed 20% of net liquidation, what
  then?** "Report now, futures hedge later." Excess USD cash is converted;
  USD securities above 20% of NAV are reported; micro EUR/USD futures are
  the hedge once Canary's mandate and order paths carry futures (L);
  never a USD loan.
- **D4. Which gap shock does a broad index fund take?** "12% for broad
  index funds"; single names keep 25%. Built as `index_gap_shock_pct` over
  the compiled list `mandate.BroadIndexInstruments`, matched on symbol and
  currency: SPY, SPX, SPXW, XSP, QQQ, NDX, IWM, RUT and DIA in USD, and the
  UCITS funds IBCF (IE00B3ZW0K18), SXR8 and CSPX (IE00B5BMR087), VUSA
  (IE00B3XXRP09), VUAA (IE00BFMXXD54), EUNL, IWDA and SWDA (IE00B4L5Y983),
  VWCE (IE00BK5BQT80), VWRL and VGWL (IE00B3RBWM25). A line not on the list
  keeps 25%, the stricter stress; an addition is a reviewed change. Since
  2026-10-18 a line also matches by the ISIN IBKR lists its contract
  under (the core's lines, `mandate.Input.LineISINs`), so the fund's
  London line takes the 12% whatever its symbol.

Decided 2026-10-18 08:06 CEST, the last clause of the owner's words quoted
under D1: "go for the core venue fix with the core fund out of
protection's automatic sales." Recorded here:

- **Venue.** The policy names only the fund and the currency: `isin`
  and `currency` in `[mandate.core]`, with `con_id` and `exchange`
  optional. Canary takes the contract, its venue and its calendar from
  IBKR's record for the ISIN in that currency, stops and holds naming
  every candidate when IBKR lists more than one, and holds when the line
  differs from the one last used (section 1). Built on `core-venue`.
- **D5. Does the pinned contract leave protection's automatic scope?**
  Protection proposals cover every held line; a protection sale of the
  core's line lowers the holding and the next decision may buy back
  under the headroom. Proposed: exclude the core's contract from
  protection's automatic sales; the mandate's forced reduction still
  covers it. Accepted ("the core fund out of protection's automatic
  sales"). Built on `core-venue` (section 3, "Protection").

## Proposed policy

The owner writes these; the code carries no default that acts. A missing
key holds the core and names the key, in the core only: it never enters
the constitution's approval list, so a half-written table changes nothing
else. Phase 1 reads the keys below (`risk.ConstitutionMandateCore`; the
template in `canary policy default constitution` shows them commented
out); the build brief's `[core]` is this `[mandate.core]`.

```toml
[mandate.core]                  # absent: the core is inactive
rule            = "core-ibcf-v1" # compiled; carries the lab spec hash
isin            = "IE00B3ZW0K18" # the fund; IBKR's record names its line
currency        = "EUR"
# con_id        = 0              # optional: chooses among several lines IBKR lists
# exchange      = "LSEETF"       # optional: must be a venue IBKR lists the line on
scale_pct       = 25             # ramp: 25, then 50, then 100; 0 suspends
band_pct        = 10
min_order_base  = 2500
max_value_pct   = 40             # of the budget base × room
# spec_hash     = "2408d09e…"    # optional while not live: the lab spec cited
# stage         = "shadow"       # optional: unwritten reads shadow; live, suspended
```

Since 2026-10-18 `[mandate.core]` is the strategy register's first entry
([strategy-register.md](strategy-register.md)). Its two register keys are
optional in this form, so the file above keeps its behaviour and its
fingerprints: an unwritten `stage` runs the core in shadow (it serves its
target and sends nothing, as phase 1 always has), and an unwritten
`spec_hash` lets the compiled rule's own spec stand. The core goes live only
when the owner writes `stage = "live"` with `spec_hash`, and once `stage` is
written `scale_pct` is 25, 50 or 100 and a suspension is `stage =
"suspended"`.

The core also reads two `[mandate]` keys and holds while either is
unwritten: `[mandate.sleeves] core_vol_pct` and `index_gap_shock_pct` (the
owner's 12, D4).

Phase 2 adds the window keys (`window_sessions = 5`, `send_after =
"15:45"` Europe/Berlin) with the rows that read them, and the cash rule's
`[cash.currency_cap.USD]` (`max_share_pct = 20`, `target_share_pct =
15`). An unknown key refuses the constitution, so neither is written
before the code that reads it.

The sleeve's risk allocation is the existing `[mandate.sleeves] core_vol_pct`.
The rule's own parameters (the 253-session window, ten months, span 63, 252
returns, the 15% floor) are compiled with the rule version and are not policy
keys; changing one is a new lab spec version and a new rule.

## Files

Built in phase 1:

```
internal/mandate/index.go          broad index list (D4), IBCF's returns proxy (S5)
internal/core/                     pure evaluator; parity fixture test
internal/core/testdata/            synthetic fixture shared with the lab
internal/rpc/core.go               core.target method and result types
internal/daemon/core_target.go     verdict, frozen monthly state, cadence
internal/daemon/core_resolve.go    the contract from IBKR's record for the ISIN (2026-10-18)
internal/daemon/mandate_history.go keeps the core instrument's series (the line's calendar)
internal/daemon/proposal_automatic.go  D5: the core's line outside protection's automatic sales
internal/daemon/mandate_verdict.go the gap-cap check on additions (B1)
internal/risk/mandate.go           [mandate.core] and index_gap_shock_pct
internal/cli/core.go               canary core [--json]
internal/mcp/tools.go              canary_core, read-only
lab: lab/core_rule.py, lab/core_fixture.py, tests/test_core_rule.py,
     specs/s0_core_sleeve.yaml (2408d09e6189)
```

Phase 2 (built 2026-10-18):

```
internal/daemon/core_rebalance.go  the row builder, the window, the typed buy
                                   exception, the decision record, the views
internal/daemon/core_target.go     the live decision record on the refresh;
                                   read_only false while live
internal/daemon/proposal_engine.go the row in each generation; the core's
                                   admission in the preview safety checks
internal/daemon/shadow_record.go   the intent's terms from the one builder
internal/daemon/scorecard_authority.go  core_rebalance fills in sleeve core
internal/register/register.go      core-ibcf-v1 sends
internal/rpc/core.go, opportunities.go  CoreRowView, CoreDecisionRecord,
                                   CoreRowTerms, TradeProposalCore, the status
internal/cli/core.go               the Row and Last decision lines
```
