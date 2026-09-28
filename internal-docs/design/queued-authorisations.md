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
| Arm | `canary proposals queue arm --stdin [--json]` | `trade.proposals.queue_arm` |
| Cancel | `canary proposals queue cancel QUEUE_ID` or `--all` | `trade.proposals.queue_cancel` |
| List | `canary proposals queue list [--live] [--json]` | `trade.proposals.queue_list` |
| Status | `canary proposals queue status QUEUE_ID [--json]` | `trade.proposals.queue_status` |

None of these methods has an MCP surface; agent tools stay read-only.

## Terms

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
  next regular open plus 15 minutes for options or 5 for stocks), not_after
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
   sent orders still working, under `in_flight`.
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
- `filled`, `partially_filled` or `expired_unfilled` once it ends.

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
Desk and the companion keep such rows out of approvals.

## Pre-authorised due time

A pre-authorised record whose order prices off the regular session (a patient
limit, or a trail without an initial stop) submits no earlier than the open
plus the opening offset. Its due time is max(notice + veto window, open +
offset). The bound is set when the record is created. A record found due while
its session is closed, or still inside the opening window, is rescheduled
(event `rescheduled`) instead of being attempted and failing. Seeded stock
trails keep their plain window.

## Events

Each transition appends a `trade_proposal_queued_event` in the same daemon.db
transaction as the record document. Types are `queued_auth.prepared`, `armed`,
`held`, `resumed`, `sending`, `sent`, `filled`, `partially_filled`,
`expired_unfilled`, `cancelled`, `expired`, `failed`, `recovered` and
`cancel_requested`. `signed` is Desk's event.

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
