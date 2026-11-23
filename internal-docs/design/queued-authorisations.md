# Queued authorisations

Added 2026-09-28 18:52 CEST (owner decisions #26–#31, 2026-09-28). A queued
authorisation carries one owner-signed reduction to the next regular session.
Canary holds it and sends it itself inside a one-hour window after the open,
after every gate a manual submit passes. The clock only chooses when to send;
the owner's signature is the authority. If the Mini, Canary or the Gateway is
down, nothing sends.

| Operation | CLI | RPC |
| --- | --- | --- |
| Prepare | `canary proposals queue prepare KEY REVISION --json [--quantity N]` | `trade.proposals.queue_prepare` |
| Arm | `canary proposals queue arm --stdin [--json]` (one record, or an exit plan's records together) | `trade.proposals.queue_arm` |
| Cancel | `canary proposals queue cancel QUEUE_ID [--prepared-only]` or `--all` | `trade.proposals.queue_cancel` |
| List | `canary proposals queue list [--live] [--json]` | `trade.proposals.queue_list` |
| Status | `canary proposals queue status QUEUE_ID [--json]` | `trade.proposals.queue_status` |

None of these methods has an MCP surface; agent tools stay read-only.

## Owner orders (version 2)

Owner decision 2026-10-10 21:37 CEST: queue Add, Buy, Reduce and Close as exact
stock/single-leg option orders; price them at the send-time midpoint within an
approved maximum buy price or minimum sell price. Stock trailing stops cover
the whole position, growing and shrinking without resetting or loosening the
broker trail. This is an explicit, versioned owner mandate, not additional
authority for a rule, schedule or agent. Existing v1 records keep their terms.

Prepare uses `proposals queue prepare --stdin --json` with `owner_order: true`,
account/mode and either an exact positive-ID `order` or proposal key/revision.
`worst_price` is the approved cap/floor; `full_position` requests the stock-stop
mandate. The v2 `owner_order` object binds type, trail, trigger, trading hours,
Adaptive priority and full-position/Add flags. Desk and the companion understand
these fields before signing the exact bytes. Unknown fields remain a refusal.

Ordinary v2 orders have exact quantity, midpoint_bounded pricing, Adaptive Normal,
DAY duration, regular hours and the existing exchange opening offset/spread/send
window. They wait while outside the cap/floor and expire unsent at the window's
end. Admission runs under the broker write lock and rechecks current cash,
positions, policy, order inventory and account evidence. Future queues do not
reserve cash or mutually block each other. A request-scoped admission exempts
only unattempted future queues; attempted, uncertain and actual orders remain
commitments. Manual Add has no such exemption. The first-byte check revalidates
the same queue, exact quantity/cap, cancellation and evidence generations.

Full-position stock stops have no queue expiry and no artificial opening delay.
They remain queued until broker acceptance, owner cancellation or a confirmed
flat position. Their reviewed DAY/GTC duration applies at the broker and is
independent of queue lifetime. Fresh position size is used before sending.
After acceptance, the existing protective-stop guard may resize only this signed
mandate's exact broker order. It preserves trigger, hours, TIF, trail distance,
limit offset and the broker's current ratchet. Total quantity includes prior
fills plus the remaining position. Missing identity/ratchet evidence holds work.
Confirmed flat ends the mandate permanently, even if the instrument reopens.

Lost/ambiguous sends retain their order identity and wait for reconciliation;
they are never retried merely because the broker is silent or reports an error.
Only explicit definitely-unsent evidence permits another attempt. Later broker
acknowledgement takes precedence over an earlier error. Queue state is not proof
of active protection or a fill. Existing arm signatures and gated write path
remain unchanged, and no MCP write surface is added.

The corresponding immediate Desk order uses the explicit preview limit with
`--adaptive-priority Normal` on `order preview` or `add preview`. The optional
`adaptive_priority` RPC field is part of the signed draft, supports only a
SMART-routed stock or ETF limit and known priorities, and retains all normal
owner execution gates; an option, a combination or a listing on its own
exchange goes as a plain limit, and asking for Adaptive on one is refused
before the broker (2026-11-22 23:23 CET: options were admitted here and then
refused by the broker layer). Omitting it preserves existing plain-limit CLI
behavior.

## Exit plans (version 3)

An exit plan is the owner's signed exit for one held long call: an optional
first sale of part of the line once the session has been open for the plan's
wait, and a trail on the underlying stock that sells what is still held when
the stock gives back to its line. The broker sees only plain limit sells
(LMT, DAY, regular hours).

Prepare takes `exit_plan` with `owner_order: true`, account and mode:

```json
"exit_plan": {
  "contract": { "con_id": 1, "symbol": "SYNA", "sec_type": "OPT", "...": "the exact option" },
  "wait_minutes": 20,
  "first_sale": { "quantity": 10, "min_price": 4.20 },
  "trail": { "trail_percent": 3.0, "initial_stop_price": 49.50 }
}
```

It cannot be combined with `order`, `key` or `full_position`. `trail` is
required; `first_sale` is optional, at least 1 and below the held quantity.
`first_sale.min_price` is optional: left out or zero, the first sale has no
minimum, its terms carry no `worst_price`, and it sells at the mid inside
the option spread limit, as the owner may sign before any option price exists.
`wait_minutes` is at least the market's opening offset and at most 120; it
is a term of this plan only. While the session is open past the wait, a
first sale is refused (`queue_not_offered`); a trail-only plan starts at
once. A put is refused (`queue_right_unsupported`): the line sells as the
stock falls, which protects a call and would sell a put at its best.

The result carries `plan`: `plan_id`, `plan_digest`, `queues` and
`queued_refs`, first sale then trail; `queue` and `queued_ref` stay empty.
`plan_digest` is the lowercase hex SHA-256 of the records' `terms_digest`
values joined in that order by one newline.

### Two records

Both records are version 3 and carry `owner_order.plan` (`plan_id`, `role`,
`wait_minutes`). Each stays a single-send record, so the executor's states,
holds, recovery and events apply to it unchanged.

- **First sale:** a midpoint owner order as in version 2: `midpoint_bounded`,
  plain limit (an option carries no Adaptive), `worst_price` the minimum (omitted when there is none),
  `max_quantity` the first-sale quantity, a one-hour window from the open
  plus the wait. Its minimum not met within the hour, it expires unsent.
- **Trail:** style `stock_trail`, no `worst_price`, `max_quantity` and
  `position_quantity` the whole line at signing, the option spread limit of
  the protection policy, a window from the open plus the wait (or now) to the
  earlier of the stock's and the option's regular close, and
  `owner_order.stock_trail`: the resolved stock contract, the trail percent
  (above 0, at most 50), the starting stop and the stock price read at
  prepare (`reference_price`, for review only; 0 when none was read).

Prepare stores both records in one update. Arm takes `queued_refs` and
`plan_digest` in place of `queued_ref` and `terms_digest` and arms every
record of the plan in one update, or none: part of a plan, a wrong digest
or the single-record form is refused (`queued_plan_incomplete`,
`queued_terms_mismatch`, `queued_plan_arm_required`). Cancel is per record;
cancelling one leaves the other as it is.

Records of one plan may share the contract and side. Everything else stays
one live intent per contract and side: another plan, a version-2 order or a
proposal for that option is refused beside them (`queued_intent_exists`).
A plan commits no cash, so it never holds a stock Add's sizing.

### The trail

Inside its window the executor reads the stock at least every ten seconds,
running only the plans' records between proposal refreshes; with no trail in
its window nothing extra runs.

- **Price.** The midpoint of a fresh, live, two-sided quote whose spread is
  within the protection policy's stock spread limit. Fresh is the test the
  bounded limit prices by: a live feed in an open session, with a bid and an
  ask both received on the read's own request after it began (one read waits
  at most three seconds), so the quote is at most seconds old. A stale, delayed,
  one-sided or missing quote, a broker link that is down, or an active halt
  or limit-up-limit-down pause on the stock leaves the trail unwatched
  (hold `stock_quote_unusable`): it never fires on such a price, nor because
  a price is missing. A usable read resumes it.
- **Line.** The first usable price seeds the high;
  `stop = max(initial_stop_price, high × (1 − trail_percent/100))`, to four
  decimals, and the stop never falls. The high and the stop are written to
  the record whenever the stop rises (`queued_auth.stop_raised`), so a restart
  resumes from them.
- **Fire.** A read at or below the line, confirmed by a later read at or
  below it at least five seconds after it by the executor's clock, fires the
  trail (a stock resting under the line with an unchanged quote confirms;
  the quote's own time decides only whether a read is fresh)
  (`queued_auth.triggered`); any read above the line, or an unusable one,
  in between starts over, so one bad print never sells. The trigger is
  latched with its time and price and the trail keeps trying to sell until
  its window ends, even if the stock recovers.
- **First sale.** Firing withdraws an unsent first sale in the same store
  update (`plan_stop_triggered`). A first sale still working at the broker
  is cancelled through the daemon's cancel path under a grant that names
  that order alone (`queued_auth.first_sale_cancel`, repeated every 30
  seconds while it works); the trail holds as `plan_first_sale_cancelling`
  until the order journal shows it ended. A first sale never stages once its
  trail has fired, and a trail never stages beside a live or working first
  sale. While a read at or below the line awaits confirmation, the first
  sale waits that cycle.
- **Sale.** What is held when the trail sells, never more than its maximum
  less what the trail has already sold, through the ordinary owner path: a
  bounded limit priced from the option's quote, with no minimum price, stated as such to the pricer (a zero worst price alone is
  still refused), inside the option spread limit as signed, from a fresh,
  live, two-sided quote. The first sale stays at the mid. A fired trail
  works its price down: the first attempt at the mid, the second halfway to
  the bid, the third and every later one at the bid. A step works 10 seconds
  unfilled before the next; at the bid the 30-second re-price applies. A
  step that would send the ended attempt's limit again on an unchanged quote
  is passed over at once (`queued_auth.sale_step_skipped`), so a market one
  tick wide goes from the mid straight to the bid. Worst case, about 20
  seconds from the first attempt to the bid. The step is journaled with each
  send and shown as `sale_step` (`mid`, `half`, `bid`). Every hold the
  queue knows applies: freeze, paused configuration, broker link, session,
  spread, quote, a working order on the contract and the order caps (a sale
  that fails the delta-reducing exemption holds and retries). None cancels
  the trail inside its window. A protection policy changed since signing
  holds a trail that has not fired, which then cannot fire; a fired trail
  sells on its signed terms. A closed line ends the plan (`position_changed`;
  `filled` once the trail has sold). Neither the drawdown brake nor a
  sell-only state holds a sale that only closes or reduces the line.
- **Re-price.** A sale still working, not fully filled, when its step has
  run (10 seconds below the bid, 30 at it) is cancelled through the same
  granted cancel path (`queued_auth.sale_cancel`, with its limit and quote);
  once the order journal shows it ended, the trail sends what is still held
  at the next step's price from a fresh quote, until the line is sold or the
  window ends. Partial fills count toward what it sold. One trail order works at a time, never beyond the held line.
  A broker reject or a broker-side cancel returns the trail to fired
  (`trail_sale_ended`, with the broker's words) and it sends again next
  cycle. A send whose outcome is unclear is held for reconciliation
  (`send_outcome_unclear`, or `trail_sale_unconfirmed` once the broker shows
  it gone without a recorded end) and never resent.
  Worst case: one cancel, one WhatIf and one order every 30 seconds, about
  2,300 broker messages over a 6.5-hour afternoon. The price stays bounded by
  the signed spread limit and the fresh, live, two-sided quote each attempt
  needs.
- **End.** Untriggered at its window's end, the trail expires
  (`window_ended`) and nothing is sent.

A stress open (the latched regime stage reads confirmed stress when the
plan is prepared) moves only the first sale, a discretionary sale: it starts
at the later of the plan's wait and the stress open's 30 minutes. The trail
is a protective exit and starts at the plan's own wait.

Each trail record's `queue list` and `queue status` view carries `watch`:
`state` (`waiting` before its window, `watching` while a usable read is at
most 30 seconds old, `unwatched` otherwise, `triggered` once fired), the
last stock price and its time, the high, the stop, the trigger's time and
price, the sale's attempts, price step, working limit and contracts sold
(`sale_attempts`, `sale_step`, `sale_limit`, `sale_filled`), and the reason it is
unwatched, or why a fired trail has not sold yet.

## Version 1 terms

Only governor, theta and issuer-trim rows are queueable: single-contract DAY
limit reductions with an exact contract id. Readiness must offer the queue,
with the session closed (`market_closed`) or open for less than its offset
(`opening_window`). Loss exits need a live bid and are never queued. Prepare
also refuses while proposal submit is disabled, while the row carries a
blocker, and while a pre-authorised record for the row or its contract and side
waits or submits.

Prepare fixes the terms and sends nothing. It reads no quote; the price is set
when the order is sent. The terms are:

- identity: queue ID (also the correlation ID), account and mode, key, bucket,
  and the revision the owner reviewed (audit only);
- order: the exact contract, side and position effect, the signed maximum
  quantity (the row's quantity, or lower with `--quantity`), and the position
  the owner saw;
- price: style `bounded_limit`, concession 0.5 (halfway from the mid to the
  bid, or to the ask for a buy), the worst price, the reference mark with its
  time, and the spread limit;
- worst price: the row's mark moved 25% against the order, rounded inward on
  the tick grid;
- spread limit: theta hygiene's own for its options, the option trail's
  (default 25%) for other options, and the stock trail's (default 2%) for
  stocks;
- time: market and session date from the embedded calendar, not_before (the
  next regular open plus 15 minutes for options or 5 for stocks; 30 for
  options at a stress open, while the latched regime stage in force reads
  confirmed stress, owner decision 2026-09-30 12:35 CEST; every queueable
  bucket is discretionary-scale, so each takes it), not_after
  (one hour later, never past that session's close), TIF DAY, and the arm
  deadline (ten minutes after prepare);
- revalidation: the row-terms digest (bucket, contract and side, effect, order
  type, TIF) and the effective protection-policy and Rulebook fingerprints.

`terms_digest` is the lowercase hex SHA-256 of the terms' JSON as Canary serves
it; every time in the terms is UTC, so a stored record re-encodes to the same
bytes. The prepare result adds a private `queued_ref`
(`canaryqa1.<queue id>.<secret>`). Only its hash is stored. Keep it out of
browser payloads, URLs, logs and argv. A newer preparation for the same
contract and side replaces an unarmed one, which never carried authority.

Desk and the companion refuse terms that name a field they do not know, at
every level: the terms, the contract and the fingerprints (added 2026-09-28 20:30 CEST).
The owner signs every byte, so a field the review does not show is never
signed. Adding a field to `QueuedAuthTerms` is therefore a contract change:
Desk and the companion learn it first, or Desk refuses the preparation with
`queue_terms_unknown` and nothing is queued.

## Arm

Desk arms a record once the owner has signed the digest (Desk phase 3: the
companion signs `desk-companion-queue-v1`). Arm reads one JSON object on
standard input: `queued_ref`, `terms_digest`, `desk_action_id`, `credential`
and `envelope`. Canary recomputes the digest from its stored terms and refuses
any of these:

- a digest mismatch;
- a missing or forged reference;
- a record that is no longer prepared;
- an arm after the deadline, which also expires the record;
- a different account or mode;
- proposal submit disabled;
- another live intent for the contract and side, or a queued order for them
  still working at the broker (`queued_order_working`);
- a record this build cannot fully read (below);
- a pre-authorised record for the row or its contract and side, created since
  prepare (`automatic_submission_pending`). This is checked before the
  queued-store update, never inside it, so the two stores never lock in
  opposite orders.

The envelope is kept for audit only: Canary does not verify the owner's
signature. Desk verifies it (the companion's P-256 signature or the passkey)
before it arms, and the private `queued_ref`, which only Desk holds, is the
capability that arms. Desk is therefore a trusted relay. A process that can
read Desk's private store could arm a prepared record inside its ten-minute
window without the owner's signature, though only the terms Canary itself
prepared and stored. This is the same-user residual risk as a direct call to
Canary today; Canary verifying the signature is listed under Not in v1.

Arm also records the hand orders working at that moment as the settling
baseline. Once armed, the terms are immutable.

## Execution

The executor runs in the proposal loop after the pre-authorised cycle. The loop
also wakes at the next send-window start, arm deadline or window end.

0. **Readable.** Before anything else, the executor ends every record this
   build cannot fully read (`record_unreadable`, added 2026-09-28 21:32 CEST). That covers a
   field, version, style or state it does not know, and terms that no longer
   hash to their digest. The store loads each record strictly, so one such
   record never stops the rest. An unsent record is cancelled; one caught
   sending, sent or in an unknown state fails, for the owner to confirm
   against the order journal. None is sent. The terms are read again at
   every send and when the intent is staged.
1. **Window.** Nothing happens before not_before. Once not_after passes, a
   waiting record expires (`window_ended`), and a prepared record expires at
   its arm deadline (`not_armed`). The window is checked again when the
   intent is staged, so a send whose preview ran past not_after never goes.
2. **Hold.** A record that the send can wait out is held and journaled once
   per hold code:
   - `market_closed`, `trading_frozen`, `halted`, `quote_unusable` and
     `spread_too_wide`;
   - `broker_unavailable`: the link is down, the scope is not concrete, the
     order inventory or WhatIf went unanswered, or a transient preview stage
     failed;
   - `hand_order_working`: an order placed or modified outside Canary's gate
     since the arm, or a same-side order working for the contract;
   - `worst_price`: the bounded limit would pass the worst price;
   - `config_automation_paused`: config.toml runs `[trading]`, `[auto_trade]`
     or `[rulebook]` on Canary's defaults, as for pre-authorised submission
     (owner decision #42).

   A failed refresh holds the send even when an older snapshot is at hand.
3. **Revalidate by key, not revision.** A fresh refresh must still carry the
   key, with the same row-terms digest, the same effective policy and Rulebook
   fingerprints, the same position and the same account and mode. Snapshot
   revision churn at the open never cancels.
4. **Cancel.** Anything else the send cannot wait out cancels the record under
   its own reason code: `position_changed`, `row_gone`, `row_terms_changed`,
   `policy_changed`, `account_changed`, `fast_path_disabled`,
   `whatif_refused` (a rejected WhatIf; an unanswered one holds), or the
   blocker's code. A draft that cannot sit inside the signed contract, side,
   maximum or worst price cancels as `send_refused`. The owner can cancel too
   (`owner_cancelled`). Cancelling only withdraws authority, so any origin may
   ask, and the origin is recorded. A cancel that arrives while the order is
   being placed is kept (`cancel_requested`): an attempt that proves unsent
   ends cancelled and is never retried. Cancel-all lists such records, and
   sent orders still working, under `in_flight`. With `--prepared-only`
(`prepared_only`, added 2026-09-28 22:19 CEST) a cancel withdraws a record only while it is
still prepared: the owner closing an unconfirmed review ("Not now", owner
decision #43) never withdraws a queue confirmed meanwhile
(`queued_not_prepared`). The list names the account and mode Canary serves now
(`account_id`, `account_mode`, empty while unscoped), so a reader keeps that
scope's records.
5. **Send.** The executor sends through the ordinary proposal submit path,
   under `brokerWriteMu` and a grant for the one record, with origin
   `daemon-owner-queued`:
   - Quantity is the signed maximum or the row's current quantity, whichever
     is smaller. A working reducing order blocks the row, so it never adds to
     one.
   - Preview prices a `bounded-limit` draft from the live quote. It needs a
     fresh, live, two-sided quote with the spread within the limit. The limit
     is the concession's way from the mid, on the tick grid, inside the
     spread, never past the worst price.
   - Reduce-only is checked on the row and on the preview, and the draft must
     match the signed contract, side, maximum and worst price.
6. **Intent before send.** The intent (`sending`, preview token ID, limit,
   quantity and the quote) is persisted before the broker call. Only the
   attempt whose token the intent names may send.
7. **Outcome.**
   - An accepted send is `sent`, flagged `late` when more than two minutes
     after not_before.
   - A refusal before the first frame (the freeze, or no journal trace) holds
     the record again, with its spent token proven unsent, or ends it
     cancelled if the owner asked meanwhile.
   - Anything else fails as `send_outcome_unclear` and is never resent.

Sent orders are followed in the order journal:

- fill progress while the order works;
- `filled`, `partially_filled` or `expired_unfilled` once it ends;
- `failed` with `fill_unconfirmed` when the broker reconciled the order as
  gone without a recorded end: its fill is not known, so it is never called
  unfilled.

A cancel that arrived while the order was being placed, after it reached the
broker, leaves the record `sent` with `cancel_requested`; the executor warns
that only a broker cancel (`canary order cancel <order_ref>`) stops it now.

After a restart, the order journal resolves a record left in `sending`. The
journal stages each attempt before the first frame, so:

- a journaled send is `sent`;
- a staged attempt without an outcome is `failed` as unclear;
- no trace at all returns the record to armed, for a late send inside the
  window after full revalidation, or cancelled if the owner asked meanwhile.

The same resolution applies, without a restart, to a record this process left
in `sending` because its outcome write failed, once it is more than a minute
past the send timeout. The executor sends synchronously, so nothing is in
flight between cycles.

The write gate accepts `daemon-owner-queued` only while the executor holds the
grant for a record that is still a live intent. The grant is set and cleared
under `brokerWriteMu`, which every RPC write also takes, so no caller can ride
on it. Every other gate applies as for a human write, through to the wire
guard.

## One live intent per contract and side

An armed, held or sending record is a live intent. `liveIntentFor` reports it
to the netting (proposal_same_contract.go): every other row for the contract
and side blocks with `existing_reduction_intent`. A second queue for them is
refused (`queued_intent_exists`), as is a queue while a queued order for them
still works (`queued_order_working`). Once the order works at the broker, the
working-order netting takes over. Records of another account or mode never
block.

Row generation exempts the intent's own key, so the executor's revalidation
still finds its row. The submit gates do not (review #41, 2026-09-28 21:32 CEST): a preview, a
prepared submit or a submit of any row for the contract and side, the queued
row's own key included, is refused with `queued_intent_exists`. The one
exception is the executor sending that very record under its grant. The
pre-authorised scheduler creates no record for such a row either.

Each served row carries `queued` (`queue_id`, `key`, `state`, `not_before`,
`not_after`) while a record for its contract and side, in the snapshot's
account and mode, is armed, held or sending, or sent until its order resolves.
Desk and the companion keep such rows out of approvals. Such a row is counted
queued, never ready to act (`counts.queued`, added 2026-09-28 21:50 CEST): the queued row leaves
`actionable`, the rows it holds back leave the blocked remainder, and the brief
says "N queued for the open" without raising attention. Its readiness is never
`queueable`, and a prepare for it names the queue (`queued_intent_exists`, or
`queued_order_working` once sent) rather than the session.

## Pre-authorised due time

A pre-authorised record whose order prices off the regular session (a patient
limit, or a trail without an initial stop) submits no earlier than the open
plus the opening offset (the stress open's 30 minutes for a budget-reduction
options row while the regime reads confirmed stress; trailing stops, option
profit trails and option loss exits keep 15, a stop is a stop; a cash sweep
row waits for its bill's own session and five minutes after it opens). Its due time
is max(notice + veto window, open + offset). The bound is set when the record
is created. A record found due while
its session is closed, or still inside the opening window, is rescheduled
(event `rescheduled`) instead of being attempted and failing. Seeded stock
trails keep their plain window.

## Events

Each transition appends a `trade_proposal_queued_event` in the same daemon.db
transaction as the record document. Types are `queued_auth.prepared`, `armed`,
`held`, `resumed`, `sending`, `sent`, `filled`, `partially_filled`,
`expired_unfilled`, `cancelled`, `expired`, `failed`, `recovered`,
`cancel_requested`, and for an exit plan's trail `stop_raised`, `triggered`,
`first_sale_cancel` and `sale_cancel`; the first two carry the stock price,
high and stop. `signed` is Desk's event.

An event carries:

- correlation and transition: `correlation_id`, `queue_id`,
  `desk_action_id`, `seq`, `at`, `state_from`/`state_to`, `reason_code`,
  `reason`, `origin` and `credential`;
- terms: `key`, `revision_at_queue`, `row_terms_digest`, `terms_digest`,
  `bucket`, `con_id`, `side`, `max_qty`, `qty_sent`, `worst_price`, `limit`
  and `not_before`/`not_after`;
- the quote at send;
- broker outcome: `preview_token_id`, `order_ref`, `perm_id`, `fill_qty` and
  `avg_price`;
- liveness: `daemon_started_at` and `executor_tick_at`.

An event never carries the reference, its hash or the envelope. Final records
are kept for seven days.

`events.jsonl` gains `queue_prepare`, `queue_arm`, `queue_cancel` and
`queue_execute` lines, with the queue ID in `ids.queue`. Outcomes are
`queued`, `armed`, `cancelled`, `held`, `sent`, `expired`, `failed`, `filled`,
`partially_filled` and `expired_unfilled`.

## Not in v1

- goodAfterTime (decision #31);
- IBKR algo styles;
- an exact typed limit;
- Canary verifying the companion signature itself;
- status rows and restart checks (phase 4).

Desk phase 3 owns the owner's sheet, the digest and signature, notices and the
MANUAL switch's "Cancel all queued" (`cancel --all`).

Synthetic tests in both builds cover the following:

- the 15:19 CEST fixture;
- the digest, the reference, the deadline and scope at arm;
- one intent per contract and side;
- cancel and expiry;
- holds and cancels by cause, with revision churn still sending;
- the signed maximum;
- exactly one order under concurrent ticks;
- a lost reply, a freeze at the wire, and restarts in `sending`;
- the origin gate;
- price and WhatIf outcomes;
- fill following;
- the calendar across the 26–30 October 2026 week, Thanksgiving, Christmas and
  a weekend;
- the pre-authorised due time;
- a bounded-limit property test.

Mutating any of these guarantees fails the tests.
