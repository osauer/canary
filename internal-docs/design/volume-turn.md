# Intraday volume-turn observation

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

At most 20 complete symbol/session profiles remain in a disposable in-memory
cache; restart discards them. No additional durable bar store or candidate
ledger is introduced. Current bars are re-read. An exact-contract historical
profile may survive a broker reconnect within the same process and session date;
this is retained historical evidence, not a fresh broker-authority claim. Its
`baseline_observed_at` stays fixed, and a new daily key causes re-acquisition.
Corrections arriving after that read are not discovered until re-acquisition.

The result supplies the current session chart, the selected spike's 20 baseline
observations, source clocks and an input hash. Source units are explicitly IBKR
historical trade volume; the ratio does not claim signed buying pressure, and
historical corrections cannot become evidence known at the original decision.

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

## Standard-call discovery

`canary setups options --symbol SYNTH --con-id 17 --json` lists up to 64
current/future expiries; add `--expiry YYYYMMDD` for at most 21 calls around the
underlying price, then `--strike N` to resolve the exact selected call. Empty
listings remain JSON arrays. `truncated` discloses a capped display window.
These are owner-selected reads, not recommendations or order previews.

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
`not_requested`, with optional bid/ask/source clocks omitted: discovery does
not acquire or release shared option quote lines. Exact selection returns an
identity; the existing subsequent Canary preview acquires execution evidence
and owner-confirmed order authority remains mandatory. The typed Go adapter
is `Client.DiscoverSetupOptions`; no MCP tool is exposed.

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
option-exit evidence (never the strike-rounding option key), waiting at most
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
