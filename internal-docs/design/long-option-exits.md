# Long-option exits: protect, harvest, time

Written 2026-10-24 08:05 CEST at the owner's instruction (07:57 CEST) as the
requirements for how the desk runs a long option once it is held. It states
what must be true, not how; the mechanics live in the designs it names.
Current mechanics: [option-exit-policy.md](option-exit-policy.md).

Long options on volatile stocks make their money by staying in winners and
keep it by cutting losers and resizing. An exit has one of three jobs, each
with its own rules. Status per requirement: **built**, **partly**, **missing**,
as of Canary 278cc600.

## Protect: cut what is wrong

| ID | Requirement | Canary today |
|---|---|---|
| LO-P1 | An exit fires when the stock breaks the idea (a level on the underlying, or the event that was the thesis), not when the option's price wobbles. | missing |
| LO-P2 | A last-resort loss on the premium backs it up. | built: loss exit at −60% (Rulebook loss line), full close, owner approval. The requirement stands; the mechanism needs refining (see Findings) |
| LO-P3 | A protect exit sells with a patient limit inside the spread, never at market. | partly: previews price patient limits; the profit trail is a broker trail that becomes a market order |
| LO-P4 | Adding to a long option that is down more than 40% from its cost waits a cooling-off until the next session and shows the loss so far and the days left; it cannot be sent in the moment. No purpose is asked. (Owner 2026-10-24 08:33 CEST: 40%, cooling-off rather than a hard stop.) | missing |

Considered and not adopted: a shared premium limit for names that move
together (owner 2026-10-24 08:33 CEST: co-movement between these names is
likely a passing phase of speculative flows, not lasting). Co-movement stays
visible as each name's share of risk on Risk › Mandate, which follows the
names' actual joint moves.

## Harvest: keep winners, take risk off

| ID | Requirement | Canary today |
|---|---|---|
| LO-H1 | A winner that grows past its share of the risk budget (premium per name over 15% of NLV) is trimmed back to it; the rest stays on. | partly: the cap blocks additions (2026-10-23); book-level budget reductions exist; no per-name trim |
| LO-H2 | After a large gain, enough is sold to recover the cost; the rest runs. | partly: `recover_cost_gain_pct` (default 100) is in the protection policy and the replay reads it; the exit is not built, every exit is a full close |
| LO-H3 | A winner can be rolled up or out: sell it, buy a further strike or later expiry, keep the direction with less money at risk. The new leg reuses Add's discovery (expiry by horizon, strike by delta range), pricing and sizing; the old leg uses the exit; both go as one combo order at a net price, and the order gates judge the roll by its net change, not as a fresh addition. (Owner 2026-10-24 08:44 CEST.) | missing: deferred since 2026-09-28. Parts exist: Add's call discovery, plan and price ladder; the option exits; a combo route that closes multi-leg units (`option_exit_units.go`), not yet one that closes and opens |
| LO-H4 | Any trail follows the stock and scales to how much the stock moves, so one big option day does not stop the position out. | partly: an owner-signed exit plan (queued authorisations, version 3) trails a held long call on its underlying stock for one session, or for N sessions or until cancelled up to the option's last trading day (version 4, 2026-11-24), at a signed percent from a signed starting stop, and sells what is held when the stock gives back; it can carry a first sale of part of the line after the open. The trail percent is the owner's, not yet scaled to the stock's range (`stock_trail_range_multiple`, default 2.5, is in the protection policy and the replay reads it); the rule-driven trail is still on the option premium, armed at +50%, full close |

## Time: never let decay or expiry decide

| ID | Requirement | Canary today |
|---|---|---|
| LO-T1 | An in-the-money option is closed or rolled before expiry, so nothing is exercised by accident. | built: expiry close at the Rulebook's runway (7 days), owner approval |
| LO-T2 | An option is rolled or closed before its last weeks, when decay speeds up, unless the owner keeps it on purpose. | partly: theta hygiene proposes closes; no roll |

## Control and budget

| ID | Requirement | Canary today |
|---|---|---|
| LO-C1 | An armed exit acts after a short veto window; the owner can stop it, not stall it. | partly: running for stock stops; option buckets exist (`option_loss_exit`, `option_profit_trail`) but are not pre-authorised |
| LO-C2 | A position counts against the premium budget at its exit distance while its exit is armed, at full premium otherwise. | missing |
| LO-C3 | Every exit says which job it does and why, in one line. | partly: proposals carry reasons; no job label |

## Verification

| ID | Requirement | Canary today |
|---|---|---|
| LO-V1 | Before a rule acts without the owner, it is replayed on the owner's own option trades and compared with what the owner did, net of spread and fees, every price marked as recorded or modelled. | built and installed: `canary edge replay` (read-only) replays sets A-D and each rule alone, recorded and modelled prices apart. Missing: overlay runs (the owner's exits kept, the rule acting when it fires first) and parameter ranges |

Underlying history for the replay (2026-10-25 16:34 CET, at the owner's
request through the orchestrating session). A replay must not change its
answer because history is missing, and must give the same answer later
without the gateway. Sources, in order: the replay's own retained bars
(state document `edge_replay_history`, one per underlying, the union of what
earlier runs read, never evicted); the mandate's decision-grade closes
(retained and never evicted, but only names held now, the newest 320
sessions, closes without a high or low); the market-history chart cache
(high and low, but a display cache that evicts its oldest series at 750);
the broker only when these do not span a name's days. Neither existing
store was extended: the mandate series is a rolling window its verdict
judges freshness on, and eviction is what bounds the chart cache. A name
whose history still does not span the statement days its options were held
(four calendar days of slack at either end) leaves every run, set A
included, and is listed with the reason (`gateway_offline`,
`not_retained`, `instrument_unavailable`, `read_failed`,
`history_fetching`) and the owner's P/L on it; the result is marked
`incomplete`. Broker reads start together and the replay waits for them at
most 25 s and half of what remains of its 60 s budget; a read still running
then leaves its name out as `history_fetching`, goes on for up to four
minutes on its own and keeps the bars for the next run (2026-10-25).
The mandate's closes are read through its identity-bound keys
(`mandate_history_identity.go`): the key of the contract the replay asks
(stock, or index), a legacy symbol-only document only where its contract
matches, and for a held stock, whose key binds the position's contract ID,
the series the mandate has loaded when they name one contract (2026-10-25). Index underlyings (SPX, NDX,
RUT, VIX) are read as `IND` with the market tape's identity. A bar without a
high and low enters the stock trail's ATR as the close-only true range and
is counted (`close_only_range_days`); days with under 21 sessions are counted
as `trail_unmeasured_days`. A statement day on which the underlying's
exchange held no session by Canary's official exchange calendar
(`internal/marketcal`, for USD, GBP, JPY and HKD names; its coverage starts
2026-01-01) and which has no bar carries the previous close and option mark,
acts on no rule and is counted (`closed_session_days`); an open session
without a bar stays untestable (2026-10-25). Code: `internal/daemon/replay_history.go`,
`internal/edge/exit_replay.go`.

## Findings (2026-10-24 and 25)

Replay of the owner's long options, 2025-11-25 to 2026-10-23, recorded
prices, Canary 4dcec757 (holiday sessions carried, VIX read as an index;
earlier runs kept the owner's outcome after holidays and flattered the
owner). The owner made 139k EUR with option value peaking at 89% of NLV.
Single-rule runs replace all the owner's exits.

- LO-H1 trim to cap: −17.8k vs the owner with peak option value 76% of NLV.
  The only rule that stays within the book. Candidate.
- LO-H2 recover cost: −3.8k, but only by holding up to 175% of NLV. Test
  beside the owner, not instead.
- LO-T1 expiry close: helped where it fired. Candidate.
- LO-P2 premium loss exit: costs most of all (−304k replacing the owner); an
  out-of-sample backtest beside the owner (on the earlier build) found no
  setting of 40–90% of cost, or from the high, that wins in both periods. The
  requirement stands; refine the mechanism, likely toward LO-P1.
- LO-H4 stock trail: best value (2.5 × ATR20) was a spike that failed out of
  sample. Unproven.
- Next: rerun the overlay backtest on this build before deciding any rule.
