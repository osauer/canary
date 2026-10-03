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
