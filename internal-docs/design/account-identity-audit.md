# Account Identity: Audit of the v2.6.0 Blast Radius and Pin-Change Semantics

Updated: 2026-08-02 09:15 CEST
Status: proposed, awaiting decision. This document binds nothing. It is the
finding record for a multi-agent audit run after issue #14, and the input to a
decision about n-account logins. Only the commits listed under *Landed* exist as
code; every finding below them is a claim awaiting a decision.

The findings sections describe the tree as audited on 2026-08-01. Several have
since been fixed — see *Update 2026-08-02* at the end, which is authoritative
where it and an earlier section disagree.

## Why this exists

A user running v2.6.0 against TWS, with one login carrying two unlinked accounts
and `gateway.account` pinned to one of them, reported that `canary rules`
returned input health `account: unavailable` — "account snapshot is missing or
belongs to another broker account" — so all 11 account-dependent rules came back
unknown, while `canary account` kept answering normally.

Three distinct defects produced that, all of them the same conflation: the raw
`msgManagedAccts` value is a **session inventory** (`U1234567,U7654321`), not an
account code, and code that treats it as one either fails closed and silently
disables a surface, or publishes one account's data under another's identity.

That conflation had already been fixed once, for portfolio frames, in `0fab26d`.
It was not swept. This audit asks what else it touches, and what happens to
persisted state when the pin moves — because pins do move: taxable to IRA, paper
to live, an entity migration, or a mistyped pin corrected an hour later.

## Landed

| Commit | Defect |
|---|---|
| `7601128` | `CachedAccountSummary` stamped the snapshot `AccountID` with the aggregate, so every account-scoped consumer failed closed on it |
| `cd6a98d` | `reqAccountSummary` is issued with group `"All"`, so a multi-account login answers with every account's rows; a sibling row was classified as a scope violation and discarded the whole snapshot, failing the one-shot deterministically on every evaluation |
| `39e905f` | `canary status` and the TUI printed the aggregate as the session account, naming a sibling the operator never pinned |
| `c673af4` | every non-option position row was quote-probed as `STK` on its bare symbol, so a treasury symbolled `T` was priced and day-changed against AT&T; the `100`-to-`1` multiplier coercion applied to futures; group base totals counted one of several same-symbol equity rows |
| `ba8f9d8` | an unregistered summary row rebound the session identity to a sibling and blocked every broker write; `AccountSummaryRaw` read the unstamped cache unguarded; `lookupAccountValue` resolved `UnrealizedPnL` to a ledger slice |

`cd6a98d` is the root cause of the reporter's symptom; `7601128` is why the
fallback that masked it was mislabelled. Neither works alone.

## Method and its limits

Two audits, each a fan-out of independent readers over distinct lenses, with an
adversarial refuter on every finding that defaults to *refuted* when unsure.

- 44 hazards filed across 5 lenses on persistence, pin-change semantics,
  runtime-switch readiness, logging, and served DTOs.
- 13 refuted, 31 survived, 0 lost. The survivors dedupe to the 20 issues below.
- A third audit, on the wider v2.6.0 blast radius, and a fourth, on the IBKR
  `$LEDGER` `Account=All` semantics, were still running when this was written.

Limits worth stating. Verification is adversarial reading, not execution — no
finding here was reproduced on a live multi-account login, because none was
available. Severities are the verifier's after correction, not the filer's. The
refuted list is evidence too, and is kept below for that reason.

## What the refuters killed

These were filed and do **not** hold. They are the parts that were designed
carefully, and they should not be re-litigated without new evidence.

- Purge-ledger rows for a former account stay scoped and restorable.
- The account-level `reqPnL` stream does rebind.
- Order-lifecycle callbacks are stamped with the right account, not the current pin.
- `PurgeStatusParams.Account` does not let a client read another account's book.
- The `gateway_account_mismatch` blocker does not leak the way it appeared to.
- `HealthResult.AccountMode` does not contradict the broker scope.
- Account identity is present in the proposal/opportunity revision and the alert
  authority scope, the two fingerprints where it matters.
- No in-flight-work interlock gap in the current (restart-only) pin change.
- The alert episode registry does not strand open episodes across a scope change.
- `TailLastLine` does not copy arbitrary log content into CLI errors.

## Findings

Severity is the verifier's. **Now** means reachable on today's restart-only pin
change; **switch** means it must be fixed before a runtime account selector
ships.

### Wrong number presented as authority

These are the dangerous ones. They do not fail closed — they serve a figure that
looks authoritative and belongs to a different account.

**A1. Capital state is welded to the first account; the report path is unscoped.**
`internal/daemon/risk_capital_state.go` — high, now + switch.
The document key is the constant `"daemon"`; `state.AccountID` is adopted once on
the first accepted observation and never reassigned. `Observe` correctly refuses
a mismatched account — which is precisely why the stale peak survives — but
`Report`/`reportLocked` take no scope and compare that peak against whatever
equity they are handed. After a pin change the drawdown is computed across two
accounts: a fabricated block tier when repinning to a smaller account, a
collapsed one when repinning to a larger. It reaches `canary policy show`, the
SPA risk panel and the brief's capital row. `rpc.CapitalStateReport` carries no
account field, so nothing on the wire discloses it, and there is no rebind path
short of deleting state.
*Fix:* key the capital document by `(account, mode)` the way the alert registry
keys by authority scope. Minimum viable: give `Report` the scope `Observe`
already takes, return `Tier=unknown` with a named reason on mismatch, and add a
bound-account field to the report.

**A2. Retained Flex statements pool across accounts.**
`internal/daemon/recon_engine.go` — high, now.
`mergeRetainedStatements` takes no account and applies no filter, and the
retained-statement directory is flat and never pruned. Any past pin change
leaves the previous account's XML in the merge input permanently. A foreign
deposit shifts `adjusted = equity - flows` and directly decrements
`AdjustedPeakBase` — another account's cash movement moves this account's
drawdown baseline. It also feeds `reconEquityCheck`, the divergence gate for
clean-report auto-extend. Worse than first filed: `equityByDay` is keyed by
calendar day alone over a list sorted newest-first, so on any day two accounts
cover, one row silently wins and the other is dropped.
*Fix:* filter by the pinned account at parse time — `flexstmt.Statement.AccountID`
is already available — and key `equityByDay` by `(accountKey, day)`. Statements
for other accounts should be counted and surfaced as a health note, never
silently merged.

**A3. The app has no account epoch, and the plate mislabels the stale book.**
`internal/app/live/service.go` — high, switch.
On a scope change the account and positions reads fail closed while status and
trading-status do not, and `PollOnce` clone-forwards the prior snapshot. Because
the SPA resolves the displayed account first-non-empty with `trading.account`
ahead of `account.account_id`, the plate flips to the **new** account while the
figures stay the **old** account's.
*Fix:* put an account identity on the live snapshot envelope and on every
account-bearing SSE event, and hard-reset client state when it changes. The
alert store already models exactly this with
`AlertDeliveryEndAuthorityScopeChanged`.

**A4. Opportunity snapshots have no serve-time scope guard.**
`internal/daemon/opportunity_engine.go` — medium, now + switch.
`proposalEngine.Snapshot` has an explicit serve guard, added after paper
proposals surfaced on a live session. `opportunityEngine.Snapshot` has none.
Today the window is the seconds after a reconnect binds a different login; under
a runtime selector that does not force a reconnect it is a full refresh cadence,
two minutes by default. Neither the CLI text nor the SPA panel prints the
snapshot's account, so the mismatch is invisible — and both pass `Show: true`,
so the daemon journals "shown" audit events for the stale rows against the old
account id. Preview and submit are not affected: they always regenerate.
*Fix:* mirror the proposal engine's guard, returning the refusal shell **before**
`appendShownEvents`, plus the twin of `TestProposalSnapshotServeRefusesScopeMismatch`.

**A5. Per-contract daily P&L is cached by `conID` alone.**
`pkg/ibkr/pnl.go` — medium, switch.
Any contract held in both accounts serves the previous account's figure after a
switch.

**A6. Trading guardrails and limits are daemon-global.**
`internal/daemon/platform_settings.go` — medium/low, now + switch.
A notional cap sized for a large taxable account carries unchanged onto a small
IRA, silently re-sizing the guardrail relative to the book it guards. **This one
is a policy decision, not a code decision** — see *Open questions*.

### Read filters weaker than write keys

State is written with the account and read back without it.

**B1. History read paths drop the account predicate.**
`internal/daemon/daemon_state_history.go` — medium, now.
Stress, regime, rules and capital-event history are one blended stream;
`stress.history` and `recon.equity` return rows for every account ever recorded.
Filed three times independently, which is a signal in itself.

**B2. Statement equity projection is written per account, read unfiltered.**
`internal/daemon/corestore/statements.go` — medium, now.

**B3. Proposal outcome marks carry no account.**
`internal/daemon/proposal_outcomes.go` — medium, now.
One account's daily mark suppresses the other's, and the brief's
offered-versus-acted review pools two books.

**B4. Old accounts' order events are folded on every read, forever.**
`internal/daemon/order_read_model.go` — low, now.
Correctness is fine — the scope filter holds — but the work is unbounded and
grows with every account the daemon has ever been pinned to.

**B5. The brief's day-stamp is keyed by kind only.**
`internal/daemon/brief.go` — low, now.
Stamping one account closes the day for every account.

### Identity plumbing

**C1. `HealthResult.Account` reads only `discover.Endpoint`, not the live pin.**
`internal/daemon/handlers.go` — medium, switch.

**C2. The pin is read from an unsynchronised `*config.Resolved` by seven call
sites.** `internal/daemon/broker_scope.go` — medium, switch.
Safe today because the pin only changes across a restart. A runtime selector
makes it a data race.

**C3. `PositionsResult.AccountID` is never populated.**
`internal/cli/purge.go` — medium, now.
The CLI purge book's cross-check is silently disabled: it compares against an
empty string and always agrees.

**C4. The streaming account-summary map is never cleared on a resubscribe.**
`pkg/ibkr/connection.go` — medium, switch.
Only a new socket clears it. Not reachable today, because nothing rebinds the
stream to a different account within one socket — a paper/live switch changes the
port and forces a reconnect. It becomes reachable the moment a same-login sibling
switch exists, which is exactly the selector case. This is also why `7601128`
refuses the cached fallback rather than relabelling it.

### Diagnosability and disclosure

**D1. Account-scoped events log no account.**
`pkg/ibkr/connection.go` — medium, now.
Subscription bind and rebind are entirely unlogged, and the portfolio
scope-conflict latch — the daemon's own multi-account tripwire — names no
account. The defect class the latch exists to detect is undiagnosable from the
log. This is why issue #14 needed a reproduction rather than a log line.
The sibling-drop line already does it right and is the pattern to copy.

**D2. Dropped order callbacks after a pin change are a DEBUG line naming no
account.** `internal/daemon/order_lifecycle.go` — medium, now.

**D3. Scope-mismatch blocker messages print two raw account ids** into CLI, SPA
and MCP output. `internal/daemon/proposal_engine.go` — medium, now.

**D4. The SPA settings meta line renders the pinned account id unmasked**,
bypassing the account-privacy treatment used elsewhere. `web/app/settings.js` —
low, now.

**D5. Three of the four smoke scripts dump the daemon log tail and raw wire
frames to the release console on failure**; the fourth deliberately refuses.
`scripts/release-smoke.sh` — medium, now. The release console is the most likely
place for that output to be copied into a public artifact.

## Design position on n-account logins

Three shapes were considered.

**Aggregate into one virtual account — no.** Margin does not cross unlinked
accounts, so a summed net liquidation corresponds to no real constraint: sizing
at a fraction of the combined equity assumes buying power the receiving account
does not have. Cushion, excess liquidity and base currency are per-account too.
Every order lands *in* one account, so a consolidated view must hand the account
back before it can act — while inviting a decision that is legal in the taxable
account and not in the IRA. Aggregation works only as a labelled read-only total,
never as a decision surface, and here everything is a decision surface.

**One daemon per account — no.** TWS shares the account-updates service across
API clients; a second client's `reqAccountUpdates` displaces the first's. Two
daemons on one gateway *is* the #12/#14 pathology by construction, plus doubled
ports, state directories, services, app pairing and market-data lines.

**Pin one account, switchable at runtime — yes.** `brokerStateScope{Account, Mode}`
already scopes the order journal, purge ledger, proposals, opportunities,
risk-capital state and the snapshot cache, and already fails closed unless it
names one concrete account. The daemon is already a one-account-at-a-time
machine. What is missing is discovery and a switch, not a new model.

The rule the findings above keep pointing at: **the account is part of the
identity of a fact, not a filter applied afterwards.** Anything derived from one
account's data is keyed by `(account, mode)`; then a switch is a no-op on data
and switching back finds the history intact. The alternative — one record with
the account as an attribute — has only two outcomes, and A1 is what both look
like.

Not everything needs identity, and adding it where it does not belong would be
its own mistake:

- **Desk facts, never account-keyed:** watchlist, market data, regime, breadth,
  gamma, calendars, index membership, and risk *policy* — the rulebook's
  thresholds and definitions.
- **Account facts, keyed by `(account, mode)`:** balances, positions, orders,
  purge ledger, proposals, opportunities, capital state, statements, daily P&L.
- **Contested, needing one human decision each:** see below.

The clean line is that policy is desk-scoped and the state policy produces is
account-scoped, which means most of `internal/risk` needs no change at all.

## Sequencing

**Patch — no schema, no new surface.** The three landed fixes, plus D1's
connect-time line naming how many accounts the login carries and which is pinned,
plus splitting the "missing or belongs to another broker account" note into its
four actual causes. This tier cannot create a migration that has to be unwound.

**Before a runtime selector ships.** A1 and A2 first — they are wrong-number
defects that bite on today's restart-only pin change, not just under switching.
Then A3, A4, C1–C4, and the B group. The switch itself then needs: subscription
rebind, cache invalidation, a pin-change event in the ledger, and an
in-flight-work interlock.

**Enforcement, so this does not recur.** Every persisted collection declares
desk-scope or account-scope, checked the way `docgen:env` enforces env-var
documentation. Both times this bug class shipped, it shipped because a new
collection skipped the decision silently.

## Open questions for the operator

1. **Is `trading.freeze` desk-global or per-account?** Today it is global. On a
   multi-account desk, freezing "the desk" and freezing "this account" are
   different intentions, and guessing is not acceptable for a guardrail.
2. **Do notional and position limits follow the account or the desk?** (A6.)
3. **Does a runtime switch require confirmation onto a live account, and is it
   refused while orders are working?** The recommendation is yes to both.
4. **Should `canary account list` show sibling balances** — a read-only awareness
   view, with no risk semantics attached — or stay strictly single-account?

## Update 2026-08-02 — third audit, ledger research, and what landed

Two more audits finished after the sections above were written, and five fixes
landed. Totals across all three audits: **57 hazards filed, 21 refuted, 44
survived**. This section is authoritative where it and an earlier section
disagree.

### The strongest signal in the whole exercise

Five independent lenses, given different briefs, converged on one defect:
`connection.go:2785`. On a multi-account login `c.account` is the aggregate,
which is not concrete, so an unregistered summary row was adopted as the session
identity. The reachable window was not the obvious one — it is the **timeout
path**, where `awaitAccountSummarySnapshot` deregisters while the reply is still
outstanding and the cancel goes out afterward, and `handlers.go:441` runs a 3s
request on every positions read and discards the error.

`accountMismatchesConnected` then read the rebind as a configured-versus-
connected divergence and refused **every broker write for the rest of the socket
generation**. Fixed in `ba8f9d8`.

Two properties of that defect are worth recording, because both were asked and
both needed tracing rather than assumption:

- **It denied writes; it never misrouted one.** `accountMismatchesConnected`
  returns true when the configured account is not concrete, so an *unpinned*
  multi-account login was blocked too. `order_whatif.go:354` does default
  `order.Account` from the poisoned field, but the only poisoning rebind is to a
  sibling — exactly the case the refusal catches first.
- **Any reconnect cleared it.** `c.account` has three writers only, and the
  clear at `connection.go:5048` runs from `resetOrderIDReadiness` before every
  connect attempt. Nothing persisted it.

**Still open after `ba8f9d8`:** the timeout window itself. A late row still
arrives unregistered; it can no longer rebind the identity.

### Why IBKR labels ledger rows `Account=All`

It looked senseless. It is not: **`:ALL` means all accounts as well as all
currencies.** IBKR's Account Summary page states the `$LEDGER:ALL` values are
"summed up values for ALL accounts and currencies", with an example row reading
`Account = All`.

IBKR contradicts itself — the `EClient` class reference describes the same tag as
currencies only, and the cross-account sentence survives only on the page IBKR
marks deprecated. What settles it is a third-party capture from a live
six-account advisor login: plain `$LEDGER` returned 138 ledger rows (six accounts
× 23 fields) while `$LEDGER:ALL` returned 66 — more currencies, fewer than half
the rows, so the account axis collapses. In that same capture plain `$LEDGER`
rows carry **concrete account codes**, so the `All` label belongs to the `:ALL`
variant, not to the ledger family.

This vindicates refusing those rows on a multi-account login (`cd6a98d`): booking
a cross-account sum against the pin overstates its cash and market value by the
siblings' holdings. ib_async filters the same rows away.

Two consequences that are **not** yet acted on:

- **A better fix exists.** `reqAccountUpdatesMulti(reqId, account, modelCode,
  ledgerAndNLV=true)` returns the same per-currency breakdown with the account
  explicit on every row. Canary implements none of it. That is how multi-account
  logins get currency exposure back.
- **A forward-compat hazard.** TWS/Gateway **10.47** adds an API setting that
  prepends `$LEDGER-` to every per-currency key, **default enabled for new
  users**. `extractCurrencyLedger` splits on the last underscore and matches the
  prefix against `currencyLedgerField`, so `$LEDGER-CashBalance_USD` fails the
  match and the row **vanishes with no error**. This will land on new installs as
  they adopt 10.47.

### Corrected status of the findings above

Closed by `c673af4` / `ba8f9d8`: **C4** (the unstamped cache is now refused at
both doors, so nothing clears-on-rebind is needed for correctness), and the
`AccountSummaryRaw` half of the same problem. The identity rebind and the
`UnrealizedPnL` ledger collision were found by the third audit and are closed
too.

Unchanged and still open: **A1** and **A2** — the two wrong-number defects
reachable on today's restart-only pin change — plus **A3**, **A4**, **A5**,
**A6**, the whole **B** and **C** groups except C4, and all of **D**.

### Deliberate gaps, recorded so they do not look fixed

- `rpc.PositionGroup.Stock` still shows one of several same-symbol rows. The
  arithmetic is correct as of `c673af4`; the display is not. Making it a slice
  touches the SPA, CLI and MCP.
- `accountCodeConcrete` remains a free function at roughly 40 call sites. The
  `accountCode` / `managedAccountSet` types introduced in `ba8f9d8` are used only
  where those fixes already edited. Sweeping the rest is its own change and does
  not belong in a correctness release.

## Related

- `internal-docs/design/platform-settings.md` — settings and state authority
- `internal-docs/design/daemon-sqlite-authority.md` — daemon.db as sole authority
- `internal-docs/design/risk-policy.md` — the constitution A1 reports against
- `internal-docs/design/post-trade-truth.md` — the statement path A2 corrupts
