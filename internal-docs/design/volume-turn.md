# Intraday volume-turn observation

Updated: 2026-10-04 11:57 CEST (historical-request budget, coverage instrument, resolution reuse)

`canary setups evaluate --spec /private/path/playbook.json --symbol SYNTH --con-id 17 --json`
evaluates one owner-selected US stock without changing settings, risk policy,
notifications or broker orders. `--spec -` reads JSON from stdin.
`--at 2026-09-30T10:20:00-04:00` reconstructs a past clock, at most one year ago,
using history acquired now; it never certifies original availability.

```json
{"version":1,"template":"volume_turn_v1","revision":"owner-v1","spike_multiple":3,"response_bars":2,"baseline_sessions":20}
```

The fixed template compares completed five-minute regular-session trade volume
with the mean of the same slot in 20 previous calendar-comparable sessions.
The template/version, 3x threshold, two response bars and 20-session baseline
are defaults; revision is mandatory. The threshold is bounded to 1–100 and the
response window to 1–6 bars. These are unvalidated observation parameters.

A spike confirms when that bar or a following bar in the response window closes
at least as high as its preceding completed close. Opening prices never fill a
missing preceding close; an opening-bar spike can first confirm on the next
completed bar. Preserve spike and first-confirmation end times. A subsequent
decline invalidates that event permanently; a new spike is a new observation.
The latest spike is evaluated. Outside a known regular session, missing bars,
incomplete baselines and stale data produce an unavailable result.

The pure `internal/setups` evaluator receives a frozen input and clock. The
daemon resolves exact identity, reads calendar windows and collects RTH TRADES
bars in chunks no longer than seven days using the shared historical pacing,
cancellation and broker-session checks. Bar timestamps from IBKR are interval
starts; only `start + 5 minutes <= decision clock` is eligible. Each expected
slot must exist; no zero-fill or inference from an absent trade is permitted.
An early-close session requires comparable early-close history; insufficient
embedded calendar coverage stays unavailable. Collection is capped at 120
calendar days and 2,000 bars; it does not infer missing holiday calendars.

Baseline profiles and current-session reads live in a disposable in-memory
cache that restart discards; no durable bar store or candidate ledger is
introduced. An exact-contract profile may survive a broker reconnect; this is
retained historical evidence, not a fresh broker-authority claim. Each prior
session is kept with the time its read completed, and `baseline_observed_at`
is the oldest acquisition still in use: every baseline bar was read at or
after it. Corrections arriving after a session's read are not discovered
while that session stays in the window.

The result supplies the current session chart, the selected spike's 20 baseline
observations, source clocks and an input hash. Source units are explicitly IBKR
historical trade volume; the ratio does not claim signed buying pressure, and
historical corrections cannot become evidence known at the original decision.

Added 2026-10-06 21:40 CEST so a reader can chart and inspect the evidence
without recalculating it; none of it changes the rule or its state:

- `bars[].trades` is IBKR's trade count for the bar (the TRADES bar count the
  decoder already reads). A count IBKR did not report, or zero with volume, is
  absent rather than zero.
- `usual[]` is every slot of today's session with its 20-session mean `volume`
  and, when all 20 sessions reported counts, mean `trades`. It runs ahead of the
  latest bar because it comes from prior sessions only.
- `features.trade_size` and `trade_size_usual` are total volume over total trades
  for the latest hour of completed bars (`trade_size_bars`, at most 12) and for
  the same slots' usual. It is a measurement, not a claim about who traded.
- `trace[]` is the rule's state, reason, spike and first confirmation as they
  read at a completed bar's close, reconstructed from this evaluation's bars
  without acquisition deadlines. Only bars where the reading changed are listed;
  each step holds until the next, so a quiet day is one step. It shows how the
  state developed; it never certifies that a later-corrected bar was known then.
- `recent_sessions[]` is the latest four baseline sessions in hourly bars from
  the open, aggregated from bars the profile already holds: no extra broker read.

Trade counts enter the input hash with the bars they belong to.

The next expected bar plus a 60-second acquisition grace bounds validity, never
beyond the close of the first bar outside the response window. Missed refreshes
expire actionability. `setup_match` is nullable and independent of policy;
`policy_checked` is always false. A historical reconstruction has historical
deadlines and must never enter the current delivery path. `first_available_at`
names this acquisition, not a backdated first live detection; Desk retains the
first delivery episode separately. Stable reason codes accompany each state.

The module-root `Client.EvaluateSetup` provides typed read-only access. MCP has
no setup tool in this first slice. The CLI and typed client do not create a
playbook, enqueue work, choose an option or authorize an order. Desk owns those
presentation and owner-setting records; existing Canary authority gates remain
binding when any later exact order is prepared.

## Historical-request budget

IBKR's historical bucket in `pkg/ibkr/ratelimiter.go` holds 60 requests per 10
minutes, shared with every other historical reader in the daemon. Setup
evaluation spends it as follows.

- Current session: a live evaluation reads the current session's bars at most
  once per contract per completed bar. The read is reused until
  `now >= latest completed bar end + 5 minutes`; if the farm had not yet
  published a bar that should exist, the next evaluation reads again. A reused
  read reproduces the evaluation at its own clocks (`evaluated_at`,
  `observed_at` and the input hash of the evaluation that made the read), so an
  evaluation on retained bars never claims a fresher observation. Reuse needs
  the same broker session and a read no older than the paired baseline.
  Concurrent live evaluations of one contract share one read and its failure.
  Replays (`--at`) always read and never use or feed this memo.
- Baseline: profiles are keyed by exact contract, not by date. A new session
  reuses the held sessions still in its 20-session window, reads only the
  missing ones in ranges of at most seven days (one request each, still under
  the 2,000-bar bound) and drops sessions that left the window. The morning
  roll costs one request per contract; a cold profile costs four or five.
  Complete sessions from a failed or incomplete read are kept.
- Negative memo: an incomplete window or a failed baseline read is remembered
  per contract and session date for 15 minutes; until then evaluations answer
  `unavailable` without a read, then the next one reads only what is still
  missing. The reason names the retry time on the daemon clock, rounded up to
  the minute, for example
  `baseline_history_unavailable: baseline_bars_incomplete (retry after 10:33)`
  or `baseline_history_unavailable: <read error> (retry after 10:33)`. A read
  ended by the caller's cancellation or a broker reconnect is not remembered.
- Contract identity: the exact underlying resolution (one contract-details
  request) is remembered per broker session binding, so a watched contract is
  resolved once per broker session rather than per evaluation; option
  discovery shares it for its underlying. A binding that is no longer current
  resolves again, failures are not remembered, and at most 64 resolutions are
  kept, least recently used first.
- Cold reads are serialized by one gate; a cached profile is read without it.
  The cache holds max(20, distinct contracts evaluated in the live session)
  profiles, at most 40. At that size a new profile evicts the least recently
  used one whose latest session is not the live session (replays, names not
  yet evaluated today) before any live one, and a replay never evicts a live
  profile.

With 20 watched names polled once per completed bar, steady state is 20
requests per five minutes (40 of the 60 per 10 minutes) plus one baseline
request per name on its first evaluation of the day; the first poll after a
restart costs four or five per name and is paced by the bucket.

## Coverage instrument

`canary setups coverage --json [--session YYYY-MM-DD] [--symbol SYMBOL]`
(RPC `setups.coverage`, `Client.SetupCoverage`) reads, per contract and
session: `evaluations`, `history_requests`, `states` (`watching`, `pending`,
`confirmed`, `expired`, or `unavailable:<code>` from the first reason's stable
code), `slots_covered`, `slots_scheduled` (completed-bar slots since the
contract's first evaluation) and `first_evaluated_at`/`last_evaluated_at`.
The session reports `slots_completed`; slot k completes at open + 5k minutes,
and the bar ending at the close completes outside the session, so a regular
session has 77 evaluable slots. Only live evaluations inside a regular
session count. The default session is the newest retained one; `sessions`
lists all of them. Evaluations update memory only; a background writer keeps
one `setups_coverage.session.v1` state document per session in daemon.db for
the newest 30 sessions, so recording never delays an evaluation. It is
operational evidence, not a signal.

## Standard-call discovery

`canary setups options --symbol SYNTH --con-id 17 --json` lists up to 64
current/future expiries; add `--expiry YYYYMMDD` for at most 21 calls around the
underlying price, then `--strike N` to resolve the exact selected call and read
its one quote. Empty listings remain JSON arrays. `truncated` discloses a capped
display window. These are owner-selected reads, not recommendations or order
previews.

Discovery pins the stock ConID and broker session, requests actual security
definitions with cancellation, and accepts only the SMART standard trading
class with multiplier 100. It never falls back to a heuristic strike grid.
Before collection, each matching frame is limited to 128 expiries, 2,048
strikes and 65,536 expiry/strike pairs; the whole request is limited to 32
matching frames and 131,072 pairs. Overflow fails the read without partial
success, before allocating the collector's cross-product maps.
Security-definition expiry/strike combinations still require final exact
contract resolution: the selected call must retain the requested tuple,
standard class/multiplier, local symbol and broker-reported underlying ConID.
Ambiguous or contradictory identities fail visibly.

The top-level `as_of` is this acquisition's receipt. Call rows have status
`not_requested`, with optional bid/ask/source clocks omitted, and the expiry
and strike stages carry no `quote` object: listing does not acquire or release
option quote lines. Exact selection returns the identity and one `quote` for
that call; a missing quote is exactly `{"status":"missing"}`:

```json
{"bid":1.2,"ask":1.35,"as_of":"2026-10-02T15:00:01Z","data_type":"live","status":"quoted"}
```

The daemon reads it once, within 5 seconds, through its exact-contract quote
path: a private positive-ConID line on the discovery's own broker session,
released afterwards, so no shared option line is acquired or cancelled.
`status` is `quoted` only when bid and ask are both finite and positive,
bid <= ask, `as_of` is at most 60 seconds old and `data_type` is a
broker-labelled `live`, `delayed`, `frozen` or `delayed-frozen` mode.
Anything else is `missing` with bid, ask, `as_of` and `data_type` omitted, so
no zero is ever emitted as a price. A live quote is dated by its older side's
receipt; delayed or frozen sides, which IBKR sends without a source time, are
dated by the read, and `data_type` names their age. A failed read never fails
the selection; the outcome and the read's duration are logged at debug level.
The quote is display evidence for choosing a limit: the existing subsequent
Canary preview acquires execution evidence and owner-confirmed order authority
remains mandatory. The typed Go adapter is `Client.DiscoverSetupOptions`; no
MCP tool is exposed.

Why the strike list stays unquoted (updated 2026-10-04 10:54 CEST): the owner
locked on 2026-10-03 that the strike list carries no prices and that the order
preview supplies pricing. Pricing the list would hold up to 21 option
market-data lines, and their wait, on every listing for prices the preview
reads again. On 2026-10-04 the owner made one narrow extension: a single dated
quote for the exact selected call, so a premium is visible before a limit is
typed. The strike list itself stays unquoted.

## Entry markouts

Owner decision 2026-10-04; capture added 2026-10-04 11:58 CEST.
`canary setups markouts --json [--order-ref REF] [--since YYYY-MM-DD] [--symbol SYMBOL]`
and `Client.SetupMarkouts` read the ledger; there is no MCP tool. Every
result has `kind: entry_diagnostic`: a displayed quote marked against an actual
fill. It is not a fill, a paper portfolio, realized profit or accounting, and
it never replaces the existing realized P&L.

**What is scheduled.** Each broker execution journaled for an order Canary
placed through its own preview path (preview token present, no bypass) on a
stock or option schedules two targets in daemon.db, keyed by `order_ref`,
`exec_id` and horizon. Manual TWS orders and other instruments are not
scheduled. `t30` is 30 regular-session minutes after the fill on the exchange
calendar; minutes left at the close carry across breaks, weekends and holidays
to the next open, and a fill outside the session starts counting at the next
open. `next_close` is the scheduled close of the first regular session opening
after the fill (the same day's close for a pre-market fill). Options use the
09:30–16:00 ET single-name session, not the 16:15 index-options calendar.
The fill time is the broker execution time when it carries a zone, otherwise
the journal receipt, named in `fill_time_source`. Quantity, side and price are
the execution's own (`exec_shares`, `exec_side`, `last_fill_price` in the
journal), not the cumulative order fill.

**Durability.** The schedule is a state document with at most 256 pending
targets; scheduling beyond that resolves the oldest as missing
(`backlog_full`). Resolved targets leave it atomically as append-only,
non-decision observations; neither advances the order-event frontier. The
lifecycle callback only offers the fill to a bounded queue, so fill handling
never waits; queue overflow and every start rescan the journal for fills since
`tracking_since`. Fills journaled before tracking began are never scheduled.

**Capture rule.** Five seconds before the target the worker reads the exact
contract through the request-owned exact-session quote read used for
option-exit evidence (never the shared SubscribeOption key path), waiting at most
five seconds for both sides and displayed sizes; one read serves every target
on the same contract and clock, and at most four reads run at once. A target
is `captured` only if the quote is live, two-sided, finite, positive and not
crossed (locked is allowed), its as-of (the older side receipt) is at or before
the target and at most 60 seconds old there, and the displayed size on the
marking side is at least the fill quantity. Sizes are compared as reported by
IBKR; a feed reporting stock size in round lots can only understate depth and
yield `displayed_size_insufficient`, never a false capture. BUY fills are longs
marked at the bid; SELL fills are shorts marked at the ask. `markout` is
(mark − fill price) × signed quantity × multiplier in the contract currency,
BUY positive and SELL negative, so a positive value favours the fill. It
excludes commission; the journal records none, so `commission_status` is
`not_recorded` and no liquidation fee is estimated. Rows keep the full
contract, side and quantity; stock, long-call and other option captures are
never aggregated.

**Missing.** A target is `missing` with the first failing condition and no
prices: `quote_not_two_sided`, `quote_not_positive_finite`, `quote_crossed`,
`quote_not_live`, `quote_time_unavailable`, `quote_after_target`,
`quote_too_old`, `displayed_size_unavailable`, `displayed_size_insufficient`,
`quote_unavailable`, `broker_unavailable`, `capture_window_missed` (no read
before the clock passed, including while the daemon was down),
`scheduled_after_target`, `backlog_full`, `exchange_calendar_unavailable`,
`exchange_calendar_unsupported`, or a fill-identity reason such as
`fill_quantity_unknown`. Missing is never a zero, an estimate, a midpoint or an
underlying return, and a later quote is never backdated to the target. Option
quotes for past minutes cannot be reconstructed, so a missed target stays
missing.
