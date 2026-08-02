# Changelog

All notable changes to this project are documented here. The project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html), and release entries follow [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) categories (Added / Changed / Deprecated / Removed / Fixed / Security).

## v2.6.2 — 2026-08-02 15:06 CEST

### What's new

- **Earnings dates resolve for `BRK.B` and `BRK.A`.** Nasdaq answers a dotted class-share request and then echoes the symbol back with a slash, so the announcement named `BRK/B` where `BRK.B` was asked for and the payload was refused as a format change. It is per-issuer rather than a rule about dotted symbols: `BF.B` and `LEN.B` keep the dot and always resolved. The slash is now accepted only where the request used a dot, position for position, so the announcement still has to name the security that was asked for. (#18)

### Changed

### Fixed
## v2.6.1 — 2026-08-02 10:05 CEST
### What's new

- **The rulebook evaluates again on a TWS login that holds several accounts.** v2.6.0 restored positions and balances for such a desk, but `canary rules` still reported `account: unavailable` and returned all eleven account-dependent rules as unknown. Ten of the eleven now evaluate. FX exposure stays unknown on a book holding a non-base leg, because this release withholds a multi-account login's currency rows until each row can be attributed to its own account, and a base-only reading would be a guess rather than an answer.
- **A multi-account desk can place orders again.** IBKR terminates its account list with a trailing comma, and the check comparing your pin against the connected session read that empty final entry as a malformed account. A valid pin came back as `gateway_account_mismatch`, and the desk refused every broker write. A related fault could rebind a multi-account session's identity to a sibling, with the same refusal as the symptom. It denied orders; it never misrouted one.
- **Earnings dates resolve from Nasdaq again.** Nasdaq changed nothing. The parser required a response shape the endpoint does not serve, so no name had resolved a date since v2.5.2 and every rule needing a catalyst date reported unknown, with no error on any surface. Ambiguous payloads are still refused rather than guessed. (#17)
- **A treasury or bond no longer shows the daily move of the stock that shares its ticker.** Non-option holdings were priced by quoting their bare symbol as a stock, so a treasury symbolled `T` wore AT&T's quote and printed a daily move around 252%. Bonds, bills, funds and futures now keep the broker's own mark and carry no quote context and no day change. Equities are unaffected.

### Fixed

- **A bond, bill, fund or future no longer reports a market value roughly a hundred times too large.** IBKR quotes these as a percentage of par against a face-value quantity, and Canary multiplied the two, so a bill marked at 98.5 on 10,000 face published as about 985,000 instead of 9,850. The broker's own market value is now reported. No rule verdict moves, but the portfolio's aggregate dollar delta is computed separately and still enlarges on such a holding: preview a derisk sweep on a book like that and read the per-leg quantities before submitting.
- **The account Canary reports as connected is one account, never a list.** A login holding several accounts served the whole comma-joined string as `connected_account`, so the app and AI clients named every account at once. It now carries the account the session is scoped to, and nothing at all when a multi-account login has no pin.
- **The FX-exposure rule no longer reports a clean 0% on a book it could not measure.** Only equities form a group, and the check for a genuinely single-currency book walked groups, so a foreign bond, bill, fund, future or cash leg was invisible to it. Every held leg is now inspected. One foreign bond was enough to trigger this.
- **Option-premium, extrinsic-budget and exit-discipline rules no longer read a foreign leg's premium as if it were already in base currency.** With no exchange rate available, the raw contract-currency figure stood in, which understated some pairs and suppressed the disclosure of legs that could not be measured. Such legs are now marked at substitution: the premium rule reports unknown, and the other two list the leg unconditionally.
- **Two holdings that share an underlying no longer report a portfolio percentage counting only one of them.** The base-currency total behind concentration came from a single representative row, so a name held across two currencies or two issues looked smaller than it was. An exposure rule could pass a name that should have tripped, and the engine could decline a trim it owed. The group still displays one row rather than all of them.
- **Percent-of-portfolio figures no longer use a sibling account's balance as the denominator.** A second, untyped reader of the unattributed account cache supplied the net-liquidation figure every percentage divides by. On a multi-account login the value is now absent, and the bounded refresh supplies an attributed one.
- **Unrealised and realised P&L report the account total rather than one currency's slice.** Where a tag arrived both as the account's base-currency figure and as per-currency rows, the reader took whichever currency sorted first alphabetically. A desk holding CHF, EUR and USD saw the CHF slice presented as the account figure.
- **Futures are no longer understated by a contract multiplier read as 1.** A rule that coerced a multiplier of exactly 100 applied to every non-option row, not only the equity rows it was written for.
- **`canary status` and the terminal dashboard name the account the session is pinned to.** Both led with the connected identity, which on a multi-account login is the comma-joined list, so the session line claimed the desk was scoped to every account at once. An unpinned multi-account login now reads `auto-detect`.
- **A multi-account login gets a clean `unavailable` from the cached account fallback.** The shared streaming cache keeps no per-account attribution, so nothing at read time can prove its contents belong to the pin. Refusing also stops the fallback masking the summary failure that sent the read there, which is what hid the rulebook fault above.

### Security

- **A release can no longer be narrowed on its way out, and its required market gate can no longer be switched off.** v2.6.0 bound publication to one commit and a complete evidence set, but two settings could still be overridden when the pipeline started. Narrowing the build platforms left assembly treating that subset as the whole release, and the inventory was counted only after the tag had been pushed, so an incomplete release could reach a published tag before anything objected. The other switched off a market-data check the release smoke requires. Both are now refused from the command line and the environment alike, along with every other setting a release path reads that could weaken a gate. The payload inventory is proved by exact name before the tag is pushed as well as before publication.
- **The identity scan refuses to run on a half-merged working tree.** `git ls-files --cached` lists an unmerged path once per stage, so a conflicted file was scanned repeatedly and spent its reviewed allowance several times over, then reported its content as stale. It now names the unmerged paths and refuses, rather than passing over content that is not what will be committed.

## v2.6.0 — 2026-07-31 21:12 CEST

### What's new

- **The app wears its new design: Panel Dark.** The paired app is now a dark cockpit annunciator panel. The Monitor is a fixed grid of instrument windows — every regime cluster the daemon ranks and the desk's rules, protection and stress — each with an engraved legend, a state caption that lights amber or red only when its own condition does, and figures that always stay in plain readout ink. A master annunciator sums the panel and never disagrees with it: when a red exists that the panel cannot show, or a governor holds severity down, the master's subline says so in words.
- **Gauges show their triggers.** Every regime indicator now serves a compact trip anchor beside its worded bands ("trips <40% (50d)"), dealer gamma serves its measured spot/γ-zero pair, and the stress read serves the margin-cushion watch floor (`cushion_trip_pct`). Monitor windows print the trigger engraved beneath the reading — "cushion 76% · trips <35%" — so distance-to-trouble is read at a glance, and a window whose producer serves no trigger stays reading-only: the app never invents a cutoff.
- **All six regime clusters have windows.** Funding and FX join Breadth, Volatility, Credit and Dealer gamma in a fixed three-across grid; the first four keep their seats. The morning brief's Ready movement now also counts staged protection work — "2 protection proposals are ready to act, with 1 more blocked" — as a served fact that joins the brief's content identity; staging is never authority to place anything.
- **The Brief is a real briefing.** The daemon now composes the morning brief as prose — a lead verdict, Review and Ready paragraphs, and a closing line — from the same facts the row view showed. Clean topics fold into summary clauses, the text grows exactly where something is flagged, and unread sources are named rather than skipped: unknown is not clean. The row view remains as the fallback when a narrative is not served.
- **`canary brief` reads the briefing too.** The terminal renders the same composed prose the app does, wrapped to a readable measure, with watch-class clauses in amber and act-class in red and figures left in the terminal's own ink; pipes, `NO_COLOR` and `CANARY_COLOR=never` give the plain sentences. Prose carries no per-row reason, so a short **Degraded inputs** list under the briefing names each unread row and why it went cold — nothing is disclosed less than the table disclosed. An older daemon, or a payload whose movements did not compose, still gets the row table, and `--json` is unchanged.
- **Depth moved into sheets.** Protection proposals with the trim controls, the full 14-rule checklist with its evidence, and the underlyings book with its bulk actions each open as a full-height sheet from their Monitor window; the Monitor itself stays a single calm glance. Option-exercise opportunities appear only when they exist, as an advisory bar on the Protection sheet.
- **The lamp test.** The Monitor's stamp line counts live sources ("10/10 sources ok") and opens a detail dialog showing every wired feed with its state and evidence time, delivery health, and the daily broker-report check. A dead feed turns its Monitor window into a dead instrument — dimmed, keeping its last good figure with its timestamp — instead of pretending calm.
- **Settings is the instrument's back panel.** Engraved banks for notifications (level segments, push state, safe test), workflow switches with printed state words, the process-evidence record (moved here from Alerts), a six-cell status grid, and a stamped type plate.
- **`canary stop` puts the desk's local processes down without a kill command.** It stops the app first and the daemon second — the order that keeps a running app from starting a replacement daemon mid-stop — and `--app` or `--daemon` stops just one. Before stopping a daemon that still has work in flight it says what that work is, in the daemon's own words ("open-orders  3 working"), and asks; the same work its idle shutdown already refuses to exit on. A script answers with `--yes`. `--force` means one thing only: escalate a process that ignored SIGTERM to SIGKILL once `--timeout` has passed. An app supervised by launchd is unloaded rather than signalled, because KeepAlive would otherwise put it straight back and the command would be reporting a stop that did not happen; the reply says how to bring it back. MCP servers are counted and named with their AI client, never signalled: that process belongs to the client that started it and already exits with it.
- **A market-data feed that has gone quiet is now visible in the regime dashboard's own timestamps.** Every per-scalar freshness stamp on a regime row was written when the snapshot was assembled rather than when the value arrived, so each one read about a second old whatever its true vintage — two legs of the same row came back identical to the microsecond. A subscription the broker still labels live but which has stopped delivering ticks therefore looked exactly like one that is ticking: the data-type flag reports the subscription's mode, and the timestamp, the one field that could have revealed a silent feed, was overwritten with read time. Each measurement now carries the instant its own tick arrived. Two limits are stated where they are read: in frozen mode the broker re-sends its last known value on request, so a frozen quote's stamp is essentially read time however old the value is; and the stamp advances on any tick, size and volume included, so a name delivering only size ticks still looks alive while its price is frozen. Nothing fails, degrades, or blocks on the new number — it is measurement, and a stamp that is missing or ahead of the daemon's clock is served as no timestamp at all rather than as a fresh observation, which is the same conservative reading as before.
- **The off-window VIX3M leg is dated from Cboe's own published close, so a broker that keeps answering with a stale value is caught.** Outside Cboe's publication window nothing bounded how old the served VIX3M could be: the row read `not_due`, which exempts it from every age bound, so the volatility source read healthy indefinitely. A dead subscription was never the reachable failure — that returns no tick and correctly reaches `overdue`. The gap was a gateway that keeps answering: a lapsed market-data entitlement, or a stale contract id. Off-window the served leg is now Cboe's official daily close whenever one covers the last completed session, and comparing it against the broker's own reading is what makes a stuck subscription visible. The verdict is served as `vix3m_cross_check`: agreement, official-only, and pending-publication vouch for the leg and keep `not_due`, while disagreement or an unverified leg fails closed to `overdue`. On a disagreement the official close is served and the broker's reading is retained beside it, so the discrepancy is inspectable rather than asserted. Cboe publishes after the session, so for the evening between the close and publication the broker leg stands in, bounded to one session; in session the gateway remains the source and the check does not run, because Cboe publishes closes, not intraday values. An official close never earns confirmable freshness — the row lands on stale, so it can neither bank a persistence session nor confirm stress the way a live same-session tick can.

### Changed

- The Alerts tab is a pure annunciator log: active entries carry their source placard, severity-tinted title and a worded age line (weekday within the week, date beyond it), act entries sort above watch, and a seven-day extinguished register sits beneath; a confirmed-quiet desk shows the engraved ALL DARK poster. Source coverage and delivery health moved to the lamp-test dialog.
- The header carries the canary mark, a minutes-precision session chip that counts down in both directions ("RTH · closes 3:59" / "opens 17:12"), and the account dataplate in mono; the market strip is fixed to SPY, VIX and QQQ with neutral delta ink — readouts never lamp. A closed market now states why in the chip itself — "Weekend · opens 2d 14:30", holiday names included — instead of hiding the reason in a tooltip.
- The account strip always states the trading mode: LIVE in the plate's engraved register, PAPER as a red field with white text in the TWS simulated-trading convention. An absent pill is no longer how live mode reads.
- Briefing prose is capped at a readable measure on wide screens; the phone layout is unchanged.
- Money renders in restrained tints rather than full alarm colors, and full saturation is reserved for lamp bars, the master wash and marks; the open-orders journal renders as engraved order bars with an explicit read-only foot line.
- The MCP tool descriptions caught up with the desk. `canary_status` names the `storage` subsystem and says what a fail-closed database means for reads and writes; `canary_stress` states that a low margin cushion is urgent on its own authority, even when the headline action reads stand_down; `canary_regime` and `canary_stress` document the served trip anchors, and the monitor view's cluster and thresholds; `canary_settings` names the rulebook keys and the earnings overrides behind the rulebook's `override` source.
- `canary --help` lists its 36 commands in three groups — Desk for the account, its positions, orders and risk; Markets for quotes, chains, screens and regime research; System for running Canary itself — one command per line, short enough to fit an 80-column terminal instead of wrapping the flat list into a wall. The full summary of every command stays where it is read closely: `canary <command> --help` and the CLI reference, which now carries the group as a column.

### Fixed

- **The daemon no longer refuses to start after a restart that lands while a regime row is holding a frozen reading.** When an indicator's evidence is not fresh, the live path deliberately freezes its streak record and keeps the last fresh reading, but the startup consistency check re-derived that reading from the current snapshot — claiming a write the live path never made. The two disagreed, there is no repair path at that point by design, and the daemon failed closed and would not boot. The check now preserves the prior value when the served row proves the writer froze. Everything it actually guards is unchanged: a fresh row's reading is still re-derived and still fails closed when it disagrees.
- **A single bond or treasury holding no longer empties the whole positions list.** IB omits the contract multiplier on bond, bill, cash and fund frames, and Canary treated the absence as corruption and discarded the entire staged download — so one held treasury made every position disappear, for any account. Those rows now read as a single unit, which is what the rest of the desk already assumed. Real derivatives keep the strict check: an option, future, future option or warrant that arrives without a multiplier still fails its download rather than being published understated.
- **Two holdings that share a symbol each report their own daily P&L.** IB repeats a base symbol across treasury issues, and a future repeats one across expiries, so a desk holding two issues of the same treasury — or one future in two expiries — had both rows collapse onto a single internal position identity. The later row's contract won, and the earlier row then displayed the later one's daily figure as if it were its own; a wrong number, not a blank. Every row now carries its own contract identity, so the figure beside it belongs to it. Treasury and bill rows only became visible at all with the multiplier fix above, which is why this surfaced now.
- **Positions and account values survive a TWS login that holds several accounts.** Such a login streams every one of its accounts over the single account-updates subscription. Canary read the first sibling row as proof the stream was misrouted, latched a fault that rejected all later rows, and then resubscribed without the account pin — which rebound the stream to whichever account the gateway happened to list first, so the pinned account's own rows looked foreign and the fault latched again. Positions stayed empty, and the account summary showed the sibling's type and its zeroed balances while the P&L stream kept reporting the real ones. Sibling rows are now dropped quietly, a resubscribe keeps the account it was bound to, and the summary guard compares against that account instead of the raw account list — the case the earlier fix in this area could not see.
- **Restarting the daemon no longer reports a failure that did not happen, then leaves the phone's app stopped.** A restart stops the app first so it cannot start a competing daemon, waits for the new daemon, and brings the app back. The wait was a flat 15 seconds, but before the daemon accepts connections it verifies its whole state database, and on a desk with real history that takes closer to a minute. The restart declared the healthy daemon dead, and the app it had quiesced stayed down until someone noticed and started it by hand. The wait is now derived from the size of the file being verified, so a slow but healthy start is a wait rather than a failure, and the same applies to `canary status`, which used to give up after five seconds during a normal boot. A daemon that genuinely dies while starting is now reported in milliseconds instead of after the full wait, and the daemon prints what it is verifying and how long it took, so the pause is legible instead of silent.
- **Daemon startup no longer gets slower every day, and the state database stops growing without bound.** The daemon was writing its entire contract lookup cache into its permanent evidence ledger once a minute — 5.1 GB of it in eleven days, accelerating, none of which anything ever read back. Because every start re-verifies that ledger before opening for business, the desk was paying for those unread copies as startup delay that grew by roughly a minute a day. The cache is a local copy of contract details the broker re-serves on request, so it is now kept as current state only; nothing that a trading decision rests on left the ledger. Startup also stopped verifying the database a second time after applying no changes to it, which was half of the remaining wait. On the first start after updating, an affected installation now removes only those exact unused cache snapshots through the normal crash-safe schema upgrade, compacts the database out of place, keeps a verified compact recovery copy, and retires the oversized old backup only after a durable cleanup receipt proves success.
- **AI clients no longer get tool timeouts while the daemon is starting.** An MCP tool call opened its daemon connection inside its own answer budget, and `canary_status` — the tool an assistant reaches for first — allowed two seconds for both. Since the daemon verifies its state database before it accepts connections, and that wait now scales with the file, a healthy boot on a desk with real history answered every tool the client tried with "timed out after 2s". The connection is now opened under the daemon's own readiness budget and the tool's budget starts once it is open, so a slow start is a wait rather than a failure. A daemon that is genuinely stuck still fails, with the reason and the PID.
- **The daemon log path we print is the one in use.** `canary status`, the gateway-down hint and `canary daemon --help` printed a fixed `~/.local/state/ibkr/ibkr-daemon.log`, so a desk with `CANARY_LOG` set was sent to a file nothing was writing — exactly when it was chasing a daemon that would not start. They now print the resolved path, with the home directory still shown as `~`. The log itself also follows `XDG_STATE_HOME` the way the daemon database always has, so a moved state directory no longer leaves the log behind in `$HOME`; on such a desk the next daemon start writes beside the database, and the old file stays where it is.
- One ragged row no longer discards an entire official data series. The CSV reader locked the field count to the header, which made the loader's own short-record guard unreachable, so a single malformed line dropped every observation. Affects the VVIX and Federal Reserve series as well as the new VIX3M closes.
- Stale freshness stamps no longer wear the alarm red: an aged timestamp reads as a dead feed in the fault tint, distinct from market severity.
- On a held book with no daily movement, the underlyings book stayed reachable behind a hidden control; the movers row now keeps its opener with an honest "no daily movement" reading.

### Security

- **A release can no longer be published from mixed or incomplete evidence.** Canary now binds publication and recovery to one exact commit, the complete required GitHub Actions job set, the canonical origin and annotated tags, the signed twelve-asset inventory, identical stable and versioned MCP bundle bytes, and matching MCP Registry metadata and digest. Recovery runs the current release controls against immutable tag-owned source, so a stale workflow, partial rerun, mismatched asset, or red historical CI run fails closed before anything is published.
- **The app no longer keeps a readable login secret in browser storage.** Pairing over the plain-http LAN host used to hand the browser a long-lived secret that any script on the page could read, because that origin has no WebCrypto and so cannot hold a device key. It was never the credential that did the work — the browser already holds an HttpOnly device cookie it cannot read, and that is what authenticates the phone on every request. The secret is now gone from the client, from the request, and from stored device grants; a device that pairs without WebCrypto registers no readable credential at all. The cost is durability, deliberately: if that cookie is lost, the device re-pairs from a QR code instead of falling back to a secret worth stealing.
## v2.5.4 — 2026-07-30 15:39 CEST

### What's new

- **The app's governance panel shows live evidence again.** Its delivery section had kept reading a ledger that production stopped writing at the source-neutral alert cutover, so fresh installs showed a permanently empty panel and older installs showed frozen counters with live-looking labels. The panel now projects governance rows from the same delivery ledger every other alert uses, alongside the shared delivery health and the safe-notification test result. The per-attempt list and the frozen counter walls are gone.

### Removed

- The retired derived-history storage layer is deleted. daemon.db became the sole live authority within a day of that layer shipping, and both changes reached users in the same release — no released daemon ever created or read the secondary file. The runtime, its rotation crash-recovery, and their isolated compatibility tests (~8.4k lines) removed nothing anyone had; the retired rotation settings keys keep their read-only compatibility shape, and upgrading from any released version is unaffected.
- The app's retired governance delivery ledger is deleted (~1.9k lines). Old state files migrate on first load: legacy governance rows leave the shared unread-attention sequence space with read state preserved, and the legacy keys disappear from `state.json` on the next write.

## v2.5.3 — 2026-07-30 12:42 CEST

### What's new

- **Policy gates now respect the official options calendar.** The option-exercise trading-hours blocker and the live-IV requirement used a clock-only check that treated market holidays as open, missed early closes, and ended the session at 16:00 where listed options trade to 16:15. Both now consult the embedded official calendar, so a holiday morning is correctly blocked and the 16:00–16:15 window is correctly open.
- **The terminal UI asks before exactly the commands that write.** Seven confirm-classified paths ran without a prompt — `trading paper-smoke`, the five `policy` write subcommands, and `recon dismiss` — while ten read-only `proposals` and `opportunities` subcommands prompted needlessly. Confirmation now follows the per-subcommand guard classification.

### Deprecated

- `pkg/ibkr` marks its dead public surface for removal at the next major version: `ConnectorConfig.ServiceName` (no reader since the connection-pool removal), `RateLimitedRequest` (scheduler-internal), and the three system-notice setters whose callback types are unexported and therefore unusable outside the package. Nothing is removed in v2.

### Fixed

- The CLI reference omitted `restart --remote` and `--remote-url` and still advertised `proposals --protect-hedges`, a flag the parser rejects; the command catalog now carries both restart flags, drops the phantom, and a parity test pins every catalog flag to a real flag registration so this drift class fails `make check`.
- The Claude Desktop install page shipped malformed inline CSS — two unclosed rules that browsers silently dropped, leaving its callout unstyled. The stylesheet is closed and a new structural check gates every hand-authored docs page's `<style>` blocks.
- The experimental trading example told users to add live acknowledgement fields that the strict config loader has rejected since their removal, and the generated configuration reference described the paper smoke as governing live enablement. Both now state the real gate: `[trading].mode` plus the gateway pins, with the paper smoke as informational, release-pipeline-only evidence.
- The README claimed market data is never stored, cached, or leaves the machine; the daemon keeps local caches and journals, and optional features (remote relay, Web Push, MCP hosts) transmit data by design. The claim is now scoped and `PRIVACY.md` is named as the authoritative list.

## v2.5.2 — 2026-07-30 10:47 CEST

### What's new

- **A broker-link data loss now recovers completely.** When TWS loses its own connection to IBKR and restores it with subscriptions dropped, every live market-data subscription is re-issued automatically and the account streams rebuild; order transmission is refused while the link is down instead of sending into a dead connection.
- **The paired app's alert badge survives network hiccups.** A failed or hung alert refresh no longer clears the unread badge or announces "no unread alerts"; the last validated state stays visible until real evidence replaces it.
- **Unknown state is reported as unknown, not as zero.** A transiently zero-valued holding, an unprimed empty position stream, and an unreadable order journal each used to read as clean absence; all three now stay visible as unresolved until positive evidence settles them.

### Added

- A margin-cushion alert that does not wait for market confirmation. The stress read already classified a low cushion as urgent, but the alert channel only fired on the market-confirmed top-level verdict, so an account-only emergency in a calm market stayed a silent panel row. A cushion below the stress policy's existing watch, act, or urgent thresholds now opens its own alert episode, recovers only when a healthy cushion is actually observed, and holds open instead of resolving when the cushion cannot be read.

### Changed

- A release interrupted after its tag was pushed can be resumed with `make release-resume RELEASE_VERSION=vX.Y.Z`. Completed publication legs verify themselves and skip, every leg runs pinned to the exact tagged commit, and a partially uploaded GitHub release stops for a human decision instead of being overwritten.
- The release smoke gate exercises the regime dashboard and an option-chain read again, this time against a settled session rather than the deliberately contended sequence that had made those steps flaky enough to remove.

### Fixed

- TWS distinguishes two recovery notices after a broker-link outage: code 1102 keeps server-side subscriptions, code 1101 drops them. The connector treated both as plain recovery, so after a data-loss restore every streaming subscription stayed silently dead while farm status read ok. Each live shared subscription is now replayed exactly once on 1101, the account-updates and daily P&L streams force-rebuild, and code 1100 refuses order transmission until a restore notice arrives.
- A failed or timed-out `/api/alerts` refresh in the paired app marked the whole validated alert feed invalid: the unread badge disappeared, the tab announced "no unread alerts", and a hung request wedged every later refresh. Refreshes are now bounded by a deadline, a transport failure keeps the retained validated state and surfaces itself as a status note, and a genuinely invalidated feed also quarantines the delivery-health and selected-alert panels instead of leaving them looking current.
- A held stock whose account row transiently reported zero marks was sealed as "no market data expected" before any quote probe could disprove it, and stayed excluded from quote enrichment afterwards. The probe now runs for zero-value rows, and only the broker's own non-reporting verdict may seal a row as expecting no data.
- An empty position stream is only trusted as a flat book once the portfolio stream's account-scoped completion receipt has arrived. Previously a missing gross-position-value field, which flattens to numeric zero on the wire contract, could make an unprimed empty stream pass for a completed flat book in proposals, opportunities, and the rulebook.
- A daemon whose order journal became unreadable counted zero open orders and could idle-exit while broker-side protective orders were still working. An unreadable journal now defers idle shutdown and reports itself as a degraded status row.

### Security

- Broker-authored free text no longer crosses the MCP boundary on any order surface. The order preview withholds WhatIf prose, advanced-reject JSON, and the broker's margin warning text; the open-orders, order-history, and order-status tools withhold broker message and why-held text — those wire fields carry broker-error prose (including concatenated reject JSON) and daemon display notes without provenance, so the boundary fails closed and withholds all of it. Agent surfaces receive typed statuses, error codes, numeric margin fields, and fixed Canary guidance; the verbatim text stays on the CLI surfaces and in the local order journal.
- Release packaging refuses ambient `GOFLAGS` carrying build tags and capability-checks the binary inside every packaged archive, so the canonical read-only artifact can never silently ship a trading-capable build.

## v2.5.1 — 2026-07-29 19:29 CEST

### What's new

- **Daemon restarts warm up in seconds instead of minutes, and a connectivity blip no longer wedges tickers.** The option-contract cache persists across restarts again, so the dealer-gamma read starts from thousands of already-resolved contracts instead of re-asking the gateway for every strike. A brief TWS connectivity outage no longer leaves liquid tickers marked inactive for hours after the feed recovers.

### Fixed

- The gateway reports option expiries with a settlement-time suffix (`20260729 15:00:00 US/Central`); the connector stored that raw string, so every cached option row contradicted its own cache key and the contract store refused to persist each snapshot — a warning per minute during the gamma fan-out, and a cold two-minute option-chain prewarm with dropped strikes and thin open-interest coverage on every restart. Expiries are now reduced to their date token the way the official IB decoders do, and the option snapshot persists again.
- TWS flushes queued "no security definition" answers before the farm-break notices that explain them, and most farm breaks heal within seconds of appearing — so during a short connectivity outage the farm map read all-OK and liquid tickers were marked inactive for the 12-hour TTL. Connectivity notices 1100/1101/1102 now feed the farm tracker directly, and a 60-second settle window after recovery absorbs flushed outage-era answers before delisting detection resumes.
- A burst of stress-driven alerts no longer triggers an unbounded cascade of alert refetches in the paired app; refreshes coalesce through a minimum-interval scheduler.

## v2.5.0 — 2026-07-29 08:01 CEST

### What's new

- **The product is now Canary.** The executable, the Go module, the repository, the published site, the MCP tool prefix, the resource scheme, and the product's environment variables all leave the ibkr name behind in one release. The break is clean on purpose: nothing aliases the old name, and the Changed section lists exactly what to update.
- **The documentation has a home.** [osauer.dev/canary/docs/](https://osauer.dev/canary/docs/) is a five-section handbook — Start, Operate, Understand, Reference, and Under the hood — reached from the site navigation, with every page on a sectioned URL. Three pages that existed but had never been published are now on the site.
- **The portfolio canary is now the portfolio stress read**, so the sensor no longer shares the product's name. Open alerts, episodes, and history survive the rename; one policy-file key needs renaming by hand.

### Added

- **A documentation handbook.** Before this the site published fifteen documentation pages with no index and no navigation entry; nine of them had no inbound link from the landing page at all and were reachable only through the sitemap. The handbook runs in five sections, ordered by how far a reader has travelled: **Start** (install, connect an MCP host, first session, updating, troubleshooting), **Operate** (the daily desk routine, working with agents, the paired app, alerts, order previews, protection, reconciliation), **Understand** (concepts, sensors, trading policy, the rulebook, writing a risk policy, market data and entitlements, glossary), **Reference** (CLI, MCP tools, MCP resources, configuration, releases), and **Under the hood** (architecture, storage, the TWS wire protocol, published design notes, packaging). The first four are written for someone trading their own account; the fifth is for someone reading the code. Three pages that existed but had never been published are now on the site: the paired app, order previews and the trading build, and the opportunity research harness. Fourteen pages that do not exist yet are listed with their scope and a "not written yet" marker, without a link and without a URL, so the shape of the whole is visible while only finished pages are reachable or indexed.

### Changed

- **The product and every public identifier make one clean break to Canary.** The executable is `canary`; the Go module is `github.com/osauer/canary/v2` with its entry point at `cmd/canary`; the repository is `github.com/osauer/canary`; and the published site is `https://osauer.dev/canary/`. MCP tools use the `canary_*` prefix, resources use `canary://`, and product-owned environment variables use `CANARY_*`. Release archives, the MCPB, the Claude Code plugin and marketplace, skills, examples, badges, source links, and install commands use the same identity. The retired executable is not a command alias, and the old `/ibkr/` Pages route intentionally returns 404 rather than redirecting.
- **Upgrading is deliberately manual at the public boundary.** Replace `ibkr` commands, `github.com/osauer/ibkr/v2` imports, `ibkr_*` MCP tool calls, `ibkr://` resources, product-owned `IBKR_*` environment variables, old repository/site links, and old plugin or asset names in every host and script; then reinstall or reconfigure the shell binary, MCPB, Claude Code plugin, and other MCP hosts as applicable. New registry releases publish only as `io.github.osauer/canary`. The old `io.github.osauer/ibkr` registry entry is marked deprecated with a pointer to the new name; its published versions remain as immutable history, and no further release appears under the old name.
- **The portfolio canary is now the portfolio stress read.** The sensor had shared the product's name, making phrases such as "the canary says watch" ambiguous. Its public surfaces are now `canary stress`, `canary stress history`, `canary backtest stress`, and `canary_stress`. In `~/.config/ibkr/policies/risk-policy.toml`, rename `[inventory.canary]` to `[inventory.stress]` and increment `policy_version`; an old key or an edited file at the same version fails closed.
- **Persisted sensor state migrates without reallocating open alerts.** The history database renames its transitions table and source at schema 4. The authority database renames the table and relabels `canary_decision` events at schema 2 after taking a verified backup. Stored platform settings, the alert registry, and the app delivery ledger fold the old shape forward on read. Current-result labels move from `canary-` to `stress-`, `canary_portfolio_stress` becomes `portfolio_stress`, and the alert registry preserves the existing episode and occurrence across the presentation rename.
- **Only safety-pinned or broker-nominative old identifiers remain.** All XDG config, state, data, cache, and runtime paths stay under the `ibkr` namespace; the daemon authority remains `ibkr.sock` plus `ibkr.lock`; the macOS supervisor remains `com.osauer.ibkr-app`; paired-app identity remains `ibkr_app_device` and `ibkr_remote_route`; and persisted typed document/config kinds remain `ibkr.*`. The decision journal file keeps its `canary-decisions` name, while the alert `source` stays `canary` and its hashed episode identity stays `portfolio_canary`, so rotation and existing alert identity are not stranded. The old executable name is recognized only to quiesce a process left running across the upgrade, never to launch or advertise it. Broker-wire diagnostic variables such as `IBKR_WIRE_*` and `IBKR_PACKET_LOG_TEMPLATE`, and gateway-test pins such as `IBKR_TEST_PORT`, remain broker-nominative; they are not product aliases.
- **`spec_doc` in a regime response is now a fetchable URL.** The field exists so an agent consults the published methodology instead of inventing threshold band edges, and it previously carried a path inside this repository, which a remote MCP client cannot open. It now carries `https://osauer.dev/canary/docs/internals/regime-dashboard.html`. Consumers that pinned the old string need to update; the field name, type, and purpose are unchanged.
- **Every documentation page moved to a sectioned URL.** `architecture.html` is now `docs/internals/architecture.html`, `policies.html` is `docs/understand/policy.html`, `database.html` is `docs/internals/storage.html`, and the pages under `guides/`, `reference/`, and `specs/` moved into the section that matches their audience. The fifteen retired filenames serve a `noindex` redirect to the canonical URL, so a pre-handbook link keeps working once its `/ibkr/` prefix says `/canary/`. The sitemap, `llms.txt`, `llms-full.txt`, and the MCP discovery metadata all point at the new locations.
- **`docs/` in the repository is the published website and nothing else.** It previously held both the site and the project's own engineering documentation, so design records, agent task contracts, contributor guides, and the diagram build script were all being served publicly. Design records and developer guides now live in `internal-docs/`, the task contracts and session-hygiene notes an agent is instructed to load live in `.agents/docs/`, and the diagram renderer lives in `scripts/`. Each published page's Markdown now sits beside the HTML it produces, so a source path is the URL it publishes to and a link that is correct in the source is correct on the site.

### Removed

- **The checked-in regime and canary backtest corpora are gone.** Twelve JSONL files totalling about 30 MB sat in the unit-test fixture directory while no test, script, or make target read any of them. They recorded verdicts produced by regime code that has been corrected repeatedly since capture, so replaying them measured old defects rather than the current engine. The backtest runbook now captures a fresh tuning and holdout split instead, and built corpora land in a git-ignored directory so a capture cannot quietly become a fixture again. The small samples that tests do read are unchanged.

### Fixed

- **The paired app's Alerts tab could not display anything.** The app pinned the alert ledger version one release behind the daemon, so every live payload failed validation and the inbox was closed against real data.
- The published brief screenshot had been rendering an empty stress row, the command catalog offered a `backtest` subcommand the CLI rejected, and the shipped example prompt called a tool that no longer exists.
- The documented stress-check statuses were wrong on the landing page: it listed watch, warn, and critical, which the sensor does not return. Storage documentation claimed one production migration when there are three, and the app page described push notifications as canary-specific when the delivery gate has been severity-based across nine producers since v2.3.0.

### Security

- Migrations may now carry an explicit, audited approval to run a destructive statement, which the sensor rename needed in order to rebuild append-only triggers around a table rename. The guard itself is unchanged and still refuses every destructive statement by default; an approval must name each statement and state a reason, and is rejected if it names a statement the migration does not run or one that is not destructive. Every shipped migration's checksum is now pinned by a test, so editing a released migration in place fails loudly instead of leaving existing databases unopenable.
- The remote relay's image toolchain moved off a `sharp` release with a known vulnerability.

## v2.3.1 — 2026-07-24 09:00 CEST

### What's new

- **Releases now fail fast on the paper account, exact-session currency conversion, and broker WhatIf path.** A read-only SPY preview runs before the full test suite and proves that its USD notional was converted into the different account base currency using a current typed IBKR FX quote. It never submits an order; the existing one-share paper place, acknowledgement, cancellation, and cancellation-confirmation round-trip remains the final binding release gate.
- **Release automation is shorter and clearer without weakening authority.** The repository harness prefers typed MCP reads and falls back to the CLI only for uncovered read surfaces, while explicit Codex rules make both the read-only preflight and the sole `make release RELEASE_VERSION=vX.Y.Z` path discoverable. Direct tagging, pushing, GitHub release creation, and registry publication remain owned by the release target.

### Fixed

- The early release preflight can no longer claim FX coverage from a same-currency identity conversion or a preview missing exact-session FX provenance. Its regression fixture now proves both rejection and the absence of any order submission.
- Paper-smoke output no longer prints the connected account, broker order reference, raw broker response, or raw daemon-log tail. Failures retain an allowlisted lifecycle summary while private broker identifiers stay local.

## v2.3.0 — 2026-07-24 07:37 CEST

### What's new

- **Alerts became one inbox, and eight more sources can now reach your phone.** Until now only Canary, governance, and order-integrity alerts had a send path, each with its own owner, while Regime, Rulebook, risk policy, Protection, reconciliation, and Data Health were recorded as shadow evidence that could never page (`delivery_active=false`, `delivery_preference=unapproved`) — this supersedes the shadow-commissioning wording in the two entries further down. The fixed nine-producer universe now enters one source-neutral candidate snapshot, inbox, unread cursor, and dispatcher. The daemon decides whether a condition exists, how serious it is, and whether it opened, escalated, or recovered; the app records that decision, decides whether the chosen notification level permits a push, and owns every delivery attempt and receipt. One global notification mode covers the app host and all paired devices — **Off** (inbox and unread history only), **Action required** (act and urgent), **Watch + action** — and eligibility is sampled when an occurrence is first recorded, so turning notifications up later never arms a condition that was suppressed when it was created; `observe` severity is always inbox-only. The cutover deliberately does not page a backlog: the first complete snapshot in each account-and-mode scope sets a delivery baseline, and conditions already active at that moment stay visible in the inbox as existing context but are never pushed. The delivery gate is per source, so an unrelated source being unavailable cannot suppress an independently current candidate — but an empty candidate list means "clear" only when the whole expected source set is complete and current, otherwise the snapshot is unknown. Delivery identity is the private tuple of scope, occurrence, and target, reserved before transport, so replayed snapshots, repeated polls, and restarts cannot produce a duplicate push; retryable transport failures wait 1, 5, and 15 minutes and a fourth is terminal, a definite rejection is never retried, and a dead subscription is retired so it cannot keep degrading healthy targets. Payloads carry fixed app copy plus allowlisted severity, kind, destination, display id, and app URL — never producer prose, symbols, account fields, or source errors. A push-service acceptance is reported as exactly that and never as proof that a device displayed or you read the notification. The cutover grants no trading authority: an alert cannot place, modify, or cancel an order, change a freeze or limit, approve a risk policy, or turn a degraded observation into a decision.
- **Sensors are now explained on their own public page.** [docs/sensors.md](https://osauer.dev/ibkr/sensors.html) covers the five families — gamma (SPX/SPXW dealer structure with SPY as context), regime (eight broad-market rows into six independent clusters and a lifecycle), canary (that market state against the held portfolio), rulebook (14 advisory discipline checks), and market events (held-name borrow, threshold-list, and halt context) — and states the boundary the surfaces depend on: the daemon is the measurement authority, and CLI, MCP, app, and web read the same typed result rather than re-fetching sources or rebuilding a verdict. Two generated diagrams carry what prose handles badly: how observed sources become sensors, dependent decisions, and downstream alerts, and how freshness actually behaves — proactive refresh while the old value stays authoritative, hard expiry into fail-closed, and the difference between a producer-authored `not_due` window and a genuine overdue gap. A sensor result is never evidence that delivery is active or that a notification arrived.
- **Broker-adjacent capabilities now fail closed where the daemon lacks complete authority.** This entry supersedes older current-release wording that exempted close/reduce orders from size caps or implied that a platform toggle enabled purge/restore submission. Every equity/ETF and single-leg option order now carries the ordinary notional/quantity cap, and a sell-side apparent exit also needs short/sell-to-open permission under worst-case exposure because the daemon cannot see future manual TWS activity; a genuine blocked exit says to use TWS and reconcile. Purge/restore preview and submission are typed-disabled until exact per-leg portfolio plus account-global working-order authority exists, and option-exercise review mints no token and exposes no Canary submit action until exact option-to-underlying risk policy plus durable one-shot authority are approved. The purge/restore setting controls only its workflow/read surface. Account-base cap evidence is bound to the exact broker session from an explicit currency tag or one consistent allowlisted aggregate-value suffix; `ExchangeRate=1` and ledger rows are ineligible. Cross-currency drafts now resolve a fresh exact-session CASH/IDEALPRO quote, bind the quote- and base-currency notionals plus FX provenance into a v4 preview token, refresh the conversion at redemption, and carry that base amount to the first-byte guard; unavailable FX fails closed instead of assuming parity or using the stale display cache. Runtime trading-control updates are human-only in every mode and commit their state plus normalized-origin audit event atomically; semantic no-ops advance neither revision nor audit stream.
- **Daemon state and decision history now live in one SQLite database.** `$XDG_STATE_HOME/ibkr/daemon.db` is the sole live store for runtime settings, risk and alert state, order safety, retained observations, and local decision history. The existing `ibkr regime history`, `ibkr rules history`, `ibkr canary history`, `ibkr recon equity`, and order-history commands read through the daemon instead of opening side files. Original Flex XML statements remain separate broker evidence and are projected transactionally into the database. Do not delete `daemon.db`: it is authoritative state, not a rebuildable cache.
- **The SQLite cutover and upgrade path fails closed.** Startup validates the database shape, integrity, write watermark, and private file permissions before serving requests or connecting to the broker. Upgrades operate on a verified candidate and publish it atomically only after validation; backups, the external head watermark, and sealed legacy artifacts remain recovery material rather than live read fallbacks. Future, damaged, rolled-back, or ambiguous state stops startup instead of silently switching to older files.
- **The daily brief is now a trading-process artifact on its own tab.** Re-conceived from five data-domain sections (Market, Calendar, Portfolio, Risk & limits, Process) into two process movements the daemon composes and every surface renders verbatim: **Review** — post-trade of the last completed session (session P&L, daily attribution by underlying, process coherence [the rulebook-adherence delta plus a new proposals-offered-vs-acted count from retained proposal outcomes, and overrides used], capital events, the reconcile clock with its one-tap sign-off, and the working-orders end state) — and **Ready** — pre-trade for today (overnight & market, calendar, risk capacity, and desk readiness). It is a regrouping of facts the daemon already had — row severities and the worst-child rollup are unchanged — with the one new derivation carrying only counts and the covered day to the wire (no proposal keys, symbols, order refs, or tokens). The brief moves off the Monitor tab to its own bottom-tab slot, "Brief" (Monitor · Brief · Alerts · Orders · Settings, Monitor still the default landing and universal fallback), with sunrise (Ready) and history-clock (Review) phase glyphs and sentence-case titles. Process/governance push nudges that used to land on Monitor now land on the Brief tab; the one-tap reconcile sign-off keeps its exact endpoint, evidence, and semantics. VaR and any risk-unit measure stay out of scope (an open operator decision).
- **The account header stops shouting the account number.** The account id is demoted from an 18px near-black headline to a quiet 12px muted subtitle (tabular figures), and it is now masked by the same eye toggle that hides money values (U•••••NN, last two characters visible) — previously the id was the one sensitive string the eye left in the clear. Money values become the visual headline by the id's demotion; the folded "Detail" toggle regains a readable label; and when the freshness stamp goes stale it takes the id's former weight in red, because staleness outranks identity. The trading-environment pill is operator-decided: **live renders no pill at all** (the safe default is silent), **paper renders a loud red PAPER** ("portfolio data is fake"), and an unresolved mode renders a muted "mode?" that fails visible instead of silently resembling live.
- **The Monitor tab now separates "needs eyes" from "data is cold."** The follow-up trader-panel review (discretionary trader and prop-desk risk manager, same bar as the Alerts pass) caught the Monitor tab running both dishonesty modes at once: the capital tile rendered "OK" while its own values showed a breached block tier and an engaged drawdown latch (the "OK" was plumbing health dressed as a risk verdict, and the "Risk & limits" section header green-lit the lot as "section complete"), while a normal closed weekend inflated four "Count 0" event rows and an idle dealer-gamma cache into DEGRADED noise. The daily brief now carries a fourth row status, attention (orange), derived strictly from values: a breached or latched capital tier, an active policy override, or held-name earnings whose governing rulebook rule cannot be evaluated all render attention no matter how healthy the plumbing looks, and section headers roll up their worst child with counts ("risk and limits: 2 of 4 row(s) need attention") instead of reporting completeness as health. Expected closed-session coldness went the other way: on an official non-trading date a cold gamma cache and stale-or-idle event sources render OK, with copy that claims only what the code verified ("no fresh update expected while the market is closed (source health stale; last checked 2026-07-17 23:58)"), each event row now judged by its own sources (an unreachable borrow-fee feed can no longer degrade the halt row), and degraded reserved for abnormal-for-session states at any hour. The adversarial re-review then pushed provenance onto the risk rows themselves: the capital row prints when its adjusted peak was observed ("peak set …", the tell that exposes a poisoned observation), the latch row prints the consumed share recorded at engagement ("Engaged at 30.4%") so a later data glitch cannot rewrite why it fired, and a rulebook transition into act lifts the Rules-delta row to attention instead of hiding under data-quality vocabulary. CLI renders share the same daemon-authored statuses and copy.
- **The Alerts page now talks like a trading desk, not a debug console.** A trader-panel review (discretionary trader, prop-desk risk manager, UX writer) found the page inverted: all-clear rows dressed as alerts, one real warning buried in telemetry ("0 red clusters (none), 5/6 ranked"), a counter in storage vocabulary ("3 current / 1 stored"), and weekend-idle governance plumbing that read as an outage. The page is now severity-first: the header counts conditions ("2 watch · 2 data", "All clear"), a plain-English status sentence leads with first-seen and data-as-of stamps, only rows that need attention render as alerts, each with a text severity chip (ACT / WATCH / DATA / INFO) instead of unexplained bullet colors, and passed checks collapse into one "✓ N checks passed" line. Evidence moved behind per-row Details disclosures and dropped its zero-count noise. History and previous-context alerts fold into one collapsed, dated section whose clear button states its consequence, and "Governance evidence" became a one-line "Process checks" status ("paused — data sources are not ready (normal outside market hours)") with the delivery ledger tucked behind the evidence disclosure, nonzero counters only.
- **A push-delivery failure during market hours now interrupts instead of hiding in evidence.** The accepted follow-up batch from that Alerts trader review lands, led by its top item: when web-push delivery health is degraded or unavailable while the US-equity session is open, the Alerts page leads with a plain-English banner ("Alerts may not be reaching your phone — push delivery is degraded while the market is open. Rely on this page until delivery recovers."), instead of burying the only warning in the governance evidence disclosure. Known-closed sessions keep the quiet disclosure-only presentation, an unknown session state fails visible, and operator-chosen "Off" (suppressed delivery) never banners. A JS fixture pins the degraded-delivery + open-session rendering and every quiet case.
- **Broken market inputs no longer masquerade as an early warning.** Regime now reserves `early_warning` for current, usable market evidence. Any missing, invalid, or overdue required source publishes the explicit state **"Market state undefined — data incomplete"**, with decision readiness blocked and low confidence; provisional measurements remain visible as unconfirmed context but cannot define the market posture. Source cadence is exchange-aware rather than wall-clock-only: VIX and VIX3M have separate official dissemination windows, gamma distinguishes not-due from a missed session or no last-good result, and the latest completed-session or weekly observation remains usable for its declared cadence instead of disappearing at midnight. Canary carries the same typed source health and surfaces unrelated data gaps separately rather than rewriting a valid Regime state.
- **Regime, Protection, and Data Health now participate in the durable shadow alert lifecycle.** The desk-approved Regime producer opens one market-stress episode only from current `early_warning`, follows the existing governed severity through `confirmed_stress` and `panic`, rechecks source age/classification, and refuses to treat `data_quality` as a warning or recovery. Protection covers only structurally complete, current orphaned-order and reconciliation-required evidence; ordinary partial or unprotected holdings stay context until a separate protection-obligation policy exists. Data Health records allowlisted root incidents without farm-name fan-out, distinguishes a live startup handshake from a failed connection, and treats source-authored `not_due` and normal computing as healthy cadence. Daemon-owned 30-second heartbeats keep all three observed when no app or CLI is open. Status remains responsive through an ordered semantic-transition, shutdown-drained worker if shadow persistence fails, and contract identity remains intact in unattended Protection reads so an ambiguous symbol fallback cannot fabricate a clear. Existing throttled reconnect and account-stream renewal remain read-side repairs, not broker writes. SQLite's own shadow incident is explicitly best-effort while its typed health row remains authoritative. All three were commissioned as shadow evidence rather than pages or enforcement; that status is superseded within this same release by the alert cutover above, which routes them through the single dispatcher and its per-source delivery gate.
- **The remaining shadow alert sources now stay observed without an open app or CLI.** Daemon-owned 30-second heartbeats reuse the canonical Nudge boundary for Risk Policy, Reconciliation, and Governance, and the canonical unfiltered read models for Rulebook and Order Integrity. Every read is deadline-bounded and broker-scope-bound; stale, incomplete, future-dated, wrong-account, timed-out, or reconnect-crossing evidence is explicitly uncovered and cannot recover an active episode. Rulebook now applies the same completed portfolio-stream receipt health as Order Integrity, so a cached empty book after stream silence cannot manufacture a clear. The loops drain before SQLite closes, suppress duplicate Protection observations caused by their shared position projections, and introduce no broker write, policy change, or submit-eligibility change. Their shadow-only delivery status is likewise superseded by the alert cutover above; the heartbeats are what make those sources observable enough to deliver.
- **Earnings rules can now distinguish a verified terminal issuer from missing data.** An optional private `[rulebook].terminal_evidence_file` imports reviewed, exact-contract terminal/non-reporting evidence into a typed daemon.db authority at startup. The closed schema requires ConID + symbol + stock type, a classification, strictly non-future review/verification timestamps, a bounded revalidation deadline, and at least two independent allowlisted primary-source documents; an optional CIK must be a nonzero ten-digit identity and SEC evidence must match it. Rule snapshots serve only the committed SQLite revision and expose its revision, fingerprint, catalog review watermark, expiry, and redacted primary-source provenance. Identity reuse, provider/override conflict, expired evidence, unsafe input, and rollback all fail closed. A valid exact-contract match renders rules 6–8 as explicit `not_evaluated` with an Exempt row—never as pass, including stock-only terminal holdings—while mixed ordinary names remain assessed. Revocation persists a per-ConID tombstone, reactivation requires record verification strictly after that watermark, and immutable typed authority-change observations commit atomically with every initialize/import/update/revoke revision. Every rule-transition payload now links accepted terminal evidence back to its exact authority revision and record fingerprint using a sorted typed list; the audit link excludes issuer, symbol, CIK, URLs, and prose, and is explicitly empty when no terminal authority was accepted.
- **Borrow-fee outages now have a narrow, fail-closed diagnosis path.** The global IBKR FTP file remains primary and is still the only source eligible to drive the annualized 50% extreme-fee flag; stale FTP evidence cannot emit or clear it, and `not_due` never triggers a substitute read. When a regular-session FTP refresh actually fails, backs off, or is unusable, the daemon can request historical `FEE_RATE` only for exact positive-ConID stocks currently held short in a complete, fresh, same-account portfolio stream. The RPC reports one typed `borrow_fee_coverage` row per exact contract (or an explicit symbol/route gap), including global versus portfolio-only scope, entitlement, nullable value, data type, scale status, policy eligibility, and redacted failure. Because IBKR documents the OHLC meaning but no controlled numeric-scale fixture is commissioned, every TWS row is `scale_status=unverified`, `policy_eligible=false`, and unable to create or clear the flag. Separate atomic SQLite state keeps opaque account-scope/exact-contract fingerprints, validated completed-session last-good records, and dated attempts without raw account ids or broker prose. Runtime entitlement, failure, and backoff state is discarded on restart; only the exact identical-wire 15-second retry boundary survives, preventing an immediate duplicate request without treating old runtime evidence as current. The portfolio cache now preserves same-symbol stock ConIDs and account identities independently and returns rows plus stream receipt under one projection lock, so collisions, foreign rows, stale receipts, incomplete routes, or reconnect scope drift fail closed before any historical result is published.

### Changed

- Order reads now use the same `daemon.db` authority as order safety. `orders.open`, `orders.history`, preview-token redemption, and the reserved-order-ID floor read the committed order tables while preserving the existing Go predicates, critical sections, accept/reject decisions, and error text. There is no file-scan fallback or settings escape hatch; unavailable or inconsistent database authority fails closed before a broker write.
- The public architecture page was rewritten for readers rather than converters. `docs/architecture.md` is restructured into ten numbered sections in plain English with a new observability section, and two claims were corrected against the code: HyperServe serves only the app process (the daemon speaks newline-delimited JSON over its Unix socket and MCP is stdio-only, with no HTTP transport), and the embedded exchange calendars cover 2026 through 2028 and report an explicit unknown state outside that range rather than falling back to weekday logic. The published twin at osauer.dev/ibkr/architecture.html is now a designed page with numbered chip navigation, process and ownership cards, and scroll-contained tables, and both diagrams are regenerated from the deterministic generator around the single-database storage and recovery model.
- Canary copy is authored in plain English at the source: "A provisional market warning is visible" → "An early market warning is flashing (credit red, not confirmed yet)"; market evidence renders only informative facts ("2 yellow (credit and vol); red but unconfirmed: credit; 5 of 6 clusters reporting") and never zero-count buckets; low-exposure watch guidance reads "your exposure is low; keep watching — no reductions needed"; internal pipeline terms (confirmation-eligible, ranked regime clusters, cached compute, "rebalance this title") were rewritten as observable facts. CLI and MCP surfaces render the same daemon-authored copy.
- A trader-panel re-review then drove a second pass: with the market closed, the status line stamps "market closed — prices are last-session prints" instead of presenting frozen Friday quotes as fresh; data-quality caveats sit in their own quieter "Data caveats" band with a muted DATA chip and never inflate the needs-attention count (which now counts decisions only); the market-stress check explains itself when an early warning is flashing but below the de-risking trigger, so the status line and the passed-checks line can no longer contradict each other; SPY/VIX tape evidence prints its trigger levels next to the observed moves; history timestamps older than two days carry real dates; and the history section is titled "Signal history".
- Dismissing live alerts is now safe by construction: the control is labeled "Hide until signal changes", hides only watch/data rows, never touches act-severity rows or the header counter, and a regression test pins the act-day rendering (act rows sort first, dismiss keeps them visible, counter stays truthful).
- Stored alert records are titled "Canary: watch" with a plain body ("Watch severity — market stress partly confirmed."); the "Open ibkr for portfolio details." call to action now appears only in the web-push payload, never inside the app. Push payloads remain redacted (no symbols, quantities, or account data).
- The alerts severity filter bar (All / Warnings / Info) was removed: with only exceptions rendered, the filter had no job left, and its labels were the page's only (accidental) color legend.
- Canary tape severity is now session-aware, closing the top follow-up from the trader-panel review: on official non-trading dates (weekends and holidays, per the embedded NYSE calendar) a frozen last-session SPY/VIX shock demotes to observe with "confirm at next open" guidance naming the reopening time, instead of holding "Freeze new risk" WATCH wallpaper all weekend, and the same frozen prints can no longer confirm stress, panic, carry unwind, or constructive tape anywhere in the canary. Trading-date pre/post-market moves keep full severity (live extended-hours prints are the row's documented purpose; the journal shows VIX printing all night on weekdays), and dates outside embedded calendar coverage keep legacy behavior, so historical replay is unchanged (all four sourced backtest corpora report identical metrics before and after). Tape evidence now carries the provenance stamp at the source ("market closed (weekend), frozen last-session prints"), which the Alerts page previously could only approximate in presentation. Six years of daily history motivated the demotion: 39 weekend-lit WATCH rows (roughly 434 stale-lit hours per year), 77% fully faded by the next close, and the real shocks re-light from live prints at the next open. Date-only replay observations are now clocked 15:59 ET inside their observation date rather than midnight UTC, which lands on the prior New York evening.
- The regime lifecycle stage machine now applies the same closed-date tape rule, completing the pass the canary change deferred: on official non-trading dates frozen SPY/VIX day-change prints can no longer enter or hold any tape-driven stage (panic, confirmed stress, early warning, opportunity, stabilization), co-sign a heuristic confirmation, or claim the pure-tape panic severity exemption, which is what held early_warning/watch on half-reset Saturday prints across the 2026-07-18/19 weekend journal. Cluster-driven evidence is untouched, so real cluster reds still warn and confirm on any date, and the lifecycle's tape evidence rows keep the frozen print's magnitude while reading forward-warning/observe/unconfirmed. Every regime snapshot and decisions-journal line now discloses the calendar classification (tape_session_state / tape_session), and the trading rulebook's regime-stage latch holds through closed dates so the last trading-date stage governs weekend rule thresholds via the existing carried worse-of path, instead of a weekend stage re-latching fresh in either direction. Weekday pre/post/overnight prints keep full effect, dates outside embedded calendar coverage fail open, and both sourced regime backtest corpora report identical metrics before and after.
- Brief movers now aggregate daily P&L by underlying (stock plus option legs per name, the same basis the Underlyings panel uses) and disclose the truncation residual as "N others" so the row reconciles to the account's daily attribution instead of silently dropping the tail; the Underlyings hero states the shared basis on screen ("Daily P/L by underlying · all held names · last session"). Closed sessions stamp themselves everywhere the number could read as live: the brief account row appends "market closed — daily P&L is from the last completed session" and the account header's "+x% today" becomes "+x% last session".
- Protection tiles stopped rendering unavailable as zero: "Actionable theta/day" shows "--" with the Greeks-coverage reason when option legs lack Greeks (a €0.00 there meant "could not evaluate", not "nothing pending"), and "No-stop exposure" shows the uncovered row count instead of "€0" when those rows cannot be valued (stale or delisted marks). "Premium at risk" keeps its amount but downgrades with "the protective share of this premium is unknown" whenever a hedge-candidate leg cannot be classified. The "Trim Δ-adjusted risk" picker and Preview grey out with the reason on the control while portfolio delta is unavailable, instead of offering a preview that cannot size.
- The rules card cross-links live events to their governing rules: upcoming held-name earnings while the earnings rules report unknown get an amber note naming both ("Earnings ahead (…) while rule 8 (At size before earnings) is unknown — the freeze cannot be confirmed"), and the brief's earnings event row escalates to attention with the same fact. Disclosure only; verdicts, ranking, and gating stay with the daemon.
- Brief copy nits from the same review: the canary row collapses "watch · watch" to one word (app and CLI), capital tokens carry labels ("Tier block · Enforcement shadow"), the sign-off button speaks plainly and scopes its claim ("Sign off this reconcile report — statement clean", exact report id in the tooltip and row value, plus an on-control caveat whenever the rulebook changed since the last stamped brief), weekly artefacts read "not yet completed this week", the exposure-composition legend drops sub-half-percent "0%" chips, and the screen-reader-only theta and FX spans now carry their labels.
- Every remaining passed-check number prints its policy trigger beside the observed value, extending the tape row's disclosure style to the whole checklist: margin cushion carries "(watch below 35%)", daily P&L "(watch at ±5%)", the exposure caps "(watch 150%)" per component, and the single-name concentration caps "(watch 35%)" — all read live from the risk policy, never hand-typed into copy. The reader now sees how far each number sits from the line that would flip the row, on the Alerts page, CLI, and MCP alike.
- The protection-coverage row names the largest unprotected position and its uncovered amount in the row copy itself ("Review largest unprotected stock/ETF exposures before adding risk; largest unprotected MSFT $ 12,000.00."), mirroring the Monitor protection panel's "largest unprotected SYM amount" wording; a largest-unprotected row whose notional cannot be valued still contributes its name.
- The daemon now stamps `portfolio_alert_relevant` on every canary snapshot, and both downstream relevance gates — the app's push/history gate and the Alerts page's preview gate — read the stamped verdict instead of each re-deriving the low-fit/exposure edge cases (the JS copy was one fit-semantics refactor away from silent drift after the recent portfolio-fit changes). The policy's single copy lives in `internal/canary`; an unstamped snapshot from an older daemon fails open to relevant, so version skew can add market-weather noise but never suppress alert delivery, and the compact alert view carries the stamp for scheduled monitors.
- The act-day regression test now pins the negative literally: the alerts header can never render "All clear" while any act or watch condition exists, before or after dismissing rows.
- Previous-context alert history now expires itself instead of waiting for the manual clear button (operator decision: 14 days). On every canary observation the app stamps records that still match the live context (fingerprint for canary-source records, account and mode for all sources); read records expire 14 days after they stop matching. Unread records and still-matching history never expire, an unknown live context expires nothing, and governance occurrences keep their separate 90-day retention.
- Earnings dates now resolve through typed, per-provider evidence instead of a single opaque lookup. Each symbol records its aggregate resolution plus every provider's latest attempt, next retry, last-good value, and typed redacted failure, and applicability is decided from the exact broker contract identity rather than the symbol string — a definitive `COMMON` classification, an unknown one, and an identity mismatch are three different answers, and only the first can carry an ordinary issuer's date. An observed Nasdaq envelope that legitimately carries no date is parsed as such rather than as a provider failure, a persisted resolution is upgraded when better evidence arrives, and a genuinely unsupported instrument is recorded as unsupported rather than retried forever. Coverage gaps are typed and distinguished from negative answers throughout. What the desk sees is the same rule as before, now harder to evade: rules 6, 7, and 8 report `unknown` — never pass — whenever an ordinary issuer's date is unknown or stale, that check runs before any stock-only, option-side, or size normalization, and on those rules `not_evaluated` is a trusted negative only when every input is covered.
- `ibkr rules` and `ibkr rules history` are now discoverable where every other command is listed: the CLI command table and the agent skill's command reference both carry them, with the history read labelled for what it is — persisted evaluator state transitions, not trade causality or broker evidence. The rulebook's own vocabulary was aligned at the same time: `rules.snapshot` emits `rulebook-fp-v3` as a policy-identity fingerprint of the compiled discipline model, so the docs now say "model" where they said "policy" and state plainly that a compiled model is not proof that every threshold carries operator approval.

### Fixed

- Sensors now refresh on a schedule that keeps their authority current instead of letting it lapse and then reacting. A daemon-owned Regime scheduler starts a normal refresh one full timeout ahead of the hard freshness boundary, so the ordinary case replaces the published value before it can expire, and a failed early attempt no longer suppresses recovery for another whole window. Dealer gamma schedules its next run from when the last one *completed* rather than when it started — the old start-based clock could ask the gateway for a fresh fan-out while a successful result was two minutes old — and a persisted result without an in-memory completion stamp is dated conservatively rather than treated as brand new. Dependent sensors recover as soon as their inputs are current again instead of waiting out the next cycle, and S&P 500 breadth publication survives a restart mid-computation rather than losing the in-flight window.
- Daily P&L health is now its own retained observation rather than a per-read guess. A detected regular-session failure is held until a newer valid frame proves recovery, so a failure raised at 16:20 no longer disappears merely because the session closed; the observation carries an explicit timestamp instead of inheriting the read's, after-hours reads are typed `not_due` rather than counted as missing data, and provider errors are normalized so the same underlying fault reads the same way every time. Only an opaque scope fingerprint and the value-free health state are persisted. The account projection behind it also stopped tripping over aggregate rows: correctly scoped aggregate ledger rows are admitted, and aggregate account rows the model does not describe are ignored rather than misread as position or ledger facts.
- Shadow Protection and Order Integrity can no longer clear from a mixed broker/journal moment. Their final decision is now bound to one exact socket session, order-lifecycle generation, structural portfolio generation, and local SQLite order-event head. Protection compares the complete all-client API snapshot in both directions: an API order outside the daemon journal or an open journaled order absent from the broker snapshot becomes explicit `unknown` / reconciliation-required evidence. Lifecycle persistence failure also latches the source unavailable until a complete, same-session, compare-and-append reconciliation succeeds.
- Reconnects can no longer move an already-authorized order instruction onto a replacement socket. Place, broker WhatIf, cancel, exercise, and all-client open-order snapshot requests carry the exact socket epoch at which their authority or request ID was claimed, use zero automatic retries, and write zero bytes if that epoch is no longer current. Rulebook shadow recovery is likewise fail-closed: it requires the exact fourteen governed rule rows and all five typed input-health rows, and accepts `not_evaluated` as a safe negative only for the explicitly defined terminal-issuer, off-session, and no-long-book cases.
- Dealer gamma can recover a strictly validated, quarantined legacy-import observation into a separate context-only last-good copy when SQLite has no current result. The immutable imported observation remains decision-ineligible, recovery is idempotent and provenance-stamped, and any fresh compute replaces the recovered context. A restart now serves the retained rankable result while the new fan-out runs instead of presenting a false cold start.
- Daily-brief Canary observations are timestamped after their account, positions, Regime, Rulebook, and market-event reads. The previous pre-read timestamp made healthy source rows appear to come from the future, so the shadow producer accumulated `source_time_invalid` coverage failures even though the Gateway and inputs were healthy. Genuine partial inputs still fail closed under their real reason.
- The Alerts status line no longer calls live pre-market prints "last-session". Observed live on the Monday pre-open: the header ticker counted down "pre-open · opening in 7:10" over moving indicative quotes while the status line claimed "market closed — prices are last-session prints" — wrong for the prints, right only for the daily-change anchors. The stamp now reads the same official-calendar session bounds as the header ticker and tells the price story per phase: pre-open on a trading date stamps "pre-market — live indicative prints; daily changes anchor to the last close", after the close it stamps "after close", weekends and holidays name themselves, and a session without calendar coverage claims nothing instead of defaulting to "market closed". The delivery-down banner made the deliberate companion call: pre-open on a trading date now counts as attention-on — pre-market alerts are exactly the ones a phone must deliver before the bell — so failing push delivery banners with "ahead of the open" honesty instead of staying quiet until 15:30 CEST, while after-close, weekend, and holiday phases keep the quiet disclosure-only presentation and an unknown session state still fails visible.
- The app browser smoke's attention-read guard guards again. The SPA's service worker claims its clients immediately, and WebKit never surfaces service-worker-controlled page fetches to Playwright's network routes — so the smoke's route-based interception silently let its QA acknowledge POST reach the real app host, exactly the "mark the operator's real unread as read" hazard the guard exists to prevent (benign in the observed runs only because nothing was unread). The guard now diverts the POST inside the page's wrapped fetch, before any network layer can see it, with the route kept as a second net; the governance not-observed fixture also caught up with the redesign and asserts the "waiting" chip instead of the raw enum.
- Weekend and holiday regime tape headers no longer drift: on official non-trading dates the SPY/VIX day-change fields pin to the official daily closes of the last two completed sessions, with new `spy_change_basis` / `vix_change_basis` provenance fields naming the sessions spanned. The gateway's last print and its tick-9 previous-close anchor can each reset independently while the market is closed, and live Sunday evidence caught the result: the header read SPY +0.00% beside VIX +12.19% (then SPY −1.09% half an hour later) while Friday truly closed SPY −0.99% / VIX +12.19% — a pair no market ever printed, rendered into tape evidence and the dashboard header. Bars are matched by exact official session date, so a stale series withholds the change (`fields_missing: spy_day_change` / `vix_day_change`) instead of quietly reporting a wrong-session move, and a failed pin never falls back to the drifted snapshot values. Trading dates, including pre- and post-market, keep the live tick-9 behavior; the closed-date key is the same embedded-calendar authority the canary's session-aware severity and the regime lifecycle's closed-date gating use.
- The canary could tell the operator "your exposure is low; keep watching — no reductions needed" when every exposure-measuring signal was blocked or data-quality (the "low" portfolio fit was a fall-through default, and the greeks-degraded escalation branch was unreachable dead code), converting missing Greeks into reassurance with a prescriptive instruction attached. Portfolio fit is now "unknown" when the measuring signals themselves are blind, the watch summary says "your portfolio exposure could not be measured from this snapshot; verify exposure before relying on this reading", live market pressure keeps its defensive watch frame instead of demoting to a data-quality footnote, and a regression test pins that data-blocked exposure signals can never yield a "low" fit.
- The risk-capital peak ratchet accepted equity observations from the wrong broker session: a paper-pinned daemon sharing the production state directory could silently ratchet the live adjusted peak with the paper account's equity (the proposals engine already scope-checks its snapshots; the capital store did not), overstating drawdown and consumed risk against a phantom peak — the trader-panel review caught exactly this state rendering as "OK". Capital state now binds to one broker identity: observations from a non-live mode, an unresolved scope, or a different account are refused and journaled ("equity_observation_rejected" with both identities), the state file records its owning account, and every accepted peak ratchet is journaled ("adjusted_peak_advanced" with before/after peak, the observed equity, and its timestamp). A new human-only, journaled `ibkr policy correct-peak` verb repairs a poisoned peak — corrections may only lower it, anchored to the retained-statement replay (`--from-statements`) or an explicit value with a mandatory reason — and deliberately never touches the drawdown latch, which records a real engagement and stays `reset-drawdown`'s job.
- `urgent`, the top canary severity, rendered with the informational blue tone in the alerts list; severity tones now map the daemon ladder exactly (observe < watch < act < urgent, order-mismatch `critical` included) and the copy-text sniffing fallback is gone.
- Protection evidence no longer renders a bare "unprotected € —" when the unprotected notional is zero or unavailable.
- Protective-stop mismatch alerts no longer sink into "previous context" the moment the canary fingerprint moves: fingerprint staleness now applies only to canary-source alerts, so order-mismatch alerts stay current until their account or mode context actually changes.
- Opening the Alerts tab before the first canary snapshot no longer floods "Needs attention" with the entire stored history (each stored record counted as a live condition); history now stays history until a staleness verdict is possible, and repeated records of one condition count once.
- A data race in connection teardown (caught intermittently by `make test`): `Disconnect` nulled the transport fields while the reader goroutine could still dereference them. Teardown now closes only the socket to unblock a parked reader and drops the buffered transport state after the goroutine wait succeeds; a reader that outlives the bounded wait keeps valid fields until the next connect attempt replaces them.

## v2.2.1 — 2026-07-19 19:49 CEST

### What's new

- **Ghost orders no longer haunt the orders tab.** Protective GTC stops that were cancelled or filled while the daemon wasn't listening used to stay "open" in the local order journal forever, showing up as stale "needs reconcile" rows. The daemon now asks the broker for its actual open-order list after each reconnect (and every 30 minutes) and closes journal rows the broker no longer reports as `Closed (reconciled)`. Broker statements remain authoritative for the final fill-vs-cancel outcome.
- **A stop that no longer matches its position now defends itself.** Sell part of a protected position outside the platform and the app flags the stop in red, explains that triggering it would open the excess in the opposite direction, sends one push notification, and offers a single guided fix: reduce the stop to the shares still held. Nothing adjusts automatically, and the daemon refuses the fix when position evidence is missing or has moved.
- **Releases no longer wait on a human for the registry step.** The MCP Registry entry is published automatically by a GitHub Actions workflow when the release goes out; the release pipeline now verifies that publish happened instead of prompting for a device code. The interactive login remains only as a fallback when the workflow fails.

### Added

- Broker open-order snapshot reconcile: `reqAllOpenOrders`-based sweep that appends terminal `reconciled-absent` events for journaled orders absent from a complete snapshot. Fail-safe by construction: no complete snapshot means no action, and only rows in the connected account/mode scope, with a broker PermID, and quiet past a grace window are eligible. The sweep never sends a broker write. The snapshot request is its only wire interaction, and the append-only journal is never rewritten.
- Protective-stop mismatch alerts and a guided fix. When a close-only stop no longer matches the position (for example after selling part of it in TWS), the daemon classifies the consequence (full or partial opposite-direction entry on trigger) and the app shows a red critical row with plain-language risk copy plus a single allowed action: reduce the stop to exactly the held quantity through the normal preview-and-confirm flow (flat positions keep Cancel as the offered fix). The daemon enforces the same reduce-only constraint server-side against a fresh positions read at both preview and confirm, failing closed when position evidence is unavailable or changed. A critical push notification fires once per occurrence via the existing alert path, debounced across two polling passes so transient zeroed position reads can never page the trader, and redacted like canary pushes (no symbols or quantities on the wire).

### Changed

- The release pipeline's registry leg is verify-first: after the GitHub release is created, it polls the public MCP Registry for the exact released version (about four minutes, reporting the Actions `registry-publish` run status while it waits) and succeeds without operator interaction once the workflow-published entry appears. Only on timeout does it fall back to the previous interactive device-code login and direct publish. Standalone `make registry-publish` keeps its direct login+publish behavior as the manual heal, and the release preflight reminder now notes a browser is needed only if the automated publish fails.

### Fixed

- A cancel request no longer downgrades an order's journaled transmit state, so a missed cancel confirmation can no longer wedge the row permanently write-ineligible ("open" yet not cancel-eligible); the cancel simply stays retryable until broker truth arrives.
- Position/order matching now accepts the wire's "STOCK" security-type label as equivalent to "STK". Protective orders journaled without a contract id (hand-previewed drafts, especially non-US routings) previously matched no position and were falsely classified as uncovered, which mislabeled healthy stops as critical and blocked their legitimate modifies. Caught by the paper-account rehearsal on the first real modify attempt.
- Broker open-order snapshots cannot mint phantom journal rows: unmatched openOrder/orderStatus callbacks (e.g. manual TWS orders) are observed but not adopted into the daemon's order journal.
- `ibkr status` background-task line for open orders now leads with the current account/mode count (matching the orders tab) and discloses any off-scope remainder as `(+N other scope)` instead of silently reporting a different total.

### Removed

- The Codex delegation lane (agent development process, no product change): the implementation-lane hook, delegation runner, waiver valve, and lane policy are retired, and agent sessions implement inline again. The lane and its reactivation runbook are archived in `docs/archive/codex-delegation-lane.md`.

## v2.2.0 — 2026-07-19 07:22 CEST

### What's new

- **The risk constitution now nudges instead of nagging.** Governance follow-ups that used to rely on the operator remembering them — reconcile coming due, a drawdown tier consuming its budget, policy drift between siblings — surface as advisory nudges in the paired app and the daily brief. Everything stays shadow/advisory: no nudge blocks an order, and the monthly governance pulse is acknowledged with one tap on the paired device. **Action required:** none; nudges activate only under an approved v4 risk policy and stay silent otherwise.
- **The regime dashboard's gamma indicator measures the market again.** A gateway quirk was silently zeroing every call leg's open interest, which forced the dealer-gamma reading permanently negative — a data artifact, not a market signal. Open-interest capture is now right-aware, and per-leg call OI in `ibkr chain` is correct again.
- The HYG/SPY credit row now degrades gracefully to the latest official close when a live tick is missing, instead of erroring out and destabilizing regime stage transitions.

### Added

- Risk-governance nudges (advisory/shadow): the daemon derives typed nudge candidates — reconcile due, drawdown tier consumption, sibling policy drift, monthly governance pulse — strictly from active, fully approved, internally consistent v4 policy authority; any unhealthy or pre-v4 posture suppresses candidates with a disclosed typed reason rather than a clean-empty result. Shadow occurrences are timestamped at the first qualifying preview and survive restarts.
- `nudges.cutover_review`: a fixed-shape, paired-device-only acknowledgement for the governance cutover review. The daemon authenticates the paired session, revalidates the canonical policy report at write time, pins the reviewed coverage and report fingerprint into daemon-owned evidence, and keeps identical retries idempotent while conflicting or stale retries fail closed. It acknowledges review only — it cannot submit broker writes, change freeze or limits, or weaken policy.
- The paired app delivers nudges through alerts and the governance panel, and `POST /api/governance/cutover-review` accepts only an empty body: origin and evidence are fixed server-side, hostile payloads are rejected before the daemon call, and daemon errors return without private text. The daily brief (`ibkr brief`) renders the monthly governance aggregate.

### Security

- `PATCH /api/settings` on the paired-app route now rejects any `trading.*` patch with a 400 before the daemon is called (the CLI remains the only trading-settings surface), and assigns `origin` server-side as `paired_device`, discarding whatever origin the client body claims.

### Fixed

- Per-leg call open interest is no longer zeroed by the gateway's put-side companion tick. The gateway delivers a call-OI and a put-OI tick on the same option-leg subscription — the opposite-right one carrying zero — and last-write-wins left every call leg at OI 0 (observed: all 6,991 journaled SPX call contracts across five weeks, while puts stayed positive). With call legs zeroed, the dealer-GEX profile became put-only, every sweep point went negative, and the regime dashboard's gamma row was pinned to a permanent "wholly short" reading with `no_crossing_in_window` on 100% of computes — a measurement artifact, not a market state. `ibkr chain` per-leg call OI was corrupted the same way. Open-interest capture is now right-aware: a subscription commits only the tick matching its own call/put right, and the zero companion is discarded. Gamma journal rows recorded before this fix (regime gamma decisions and `gamma_sign` diagnostics) are artifact data — calibration windows restart at the fix.
- The regime dashboard's HYG/SPY row no longer drops to `error` when the HYG spot subscribe delivers no tick: the fetcher now falls through to the latest official daily close as banding input at any hour and marks the row `stale` (disclosed via `fields_missing: hyg_spot_tick` and the quality label). An errored row blanked the band, which bypassed red-exit hysteresis and the confirmation latch — a latched red de-confirmed and re-confirmed around every transient gateway blip (8,012 stage transitions in 5 weeks, median dwell 34 seconds, including 2,344 on a closed-market holiday). A stale row keeps its band, so confirmations now hold through fetch failures; a genuinely unavailable close (no spot tick and no usable history) still lands `error`. Off-hours behavior with a live tick is unchanged: official close, status `ok`.
- Market-data subscribe errors in the daemon's snapshot helpers are now logged with the symbol instead of silently discarded, so gateway farm blips and silent subscribe timeouts are distinguishable in the daemon log.

## v2.1.1 — 2026-07-18 20:55 CEST

### What's new

- **Maintenance release for the release pipeline itself** — no functional changes to the CLI, MCP tools, or paired app. The release credential preflight now matches the MCP Registry's short-lived login tokens (about five minutes), so releases no longer gate on a token-validity minimum that could never pass.

### Changed

- The release credential preflight no longer gates on stored MCP Registry JWT validity: registry JWTs live only about five minutes, so the former 30-minute minimum (and its `REGISTRY_TOKEN_MIN_VALID_MINUTES` override) could never pass, and a start-of-pipeline refresh would expire before the registry leg anyway. The preflight now verifies gh auth and the publish tooling, and fails fast only when the registry leg's automatic device-code login is disabled (`MCP_REGISTRY_AUTO_LOGIN=0`); the login itself always happens at the registry-publish leg with the operator at a browser.

## v2.1.0 — 2026-07-18 20:28 CEST

### What's new

- **A personal risk constitution now governs capital discipline** (advisory/shadow — nothing blocks orders). The operator writes every number into `~/.config/ibkr/policies/risk-policy.toml` (no defaults in code): protected equity floor, declared risk capital, a two-tier drawdown ladder measured from a cash-flow-adjusted peak with a latching shadow block, one-shot expiring overrides, and reconciliation tolerances. `ibkr policy show --explain` renders the whole contract in plain language.
- **Broker statements are ingested and reconciled daily.** A `[flex]` config section fetches IBKR Flex statements on a daily schedule (immutable raw retention), a recon engine matches statement flows against the declared ledger with never-false-match semantics, and `ibkr recon` renders the report. At policy v3, broker-confirmed flows become the authoritative capital-flow input and a fully clean, fresh report extends the reconcile clock automatically — recurring sign-off chores are retired; exceptions, and only exceptions, require the human. `ibkr recon backtest` replays the full statement window (complete flow review plus capital-ladder equity replay) for validation before trust.
- **Agent sessions now implement through a hook-enforced Codex-only lane** (`scripts/codex-implement.sh`): planning, review, and integration stay in the orchestrating session, implementation runs headless in an isolated sibling worktree with fail-closed approvals, and inline code edits by agent sessions are deterministically blocked (human-approved break-glass: `scripts/waive-inline.sh`). **Action required:** none for CLI/MCP/app users; repo contributors using agent sessions get the new flow automatically.

### Added

- Risk constitution phase 1: `ibkr policy` (show/--explain, capital-event, override, reset-drawdown, artefact) over an operator-authored `risk-policy.toml`; capital-event journal with replay; drawdown warn/block ladder where the block latches until a journaled human reset; policy fingerprinting and sibling-policy drift display. All five policy write verbs are human-origin-only at the daemon.
- Post-trade truth (phase 3a): `[flex]` daily statement ingestion with single-purpose token-file rules; `internal/flexstmt` typed parser — unknown line types become loud `uncategorized` exceptions, never actions; recon engine + `ibkr recon [--json]`; the reconcile verb is report-gated (bare attestation retired) with a one-shot override as the statement-outage valve.
- Recon backtest: `ibkr recon backtest` reviews every flow in the retained statement window and replays the daily equity series against the recorded peak and drawdown-ladder crossings; the equity comparison is same-day (statement close vs that day's runtime observation) instead of latest-vs-latest.
- Risk-policy v3: clean-report auto-extend of the reconcile clock behind a strict same-day equity-divergence gate (new operator-authored key `recon.max_equity_divergence_pct`); statement-authoritative cumulative flows with declarations demoted to provisional same-day bridge entries; new non-exception `confirmed` category for broker-confirmed undeclared flows; statement value-date peak correction, exactly once per statement line; declared-vs-statement figures displayed side by side during the transition. Pre-genesis statement flows classify as disclosed, report-id-pinned `baseline` rows — never exceptions, never signature-gated.
- Codex delegation runner with a transactional lifecycle (fresh-task/resume/cleanup guards), pinned model/effort/service tier per run, offline-only gates for delegates, and a project execpolicy that prompts on live targets.
- The paired app's monitor tab opens with a daemon-composed daily brief: five sections (market, calendar, portfolio, risk, process) built from existing typed results with per-row disclosed degradation. Viewing stamps the brief once per foreground look with a server-assigned human origin — agent renders never stamp — and a one-tap sign-off submits the reconcile attestation for exactly the report the operator saw. A governance write, not a broker write; the CLI renders the same snapshot.
- `ibkr app devices` lists paired-device grants, and `ibkr app devices prune [--keep-days N]` (default 7) retires stale grants and their push subscriptions while keeping freshly-paired-but-unused devices (activity is the later of created and last-seen). The endpoints answer only local-Mac callers and the relay refuses to forward the device paths, so relayed requests cannot reach them.

### Changed

- `pkg/ibkr` constructors now clone caller-supplied configs — later mutations of the caller's config struct no longer leak into a live client — and stale nil-context doc promises are dropped.
- Rulebook policy fingerprint moved to a JSON projection (`rulebook-fp-v3`); the rules-decisions journal now records the policy fingerprint with every transition, so journaled verdicts name the policy identity that produced them.
- Runtime settings surfaces are single-sourced from the settings registry behind the generated reference — daemon flatten/apply, CLI grammar/help, and generated docs consume one table, parity-gated.
- `ibkr policy show` renders capital state as plain-language sentences (risk brake, money at risk, ledger check) instead of raw field dumps.
- The Canary SPA is split into ES-module feature files behind a drift gate, and the canary engine moved from `internal/cli` into `internal/canary`; no rendered-output change.
- CLI gamma open-interest warnings trust the daemon's session classification instead of reclassifying locally.
- Proposal-outcome deduplication keys are cached; dead outcome fields and unused MCP modes were removed from the proposal surface.
- `make release` now preflights credentials before any gate work: gh CLI auth plus the stored MCP Registry JWT's expiry (30-minute minimum, `REGISTRY_TOKEN_MIN_VALID_MINUTES` overrides), running the interactive device-code refresh up front instead of stranding at the final registry-publish leg.

### Fixed

- Option days-to-expiry calculations were off by one across DST boundaries (both copies of the date math agree again).
- Flex statement lines with a missing amount attribute parse as unavailable and surface as loud `uncategorized` exceptions, instead of fabricating an amount of 0.00; an explicit zero stays present.
- The documented reconcile-clock outage valve — a one-shot override on `capital.max_unreconciled_days` — was recorded, journaled, and displayed but never consumed by evaluation; it now extends the clock, for exactly that one control.
- Paired PWA sessions survive daemon restarts, binary rebuilds, and iOS home-screen isolation; pairing state no longer resets.
- Opportunity refresh backoff is capped at 15 minutes and shared across engines, so a transient failure no longer parks a feed for hours.
- Integration tests against a live gateway no longer race the async handshake (they poll up to 25 s), and closed-market skips key on typed calendar/session authorities, never on error text.
- The paired app's market-session clock renders in the browser's locale instead of hard-coded en-US string surgery.

### Engineering notes

- Remote-relay unit tests and the embedded PWA asset-syntax check are bound into `make check`; delegated Go briefs require `make check` in-worktree.
- v2 policy files cannot drift spuriously under the new binary: the pre-v3 constitution-fingerprint projection and the v2 recon report-id projection are preserved byte-for-byte, regression-tested.
- The browser smoke now covers settings-tab navigation and visibility — previously the only panel without id-contract coverage.

## v2.0.0 — 2026-07-11 22:41 CEST

### What's new

- **v2.0.0 is a Go-library major, not a product rewrite.** The `ibkr` CLI, daemon, MCP tools, and paired app behave the same as v1.15 plus the improvements below; the major bump exists because the published `pkg/ibkr` Go API modernized in one breaking pass (context-first methods, no `Get` prefixes — details under Changed) and Go-module semver therefore requires the module path to move to `github.com/osauer/ibkr/v2`. **Action required** only for Go developers importing the library: update imports to `github.com/osauer/ibkr/v2/pkg/ibkr` and installs to `go install github.com/osauer/ibkr/v2/cmd/ibkr@latest`. Binary, MCPB, and plugin users are unaffected.
- Trading rulebook v2, from the 2026-07-08 dual senior review against a live confirmed-stress tape. The checklist grows from 12 to 14 rules: **rule 13 `exit_discipline`** fences long-option losses at −40% of premium paid (watch) / −60% (act), with classified hedge legs exempt — the hedge is allowed to decay — and an advisory preview warning when a draft would average down into a flagged line (averaging down resets the basis; the fence does not follow it down). **Rule 14 `fx_exposure`** makes non-base-currency NLV explicit (watch-only above 60% — a structural breach must inform, not become a permanently lit act). Rules 3/4/12 thresholds are now **regime-conditional**: the daemon latches the regime lifecycle stage (persisted across restarts, so a bounce mid-stress cannot reset thresholds to calm), buckets it calm / early_warning / confirmed (cash floor −25/0/+10% of NLV; ex-hedge extrinsic budget 10-15/7.5-12/5-10%; hedge band 25-35/30-50/40-70%), and a carried or never-seen stage evaluates BOTH the carried and calm sets and reports the worse verdict — stale regime data can hold or tighten a verdict, never relax it, in either band direction. Partial data may now indict but never acquit: rule 1 reports provable lower-bound breaches ("at least X% of NLV", `observed_is_lower_bound`) from signed interval arithmetic when deltas are missing, instead of hiding a certain breach behind `unknown`.

### Added

- Rule 2 hedge-premium tier: option lines classified as rule-12 hedges measure against 15%/25% of NLV instead of the 5%/10% speculative tier, disclosed per offender — the designated hedge no longer tops the premium list every single day (alarm-fatigue fix), while rule 12 gains an act tier at more than twice the band top ("a directional short wearing a hedge's clothing"). The two changes ship atomically: a hedge past twice the band top always keeps an act somewhere.
- Rule 7 now covers short puts spanning an earnings print: watch by default, act when the line's assignment notional reaches 10% of NLV (or a name's spanning short puts together reach 20%) — a gap through the strike is a forced size-up on earnings day.
- Option legs whose greeks tick carried no underlying spot now borrow the same-name stock leg's account mark (disclosed as `stock_leg_mark`, quality-gated, never classifying hedge legs), so rules 4 and 6 evaluate pre-market for stock-backed names instead of reporting unknown until the option surface opens.
- The rules result's `input_health` gains a `regime_stage` row; row notes disclose which regime threshold set applied and why.

### Fixed

- Rule 6 (`catalyst_coverage`) could assert **pass** with every option leg's OTM-ness unassessable: legs with no underlying spot were silently skipped, so pre-market the rule passed a book holding an OTM call that expires five days before its name's earnings — a live violation of the never-false-pass invariant, observed 2026-07-08. Such legs are now named unknowns, and the property test covers per-leg nil underlying with healthy positions.
- Rule 8 (`earnings_size_freeze`) had the same silent-skip shape: names with material greeks gaps were dropped from the sweep entirely, letting the rule pass while rule 1 reported the same name unknown. Gapped names now stay unknown offenders unless earnings are provably beyond the freeze window.
- The rules-decisions journal now creates its state directory before appending (matches the order/regime journals); previously a missing `~/.local/state/ibkr/` made journaling silently no-op.
- Gateway-down reconnect attempts are now paced by an exponential backoff (1s→2s→4s→8s, capped at 15s), and the per-attempt "connection failed" line dropped from Error to Debug. Reconnection is demand-driven — every read handler that finds the connector down triggers one — and a refused dial returns instantly, so a multi-hour outage retried ~2.6×/s and wrote **66,900 identical `connection refused` lines** to `ibkr-daemon.log` over a single 7h gateway outage (observed 2026-07-08). The streak resets on a completed handshake and stays exempt on the first attempt, so a fresh drop still reconnects immediately; the 15s cap stays below the CLI's 25s `ibkr status` handshake wait, so a Gateway→TWS switch still recovers in one invocation.

### Changed

- **Breaking — `pkg/ibkr` Go API, and the module path is now `github.com/osauer/ibkr/v2`.** The published client library dropped its pre-context idioms in one pass; the `ibkr` CLI, daemon, MCP, and app surfaces are unaffected. Accessors lose the `Get` prefix (`CachedPositions`, `OptionIV`, `OptionGreeks`, `OptionUnderlyingPrice`, `OptionQuoteBidAsk`, `OptionPrevClose`, `MarketDataTypeForSymbol`, `Connection.MarketDataType`), and `GetMarketData` is now `MarketDataSnapshot` — the bare name would collide with the `MarketData` type. The five `X`/`XCtx` method pairs collapsed to one ctx-first method each: `FetchHistoricalDailyBars(ctx, symbol, lookbackDays, timeout)`, `FetchHistoricalDailyBarsWhatToShow(ctx, symbol, lookbackDays, whatToShow, timeout)`, `FetchHistoricalDailyBarsWithContract(ctx, contract, lookbackDays, timeout)` — timeout ≤ 0 keeps the 45 s default, and a ctx deadline always caps the effective budget — plus `Connection.RequestHistoricalData(ctx, …)` and `Connection.CancelHistoricalData(ctx, reqID)`. Nil contexts are no longer tolerated (the 21 per-method `ctx == nil` guards are gone; pass `context.Background()` explicitly). The unused `Order` struct left the public surface (internal `trackedOrder` now); `IBKROrder` stays the wire type and the `OrderSide`/`OrderType`/`TimeInForce`/`OrderStatus` enums are unchanged.
- Rulebook policy `rulebook-v1` → `rulebook-v2` (version 2): every new threshold is part of the policy fingerprint, and the flat rule-3/4/12 threshold fields moved into the three regime sets (the calm set carries the v1 numbers unchanged).
- Design doc corrected where it had drifted from the implementation: the rules-decisions journal lives under `~/.local/state/ibkr/`, rules 9/10 read a dedicated best-effort SPY snapshot quote (not the regime snapshot), and the operator TOML policy override is documented as planned, not shipped.

### Engineering notes

- Repo-wide taste-review pass (2026-07-11), no behavior or rendered-output changes: the ±2% dealer-gamma band and its gap→regime classifier now live once in `internal/rpc/regime_policy.go` (`GammaRegimeFromGap`, `GammaBucketRegime`, `HeuristicThresholds`) instead of six hand-coded copies across daemon and CLI — the same single-copy treatment the 2026-06-12 incident forced on regime headlines; fixed-fractional sizing moved from the CLI adapter into `internal/risk` (`risk.ComputeSize`), so MCP no longer imports a sibling adapter for policy math; `computeGammaZeroFor` decomposed from 604 to 399 lines (contract prewarm and leg fan-out extracted with their abort machinery); dead SPA CSS and an inert `IBKR_HANDSHAKE_PERMISSIVE` test knob deleted.

## v1.15.1 — 2026-07-08 08:42 CEST

### What's new

- Inactive-symbol suppression can no longer permanently poison the daemon. During IBKR's nightly server reset the gateway can answer "No security definition" for every request while nominally connected; on 2026-07-08 that marked healthy held names (AMD, BB, IBM) and the regime engine's VIX feed as delisted, persisted the marks to a state file shared by every daemon instance, hid the held rows from `ibkr positions`, failed quotes with `symbol_inactive`, and blocked the v1.15.0 release smoke — recoverable only by hand-editing `~/.local/state/ibkr/inactive-symbols.json`. Suppression is now a bounded in-memory cache instead of a permanent on-disk verdict: no candidate counting while any market-data / historical / sec-def / connectivity farm is impaired, marks expire after 12 hours (re-marking needs fresh confirmation), an inactive mark never hides a held stock row, and `ibkr restart` clears everything. A genuinely delisted ticker still gets suppressed within two poll cycles of daemon start and now costs only two probe errors per half-day instead of permanent state.

### Changed

- Security bumps for the 2026-07-08 advisory batch: `golang.org/x/crypto` 0.41.0→0.52.0 and `github.com/golang-jwt/jwt/v5` 5.2.1→5.2.2 (both indirect; `govulncheck` reported zero reachable vulnerabilities in shipped binaries, so v1.15.0 was not exposed), and the Cloudflare relay's dev tooling moved to `wrangler` 4.108.0, clearing six `undici` advisories in the lockfile (dev-only; the deployed worker runtime does not consume that `undici`).

### Fixed

- Removed the persistent inactive-symbol store (`inactive-symbols.json`) entirely. It was added 2026-06-19 to save re-confirming dead tickers across restarts — worth roughly two wire messages per daemon rebuild — and in exchange created the poisoning class above: cross-process shared state with no TTL, no self-heal, no ops surface, and a persistence filter keyed on exactly the error message a degraded gateway mass-produces. In-memory marks (which fixed the original delisted-zombie churn loop and are unchanged) carry the suppression; the state file is no longer read or written and can be deleted.
- Release-smoke, wire-smoke, and release-verify daemons now run with an isolated `XDG_STATE_HOME`/`XDG_CACHE_HOME` (as the paper smoke already did), so operator daemon state can neither fail a release nor be written to by smoke runs.
## v1.15.0 — 2026-07-07 19:31 CEST

### What's new

- New advisory trading rulebook: `ibkr rules` (and the `ibkr_rules` MCP tool) evaluates a 12-rule daily discipline checklist against the live book — per-name exposure cap, single option-line premium cap, negative-cash sell-only mode, portfolio extrinsic (theta-rent) budget, expiry runway, earnings catalyst coverage, overwrites spanning earnings, pre-earnings size freeze, red-on-green-tape weakness, winner-trim into strength, a green-day execution nudge, and hedge-band integrity — ranked hardest-first. Earnings dates come from a free per-symbol fetch with manual overrides (`ibkr settings set features.rulebook.earnings_overrides`), cached last-known-good across restarts. Statuses are pass/info/watch/act/unknown/not_evaluated, and missing inputs always degrade to `unknown` — a cold daemon or unknown earnings date can never render a false pass. The Canary app shows a compact Rules card beside the portfolio hero with a full-checklist drill-in, and order previews carry advisory `rule_*` warnings when a draft would worsen a breached rule; nothing in the rulebook affects submit eligibility or any broker-write gate, and `features.rulebook.enabled=false` turns the whole surface off.

- Corrected a long-standing mislabel in account P&L: the `daily_pnl_unrealized` / `daily_pnl_realized` fields are renamed to `pnl_unrealized_total` / `pnl_realized_total`. They carry the account's inception-to-now TOTAL unrealized/realized P&L from IBKR's reqPnL stream — not a breakdown of the daily figure, so they never summed to `daily_pnl`. The `ibkr account` CLI now shows them under a "Total P&L (reqPnL stream)" block instead of the misleading "Daily P&L breakdown / of which unrealized/realized".
- `ibkr status` now reports the quote / scanner / history / chain subsystems as `ready` when the market-data farms are actually connected. They were stuck showing `degraded` ("no market-data farm connection notice observed") for the whole session even while quotes, chains, and gamma worked — a false alarm, now fixed.
- Canary app money metrics no longer flap between currencies and formats across refreshes ("No-stop exposure" flipping €12K↔"3 rows", winner/loser daily P/L flipping $↔€, "Risk over target" flipping $24K↔"--"). The protection panel's theta/day and risk-over-target now render in the account base currency next to no-stop exposure, with an explicit "Mixed"/"Review" disclosure when a one-currency total is genuinely unavailable — never a silent USD label on a sum that isn't USD.
- Canary app mobile usability pass. Open orders now read like orders instead of journal rows: the status line shows "Pre-submitted · At broker" instead of raw `pre_submitted / broker_acknowledged / ibkr-…` internals (full detail moves to the tooltip), the quantity/trail/stop/offset edit boxes carry visible labels, and the order type / TIF line no longer clips. Tapping an alert on the Alerts tab expands its full text in place — it previously activated a detail panel on the hidden Monitor tab, so the tap looked dead and truncated alert text was unreachable.

### Added

- Trading rulebook engine (`docs/design/trading-rulebook.md`): daemon RPC `rules.snapshot` returning `RulesResult` (12 `RuleRow`s with observed/threshold/evidence/offenders, hardest-first `ranked`, `breach_counts`, result-level `input_health`, per-name `earnings[]` provenance, policy fingerprint); pure evaluator in `internal/risk` with a never-false-pass property test; Nasdaq earnings fetcher with last-known-good cache (`~/.cache/ibkr/earnings-dates.json`, async refresh off the snapshot path, per-symbol failure backoff); runtime settings `features.rulebook.enabled` and `features.rulebook.earnings_overrides`; advisory `rule_*` DataWarnings on order previews (reduce/close intents never warn); rules-decisions transition journal; `snapshot.rules` sibling section + `rules` SSE event in the app layer; SPA Rules card with drill-in; `hook-behavior-check` make gate running a 19-case table-driven test of the plugin safety hook, whose read-only allowlist now covers bare `ibkr orders`.

- Trade-proposal snapshots now carry base-currency twins of the money aggregates: per-proposal `theta_per_day_base` / `risk_excess_notional_base` and counts-level `theta_per_day_base`, `risk_reduction_excess_notional_base`, `base_currency`, plus `theta_per_day_currency` labeling the existing raw theta sum (omitted when the contributing proposals span currencies). All additive; nil/omitted means unavailable, never zero.

### Changed

- Renamed account-financials fields `daily_pnl_unrealized`→`pnl_unrealized_total` and `daily_pnl_realized`→`pnl_realized_total` across `ibkr account --json` and the `ibkr_account` MCP tool, and relabeled the corresponding CLI block. These are the reqPnL-stream account totals (inception to now) and stay distinct from the account-updates `unrealized_pnl` / `realized_pnl` — the two total-unrealized measurements come off different gateway feeds and can legitimately differ.

- Decluttered the Canary app's repeated chrome: panel freshness reads "updated now" when fresh (the clock time moved to the tooltip; aged and stale renderings are unchanged), the Market & Portfolio head collapses its two identical regime/canary timestamps into one, the exposure-composition bar shows percentages only in the legend instead of twice, redundant per-row "current signal" chips are gone from the Alerts tab's Current-signal section, the Opportunities hero metric is labeled "Exercises" (the panel is exercise-only, and the count includes not-yet-eligible rows), "Purge all!" lost its exclamation mark, and the brand header is more compact so more of a phone screen shows data.

### Fixed

- Tightened rule 1's hedge exemption in the trading rulebook to match what rule 12 actually sizes. A policy-hedge index name with net-short delta is exempted from the per-name concentration cap only up to the short delta-dollars of its rule-12-classified hedge legs (long puts with delta present); short stock or short calls in a hedge symbol, and any residual short beyond the sized legs, count toward concentration as usual, with the sized portion still disclosed in the row's `exempt` list.
- A portfolio risk-off sweep retried with the same `request_ref` inside the 90-second dedupe window now re-checks the broker-write gate before replaying the cached result, so a trading freeze engaged between the two calls surfaces as a blocker instead of replaying a pre-freeze "ready" preview (the stale preview's token was already unredeemable under freeze — this fixes the misleading status, not an order path). The freeze blocker's action text now spells out its scope: new orders, modifies, and reduce sweeps are all blocked while frozen; only cancels remain allowed.
- Corrected two stale safety-policy claims in `docs/design/trading-paper-smoke.md` that predated the 2026-06-20 agent-origin policy flip: it still described a daemon-side agent-origin live block and an agent restriction on paper-smoke, neither of which exists — origin is audit/policy metadata, and every broker write is gated by pins, mode, preview tokens, WhatIf eligibility, and freeze for all origins. The agent-policy doc drift test now guards this file and both stale phrasings.
- Fixed the Canary app's selected-alert "Clear" button rendering white-on-white: it used the dark-panel button variant on a light panel and stretched to the panel's full height; it now renders as a normal light-context button.
- Fixed a false-degraded market-data-farm status. The gateway sends its farm-status burst (`2104`/`2106`/`2158` "connection is OK") immediately after the API handshake, and the connection read loop consumed that burst before the connector wired its system-notice handler — so the notices were logged but dropped, and because the gateway only re-sends farm status on change, the connector's farm view stayed empty for the rest of the session. `ibkr status` then reported quote / scanner / history / chain as `degraded` even though the farms were connected and data flowed normally. System notices arriving before the handler is wired are now buffered and replayed when it is set (mirroring the existing order-message pending buffer), so a farm that has announced "OK" is reported ready and still flips back to `degraded` on a `2103`/`2105` disconnect.
- Stopped the daemon log from ballooning (observed at 1.5 GB). IBKR re-sends `orderStatus` frames for unchanged working orders many times per second, and each was logged at INFO; a single resting PreSubmitted order could write thousands of identical lines per minute. Only genuine status/filled/remaining transitions log at INFO now — verbatim repeats drop to Debug. Order state tracking is unchanged. The daemon log additionally rotates at boot when it exceeds 64 MiB, keeping one previous generation (`ibkr-daemon.log.1`), so the file no longer grows without bound across restarts.
- Fixed the root cause of the Canary app's flapping money metrics: the daemon re-derived the base-currency FX ledger on every positions/account request, and one transient FX snapshot-quote timeout (budget 1.2s per pair direction) zeroed the rate and stripped every `fx_rate` / `*_base` field — including `protection_coverage.unprotected_notional_base` — from that single response, also flipping the positions fingerprint and staling proposal revisions mid-confirm. The daemon now keeps a last-known-good FX-rate cache (rates only, never account balances): a rate fresher than 15 minutes is served without a gateway quote (the app host polls every 5s and used to pay up to 2×1.2s per suspicious currency per poll), a failed live resolution falls back to the cached rate for up to 72 hours (spanning weekends), and serve-mode transitions log once per episode instead of once per poll. The cache also persists across daemon restarts (rates only, `~/.cache/ibkr/fx-rates.json`, best-effort): a restart during IBKR's nightly server-reset window or over a weekend re-serves last-known-good rates immediately instead of starting cold with stripped base fields until the first live resolution.
- Removed the Canary app's remaining hardcoded-USD fallbacks. Every money formatter (`money`, `compactMoney`, `compactWholeMoney`, `riskMoney`, and the protection-coverage base-currency resolver) now renders an amount whose currency is genuinely unknown as a bare number — with a tooltip disclosure on the no-stop-exposure and opportunities-gain hero metrics — instead of silently labeling it USD. The audit also fixed two real mislabels on non-USD-base accounts: the Settings trading-limits line rendered `[trading].max_notional` (defined in the account currency) with a fixed `$`, and the portfolio risk-off sweep labeled each leg's "cuts ~X of risk" (a base-currency delta-adjusted amount) with the leg's contract currency. A static contract test now forbids any `"USD"` literal in the app source.
- Stopped WARN-logging routine "No security definition" (code 200) gateway notices for derivative/probe requests. Option-chain fan-outs (gamma, proposals/theta scans on held names) deliberately subscribe the secDefOptParams strike superset, where some (expiry, strike, right) triples are unlisted — the gamma engine already counts these as `contract_missing_legs` — and FX resolution quotes both directions of a currency pair when only one is listed, with the FX cache absorbing the miss. Each such designed-to-miss probe still echoed a WARN per request: ~12 per 15-minute gamma cycle all session, and 2,102 in a single hour during an FX-degraded episode on 2026-07-07 when every 5-second app-host poll retried live FX resolution. Definition-miss notices for OPT/CASH-aliased requests now log at Debug; stock and alias-less requests keep the WARN trail (that's the signal feeding dead-symbol inactive marking). System-notice request tags now include the security type (`reqID=42 (USD CASH)` instead of `(USD)`), naming the failing contract class directly.
- Removed a per-request WARN + full goroutine stack dump (`reqContractData anomaly`) from the daemon log. The leftover debugging tripwire fired on every by-fields stock contract-details request — the normal resolution path behind order previews (min-tick lookup) and quote historical fallbacks, which by design sends an empty primary exchange with conID=0 — writing a multi-KB stack trace per occurrence (179 in one log file; three routine order previews produced three dumps). The wire-shape trace survives at Debug without the stack for wire-level forensics; contract resolution itself is unchanged.
- Quieted the off-hours gateway-reconnect log flood. While TWS / IB Gateway is closed the daemon retries the connection on its normal cadence, and each attempt logged its full multi-line connect sequence (`Starting connection process`, `Client N: Connecting to …`, `Failed to connect … connection refused`, `Daemon up but gateway not connected`, plus the connector start/stop narration) at INFO/WARN — roughly 50k lines over a 13.5h overnight window, a larger contributor to log growth than the order-status flood above. The daemon now logs the transition into "gateway unreachable" once at WARN, demotes the identical per-cycle repeats (and the low-level connect narration) to Debug, and logs a one-line "Gateway reachable again" when the handshake recovers. Reconnect timing, backoff, and retry counts are untouched — this is a logging-volume change only.
- Hardened the opportunities subsystem's refresh loop to the proposals engine's standard. Three fixes: (1) failure streaks were double-counted — the scheduled loop recorded every outcome twice — so `ibkr status` reported "refresh blocked 4 consecutive times" after two boot-race attempts and flagged the subsystem `degraded` at half the intended threshold; (2) a transient account/positions fetch failure (every daemon boot, any gateway outage) replaced the served snapshot — including the last-good one just re-adopted from `opportunities-current.json` — with an empty blocker shell; transient failures now preserve the same-scope, same-policy snapshot with the blocker disclosed, and the portfolio-stream `positions_pending` guard from proposals now applies to opportunities too, so an unprimed empty position list can no longer generate a wrong empty snapshot; (3) blocked refreshes never logged — the subsystem now emits the same throttled warn trail and recovery line as proposals, and the degraded status row reports the actually-served snapshot's `as_of` and the streak's true start time instead of internal bookkeeping that rendered as `0001-01-01T00:00:00Z`. Operator-owned states (`opportunities_disabled`, policy drift) no longer count toward the refresh-failure streak.

## v1.14.2 — 2026-06-20 11:37 CEST

### What's new

- Tightened the agent-facing trading contract for CLI, MCP, hooks, and docs so live and paper broker writes use one connected-gateway gate instead of stale origin-only policy text.
- Made local order and opportunity read/preview surfaces safer for agentic workflows by separating local-journal evidence, preview-token minting, and submit eligibility.

### Changed

- Updated trading status to report `gateway_unavailable` and keep `can_preview=false` / `can_write=false` while TWS or IB Gateway is disconnected or still handshaking.
- Aligned CLI and MCP order-preview routing fields, replacement previews, and trailing-stop trigger-method support across help text, schemas, generated docs, and skill guidance.
- Updated the Claude/Codex hook policy to allow read-only help/piped reads while gating broker-adjacent writes through current trading readiness.

### Fixed

- Fixed stale live-agent policy drift so agent-origin live broker writes are governed by the same daemon authorization path as other origins.
- Fixed opportunity exercise preview output so blocked previews do not imply a usable submit token.
- Fixed open-order and order-status outputs to disclose account/mode scope and local-journal limitations instead of looking like a complete broker statement.

## v1.14.1 — 2026-06-20 06:44 CEST

### What's new

- Added a bounded local order-history read surface for recent order-review forensics. It groups the daemon's local order journal by current account/mode, supports date and event limits, and keeps the warning explicit: this is not an IBKR Activity Statement, Flex export, commission ledger, or broker-grade audit.
- Cleaned up the public site around the Claude Desktop path: the guided install moved higher on the homepage, duplicate setup pages now point readers to canonical pages, and the noisy setup-card block was reduced to the handful of pages most likely to help.

### Added

- Added `ibkr orders history` and the read-only MCP `ibkr_orders_history` tool, with grouped-order limits, per-order event sampling, current account/mode filtering, and generated schema/docs updates.

### Changed

- Updated the Claude/Codex skill, hooks, and settings allowlists so order-history reads are permitted while broker-write guardrails stay unchanged.
- Moved the homepage install section above the product-tour sections and narrowed "Popular setup paths" to a short set of canonical pages.

### Fixed

- Removed stale guardrail examples that mentioned the nonexistent `ibkr orders status` command.
- Ignored local `reports/` artifacts so private trading-review pages are not accidentally swept into release work.

## v1.14.0 — 2026-06-18 13:37 CEST

### What's new

- Protection proposals now carry fresher row-backed state into the CLI and Canary app: no-stop exposure is surfaced explicitly, proposal refreshes run on the shorter 30-second cadence, and protection actions preserve more of the live broker read model across preview, submit, and status flows.
- The daemon status surface reports history and farm-readiness health alongside quote, scanner, chain, gamma, breadth, proposals, and opportunities, making stale or unavailable market-data paths easier to diagnose before a higher-level read looks wrong.
- The offline opportunity research harness gained additional local dataset-building, PIT scoring, and plan-comparison commands for calibration work. This remains an experimental research surface; treat its output as diagnostics, not a trading recommendation or finished workflow.

### Added

- Added `ibkr backtest build-opportunity`, `build-opportunity-pit`, `score-opportunity`, `research-opportunity`, `capture-opportunity`, and `export-opportunity-bars` subcommands for local opportunity-harness data capture and scoring.
- Added historical-bar `what_to_show` plumbing and CLI/app test fixtures needed by the opportunity research harness.
- Added the app Protection panel's "No-stop exposure" summary for visible trailing-stop proposals without a matching open protective order.

### Changed

- Protection proposals refresh every 30 seconds by default instead of every 2 minutes, while transient failure retries still back off to avoid noisy gateway outages.
- Canary watch rendering and protection-panel action metrics now use clearer, row-derived posture labels.
- Order preview tooling now exposes the TRAIL/TRAIL LIMIT trigger method, with stock/ETF protective trails defaulting to IBKR LAST.

### Fixed

- Fixed a daemon reconnect client-ID leak that could block later gamma and data requests.
- Fixed stale protection-proposal submit/read-model edge cases by carrying order revisions, status, and broker lifecycle state through the daemon/app plumbing.
- Fixed the app Protection headline coloring so zero actionable theta is neutral and positive risk-over-target exposure is highlighted.
- Fixed quote-liquidity cache keys when a quote carries the symbol outside the contract payload.

## v1.13.0 — 2026-06-13 13:00 CEST

### What's new

- A new daemon-owned Opportunities surface finds mechanical option-exercise candidates in existing positions. The MVP bucket is conservative: long American options only, regular-hours quotes, executable option bids, policy-gated minimum gain, and full blocker disclosure. Users can inspect it from `ibkr opportunities status|list|refresh`, the Canary app's Opportunities panel, and the read-only MCP `ibkr_opportunities` tool.
- Exercise execution is deliberately separate from opportunity discovery. MCP remains read-only and never exposes submit-capable tokens; CLI/app exercise flows require a fresh preview for the exact opportunity revision, then re-check trading mode, session/account pins, freeze state, policy, and quote freshness before broker submission.
- Opportunity policy is its own hot-reloaded TOML surface (`~/.config/ibkr/policies/opportunity-policy.toml`) rather than a hidden extension of protection proposals, so exercise-threshold experiments cannot degrade protective-stop health.

### Added

- Added typed RPC, daemon refresh/status handlers, config, status subsystem reporting, source fingerprints, ignore state, CLI rendering, and app HTTP routes for Opportunities.
- Added an `ibkr_opportunities` MCP read tool and regenerated MCP/config references so LLM hosts can discover opportunity snapshots without gaining write authority.
- Added option exercise wire support and trading-build submission plumbing for the CLI/app preview-then-exercise flow.

### Changed

- CLI help now groups protection proposals and opportunities under their own command families, keeping portfolio reads separate from broker-write-capable local workflows.
- The app Opportunities panel hides the normal policy fingerprint in the common healthy state and keeps the detailed policy identity available through status/read surfaces.
- Protection risk-reduction copy is more compact, with the excess value displayed without redundant prose.

### Fixed

- No-bid option exercise candidates are suppressed instead of showing a theoretical gain that cannot be captured through an executable option sale.
- Exercise opportunities can now disclose exposure-increasing effects when that is the mechanical result of exercising a held option; the daemon classifies the position effect instead of pretending every exercise is reduce-only.

## v1.12.0 — 2026-06-12 17:21 CEST

### What's new

- A red regime indicator now has to earn confirmation before it can escalate the headline: it must be meaningfully past its threshold (e.g. HYG at least 0.25% below its 50-day average, not 7 bps), persistent (2 NY trading sessions for the vol, credit-proxy, and breadth rows, with deep day-one breaks fast-tracked), and current under its own data cadence. Reds that fail a gate stay fully visible — row red, listed under `unconfirmed`, CLI marked `provisional (day 1 of 2, …)` — but they only warn (`early_warning`), never confirm, and never vouch for another cluster. This closes the 2026-06-12 pre-open false positive where a 7 bps HYG wobble and a prior-evening gamma cache mutually confirmed "Broad stress regime / act" against a green tape.
- While regime thresholds remain `pending_backtest`, the engine cannot demand its strongest responses on heuristic evidence alone: severity "act" needs a fresh tape co-sign (SPY down 1.5%+, VIX up 10%+, or a same-session term-structure inversion), and a heuristic-only three-red panic reads "act" instead of "urgent". Every cap is disclosed in `lifecycle.governors[]` and on the CLI's new `Governed:` line — severity "watch" beside two red rows is explained policy, not a mystery. Pure-tape panic (SPY −4%/−7%) is never governed.
- Dealer gamma from a prior trading date now serves `status: stale`: the band stays visible for awareness, but yesterday evening's cache (which still contains the day's expired 0DTE exposure) can no longer pose as contemporaneous confirmation. Off-hours, the HYG credit row bands on the latest official close instead of thin pre/post-market prints.
- The headline is now one wording table for every surface: `composite.verdict`, `posture.label`, CLI, MCP, app, and the backtest all render the same served string. Previously four copies drifted — at exactly two red clusters the posture said "Broad stress regime" while the composite said "Stress signal present". Two eligible reds now read "Confirmed stress regime"; "Broad stress regime" again means three or more.
- Every regime decision is journaled to `$XDG_STATE_HOME/ibkr/regime-decisions.jsonl` (semantic-fingerprint deduped, hourly heartbeat): raw values, depths, streaks, eligibility, lifecycle, and governor records — the forward-collection corpus that finally makes the heuristic thresholds calibratable. Disable with `ibkr settings set regime.journal.enabled=false`.

### Changed

- Regime streaks count NY trading sessions: a weekend or holiday poll keys to the most recent trading day and no longer inflates the "day N" counter. Red bands also get exit hysteresis (e.g. VIX/VIX3M leaves red below 0.98 after entering at 1.00), so boundary wobble cannot flap the band and reset the streak; held sessions count toward persistence and are disclosed in the band reason.
- The canary consumes the served confirmation policy instead of recomputing it from raw bands: act/urgent-grade rows, exposure escalation, and the `regime_stress_confirmed` signal key on `eligible_red_clusters`; two visible-but-provisional reds now produce a "Stress pending confirmation" watch row instead of "Confirmed market stress / act".
- `SourceHealth.max_age_seconds` is populated for regime clusters and the app's regime stale badge derives from the served values instead of a hardcoded 60-minute twin; the app's regime status line discloses pending-confirmation reds and governor caps.
- The lifecycle fingerprint is now `lifecycle-fp-v2` (eligibility-aware evidence + governor tokens): active regime/canary alerts re-key once on upgrade.

### Added

- Wire contract (additive): per-row `eligibility` (`eligible`, `latched`, `reasons[]`) and `freshness` (`class`, `max_age_seconds`); `lifecycle.governors[]`; `composite.cluster_eligible_red_count` / `cluster_provisional_red_count`; canary `market.eligible_red_clusters` + names. `ibkr regime --explain` prints the confirmation-policy rule and the gate table sourced from the same constants the daemon enforces.

### Engineering notes

- Cluster combination, isolated-red rescue (now eligibility-keyed), eligibility gates, and the headline table live once in `internal/rpc/regime_policy.go`; the daemon composite, lifecycle builder, CLI renderer, canary, and backtest builder all consume it. The four former copies (rpc posture, daemon `verdictFor`, CLI `verdict()`, backtest) are gone.
- PIT backtest replay applies day-1 gates (depth + freshness; persistence sessions=1), so streak-gated indicators confirm only via fast paths — 2023-03-13 (SVB Monday) deliberately demotes to `early_warning` day 0 in the curated sample (watch recall stays 1.0); sequence-aware streak replay is follow-up work on the decisions journal. Threshold promotion out of `pending_backtest` is per versioned set label with measured false-alarm/recall rates (docs/specs/regime-backtest-plan.md).
- The 2026-06-12 incident is pinned as regression fixtures on both surfaces: `TestRegimeIncident20260612Regression` (daemon pipeline) and `TestCanaryIncident20260612Regression` (canary rows/signals).

## v1.11.0 — 2026-06-12 12:19 CEST

### What's new

- Protection proposals refresh every 2 minutes instead of every 15 (`[auto_trade].proposal_cadence` default), and a gateway reconnect now triggers an immediate refresh — after an outage the panel recovers in seconds instead of waiting out the next scheduled attempt (observed 2026-06-12: gateway back at 10:53, panel stale until 10:59). Sustained-failure retries are decoupled from the cadence (30s doubling, capped at 15m), so an outage stays as quiet in the logs as before. A refresh costs one account-summary round-trip plus cache reads, well inside IBKR pacing at the new rate.
- The app's protection rows now show the live quote the action would execute against — bid/ask for stock rows, the option's own premium bid/ask for theta-hygiene and option-stop rows — with tick-direction coloring (green up, red down, gray unchanged; frozen or stale quotes render muted and never carry a direction color).
- Risk-reduction proposals are trader-adjustable in the app: a stepper on each row sets how many shares/contracts to trim (clamped to the position; the daemon re-clamps at preview and submit), with the approximate order value and a one-tap reset to the proposed quantity.
- Protection rows lead with the decision number per bucket — stop level · offset · TIF for broker stops, theta burn · DTE for theta hygiene, % of NLV · excess to trim for risk reduction — and drop the boilerplate prose, order-type prefixes, and context-only flag chips that crowded them. Hard blockers and execution-friction flags (halt, LULD, borrow, Reg SHO) stay on the row.
- The protection panel header shows the account's gross exposure as % of NLV next to the per-name concentration figures, so the numbers reconcile on sight under margin (per-name % of NLV legitimately sums past 100% when levered; the per-name math was correct all along).

### Changed

- The app's protection stale badge derives from the served refresh cadence instead of a hardcoded 20-minute twin: one full cycle plus grace (5 minutes at the default 2m cadence; a configured 15m cadence keeps the historical 20m threshold).
- `counts.risk_reduction_excess_notional` is omitted when risk-reduction proposals span different local currencies — the previous raw EUR+USD sum under a `"MIX"` currency sentinel was not a number in any currency, and the app rendered it as USD. Single-currency aggregates are unchanged.
- A reducing BUY on a short option reads "Buy to close" in the app ("Buy to cover" is stock-borrow terminology and stays for short stock).

### Fixed

- A refresh racing the post-reconnect portfolio stream can no longer install an empty "no proposals" snapshot over the last-good one: when the account summary reports open positions but the position cache is still empty, the refresh preserves the last-good snapshot under a `positions_pending` blocker and retries on the quick ladder.
- Proposal journal growth is gated on revision change: revision-identical refreshes (the common case at a 2-minute cadence) no longer append duplicate per-proposal "generated" events and outcome marks to the unbounded JSONL state files; daily outcome marks still land on date rollover.
- The protection cluster label (bucket heading) was unreadably dim — it inherited the muted row-text color; it now renders in the secondary ink tone, deliberately not red so the panel's red stays reserved for blockers and stress.

## v1.10.1 — 2026-06-12 08:36 CEST

### What's new

- `ibkr account` can no longer end up reporting zeroed balances next to correct P&L when another API client shares the TWS session ([#12](https://github.com/osauer/ibkr/issues/12)): the values the gateway returns for the account request itself are now what gets served. Previously the request read a cache shared with the streaming account-update feed, and a zeroed or foreign-account batch landing mid-request overwrote the valid snapshot — deterministic in setups where a second client holds a long-lived `reqAccountUpdates` on the same account, because TWS shares that subscription across clients.

### Fixed

- `Connector.RequestAccountSummary` reads a per-request snapshot of the rows the gateway emitted for its own request ID instead of the connection-wide cache the streaming subscription also writes. A request that produced no rows (end marker only) falls back to the streaming cache as before. `Connection.WaitForAccountSummaryEnd` and `GetAccountSummary` are unchanged for importers driving the wire connection directly, but they can race streaming overwrites.
- Streaming account-value rows (`reqAccountUpdates`) that name a concrete account other than the session's bound one are dropped instead of merged into the shared summary cache — a displaced or foreign-account batch could clobber valid balances ([#12](https://github.com/osauer/ibkr/issues/12)). Rows with an empty account name still pass through, since single-account logins may omit the code.
- The `Account update time:` debug line printed the wire protocol version (always `1`) instead of the gateway's HH:MM timestamp, which misdirected the debugging in [#12](https://github.com/osauer/ibkr/issues/12).

## v1.10.0 — 2026-06-12 06:44 CEST

### What's new

- The order-size safety caps are now intent-aware: `[trading].max_notional` and `[trading].max_option_contracts` cap risk-increasing orders only — opening, adding, or flipping a position. Reduce-only protective orders (closing or trimming what you hold) are exempt, because the position itself bounds them and the flat cap was blocking the daemon's own protection proposals: a full-position protective trail on 150 shares ≈ $72k could never preview against the default $10k cap, and a stop loss is exactly the order a size cap should never refuse. Opening orders above the caps still fail, flips stay capped (their through-zero portion opens new exposure), and a preview without usable position data fails closed before the exemption is considered. `ibkr settings show` labels the two limits "(opening)" with a "Reduce-only: exempt" line.
- Live order entry is faster: the typed `live/<account>` confirmation and the `allow_live` / `live_ack_account` / `live_ack_endpoint` config keys are gone. Live trading now needs only `[trading].mode = "live"` plus the pinned gateway port, account, and client ID — cross-checked against the connected TWS session.
- The web app submits, modifies, and cancels with a single click after preview — no arm/confirm double-click, no typed phrases, no popup dialogs. Purge keeps its one typed confirm because it bulk-cancels protective stops without a preview token.
- The agent skill now covers protection-proposal reads (`ibkr proposals status|list|refresh`) and runtime settings reads (`ibkr settings show`), matching the MCP tool surface, and its trading-policy wording matches reality: paper-account broker writes are open to agents via the gated CLI flow, live agent-origin writes are blocked daemon-side.

### Changed

- `order.preview` omits `max_notional` from the result when the cap did not bind the preview (reduce-only orders), not only when uncapped — the echo no longer claims a gate that was never applied. Purge **restore** legs re-open positions and stay fully capped.
- Public discovery metadata caught up with the two-variant releases: `llms.txt`, `llms-full.txt`, and the MCP discovery JSONs now describe the preview-only MCP surface and the explicit opt-in `ibkr-trading` tarball instead of the blanket "no order-entry interface" claim, and the `.well-known` server card lists all 23 MCP tools (the trading-read, proposals, settings, and market-events tools were missing). The `ibkr_size` and `ibkr_scan_params` MCP tool descriptions now name the overlapping tools an agent should pick instead.

### Removed

- Removed `[trading].allow_live`, `live_ack_account`, and `live_ack_endpoint`. They restated the `[gateway]` pins from the same file and gated nothing the pins did not already gate. A leftover key now fails config load with a targeted "removed — delete this key" error (a plain unknown-key error would kill every CLI command with a less helpful message). Note for downgrades: an older binary with the slimmed config fails closed and asks for `allow_live = true`; re-adding it and upgrading again trips the removed-key error, so delete the key after any downgrade round-trip.
- Removed the typed `live/<account>` confirmation from the CLI, web app, and daemon (`live_confirmation_required` blocker). Live order execution is latency-sensitive; a per-write typing ritual was the wrong layer for deliberateness.

### Fixed

- Fixed the daemon log filling with repeating TWS error loops for held symbols without a market-data entitlement (354 + indicative-pricing disclaimer on every poll, error 300 from cancelling the already-dead subscription, historical 200→366 cancel-race pairs). Root cause: current gateways deliver request errors as system notices (msg 204), which bypassed the legacy error-recovery path entirely — historical fetches burned their full timeout and then cancelled a query the server had already killed, and dead subscriptions were re-requested every app poll. The notice path now drives that recovery (poller fast-abort, immediate historical failure with the error propagated, rate-limiter slot release, force-delayed on 10197), a 30-minute in-memory absence window stops resubscribing entitlement-dead names (never persisted, cleared on reconnect, skipped for option legs and during data-farm outages), and a quote for such a name returns an honest stale shell instantly instead of after a five-second budget.
- Fixed the inactive-symbol mechanism recording marks under a key no code path ever checks: marks now land on the connector's own subscription key (the exact key the subscribe paths consult), so a delisted name is actually suppressed instead of re-requested every poll cycle. Marking now also requires a second definition error within ten minutes — a single code-200 is routinely transient, and one such transient on the currency-ledger FX repair path had suppressed an IDEALPRO route for the daemon's lifetime. FX/CASH routes are never persisted to the inactive-symbol store: the inverted direction of a pair legitimately has no security definition, and a persisted entry could silently break positions FX-rate repair across sessions.
- Fixed dealer gamma sitting unranked for hours on noisy-morning skew fits: a median per-expiry skew-fit R² between the hard block (0.50) and the preferred bar (0.75 SPX / 0.70 SPY) now ranks with a disclosing gate reason instead of degrading to context-only — R² is amplitude-relative and tracks intraday smile noise, not coverage health. Each compute now also records the residual RMS per expiry and appends per-slice skew diagnostics to `$XDG_STATE_HOME/ibkr/gamma-skew-diagnostics.jsonl` as offline calibration input for these heuristic bars.
- Fixed plain `ibkr restart` managing `ibkr app` processes outside its daemon scope: app discovery is by process name and cannot tell which daemon a found app belongs to, so when `IBKR_SOCKET` points at a non-default socket the implicit app step is now skipped (reported as `"app": {"action": "skipped", "reason": "socket_overridden"}` in `--json`, with a one-line notice otherwise; `ibkr restart --app` stays available as the explicit path). Before this, the lifecycle integration tests — which always set `IBKR_SOCKET` — would SIGTERM the developer's real `ibkr app --remote`, replace it with a test-binary copy that inherited `--remote` (Cloudflare relay registration from a test process), leak that copy past teardown on `0.0.0.0:8765`, and hang `make test`. The suite now also fails loudly if a test-binary app survives, and the integration reaper sweeps `app` processes alongside daemons.
- Fixed combined SPY+SPX dealer gamma downgrading a clean, rankable SPX read when the SPY book was degraded: the combined node no longer gates on pooled derived-IV share or top-strike concentration (each slice gates its own values, the SPX verdict propagates through `spx_coverage`, and the pooled numbers stay visible in `quality.coverage` as diagnostics) — the same per-slice posture as the skew-fit gate.
- Fixed daemon boot races that could leave the positions panel empty and background reads stale: aggregate account codes (`All`, comma-separated managed-account lists) are reduced to a concrete code before the account-updates subscription (TWS rejects aggregates with error 321, so the portfolio stream never started), a subscribed-but-silent portfolio stream self-heals by resubscribing behind the positions read path (30-second throttle) when the cache is empty while the account summary reports gross position value, the proposal engine's startup refresh backs off and retries instead of persisting error shells over the last-good snapshot, and a cancelled gamma compute can no longer persist a SPY-only result as the session-final combined read.
- Fixed the protection-proposal min-tick lookup sending the canonical asset-type enum (`STOCK`) as the IBKR wire security type: TWS rejected every held stock row with error 321 on each proposal refresh — forever, because only successful lookups are cached — burning a two-second timeout per row. The lookup now builds the same wire-typed contract previews resolve through, so proposal generation and preview round trail prices on one tick grid.
- Fixed proposal-refresh outages passing silently: transient refresh failures (account/positions fetch, unscoped or mismatched session) preserve the last-good snapshot and return no error, so the served `as_of` could freeze for 40+ minutes with zero log output. The engine now warns once per backoff escalation with the blocker codes, streak duration, and the age of the snapshot still being served (staying quiet for the first two attempts, which legitimately race the gateway connect at boot), and `ibkr status` gains a `proposals` subsystem row — ready / disabled / degraded with codes and streak start — so a frozen protection panel is diagnosable from one command.
- Fixed the app's protection freshness badge brushing "stale" between healthy refreshes: the threshold equalled the daemon's 15-minute proposal cadence, so normal operation routinely crossed it; it now allows one full cycle plus grace (20 minutes).
- Fixed the paired app blocking single-order cancels while `trading.freeze` is on, contradicting the freeze contract (a freeze stops new and modified orders, never the cancel of an open one): the app's cancel route now validates only the confirmation fields and defers write-policy to the daemon, whose cancel path strips exactly the freeze blocker. Purge keeps the full write gate — bulk-cancelling protective stops removes protection and stays blocked while frozen.

### Security

- What still gates a live broker write after this simplification: a submit-eligible single-use preview token per write (WhatIf-backed), the daemon-side hard block of agent-origin requests on live routes (no override), gateway pins cross-checked against the connected session's account, paper/live endpoint heuristics in both directions, the runtime `trading.freeze` switch, and the app's server-validated `confirm_account`/`confirm_mode` fields on every HTTP write. The v1.9.0 paper-smoke release gate is unchanged.
- The app HTTP layer rejects the removed `live_confirmation` field like any unknown field on every broker-write route (the flow never shipped to a paired device, so no stale clients exist); the proposals preview/submit/ignore routes previously used a lenient decoder that silently dropped unknown fields and now strict-decode like the order and purge routes. Run `ibkr restart` after upgrading so the app serves the new embedded assets.
- The account-data pre-commit gate now scans Go files too: the previous `*.go` exclusion was exactly the blind spot that let a real account ID into two test files this cycle (caught before any push; the public history is clean). Only documented placeholder IDs and the deliberately published paper account remain allowlisted, and `release-site-check` now pins every public version stamp (spoke-page JSON-LD and the MCP discovery JSONs), not just the landing page.

### Engineering notes

- New skill-layer drift gates: a Go test asserts every CLI command is documented in the skill, the skill allowlist mirrors the shipped settings allowlist, and no broker/state write is allowlisted; the MCP Registry template's version field is pinned to the plugin version. (This bullet was briefly misfiled under v1.9.0, which had already shipped.)
- The reduce-only proof remains enforced end-to-end at proposal submit: close/reduce intent on both the proposal and the fresh preview position effect, plus quantity within the live position. The preview position impact reads cached positions and does not subtract open working sell quantity, so stacked sell previews can each claim the same shares; `duplicateProtectiveBlockers` and broker rejection backstop this today, and reserved-quantity accounting is a flagged fast-follow.
- The unreleased generation-time `order_notional_exceeds_max` disclosure blocker and the `TradeProposal.max_notional` field it populated are removed within this cycle: they mirrored the flat preview cap that no longer applies to protective close/reduce orders, and would otherwise have kept large protective proposals blocked before preview.

## v1.9.0 — 2026-06-10 21:55 CEST

### What's new

- Releases now ship two variants per platform: the standard downloads (installer, MCPB, plain `ibkr` tarballs) remain read-only builds, while a clearly marked `ibkr-trading` tarball carries broker-write capability behind explicit opt-in warnings.
- Broker writes now carry an origin: live routes refuse agent origins and require a typed `live/<account>` confirmation from a human, while paper accounts stay fully open for agent-driven testing.
- Protective trailing stops can be re-priced in place — modify keeps broker queue position instead of cancel-and-replace — and live protection trails follow a GTC policy so they no longer expire at the end of each session.
- A new freeze switch (`ibkr settings set trading.freeze=true`) instantly blocks all new broker writes while keeping cancels available.
- The remote app no longer goes dark after seven days: routes slide their expiry while connected, and the connector re-registers automatically when a route expires.
- Market-risk tooling is faster and survives off-hours outages: canary warm runs dropped from ~20 s to ~2 s, and SPX dealer zero-gamma falls back to a persisted expiry grid when IBKR's secdef farm is down.

### Added

- Added a trading-capable release variant (`ibkr-trading-*`) with an in-tarball trading warning; both variants are covered by the signed SHA256SUMS.
- Added `ibkr trading paper-smoke`: a daemon-observed paper round-trip (place, broker ack, cancel through the production order path) that records signed evidence; releases run it as a binding gate before tagging.
- Added agent-origin gating for broker writes with journaled origins and typed live confirmations; the paper-aware PreToolUse hook and trimmed settings deny list ship in `hooks/` and `settings/`.
- Added in-place modify for protective TRAIL and TRAIL LIMIT orders across CLI, web UI, and previews, eliminating the unprotected window between cancel-ack and re-place.
- Added policy-driven GTC time-in-force for protective trailing stops; broker error 135 heals expired GTC zombies.
- Added the `trading.freeze` runtime switch blocking new broker writes while cancel paths stay open.
- Added a persisted last-good expiry/strike grid per underlying so dealer zero-gamma keeps computing (flagged stale, capped at five days) through secdef-farm outages.

### Changed

- Local builds default to trading-capable; an explicit `GO_TAGS=""` still produces the read-only binary with its capability banner.
- Trade proposals are scoped to the account and paper/live mode they were generated under; serving, fast-path submit, and persisted-snapshot adoption fail closed on mismatch, and a paper ignore can no longer suppress a live proposal.
- Remote routes slide their expiry on every authenticated connector connect; the connector cycles its websocket at half the route TTL, re-registers on HTTP 410, and expired pairing sessions are reaped every minute.
- Purge execute plans all legs first and prefetches leg quotes in parallel so emergency closes of large books finish inside the deadline; purge and restore refuse stale or session-closed quotes.
- Blocked `purge restore --all` runs now name the exact working invocation for the clean subset of symbols.
- Gamma refresh paths back off with a doubling delay (60 s to 15 min) on repeated failure instead of respawning doomed computes every scheduler tick.
- Market-events probes run in parallel and remember per-source failures so a blocked endpoint stops re-burning its timeout on every snapshot.
- The landing page and MCP server pages now state that standard downloads are read-only builds and describe the trading variant as a separate explicit opt-in.

### Removed

- Removed the risk-plan surfaces.
- Removed the decoy `--bypass-preview` CLI flag; the RPC parameter remains for API compatibility.
- Removed the human-certified runtime live gate tied to paper-smoke evidence; runtime live enablement now rests on the TWS-side API toggle, a trading-capable binary, and the existing config pins and acknowledgements.

### Fixed

- Fixed daemon autospawn race losers re-reading and re-logging the gamma persisted cache and breadth members cache; each loads exactly once, in the winning daemon, on first use.
- Fixed duplicate protective-order suppression and day-order expiry inference so journal zombies neither display as open nor block re-protection.
- Fixed cross-venue tick handling by resolving the broker minimum tick per contract and clamping stocks to the 0.01 floor.
- Fixed a double-submit race with a daemon-wide broker-write mutex, and the daemon no longer idle-exits while orders are working.
- Fixed the gamma expiries timeout (30 s to 45 s) that reported SPX as skipped on healthy-but-slow secdef responses.
- Fixed Reg SHO threshold fetches following Nasdaq redirects into HTML so missing dated files fail properly instead of caching an empty list for 12 hours.
- Fixed the S&P 500 membership refresher re-fetching Wikipedia on every breadth-touching call when the content had not changed.

### Engineering notes

- Integration lifecycle tests register daemon teardown via `t.Cleanup` plus a detached reaper in `TestMain`, so interrupted runs leave no orphaned test daemons.
- Release tooling now validates the changelog entry, requires a pushed site update for non-patch releases, and runs the paper round-trip before tagging; vet, staticcheck, and tests cover the trading build tag.
- Claude artifacts were re-worded agent-agnostic; the hook-regex check was replaced by a hook-version drift gate.
- The relay worker's sliding-TTL change requires a separate, manually gated deploy; the Go connector self-heals against the undeployed worker.

## v1.8.0 — 2026-06-09 06:12 CEST

### What's new

- Canary remote app access is now documented as an experimental beta, with screenshots, remote start and pairing notes, and a concise TLS/local-auth security model.
- The daemon now avoids the app-driven market-data wakeup/logging loop that could make laptops hot and grow `ibkr-daemon.log` aggressively.

### Added

- Added the public Canary remote app beta page at `osauer.dev/ibkr/canary-remote/`.
- Added real Canary PWA screenshots for mobile and wide viewports to the public site.

### Changed

- Updated the public landing page, sitemap, LLM indexes, MCP discovery metadata, and plugin version metadata for the `v1.8.0` feature release.

### Fixed

- Fixed rate-limiter token waits so queued broker requests sleep until their reserved refill time instead of waking every 10 ms under load.
- Reduced routine market-data request, subscription, tick, and data-type logs from info to debug.
- Stopped the app's 5-second snapshot poll from re-requesting baseline market quotes when fresh streamed quote data is already present.

## v1.7.1 — 2026-06-07 11:25 CEST

### What's new

- Public MCP Registry and MCPB metadata now present the broker flavor as `canary ibkr`, link to the polished public site, and carry the raster Canary icon in the bundle.

### Added

- Added the 512px Canary icon to the MCPB manifest so compatible clients can show the branded app icon during install and browsing.

### Changed

- Updated MCP Registry title and website metadata to point users at the public canary site instead of the raw GitHub repository.
- Updated MCPB display name, homepage, and documentation links while keeping the executable, CLI, MCP server name, and broker-facing `ibkr` command surface unchanged.

## v1.7.0 — 2026-06-06 07:46 CEST

### What's new

- The mobile web app now has a fuller trader workflow for market context, portfolio risk, canary state, order status, and purge-book handling.
- Stock and ETF order drafts now support preview-only broker WhatIf review surfaces while preserving the no-transmit execution boundary.
- Release data now carries the latest S&P 500 membership refresh for breadth and market-regime calculations.

### Added

- Added mobile PWA screens for lifecycle smoke coverage, risk dashboard controls, market context, held-stress canary context, purge workflow, and paper-trading review flows.
- Added preview-only order workflow surfaces that expose reviewable broker feedback without placing, modifying, canceling, or transmitting orders.

### Changed

- Improved restart behavior around stale daemon PIDs and tightened terminal canary wording for current mobile and purge workflows.
- Updated public MCP server metadata to advertise the `1.7.0` release candidate.

### Fixed

- Refreshed the S&P 500 constituent list as of 2026-06-06, replacing `EPAM` with `FDXF`.
- Guarded paper preview readiness and stabilized the mobile app server baseline before first release readiness.

## v1.6.1 — 2026-06-03 14:53 CEST

### What's new

- `ibkr regime`, `ibkr gamma`, and `ibkr canary` now present dealer-gamma and broad-market stress as trader-facing market-structure reads instead of exposing internal voting, gate, and cache mechanics.
- Regime funding and credit inputs now use official daily series with caching: Fed commercial paper, U.S. Treasury bill rates, and FRED HY/IG OAS.

### Changed

- Tightened default regime, gamma, and canary wording around gamma rankability, cached snapshots, market context, and data quality while keeping JSON/schema contracts stable.
- Moved source/provenance mechanics toward explain and diagnostics output, and refreshed MCP and generated reference descriptions to match the new semantics.

### Fixed

- Fixed regime funding-spread and HY/IG OAS fetch behavior so official daily indicators can be served from cache when live connectivity is otherwise healthy.
- Fixed gamma cache quality handling so a valid closed-session cached snapshot can remain a rankable market-structure signal instead of being downgraded by regime or canary.

## v1.6.0 — 2026-06-02 14:52 CEST

### What's new

- `ibkr restart` now provides a first-class daemon restart command, replacing manual process cleanup with ordered stop/start/status output.
- Regime and canary data-quality handling is tighter around stale market-data fallbacks, phase classification, gamma rankability, and option open-interest sourcing.
- Gamma output now preserves clearer quality signals when option-chain data is incomplete or not rankable.

### Added

- Added the `ibkr restart` CLI command for restarting the local daemon from the same command surface as `ibkr status`.

### Changed

- Improved regime and canary data-quality phase handling so stale, degraded, and fallback-derived market signals are surfaced more consistently.
- Stabilized gamma option open-interest sourcing and rankability checks to reduce misleading confidence when chain data is incomplete.
- Marked SPX/SPXW dealer gamma as the stable production signal: fresh rankable SPX remains usable when SPY is throttled or unavailable, while SPY is treated as corroborating context or an explicitly labeled proxy.
- Clarified public documentation for regime indicator sources and cleaned up generated publishing artifacts.

### Fixed

- Fixed restart command output ordering so the user-facing status reflects the daemon lifecycle in a predictable sequence.
- Hardened gamma data-quality signaling around fallback and unrankable inputs.
- Fixed single-scope SPX reads so they reuse the freshest canonical SPX result from a degraded combined run instead of serving an older isolated cache as blocked.

## v1.5.0 — 2026-05-31 21:35 CEST

### What's new

- `ibkr regime` and `ibkr canary` now emit stress-lifecycle states for downstream risk monitoring: quiet, early warning, confirmed stress, panic or forced defense, stabilization, opportunity, and data-quality blocking.
- Canary now carries portfolio-aware source health and semantic source fingerprints for account, positions, and consumed regime state, so schedulers can dedupe alerts and preserve provenance without hashing raw JSON.
- The backtest harness now reports before/after metrics, event-level metrics, early-warning lead time, lifecycle precision and recall, canary category slices, and canary lift over regime-only portfolio stress detection.

### Added

- Added lifecycle, evidence, readiness, timing, confidence, and `not_execution` fields to regime and canary outputs.
- Added semantic account, positions, regime, canary, and lifecycle fingerprints that avoid churn from timestamps and tiny raw-value movement inside the same signal bucket.
- Added source-health reporting for regime clusters and canary account/positions/regime inputs, including stale/degraded/partial status, confidence, cadence max-age, and fingerprint-stability semantics.
- Added canary backtest category reporting for market-driven, portfolio-driven, concentration-driven, margin-driven, options-driven, and data-quality cases.

### Changed

- `ibkr regime` now keeps isolated red evidence visible while requiring severity, tape, or independent cluster confirmation before it dominates the broad-market stress trigger.
- `ibkr canary` now separates lifecycle stage from planner readiness, keeping data-quality blocks explicit even when stress evidence is real.
- CLI help, MCP tool descriptions, plugin metadata, skill docs, protocol docs, and public site pages now describe the lifecycle/source-health contract instead of the older binary stress ladder.

### Fixed

- Fixed MCP and skill documentation drift around canary fingerprints, source health, readiness, and the read-only no-execution boundary.

## v1.4.4 — 2026-05-30 17:33 CEST

### What's new

- Claude Desktop and other MCP hosts can now keep `ibkr mcp` open across long idle chat pauses without losing the next tool response.

### Changed

- `ibkr mcp` now leaves stdio process lifetime to the MCP host while daemon cleanup remains handled by short-lived per-request sockets and the daemon's own idle timeout.

### Fixed

- Fixed MCP sessions exiting after an idle window even though the host kept stdin open, which could leave later Claude tool calls waiting on a vanished server.

## v1.4.3 — 2026-05-30 11:01 CEST

### What's new

- `ibkr status` now surfaces decision-critical data-quality issues, including degraded gamma and stale regime clusters, instead of reporting a clean health snapshot while downstream market views are degraded.

### Changed

- `ibkr regime` now aligns the gamma-zero row with the rest of the table and labels expected off-hours stale regime data without hiding it.
- `ibkr regime --explain` now renders compact threshold, data-quality, and read notes with wrapping and emphasis instead of long dim methodology paragraphs.
- `ibkr status` no longer shows the ambiguous `Next concern` line.

## v1.4.2 — 2026-05-29 12:46 CEST

### What's new

- The scheduled portfolio-canary prompt now keeps the overall canary recommendation, confidence, and top action before the row table, matching the useful CLI-level summary.

### Changed

- `examples/ibkr_portfolio_canary_prompt.md` now returns a compact report with `Stage`, `Confidence`, `Action`, action rows, and warning bullets instead of a row-only table.

## v1.4.1 — 2026-05-29 12:28 CEST

### What's new

- The scheduled portfolio-canary prompt now renders the user-facing `Go`, `Watch`, `De-lever`, and `Liquidate` stages directly, so automation output matches the CLI canary UX.

### Changed

- Tightened `examples/ibkr_portfolio_canary_prompt.md` to require exactly one `ibkr_canary` call, preserve tool action text, avoid fallback recomputation with separate tools, and fail visibly when a stale MCP host does not expose `ibkr_canary`.
- Linked the canary prompt from the public MCP, agentic-use, portfolio-analysis, and portfolio-review pages.
- MCP initialize instructions now mention the portfolio stress canary alongside account, market-data, gamma, breadth, and regime tools.

## v1.4.0 — 2026-05-29 08:47 CEST

### What's new

- Added `ibkr canary` and MCP `ibkr_canary`, a compact portfolio stress canary for scheduled checks that recommends `Go`, `Watch`, `De-lever`, or `Liquidate` actions from account margin, current exposures, concentration, options risk, market regime, and data-quality ambiguity.
- The canary is designed for pre-market, after-hours, and regular-session monitoring: it separates degraded or incomplete gamma, breadth, funding, and credit inputs from true stress signals instead of silently treating missing data as safe.
- Direct SPY/VIX tape-shock coverage now catches pre-market or after-hours SPY drawdowns and VIX spikes through the canary's `Index tape shock` row, with high-precision escalation that requires confirmation before `De-lever`.
- Option context is clearer outside U.S. listed-option regular hours: held options now carry `options_closed` warnings, and option-chain guidance distinguishes closed-session reference data from executable quotes.

### Added

- Added the `ibkr canary [--json]` CLI command.
- Added MCP tool `ibkr_canary` for fast scheduled portfolio stress checks.
- Added `examples/ibkr_portfolio_canary_prompt.md` as a ready-to-run scheduled MCP prompt.
- Added canary rows for immediate margin safety, index tape shock, market stress, equity/options exposure, largest concentration, options convexity, and ambiguity/data-quality gating.

### Changed

- Improved canary text output with a simpler hero, visible escalation ladder, highlighted active stage, confidence label, concrete action row titles, and a terminal progress message while IBKR data populates.
- Canary confidence now drops when regime inputs are ambiguous, partial, degraded, or unranked, without escalating solely because data is incomplete.
- MCP descriptions now steer agents more clearly between held-option valuation via `ibkr_positions`, option-chain selection via `ibkr_chain`, stress monitoring via `ibkr_canary`, and the underlying regime evidence via `ibkr_regime`.
- MCP stdio sessions now exit after a five-minute idle window when there are no active resource subscriptions, reducing stale local server processes in desktop clients.

### Fixed

- Closed U.S. option sessions no longer make frozen option bid/ask data look tradable in chain summaries.
- Held option rows now explicitly warn when option quote/model fields are closed-session context rather than executable liquidity.

## v1.3.2 — 2026-05-28 23:15 CEST

### What's new

- MCP stdio sessions now clean up stale daemon processes more reliably, preventing old broker context from lingering behind a fresh client session.
- Changelog release hygiene is stricter, so public entries stay reader-facing and malformed release notes fail earlier.
- The portfolio-analysis prompt is clearer about where option delta and portfolio delta scope come from.

### Changed

- Hardened changelog stub and validation scripts so maintainer-process text and template leakage are caught before release.
- Clarified the portfolio-analysis prompt's portfolio delta wording to keep contract-currency and account-base interpretation explicit.

### Fixed

- Fixed stale MCP process lifecycle handling across autospawn, dial, resource, and server shutdown paths.

## v1.3.1 — 2026-05-28 07:55 CEST

### What's new

- Added the `IBKR Portfolio Analysis — Agentic MCP Workflow` prompt, backed by a base-currency exposure map in `ibkr_positions` so multi-currency account reviews no longer need external aggregation.
- Position JSON now uses explicit `_ccy` and `_base` money fields, making contract-currency versus account-base values unambiguous for agents.
- Option position rows now flag valuation marks that sit outside the captured bid/ask range.

### Added

- Added `examples/ibkr_portfolio_analysis_prompt.md` as a named portfolio-review workflow for producing live, read-only IBKR portfolio reviews with exposure, options, regime, FX, margin, and data-quality checks.
- Added per-row `market_value_base`, `unrealized_pnl_base`, `realized_pnl_base`, and `daily_pnl_base` fields when the account base currency and FX conversion are known.
- Added `portfolio.exposure_base`, `portfolio.dollar_delta_base`, `portfolio.daily_theta_base`, and per-underlying base fields including `group_market_value_base`, `group_market_value_pct_nlv`, `group_dollar_delta_base`, `group_unrealized_pnl_base`, and `group_daily_pnl_base`.
- Added `mark_outside_bid_ask` plus structured `warning_details` on option rows when the account valuation mark is outside the captured bid/ask range.

### Changed

- Position JSON money fields that are in the security currency now use `_ccy` names (`market_value_ccy`, `unrealized_pnl_ccy`, `realized_pnl_ccy`, `daily_pnl_ccy`, `dollar_delta_ccy`, and `daily_theta_ccy`) instead of ambiguous names.
- The portfolio-analysis prompt now directs agents to use `portfolio.exposure_base` and per-underlying base fields before doing any manual aggregation.

## v1.3.0 — 2026-05-27 22:05 CEST

### What's new

- Fresh-idea screening is now a documented low-latency MCP path, with tighter scanner, technical, quote, option-chain, and sizing gates for 3-6 month trade research.
- Scanner rows now expose conservative `instrument_tags` for known ETFs and leveraged ETPs that IBKR can return from stock scans, so agents can avoid ETF false positives without guessing from `sec_type`.
- Position and FX context is safer for portfolio-aware recommendations: flat position updates no longer leak as open holdings, and non-base-currency sizing uses the correct quote/base direction.

### Added

- Added `examples/ibkr_fresh_ideas_screen_prompt.md` as a compact MCP screening prompt for surfacing 1-3 fresh trade ideas with explicit readiness, liquidity, option-chain, sizing, and no-order-placement guardrails.
- Added `instrument_tags` to scanner rows in CLI/JSON/MCP output for known ETFs, broad index ETFs, sector ETPs, leveraged ETPs, and single-stock ETPs that can appear in stock scanner results.
- Added scan filters for minimum price, minimum volume, minimum dollar volume, penny-stock exclusion, and live-quote requirement so noisy scanner rows can be filtered before agent analysis.
- Added 90-180 DTE expiry-selection parameters for option-chain expiry reads so agents can ask for the trade horizon directly instead of fetching every expiry.

### Changed

- MCP tool descriptions now steer agents toward the high-signal fresh-idea path: status and calendar readiness first, one or two bounded scans, batched technical validation, quote-quality gates, and limited chain probing.
- Scanner enrichment, technical screens, watchlist, positions, and chain outputs now expose more freshness and liquidity context for agent routing without requiring extra rescue calls.
- The release MCPB wrapper now tolerates Claude Desktop launch environments that do not preserve the expected executable path layout.
- Fresh-idea sizing guidance now documents the FX direction expected by `ibkr_size` for non-base trades.

### Fixed

- Closed stock and option positions are removed from the cached positions surface when IBKR sends a zero-quantity update, preventing stale holdings from being treated as open exposure.
- Non-base currency exposure now uses gateway exchange rates as base-currency units per one quote-currency unit, fixing EUR/USD ledger math and downstream position sizing context.
- Option-chain live-IV gating now reports unavailable live IV distinctly and avoids treating stale or missing expiry IV as a valid options structure.
- Scanner filtering now falls back to 20-day average dollar volume when live volume is missing, improving off-hours and sparse-tick screening without fabricating volume.
- Market-calendar handling now treats U.S. equity and U.S. options sessions separately, so a prompt can require listed-options RTH before recommending option structures.

## v1.2.1 — 2026-05-26 22:02 CEST

### What's new

- Claude Desktop users can now install `ibkr` as an MCP Bundle (`.mcpb`) that carries the local macOS/Linux binary and runs the read-only MCP server through stdio.
- Releases now publish both versioned and stable MCPB assets, include them in signed `SHA256SUMS`, and generate MCP Registry metadata with the versioned bundle's SHA-256.
- Gamma drill-down, regime disclosure text, and option-chain closed-market spot fallbacks are tighter, so CLI/MCP reads stay aligned during smoke and off-hours use.

### Added

- Added `ibkr-vX.Y.Z.mcpb` and stable `ibkr.mcpb` release assets for Claude Desktop MCPB installs.
- Added MCP Registry `server.json` generation and validation for MCPB publish metadata.
- Added README, release-note, website, discovery, and updating documentation for the MCPB install and update path.

### Changed

- Release checksums now cover release tarballs, the versioned MCPB asset, and the stable `ibkr.mcpb` alias before `SHA256SUMS` is signed.
- Claude Desktop install guidance now prefers the MCPB path, while shell-managed installs remain the path for Cursor, Continue, Zed, terminal use, and generic MCP hosts.

### Fixed

- `ibkr chain` now labels previous-close and historical-close spot fallbacks and can reuse quote and historical data when live spot snapshots are unavailable.
- `ibkr gamma --only=spy` and `--only=spx` now reuse the matching slice from the combined gamma cache when available, keeping drill-down reads aligned with the default gamma and regime surfaces.
- `ibkr regime --explain` now reports gamma fallback leg counts against priced legs when that denominator is available.

## v1.2.0 — 2026-05-26 20:49 CEST

### What's new

- `ibkr technical` and MCP `ibkr_technical` add a multi-symbol technical screen with trend, relative-strength, volatility, volume, and data-quality context.
- Option-chain output now says whether the displayed contracts look executable, including live bid/ask coverage, open-interest coverage, liquidity grade, and structure hints.
- Quote, watchlist, chain, status, and MCP responses now explain stale, indicative, fallback, or still-computing data more explicitly instead of leaving users to infer it from missing fields.

### Added

- Added `ibkr technical SYM[,SYM...]` with `--benchmark`, `--lookback-days`, and `--json` flags for relative-strength and trend screening.
- Added MCP `ibkr_technical` for the same technical-screen data, including per-symbol trend state, relative-strength metrics, ATR percentage, average volume, dollar volume, and data-quality status.
- Added option-chain tradability and liquidity fields in CLI, JSON, and MCP responses so agents can distinguish executable chains from stale or stock-only situations.
- Added quote liquidity context for stock snapshots, including 20-day average dollar volume and spread/quality fields when historical bars and live bid/ask data are available.
- Added subsystem health rows to `ibkr status` so long-running computed surfaces can report ready, computing, unavailable, or error states.

### Changed

- `ibkr chain` now highlights previous-session option closes and IV quality so off-hours option grids are easier to read without treating stale closes as live prints.
- `ibkr quote` includes average dollar volume and quote-quality badges in text output when liquidity enrichment is available.
- MCP tool descriptions now document the new technical screen, chain tradability fields, quote-liquidity context, and timeout behavior in the wording agents read before invoking tools.
- MCP calls that wait on slow market-data paths now honor bounded timeouts more consistently, reducing the chance that one delayed gateway response blocks an agent workflow.

### Fixed

- Option-chain and quote responses now surface data-quality warnings instead of silently presenting incomplete or fallback market data as ordinary live data.
- The changelog scaffold helper no longer executes backticked example text while generating a release entry.

## v1.1.2 — 2026-05-25 22:42 CEST

### What's new

- `ibkr status` now reads like a compact gateway flight deck, with a single verdict and a `Next concern` line for the first thing that needs attention.
- `ibkr version` now acts as a trust receipt, showing local build provenance, binary path, runtime, modified time, and whether the build looks complete.
- Stock positions can now show richer quote context, including currency, price movement, freshness, ranges, volume, and market-session context.

### Changed

- `ibkr status` uses a clearer hierarchy for gateway, market-data, daemon, TWS, S&P 500 member-list, background-work, and concern state.
- `ibkr status` now surfaces CLI/daemon version drift as an `ATTENTION` verdict with restart guidance, instead of requiring the user to notice the daemon version manually.
- `ibkr version` keeps JSON unchanged while making the default text output easier to scan for provenance and stale-binary diagnosis.
- `ibkr positions --quotes` adds quote-detail columns for stock rows; the default positions view stays more compact.
- MCP `ibkr_positions` documents and returns stock quote-context fields when available, so agents can explain held-stock freshness and price source without calling a separate quote tool.

### Fixed

- Stock-position rows no longer hide available market context behind account-position marks when quote enrichment is requested.

## v1.1.1 — 2026-05-25 21:53 CEST

### What's new

- `ibkr watch` now opens the quote monitor by default, while `ibkr watch --list` remains the offline saved-symbol inventory.
- Watchlist rows are more decision-ready after hours: compact columns show price, currency, movement, ranges, volume, data freshness, and an `AS OF` value prefixed with market state such as `closed`, `open`, or `pre-market`.
- Closed-market watchlist prices now use quote and historical market data consistently, including Xetra/EUR held positions, without substituting account-position marks as quote data.

### Changed

- `ibkr watch --json` and MCP `ibkr_watch` now expose the quote monitor as the default read shape; consumers that only need saved symbols can request the list-only mode.
- Watchlist text output keeps market-calendar explanations out of per-row prose and instead places the state directly in the `AS OF` column.
- CLI hero summaries are highlighted more strongly, and the Interactive Brokers MCP landing page now shows a visible highlighted `Summary` label in the hero summary strip.

### Fixed

- Watchlist snapshots no longer stop too early on mark or previous-close ticks when a frozen last trade can still arrive shortly afterward.
- Historical close timestamps now resolve to the exchange close instant, avoiding date-only daily bars appearing as midnight UTC or the prior local evening.
- Xetra watchlist rows now reuse SMART/IBIS/EUR routing for both quote and historical enrichment, so 52-week range and average-volume fields can populate for German held positions.

## v1.1.0 — 2026-05-25 14:09 CEST

### What's new

- Watchlists are now useful as a decision monitor: `ibkr watch --quotes` and MCP `ibkr_watch` with `include_quotes` show price, currency, movement, ranges, volume, freshness text, closed-market context, and held-stock context for saved symbols.
- Official market calendars explain US equity, US options, and Xetra sessions, including holidays, early closes, next open/close, and quote `session_context` when stale or frozen data needs explanation.
- German/Xetra stock routing is now first-class for quotes and watched held positions, so EUR holdings can be monitored without forcing a USD/default route.

### Added

- Added `ibkr watch --quotes [--json]` for a one-shot enriched watchlist monitor, while `ibkr watch --watch` now reuses the same richer rows for repeated terminal refreshes.
- Added MCP `ibkr_watch` parameters `include_quotes`, `include_positions`, and `timeout_ms`, returning enriched saved-symbol rows when quote context is requested.
- Added quote JSON fields for the headline `price`, `price_source`, day and 52-week ranges, average volume, `price_at`, `price_as_of`, and stale reasons.
- Added stock-position quote context for previous close, ranges, volume, average volume, data type, freshness labels, and stale reasons on stock rows.
- Added official embedded calendars through `ibkr calendar`, with support for US cash equities, US listed options regular sessions, and German Xetra cash equities.
- Added German market routing shortcuts for stock quotes, including Xetra/EUR contract normalization where the caller or held position supplies that context.

### Changed

- Quote text output now labels the headline price as `PRICE` and includes a source hint when the daemon used mark, midpoint, bid, ask, or previous close rather than a last trade.
- Watched held stocks reuse their position exchange and currency when available, so the watchlist and positions views stay aligned for non-USD holdings.
- Status output keeps S&P 500 member metadata compact while preserving source/date/count detail for the health check.
- Position and calendar text tables have tighter alignment for narrow terminals and localized date rendering.

### Fixed

- `ibkr watch --quotes` now opens a daemon connection instead of being treated as an offline watchlist-file command.
- Frozen data during an officially open market is now marked stale and labeled as frozen instead of being described as an at-close price.
- Previous-close fallbacks now use the official prior regular-session close for their timestamp labels, including long weekends and holidays.

## v1.0.9 — 2026-05-25 08:50 CEST

### What's new

- Release/install integrity is stricter: v1+ bootstrap installs now require the signed `SHA256SUMS` file, verify the pinned release-signing key, and reject unexpected archive entries.
- The read-only boundary is tighter for agents: bundled hooks now reject shell metacharacters in allowed `ibkr` Bash calls, while `ibkr chain` and `ibkr size` fail invalid input before touching the gateway.
- Public package copy is cleaner for MCP directories and landing pages, with stale design/research notes removed from the public docs tree.

### Changed

- Polished README, landing-page, MCP package, skill, and guide copy to use concrete read-only language and avoid vague AI-assistant marketing phrasing.
- `ibkr_chain` now treats `width: 0` as an explicit ATM-only request in MCP instead of silently falling back to the default width.
- `ibkr size` validates the local trade plan before requesting account state, so malformed input returns immediately without a daemon/account RPC.
- Removed stale design, research, and superseded spec notes that no longer reflected the current gamma, regime, and release architecture.

### Fixed

- Option-chain DTE now uses the New York calendar date, avoiding off-by-one reads caused by fractional UTC time remaining until midnight.
- Chain `--side` and negative `--width` errors are now rejected locally in the CLI and classified as `bad_request` at the daemon boundary.
- Account and portfolio wire updates now log at debug level instead of info level, reducing account-sensitive default log noise.

### Security

- `install.sh` now fails closed for v1+ releases when `SHA256SUMS.asc` or `gpg` verification is unavailable, imports the tagged release-signing key, checks its fingerprint, and validates tarball paths before extraction.
- Daemon logs and opt-in wire-interceptor logs now force private directory/file modes (`0700` directories, `0600` files), including pre-existing paths.
- The Claude hook and user-copyable settings hook now share the same guard: they block trading verbs, fail closed when `jq` is missing, and reject command chaining/metacharacters in allowed `ibkr` Bash commands.
- Wire and release smoke scripts now validate `IBKR_TEST_HOST` and `IBKR_TEST_PORT` before using them.

## v1.0.8 — 2026-05-24 20:38 CEST

### What's new

- `osauer.dev/ibkr` is now a full project landing page for IBKR MCP, TWS API, and AI-assistant workflows, with fresh sanitized product screenshots.
- Search and agent discovery now have machine-readable `llms.txt`, sitemap, and MCP server metadata files.
- The project site now carries structured metadata and social-preview images for clearer sharing and indexing.

### Added

- Added a static landing page with SEO metadata, OpenGraph/Twitter cards, JSON-LD, install CTAs, and sanitized regime, option-chain, and positions screenshots.
- Added project-level `llms.txt`, `sitemap.xml`, `mcp-server.json`, and `.well-known/mcp/server.json` files for crawler, agent, and MCP directory discovery.
- Added `.nojekyll` so GitHub Pages serves static discovery paths such as `.well-known`.

### Changed

- Bumped the Claude Code plugin manifest version to `1.0.8` so the plugin release tag stays aligned with the binary release tag.

## v1.0.7 — 2026-05-24 17:25 CEST

### What's new

- `ibkr gamma` now explains why a cached gamma result cannot be served, instead of returning a bare cold state when a persisted snapshot is rejected.
- Gamma forced-run errors and JSON now include leg-quality counts split by underlying and trading class, making SPX/SPXW data gaps visible to CLI, MCP, and agent consumers.
- `ibkr regime` keeps the SPY/VIX tape between the verdict and indicator table, and missing row values no longer render as fake zeroes.

### Added

- Added `cold_reason_code`, `cold_reason`, and `cold_action` to cold gamma responses when the daemon knows why no cached value is usable.
- Added `result.leg_diagnostics` with priced, positive-open-interest, positive-gamma, and positive-absolute-GEX counts by underlying and trading class.

### Fixed

- Completed forced gamma failures during closed markets now surface as errors, rather than being hidden behind the closed-session cold-cache gate.
- Rejected persisted gamma caches now report the validation reason to the caller, including invalid per-index slices in combined SPY+SPX results.
- The regime dashboard now places the SPY/VIX tape above the indicator rows and preserves honest unavailable placeholders for missing VIX, HYG, and USD/JPY inputs.

## v1.0.6 — 2026-05-24 14:08 CEST

### What's new

- `ibkr regime` now shows an `AS OF` column and per-row freshness metadata, so live ticks, stale/frozen quotes, daily official files, cached gamma/breadth, and unavailable rows are clearly different.
- Regime JSON and MCP responses now expose compact per-indicator `band`, `band_reason`, `thresholds`, and `as_of` metadata for agent-friendly reads without expanding methodology notes.
- MOVE/rates-volatility is removed from the release dashboard until a verified IBKR contract or licensed official data connector exists; it is not proxied with ETFs or futures.

### Added

- Added a regime backtest plan covering point-in-time historical inputs, target stress definitions, walk-forward calibration, look-ahead controls, unavailable rows, source revisions, and cluster-weight evaluation.

### Changed

- The regime dashboard is now an eight-row, six-cluster evidence balance after removing unsourced MOVE/rates-volatility from the live surface.
- Regime summary confidence and warning prose now account for stale, unavailable, computing, and unranked critical rows instead of treating all non-green rows as equivalent.
- MCP and skill documentation now describe the eight-row regime contract, compact default fields, and no-probability policy pending backtest calibration.

### Fixed

- Quote JSON now surfaces IBKR mark price and assigns a non-empty data type for mark/close-only frozen snapshots, so off-hours release smoke no longer sees a successful-but-empty quote.
- Live wire smoke now accepts documented SPY mark/close fallback ticks in loose off-hours mode while still requiring bid/ask/last ticks in live mode.

## v1.0.5 — 2026-05-24 11:56 CEST

### What's new

- `ibkr regime` now opens with a color-coded hero verdict, evidence balance, and punch line before the row table, so the regime read is visible in one glance.
- The regime dashboard now adds vol-of-vol, rates-volatility, credit-spread, and funding-stress evidence alongside VIX term structure, HYG/SPY, USD/JPY, gamma, and breadth.
- Default regime JSON and MCP responses now include a compact `summary` and omit bulky methodology notes/history unless `--explain` is used with JSON.

### Changed

- Composite regime scoring now de-duplicates correlated rows into evidence clusters before producing the headline verdict, reducing double-counting from related volatility and credit indicators.
- Regime MCP/tooling docs now describe the expanded indicator set and compact-vs-explain JSON behavior.
- Official daily series for VVIX, MOVE, high-yield OAS, and funding stress are fetched through the daemon and surfaced with provenance/freshness metadata.

### Fixed

- Regime streak and composite helpers now classify the expanded indicator set consistently instead of only handling the original five-row dashboard.

## v1.0.4 — 2026-05-24 09:41 CEST

### What's new

- `ibkr gamma` now leads with a compact `summary` explaining which per-index γ-zero was identified, whether it is long/transition/short gamma, confidence, and the non-advisory caveat.
- Combined SPY+SPX gamma no longer exposes a fake top-level zero-gamma price; agents read `summary.per_index` or `per_index` for SPY and SPX levels, while `gamma_total_abs` and `top_strikes` stay as scale-safe magnitude surfaces.
- Default CLI JSON and MCP gamma/regime responses strip large profile arrays unless `--profiles` / `include_profiles` is requested, making the tooling surface much easier to consume.

### Changed

- A γ-zero crossing is classified by spot's distance from the identified level: above +2% is long-gamma, within ±2% is transition, and below -2% is short-gamma. The old `agree:flipping` combined classifier is replaced by `agree:transition-gamma`.
- The combined regime row and streak classifier now weight mixed SPY/SPX gamma bands by gross gamma exposure, with an SPX structural fallback when magnitudes are absent, so a dominant short-gamma SPX book is not flattened into a neutral mixed headline.
- Gamma warnings now serialize as scoped `warning_details` with message, impact, and action fields; raw warning tokens remain daemon-internal.
- Derived-IV disclosures now say the fallback inverts option quote mids or prior-session closes, instead of implying every fallback leg used only a prior-session close.

### Fixed

- Gamma computes now reject and invalidate all-zero GEX profiles caused by priced option legs with no non-zero OI-weighted gamma, rather than reporting them as a usable long-gamma regime.
- The regime breadth envelope no longer leaks a zero `spot_at` timestamp in JSON when the field was not observed.

## v1.0.3 — 2026-05-24 07:21 CEST

### What's new

- Release cuts are faster without relaxing the gates: the release target now overlaps the existing check, package-test, and daemon-test prerequisites, then runs JSON-contract and wire smoke in one live TWS daemon session.
- Release artifacts build in parallel across the supported OS/arch matrix while keeping the same checksum, PGP signature, plugin tag, and GitHub release publication checks.
- CI and local checks now use the same pinned staticcheck and govulncheck tool versions from `go.mod`, removing surprise `@latest` drift.

### Changed

- `make release` now runs the existing `test` gate with `RELEASE_TEST_JOBS` parallelism and replaces the separate `release-verify` plus `smoke-only` sequence with `release-smoke`, preserving the live-TWS requirement and version-stamped binary check.
- `make release-binaries` now honors `RELEASE_BUILD_JOBS` to build the four release tarballs concurrently before generating and signing `SHA256SUMS`.
- `make check` and GitHub Actions now invoke pinned lint and vulnerability tools via `go tool staticcheck ./...` and `go tool govulncheck ./...`.
- Release-flow documentation now names the strict `release-smoke` gate and the pinned local/CI toolchain behavior.

### Fixed

- Wire smoke assertions using `--since-offset` now keep the first complete frame when the offset lands exactly on a JSONL line boundary, so per-command assertions prove the command that just ran instead of accidentally skipping its first frame.

## v1.0.2 — 2026-05-24 06:12 CEST

### What's new

- Quote and scanner volume fields now stay human-scale when TWS sends IBKR Decimal sizes; strict release verification rejects empty quote snapshots and impossible billion-share volumes before publish.
- MCP quote resources now answer with an initial snapshot and late subscribers receive the current frame, so off-hours subscriptions do not appear idle after the ACK.
- CLI and daemon hardening covers spaced value flags (`--only spy`, `--target 780`, `--log path`), parallel option-chain calls, and invalid all-zero gamma cache entries.

### Fixed

- Normalized Decimal-encoded volume tick-size payloads on modern IBKR server versions while preserving ordinary tick sizes and legacy server behavior.
- Release verification now requires non-empty quote data type plus at least one price field, and fails suspiciously large SPY volume.
- CLI flag hoisting now treats `--only`, `--target`, and `--log` as value flags in all documented syntaxes.
- MCP resource reads use a longer snapshot timeout; resource subscription sends an initial update and shared subscribers are seeded from the connector cache.
- Gamma cache writes and persisted cache loads now reject ready results with option legs but zero magnitude, empty top strikes, and an all-zero profile.
- Option-chain spot discovery now holds the shared market-data subscription across the snapshot, avoiding parallel subscribe/unsubscribe races that previously surfaced as no-spot failures.
- Strict wire smoke no longer prints broken-pipe noise when `pipefail` is enabled.

## v1.0.1 — 2026-05-23 23:05 CEST

### What's new

- **Read-only hardening.** The default Go library build now refuses order-writing methods with `pkg/ibkr.ErrTradingDisabled` before any socket write. Only downstream forks built explicitly with `-tags trading` can reach the raw order encoder; the shipped daemon/CLI/MCP/plugin remain read-only.
- Option quote snapshots now request real IBKR option contracts. `ibkr quote SPY 260619 C 600` sends an `OPT` subscription with `expiry=20260619`, `right=C`, and `strike=600` instead of treating the option as a synthetic stock symbol. Option streaming is rejected with a clear message until that path has a proper shared-subscription implementation.
- Release/update polish: `go install` binaries now pass the embedded release version through update checks, daemon status, and MCP metadata; installer replacement stages inside the target directory with the prior binary still live until the final rename; release notes and MCP gamma docs now describe the v3 horizon split correctly.

### Changed

- `make release-binaries` stamps release artifacts with the tag commit timestamp instead of wall-clock build time, making rebuilt binaries deterministic on the same Go/toolchain pair.
- `make release` now builds and signs release artifacts before pushing the public binary tag. A build/signing failure can leave only a local tag, which the target removes automatically before exiting.
- GitHub release notes now append the changelog entry without duplicating the `### What's new` section already promoted into the release header.
- README and SECURITY wording now describe the v1 stable support/read-only policy instead of the old pre-1.0 policy.

### Fixed

- `ibkr quote SYM YYMMDD C|P STRIKE` now routes through the daemon's option quote path, resolves the option contract, subscribes to the option market-data line, and returns bid/ask/last/prev-close/IV fields when IBKR delivers them within the snapshot timeout.
- `ibkr quote ... --watch` for option-shaped arguments now fails before dialing the daemon with `option streaming is not supported yet`, rather than streaming a made-up stock symbol.
- `cmd/ibkr` now uses Go module BuildInfo as the effective runtime version for `ibkr update`, daemon startup, MCP server metadata, and daemon version-skew warnings. This fixes `go install github.com/osauer/ibkr/cmd/ibkr@vX.Y.Z` binaries reporting the right version in `ibkr version` but still behaving like `dev` elsewhere.
- The self-updater no longer moves the live binary out of the way before it knows the replacement can be renamed into the install directory. It copies the verified binary into the destination directory first, hard-links the prior binary to `.bak`, then performs one same-filesystem atomic rename.
- MCP `ibkr_gamma` / `ibkr_regime` tool descriptions and the regime gamma notes now name `bs-gamma-profile-v3-stickymoneyness-0dte-split` and the `zero_gamma_0dte` / `zero_gamma_1_7` / `zero_gamma_term` fields instead of stale v2 near/term wording.
- Claude Code release notes now use the same plugin install command as the README: `/plugin install ibkr@ibkr`.

### Security

- Default `pkg/ibkr` builds now hard-fail all order-writing methods with `ErrTradingDisabled`. The daemon already refused order RPCs; this patch closes the separate public Go-library path so the published module matches the read-only v1 promise.

## v1.0.0 — 2026-05-23 22:19 CEST

### What's new

- **Signed release artifacts.** Every v1.0+ release ships `SHA256SUMS.asc` — a PGP detached signature over `SHA256SUMS`, produced by the maintainer's Ed25519 key (fingerprint `D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D`). `ibkr update` refuses any release that does not publish the signature, and any release whose signature does not verify against the public key embedded in every shipped binary. Trust chain no longer depends solely on GitHub TLS + account security — an attacker with write access to the release page cannot produce a signature without the maintainer's private key. Manual-verification steps live in [SECURITY.md → Release integrity](SECURITY.md#release-integrity-v100); the bootstrap installer (`install.sh`) verifies best-effort when `gpg` is on PATH.
- Dealer-gamma methodology refresh (v3). The horizon split now isolates **0DTE / 1-7 / >7 DTE** instead of lumping 0DTE in with the rest of the weeklies — 0DTE is ~59% of SPX volume per Cboe 2025 data, and dealer hedging behaves materially differently there. The `|Γ|·OI` magnitude reading is now a real number (the prior aggregator silently returned $0 over thousands of legs when the gateway's Greeks tick raced behind IV) and is promoted to a co-primary signal alongside the signed γ-zero in both `ibkr gamma` and the `ibkr regime` gamma row. `--explain` carries a citations block (Perfiliev, Derman/Daglish-Hull-Suo, SqueezeMetrics, Cboe) and a one-paragraph note on the SPY+SPX scaling convention. **Action required:** the method token bumped to `bs-gamma-profile-v3-stickymoneyness-0dte-split`; all pre-v1 cached gamma snapshots invalidate on first daemon boot. Next regime call during market hours triggers a fresh compute.
- CLI hero and `--explain` are unified across `ibkr gamma` and `ibkr regime`. Both commands open with title · timestamp / anchor quote / status line and stop dumping methodology metadata at the user by default. `ibkr gamma --explain` matches the regime command and surfaces the per-bucket horizon breakdown, citations, scaling note, and sign-convention disclosure. Regime's gamma row renames to `γ-zero (SPY+SPX)` when scope is combined, drops the misleading SPY-spot prefix on combined rows, and no longer prints `|Γ|·OI 0.0bn` when no magnitude is available. `ibkr chain SPY --expiry <date>` gains an OI column per leg (calls + puts).
- MCP/JSON wire is at full parity with `--explain`. The `ibkr_gamma` tool gains a `scope: "spy" | "spx" | "spy+spx"` param matching the CLI's `--only`. `ibkr_regime` now returns a top-level `composite: {verdict, green_count, yellow_count, red_count, ranked_count, unranked_count}`, per-indicator `streak` (band / sessions / since), and per-scalar `*_quality` (freshness class / confidence / source). The combined gamma envelope carries `spot_anchor: "SPY"` to signal which top-level fields are SPY-anchored shallow copies vs truly combined. Off-hours: the daemon never recomputes when markets are closed; it serves the persisted snapshot with a `cache_stale_off_hours` warning when more than 24h old.

### Changed

- **Breaking (Go library):** `rpc.GammaZeroComputed.Method` is now `bs-gamma-profile-v3-stickymoneyness-0dte-split` (was `perfiliev-bs-sweep-v2-stickymoneyness`). The cache method-token gate invalidates v2 snapshots on load; first call after install pays one compute. Wire consumers that pin on the method string for assertions must update.
- `ibkr gamma`: default hero cut by roughly one-third. Skew model, method token, source, compute duration, derived-IV count, leg count, scope, and per-bucket horizon detail moved under `--explain`. Default view shows hero / regime line / per-index compact row / magnitude / warnings only.
- `ibkr gamma`: signed γ-zero disclosure rewritten. The 2017 SqueezeMetrics "dealers long calls, short puts" convention is now flagged as materially deprecated by the literature since 2022 (SqueezeMetrics DDOI, SpotGamma TRACE, Glassnode taker-flow GEX) — treat the signed level as a regime hint and the magnitude as the more robust read.
- `ibkr regime`: gamma row's value cell drops the redundant `spot X.XX · ` prefix on combined scope (the SPY anchor quote on the header line already shows the spot). The note column shortens from 91 chars to `dealer long-γ · stabilizing`; the long-form explanation lives under `--explain`.
- `ibkr regime`: `--explain` block streak markers and per-scalar quality tags now match exactly what `--json` and MCP return — no CLI-only surface for these fields.
- `gamma_combine`: warnings on a combined `SPY+SPX` envelope are now the de-duped union of the SPY and SPX warnings, not SPY's alone. The combined `Profile` field is set to `nil` (with a `combined_profile_grid_mismatch` warning) when SPY and SPX sweep grids differ — which they always do in production due to the ~10× price ratio. Consumers wanting the per-underlying profile read from `per_index["SPY"].profile` / `per_index["SPX"].profile`.

### Added

- `rpc.GammaZeroComputed.ZeroGamma0DTE` / `ZeroGamma1to7` / `ZeroGammaTerm` (and `Profile0DTE` / `Profile1to7` / `ProfileTerm`, `GammaSign0DTE` / `_1to7` / `_Term`, `LegCount0DTE` / `_1to7` / `_Term`) — three-bucket horizon split replacing the v2 `≤7` / `>7` split. All fields `omitempty`. The v2 names (`ZeroGammaNear`, `ProfileNear`, `GammaSignNear`, `NearLegCount`) remain as aliases for the merged 0DTE∪1-7 bucket and are marked deprecated for removal post-1.0.
- `rpc.GammaZeroComputed.GammaTotalAbsConvention` (`"sign-agnostic"`) — labels the convention of the magnitude reading so downstream renderers can show it correctly without re-deriving.
- `rpc.GammaZeroComputed.MethodologyCitations []string` — four-entry array (Perfiliev 2022, Derman / Daglish-Hull-Suo, SqueezeMetrics 2017, Cboe 2025); rendered by `ibkr gamma --explain` and available verbatim to MCP / Go consumers.
- `rpc.GammaZeroComputed.SpotAnchor string` — set to `"SPY"` on combined scope; `""` on single-underlying scopes. Signals to wire consumers that the envelope's top-level `spot_underlying`, `zero_gamma`, `gamma_sign`, and per-bucket triples are SPY-anchored shallow copies; use `per_index["SPX"]` for SPX-scale numbers. The struct doc-comment block enumerates exactly which top-level fields are combined-and-correct vs SPY-anchored.
- `rpc.RegimeSnapshotResult.Composite` (`RegimeComposite{Verdict, GreenCount, YellowCount, RedCount, RankedCount, UnrankedCount}`) — single source of truth for the regime verdict and traffic-light tally, shared by CLI text and the wire so MCP consumers no longer need to re-derive from row-by-row state.
- MCP `ibkr_gamma` tool: optional `scope: "spy" | "spx" | "spy+spx"` request parameter (default `"spy+spx"`), matching the CLI's `--only` flag. Unknown values rejected at validation.
- MCP `ibkr_regime` tool: response now includes per-indicator `streak: {band, sessions, since}` and per-scalar `*_quality: {freshness_class, confidence, as_of, source}` fields previously CLI-only. `composite` field exposes the verdict and counts.
- `ibkr gamma --explain` flag (was regime-only). Surfaces methodology metadata, per-bucket horizon split, citations, scaling caveat, and the sign-convention disclosure.
- `ibkr chain <symbol> --expiry <date>` gains an `OI` column per side (calls + puts). Sourced from IBKR ticks 27/28 on the existing option subscription; zero extra round-trips per leg. Off-hours options books are closed; OI renders as em-dash when unavailable. JSON consumers see `call_oi` / `put_oi` on each `ChainStrike` (the fields existed in the wire schema since v0.x but were never populated).
- New shared CLI hero renderer (`internal/cli/hero.go`): single `renderCommandHero(title, timestamp, anchor, summary)` used by both `ibkr gamma` and `ibkr regime` so the look-and-feel converges across commands.

### Fixed

- `gamma_total_abs` aggregator (the `|Γ|·OI sum` magnitude) returned `$0` when the gateway's Greeks model-computation tick raced behind the IV tick — the aggregator read a leg's `gammaAtSnapshot` from the optional Greeks payload, which was zero whenever the poll loop exited on IV-first. Off-hours where the gateway's model engine is bursty, this cancelled most legs and the wire surfaced $0 over 3200+ active SPY+SPX legs. The aggregator now BS-recomputes gamma from each leg's captured IV at snapshot spot (same recipe the sweep already uses).
- Gamma cache off-hours: a daemon booted on a closed-market day (overnight / weekend) used to kick a fresh multi-minute compute against a closed gateway because the same-NY-session-key gate dropped the prior session's cache on load. The daemon now serves any persisted result on `SessionClosed` (regardless of session key) and never kicks a compute outside trading hours; results older than 24h carry a `cache_stale_off_hours` warning so renderers can flag the staleness explicitly.
- Gamma cache method-token gate: the existing gate compared the persisted envelope's `Method` against the persisted result's `Method` (always equal, since both are written together — never fired in practice) but did NOT compare against the daemon's current runtime token. A prior-era cache would survive a daemon upgrade and be served as if fresh under the new label. The gate now rejects on either mismatch.
- Gamma handler nil-job panic: when the off-hours `kickOrJoin` correctly returned `(nil, false)` (per the "never compute on closed" contract) and the caller passed a non-zero `WaitMs`, the handler panicked on `<-job.done`. The wait block now guards on a non-nil job and falls through to a Cold snapshot when there's no work to wait on.
- `ibkr regime` no longer prints `|Γ|·OI 0.0bn` on the gamma row when no crossing was found inside the sweep window — the magnitude segment is omitted entirely when `gamma_total_abs == 0`.
- `ibkr regime` gamma row label now reflects the actual scope of the underlying envelope (`SPY γ-zero` / `SPX γ-zero` / `γ-zero (SPY+SPX)`) instead of always reading `SPY γ-zero` even when the data is combined SPY+SPX. The cold / computing / error fallback (no envelope yet) now defaults to the combined label too, so the row name no longer silently flips between calls depending on whether a compute has landed.
- `ibkr gamma` in cold-cache state (no compute has run this NY session and none is in flight — the common state off-hours after a method-token bump invalidates prior caches) now prints a clear explainer with the `--force` hint and exits 0, instead of erroring out with `daemon returned status "cold" without a result payload` (exit 1).
- `ibkr gamma --force` followed by a non-force poll on a closed market: the in-flight forced compute is now visible to subsequent callers (they join the same job and see progress / ETA) instead of being told `cold` while the compute is actually running. The "never auto-kick on closed" invariant is preserved — non-force calls still do not start a new compute on a closed session.
- `ibkr chain <symbol> --expiry <date>` with a malformed expiry like `not-a-date` now fails in under a second with `chain: --expiry must be YYYY-MM-DD (got "...")`, instead of burning the full 60-second RPC deadline against a doomed strike fan-out.

### Deprecated

- `rpc.GammaZeroComputed.ZeroGammaNear` / `ProfileNear` / `GammaSignNear` / `NearLegCount` — kept as v2-compatible aliases for the merged 0DTE∪1-7 bucket. New consumers should use the v3 per-bucket fields. Removal targeted post-1.0.

### Security

- Release SHA256SUMS files are now PGP-signed by the maintainer's Ed25519 key (`D984 26D4 8FED 85EF A339  0469 4D92 2A4F 922B 7D7D`), with the public key embedded in every ibkr binary at `internal/update/release-signing-key.asc` and a fingerprint constant cross-checked at startup. `ibkr update` fails closed on missing or invalid signatures — no `--insecure` flag. See [SECURITY.md → Release integrity (v1.0.0+)](SECURITY.md#release-integrity-v100) for the trust model, manual verification commands, and rotation policy. New dependency: `github.com/ProtonMail/go-crypto`.

### Engineering notes

The 1.0 line locks down four surfaces — Go API, CLI human text, CLI `--json`, MCP — at field-level parity so downstream agents and Go consumers no longer need surface-specific shims. The combined `SPY+SPX` envelope retains its shallow-copy-of-SPY shape on the wire because a clean `CombinedGammaZeroComputed` type would have been too much surface change late in the cycle; the `spot_anchor` field and the explicit struct doc-comment list of "combined-and-correct vs SPY-anchored" fields document the convention. The signed-vs-magnitude question is treated empirically: the 2017 SqueezeMetrics convention assumed dealers long calls / short puts, and the literature has materially walked off that assumption since 2022 (DDOI, TRACE, taker-flow GEX). The release surfaces the magnitude as a co-primary read and frames the signed level as a regime hint, matching how reputable vendors talk about the same data today. Off-hours behavior is now uniform: the daemon never recomputes on a closed market and the renderer flags staleness explicitly when serving a cache more than 24h old — overnight and weekend calls no longer race a doomed compute.

## v0.32.0 — 2026-05-22 11:11 CEST

### What's new

- `ibkr gamma` calls feel responsive again. The default blocking wait dropped from 50 s to 3 s, so an in-flight compute returns a `computing N%` progress envelope within seconds instead of stalling for nearly a minute. Cached runs still return immediately. `--no-wait` is unchanged for callers that want zero blocking.
- Dealer term-gamma now reflects the horizons that actually matter. The expiry picker reaches past the next ~2 weeks of weeklies to anchor the term basket on the next monthly OPEX and next quarterly expiration, so `DTE > 7` gamma is no longer near-empty for SPY-anchored views. The `Top strikes` table picks up institutional hedges that the prior weeklies-only basket missed entirely.
- Dealer gamma now survives daemon restarts and tunes its refresh cadence to the trading-session phase. A `pkill && relaunch` cycle warm-starts from the on-disk snapshot instead of paying a 5-min cold recompute; overnight and on weekends the daemon stops kicking doomed refreshes against a closed market.

### Changed

- `ibkr gamma`: default `--wait_ms` is now 3 s (down from 50 s). Cached runs return immediately. In-flight runs come back with a `computing N%` envelope the renderer turns into a progress row, so polling sees fresh state on every call rather than burning a full wait window per iteration. `--no-wait` (`wait_ms=0`) is unchanged.
- `ibkr gamma`: per-compute expiry basket follows a fixed slot policy — front-week, front-week+1, end-of-week, next monthly OPEX, next quarterly expiration, plus a fill. Output ordering and the `YYYY-MM-DD` wire shape are unchanged; the `Leg count`, top-strikes table, and `DTE > 7` (`term`) sub-row now carry hundreds of legs in regimes where they previously carried near-zero. Single-class equity path (`selectExpirations`) only; the SPX multi-class path is unchanged.
- `ibkr gamma`: cache soft refresh is now session-aware. During RTH (09:30–16:00 ET weekdays) a cached compute refreshes ~60 min after it landed; during pre-market and post-market (04:00–09:30 ET, 16:00–20:00 ET weekdays) it refreshes ~30 min; outside those windows (overnight + weekends) it does not refresh at all and continues to serve the last successful snapshot until the NY-midnight session-key boundary rolls. Crossing a session boundary (e.g. pre-market → RTH at 09:30 ET) ages out the cached value on the next call so callers right after the open see a fresh RTH read rather than the pre-market profile carried into the new window.

### Added

- Gamma cache persists across daemon restarts. The most recent successful compute for each scope (`spy+spx`, `spy`, `spx`) is written to `~/.cache/ibkr/gamma-zero/gamma-zero-<scope>.json` (atomic temp + rename, pretty JSON) on success and reloaded on daemon startup. Cold-cache gates check schema version, NY session key, scope name, and methodology token independently — a mismatch on any gate falls back to a fresh compute rather than serving stale state across schema or session boundaries. The three scope files are written and gated independently; no cross-scope poisoning.
- `rpc.ClassifySession(now time.Time) rpc.SessionClass` — four-way classifier returning `SessionClosed` / `SessionPre` / `SessionRTH` / `SessionPost`, source of truth for the new session-aware cache cadence and reusable by other components that need to branch on the U.S. equity-options session phase. Fail-open to `SessionRTH` under missing tzdata, mirroring `IsOptionRTH`.

### Engineering notes

The pre-this-release single-slot cache keyed only by NY session date would, once persistence landed, have surfaced a `--only=spy` result to a combined caller — fixed by carrying one slot per scope from day one of persistence; `gamma-zero-spy+spx.json` / `-spy.json` / `-spx.json` are written and gated independently. The session-aware soft TTL replaces a constant 5-min window that was both noisy intraday (refreshing more often than dealer positioning actually shifts) and wasteful overnight (kicking 5-min recomputes against a market that won't produce fresh quotes). The expiry-picker change is scoped to the equity single-class path (`selectExpirations`); the SPX multi-class path (`selectSPXExpirationsClassed`) is handled separately, and strike-window / skew-weighting / multi-cross detection / cache-key refactor remain explicitly out of scope here, gated on live measurement of this release's basket.

## v0.31.0 — 2026-05-21 22:12 CEST

### What's new

- `ibkr gamma` now covers SPY **and** SPX by default. The headline is a `Regime` line classifying whether the two indices agree (both long-γ / both short-γ / both flipping) or `DISAGREEMENT` — the actionable case where one book is stabilising while the other amplifies (institutional/retail divergence). The per-index breakdown (SPY γ-zero · SPX γ-zero, each with near/term sub-rows) is the primary decision surface. The top-strikes table gains an `INDEX` column in combined scope.
- **Entitlement-graceful fallback:** accounts without CBOE OPRA see the same combined command degrade cleanly to SPY-only with a one-line `⚠ SPX skipped — entitlement missing (IBKR error 354). Showing SPY only.` banner above the headline (exit 0). Partial cases — one SPX trading class lands, the other 354s — surface in the JSON envelope's `partial_classes` map. No reconfigure needed.
- New `--only=spy` (bit-for-bit pre-coverage-arc SPY-only output, fast path) and `--only=spx` (SPX-only; errors out if SPX is unreachable rather than degrading). MCP `ibkr_gamma` tool gains the same `scope` argument. `ibkr gamma` with no flag defaults to combined.

### Changed

- `ibkr gamma`: default scope is now combined SPY+SPX; the renderer header reads `Dealer Zero-Gamma (SPY + SPX)`, the headline is a `Regime` row, and per-index detail prints under `Per-index:`. SPY-only consumers wanting the pre-coverage-arc layout pass `--only=spy`.
- `ibkr gamma`: `Source` row now reads `computed from IBKR SPY+SPX option chains` in combined scope; single-underlying scopes interpolate the actual symbol.
- `ibkr regime`: indicator 4 (dealer zero-gamma) is now sourced from the combined compute; the regime row's headline figures still reflect the SPY-anchored view for back-compat, but the per-index detail is reachable via `ibkr gamma --json`.
- MCP `ibkr_gamma`: tool description rewritten to cover combined scope, per-index breakdown, scope flag, entitlement-graceful fallback, partial-class state, and the regime-agreement classifier. Argument list gains `scope` (one of `spy` / `spx` / `spy+spx`).
- AM-settled SPX-class monthlies now respect their 09:30 ET settlement instant in DTE filtering; previously the unified 16:00 ET cutoff over-stated their time-to-expiry by ~6.5 hours on the third Friday morning, inflating their gamma contribution.

### Added

- `rpc.GammaZeroSPXParams` gains a `scope` string field (`"spy"` / `"spx"` / `"spy+spx"` / empty → combined default).
- `rpc.GammaZeroComputed` gains `scope`, `per_index` (map keyed by underlying), `partial_classes` (per-class entitlement gaps), and `regime_agreement` fields. All `omitempty`. The single-underlying `per_index` entries are fully-formed `GammaZeroComputed` payloads so consumers can recurse using the same schema.
- `rpc.StrikeConcentration` gains `underlying` and `trading_class` fields so combined-scope `top_strikes` rows are self-describing.
- New `Connector.FetchOptionExpiryStrikesClassed(symbol, timeout)` Go method returning `map[string][]ExpiryClassedStrikes` — the per-(date, trading-class) strike grid, used by the SPX-aware enumeration to keep SPX-class AM monthlies and SPXW-class PM weeklies separated on third-Friday dates.
- **Breaking (Go library):** `optionContractKey` (the on-disk and in-memory option-cache key) now includes the trading class as a second field: `symbol|class|expiry|strike|right`. `IsOptionContractCached` and `SubscribeOption` gain a `tradingClass` argument; empty normalises to `symbol` so SPY-shaped callers pass through unchanged.
- On-disk option-contract cache schema bumped to v3. v2 files (no trading class in keys) are read transparently: keys are migrated to the v3 empty-class shape (`SYMBOL||EXPIRY|STRIKE|RIGHT`) on load; the next prewarm overwrites them with class-qualified keys.
- `make smoke` honours `SPX_EXPECTED_REACHABLE` (default `1` in this repo): when set, an SPX-skipped banner during the new `--only=spx` smoke step fails the run rather than passing silently — guards against unnoticed SPX regression on accounts that legitimately have CBOE OPRA entitlement.

### Fixed

- SPX vs SPXW collision in the option-contract cache: a third-Friday SPX-class AM contract and the SPXW-class PM contract at the same strike share `(symbol, expiry, strike, right)`. The pre-v3 key shape silently overwrote one with the other in the in-memory cache and persisted file; v3 keys include trading class so both contracts round-trip without collision. Affects SPX gamma compute correctness on third-Friday dates.
- `dteYears` (option time-to-expiry helper) used 16:00 ET as the unified expiration instant. SPX-class monthlies cash-settle at 09:30 ET; the helper now branches on trading class and uses the correct settlement instant per class, eliminating a ~6.5-hour TTE over-statement on third-Friday morning runs.

### Engineering notes

The combined sweep aggregation prototyped during the coverage arc was dropped before release. The spot² scaling (SPX/SPY per-contract dollar gamma ≈ 100×) makes SPX 75-80% of the combined dollar magnitude in normal regimes, so a fused "combined γ-zero spot X%" was indistinguishable from SPX-only with epsilon SPY noise. The release exposes the `regime_agreement` classifier instead — derived from per-index sweep signs, surfacing the decoupling case directly. The 20-day SPY/SPX price-correlation gate prototyped alongside was also cut: prices stay > 0.99 correlated essentially always while gamma regimes can decouple, so the gate would never fire on the case it was meant to catch.

## v0.30.3 — 2026-05-21 18:24 CEST

### What's new

- `ibkr gamma` text output is easier to read. Dollar gamma values now render as `$9.65B` / `$547.37M` / `$640k` instead of scientific notation (`9.65e+09`). The top-strikes table gains a `NOTIONAL` column (`OI × strike × 100`) between `|GEX|` and `OI`. A `Top strike` line names what share of total |Γ|·OI is parked at the single largest strike, and a `Scope` line spells out the underlying / strike width / expiration count so the absolute magnitudes can't be misread against an SPX-inclusive vendor number. When the swept profile produces no γ-zero crossing the absolute window (e.g. `$627.74–$849.30`) is printed instead of the vaguer "well above/below spot." On the third Friday of the month a one-line `Calendar` row notes that monthly OPEX is today.
- `ibkr gamma --json` gains three additive fields: `top_concentration_pct`, `sweep_low_abs`, `sweep_high_abs`. Existing JSON consumers see no breakage — all three are `omitempty`, the wire schema and methodology token (`perfiliev-bs-sweep-v2-stickymoneyness`) are unchanged.

### Changed

- `ibkr gamma`: dealer GEX magnitudes (`|Γ|·OI sum` and the per-strike `|GEX|` column) render in `$X.XXB` / `$X.XXM` / `$XXXk` form with an explicit `per 1% move` unit, replacing scientific notation. Per-strike notionals (`OI × strike × 100`) appear in a new `NOTIONAL` column.
- `ibkr gamma`: γ-zero distance from spot is now signed from the perspective of γ-zero (`γ-zero $720.50 (−2.9% from spot)`) rather than from the perspective of spot (`spot +2.9%`). Same magnitude, sign flipped to match how the row reads aloud.
- `ibkr gamma`: the no-crossing line names the absolute sweep window (`γ-zero outside swept range $627.74–$849.30`) instead of the qualitative "γ-zero well above/below spot." The `±N% sweep` figure is now derived from the actual `Params.SweepRangePct` rather than hardcoded.
- `ibkr gamma`: new one-line rows — `Top strike  N% of total |GEX| (724P 2026-05-22)` and `Scope       SPY only · ±10% strikes · 6 expirations`. On the third Friday of the month a `Calendar  monthly OPEX today — front-week reading is distorted by expiring contracts` row prints above the warnings block.

### Added

- `rpc.GammaZeroComputed` gains three additive JSON fields (all `omitempty`): `top_concentration_pct` (top strike's share of total |Γ|·OI), `sweep_low_abs`, `sweep_high_abs` (absolute spot bounds of the sweep window in dollars). Surfaced so renderers and downstream consumers don't have to re-derive `spot × (1 ± SweepRangePct)` or `TopStrikes[0].AbsGEX / GammaTotalAbs`.

## v0.30.2 — 2026-05-21 15:03 CEST

### What's new

- Internals-only refactor release. No CLI, MCP, or wire-format changes; `ibkr regime`, `ibkr chain`, `ibkr gamma`, and every other command render identical output to v0.30.1. Skippable for CLI/MCP users; relevant only for anyone importing `internal/daemon` or reading the source.

### Changed

- `internal/daemon` source layout reorganized — no API, no behaviour change. The 2,644-LOC `handlers.go` is now split into `handlers.go` (account / positions / quote / scan / status / breadth), `chain.go` (chain expiries + fetch), `gamma_handler.go` (gamma-zero RPC wrapper), and `snapshot.go` (shared brief-snapshot helpers); `regime.go`'s `populateStreaks` is replaced by a registry loop over per-indicator structs in the new `regime_indicators.go`. Affects readers of the source tree only — the daemon is `internal/`, so no Go importer outside this module is impacted.

### Engineering notes

- `internal/daemon/handlers.go` split from 2,644 LOC into focused files: `chain.go` (~568 LOC — `handleChainExpiries`, `handleChainFetch`, and the expiry/strike/IV helpers only they use), `gamma_handler.go` (~113 LOC — the `handleGammaZeroSPX` RPC wrapper; the compute layer in `gamma_zero_compute.go` et al. is untouched), and `snapshot.go` (~241 LOC — shared `briefSnapshot*` helpers used by chain, gamma compute, regime, and positions). `handlers.go` is now ~1,757 LOC carrying account, positions, quote streaming, scan, status/history, and breadth. `classifyBreadthState`, `normalizeExpiry`, and `daysUntil` stay in `handlers.go` (they have callers outside the moved clusters). Pure code motion.
- `populateStreaks` in `internal/daemon/regime.go` (909 → 854 LOC) replaced with a `streakIndicator`-registry loop over 5 zero-state structs (one per indicator) implementing `key()` / `bandAndValue()` / `attachStreak()`, registered in the new `regime_indicators.go` (~130 LOC). Gate conditions preserved byte-for-byte — `vix_term`, `hyg_spy`, `usdjpy` accept `OK || Stale`; `gamma_zero` requires `Status == OK && Envelope.Result != nil` (Stale not accepted); `breadth` requires `(OK || Stale) && Envelope.State == Ready`. Classifiers (`classifyVIXTermBand` and friends in `regime_streaks.go`), the streak-key constants, the fetchers, `runRegimeFanout`, `handleRegimeSnapshot`, and the CLI renderer are all untouched.

## v0.30.1 — 2026-05-21 12:30 CEST

### What's new

- `ibkr status` now surfaces the daemon's startup pre-warms on the FIRST call after a restart. Previously a status call landing in the brief window between gateway handshake and the prewarm goroutines being scheduled returned `Connected: true` with no `Background:` row, even though gamma-zero, regime-prewarm, and (when needed) breadth-spx work was about to start a few milliseconds later. The second status call seconds later showed them. The race window is closed; one call is enough.
- Breadth (SPX 50-DMA / 200-DMA / new highs–lows) now catches up on morning startup when the cached snapshot was taken before yesterday's 16:00 ET close. Previously a daemon restart cluster across yesterday's 16:35 ET tick would leave a partial pre-close snapshot in cache; the scheduler then sat on it until today's 16:35 ET because it compared only the snapshot's NY session-key (which matched yesterday) without checking whether the snapshot was actually from before yesterday's close. Worst case was ~10 hours of stale partial-day data on a normal weekday morning startup. Weekends roll back correctly: a Friday-close snapshot examined Monday morning still does not trigger a spurious refresh.

### Changed

- `ibkr status` reports `Connected: true` once the daemon's post-connect initialization completes, not at the instant of TWS handshake. The shift is normally ~50–100 ms — invisible during a healthy startup — but the new semantic guarantees that any `Connected: true` response also carries the full set of in-flight background tasks. Previously, a status call landing in the gap between the connection read-loop flipping `c.ready=true` and `postConnectSetup` finishing its synchronous sentinel-setting reported `Connected: true` with an empty `Background:` row.

### Fixed

- `ibkr status` BackgroundTasks list is now coherent with the daemon's connect-complete edge: `regime-prewarm`, `gamma-zero`, and `breadth-spx` (when a bootstrap refresh will fire) all appear before the first status call returns `Connected: true`. The sentinels are now set on the launching goroutine (not inside the spawned worker) and a `postConnectSetupDone` barrier gates the `Connected` field so a status poll arriving within the connection-establish window waits for full initialization rather than reading partial state.
- Breadth scheduler's startup catch-up now triggers when `snap.AsOf` predates the most recent past weekday 16:35 ET tick, regardless of whether the snapshot's NY session-key matches "yesterday." Closes the partial-snapshot stale window described above.
- Breadth scheduler distinguishes transport errors from below-threshold coverage in its retry policy. A `Refresh` that errors (gateway down, bulk-connector not yet ready, ctx-cancel upstream) now backs off 30 s and retries, instead of incrementing the below-threshold counter and waiting 12 min between attempts. Worst-case prior behavior was 15 × 12 min = 3 hours of silent retry storms after a startup-time hiccup before falling through to the daily cadence.

### Added

- `Engine.MarkPendingBootstrap()` in `internal/breadth/spx`: pre-sets the breadth engine's in-flight flag iff `shouldRefreshOnStartup` would fire against the current snapshot + clock. Called from `postConnectSetup` immediately before `go e.Run(ctx)` so `ibkr status` reflects an imminent bootstrap refresh without waiting for the goroutine to be scheduled. No-op when no bootstrap will fire, so the flag never sticks.

### Engineering notes

All three bugs trace back to the same shape: a sentinel or observable that the launching goroutine sets only after the spawned worker has executed its first lines. The daemon's `postConnectSetup` flips the connector's `IsReady` true at the start of the function, but `regimePrewarming.Store(true)` was inside `prewarmRegimeSymbols`, `c.current` was assigned inside the prewarmZeroGamma goroutine's `kickOrJoin` call, and `engine.refreshing` was set inside `Refresh`. The fix in each case is to set the observable state synchronously on the launching goroutine — for gamma, the existing `spawnJob`/`startLocked` split in `gamma_zero_cache.go` already separates "create the placeholder under cache mutex" from "run the compute on a fresh goroutine," so a straight synchronous `kickOrJoin` call works without changing the compute's lifetime. Breadth needs the new `MarkPendingBootstrap` helper because the bootstrap decision lives entirely inside the scheduler's `Run` loop and we have to mirror the predicate from the launcher. The third bug (scheduler retry conflation) was found by the reviewer during the design pass; it has the same conceptual shape as Bug 1 ("startup-time transient state is misclassified") but a different consequence (12-min back-off storm instead of UI gap).

## v0.30.0 — 2026-05-21 11:29 CEST

### What's new

- `ibkr gamma` now lands a real result off-hours. The pre-market dealer zero-gamma compute previously aborted with `low_coverage` (≈1% legs landed); after this release the typical pre-market run lands 85–90% of legs and produces a usable γ-zero signal. Methodology and result envelope are unchanged. RTH behavior is unchanged.
- `ibkr chain SYM` and `ibkr chain SYM --expiry YYYY-MM-DD` now print a one-line yellow disclosure when the U.S. equity-option session is closed: "Options markets closed · IV is model-computed by IBKR from prior-session prices; bid/ask resume 09:30 ET". Suppressed during RTH; the existing dim "IV is delivered as a model-computation tick" caption stays put either way.

### Changed

- Dealer γ-zero compute now runs the worker fan-out under the daemon's default `MarketDataType` (frozen-aware). The v0.28-era switch to `MarketDataType=1` (live) was suppressing the OPTION_COMPUTATION model ticks pre-market and is reverted.
- Dealer γ-zero leg fetcher no longer gates leg acceptance on open-interest delivery. Per-strike OI is genuinely sparse off-hours — even IBKR's own TWS UI shows OI only on actively-traded strikes — and the prior hard gate dropped legs that had real model-tick IV but no OI tick. OI is now read opportunistically and a missing tick yields `oi=0` on that leg (contribution to dealer GEX is zero either way; the strike still enriches the IV surface for skew fitting).

### Added

- `rpc.IsOptionRTH(now time.Time) bool` — clock-based helper (weekdays 09:30–16:00 ET, fail-open if `America/New_York` is unavailable). Used by `ibkr chain` renderers to gate the off-hours disclosure; preferred over `rpc.IsLiveDataType` for option-context surfaces because the SPY-style ETF can stay `data_type=live` via extended-hours ARCA quoting while CBOE options are closed.

### Fixed

- `Connector.UnsubscribeMarketData` now releases the rate-limiter's market-data slot for every subscribed reqID regardless of whether a price/size tick was ever observed. The prior `&& sub.Observed` guard skipped `CancelMarketData` (and therefore `releaseMarketDataSlot`) for OPT subscriptions that received only OPTION_COMPUTATION (msg 21) model ticks — `Observed` is set by `handleTickPrice` / `handleTickSize` but not by `handleOptionComputation`. Off-hours, every option-chain fan-out leg leaked one slot in `rateLimiter.marketDataSubs`; the 100-slot semaphore filled in ~2 min, the rate limiter recorded 5 consecutive errors, the circuit breaker opened, and the dealer γ-zero compute aborted with `low_coverage`. With the guard removed the cancel fires unconditionally on a connected wire, slots are recycled, and the fan-out can complete.

### Engineering notes

This release closes the off-hours dealer-γ regression that had survived several rounds of fixes through v0.28 and v0.29. The previous attempts (lower coverage threshold, longer per-leg budget, bulk prewarm, hold-underlying lifetime, BS-IV fallback solver) all addressed real adjacent issues but didn't reach the slot-leak root cause — the rate-limiter side never showed up in `gamma.abort` logs because the cascade surfaced as `throttled` or `low_coverage` downstream. Diagnostic instrumentation in this cycle's debugging branch printed the failing rate-limit path (`mdsubs_cap_100`) and the slot count at trip time (`100/100`), which immediately pointed at the `sub.Observed` guard. The clock-based off-hours banner is a side-effect of the same investigation: `rpc.IsLiveDataType` was not a sufficient signal for option-market-closed because SPY's underlying stays `live` on ARCA extended hours.

## v0.29.0 — 2026-05-20 23:00 CEST

### What's new

- `ibkr regime` gamma no longer aborts at "23 of 1632 legs" during US market hours. The daemon now bulk-resolves the option chain via a partial-contract `reqContractDetails` per expiration (6 round-trips instead of ~1600), persists option ConIDs across restarts, and filters out non-listed strikes before fan-out.
- `ibkr regime` default layout drops `day N` streaks, `est NNNs` clocks, and parenthetical reason restatements for a cleaner scan. A column header row keys the five columns (state · indicator · value · band · note). Pass `--explain` for the full provenance + methodology view.
- Daemon idle timeout extended from 5 min to 15 min so a pause between calls no longer discards the in-memory gamma + option-contract caches. Override via `[daemon] idle_timeout = "30m"` in `~/.config/ibkr/config.toml`.

### Added

- `Connector.PrewarmOptionChain` bulk-resolves an option chain by issuing one partial-Contract `reqContractDetails` per expiration (no Strike/Right). The gateway streams every listed (Strike, Right) combination back in one burst; the resolved ConIDs populate the in-memory option contract cache. Used internally by the gamma compute before the worker fan-out; callers fanning out many option subscribes can invoke directly to skip the per-leg resolution tax.
- INFO logging across the gamma compute path: `gamma.kickoff`, `gamma.jobs`, `gamma.prewarm` (per-expiry + summary), `gamma.filter`, `gamma.fanout.done`, `gamma.done`, and `gamma.abort` with reason attribution. Makes failure diagnosis self-contained from `daemon.log`.
- Option contract persistence in `~/.cache/ibkr/contracts.json` (file schema bumped to v2 with an `options` map alongside the existing `contracts`). Entries are GC'd at save time once their `Expiry` is in the past, so the file stabilises at a few thousand live-contract rows.
- `RetryOfErrorAt` / `RetryOfErrorSummary` on the gamma RPC envelope. When the in-flight compute is a retry of a recently-failed run (within `gammaErrorRetryTTL`), the renderer surfaces `computing · retry of <summary> at HH:MM:SS` instead of silently dropping the prior error context across the 60-second cooldown boundary.
- `Connector.SnapshotOptionContracts`, `Connector.SeedOptionContracts`, `Connector.IsOptionContractCached` for daemons and callers that need to drive the option-contract cache directly.
- `ContractDetailsLite` now carries `SecType`, `Expiry`, `Strike`, `Right` (omit-empty for non-OPT entries). Required to key bulk-resolved option entries by their OPRA-style tuple.

### Changed

- Daemon default `idle_timeout`: `5m` → `15m`. Combines with the new disk-persisted option contract cache to make brief pauses essentially free.
- Gamma `MinLegCoverageFraction`: `0.5` → `0.2`. The IBKR gateway's OPT model-tick delivery during RTH is bursty; 20-40% leg coverage is typical, not a degraded run. The previous 50% threshold discarded usable results and forced the 60-second retry cooldown to fire repeatedly, leaving the dashboard "computing" for 5-10 minutes per session.
- `ibkr regime` default rows trimmed: header row added; per-row `day N` streak markers, `est NNNs` clock suffixes, and parenthetical reason restatements removed. All four are still surfaced under `--explain`.
- Help text for `ibkr regime` tightened: the `--log` description collapsed from a 300-character paragraph to a one-liner; methodology prose moved into `--explain`.

### Engineering notes

The 23/1632 abort pattern had three compounding causes: (a) per-leg `reqContractDetails` ran into IBKR's per-account rate limit under burst, (b) the 5-min idle timeout discarded the in-memory option contract cache between user sessions, and (c) the compute path had zero observability so even the diagnosis was hard. v0.29.0 addresses all three: bulk resolution sidesteps the rate limit, disk persistence survives restarts, and INFO logs at `gamma.*` make diagnosis trivial. The `MinLegCoverageFraction` drop to 0.2 is the only behavior change that affects ranking — runs that previously failed are now persisted, giving the dashboard a real γ-zero number instead of repeated "computing" placeholders.

## v0.28.2 — 2026-05-20 18:45 CEST

### What's new

- `Ctrl+C` and the OS "Quit" action now actually terminate the `ibkr daemon` and `ibkr mcp` processes. Previously both ignored SIGTERM/SIGINT and required Force Quit (SIGKILL); now they shut down within a few seconds.

### Fixed

- `ibkr daemon` process now exits on SIGTERM/SIGINT instead of hanging until SIGKILL. `pkill ibkr`, Activity Monitor → Quit, and shell-driven shutdown all work without escalating to Force Quit. Graceful shutdown completes within ~4 s on a connected daemon.
- `ibkr mcp` process now exits on SIGTERM/SIGINT instead of hanging until SIGKILL. Killing the MCP host (e.g. Claude Desktop quitting and orphaning the MCP child) no longer leaves a zombie process holding the daemon connection open.

## v0.28.1 — 2026-05-20 18:11 CEST

### What's new

- Pre-market and weekend `ibkr gamma` finishes roughly 3× faster — wall-clock drops from ~20 min to ~6 min on a typical SPY chain. RTH timing is unchanged. The γ-zero level, methodology, and result envelope are unchanged.

### Changed

- Per-leg poll budget in the dealer γ-zero compute shrunk from 5 s to 1.5 s. The prior 5 s was waste on the pre-market path where the gateway's option-model engine never pushes a tick and every leg burned the full budget before falling through to the BS-IV fallback solve; 1.5 s remains 3× the typical-arrival headroom for live RTH model ticks.

### Fixed

- Option subscriptions that hit a terminal gateway error (200 "no security definition", 320/321/322 update-encoding failures, 354 "not subscribed", 10197 "competing live session") now abort their pollers immediately instead of running out the per-leg deadline. The gamma compute previously wasted up to 5 s per "ghost strike" — chain-dedupe entries that don't exist as listed contracts on every expiry — before the fan-out could move on. Combined with the budget shrink above, pre-market wall-clock on a chain with many ghost strikes drops by an additional margin.

## v0.28.0 — 2026-05-20 15:46 CEST

### What's new

- The dealer γ-zero compute now reprices each leg's implied vol at the
  scenario-spot's moneyness via a per-expiry skew curve, instead of
  holding the snapshot IV fixed across the sweep. Expect the headline
  γ-zero level to shift ~30-80 SPX points and track SpotGamma's free
  Friday-recap posts materially better. The methodology token bumps
  from `perfiliev-bs-sweep-v1` to `perfiliev-bs-sweep-v2-stickymoneyness`.
- `ibkr breadth` now reports the 200-day reading alongside the 50-day
  reading and counts how many S&P 500 names are at fresh 52-week
  highs vs lows today. The narrow-rally pattern — SPX at highs with
  almost nothing making new highs underneath it — is now visible at
  a glance.
- `ibkr regime` now shows how many consecutive sessions each indicator
  has been in its current band — "(yellow · day 3)" reads inline with
  the row, so a single snapshot tells you whether today's flip is
  day 1 of a stress event or day 5.
- `ibkr gamma` now reports separate γ-zero readings for the near
  bucket (DTE ≤ 7 days) and the term bucket (DTE > 7), alongside the
  combined headline. When the two readings disagree the regime row
  flags it inline.
- Use `ibkr regime --log <path>` to append today's snapshot as a single
  JSONL line — one object per invocation, top-level keys
  `{timestamp, regime}`. Plain append-only file; `jq` and pandas both
  read it. Useful for the 4-week SpotGamma cross-check the spec
  describes (run from cron each weekday after close, analyse later).

### Added

- Sticky-moneyness skew model on the gamma envelope: fits a quadratic
  in `m = ln(K/S)` per expiry, clamps evaluations to the observed
  moneyness range. `SkewModel = "sticky-moneyness-v1"` names the
  fitted model on the wire; `SkewFitQuality` carries per-expiry
  `{points, r_squared, range}` diagnostics. Expiries that fail to fit
  fall back to sticky-IV individually and surface as
  `skew_fallback:YYYYMMDD` warnings.
- Near vs term split on the gamma envelope: `ZeroGammaNear`,
  `ProfileNear`, `GammaSignNear`, `NearLegCount` (DTE ≤ 7); symmetric
  term fields. The regime gamma row carries a `HorizonAgreement`
  string (`both_above` / `both_below` / `diverge` / `near_only` /
  `term_only`) so a consumer can detect the high-information case
  where near and term γ-zero straddle spot.
- `StreakInfo` field on every regime row (VIX/VIX3M, HYG/SPY, USD/JPY,
  gamma, breadth): `{band, sessions, since}` counting consecutive
  trading sessions in the current band. Persisted across daemon
  restarts at `$XDG_CACHE_HOME/ibkr/regime-streaks.json`.
  Computing/unavailable/error states freeze the counter rather than
  reset it.
- Add `BreadthSPXResult.PctAbove200DMA` (% above 200-day SMA), plus
  `NewHighsToday`, `NewLowsToday`, and `NetNewHighsPct` for the
  rolling 252-bar new-highs/lows count. `BreadthDailyValue` carries
  all four numbers per history point so the trailing series renders
  cleanly. `RegimeBreadth` echoes the same fields onto the regime
  row.
- Add `ibkr regime --log <path>` for the calibration-ritual JSONL
  append. Each invocation appends one JSON object on its own line.
  No `--replay` subcommand: ship `--log` first, decide the right
  extraction shape after the log accumulates real data.

### Changed

- The `Method` token on `GammaZeroComputed` is now
  `perfiliev-bs-sweep-v2-stickymoneyness`. Pure cutover — no
  dual-emit; SpotGamma's Friday posts are the comparator.
- The `Method` token on `BreadthSPXResult` is now
  `constituent-fanout-50/200dma+nh-v2`. The breadth refresh now pulls
  ~262 daily bars per constituent at cold-start (was ~60) so all
  three readings — 50-DMA, 200-DMA, and the rolling 252-bar
  max/min — seed from the same fetch. `Store.LoadSnapshot` now
  treats any snapshot whose `Method` differs from the current
  constant as no-cache, forcing a cold rebuild on methodology bumps.
  Without this gate, a stale v1 snapshot.json silently decoded into
  a v2 struct with the new fields zeroed and the engine published
  the phantom "0% above 50-DMA" reading as if it were real.

### Removed

- Drop `BreadthSPXResult.Value` and `BreadthDailyValue.Value`. The
  field's renamed equivalent `PctAbove50DMA` carries the same number
  with a more honest name now that the two-window reading needs
  disambiguation. Consumers reading `Value` must migrate.

### Engineering notes

Skew curve: quadratic in `m = ln(K/S)`, fit per expiry via Cramer's
rule; calls and puts pool into one fit (put-call parity doubles
sample size). Near/term boundary hardcoded at 7 DTE. Cutover revert
criterion: if 4-week sign-agreement vs SpotGamma's Friday recap drops
below v1, roll back. Streak store is its own JSON file with version
header and atomic temp+rename, same shape as 2fbd614's contract
store but separate invalidation. Breadth's `WindowSet` and
`HistorySet` bump to v2; v1 caches cold-rebuild. Cold-start cost is
unchanged — IBKR's pacing limit is per-request, not per-bar.
50-DMA bands stay at 55/40 (recalibration is a separate decision);
200-DMA bands at 60/40 — StockCharts' 70/30 default would have
flagged red routinely in 2024-25 from Mag-7 concentration.

## v0.27.12 — 2026-05-20 10:33 CEST

### What's new

- `ibkr breadth` now populates on the first refresh after a fresh
  install — previously its on-disk cache could fail to converge,
  leaving every call cold for the daemon's lifetime. Berkshire B,
  Brown-Forman B, and Eversource Energy in particular were silently
  dropped from the S&P 500 breadth compute; they're now included.
- Daemon restarts no longer pay the full IBKR contract-lookup
  rate-limit cost: known stocks are loaded from a local cache and
  re-resolved only when the S&P 500 list itself has changed.
- Nothing to reinstall or reconfigure. CLI and MCP surfaces are
  unchanged.

### Added

- Persistent contract cache at `~/.cache/ibkr/contracts.json` — the
  daemon saves resolved IBKR contract identities on every successful
  lookup and reloads them at startup. The S&P 500 membership is
  hashed into the file so a reconstitution between runs prunes stale
  members at load while keeping the rest of the cache useful.
- Dedicated bulk-historical IBKR client (configurable via
  `gateway.breadth_client_id`, defaults to a second client ID) so
  the breadth refresh's ~500-name fan-out runs against its own
  rate-limit budget and no longer competes with interactive commands
  and the gamma compute for the primary connection's slots.

### Changed

- The breadth refresh now persists per-symbol daily-bar windows on
  every pass, including passes that didn't reach the 80% coverage
  threshold. Earlier behaviour discarded partial progress; now each
  refresh builds on the last, and a below-threshold pass schedules
  a retry within ~12 min instead of waiting until the next 16:35 ET
  tick. Combined with the cache above, cold-start convergence drops
  from "never" (under IBKR's per-account contract-lookup cap) to
  roughly one bootstrap pass + one retry.
- IBKR symbols with the S&P dual-class dot convention (e.g. `BRK.B`,
  `BF.B`) are translated to IBKR's wire form (`BRK B`, `BF B`) for
  every contract request, so the breadth compute includes them
  instead of failing them as "no security definition". The
  translation runs on a regex pattern so future dual-class additions
  to the S&P 500 work without a code change.

### Fixed

- Historical-data requests no longer fail with `request timeout
  after 30s` when many are in flight at once: the rate limiter's
  outer timeout is now type-aware so historical requests get a
  budget consistent with IBKR's documented 60-per-10-minute pacing.
- Slow historical token waits no longer head-of-line-block unrelated
  requests (contract-detail lookups, market-data subscribes) queued
  behind them. Each rate-limited request is now dispatched to its
  own goroutine; concurrency is bounded by the rate-limit primitives
  themselves.
- Ticker `ES` (Eversource Energy) is no longer misclassified as the
  E-mini S&P futures contract — that case was dead code that broke
  the Eversource stock for breadth without ever benefiting any
  futures caller.

### Engineering notes

Closes a long-standing convergence failure in the constituent-fanout
breadth compute: prior to this release `~/.cache/ibkr/breadth-spx/`
could remain empty indefinitely because every refresh started cold,
exhausted IBKR's per-account contract-detail bucket on the first
~50 names, and never persisted partial progress. Verified live
against TWS on 2026-05-20: first refresh after the fix landed
converged at 455/503 coverage (90.5%) and produced a stable
`ibkr breadth` reading.

## v0.27.11 — 2026-05-20 05:48 CEST

### What's new

- `ibkr regime` and `ibkr breadth` are now reliable right after the daemon
  starts up — they no longer occasionally return errors under early load.
- Nothing to reinstall or reconfigure. CLI and MCP surfaces are unchanged.
- **Breaking (Go library only):** market-data subscription methods on
  `Connection` and `Connector` now take a leading `context.Context` — see
  Changed.

### Changed

- **Breaking (Go library):** `Connection.RequestMarketData`,
  `RequestMarketDataWithContract`, `RequestMarketDataWithPrimary`, and the
  `Connector.SubscribeMarketData` / `EnsureMarketDataSubscription` helpers
  now take a leading `ctx context.Context` argument. Pass the caller's
  per-request ctx; `nil` is treated as `context.Background()` so callers
  without a deadline see no behaviour change.

### Fixed

- Per-request `ctx` deadlines on market-data subscribes now fire as
  intended. Previously the slot-acquire wait blocked on the connection's
  lifetime ctx, so caller-set timeouts were silently absorbed until the
  connection itself closed.

### Engineering notes

Lands the structural fix promised in v0.27.9, which had added
`boundedSnapshot` as an orchestrator-level defense and noted the
slot-layer fix as a follow-up. The slot semaphore's wait was blocking
on the connection's lifetime ctx, so caller-set per-request budgets
were silently absorbed and only released when the connection itself
closed.

`boundedSnapshot` is kept as defense-in-depth — its budget+1s timer
catches future regressions in any inner code that blocks past its
declared budget.

## v0.27.9 — 2026-05-19 17:45 CEST

### What's new

- `ibkr regime` no longer returns spot rows with a "fan-out exceeded
  handler deadline" error message when called while breadth is mid-cold-
  start. The per-fetcher 5–8 s budgets are now enforced at the regime
  layer (defense-in-depth; the structural fix lands in v0.27.11).
- `ibkr status` now reports `regime-prewarm` during daemon startup pre-
  warming, alongside the existing `breadth-spx` and `gamma-zero`.
- **Wire addition (library / MCP consumers):** gamma's snapshot exposes
  a new `cold` state on the wire (previously folded into `computing`).
  Treat `Status="cold"` as "no compute has run this session yet" —
  `fetchRegimeGamma` maps it to `unavailable` in the regime row.

### Added

- Gamma snapshot exposes a `cold` state on the wire
  (`GammaZeroStatusCold = "cold"`) for "no compute has run this
  session yet". Previously folded into `computing`. `fetchRegimeGamma`
  maps `cold` → `unavailable` in the regime row, mirroring breadth's
  Cold handling. The schema is ahead of need since the gamma compute
  currently auto-kicks on every call.

- Gamma compute now surfaces an error (not a warning-flagged result)
  when leg coverage drops below `MinLegCoverageFraction = 0.5`. The
  cache layer then re-attempts within the same NY trading session via
  `gammaErrorRetryTTL`, closing the v0.27.0-class poison-cache failure
  mode for gamma. Threshold is 0.5 (vs breadth's 0.8) because the
  OI-weighted gamma compute concentrates near ATM, so missing far-OTM
  legs has limited impact above 50% coverage.

- `ibkr status`'s `background_tasks` list now includes `regime-prewarm`
  during daemon startup pre-warming. Closes the gap where an autospawned
  daemon could idle-out mid-prewarm because the task was invisible to
  the idle watcher.

### Changed

- `HealthResult.BackgroundTasks` (the wire field `background_tasks`) and
  the internal `isBusy()` predicate now ride a single registered set.
  Previously two parallel switch statements that could silently drift —
  a real risk after v0.27.6 made the wire field operationally
  load-bearing for release gating.

- Regime fan-out timeout error message now names the contending task.
  Example: `"regime fan-out exceeded handler deadline (contended with
  daemon-internal task(s): breadth-spx)"` instead of v0.27.6's generic
  hedge. The named tasks reflect daemon state at the moment the
  deadline fired.

- `ibkr gamma`'s leg-coverage gate is now unit-testable via a
  `checkLegCoverage` helper. The compute's error message includes
  throttle attribution, so users see e.g. "gateway throttled X of Y
  legs" directly instead of having to combine two signals.

- `GammaZeroComputed.Warnings` doc no longer references the stale
  `"low_leg_coverage"` warning (low coverage is now surfaced as an
  error).

### Fixed

- Regime spot fetchers (`ibkr regime` fan-out) now respect their per-
  leg budgets (5–8 s) at the regime layer via a budget-bounded snapshot
  wrapper. Previously a saturated market-data slot pool absorbed the
  budgets until the orchestrator's 45 s handler ctx fired, producing
  the user-visible 45 s wait. Structural fix at the slot layer lands
  in v0.27.11.

- `release-verify.sh` step 7 (regime call-sequence drop check) also
  gates output shape under contention. v0.27.6 added the breadth-mid-
  fan-out wait; this release adds an assertion that any regime row
  whose `error_message` contains `"fan-out exceeded handler deadline"`
  blocks the release — that error message is the exact regression
  signature this release closes.

### Engineering notes

Two-part release. (1) Regime spot fetchers' per-leg budgets were being
absorbed by `acquireMarketDataSlot` blocking on the connection's
lifetime ctx rather than the caller's; the minimal-blast-radius defense
here adds a budget-bounded snapshot wrapper at the regime layer, with
the structural fix landing in v0.27.11. (2) Brings gamma's wire shape
(Cold state) and persistence guard (coverage threshold) up to par with
the breadth engine, closing the same poison-cache-from-partial-fan-out
class that v0.27.0 → v0.27.3 closed for breadth.

## v0.27.7 — 2026-05-19 16:28 CEST

### What's new

- `ibkr regime` opens with a color-coded headline — SPY spot + day's
  dollar and percent change, then VIX spot + percent change — before the
  indicator table. SPY change colors long-default (green up / red down);
  VIX is inverted (red up = vol expanding = risk-off).
- Nothing to reinstall. CLI output gains one header line.
- **Wire addition (library / MCP consumers):** `RegimeVIXTerm` now
  carries `VIXPrevClose` and `VIXChangePct`; `RegimeHYGSPYDivergence`
  now carries `SPYPrevClose`, `SPYChange`, and `SPYChangePct`. Consumers
  that ignore unknown fields are unaffected.

### Added

- SPY + VIX headline above the `ibkr regime` indicator rows. Format
  `SPY 530.42  +1.20  (+0.23%)    VIX 14.50  (−2.10%)`. SPY change
  colors long-default (green up / red down); VIX is inverted (red up
  = vol expanding = risk-off). A missing previous-close anchor falls
  back to a dim `(—)` placeholder rather than fabricating zero.

- Wire fields on `RegimeVIXTerm` (`VIXPrevClose`, `VIXChangePct`) and
  `RegimeHYGSPYDivergence` (`SPYPrevClose`, `SPYChange`, `SPYChangePct`).
  Populated from the gateway's previous-close tick that already flows
  alongside existing subscribes — no extra round trips. The VIX anchor
  survives a VIX3M failure so the header is useful even when the
  term-structure leg drops.

## v0.27.6 — 2026-05-19 16:04 CEST

### What's new

- `ibkr regime` no longer hangs and times out with `regime: context
  deadline exceeded` when a second call arrives while breadth or gamma
  are running. The handler now returns within its budget; contending
  rows are surfaced as `Status="error"` with a clear contention message.
- Nothing to reinstall or reconfigure.

### Fixed

- Regime fan-out (`ibkr regime`) now returns within its 45 s ctx
  deadline even when individual fetchers hang. Rows still completing
  at the deadline are surfaced as `Status="error"` with
  `ErrorMessage="regime fan-out exceeded handler deadline (gateway
  likely under contention from concurrent breadth/gamma work)"`.
  Previously a single stuck fetcher could block the whole handler
  indefinitely.

- `pkg/ibkr.FetchHistoricalDailyBars` gains a ctx-aware variant
  `FetchHistoricalDailyBarsCtx(ctx, sym, days)` that propagates the
  caller's deadline into the gateway timeout. An upstream cancellation
  now stops the HMDS fetch promptly rather than running to the legacy
  20 s internal timer. Regime fetchers switch to the new variant;
  breadth and gamma keep the legacy `(sym, days, timeout)` call.

### Added

- `release-verify` regime call-sequence check (`scripts/release-verify.sh`
  step 7) now waits for breadth to enter cold-start fan-out before
  firing — polls `ibkr status --json` for `breadth-spx` in
  `background_tasks` (up to 20 s), then sleeps 8 s for the fan-out to
  reach steady-state HMDS pressure. Reproduces the production
  contention the v0.27.5 smoke missed against a quiescent daemon.

### Engineering notes

The v0.27.5 tests exercised the per-fetcher drop layer and missed the
orchestrator's deadline handling — a second `ibkr regime` arriving
mid-breadth-fan-out hung the handler. This release gates the
orchestrator and adds a release-verify check that reproduces the
contention against a daemon known to be mid-fan-out before asserting
output shape.

## v0.27.5 — 2026-05-19 15:03 CEST

### What's new

- No user-visible behaviour change. `ibkr regime` continues to report
  dropped indicators as `error` or `unavailable` rather than serving a
  stale cache — this release just makes that contract testable and
  enforced by the release gate.
- Nothing to reinstall.

### Added

- `release-verify` step 7 (regime call-sequence drop check): two `ibkr
  regime --json` calls 30 s apart against the smoke gateway; if any
  row's `status` downgrades from `ok`/`stale` to `error`/`unavailable`
  between them, the release aborts before the tag is created.
  One-directional (`computing → ok` and recovery are fine), so
  off-hours flake patterns that error on both calls don't block the
  release.

- Regression test `TestRegime_CallSequence_HonestUnavailable` pins the
  honest-unavailable contract for the four live-gateway risk surfaces
  (VIX3M timeout, USD.JPY FX miss, SPY tick 165 miss, SPY 252d
  fallback collapse).

- Regression test `TestClassifyBreadthState` pins the breadth handler's
  `(snapshot, refreshing)` → `State` enum mapping (a v0.27.3 fix that
  was previously folklore).

## v0.27.4 — 2026-05-19 09:31 CEST

Applies the pattern from v0.27.3's wire-state fix codebase-wide. The
v0.27.3 review surfaced that gamma compute had the same shape as the
breadth idle-timeout issue (a daemon-internal task running while no
client is connected) — fixed defensively here. Also adds an external
visibility surface for daemon-internal work, so a user running `ibkr
status` against an autospawned daemon can see what it's actually
doing.

### Added

- **`HealthResult.BackgroundTasks`** — typed list of daemon-internal
  long-running computes that are running RIGHT NOW. Presence in the
  list is the state ("this task is running"); idle/ready/cold tasks
  are omitted entirely. Always emitted as a (possibly empty) slice
  so consumers can rely on `len(background_tasks) == 0` for "idle"
  without inferring from absence. Current values:
  - `breadth-spx` — the SPX 50-DMA breadth engine is running a
    refresh (cold-start bootstrap or daily post-close refresh).
  - `gamma-zero` — the SPX zero-gamma compute is fanning out across
    option legs.

- **`ibkr status` shows a `Background:` line** when any task is
  running, comma-separated by name. Compact (single line, no ETA
  noise) so an idle daemon's status display is unchanged. Pinned by
  `TestRenderStatus_BackgroundLine`.

- **`gammaZeroCache.IsComputing()`** — public accessor mirroring
  `breadth.IsRefreshing()`. Used by both `isBusy()` and the status
  handler so the two surfaces stay coherent.

### Changed

- **`s.isBusy()` now defers idle shutdown for gamma compute too.**
  v0.27.2 added the predicate but only checked breadth. Gamma
  compute is faster than the 5-min idle window in practice (~1–2
  min), so the gap never bit in observable usage, but the
  architectural hole is the same shape as breadth's: a long-running
  daemon-internal task can outlive the CLI invocation that triggered
  it. Pinned by `TestIsBusyIncludesGammaCompute`. Future long-
  running tasks add their own clauses here and a matching entry in
  `handleStatusHealth` — comment in `isBusy()` documents the
  contract.

- **`scripts/release-verify.sh` step 2** now asserts
  `status.background_tasks` is always a list with well-formed
  entries (string `name` per item). This is the v0.27.x pattern from
  the verification-review skill applied to the new surface: the
  contract is "always emitted" so consumers can rely on `len() == 0`
  for idle; absent or non-list shape would break MCP/CLI consumers
  that read the field without nil-checking.

## v0.27.3 — 2026-05-19 09:05 CEST

After v0.27.0 → v0.27.1 → v0.27.2 each shipped a different lifecycle
bug in the breadth engine, an external review surfaced that the root
cause was missing observability: every consumer had to side-channel
via `engine.IsRefreshing()` to disambiguate cold-start from
ready-to-render, which mis-classified the poisoned v0.27.0 cache as
"ok". This release makes engine state a first-class wire field so the
bug pattern is structurally impossible.

### Added

- **`BreadthSPXResult.State`** with values `cold | computing | ready
  | degraded`. The single source of truth for consumers — replaces
  the side-channel pattern in `fetchRegimeBreadth` that triggered all
  three v0.27.x bugs. `cold`: engine hasn't computed a snapshot.
  `computing`: refresh in flight. `ready`: snapshot present and
  reliable. `degraded`: reserved for a future schema where
  below-threshold snapshots persist with a warning marker; v0.27.3
  engine refuses to persist below threshold so this state isn't
  currently produced.

- **`MinCoverageFraction = 0.80` in `internal/breadth/spx`.** The
  v0.27.1 `Coverage == 0` persist-guard generalised: a refresh whose
  coverage falls below 80% of `MemberCount` is "did not converge"
  and is not persisted. Tolerates ordinary per-name fetch errors
  (delisted tickers, transient pacing) while rejecting catastrophic
  fan-outs. Pinned by `TestEngineRefreshBelowCoverageThresholdIsNotPersisted`.

- **`TestStartDoesNotLaunchBreadthBeforePostConnect`** in
  `internal/daemon/server_test.go`. Asserts that with a blocking
  fake attempter (modelling "gateway accepted TCP but never
  handshook"), the breadth fetcher receives zero invocations
  throughout the test. Directly pins the v0.27.0 bootstrap-race
  invariant rather than relying on retrospective regression tests
  written after each shipped bug.

- **`release-verify.sh` step 6: breadth state coherence.** Asserts
  `state ∈ {cold, computing, ready, degraded}`, `state == "ready"
  implies value > 0` (the unambiguous v0.27.0 poison-cache
  fingerprint), and `state == "cold" implies value == 0` (a finalise
  bug would let value leak without a snapshot). The check completes
  in seconds against any state of the engine; it doesn't wait for a
  cold-start bootstrap to finish.

### Changed

- **`fetchRegimeBreadth` reads `result.State` directly** instead of
  calling `s.breadth.IsRefreshing()` alongside `handleBreadthSPX`.
  Removes the consumer-side check that mis-classified the v0.27.0
  poison.

- **`unaryDeadline(MethodBreadthSPX)`: 20s → 2s.** The handler is a
  pure projection of in-memory engine state (`Get()` +
  `History()`) since v0.27.0; the 20s budget was inherited from the
  pre-v0.27.0 live-INDEX-feed implementation. A handler taking
  more than 2 s here would signal mutex contention or a stuck
  scheduler, not legitimate work.

## v0.27.2 — 2026-05-19 08:28 CEST

The third bug in the v0.27 release cycle, plus a correction to my own
cold-start latency claims. v0.27.0 introduced the breadth engine,
v0.27.1 fixed the cold-start race and the poison-cache; this release
keeps the daemon alive long enough for the bootstrap to actually
complete.

### Fixed

- **Idle watcher now defers shutdown while the breadth engine is
  refreshing.** The daemon's default 5-minute idle timeout was
  killing the cold-start bootstrap (which takes ~60 min — see the
  correction below) any time a user autospawned the daemon and
  walked away. A new `s.isBusy()` predicate consults
  `breadth.IsRefreshing()`; the idle watcher resets its timer
  instead of shutting down whenever a background refresh is in
  flight. Pinned by `TestRunIdleWatcherDefersShutdownWhileBreadthRefreshing`.
  The predicate is extensible to gamma later if we observe the same
  pattern there (in practice gamma compute finishes inside the 5 min
  idle window, so it isn't currently affected).

### Changed

- **Cold-start latency claims corrected from "~10–15 min" to "~60
  min".** v0.27.0's documentation across README, CHANGELOG entry,
  the `internal/breadth/spx/engine.go` docstring, and the regime
  CLI's `computing` row reason all named ~10–15 min for the first
  bootstrap. The real number is dominated by IBKR's historical-data
  pacing limit (60 requests per 10-minute sliding window, hard cap
  per gateway connection), so 503 constituent fetches sustain
  ~6 names/min after the initial burst — observed cold-start in
  this release: 8.4 fetches/min, extrapolated full bootstrap ~60 min.
  Adding workers above the default 6 doesn't help because the
  gateway throttles us. The regime row's `computing` reason now
  mentions `ibkr daemon --foreground` so users know how to keep
  the daemon alive through the bootstrap; README adds the same
  workaround.

## v0.27.1 — 2026-05-19 06:34 CEST

Two startup bugs in v0.27.0 that prevented the breadth engine from
producing a real number on a fresh daemon.

### Fixed

- **Bootstrap race against gateway handshake.** v0.27.0 launched the
  breadth scheduler from `Server.Start` in parallel with the gateway
  connect goroutine. The cold-start refresh fired before the connector
  was ready, every one of the 500 `FetchDaily` calls returned "no
  gateway connector" instantly, and the fan-out finished in
  milliseconds with zero data. `breadth.Run` now launches from
  `postConnectSetup` behind a `sync.Once` — the bootstrap waits for
  the first successful gateway handshake, and the Once guard means a
  reconnect-driven second `postConnectSetup` is a no-op rather than
  spawning a duplicate scheduler loop.

- **`finalise()` no longer persists a degenerate snapshot.** The
  zero-fetch refresh above wrote `windows.json` with an empty map and
  `snapshot.json` with all 500 names in `Excluded[reason=no_window]`
  and `Coverage=0`. On the next daemon start the scheduler saw "we
  have today's snapshot" and skipped the next bootstrap, so the
  indicator stayed dark for 24 h. `finalise()` now refuses to persist
  any snapshot with `Coverage == 0` — a true S5FI of zero is
  unobserved in market history, so the case is always "no data
  landed," and the engine logs the warning and retries on the next
  tick. Regression pinned by
  `TestEngineRefreshAllFailDoesNotPersist`.

If you ran v0.27.0 against a live daemon (autospawn or
`ibkr daemon`), clear the poisoned cache before upgrading:
`rm -rf $XDG_CACHE_HOME/ibkr/breadth-spx/` (defaults to
`~/.cache/ibkr/breadth-spx/`). v0.27.1 cold-starts cleanly from a
removed cache or from an unaffected v0.26 install.

## v0.27.0 — 2026-05-19 06:19 CEST

`ibkr breadth` now actually returns a number on retail IBKR accounts. The
daemon computes S5FI locally from constituent daily closes instead of
asking the gateway for an index feed it isn't entitled to.

### Added

- **Local S5FI compute engine.** A new `internal/breadth/spx` package
  owns the constituent-fanout-50dma compute: for each of the ~500 S&P
  500 names it keeps a 50-bar sliding window of daily closes, counts
  names whose latest close is at or above their window mean, and
  divides by coverage. Output matches S&P DJI's published `S5FI`
  bit-identically when the membership list and constituent data are
  both current. State persists as three small JSON files (snapshot,
  windows ~250 KB, rolling history ~60 days) under
  `$XDG_CACHE_HOME/ibkr/breadth-spx/`; temp-rename atomic writes keep
  the cache coherent across daemon crashes. Method token on every
  snapshot is `constituent-fanout-50dma`.

- **Daily scheduler with cold-start bootstrap.** The engine runs once
  per US trading day at 16:35 ET — 35 min after the close, enough for
  late prints to settle. On startup it checks whether today's window
  has been missed (daemon down at 16:35, or fresh install) and runs a
  catch-up refresh immediately; otherwise it sleeps until the next
  tick. Refresh is serialised via `refreshMu` so concurrent triggers
  wait behind the first run instead of contending for fetcher slots.
  Per-symbol fetch failures surface as `Excluded` entries on the
  snapshot rather than failing the whole compute.

- **`status: "computing"` for the cold-start breadth row.** Before the
  first refresh lands (~10–15 min on a fresh daemon), `ibkr regime`'s
  breadth row maps to `computing` instead of `unavailable`, with the
  reason "first cold-start refresh in flight (~10–15 min)". Once a
  snapshot is cached the row flips to `ok`; only the rare
  engine-construction-failed path (unresolvable cache dir) keeps the
  old `unavailable` rendering.

- **`make refresh-spx-members`.** New developer target that pulls the
  current S&P-500 list from Wikipedia and rewrites
  `internal/breadth/spx/members_data.go`. The release flow runs this
  automatically on every cut, so each tagged binary carries a current
  list — and a same-tag refresh that would change the file fails the
  release with "commit and re-run" rather than letting the git tag
  and the baked-in list drift apart. Sanity bounds (450–520 names)
  refuse to write if Wikipedia's table structure breaks. Member-list
  parser has its own unit tests against a checked-in fixture.

### Changed

- **`ibkr breadth` / `breadth.spx` RPC source and method tokens.** The
  envelope's `source` is now `"Computed from S&P-500 constituent daily
  bars (IBKR HMDS)"` and `method` is `"constituent-fanout-50dma"`
  (was `"s5fi-direct"`). Wire shape is otherwise unchanged — `value`,
  `as_of`, and `history[]` keep the same semantics. JSON consumers
  that ignore the disclosure strings see no diff.

- **`handleBreadthSPX` is now a thin engine projection.** The handler
  used to subscribe live to the `S5FI` index and fan out historical
  bar fetches inline; it now reads `s.breadth.Get()` /
  `s.breadth.History(n)` and returns. The long-running compute lives
  on the engine's scheduler goroutine, off the request path. Per-call
  budget drops from minutes to microseconds.

- **Engine fetcher reads the live connector through a thunk.** The
  daemon-side adapter (`internal/daemon/breadth_fetcher.go`) doesn't
  capture the connector pointer at construction; it dereferences
  `s.gatewayConnector` on each `FetchDaily`. The engine survives
  gateway disconnects and reconnects without re-instantiation.

- **Breadth-row "unavailable" reason copy.** When the engine has no
  snapshot AND isn't refreshing, the row's reason now reads `"breadth
  engine offline (no cached snapshot)"` — accurate for the only
  remaining trigger (engine construction failed). The old `"S5FI feed
  not entitled on retail IBKR"` copy was made false by the local
  compute.

Cold-start path is the only user-visible delay: a fresh daemon that
has never run the engine sees ~10–15 min of `status: "computing"` on
the breadth row before the first snapshot lands. After that, the row
is instant on every call until the next post-close refresh.

## v0.26.0 — 2026-05-18 18:50 CEST

Pre-market gamma now produces a result instead of timing out; the regime
gamma row ranks the no-crossing case instead of marking it unavailable;
"γ-zero" replaces "flip" in user-facing strings.

### Added

- **BS-IV Newton-Raphson fallback for pre-market gamma.** When the IBKR
  gateway's model-computation engine is idle (typical pre-market — it
  fires only against active option order flow), the Phase 2 compute now
  back-solves implied volatility from each option's prior-session close
  (tick 9, always pushed on subscribe). New `bsImpliedVolatility` /
  `bsCallPrice` / `bsVega` helpers in
  [blackscholes.go](internal/daemon/blackscholes.go); put prices convert
  to equivalent calls via parity, the solver refuses (returns 0) on
  intrinsic violation, sub-1h DTE, or σ outside `[0.01, 5.0]`. Stage 2b
  is factored as `bsIVFallback` so the composition is unit-testable
  without a live gateway — `TestBSIVFallback_AssemblesLegFromSyntheticPrice`
  is the regression pin.

- **`DerivedIVLegs` on the gamma envelope** counts how many legs used
  the fallback. Pre-market this is typically `== LegCount` (engine
  idle); during regular hours it stays at 0. The regime row's
  `Quality.Source` carries `"perfiliev-bs-sweep-v1 · BS-IV from
  prior-session last price"` when every leg used the fallback, and
  `--explain` adds a `"compute used N/N legs with BS-IV from
  prior-session last price"` disclosure line.

- **Regime gamma row ranks the no-crossing case.** When the swept
  profile never crosses zero, the renderer bands on `GammaSign`:
  positive → green (dealer long-γ, stabilizing), negative → red (dealer
  short-γ, amplifying), no_data → unranked. Value cell shows
  `spot 736.91 · short-γ |Γ|·OI 4.1bn`; three regression tests pin the
  three no-crossing branches.

- **`--explain` paragraph for the gamma row.** Plain-English description
  of what γ-zero means and how to read the bands, so a non-quant reader
  doesn't have to leave the terminal for the methodology spec.

- **`gamma-premarket-derived` wire-smoke check.** In loose mode
  (off-hours / frozen), asserts `derived_iv_legs > 0` in the gamma
  envelope. Reads the daemon's JSON response rather than a wire frame
  because the counter is a daemon-internal aggregation. `wire-assert`
  grew `--gamma-envelope-path` and a `checkInputs` struct; cataloged
  alongside the existing six in `--list`. Strict mode skips it
  automatically.

### Changed

- **"Flip" → "γ-zero" in user-facing strings.** Renderer row labels,
  reasons, the `ibkr gamma` command output, and `rpc.GammaZeroComputed`
  field docs. "Flip" survives only in code identifiers and `--explain`
  prose where the term is contextually defined.

- **`qualityTag` suppresses age on modelled rows.** A model output 37s
  old is no more stale than one 1s old — the inputs were captured at the
  same compute. The row shows `· modelled` until the cached model
  crosses 5 minutes, then `· modelled 6m old`. Live-tick rows keep the
  existing seconds-old age suffix.

### Fixed

- **Pre-market "no option ticks landed" abort.** The 30s early-abort
  error message now blames both failed paths (model ticks and BS-IV
  fallback) and points at gateway entitlement / farm-connection notices
  in the daemon log, since the BS-IV fallback recovers the routine
  pre-market case.



The gamma compute now works end-to-end during US regular trading hours
and the regime dashboard tells you how trustworthy each value is. Plus
a wire-level smoke gate that would have caught the gamma bug the first
time it was tested.

### Fixed

- **`ibkr gamma` actually returns a result during regular hours.** The
  Phase 2 zero-gamma compute had two latent bugs that combined to make
  every leg time out:
  1. `productionLegFetcher` polled `MarketData.IV` (only fed by IBKR
     generic tick 106, which the gateway does NOT deliver for OPT
     subscriptions) instead of `GetOptionIV(key)` (fed by the
     OPTION_COMPUTATION model tick, msg 21 — the canonical per-strike
     IV source). Verified at the wire level: model ticks arrive with
     non-NaN IV; the consumer was reading the wrong field.
  2. The IBKR gateway delivers OI ticks (27 / 28) under
     `MarketDataType=1` (live) but model ticks (msg 21) under
     `MarketDataType=2` (frozen-aware) — never both in the same mode.
     The gamma fan-out now temporarily switches to type=1 and restores
     to type=2 on return, so each leg sees both ticks. Verified
     end-to-end: cold compute completes in ~1 min during regular
     hours with real leg coverage.

  Pre-market is still degraded — IBKR's model engine doesn't fire when
  options aren't trading. The compute now surfaces a clear pre-market
  error within 30 s instead of hanging behind a misleading "computing"
  status. A BS-IV-from-last-trade fallback for pre-market is a
  documented follow-up.

### Added

- **Per-scalar provenance on every regime indicator.** A new
  `rpc.Quality` envelope (`AsOf`, `FreshnessClass`, `Confidence`,
  `Source`) hangs off each scalar field on each row. Daemon fetchers
  populate it at the assignment site so the renderer can show:

  ```
  ●  VIX/VIX3M     0.870  (18.40 / 21.14)  green   (<0.92 contango)
  ●  HYG vs SPY    HYG 79.67 / 50dma 79.87 yellow  (HYG < 50dma …)   · est 9s
  ●  USD/JPY       158.7300  +1.15%/wk     green   (<1% weekly)      · est 9s
  ```

  Firm-live rows stay unannotated (no clutter); derived rows declare
  themselves as estimates; modelled rows (gamma) declare themselves
  as modelled. `--explain` extends with per-scalar provenance blocks
  so any single number can be audited:

  ```
  HYG vs SPY
    HYG             firm live · age 5s · HYG tick (ARCA)
    HYG_50DMA       estimate derived · age 5s · HYG 50-bar SMA
    SPY             firm live · age 5s · SPY tick
    SPY_52w_high    firm live · age 5s · SPY tick 165 (Misc Stats)
  ```

  The wire shape is additive — every new `*Quality` is `omitempty`,
  existing `Status` / `DataType` / `FieldsMissing` keep their meaning.
  JSON consumers that ignore unknown fields are unaffected.

- **Wire-level smoke gate (`make smoke`).** A new lifecycle script
  (`scripts/wire-smoke.sh`) spawns an isolated daemon with
  `IBKR_WIRE_INTERCEPTOR=1`, runs a fixed read-only command sequence
  against a live gateway, and asserts per-command protocol-level
  invariants via `bin/wire-assert`. Hooked into `make release` after
  `release-verify` and SKIPs cleanly when no gateway is reachable so
  `make release` still works on a laptop without paper-account IBKR.
  Six v1 invariants: `status-handshake`, `quote-spy`, `account-summary`,
  `chain-iv-source`, `regime-subs`, `gamma-noflag`. The
  `chain-iv-source` check is exactly the assertion that would have
  caught the v0.24.x IV-source bug on the first test run.

- **Early-abort watchdog in the gamma compute.** When zero legs land
  in the first 30 s of the fan-out the compute aborts with a
  pre-market-aware error message instead of grinding for minutes.
  Distinguishes "the gateway isn't pushing the ticks we need" from
  "the gateway is throttling contract resolution".

- **Regression test pinning the IV-data-model invariant.**
  `TestModelTickPopulatesOptionIVNotSubscriptionIV` documents that
  model ticks land in `optIV[OPRA_key]` (readable via `GetOptionIV`)
  and NOT in `subscriptions[…].IV`, so future refactors can't
  silently re-introduce the v0.24.x IV-source bug.

### Changed

- **Misleading "re-run later for the cached result" copy removed**
  from `ibkr regime` and `ibkr gamma`. Replaced with honest framing:
  the compute runs once per NY session (typically 2-4 min on a warm
  contract cache), subsequent calls within the day return cached, and
  error states tell you the cache will retry after 60 s.

- **Corrected misleading comment in `SubscribeOption` about generic
  tick 106.** The previous comment claimed requesting tick 106 closed
  a gap when model-computation ticks weren't dispatched off-hours.
  Field experience and wire traces show tick 106 is not reliably
  delivered for OPT contracts; the canonical per-strike IV source is
  the OPTION_COMPUTATION model tick (msg 21) routed via
  `GetOptionIV`. Comment now names the source-of-truth correctly.

## v0.24.0 — 2026-05-18 12:33 CEST

The regime dashboard's robustness pass. Three indicators that had been
intermittently failing off-hours (VIX/VIX3M, HYG/SPY, USD/JPY) now
rank reliably from the first `ibkr regime` call after a cold daemon
start. Indicator 4 (gamma) moved from SPX to SPY so it can land
during extended hours when SPX option IV ticks aren't flowing. The
wire shape of the gamma envelope changed (`spot_spx` →
`spot_underlying`) — that's the visible reason for the minor-version
bump.

### Changed

- **Indicator 4 (gamma) now computes against SPY instead of SPX.**
  SPY trades extended hours on SMART/ARCA with continuous market-maker
  quotes and a single option trading class; SPX has no spot trading
  outside RTH and IBKR's model-computation engine doesn't push IV
  ticks for SPX options pre-market. The previous SPX compute
  consistently failed to land a single leg off-hours. The regime
  signal is unchanged — SPY dealer gamma tracks SPX dealer gamma
  closely (same dealer-positioning regime); only the absolute level
  is SPY-scale (~SPX/10). Renderer label is now `SPY γ-zero`. Spec
  doc and MCP tool description updated to match.

- **`GammaZeroComputed.SpotSPX` renamed to `SpotUnderlying`** with
  JSON tag `spot_underlying` (was `spot_spx`). Consumers parsing the
  gamma envelope must re-map.

- **Gamma compute iterates strikes ATM-outward, not low-to-high.**
  `secDefOptParams` returns a deduped strike SUPERSET across
  exchanges, so the chain contains candidates that aren't listed on
  every expiry — especially far-OTM strikes that exist only for
  select events. Processing nearest-ATM first means the compute hits
  liquid, listed strikes quickly and accumulates legs while the
  worker pool drains the long tail of dead candidates. Also avoids a
  worst case where the first 50 attempts are all far-OTM failures
  and the 5 % throttle threshold aborts before reaching ATM.

### Added

- **`Connector.SeedContractDetails(symbol, detail)`** — public API
  to pre-seed the in-memory contract cache. Refuses to overwrite an
  entry that already has a non-zero ConID, so a live gateway
  response always wins. The daemon's regime prewarm uses this with a
  static map of stable spot conIDs (VIX, VIX3M, HYG, SPY, USD.JPY,
  SPX) so the regime row's historical fetches keep working even when
  the gateway's `reqContractDetails` is silent — observed for hours
  on end against an account where only `secdefeu` connects.

- **Tick 37 (mark price) captured on every market data subscription**
  via the new `Subscription.MarkPrice` / `MarketData.MarkPrice`
  fields. Index symbols (VIX, VIX3M, SPX) don't emit bid/ask/last —
  only mark — and previously the brief-snapshot helpers returned 0
  for them, which made VIX/VIX3M unrankable off-hours.

### Fixed

- **`ibkr regime`'s VIX/VIX3M row ranks reliably off-hours.**
  `briefSnapshotPrice` now falls back through last → mid → bid → ask
  → mark → close. Mark (tick 37) is the index path; close (tick 9)
  is the last-resort anchor for thin CBOE indices that emit only
  yesterday's regular-session close pre-open. The row's `data_type`
  honestly reports "frozen" when the value came from the close, so
  the renderer dims the row instead of pretending it's live.

- **Gamma compute's "no security definition" no longer triggers
  spurious throttle abort.** The signal was conflated with real
  gateway throttling; with the abort firing in the first 50 attempts
  before any near-ATM strike was reached, the compute consistently
  errored before producing a result. The fetcher now distinguishes
  "contract details unavailable" (skip this leg) from
  `ErrContractDetailsTimeout` (genuine throttle).

- **Gamma per-leg budget lifted from 5 s to 15 s.** OI ticks (27/28)
  arrive quickly but IV (tick 21, model-computation) can be much
  slower for OTM legs pre-market when the gateway's
  model-computation queue isn't running hot. The previous 5 s
  routinely dropped legs that had OI without IV; 15 s captures the
  full pair without unbounded waits.

- **`contractDetailsLateGrace` raised from 3 s to 30 s** so late
  ContractData frames from slow gateways (observed at 15-45 s
  post-request against cold-after-TWS-restart accounts) still
  populate the cache. Subsequent `prepareContract` /
  `ensureContractDetails` calls then hit a warm entry instead of
  re-running the slow round-trip.

### Honest limitations

This release does NOT solve the case where IBKR's gateway isn't
streaming option market data at all (observed pre-market against
some account states: SPY option subscriptions resolve cleanly but
OI/IV ticks never arrive). The gamma compute is still bounded by
whatever the gateway is willing to push; on a healthy market-hours
gateway with extended-hours liquidity, it lands. On a wedged
gateway, it doesn't. Restart TWS or wait for US market open.

## v0.23.3 — 2026-05-18 07:30 CEST

A small follow-up to v0.23.2 covering the regime dashboard's VIX term
structure row. The VIX3M leg was intermittently dropping to null on
cold off-hours calls — reproduced twice in succession against IB
Gateway 10.37 in frozen mode — and the surfaced error message pointed
readers at a classifier bug that doesn't exist. Both behaviours fixed
in `fetchRegimeVIXTerm`.

Scope honesty: this release does NOT touch the gamma_zero row's
`fetch SPX expiries: timeout waiting for contract details` failure
surfaced by the same investigation. That one is gateway-side — only
`secdefeu` connects on the affected account, and SPX index contract
definitions live in `secdefus`. A daemon-side fix isn't possible until
the sec-def farm gap is resolved.

### Fixed

- **VIX3M snapshot budget lifted from 5 s to 8 s.** VIX3M is a much
  thinner CBOE index than the VIX itself; outside RTH the gateway
  pushes its snapshot tick later than the VIX leg, and 5 s reliably
  lost the tick on cold off-hours calls even with a warm contract
  cache (ConID 47511905 resolved on every call; the tick just didn't
  arrive in budget). 8 s matches the SPY 52w-high budget which sees
  the same pattern. Doesn't make the gateway any faster — bounds the
  intermittent miss rate.

- **VIX3M error message no longer claims a classifier bug that
  doesn't exist.** The pre-fix string `(classifySymbol entry may be
  missing)` was historical guidance from when the entry truly was
  missing; the VIX3M routing to `IND/CBOE/USD/CBOE` has shipped since
  the original regime indicator landed, and the daemon log confirms
  ConID resolution on every call. Replaced with a description of the
  actual failure mode (`no spot tick within budget (thin CBOE index,
  common off-hours)`) so readers aren't pointed at a non-existent
  bug.

## v0.23.2 — 2026-05-17 21:28 CEST

Two off-hours bugs in the regime dashboard's HYG/SPY divergence row
fixed in one release. Before v0.23.2 the row was silently dropping to
a 2-state signal every time the market was closed: `spy_52w_high` came
back null because the underlying tick stream doesn't exist in frozen
mode, and `hyg_50dma` came back null because HYG's contract was never
resolving. Both fields now land on the first regime call after a cold
daemon restart, with NYSE closed.

### Fixed

- **SPY 52-week high now lands on off-hours regime calls.** The
  HYG/SPY divergence row needs SPY's 52w high to evaluate the
  yellow-band trigger ("HYG breaks 50DMA while SPY within 3% of 52w
  high"). It was sourced from generic tick 165 (Misc Stats) in
  `briefSnapshotPriceWith52WHigh`, which is part of IBKR's streaming
  tick set. In `MarketDataType=2` (frozen — every non-RTH call) the
  gateway sends one static snapshot of bid/ask/last and then goes
  silent; tick 165 never arrives, no matter the budget. The row came
  back with `spy_52w_high: null` on every weekend / overnight /
  holiday regime call, dropping the indicator's signal from 3-state
  to 2-state for most of when the dashboard actually gets read.
  `fetchRegimeHYGSPY` now falls back to `max(High)` over the most
  recent ~252 daily bars (one trading year) from the same HMDS path
  the indicator already uses for HYG's 50DMA. The live tick is still
  primary — the fallback fires only when the gateway returned zero,
  so RTH calls get the gateway's real-time intraday-accurate value
  and never the slightly-stale derivation.

- **HYG 50-day SMA now lands on cold-start regime calls.** HYG was
  the only regime indicator symbol missing from the ETF case block in
  `classifySymbol`, so it went out to the IBKR gateway with
  `primary=""`. Without a fast-lookup hint the gateway did a wider
  security-master search that routinely overruns the regime fetcher's
  2 s `prepareContract` budget (observed empirically at ~21 s; the
  prewarm budget is 30 s precisely for this reason). HYG's history
  fetch aborted with "contract details unresolved", and `hyg_50dma`
  surfaced as null on every cold-start call. `classifySymbol` now
  routes HYG through ARCA alongside SPY, QQQ, IWM, DIA, GLD, TLT —
  HYG is on ArcaEdge like the others. The "reqContractData anomaly
  symbol=HYG primary=''" warning at `connection.go:3268` (which is a
  diagnostic gate, not a separate bug) no longer fires for HYG
  either. Investigation confirmed HYG was the only affected regime
  symbol; no broader audit needed.

## v0.23.1 — 2026-05-17 20:56 CEST

A one-fix follow-up to v0.23.0. The prewarm shipped in v0.23.0 wasn't
actually warming the cache it was meant to warm; this release closes
that gap so cold-start regime calls really do benefit from it.

### Fixed

- **Cold-start regime calls now actually benefit from v0.23.0's
  prewarm.** `prewarmRegimeSymbols` fires `FetchContractDetails` for
  each indicator right after the daemon connects, expecting to prime
  `contractCache`. But `FetchContractDetails` only wrote to the cache
  on its timeout path — on the success path it returned the contract
  and discarded it. The prewarm goroutine also discards its return
  value, so the cache stayed cold for any symbol whose response
  arrived within budget. The next regime call then re-issued the same
  request with a 2 s `prepareContract` budget that times out for HYG
  (no primary exchange in `classifySymbol`), aborting HYG's history
  fetch with "contract details unresolved" and sending SPY's
  market-data subscription out with ConID 0 on every first call after
  a daemon restart. `FetchContractDetails` now populates the cache
  from the success branch too, guarded the same non-clobbering way
  `deferContractDetailsCleanup` already does. Verified: first-call
  `hyg_50dma` and the SPY 52w high land without "(pending)"
  daemon-log anomalies after a cold restart.

## v0.23.0 — 2026-05-17 20:14 CEST

v0.22.0 shipped the regime dashboard's redesigned UX; v0.23.0 makes
the data underneath honest. A senior-dev review of `ibkr regime`
found that on cold-cache calls only one of five indicators came back
fully populated, the daemon silently swallowed history-fetch errors,
the gamma compute's first failure of the day stuck for the rest of
the NY session, SPY's 52-week high never actually arrived, and the
composite verdict happily printed bold-green "Normal regime" with
1 of 5 indicators ranked. None of those are catastrophic — they're
the difference between "looks like it works" and "real, reliable,
robust." This release is the gap.

### Fixed

- **History-fetch errors are no longer silently swallowed.** The
  daemon now logs at warn level when `fetchRegimeHYGSPY` or
  `fetchRegimeUSDJPY` lose their `FetchHistoricalDailyBars` call, and
  when the returned bar slice is too thin for the SMA / lookback the
  fetcher wanted. A null `hyg_50dma` or `weekly_change_pct` in the
  envelope used to tell the consumer *that* a field was missing
  without telling them *why* — the daemon log now answers, with the
  specific error or the bar-count shortfall named.

- **Gamma cache no longer poisons the rest of the NY session on a
  single transient error.** A one-shot gateway-side timeout (e.g. the
  cold-start "fetch SPX expiries: timeout waiting for contract
  details" race) used to stick in `c.current` until midnight NY
  rolled the session key. The cache now treats a cached error older
  than 60 s as stale-on-error and falls through to a fresh kick.
  In-flight jobs and successful results stay sticky regardless of
  age — the retry path only fires on the (errored, isDone, > 60 s old)
  shape so a flapping gateway doesn't get retry-stormed.

- **SPY 52-week high actually lands now.** The HYG/SPY indicator
  needed SPY's 52-week high to evaluate the spec's yellow-band
  trigger ("HYG breaks 50dma while SPY near highs"), but the original
  `briefSnapshotPrice` returned on the first price tick and
  unsubscribed before the gateway could deliver tick 165 (Misc
  Stats / week-range highs and lows). A new
  `briefSnapshotPriceWith52WHigh` keeps the subscription open until
  both the price triple and `Week52High` have landed, or 8 s
  expires — partial results stay honest if a field doesn't arrive.

- **Cold-start contract-resolution race partially eliminated via
  pre-warm.** A new `prewarmRegimeSymbols` goroutine fires off
  `FetchContractDetails` for all five regime indicators right after
  daemon connect, on a 30 s per-symbol budget. Empirically this
  resolves the VIX, VIX3M, and USD.JPY first-call gap — `USD.JPY`'s
  `weekly_change_pct` now lands on the very first regime call after
  daemon restart. HYG and SPY may still race on cold start because
  their contract-details responses arrive marked "pending" for
  STK-with-no-PrimaryExch; deeper fix is queued as separate work.

- **Lookback buffers widened so a single US holiday doesn't drop a
  row.** HYG 50DMA pulls 90 calendar days now (was 70) — 50 trading
  days is closer to 71 calendar days with zero holidays in the
  window, and one Memorial Day / Labor Day / Thanksgiving in the
  trailing window short-counts the SMA's bars. USD/JPY 7-trading-day
  lookback pulls 14 calendar days (was 12) so a Monday/Friday bank
  holiday doesn't drop a bar.

### Changed

- **Composite verdict surfaces "Insufficient signal" when fewer than
  3 indicators ranked.** Previously the verdict block printed bold
  "Normal regime" whenever fewer than 3 reds and 3 yellows had been
  seen — including the v0.22.0 weekend behaviour where 4 of 5 rows
  returned unranked (computing / unavailable / spec-field missing)
  and the verdict was effectively one VIX/VIX3M tick. The dim count
  summary below was honest about coverage; the bold line above it
  wasn't. Three was chosen because the spec's yellow- and red-band
  rules both reference "majority of indicators" — a verdict needs at
  least majority coverage before it stops being a guess.

- **`ibkr regime --watch` default `--rate` is 5 minutes (was 60 s).**
  The spec calls for daily refresh; the indicators move on minute-to-
  hour scales. 60 s re-polled the daemon ~60× more than the data
  warranted, padding daemon log noise and gateway slot pressure for
  no payoff. Users who want tighter cadence can override with
  `--rate=30s`.

### Added

- **TTY spinner during the regime fetch.** A single-call
  `bin/ibkr regime` sat silent for 10-20 s while the daemon's
  five-way fan-out completed — the slowest fetcher bounds the wall
  clock and stdout stayed blank the whole time. When stdout is a TTY
  and the call isn't `--json`, a one-line braille spinner names the
  indicators in flight and clears before the dashboard's first line
  lands. Non-TTY callers (pipes, file redirects) and `--json` keep
  their atomic-stdout shape unchanged; the existing buffer-based
  tests and renderer contract are not affected.

## v0.22.0 — 2026-05-17 12:38 CEST

`ibkr regime` had shipped with a deliberate-placeholder renderer — five
indicator rows dumped one after another, each followed by a paragraph
of dim spec prose, the composite verdict that the spec demands at the
top nowhere to be seen. This release replaces it with the dashboard the
spec was actually asking for: one bold verdict line, a count summary
that's honest about which indicators were excluded from the count, and
one compressed row per indicator with a colored band glyph and the
threshold band in the dim parenthetical. The full spec prose moves
behind `--explain` so it's still one keystroke away when you want it.

The wire shape and JSON output are unchanged. Threshold derivation now
lives in the renderer with the spec defaults; the daemon still emits
raw measurements only.

### Changed

- **`ibkr regime` text output is the dashboard the spec describes.**
  The old screen took ~30 lines per call and dedicated ~3 KB of dim
  prose to per-row methodology notes. The new screen takes ~12 lines:
  a bold `Normal regime` / `Watch closely` / `Regime shift likely` /
  `Full risk-off conditions` headline taken verbatim from the spec's
  interpretation table, a dim `2 green · 1 yellow · 0 red  ·  3 of 5
  ranked · 2 unranked` summary, then one row per indicator with a
  colored badge glyph (filled circle for ranked rows, distinct glyphs
  for `computing` and `unavailable`), the value cell, the colored band
  word (green / yellow / red), and the threshold band as a dim
  parenthetical. JSON output is unchanged.

- **Composite count excludes `computing` and `unavailable` rows.** The
  count summary names ranked and unranked separately so a reader sees
  that some indicators were excluded from the verdict rather than
  assuming it was computed over all five. Empirically this matters: on
  retail IBKR accounts indicator 5 (breadth) is structurally
  unavailable, and on the first regime call of the NY trading day
  indicator 4 (gamma) is `computing`. The previous renderer offered no
  visible signal that those rows weren't contributing.

- **`--watch` flag, sibling parity with `account` / `positions` /
  `quote`.** Re-polls on a fixed interval with in-place TTY redraw;
  default rate is 60 s because regime data updates slowly and a tighter
  rate just burns gateway round-trips. Mutually exclusive with `--json`.

- **`--explain` flag for the spec prose.** Default output shows the
  threshold band as a compact parenthetical (e.g. `(<0.92 contango)`).
  Pass `--explain` to print the full spec language under each row, the
  same prose that previously bloated every default render. The JSON
  surface still carries the prose verbatim on the `notes` field for
  programmatic consumers.

- **Threshold derivation moved into the renderer.** Spec defaults are
  baked as renderer constants (`vixRatioGreen = 0.92` etc.). Daemon
  still emits raw measurements only — the spec calls the bands
  user-tunable, which means defaults in the renderer rather than
  policy in the daemon. Configurability is deferred until asked for.

- **HYG vs SPY row stays unranked when `spy_52w_high` is missing**
  rather than guessing. The spec's yellow band requires SPY within 3 %
  of its 52-week high; without that data the renderer can't band
  honestly, so the row counts as unranked and the reason names the
  missing field. This was a HIGH-finding correctness consideration
  during the taste review: don't guess what we can't compute.

### Removed

- **The `Spec doc:` line on the default render.** The path served no
  human navigation purpose. The JSON envelope still carries `spec_doc`
  for programmatic consumers — that surface is unchanged.

- **The `(missing: ...)` advisory line under each row.** The same
  information is now communicated by the band classification itself
  (an unranked row whose reason names the missing field). One source
  of truth instead of two.

### Added

- **`PreviewRenderRegime` + a `regime` fixture in `cmd/_preview`.** The
  flagship regime command had shipped without entering the screenshot
  preview pipeline. `go run cmd/_preview/main.go regime` now renders a
  realistic mid-session shape: VIX OK + live, HYG stale + yellow, USD/JPY
  OK + weekly change, gamma computing + ETA, breadth structurally
  unavailable. Every render branch lit up in one screen.

- **Renderer test coverage for `regime`.** Eight tests pin the new shape
  (composite verdict, one-line-per-row layout, band words, gamma ETA
  inline, breadth reason inline, no spec-doc line, `--explain` mode,
  default-omits-prose). Eleven more pin the threshold-derivation logic
  (full composite verdict table, VIX bands, USD/JPY yen-strengthening
  asymmetry, HYG/SPY honesty-floor when 52w-high is missing).

## v0.21.0 — 2026-05-17 12:54 EEST

Wraps the risk-regime dashboard work into a single user-facing
command. Where v0.20.0 added two indicator endpoints (gamma + breadth)
in isolation, v0.21.0 makes them readable from a Claude Desktop
conversation in plain English, alongside the three indicators that
already worked, by adding one aggregator + the FX routing required
for indicator 3.

### Added

- **`ibkr regime` / `ibkr_regime` / `regime.snapshot`** — risk-regime
  snapshot in a single call. Returns all five dashboard indicators
  (VIX/VIX3M ratio, HYG vs SPY divergence, USD/JPY, SPX zero-gamma,
  SPX breadth) in one JSON envelope. Each row carries raw
  measurements plus a `notes` field embedding the spec's threshold
  bands verbatim, so an LLM consumer can interpret the response
  without reading the methodology doc. The envelope also carries a
  `spec_doc` pointer for deep-linking. The pitch is "state of all
  five in one tool call" rather than "all five populated": gamma's
  first call of an NY trading day returns `status: "computing"` while
  the background compute runs, and breadth surfaces as `unavailable`
  on retail IBKR because the S5FI feed isn't entitled. The notes
  field on each row explains its disposition so an LLM consumer can
  reason without consulting the methodology doc.

- **`fields_missing` advisory slice** on the HYG/SPY and USD/JPY
  regime rows. Optional sub-fields (`hyg_50dma`, `spy_52w_high`,
  `close_7d_ago`, `weekly_change_pct`) can fail to land independently
  of the row's primary spot. Rather than degrade the whole row to
  `error` on a thin history fetch, the row stays `ok` and the slice
  names what's missing — consumers (LLM or otherwise) can hedge the
  read instead of guessing whether the daemon asked.

  Indicator 4 (gamma) is auto-kicked on the first regime call of an
  NY trading day; subsequent calls return the cached result. The
  daemon never derives green/yellow/red status colours from raw
  values — the spec explicitly calls those bands user-tunable, so
  threshold derivation stays in the renderer.

- **Native CASH/IDEALPRO FX-pair routing**. `pkg/ibkr/classifySymbol`
  now accepts G10 FX pairs in both dotted (`USD.JPY` — canonical) and
  slash (`USD/JPY`) forms; the connector lifts the base currency onto
  `Contract.Symbol` and routes through IDEALPRO with the quote
  currency on `Contract.Currency`. Works through the existing `ibkr
  quote` + `ibkr history` paths — no new RPC method, no new CLI
  command, no new MCP tool. Lets regime's USD/JPY row read live FX
  data instead of a stock-proxy substitute.

  G10 majors only: USD/EUR/JPY/GBP/CHF/AUD/NZD/CAD. The allowlist
  protects stock tickers with dots (BRK.B, RDS.A) from being
  misclassified.

- **VIX3M** in `classifySymbol` (IND/CBOE) so the regime aggregator's
  VIX-term ratio computes without a "no security definition" fallback.

### Changed

- **`pkg/ibkr.FxPair(symbol)` is exported.** Daemon-side callers (the
  echoed `Contract` rewrite in `handleQuoteSnapshot`) use the same
  parser as the classifier so the dotted/slash rule stays in one
  place.

- **`defaultHistoricalWhat("CASH") == "MIDPOINT"`** and
  `historicalWhatSequence` skips the TRADES fallback for CASH
  instruments. FX has no consolidated trade tape; the TRADES retry
  would burn the deadline and return error 162.

- **`gamma.zero_spx` `eta_seconds` is progress-derived** once the
  compute has made meaningful progress (>5% complete). Projects from
  elapsed × remaining-progress so the countdown stays honest when a
  run overruns the static 360s estimate. Floored at 5s; capped at 4×
  the initial estimate so a stalled leg can't return absurd
  projections.

### Fixed

- **Regime aggregator code hygiene** — removed a duplicate
  `isLiveDataTypeWire` helper that shadowed `rpc.IsLiveDataType`, and
  a double `AsOf = time.Now()` write that pre-stamped the envelope
  before the fan-out. No behaviour change; the code is shorter and
  the wall-clock anchor is unambiguous.



Two new read-only endpoints feed the risk-regime dashboard spec
(`docs/specs/risk-regime-dashboard.md`) directly from IBKR data,
replacing what were previously manual-entry indicators. Both default
to "what the spec asks for" and degrade honestly when the gateway's
feed state isn't live.

### Added

- **`ibkr breadth` / `ibkr_breadth` / `breadth.spx`** — current S&P 500
  stocks-above-50-DMA reading plus ~30 trailing daily values for
  sparkline rendering. Sourced from S&P DJI's `S5FI` index via IBKR's
  `INDEX` exchange — no constituent fan-out, no daemon-side SMA
  recomputation. Threshold derivation lives in the renderer (the spec
  itself calls those bands user-tunable). End-to-end honest about the
  gateway's `data_type`; falls back to PrevClose when no Last is
  delivered within budget.

- **`ibkr gamma` / `ibkr_gamma` / `gamma.zero_spx`** — dealer
  zero-gamma estimate for SPX. The compute is heavy: fan-out across
  hundreds of SPX + SPXW option legs at the documented 4-concurrent
  gateway throttle, multi-minute wall clock. The endpoint is
  background-and-poll: the first caller of an NY trading session kicks
  the job and gets `status: "computing"` with an ETA hint; subsequent
  callers within the same session receive `status: "ready"` with the
  cached payload. Singleflight prevents duplicate fan-outs against the
  same gateway slot pool.

  Returns **two complementary signals** so the dashboard can be honest
  about regime hints versus precise levels:
  - Signed `zero_gamma` + 60-point `profile` under the Perfiliev
    convention (calls long, puts short). Documented as a regime hint
    that can invert near covered-call ETF flow or autocall barriers.
  - Sign-agnostic `gamma_total_abs` + `top_strikes` magnitude view.
    Robust to the dealer-positioning assumption; what to read when
    the signed sign is suspicious.

  Methodology token `perfiliev-bs-sweep-v1`. Full disclosure
  (limitations, calibration ritual, deferred backlog) in
  `docs/specs/risk-regime-dashboard.md`.

- **Risk Regime Dashboard build spec** committed at
  `docs/specs/risk-regime-dashboard.md`, including a "Daemon
  methodology" section that names every assumption the two new
  endpoints make. Authors of dashboard renderers should read it
  before trusting the numbers.

### Changed

- **`pkg/ibkr.MarketData.OpenInt` is now populated.** The field has
  existed since the type was introduced but no parser wrote to it:
  the gateway was delivering option open-interest ticks (27 / 28) on
  every leg subscription and the connector silently dropped them.
  `handleTickSize` now writes both call-OI and put-OI into the field
  (one leg subscription receives at most one of the two — they don't
  race). Wire-fixture tests pin both paths through to `GetMarketData`.

- **`pkg/ibkr.classifySymbol` knows `S5FI`** (`IND` / `INDEX` /
  `USD`), with a matching entry in `contractDisplayHints` for stable
  routing. Sibling breadth indices (S5TW/S5OH/S5TH for 20/100/200-day
  MAs) are intentionally not pre-registered — the spec only asks for
  50-day.



### Fixed

- **`TestStreamingMCPResourceSubscribe` no longer flakes against a live
  gateway.** The test fed `srv.Serve` a `bytes.Buffer` that EOFed the
  instant the four request lines drained. `Serve`'s deferred
  `shutdownSubscriptions()` then cancelled the just-spawned subscription
  goroutine before the daemon's 150 ms tick loop had fanned out the
  first frame, so no `notifications/resources/updated` ever reached the
  output buffer. Wrapped the input with a `stayOpenReader` that blocks
  on EOF until the test's context is cancelled — mirrors how a real MCP
  client (Claude Desktop, Cursor, …) keeps stdin open for its lifetime.
  Test now passes in ~4.2s, deterministic across repeated runs. No
  production behaviour change; the bug was in the test harness, not the
  MCP server.

- **`Makefile`: gofmt step filters staged-for-deletion paths.** `git
  ls-files --cached` includes files removed from the working tree but
  still in the index, so `gofmt -l` emitted `lstat …: no such file or
  directory` warnings during the brief window between `git rm` and
  `git commit`. Filtered the input list through a per-file existence
  check; `make check` is now clean even mid-deletion.

## v0.19.0 — 2026-05-15 08:50 CEST

Taste-review pass: three HIGH findings from a senior-review of the
whole repository, applied. The release subtracts ~700 LOC of speculative
multi-client scaffolding, removes a leaky encode/decode boundary
between the library and the daemon, and replaces a silent-warn pattern
in the rate limiter with a fail-loud guard. No CLI/MCP behaviour
change; `pkg/ibkr` library surface narrows.

### Changed

- **`pkg/ibkr.Connector` owns a single `*Connection`.** Multi-client
  pool scaffolding (`ConnectionPool`, `ConnectionLease`, leases,
  heartbeats, `findAvailableConnection`, `monitorLeases`,
  `maintainLease`) is retired. Production was always running with
  `ClientIDs=[1]` since v0.10.2, and the lease/heartbeat machinery was
  never exercised — the daemon couldn't even route a
  `RequestHistoricalData` to a different connection from a
  `SubscribeMarketData`. Collapse to a one-connection-per-Connector
  design removes ~500 LOC from `pool.go` plus ~50 LOC from
  `connector.go`. Reconnection on loss stays the daemon's job (via
  `s.triggerReconnect`); the connector reports honest connectivity
  rather than masking it with retry state.

- **`Connector.GetCachedPositions()` returns `[]*RawPosition`** instead
  of the synthetic `[]*Position`. The daemon's `handlePositionsList`
  reads typed `Contract.{Symbol, SecType, Expiry, Right, Strike}`
  fields directly — no more `fmt.Sprintf("%s_%s_%s%.0f", …)` encode
  followed by `strings.Split` + `fmt.Sscanf` decode in adjacent layers.
  One source of truth, no round-trip cost.

- **`Semaphore.Release` is fail-loud.** The previous "swallow with
  Warnf" branch hid mismatched Acquire/Release pairs in the market-data
  slot tracking. The Release path now panics on empty-semaphore release
  (a real bug at the caller), and the Connection layer routes every
  Acquire/Release through reqID-aware helpers
  (`acquireMarketDataSlot(ctx, reqID)` /
  `releaseMarketDataSlot(reqID)`) so the error handlers for codes
  200/354 and `CancelMarketData` are idempotent against a single
  subscription's lifecycle. Subtracts the v0.10.2-era
  "(shouldn't happen in normal use)" Warnf branch in favour of CLAUDE.md
  "Fix root causes, not symptoms."

- **`ConnectorConfig`** now exposes `BaseConfig *ConnectionConfig`
  directly (was nested as `PoolConfig.BaseConfig`). Library consumers
  building a `*Connector` by hand need to flatten:
  `ConnectorConfig{BaseConfig: cfg, PreferredClientID: 15}`.

### Removed

- **`pkg/ibkr` types: `Position`, `Asset`, `AssetType`,
  `AssetTypeStock`/`AssetTypeOption`/`AssetTypeFuture`/`AssetTypeIndex`,
  `Order.Asset`** field, `ConnectionPool`, `ConnectionLease`,
  `PoolConfig`, `DefaultPoolConfig`, `NewConnectionPool`,
  `Connector.GetPoolStatus`, `Connector.maintainLease`,
  `Connector.reconnect` (lease-driven). Downstream library callers
  that imported the dropped types should switch to `RawPosition.Contract.*`
  for the position shape and to `ConnectorConfig.BaseConfig` for the
  pool-config flattening.

- **`pkg/ibkr/connector_positions_test.go`**: the no-synthesis property
  it asserted is now structural (the handler reads `pos.UnrealizedPNL`
  directly with no synthesis layer in between).

- **`pkg/ibkr/pool.go` + `pool_test.go`**: ~700 LOC gone with the pool
  collapse.

### Fixed

- **`Connection.sendMessage` / `sendMessageWithType`** now return an
  error rather than panic when called with a nil writer. The state is
  inconsistent (claims connected but transport not yet attached) and
  surfacing it as an error lets the rate limiter unwind cleanly instead
  of taking the process down.

### Internal

- The taste review's MED items (MarketData zero-write fields, the order
  tracking around `SubmitOrder`, `handleTickPrice` triple-switch, the
  19-mutex `Connection` struct, `RequestMarketData` /
  `RequestMarketDataWithPrimary` duplication, `briefSnapshot*`
  template-coding, the README server-version doc-drift,
  `TestConnection_EventDrivenVsSleep`) are deferred to follow-up
  passes — listed here so future-me doesn't relitigate the same
  ground.

## v0.18.2 — 2026-05-14 22:43 CEST

### Fixed

- **README hero image not rendering inline on github.com.** The PNGs
  added in v0.18.1 carried a Display P3 colour profile (sips default
  on retina macOS captures); GitHub's blob viewer happily decoded
  them when clicked, but inline rendering on the rendered README
  page left a blank space. Re-encoded `docs/social/positions.png`
  and `docs/social/social-preview.png` with the sRGB IEC61966-2.1
  profile so they display inline. No content change to either image.

## v0.18.1 — 2026-05-14 22:29 CEST

Polish pass on v0.18.0: matches the two `--watch` defaults, mentions
`--watch` in the skill + CLI help where v0.18.0 forgot to, and adds a
hero screenshot to the README.

### Added

- **README hero image.** `docs/social/positions.png` shows
  `ibkr positions --by underlying` with per-leg Greeks and the
  portfolio rollup, replacing the inline ASCII code block.
  `docs/social/social-preview.png` is the 1280×640 variant for the
  repo's GitHub social preview (Settings → General → Social preview).

### Changed

- **`ibkr positions --watch` default rate is now `1s`** (was `2s`),
  matching `ibkr account --watch`. The 2s default was justified in
  the v0.18.0 changelog with "positions.list does more work than
  account.summary" — true in absolute terms, but both are
  microsecond-scale cache reads on the daemon side. Unified default
  makes the two surfaces feel consistent. Override with `--rate`.

### Fixed

- **`--watch` mentions on `ibkr account` / `ibkr positions`** in the
  skill table, the per-command flag listings, and the
  `isStreamingInvocation` comment in `cmd/ibkr/main.go`. The
  v0.18.0 docs only mentioned `quote --watch`.

## v0.18.0 — 2026-05-14 21:38 CEST

`ibkr account` and `ibkr positions` now answer "how am I doing today?"
on the first call (no more "wait for the second invocation"), refresh
in place via `--watch`, and read on a standard 100-col terminal without
horizontal squinting.

### Added

- **`--watch` on `ibkr account` and `ibkr positions`.** Re-polls the
  daemon on a fixed interval (`--rate 1s` for account, `2s` for
  positions; both override). On a TTY the screen clears and the
  snapshot redraws in place; in a pipe, snapshots are appended
  separated by a dim rule so log captures stay parseable. Polling is
  pull-based — no new RPCs were added; this just calls the existing
  `account.summary` / `positions.list` on a ticker. The default cli
  budget is bypassed for streaming invocations so a long watch isn't
  killed at 60 s.

- **`Daily P&L` row on `ibkr account`** is now always visible. When
  the gateway hasn't yet delivered a `reqPnL` frame, the row renders
  with a dim em-dash + `(subscribing — value lands on next call)`
  hint. The lazy reqPnL kickoff from v0.17.0 is unchanged; the row
  is just discoverable from call one instead of being silent until
  the daemon's cache warms.

- **`DAY P&L` column on `ibkr positions`** (replaces v0.17's
  `DAILY P&L`) is also always visible. Stocks and options share the
  same column source — IBKR's per-conId `reqPnLSingle` (TWS msg 95)
  — so the table answers "today's P&L" with one column instead of
  two near-duplicates. Nil rows render as em-dash; the column is
  never suppressed.

### Changed

- **`Daily P&L` is the second line of `ibkr account`,** sitting
  directly under `Net liquidation`. The decomposition (`of which
  unrealized / realized`) moved to a `Daily P&L breakdown` sub-block
  at the bottom of the snapshot. Rationale: NLV and today's delta
  are the two numbers a trader scans for first; everything else is
  supporting detail.

- **`ibkr account` is more compact.** Five blank-line section
  separators removed — the dim section headers (`Balances`,
  `Session P&L`, `Margin`, …) provide enough visual break on their
  own, saving roughly half a screen.

- **`ibkr positions` table is narrower.** The wide `DAY $` column
  (24 cells with the `+$ X.YZ (+P.QQ%)` composite) is gone in favour
  of the `DAY P&L` column above; total width drops from ~120 cells
  to ~85 (96 with the optional `REAL P&L`), fitting a standard
  100-col terminal without horizontal scrolling. The
  `day_change_money` / `day_change_pct` JSON fields are unchanged
  (consumers may still want them) and the `--by underlying` view's
  `CHANGE / GREEKS` cell still surfaces them inline.

### Removed

- **`DAY $` column on the flat `ibkr positions` view.** Replaced by
  the unified `DAY P&L` column described above. The wire-level
  `day_change_money` / `day_change_pct` fields are still emitted
  in JSON; only the rendered table changes.

## v0.17.0 — 2026-05-14 21:16 CEST

Adds Daily P&L (account-level and per-position), expands the account
snapshot with the fields a trader actually scans for, and switches the
positions table from per-share day moves to position-level dollar moves.

### Added

- **Daily P&L on the account row.** A `Daily P&L` block on `ibkr account`
  carries IBKR's start-of-trading-day delta from the `reqPnL` stream
  (TWS msg 94), with optional `of which unrealized / realized` subrows
  when the gateway supplies them. Distinct from the session-running
  unrealized/realized totals already surfaced. The block renders only
  when the gateway has delivered a frame; otherwise silent.

- **Daily P&L per position.** A new `DAILY P&L` column on
  `ibkr positions` carries IBKR's per-conId start-of-trading-day delta
  from `reqPnLSingle` (msg 95). Shown only when at least one row has
  a value; nil rows render as em-dash. The daemon caps active per-
  position subscriptions at 50; overflow rows render em-dash, never a
  fabricated zero.

- **Account screen now surfaces** `account_type`, `gross_position_value`,
  `unrealized_pnl` (session-running), `realized_pnl` (today's closed),
  `cushion` (with severity bands — red < 0.10, yellow < 0.30, green
  ≥ 0.30), and the `look_ahead_*` quad (init / maint / available funds
  / excess liquidity — post-overnight-cycle projection). All from the
  existing `reqAccountSummary` stream, no new gateway round trips. The
  look-ahead block suppresses itself when every value is zero (cash
  accounts, pre-handshake).

- **MCP-keeps-daemon-alive note** in `README.md` §Architecture,
  documenting that an MCP client's persistent stdio connection pins
  the daemon's `activeConns ≥ 1` for the lifetime of the client.
  Idle-shutdown fires when the MCP host quits.

- **`reqPnL` / `cancelPnL` / `reqPnLSingle` / `cancelPnLSingle` wire
  encoders and inbound handlers** in `pkg/ibkr/pnl.go`. IBKR's
  `DBL_MAX` "not yet computed" sentinel is normalised to nil at parse
  time. Older gateways emitting only the bare `dailyPnL` field
  (without unrealized / realized) are accepted via a short-form
  payload branch.

- **`Connector.SubscribeAccountPnL` / `SubscribePositionDailyPnL`** —
  idempotent subscription kickoffs. Cache lives on the `*Connector`
  so a Connection rebuild resets state cleanly. Counterpart
  `AccountDailyPnL()` and `PositionDailyPnL(conID)` readers are
  non-blocking cache lookups.

- **Unit, daemon, renderer, and integration tests** at every layer
  of the new PnL pipeline.

### Changed

- **Account renderer regrouped** into `Balances` / `Session P&L` /
  `Margin` / `Look-ahead margin` / `Daily P&L` blocks. The hero
  `Net liquidation` stays at the top. Section names line up with the
  IBKR Account Window so the labelling translates 1:1.

- **`DAY CHG` column on `ibkr positions` is now `DAY $`.** It renders
  the position-level money move (qty × per-share for stocks; qty ×
  multiplier × Δ for options when `OptionPrevClose` is populated),
  with the underlying's percent in parens. Money leads because that's
  the figure a trader scans for. Sign-coloured. JSON consumers that
  grep rendered output for `DAY CHG` need to switch to `DAY $`; the
  underlying `day_change` / `day_change_pct` JSON fields are
  unchanged, and a new `day_change_money` field is additive.

- **`pkg/ibkr.Position` now carries `ConID`.** Required for the daemon
  to key `reqPnLSingle` by contract ID. Additive on the JSON wire.

- **`AccountResult` / `PositionView` gain pointer P&L fields:**
  `daily_pnl`, `daily_pnl_unrealized`, `daily_pnl_realized` on
  `AccountResult`, `daily_pnl` on `PositionView`. All `*float64` —
  `nil` distinguishes "no frame yet" / "no entitlement" / "DBL_MAX
  sentinel" from a real zero.

### Fixed

- **Account-level Daily P&L now lands on auto-detect accounts.** The
  post-connect subscription only fired when `[gateway].account` was
  pinned in config; in the common auto-detect path the daemon didn't
  know the account code until after handshake's `managedAccounts`
  message, and the subscribe was silently skipped. Now
  `account.summary` lazy-subscribes via the runtime-resolved account.
  The first call kicks the stream; the next call (within a few
  seconds) shows values.

### Security

- **`SECURITY.md` no longer carries the maintainer's personal email.**
  Reports now flow through GitHub Private Vulnerability Reporting
  (already the preferred path). A no-GitHub-account escape hatch is
  documented.

## v0.16.0 — 2026-05-14 18:10 CEST

Taste-driven sweep prompted by a three-orchestrator senior review. Every
HIGH finding is fixed: a wire-contract violation around option Greeks, a
data race in the message-dispatch path, a half-wired MCP option-resource
template that shipped broken since v0.10.2, and a pocket of orphan exports
in `pkg/ibkr` that the v0.15.0 sweep left behind. The pkg/ibkr API surface
shrinks: this is the v0.16.0 minor bump's only externally-visible change.

### Added

- **`TestFillOptionGreeksPreservesGenuineZero`** in `internal/daemon`.
  Pins the wire contract from `internal/rpc/rpc.go`
  ("Delta/Gamma/Theta/Vega ... never zero-substituted"): a deep-ITM
  call's gamma → 0 / theta → 0 now surfaces as a non-nil pointer instead
  of being silently filtered out. Previously the per-field `g.Delta != 0`
  filter at `handlers.go::fillOptionGreeks` discarded real zeros, making
  consumers branching on `nil-as-unavailable` lie about model output.

- **`TestRegisterUnregisterRace`** in `pkg/ibkr`. Drives concurrent
  `snapshotHandlers` and `UnregisterHandler` against the same msgID
  under `-race` — the previous implementation released the read lock
  before iterating its captured slice while `UnregisterHandler` shifted
  entries in place via `append(entries[:i], entries[i+1:]...)` on the
  same backing array. `deferContractDetailsCleanup` (connector.go) was
  the canonical production trigger.

- **`make hook-regex-check`** Makefile target, invoked from
  `make plugin-check`. Diffs the PreToolUse jq regex between
  `hooks/hooks.json` (the bundled plugin) and `settings/ibkr.settings.json`
  (the user-copyable settings template). Both must run the same regex
  against `.tool_input.command` or the trading-verb defense drifts between
  distribution paths.

- **`TestStreamingParity` is now in `make parity-check`.** The strict
  pre-commit gate previously ran only `TestParity|TestNoTradingTools|
  TestSchemasAreValidJSON`. `TestStreamingParity` lives in the same
  package and is the binding gate for streaming-resource templates, but
  a contributor renaming or adding an `ibkr://...` template would not
  fail `make check`.

### Changed

- **`fillOptionGreeks` and chain `fillOptionLeg` no longer per-field
  filter Greeks against `!= 0`.** The cache's `e.ok` (and
  `Connector.GetOptionGreeks`'s ok flag) already gate "captured tick vs
  miss"; the per-field filter discarded genuine model zeros. The wire
  contract on `PositionView.{Delta,Gamma,Theta,Vega}` and
  `ChainStrike.{Call,Put}Delta` is "nil = unavailable, never zero-
  substituted" — now honored.

- **`snapshotHandlers` iterates under the RLock.** The previous release
  built the snapshot inside the lock, released, then iterated outside —
  fine if reader and writer never overlapped, but
  `deferContractDetailsCleanup` calls `UnregisterHandler` from a goroutine
  while `readMessages` dispatches through the snapshot. Lifting the loop
  under the lock costs zero extra allocations (the per-call slice copy
  already happened) and closes the race.

- **`RateLimiter.SubmitWithPriority(reqType, fn, priority, maxRetries)`
  is now `SubmitWithRetries(reqType, fn, maxRetries)`.** The priority
  parameter was a lie: `processRequests` drained `requestQueue` in
  strict FIFO order with no priority discrimination; the heartbeat path's
  `priority = 1000` bought exactly nothing. The collapsed signature is
  honest about what the queue does. **Library callers of pkg/ibkr's
  rate limiter must rename their call sites** — the only in-tree caller
  was the heartbeat path, which now passes `maxRetries: 0`.

- **`handleStatusHealth` no longer hardcodes `res.DataType = "live"`.**
  v0.15.0 retired hardcoded-live from AccountResult / PositionsResult /
  HistoryDailyResult; HealthResult survived the cleanup. `HealthResult.
  DataType` remains on the wire shape (omitempty) for renderer-fallback
  compatibility but the daemon never writes it — `status` has no
  per-reqID feed type to honestly report.

- **MCP server resource templates now expose only stock quotes.** The
  v0.10.2-era `ibkr://option/{symbol}/{expiry}/{right}/{strike}` template
  was advertised in the manifest, parsed end-to-end, and listed in
  `TestStreamingParity` — but `handleResourcesRead` and
  `runResourceSubscription` unconditionally sent `SecType: "STK"` to the
  daemon, so option subscriptions never actually delivered frames. The
  template, scheme, parse branch, `parsedURI.IsOption` field, and
  `TestParseQuoteURIOption` are all removed. Re-introduction needs a
  proper OPT `ContractParams` build at the seam plus an end-to-end
  integration test.

- **`internal/daemon/handlers.go` and `pkg/ibkr/{connection,
  connector_expiries}.go` switched from `sort.SliceStable` /
  `sort.Strings` / `sort.Float64s` to `slices.SortStableFunc` /
  `slices.Sort`.** Same repo, same Go floor — converging on one idiom.
  The `modernize` analyser doesn't catch this category because the
  imports are distinct packages, not pattern-replaceable.

- **`internal/mcp/tools.go::sortedKeys`** uses `slices.Sort` instead of
  a 13-line hand-rolled insertion sort. The "not worth importing sort"
  argument from before Go 1.21 doesn't survive a second look.

- **`internal/dial/dial.go`** uses `errors.As(err, &ne)` for `net.Error`
  classification instead of the bare `err.(net.Error)` type assertion —
  matches the CLAUDE.md user-rules convention and tolerates a future
  wrapped `net.OpError`.

### Removed

- **Orphan exported APIs in `pkg/ibkr`**, all with zero non-self callers
  and no keep-on-purpose history:
  - `Connection.RequestOpenOrders` — a scaffold no-op (`return nil`).
  - `Connector.GetPool` — exported test shim with no caller (the
    package-private `c.pool` field is reachable from in-package tests).
  - `Connector.Name` — returned the hardcoded string `"IBKRConnector"`
    with no observed caller.
  - `RateLimitedRequest.Priority` field and `SubmitWithPriority` —
    queue is strictly FIFO; field was set but never read. See §Changed
    for the renamed entry point.
  - `RequestType.RequestTypeAccount` — unused enum value (the switches
    in `executeRequest` and `checkCircuit` never matched it).

- **Duplicate `tickType == 106` branch in
  `pkg/ibkr/connector.go::handleTickPrice`.** The same Option Implied
  Volatility tick is handled by `handleTickGeneric` (msgID 45), which
  is the registered handler for the IBKR wire frame that actually
  carries tick 106. The mirror branch in handleTickPrice (msgID 1)
  only wrote to `c.optIV` and missed `sub.IV`, so even if the gateway
  ever delivered 106 on msgID 1 (it doesn't, per current TWS API),
  scan-row enrichment would have seen stale data.

- **Unreachable `map[any]any` fallback in
  `pkg/ibkr/connector.go::Connector.Start`.** `pool.GetPoolStatus`
  unconditionally builds `connections` as `map[int]map[string]any`;
  the defensive type assertion against `map[any]any` could never fire.
  Trimmed ~14 LOC of double-pathway log handling.

- **Per-frame allocation in `processMessage`.** The 14-entry
  `suppressedMessages` map literal that gated the per-frame debug-log
  line was being rebuilt on every inbound IBKR frame (hundreds per
  second during RTH). Hoisted to a package-level
  `suppressedMessageLogIDs` var; the lookup is the same shape, the
  allocation churn isn't.

- **Package-level `func min(a, b float64) float64`** in
  `pkg/ibkr/ratelimiter.go`. Shadowed Go 1.21's predeclared `min` —
  the `modernize` gate doesn't catch shadows. Removing the local form
  let the analyser flag four additional `if x > N { x = N }` patterns
  that now use the builtin.

- **`OptionQuoteURITemplate` / `optionQuoteScheme` / `parsedURI.IsOption`
  / `TestParseQuoteURIOption`** in `internal/mcp/resources.go`. See
  §Changed for the rationale; consumers of the option template (none
  known) should rebuild on top of `ibkr_quote` until a proper OPT
  resource path is reintroduced.

- **Tautological `if alt != base` in `pkg/ibkr/connection.go::tlsAttempts`.**
  Immediately after `alt := !base`, the inner condition was always true.

- **Redundant `if st.Mark > 0` in
  `internal/daemon/handlers.go::buildPortfolioAggregates`.** The loop
  body already `continue`d on `st.Mark <= 0` three lines earlier.

- **Integration-test scaffolding wrappers in `test/integration/
  integration_test.go`:**
  - `errsf` / `osErr` — one-line wrappers documenting themselves as
    "tiny wrapper to keep TestMain free of fmt imports" while `fmt` is
    imported and used in their bodies. Replaced with `fmt.Errorf`.
  - `atomicAdd` — single-caller rename of `atomic.AddInt32` that
    obscured the standard call.

### Fixed

- **`Connector.Start` no longer threads the never-fires `map[any]any`
  branch.** See §Removed; behaviour change is "one log line instead of
  two equivalent log paths."

- **`buildPortfolioAggregates` always counts dollar-delta for the
  surviving stock rows.** Mark is positive by the time the body runs;
  the inner `if` was dead code that suggested otherwise.

## v0.15.1 — 2026-05-14 15:56 CEST

Diagnostics + release-process hardening prompted by a post-v0.15.0
investigation: a `chain SPY` timeout during smoke-testing went
unexplained until an audit caught two issues — `handleChainExpiries`
was unobservable when it failed, and the binary I'd "verified" was a
stale pre-commit build, not the release artefact. Both are addressed
here. No runtime behaviour change to existing surfaces.

### Added

- **Per-stage budget logging in `handleChainExpiries` and
  `handleChainFetch`.** Each handler emits one INFO line at every
  exit (success and error) with the wall-clock breakdown — e.g.
  `chain.expiries SPY done in 8013ms (expiries+strikes=8002ms, spot=0ms,
  iv-fanout=0ms)`. The next investigator can read the line and know
  immediately whether a 25 s budget went to the SECDEF round-trip, the
  spot snapshot, or the IV fan-out. Previously a chain timeout left
  55 seconds of dead log silence and required reading the source to
  guess where the budget was spent.

- **`make release-verify` target** (and `scripts/release-verify.sh`).
  Wired into `make release` between the rebuild and the cross-compile,
  so a binary that cannot talk to the gateway cannot reach GitHub
  Releases. The smoke matrix runs against an isolated daemon under
  `/tmp` (no contact with the user's running one) and asserts:
    1. The binary stamps the expected version.
    2. `status` reports `connected=true` and the daemon version matches.
    3. `account.summary` returns a non-empty `account_id` and emits
       no `data_type` field (v0.15 omitempty contract).
    4. `positions.list` returns valid `stocks` / `options` arrays and
       emits no `data_type` field.
    5. `quote SPY` returns `symbol=SPY` with a `data_type` that is
       either empty (no tick in budget — degraded but acceptable) or
       one of the canonical strings (`live`, `delayed`, `frozen`,
       `delayed-frozen`).

  Each check runs under a 15 s per-command deadline (override via
  `IBKR_RELEASE_VERIFY_TIMEOUT`). Option-chain commands are deliberately
  excluded — IBKR's secdef-farm is genuinely degraded pre-RTH and a
  chain request can legitimately take ≥25 s on a cold cache, which is
  not what a release gate should branch on.

### Changed

- **`make release` now calls `make release-verify` between `make build`
  and `git push origin TAG`.** A verify failure aborts the release
  before any tag, plugin tag, or binary reaches origin; recovery is
  `git tag -d $RELEASE_VERSION` and try again. Closes the gap that
  caused v0.15.0 to ship a binary I'd never actually smoke-tested
  against the gateway (my "verified" run was against the pre-commit
  v0.14.0-dirty build).

## v0.15.0 — 2026-05-14 14:39 CEST

Taste-driven audit pass: every HIGH finding from the senior-review sweep is
fixed. The bulk is correctness — sentinel-based reconnect classification, lock
ordering on the subscription path, reqID-scoped option-IV cancellation, and an
honest `data_type` story across the wire surfaces — plus a round of orphan
subtraction in `pkg/ibkr` and four tests rewritten to actually pin the
behaviour they advertised.

### Added

- **`Connector.CancelOptionIV(reqID int)`** in `pkg/ibkr`. Cancels an
  option-IV subscription previously returned by `SubscribeOptionIV` using its
  reqID — the chain-IV fan-out (`internal/daemon/handlers.go::collectExpiryATMIV`)
  now pairs every subscribe with this reqID-scoped cancel instead of the
  previous symbol-scoped `UnsubscribeMarketData(symbol)`, which under
  concurrent fan-out could no-op or rip down an unrelated streaming-quote
  subscription on the same underlier.

- **`errClientIDInUse` sentinel** in `pkg/ibkr/connection.go`. The reconnect
  path's "code 326 / client id already in use" classification is now done via
  `errors.Is`. Producers wrap with `%w`; consumers stopped substring-matching
  their own format strings, which had coupled the retry decision to the exact
  human-readable wording at two call sites separated by 400 lines.

- **`requestCtx(parent, method)` helper** in `internal/daemon/server.go`,
  factoring the per-method unary deadline out of `dispatch`. Lets a real test
  observe the child context's deadline rather than asserting (uselessly) that
  `context.Background()` has none.

### Changed

- **`Quote.DataType` is now plumbed from the live subscription** rather than
  hardcoded `"live"`. The daemon reads the per-reqID feed state via
  `Connector.GetMarketDataTypeForSymbol` while the subscription is still open,
  so consumers branching on `data_type` see the real value (live / delayed /
  frozen / delayed-frozen) instead of a constant.

- **`AccountResult.DataType`, `PositionsResult.DataType`, and
  `HistoryDailyResult.DataType` are `omitempty` and emitted empty.** These
  surfaces do not have a meaningful live/delayed dimension (account-summary
  is gateway-direct; historical bars are stored data; positions arrive from
  the portfolio stream, not per-symbol market data). The fields were
  previously hardcoded `"live"` — a lie that hid genuine feed-state issues
  from any renderer that branched on the value.

- **`dropSubscription` and `SubscribeMarketData` no longer hold `subMu`
  across `CancelMarketData`.** Both sites lift the cancel target under the
  lock, release `subMu`, then call into the rate-limited socket write — so
  the 30 s rate-limiter ceiling and the connection's `writeMu` no longer
  block every other subscription reader (tick handlers, `GetMarketData`,
  scan-row enrichment) for the duration of the cancel.

- **`InactiveSymbolStore` interface and `Connector.UseInactiveSymbolStore`
  unexported** to `inactiveSymbolStore` and `useInactiveSymbolStore`. The
  interface method signatures take the unexported `inactiveSymbolState`, so
  the contract was satisfiable only from inside the package — the public
  shape was a misadvertised extension point. Tests stay in-package and keep
  using the renamed entry.

- **`rpc.go` MarketDataType doc** now lists only the struct fields that
  actually carry it (`Quote`, `Frame`, `ChainResult`, `HealthResult`).
  Previously it named two fields (`PositionView.DataType`, `ScanRow.DataType`)
  that have never existed on the struct.

- **`internal/cli/cli.go::formatMoney`** doc no longer calls itself a "legacy
  entry point." It is a USD-only convenience used by renderers that work with
  intrinsically USD-only data (chain strikes, history rows, scan results) —
  not a deprecated form.

### Removed

- **Speculative / orphan exports in `pkg/ibkr`:**
  - `Connector.GetOptionQuoteMid`, `Connector.GetOptionParams`,
    `Connector.SetDerivedOptionIV`, `Connector.DrainOrderUpdates` —
    exported with zero non-test callers anywhere in the tree. Their
    write-only supporting state (`optQuoteMid` map + tick-handler writes,
    `optParams` map + writes, `orderUpdates` slice + `onOrderStatus` write
    site) went with them. The cascaded reduction trims ~70 lines from
    `connector.go` and removes two derive-mid blocks whose only consumer
    was the orphan getter.
  - `Connector.PrewarmContracts` + `PrewarmConfig` (and their dedicated
    file `prewarm.go` + `prewarm_test.go`). Only the test exercised them;
    the daemon prewarms differently via `handlers.go::prewarmPrevCloses`
    and `prewarmOptionGreeks`, which are unaffected.
  - `PoolConfig.EagerConnect` field and the whole eager-connect code path
    in `ConnectionPool.Start`. The field was never set to true anywhere —
    the daemon always uses lazy connect. The branch was unreachable in
    production.

- **Dead struct fields:** `Position.{VaR, MaxLoss, MarginRequired}` and
  `Order.{StrategyID, ParentOrderID, InstanceID}`. Declared on exported
  types in `pkg/ibkr/types.go`; never assigned or read by any caller.

- **Hand-rolled `maxInt(a, b int) int`** in `internal/daemon/handlers.go`,
  replaced by Go 1.21's builtin `max`. The `modernize` gate does not flag
  custom-named helpers — this was a human-review find.

- **`debug_handshake_test.go` and `gateway_test.go`** in `pkg/ibkr`. Both
  required a live Gateway on a specific port and `t.Skipf`'d on every CI
  run without one, contributing zero signal while shipping the appearance
  of coverage. The shared helper `buildHandshakeFrame` stays in
  `handshake_test.go` where the real handshake unit tests live.

- **Orphan promise in `scanner_params_test.go`** — the comment advertising
  a `TestParseScannerParametersXML_LiveFixture` that was never written.

### Fixed

- **`collectExpiryATMIV` no longer cancels with the wrong handle.** The
  previous `defer UnsubscribeMarketData(symbol)` either no-op'd (the common
  case, because `SubscribeOptionIV` does not install a
  `subscriptions[symbol]` entry) or — when an unrelated `quote --watch` was
  active on the same underlier — tore that quote subscription down out from
  under its owner. The fan-out at `chainExpiryWorkers` parallelism now uses
  the reqID-scoped `CancelOptionIV` so concurrent expiries on one symbol
  cannot interfere with each other or with anyone else.

- **`io.EOF` comparisons in the reconnect classifier** now use
  `errors.Is(err, io.EOF)` instead of `err == io.EOF`, matching the two
  other sites in the same file. Wrapped EOF from a future transport shim
  would otherwise have been silently misclassified.

### Tests

- **`TestRequestCtxAppliesUnaryDeadline` (new)** replaces the tautological
  `TestDispatchAttachesPerRequestDeadline`, which asserted only that
  `context.Background()` had no deadline — true by construction. The new
  test exercises `requestCtx` directly and verifies the child carries a
  deadline within ±1 s of `unaryDeadline(method)` while the parent stays
  untouched. A companion test pins the streaming branch returning the
  parent context unchanged.

- **`TestOpenSocketRemovesStaleSocketFile` rewritten.** The previous form
  could pass without exercising the stale-socket branch at all: closing the
  staging listener cleaned up the inode, so the fallback wrote a regular
  file (not a socket) and the assertion accepted EADDRINUSE as success.
  Now uses `(*net.UnixListener).SetUnlinkOnClose(false)` to deterministically
  stage a stale socket inode and dial-probes the freshly bound listener.

- **`TestOrderHandlersAlwaysRefuse` and
  `TestDispatchOrderVerbsClassifyAsTradingDisabled` (new)** in
  `internal/daemon/trading_disabled_test.go`. The build-tagged simulator that
  used to override these handlers was removed in v0.14, but no unit test
  pinned the always-refused behaviour — `TestTradingVerbsRefused` in
  `test/integration` only ran with a live gateway. These tests now backstop
  both the handler-level refusal (`errors.Is(err, ErrTradingDisabled)`) and
  the dispatcher-level wire classification (`rpc.CodeTradingDisabled`).

- **`isScannerTimeout` narrowed** to scanner-subscription-specific error
  shapes only. Generic `"context deadline exceeded"` and `"i/o timeout"`
  were dropped — those fire on any handler deadlock or socket drop, which
  the integration tests should fail on, not skip. `TestScanParamsReturnsCatalog`
  also lost its `t.Skipf` entirely: catalog requests are gateway-stored data,
  not subject to off-hours scanner-subscription warmup, so a timeout there
  is a real regression in the wire/parser path and must surface.

## v0.14.0 — 2026-05-14 11:05 CEST

Audit-driven cleanup: two portfolio-aggregate correctness fixes, plus ~5,000 LOC of subtraction across lifecycle scaffolding, dormant subsystems, and dead test suites. No behaviour change for the daemon or CLI; library consumers see the removed items disappear.

### Fixed

- **Portfolio aggregates honour the option contract multiplier from the wire.**
  `optionMultiplier` previously took a `PositionView` and discarded it, returning
  a hard-coded `100`. The wire already populates `PositionView.Multiplier` from
  `pos.Asset.Multiplier`, so for index options on
  multipliers other than 100 — NDX/SPX 100, mini-options 10, some indexes
  1000 — `effective_delta`, `dollar_delta`, and `daily_theta` were silently
  off by an integer factor. Helper now reads `p.Multiplier`, falling back to
  100 only when the wire didn't carry a value. New regression test
  `TestBuildPortfolioAggregatesHonorsMultiplierFromWire` pins the index-option
  case at `Multiplier=1000`.

- **`dollar_delta` is computed against the spot the Greeks were modelled at.**
  The aggregator's comment claimed it would "use the option's mark-side
  underlying if available, else fall back to PrevClose," but there was no
  mark-side branch — it always used PrevClose. After any overnight gap the
  number lied by the size of the gap (a 3% gap → a 3% lie). The greeks cache
  already captured the model-computation underlying alongside the per-leg
  Greeks in `greeksEntry.underlying` and just dropped it on the floor. New
  `PositionView.Underlying` field surfaces the captured spot to the aggregator
  via `fillOptionGreeks`; the aggregator prefers it and falls back to PrevClose
  only when the leg's Greeks tick didn't carry a spot. New regression tests
  cover the precedence and fallback paths.

### Changed

- Toolchain floor raised to Go 1.26. Internal modernization to Go 1.21–1.26
  idioms (`any`, `range N`, `maps.Copy`, `strings.SplitSeq` / `FieldsSeq`,
  `b.Loop`, `wg.Go`, `new(expr)`, `strings.Cut`, `fmt.Appendf`,
  `strings.Builder`, `max`). No behavior change. Build now needs Go 1.26+.
- Added `make modernize-check` gate (runs `go fix -diff` + `go tool modernize`).
  Wired into `make check` so idiom drift fails CI. Modernize version is pinned
  via the `tool` directive in `go.mod` — no `@latest` install in CI.
- **`internal/daemon/trading_disabled.go` no longer hides behind `//go:build !trading`.**
  The build-tag gate promised a `trading_enabled.go` counterpart for v2 that
  doesn't exist and isn't planned. Same dispatcher rejection
  (`MethodOrderPlace` / `MethodOrderCancel` → `ErrTradingDisabled`), now
  unconditional. README's safety section no longer claims a build tag exists.

### Removed

First wave — lifecycle scaffolding never wired through:

- **`Connector.PlaceOrder(*Order)` simulator stub.** Comment said `// For now,
  simulate order placement`; status got stamped `Submitted` without touching
  the wire. Library consumers should call `Connector.SubmitOrder` (the real
  wire path, unchanged). README's protocol-coverage table and `pkg/ibkr/doc.go`
  now name `SubmitOrder` instead of `PlaceOrder`. The dependent
  `Connector.validateOrder` (only called by the deleted stub) is gone too.
- **`OrderManager` + `OrderFill` + 12 methods + `isOrderOpen`.** Parallel
  in-memory order tracker; zero non-test callers. The Connector's own
  `openOrders` map handles tracking.
- **Two `AccountSummary` shapes collapsed to one.** `Connector.GetAccountSummary`
  (returning `*AccountSummary`) and its parser `buildAccountSummary` were
  shadowed by `Connector.RequestAccountSummary` (returning `*RawAccountSummary`)
  the daemon actually uses. The two parsers also disagreed subtly (one invented
  `BuyingPower` from margin if absent). Live path unchanged.
- **`IBKRStatus` enum + maintenance-window detector** (`pkg/ibkr/connector_status.go`).
  4-state coarse status that wrapped the existing 5-state `ConnectionStatus`;
  zero non-test callers.
- **`DBInactiveSymbolStore` Postgres-backed inactive-symbol store.** Zero
  callers; `go.mod` carried no SQL driver. The `InactiveSymbolStore` interface
  stays — library consumers can still implement it.
- **`Connector.GetStatus` / `GetSubscriptionStats` / `GetErrorStats` +
  `recordError` + the `errMu`/`errTotals`/`errEvents` plumbing.** A "system
  status endpoint" that doesn't exist in this binary; everything fed it is
  now removed.
- **`MarketPhase` type + 6 constants + `FreshThresholdForPhase`.** Zero
  callers, including tests.
- **`GatewayBootstrapper` interface, `GatewayBootstrapFunc` adapter, and the
  Connector retry branch.** The hook field was wired through `ConnectorConfig`
  → `Connector.gatewayBootstrapper` → a retry-on-lease-failure call path —
  but no production code ever assigned it. The daemon autospawns via
  `internal/dial/autospawn.go` instead.
- **`ConnectionConfig.ClientIDIncrement`.** Documented as `1=linear, 2=exponential`
  but `Connect` always did `currentClientID++`. Setting it to 2 had no effect.
- **Daemon `cache` package** (`internal/cache/`). The package held two
  storage primitives: `JSONCache` for contract details (`Put`-only — daemon
  never read; the Connector has its own contract cache) and `InactiveStore`
  (opened, threaded through, `Flush`ed on shutdown — never `Mark`ed and
  never read). Both removed; the package is empty so the directory is too.
  Files at `~/.local/state/ibkr/contracts.json` and `inactive.json` from
  prior daemons can be deleted by hand — they're no longer touched.

Second wave — dormant subsystems flagged during cleanup:

- **Wire interceptor's override path.** `ApplyOutboundOverrides`,
  `OverrideOperation`, `messageOverride`, `applyOperations`, the `autoApply`
  bit, the `overrides` map, the `IBKR_WIRE_MAX_AUTOFIX_ATTEMPTS` env var,
  and `HandleParserError` (with its `ParseError` type and the
  `reportParserError` Connection method that fed it). Production used
  passive recording only; the override machinery sat behind an `autoApply`
  bit no production caller ever flipped, with a doc-comment admitting
  "preserved so future tooling can attach." Per CLAUDE.md "no speculation."
  Passive recording (`RecordInbound` / `RecordOutbound` / ring buffer +
  optional JSON-lines persistence via `IBKR_WIRE_LOG_PATH`) is unchanged.
  The orphan `encodeFromFields` helper in `Connection` (only consumed by
  the deleted re-encode path) goes too.
- **Execution-report family.** `RegisterExecutionListener` /
  `RegisterCommissionListener` / `Connector.RequestExecutions` /
  `Connection.RequestExecutions`, the `ExecutionReport` /
  `CommissionReport` / `ExecutionFilter` types, the parsers
  (`parseExecutionReport`, `parseCommissionAndFees`,
  `decodeExecutionDetailsProto`, `decodeExecutionDetailsEndProto`,
  `parseExecutionDetailsPayload`, `parseExecutionFields`,
  `parseContractFields`, the surrounding protobuf-decode helpers in
  `protobuf_decode.go`), the dispatcher cases for `msgExecutionData` /
  `msgExecDetailsEnd` / `msgExecutionRequestAck`, the `Connector`'s
  `execListeners` / `commListeners` / `installExecutionHandler` /
  `installCommissionHandler` / `dispatchExecutionReport` /
  `dispatchCommissionReport` / `snapshotExecutionListeners` /
  `snapshotCommissionListeners` / `logError`, plus the dormant
  `tryDecodeProtoMessage` codepath and seven now-orphan IBKR server-version
  / message-id constants. Plumbed end-to-end but zero non-test consumers;
  `pkg/ibkr/doc.go`'s "Protocol coverage" never advertised it.
- **`google.golang.org/protobuf` dependency.** The execution family was
  the only consumer; `go mod tidy` removed it. `pkg/ibkr` now has zero
  third-party Go dependencies on its hot path.
- **`pkg/ibkr/testdata/reqmktdata_{index,option,stock}_sv176.bin`** plus
  the two Python generator scripts (`generate_reqmktdata_fixtures.py`,
  `generate_reqcontractdata_fixture.py`). Committed years ago, never
  consumed by any Go test, no Make target. Undiscoverable dev tools.

Third wave — test suites for surfaces the binary refuses or doesn't exercise:

- **Order-test suites that test code the `ibkr` binary refuses.** The whole
  of `pkg/ibkr/orders_test.go` (~559 LOC: `TestOrderPlacement`,
  `TestOrderValidation`, `TestLiveOrderFlow`, `BenchmarkOrderValidation`,
  `setupTestConnection`) and `pkg/ibkr/connector_orders_test.go` (~411 LOC:
  `TestConnectorOrderFlow`, `TestOrderMessageEncoding`,
  `TestOrderValidationExtended`, `BenchmarkOrderPlacement`). Big suites
  gated on `testing.Short` + `IBKR_RUN_ORDER_FLOW=1` + `skipIfLiveTrading`,
  with mostly `t.Logf`/`t.Skip` assertions even when they DO run, against
  a feature `internal/daemon/trading_disabled.go` refuses unconditionally.
  The integration test `TestTradingVerbsRefused` pins the actual binary
  contract; that's the one that matters. `connection_orders_test.go`
  shrinks to a single regression — `TestPlaceOrderDoesNotSendDoubleMaxSentinels`,
  the v0.7-era wire-shape pin worth keeping for downstream forks /
  clean-room ports. `skipIfLiveTrading` + `isLiveTradingEnv` helpers in
  `testenv_test.go` go with the suites (no other callers).

- **`TestRequestAccountSummary_HappyPathParsesSummary`** in
  `pkg/ibkr/account_summary_test.go`. 50 lines of careful scaffolding
  followed by an unconditional `t.Skip` admitting "orchestration test
  deferred to integration." Parser logic is already covered by the four
  `TestParseAccountSummary_*` siblings. Misled future readers into
  thinking happy-path orchestration was covered. (Side fix:
  `TestRequestAccountSummary_TimeoutDoesNotLeakGoroutines` snapshots the
  goroutine baseline AFTER constructor setup — the leak detector now
  measures only per-call leaks, not pre-existing rate-limiter
  goroutines that the deleted test had been polluting the baseline with.)

- **`TestHandlerRegistration_NoRaceCondition` and three sister tests**
  (`pkg/ibkr/connector_handler_race_test.go`, ~267 LOC,
  `TestConnector_HandlersRegisteredBeforeReady`,
  `TestConnector_EarlyMessageHandling`,
  `TestConnector_ConcurrentHandlerRegistrationAndMessages`). Tests
  manually pulled handler closures from `mockConn.msgHandlers` and called
  them directly, bypassing the production read-loop / dispatch path
  where any real registration race would actually live. Test names
  promised concurrency coverage they didn't deliver — a future race
  in `Connection.Run` / `processMessage` would have passed every one
  of them. If a real race-test is wanted later, drive `Connection.Run`
  against a `net.Pipe`.

- **`TestSkillDocumentsEveryCommand`** in `internal/cli/cli_test.go` plus
  the `skillExcluded` map. Greped SKILL.md for the literal `` `ibkr <name> ``
  substring per CLI command — a brittle prose-pattern check that fails on
  honest doc rewords and passes on subtly-wrong rewordings. The structured
  parity gate in `internal/mcp/tools_test.go::TestParity` is the one that
  matters (wire-surface drift, not prose).

- **`pkg/ibkr/protocoltest/` package** plus `cmd/matrix/main.go`
  (~850 LOC). Advertised as a "wire-format encoder/decoder spec used by
  unit tests," but `TestEncodeMessageVariants` only checked byte-length
  sanity and the null delimiter — no decode round-trip, no comparison to
  the production `Connection.encodeMsg`. The captured-fixture pattern
  in `pkg/ibkr/wire_fixtures_test.go` + `scanner_test.go::TestParseScannerData_LiveFixture`
  is what actually catches wire regressions, and it never imported
  `protocoltest`. A future wire-spec contract test (one that diffs
  `EncodeMessage` against `Connection.encodeMsg`) is welcome — but this
  package wasn't that test.

### Documentation

- **README** picked up four drift fixes: `Connector.GetPositions` (deleted in
  v0.12.5), `Connector.PlaceOrder` (now `SubmitOrder`), `go install` Go
  floor (1.25 → 1.26 to match `go.mod`), and the safety-layer description
  (no more `//go:build !trading`).
- **`pkg/ibkr/doc.go`** protocol coverage header updated; order-placement bullet
  and read-only-safety section now point at `SubmitOrder` and describe the
  daemon dispatch refusal accurately.
- **README troubleshooting** picks up an entry for the wire-capture
  diagnostic env vars (`IBKR_WIRE_INTERCEPTOR`, `IBKR_WIRE_LOG_PATH`,
  `IBKR_WIRE_RING_SIZE`, `IBKR_PACKET_LOG_TEMPLATE`) — all four are off
  by default, were silently consulted at runtime, and previously had no
  user-facing documentation.
- **SECURITY.md** gains a "Diagnostic data sensitivity" section
  spelling out what the wire-capture files contain (account IDs, contract
  identifiers, P&L) and how to handle them when sharing for debugging.
- **`internal/rpc.PositionsListParams`** doc-comment corrected: it
  previously said "v1 ignores fields" even though the daemon honours both
  `Symbol` and `Type` filters.

## v0.13.0 — 2026-05-13 21:37 CEST

Drift-cleanup minor. Bundles the patch-class fixes that surfaced during
the v0.12.3 design-system rollout and the v0.12.4 AVG COST audit with
two wire-shape additions: per-row `currency` on scan results and
`daily_theta_currency` on the portfolio aggregate. Both wire changes
are additive (`omitempty`) — old MCP / JSON consumers keep working
without changes.

### Fixed

- **`ibkr positions` AVG COST on options actually normalises to per-share now.** The v0.12.4 fix shipped a SecType check against `"OPT"`, but the daemon stamps `PositionView.SecType` with `string(pkg/ibkr.AssetTypeOption)` — the full word `"OPTION"`. The check never matched in production, so every option row continued to render the per-contract premium next to a per-share Mark. The hand-written unit tests used `"OPT"` to match the buggy renderer, so the suite stayed green while the live CLI was wrong. The renderer now compares against `rpc.SecTypeOption` (the canonical wire value, hoisted into the rpc package so future drift surfaces at the constant); the tests use `STOCK` / `OPTION` to mirror the actual wire shape; a new "OPT short form returns raw" case pins the legacy literal as a no-op so a future revert can't quietly re-introduce the bug.

- **`ibkr size` no longer stamps the base-currency symbol on quote-currency values when `--fx ≠ 1`.** The v0.12.3 design-system rollout switched `size` from `formatMoney` (hardcoded `$`) to `env.formatMoneyNegCcyRight(v, ccyBase, …)` for visual consistency. The labelling was correct for the common `--fx 1.0` case (base == quote) but became actively misleading when `--fx 1.085` (EUR account sizing a USD trade): `Risk in quote ccy`, `Per-share risk`, `Notional`, `Max loss at stop`, and `Max gain at target` all rendered with the EUR symbol on values that were actually in USD. Fix: quote-currency lines render bare via `formatMoneyBare` when `fx ≠ 1`; base-currency lines keep the symbol; a new "Max gain in base ccy" line surfaces `reward_base` (already in the JSON wire shape, never rendered before) so the user can compare reward to risk in matching currencies.

- **`ibkr positions --by underlying` no longer reserves dead width on the CHANGE/GREEKS column.** Previously hardcoded to 27 cells (sized for the full Greek tuple). When the daemon's Greeks pipeline goes silent — model-computation tick OOH, illiquid leg, busy subscribe slots — every option row falls to a 15-cell placeholder, leaving 12 cells of trailing whitespace before MKT VALUE on every option row. The column now sizes itself to the widest cell actually rendered in this call: header width (15) as floor, full Greek tuple (27) as ceiling. Captured-Greeks layout unchanged; unavailable-Greeks layout much tighter.

### Changed

- **JSON schema docs catch up to several releases' worth of wire-shape additions.** `skills/ibkr/schemas.md` had drifted since v0.10.x: account schema showed a phantom `"profile": "live"` field the daemon doesn't emit and was missing `currency_exposure[]` entirely; positions schema was missing `multiplier` (which v0.12.4 added and is the divisor a JSON consumer needs to reproduce the per-share AVG COST normalisation), per-leg Greeks (`delta`/`gamma`/`theta`/`vega`), the per-leg option market data (`option_bid`/`option_ask`/`option_prev_close`/`iv`), the stock-side prev-close/day-change/FX columns, and the entire `portfolio` aggregate block. `SKILL.md`'s positions example also showed `avg_cost: 6.82` (per-share) for an option in conflict with the schema's correctly-shaped per-contract value (`682.0`). Both files now document the existing fields accurately, with explicit per-share / per-contract guidance on `avg_cost` and a note pointing JSON consumers at `multiplier` as the divisor. New text in both files documents the SecType wire-value convention (`STOCK`, `OPTION`, ... — full word, not three-letter short form) so v0.12.4-class drift is harder to repeat.

- **`rpc.SecType*` constants** for `STOCK` / `OPTION` / `FUTURE` / `INDEX`. The daemon fills `PositionView.SecType` from the `pkg/ibkr.AssetType` enum, whose stringified values are these full words. Comparing against the constants instead of literal strings prevents the v0.12.4-class "two callers, two literals" failure mode. Call sites updated: `internal/cli/positions.go` (`avgCostPerShare`), `internal/cli/positions_test.go` (every case), `internal/daemon/handlers.go` (`optionGreeksKey`), and the `cmd/_preview` fixture. `ContractParams.SecType` (the request-side shape, which uses the IBKR API's three-letter short form `STK`/`OPT`/`FUT`/`IND` — a different path) gains a doc-comment that spells out the asymmetry explicitly.

- **`rpc.Quote.IV` / `rpc.ScanRow.IV` / chain IV fields explicitly documented as decimal fractions.** Unit conventions were consistent across every renderer but absent from the Go doc-comments — only the test suite pinned them. `IV` is a decimal fraction (`0.247` = 24.7%); `ChangePct` is in percent units (`0.70` = 0.70%). Both convention notes now live on the `Quote` and `ScanRow` type comments where any new MCP / JSON consumer would look first.

- **`make check` gofmt gate scopes to git-tracked files** (via `git ls-files`) instead of walking the whole tree. Gitignored paths — Claude Code agent worktrees at the repo root (`optimistic-newton-*-*`), `bin/`, `dist/`, etc. — no longer trip the gate when they happen to contain unformatted Go files. Same scope on `make fmt` so the two stay idempotent.

### Added

- **`rpc.ScanRow.Currency`** (ISO-4217). IBKR's scanner subscription already returns each row's contract currency in `pkg/ibkr.ScannerRow.Currency`; the daemon now threads it through to the rpc-level `ScanRow`. Renderer uses it to pick the right symbol (`$` / `€` / `£` / `¥` / ISO code for the rest). Empty currency falls back to `$` so consumers reading older daemon output keep working. Fixes the hardcoded-`$` rendering for non-US ad-hoc scans (`--exchange STK.EU.IBIS`, `STK.HK`, `STK.LSE`, etc.).

- **`rpc.PositionsPortfolio.DailyThetaCurrency`** (ISO-4217 or `"MIX"`). Mirrors the existing `DollarDeltaCurrency`: a single ISO code when every theta-bearing option leg agrees on currency, `"MIX"` when the book mixes currencies (in which case the renderer prints "(mixed currencies)" and skips the symbol — the sum is undefined). For a USD-only book the value is correctly `"USD"` so the renderer no longer hardcodes `$`. Tracked independently from `DollarDeltaCurrency` because the contributing leg sets can differ (a leg can have theta but not delta, or vice versa, depending on which model-computation ticks the gateway delivered within budget).

## v0.12.5 — 2026-05-13 09:20 CEST

Cleanup release. The v0.12.4 audit surfaced that `Connector.GetPositions` had no callers anywhere in the repo — every position read goes through `GetCachedPositions` → `convertIBKRPositions`. The two functions weren't equivalent: `GetPositions` sent a fresh `reqPositions` to the gateway, which would actively clear the cache populated by the streaming `RequestAccountUpdates` subscription and lose mark/value/P&L. Anyone who wired through it would have got snapshot positions with no live prices, on a connection whose streaming state was now broken. Deleted. No flag changes, no behaviour change for the daemon, no API change anyone is using.

### Removed

- **`Connector.GetPositions` (~115 LOC).** Old snapshot-based positions API, superseded by `GetCachedPositions` during the v0.10.x refactor that introduced `RequestAccountUpdates` streaming. The function did three problematic things: (1) called `conn.RequestPositions()` which clears the cache the streaming subscription populates — calling it would have left the daemon's portfolio state corrupt until the next gateway push; (2) waited up to 15 s synchronously for `positionEnd` — fine for a one-shot CLI but wrong for a long-lived daemon; (3) contained the off-by-100× synthetic-P&L fallback that v0.12.4 already removed in case anyone wired through it. Doc-comment references in `RequestAccountUpdates`, `convertIBKRPositions`, and `pkg/ibkr/doc.go` were updated to point at `GetCachedPositions` as the canonical positions read.

## v0.12.4 — 2026-05-13 08:57 CEST

Two `AvgCost` plumbing fixes from an audit pass: the visible `AVG COST` column on option rows now normalises to per-share, and a latent off-by-100× synthetic-P&L fallback in an unused code path is gone. JSON output stays IBKR-faithful; only the rendered column normalises.

### Fixed

- **`AVG COST` on options now reads in per-share units.** IBKR's `averageCost` field in `msgPortfolioValue` is per-share for stocks but per-contract (multiplier-inclusive) for options — so a $3.00 premium call comes off the wire as `300.00`, which renders as `$300.00` in the AVG COST column right next to a `$3.00` Mark. Looks broken; isn't broken. The renderer now divides by the contract multiplier on OPT rows before formatting (`avgCostPerShare` in `internal/cli/positions.go`), so the columns now share a unit. Stocks unchanged. `PositionView.Multiplier` was added to the JSON schema so consumers reading JSON directly can do the same normalisation; the field is set to 1 for stocks, 100 for standard equity options, and whatever the gateway reports for index/futures options. The raw `avg_cost` field stays IBKR-faithful (per-contract on OPT) — no silent unit change in the wire shape. Seven table-test cases in `internal/cli/positions_test.go` cover STK, OPT-100, OPT-1000, missing multiplier (returns raw, no div-by-zero), missing SecType (defensive — no OPT assumption), and a negative cost. Preview fixtures in `cmd/_preview/main.go` were updated to mirror real wire data (per-contract AvgCost + Multiplier set) so the visual preview matches what the daemon serves.

- **Latent off-by-100× in `Connector.GetPositions` removed.** That function carried a fallback that synthesised `UnrealizedPnL` when IBKR reported zero: `(currentPrice - AverageCost) × position × multiplier`. Fine for stocks (both values per-share, mult 1). Catastrophically wrong for options — `currentPrice` is per-share but `AverageCost` is per-contract on OPT, so the formula effectively subtracted a per-share number from a per-contract number, then multiplied by 100. On a long AAPL $210C bought at $5.10/share showing zero P&L, the synthesised value was on the order of −$30,400. IBKR sends `UnrealizedPNL` directly on every portfolio update, so the fallback also wasn't necessary — the wire-reported value is authoritative, including a genuine zero. `Connector.GetPositions` has no remaining callers (the daemon uses `GetCachedPositions` → `convertIBKRPositions`, which never had this fallback), so the bug was never reachable in production — but leaving it in the code was a footgun for any future caller that wired through. New regression test `TestConvertIBKRPositionsPassesUnrealizedPNLThrough` in `pkg/ibkr/connector_positions_test.go` pins the live path's behaviour: a wire-reported zero stays zero, no synthesis allowed.

## v0.12.3 — 2026-05-13 08:45 CEST

Patch release. Every CLI renderer now follows the same visual language: dim column headers with a dim rule beneath, right-aligned money columns so decimal points line up, bold reserved for the single hero number per screen, sign-coloured P&L, em-dash placeholders for missing data (never a fabricated zero). Two daemon-side fixes ride along: the FX-sensitivity line stops printing the literal `BASE` pseudo-currency, and the per-leg Greeks cache now expires negative entries fast enough to recover from a cold-start miss.

### Fixed

- **`ibkr positions` no longer prints literal `BASE per +1% FX`.** The portfolio FX-sensitivity line now names the actual base currency (e.g. `EUR per +1% FX`). The daemon resolved the base from a bare `Currency` tag in the streaming account-summary map, but IBKR populates that tag with the literal string `"BASE"` (the gateway's pseudo-currency name, not the account's real base), so `FXBaseCurrency` came back as `"BASE"` and the renderer dutifully printed it. The resolver now scans the `$LEDGER:ALL` rows for an `ExchangeRate_<ccy>=1.0` entry — the currency whose rate is exactly 1.0 is the base by definition — and only uses the `Currency` tag when its value isn't the literal `"BASE"`. Five regression tests in `internal/daemon/fx_decorator_test.go` cover the pseudo-currency case, a real-currency `Currency` tag, the `ExchangeRate`-only fallback, the no-signal case (returns empty, never invents a default), and the empty/pre-handshake map.

- **Greeks cache recovers from a cold-start miss.** A cold daemon's first prewarm call commonly fails to receive option model-computation ticks (msg 21 tickType 13) within the 2.5 s budget — the option-tick pipeline takes a few seconds to settle on a fresh connector. Under the previous single 60 s TTL, that one transient miss negative-cached every leg and locked retries out for a full minute. Negative entries now expire after 10 s (`greeksNegativeTTL`); positive entries keep the existing 60 s. A retry after a cold-start miss can now re-subscribe within 10 s and capture the live values once the connector has settled. Positive cache behaviour is unchanged — back-to-back invocations during a decision pause still cost zero gateway round trips. New test `TestGreeksCacheNegativeTTLShorterThanPositive` pins the asymmetry: at the same stale age a negative entry must miss while a positive entry must hit.

### Changed

- **Every CLI renderer now speaks one visual language.** The conventions established in v0.12 for `ibkr account`, `ibkr positions --by underlying`, and the `ibkr chain` expiry list have been propagated across the surface: `ibkr positions` (flat stocks/options tables) splits into `renderStocksTable` + `renderOptionsTable` with dim column-header + rule and right-aligned money columns; the `ibkr chain --expiry` strike grid bolds the ATM strike (the single hero per grid) and dims the group/column headers; `ibkr quote` snapshot and `--watch` headers gain the dim header + rule; `ibkr history`, `ibkr scan <preset>`, `ibkr scan list`, and `ibkr scan params` all adopt the same column-header treatment; `ibkr size` right-aligns money to a single column edge and bolds Shares (the sizing tool's whole answer); `ibkr status` extracts a `renderStatusText` so the preview tool can reach it, colours the state suffix by severity (degraded → yellow ⚠, starting → dim, ok → plain), bolds the `Connected:` value, and dims the daemon-log hint. The `formatRange52w` helper had a dead branch (both arms returned the same dim-wrapped string) that's been collapsed. No flag changes, no wire-shape changes, same data, no fabrication of zeros where data is missing — em-dashes everywhere a value couldn't be captured. The `cmd/_preview` tool gains screens for every newly-styled view so a single `go run cmd/_preview/main.go all` capture demonstrates the full visual language; the chain strike-grid caption was also corrected (was about "Greeks" but the view only shows IV).

- **Smaller binaries.** `make build` and `make release-binaries` now pass `-s -w` to the linker, which strips the external symbol table and DWARF debug info. The `bin/ibkr` artefact drops from 9.6 MB to 6.5 MB on darwin/arm64 (~32%); release tarballs shrink proportionally. What's traded away: external debuggers (`delve`), `go tool nm`/`objdump`, and third-party profilers can no longer symbolicate the binary. Go panic stack traces remain fully readable — the runtime carries its own function metadata, separate from the symbol table. Startup time is unchanged; this is purely a size optimisation. Same convention used in Docker, Kubernetes, and most production Go binaries.

## v0.12.2 — 2026-05-12 22:05 CEST

Fix release. Five defects from a code-review pass on v0.12.1, all small, all with regression tests in the same change. No flags removed, no wire shapes broken, no behaviour change for existing successful calls — the changes either close a silent leak, harden the daemon against hostile/buggy clients, or move CI to match the README's gating promise.

### Fixed

- **`cancel` RPC actually cancels now.** `rpc.MethodCancel = "cancel"` was declared and `handleQuoteSubscribe` carefully registered each stream into `s.streams[req.ID]` so a peer could cancel it — but the daemon's dispatcher had no case for the method, so every `cancel` request came back `unknown_method` and the `subEntry` refcount stayed held until the client's TCP socket EOFed. For long-lived MCP clients that subscribe many resources, that's the difference between releasing IBKR market-data slots on-demand and burning them until the session ends. Added the dispatcher case plus `handleCancel` (rejects empty/unknown ids with classified `bad_request` — silent success would mask client-side bugs). Two regression tests cover the happy path and the unknown-id path. Live smoke against the running daemon now returns `{"ok":false,"error":{"code":"bad_request","message":"no active stream with id …"}}` for unknown ids instead of `unknown_method`.

- **`UnsubscribeMarketData` is case-insensitive.** `SubscribeMarketData` upper-cases the symbol before storing it in `c.subscriptions`; `UnsubscribeMarketData` did not, so `Unsubscribe("aapl")` after `Subscribe("aapl")` was a silent no-op — the IBKR-side `reqMktData` line stayed open and ate one of the ~100 subscription slots until the connection bounced. Hits anyone forwarding user-typed symbols straight through without pre-normalising. One-line fix in the library plus a regression test (`TestUnsubscribeMarketData_CaseInsensitive`) that pins the contract.

- **Bundled settings now allow `ibkr size`.** `settings/ibkr.settings.json` listed every read-only verb in `permissions.allow` except `Bash(ibkr size*)` — the position-sizing helper that shipped in v0.11. Users who copied the file into `~/.claude/settings.json` (a path the README explicitly recommends) got a permission prompt every time they ran `ibkr size`. The SKILL.md frontmatter had it; settings did not. One-line addition.

### Security

- **Daemon survives handler panics and oversize frames** — closes two latent denial-of-service surfaces on the Unix-socket RPC server. `serveConn` used `bufio.ReadBytes('\n')` with no upper bound, so a peer sending a newline-free megabyte would grow the read buffer until OOM. The dispatcher had no panic recovery, so a `json.Marshal(NaN)` or any other handler panic would unwind through the per-connection goroutine and disconnect every other client sharing the listener. Added a 1 MiB per-frame cap (`readBoundedLine` + `errFrameTooLarge`) — well above any real CLI/MCP payload — and a `defer recoverHandler(...)` in `dispatch` that converts a panic into a classified `internal` error on the request's own id, with the full stack trace landing in the daemon log for postmortem. Five regression tests: two unit tests on the bounded reader (rejects oversize, accepts at exactly the cap), two on the recover helper (writes error response, tolerates nil request), one end-to-end on `serveConn` that pushes a 2 MiB blob through a `net.Pipe` and asserts a classified `bad_request` response without OOM or hang.

### Changed

- **CI now invokes `make check && make test`, not an inlined re-implementation.** The README labels `make check` and `make test` as the binding gates — but CI re-implemented gofmt/vet/staticcheck/govulncheck inline and skipped `plugin-check` and `parity-check` entirely. The MCP↔CLI drift test (`parity-check`) ran only by side effect of `go test ./internal/...`; the plugin-manifest validation never ran in CI at all. The new CI workflow shells out to `make check CHECK_DEPS=parity-check`, `make test-pkg`, and `make test-daemon`, with `CHECK_DEPS` introduced in the Makefile as the documented escape hatch for environments without the `claude` CLI on PATH (the parity gate stays strict). `make check` and `make test` are now single-source-of-truth gates: a contributor's local run is the same gate CI applies. Test timeouts in `test-pkg` and `test-daemon` bumped to match the previous CI values (180 s / 240 s / 420 s) so the consolidation doesn't tighten anything CI was depending on.

## v0.12.1 — 2026-05-12 21:36 CEST

Bug-fix release. The headline feature from v0.10.0 — per-leg option Greeks on `ibkr positions` — has been quietly broken since the IBKR gateway rolled forward to server version 165 or later. The handler that parses the model-computation tick was reading the wrong field as `reqID`, so every Greek the gateway sent landed on a key nobody was looking up, and `greeks_coverage` came back 0/N on every call. That's fixed here, alongside two related issues that compounded the symptom and one zombie-position bug. Three new optional fields show up on each option row in JSON: `option_bid`, `option_ask`, `option_prev_close`, plus `iv`. No flags or wire shapes were removed; existing consumers see the same output plus those four optional fields.

### Greeks now arrive and get routed to the right contract

`pkg/ibkr.handleOptionComputation` decoded tick 21 (model-computation Greeks) using offsets that only matched the pre-server-version-165 wire layout — `fields[2]` was treated as `reqID`, `fields[3]` as `tickType`. Modern gateways send `[msgID, reqID, tickType, tickAttrib, impliedVol, delta, optPrice, pvDividend, gamma, vega, theta, underlyingPrice]`, so the handler was reading `tickType` (10/11/12/13) as the reqID. That ID isn't in the connector's option-request map, so the handler exited early and the Greek values fell on the floor. Result: `greeks_coverage: 0` on every option-bearing positions call, including the agent-feedback example. Fixed by reading `reqID` at `fields[1]` and `tickType` at `fields[2]`. New regression test in `connector_greeks_test.go` (`TestHandleOptionComputationWireOffsetIsAtFieldOne`) pins the wire offset so a future revert can't silently re-introduce the bug. Existing handler tests were updated to use the modern layout — under the old layout the test rows happened to align by coincidence, which is part of why the bug shipped.

### Held options skip the slow contract round-trip

`SubscribeOption` resolves each option's ConID via `reqContractData` before requesting market data. That round-trip can take a few hundred ms when the gateway is warm — but under load it sometimes silently times out, and the code then tries a second exchange, eating the full 10 s before giving up. For a 13-leg book that wipes the 30 s positions deadline before Greeks can be requested at all. The fix is honest: `msgPortfolioValue` already carries the full contract spec (ConID, exchange, trading class) for every held position, so the daemon now seeds the option-contract cache from portfolio data as it arrives. `SubscribeOption` hits cache for held positions and skips the round-trip entirely. New test `TestHandlePortfolioValueSeedsOptionContractCache` covers it.

### Per-leg option market data exposed in JSON

Each option row in the `positions` response now carries four new optional fields populated from the per-leg market-data subscription the daemon already opens for Greeks:

- `option_bid` / `option_ask` — the option contract's own bid and ask, not derived from the underlying. The mark sits between them. When the spread is wide on illiquid contracts (RDDT $185C, GME $30C), the mark may not be tradable — these two fields are how callers can tell.
- `option_prev_close` — the option contract's own previous regular-session close (tick 9 from the option's own market-data feed). The existing `prev_close` field on option rows continues to carry the underlying's prev close for backward compatibility, which the agent feedback correctly flagged as confusing. The new field is the one to use for option-level day-over-day P&L.
- `iv` — the implied volatility from the model-computation tick, as a fraction (0.30 = 30%).

All four fields are nil-omitted when the subscription didn't capture them in the budget — no fabricated zeros.

### Delisted positions no longer inflate `effective_delta`

A held delisted ticker (ZVZZT plays the zombie in the test book) arrives via `msgPortfolioValue` with mark=0 — the gateway streams the position but rejects market-data subscriptions for it. On the first `positions` call after daemon start, the connector hasn't yet flagged the symbol inactive, so the zombie contributed its full share count (20 000 in the test book) to `effective_delta`. The second call would correctly exclude it once the inactive flag landed, so the same daemon reported wildly different effective deltas back-to-back. `buildPortfolioAggregates` now skips stocks with mark ≤ 0 from the aggregate; the position row still renders with mark=0, which is the honest answer. New test `TestBuildPortfolioAggregatesExcludesZombieStocks` covers it.

### Other notes

- `optionGreeksBudget` was 1.5 s; bumped to 2.5 s to give the gateway a comfortable margin once Greeks actually flow. Per-leg observed latency on the test book was 750–1100 ms in cache-warm conditions; 2.5 s leaves room without blowing the 30 s positions deadline (4-way parallel × 13 legs × 2.5 s worst case = 8.1 s).
- The wire-decode fix means `handleOptionComputation` now reads `fields[3]` as `tickAttrib` (option-computation flags). The field is parsed but not yet consumed; we'll wire it through to the renderer if it turns out to carry information worth surfacing.

## v0.12.0 — 2026-05-12 07:45 CEST

Four scanner-track changes: an ad-hoc scan path that doesn't require editing config, per-row enrichment (last / change / volume / IV / 52w instead of bare symbols), a fresh seven-preset default set validated against the live gateway catalog, and two hardening fixes (wire-frame cap, status/scan readiness). Plus a test-harness orphan-prevention fix for `make test`. Wire shapes back-compatible (the existing `last`/`change`/`volume` fields on `rpc.ScanRow` became pointers — `omitempty` drops nil same as zero). The default `[scans]` map changed — see the migration note below.

### Scanner subscription timeout bumped 20 s → 35 s; clearer error on cold-start

The wire-level scanner-subscription timeout was 20 s — fine during RTH, too tight off-hours when IBKR's scanner farm needs 25-45 s to warm up for the time-of-day-dependent scanCodes (HIGH_OPEN_GAP, TOP_PERC_GAIN, HIGH_OPT_IMP_VOLAT_OVER_HIST, HOT_BY_OPT_VOLUME). Bumped to 35 s. The timeout error text now says *"scanner subsystem did not respond within Ns (often a cold-start off-hours; retry in a few seconds, especially for HIGH_OPEN_GAP / TOP_PERC_GAIN / option-IV scans)"* instead of the previous "scanner timed out after Ns" so users know retry is the right response. Daemon `MethodScanRun` ceiling raised 30 → 75 s and the CLI per-invocation budget for `scan` raised 60 → 90 s so the daemon's classified error reaches the user instead of a socket timeout. The matching `Scan.Timeout` field in `config.toml` still overrides the default — useful for users who want to fail fast or wait longer per preset.

### `ibkr scan` rows now carry market data, not just symbols

IBKR's `reqScannerSubscription` protocol returns only `rank` + `symbol` per row (plus three free-text fields that are empty for the common scan types — verified at the wire level for `MOST_ACTIVE` and `HOT_BY_VOLUME` against server v203). v0.11 surfaced that bare leaderboard verbatim, which made the output essentially useless: a list of tickers with no way to tell whether they were up or down, liquid or illiquid, near 52-week highs or lows. v0.12 enriches each row by issuing parallel `Hold`-based market-data subscriptions in a bounded worker pool (20 concurrent × 6 s per-row window), then merging the resulting ticks back into the row before serialisation. Fields added to `rpc.ScanRow`: `last`, `prev_close`, `change`, `change_pct`, `volume` (compact K/M/B in the text renderer), `iv` (averaged option IV from generic tick 106 — fraction, 0.234 = 23.4%), `week_52_high`, `week_52_low`. The text renderer adds matching columns with green/red colour on `change_pct` and dim 52w range, identical width/colour conventions to `ibkr quote` so the eye doesn't have to re-train. Nil fields stay nil — no fabricated proxies, em-dash in the text renderer — which is the load-bearing read: off-hours, most ticks don't arrive, and the honest column is empty rather than misleading. Enrichment happens daemon-side so MCP / JSON consumers see the same enriched payload as the text renderer.

Plumbing: `pkg/ibkr/connector.go` `Subscription` struct gains `Week13/26/52Low/High` and `IV`; `handleTickPrice` switch extended for tick types 15-20; `handleTickGeneric` for tick 106 now also writes to the subscription (it previously routed only to the chain-IV cache); `MarketData` / `GetMarketData()` surface the new fields; the daemon's `subManager.Hold` now requests generic ticks `100,101,104,106,165` so the gateway actually delivers the new ticks (previously asked for `100,101,104` — IV and 52w were unreachable from the snapshot path). `MethodScanRun` unary deadline bumped from 30 s to 50 s to accommodate enrichment waves.

### `ibkr scan` — three new shapes

Until v0.11 the only way to run a scan was a preset by name, which forced anyone wanting to try a different `scanCode` / `locationCode` to first edit `~/.config/ibkr/config.toml` and restart the daemon. That hard-coded gate has been replaced with two new modes:

- **Ad-hoc:** `ibkr scan --type TOP_PERC_GAIN --exchange STK.NASDAQ --limit 25 [--json]`. No preset required. Rows are capped at 50. MCP tool `ibkr_scan` accepts the same `type` and `exchange` fields. Designed for agent workflows that need to compose a scan on the fly.
- **Catalog dump:** `ibkr scan params [--instrument STK] [--raw] [--json]`. Pulls IBKR's full `reqScannerParameters` XML, parses it, and returns the three lists agents need to compose a valid scan: `instruments` (e.g. STK / OPT / ETF), `locations` (every `locationCode`), and `scan_types` (every `scanCode` with display name + applicable instrument types). The catalog varies by gateway version and market-data permissions — never assume a `scanCode` exists without checking. `--instrument STK` narrows to stock scans; `--raw` attaches the full XML (~200 KB–2 MB) for the rare case where a field outside the parsed result is needed. MCP exposes this as `ibkr_scan_params`.

Preset mode is unchanged. The MCP `ibkr_scan` tool's empty-args branch (no `preset` / `type` / `exchange`) returns the preset list, same as before.

### New default preset set (replaces the four v0.10.x defaults)

Validated against live IB Gateway server-version 203 via the new `scan params` dump before being committed. The selection covers the screens an active US stock + options trader actually runs:

| preset             | scanCode                        | exchange      | rationale                                  |
|--------------------|---------------------------------|---------------|--------------------------------------------|
| `top-movers`       | TOP_PERC_GAIN                   | STK.US.MAJOR  | unchanged                                  |
| `top-losers`       | TOP_PERC_LOSE                   | STK.US.MAJOR  | symmetric counterpart (was missing)        |
| `most-active`      | MOST_ACTIVE                     | STK.US.MAJOR  | unchanged                                  |
| `unusual-vol`      | HOT_BY_VOLUME                   | STK.US.MAJOR  | unchanged                                  |
| `gappers`          | HIGH_OPEN_GAP                   | STK.US.MAJOR  | opening earnings/news reactions            |
| `high-iv-rank`     | HIGH_OPT_IMP_VOLAT_OVER_HIST    | STK.US        | replaces `high-iv`; IV vs own history is the option-seller signal — absolute IV always surfaces the same biotech/SPAC names |
| `unusual-opt-vol`  | HOT_BY_OPT_VOLUME               | STK.US.MAJOR  | the canonical "smart money flow" scan      |

**Migration note.** `high-iv` is gone, replaced by `high-iv-rank` with a different `scanCode` (`HIGH_OPT_IMP_VOLAT_OVER_HIST` vs. `HIGH_OPT_IMP_VOLAT`). Users who pinned `[scans.*]` blocks in their own `config.toml` are unaffected (the table is replace-not-merge — your file always wins). Users who relied on the built-in defaults will see the new set after upgrading; run `ibkr scan list` to view it.

### Wire-frame cap raised to 16 MB, stream-desync recovery hardened

`pkg/ibkr.readMessage` had a 1 MB cap on a single TWS message frame. The IBKR scanner-parameters XML on a US Pro gateway with options data is 1–2 MB. Hitting the cap was silent until v0.12: the previous read loop logged the error and `continue`d, which left the reader positioned mid-frame and turned subsequent body bytes into bogus length prefixes — one local repro saw 500 k+ "message too large" log lines in a few seconds before disconnect. Two surgical changes: (a) cap raised to 16 MB, well above any realistic IBKR frame; (b) any non-timeout / non-EOF read error now signals disconnection and exits the read goroutine, so reconnect logic can rebuild a clean stream rather than blindly continuing. Unit test in `pkg/ibkr/scanner_params_test.go` plus integration test `TestScanParamsReturnsCatalog` pin the cap behavior.

### `status.connected` now reflects `IsReady`, not `IsConnected`

The daemon was using TCP-level `IsConnected()` for `status.health.connected` but every data verb (quote, chain, scan, positions) gated on `IsReady()` — the post-handshake "handlers armed" state. When the connector landed in `{ready=false, conn=up}` (overnight TWS hiccup, market-data farm reconnect), `ibkr status` cheerfully reported `connected: true, data_type: "live"` while every other call returned `gateway_unavailable`. Worse, `triggerReconnect` only fired when TCP dropped — so the stuck state was unrecoverable without a daemon restart. Three lines changed: status.connected, `gatewayConnector`, and the early-exit guard in `triggerReconnect` all now consult `IsReady()`. Stuck-state recovery is now self-healing. Pinned by `TestConnector_IsReadyAndIsConnectedCanDiverge` in `pkg/ibkr/connector_ready_test.go`.

### Integration test harness no longer orphans daemons

`test/integration` spawned `ibkr daemon --foreground` without `Setpgid`. macOS doesn't propagate parent death; if `go test` was SIGKILL'd, timed out, or panicked before `TestMain` reached `stop()`, the spawned daemon stayed alive indefinitely. The harness now: (a) places the daemon in its own process group via `SysProcAttr.Setpgid`, (b) signals the whole group via `kill(-pgid, …)` in `stop()` so any future grandchildren die too, (c) installs a `signal.Notify` handler for SIGINT/SIGTERM that routes through `stop()` before `os.Exit`. SIGKILL is still unrecoverable — nothing we can do there — but every other interrupt path now cleans up. File now has `//go:build !windows` (Setpgid is Unix-only; the package was already Unix-only in practice).

## v0.11.0 — 2026-05-12 05:48 CEST

Two trader-math additions that fit the existing snapshot surface — both pure derivations from data the daemon already pulls. No new RPCs, no new gateway round trips, no journaling. The wire response shapes for `chain.expiries` and `size` carry new optional fields; consumers that ignore them work unchanged. Plugin tag and binary tag both move in lockstep.

### `ibkr size --target` adds R-multiple and breakeven win rate

`ibkr size` already returned the fixed-fractional share count from entry + stop + risk %. Pass an additional `--target` (long: `target > entry`; short: `target < entry`) and the response now carries:

- `r` — reward-to-risk multiple, `|target − entry| / |entry − stop|`. The standard discretionary filter; ≥ 2R is the common "this trade is worth the risk" threshold from Van Tharp / Minervini / O'Neil.
- `reward_quote` / `reward_base` — max gain at target, in trade-quote and account-base currency respectively (same FX treatment as `risk_*`).
- `breakeven_win_rate` — `1 / (1 + R)`, the strategy's break-even hit rate at this R. Reads "at R = 2 you need to be right 33.3% of the time to break even."

The text renderer adds a three-line "reward" block right after "Max loss at stop" when `--target` is supplied, and suppresses it otherwise so the no-target output stays identical to v0.10.x. Long/short asymmetry is enforced in `ComputeSize` (covered in `size_test.go::TestComputeSizeRMultiple`) so a fat-fingered target on the wrong side of entry gets a structured validation error, not a negative R. `ibkr_size` (MCP tool) carries the new optional `target` arg with matching schema.

### `ibkr chain SYM` adds DTE and 1-σ implied move per expiry

The expiry-listing path now decorates each row with two new fields:

- `dte` — calendar days from today (local) to the expiry. Same-day expiries get `dte = 0`.
- `implied_move` / `implied_move_pct` — the canonical 1-σ expected dollar move by expiration, computed `spot × IV × √(DTE/365)`. Same formula CBOE's option calculator uses; the desk-standard "what move is the market pricing in" number for earnings sizing and strike selection. Populated only when both spot and IV are known; nil otherwise — never a substituted proxy.

The result body also carries top-level `spot` (the mid the daemon used to pick the ATM strike — previously implicit). The text table grows two columns (`DTE`, `EXPECTED MOVE`) when IV is requested, or one (`DTE`) when `--no-iv` is passed. No new round trips: spot was already fetched once per call to pick the ATM strike; the math is pure post-processing on the existing IV data.

`schemas.md` and `SKILL.md` updated so Claude knows when to surface the new fields ("what move is priced into Friday?", "is this 2R trade worth taking?"). Tests in `internal/daemon/implied_move_test.go` cover the day-count helper and the formula against hand-computed references including the √(4×DTE) = 2× scaling property.

## v0.10.3 — 2026-05-11 22:17 CEST

Hardening pass after an end-to-end review: panic recovery on the wire reader, a non-atomic close() in the connection pool, a context leak in the rate limiter retry path, MCP subscription contexts now scoped to the server's lifecycle, and a new GitHub Actions CI workflow. Two minor cleanups round it out. No CLI flag changes; safe drop-in upgrade from v0.10.2.

### Panic recovery on the TWS reader goroutine

`Connection.readMessages` is the sole consumer of the gateway socket. Pre-fix, a panic inside any message handler (bad protobuf shape, unexpected wire field, downstream nil deref) silently killed the reader while the connection's status field still read `Connected` — every subsequent write queued forever waiting for a reply that no one was reading. The reader is now wrapped in a `defer recover()` that logs the panic with a full stack trace and converts it into a structured disconnect, so the existing reconnect-with-backoff loop takes over instead of leaving the process wedged.

### `ConnectionPool.Stop()` race fix

The pool's `Stop()` used a `select { case <-stopChan: default: close(stopChan) }` pattern that is not atomic with respect to a concurrent caller. Two goroutines hitting `Stop()` simultaneously could both observe the default branch and race into `close()`, panicking on the double close. Now guarded by `sync.Once` — `Stop()` is idempotent and concurrent-safe.

### Rate-limiter retry no longer leaks on shutdown

`RateLimiter`'s exponential-backoff retry goroutine slept on a bare `time.Sleep(backoff)` with no awareness of the limiter's context. A shutdown during the sleep left the goroutine running out the full delay before noticing — wasting work and delaying clean exit. The sleep now selects on `ctx.Done()` and the retry-enqueue also bails on cancellation. Tracked via the limiter's existing `sync.WaitGroup` so `Stop()` waits for in-flight retries.

### MCP resource subscriptions scoped to server lifecycle

`handleResourcesSubscribe` was creating its streaming context from `context.Background()`, which severed each subscription from the MCP client's lifecycle. If the client crashed without an explicit `resources/unsubscribe`, the subscription persisted until `shutdownSubscriptions()` happened to run — which it did on a clean EOF, but not on the process being SIGKILLed mid-stream. Subscriptions are now children of the `Serve()`-scoped context, so an outer context cancel (or the existing client-EOF path) reaches every active subscription deterministically.

### Tautological assertion removed

`Connection.sendMessage` re-decoded the four-byte big-endian length prefix it had just encoded and panicked if the round-trip disagreed — a check that cannot fire short of `encoding/binary` malfunctioning. Removed; the value was zero and it sat on the hot send path.

### CI: GitHub Actions workflow

Added `.github/workflows/ci.yml`. Three jobs run on every push to `main` and every PR: `check` (gofmt + go vet + staticcheck + govulncheck), `test` (matrix on `ubuntu-latest` and `macos-latest` — `pkg/ibkr` unit tests + `internal/...` and `test/integration/...` under `-race`; live-gateway integration tests skip cleanly with no gateway present), and `cross-compile` (full release matrix on `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`). The README now carries a CI badge.

### Modernized to Go 1.21+ sort idiom

`internal/cli/positions.go` switched from `sort.SliceStable` to `slices.SortStableFunc` with `cmp.Compare`. Same behaviour, type-safe comparator, and lines up with the rest of the codebase's `slices`-based usage.

### Reproducible-builds note in the README

The release pipeline already built with `-trimpath -buildvcs=false` and stamped version/commit/date via `-ldflags`. Surfaced this in the "Other install paths" section so a security-conscious user knows they can rebuild any tag and compare against the published `SHA256SUMS`.

## v0.10.2 — 2026-05-11 21:46 CEST

Pre-publish hygiene + a README that earns its first scroll. No behavioural changes; safe drop-in upgrade from v0.10.1.

### README rewrite

The landing page now leads with what the binary does rather than how it's structured: a faithful sample of the account snapshot (including the v0.10.0 currency-exposure block and v0.10.1 currency-aware money symbols), a `--json | jq` recipe, and a streaming `--watch` example. The features list surfaces the v0.9.x and v0.10.x additions that were buried before — option Greeks per leg, portfolio aggregates with FX sensitivity, daemon-side chain ATM-IV cache with phase-aware TTL, MCP resource subscriptions for streaming, sign-coloring with `NO_COLOR` / `IBKR_COLOR` opt-out. All flag names, JSON field paths, and rendered widths in the examples are checked against the actual code; no fabricated output.

### Disclaimer & trademarks block

Added an explicit "not affiliated with Interactive Brokers" notice naming the trademarks used nominatively, calling out that `pkg/ibkr` is a clean-room re-implementation that bundles no IBKR-distributed code, and reaffirming the no-market-data-redistribution / IBKR-Pro-required / AS-IS posture. Mitigates the standard third-party-client legal exposure before the project gets broader attention.

### Personal-path hygiene

Removed hard-coded `/Users/<user>/...` paths that had crept into three places: the `pkg/ibkr/connection.go` package doc (now points at IBKR's public GitHub mirror), the two `pkg/ibkr/testdata/generate_*.py` fixture generators (now read `IBPY_ROOT` from the environment with an actionable error when unset), and the macOS-shaped TWS process line in `internal/discover/process_test.go` (anonymised to `/Users/local/...`). No behavioural impact — the tests still verify the same substring match — just clean for outside contributors reading the repo.

## v0.10.1 — 2026-05-11 21:16 CEST

Bug-fix pass after user testing v0.10.0 against a live multi-currency account. Five issues — one critical (the `--watch` regression), three rendering, one Greeks pipeline gap.

### `ibkr quote SYM --watch` no longer exits after ~1s (CRITICAL)

The pre-flight version-skew check (v0.9.1+) sets a 1-second context deadline on the shared socket connection via `Conn.applyDeadline` and never resets it. When the next operation on the same `*Conn` was `Stream` (the watch path), the stale deadline fired ~1 second into reading frames; `net.ReadBytes` returned a timeout, the loop interpreted `ctx.Err()` (the watch's own ctx, still valid) as nil, and the CLI exited cleanly with no error message. Users saw a few rows then a silent shell prompt.

Fix: `Conn.Call` now clears the socket deadline (`SetDeadline(time.Time{})`) on return, success or failure, so the next operation starts with no inherited deadline. Regression test in `internal/dial/dial_test.go::TestCallDeadlineDoesNotLeakIntoStream` reproduces the original failure mode against a stub daemon and proves the fix.

### Maintenance margin restored

v0.10.0 added `$LEDGER:ALL` to the account-summary request, which shifted IBKR's `MaintenanceMarginReq` field from the bare base-currency form to the per-currency form. The parser was still asking for the long-form tag name; the result was a silent em-dash where the maintenance margin should have been. The canonical IBKR tag is `MaintMarginReq` (shorter); the parser now accepts both forms so neither the old nor the new wire shape loses the value.

### Currency exposure no longer duplicates the base currency

For a EUR base account the `currency_exposure` block also included an "EUR" row with FX=1.0 — duplicating the top-level totals and confusing the renderer. The base-currency row is now dropped explicitly when `base_currency` is known, with an `ExchangeRate==1.0` fallback for the pre-handshake state.

### Currency-aware money rendering

Account and currency-exposure rendering used a hardcoded `$` prefix everywhere, which read wrong for a EUR account. Money values now use `€`/`$`/`£`/`¥` symbols for the common cases and fall back to the ISO code prefix (e.g. `CHF 825.06`) otherwise. The portfolio `Dollar delta` and `FX sensitivity` lines drop the symbol entirely — the currency code is named on the same line, so prefix and suffix would be redundant.

### Option Greeks pipeline now actually subscribes

The v0.10.0 Greeks pipeline silently skipped every option position because `optionGreeksKey` rejected `SecType="OPTION"` — the long-form enum value that `convertIBKRPositions` stamps onto position views — accepting only the wire-level `SecType="OPT"`. Result: every account reported `greeks_coverage 0/N` for option-bearing books regardless of what IBKR delivered. The key builder now accepts both forms.

Positions request deadline bumped from 10 s to 30 s so cold-cache option contract resolution (4-way parallel × up to 5 s per leg) fits within budget. After the option contract cache is warm, subsequent positions calls return under 5 s.

### Known follow-up: Greeks pipeline serialization (v0.10.2)

Even with the fixes above, only the first option in a multi-leg book currently completes its model-computation tick capture within the call — the other legs appear to serialize behind an internal rate limiter or contention point in `pkg/ibkr`. Aggregate effective delta and dollar delta still benefit from the cached values that DO land between invocations, but `greeks_coverage` lags reality. Tracked for v0.10.2; needs deeper instrumentation of the option-subscribe path.

## v0.10.0 — 2026-05-11 19:58 CEST

Two items from an MCP-surface audit: option Greeks per leg + portfolio aggregates, and FX exposure attribution for non-base currency holdings. No order placement, no new IBKR entitlement required.

### Option Greeks per leg + portfolio aggregates

`ibkr positions` now returns delta, gamma, theta, and vega per option leg, plus a `portfolio` block summed across legs:

- `effective_delta` (share-equivalents — long calls contribute +qty×delta×100, etc.)
- `dollar_delta` (effective delta × underlying spot, in `dollar_delta_currency`)
- `daily_theta` (IBKR reports theta as daily decay, so the sum is the daily P&L bleed assuming everything else holds)
- `gamma`, `vega`
- `greeks_coverage` / `greeks_total` so partial-coverage state is explicit when the gateway didn't model some legs (typical for far-OTM / illiquid OOH)

The Greeks come from IBKR's model-computation tick (msg 21, tickType 13) that already arrives on every option subscription — pre-fix the daemon parsed the row and discarded everything except IV (`_ = vega` was literally in the source). A new daemon-side cache (60 s TTL) means back-to-back `ibkr positions` calls within a decision pause pay zero extra gateway round trips; the cold first call adds ~3-5 s for a typical option book at 4-way parallelism. Same bounded fan-out pattern as the chain ATM-IV fetch.

`ibkr chain SYM --expiry YYYY-MM-DD --json` now also populates `call_delta` / `put_delta` per strike — the wire fields existed but were unused.

Nil discipline preserved: a leg whose model tick didn't arrive within budget shows `null` for each Greek, never a zero substitute. The renderer flags partial coverage on the summary line.

### Currency exposure for multi-currency accounts

`ibkr account` now returns `currency_exposure`: one row per non-base currency holding, with `net_liquidation_ccy`, `cash_ccy`, `stock_market_value_ccy`, `option_market_value_ccy`, `unrealized_pnl_ccy`, `realized_pnl_ccy`, `exchange_rate` (base-per-CCY), and `net_liquidation_base` (reconciled within 0.5%). For a EUR account holding $90k of USD positions, the row makes clear how much of NLV is FX-exposed.

`ibkr positions` decorates each non-base position with `fx_rate` and `market_value_ccy`. The `portfolio` block adds `fx_sensitivity_per_pct` — the base-currency P&L change per 1% FX move (Σ non-base NetLiq × FX × 0.01). That answers "how much of my book is currency-exposed" in the actionable form; it is **not** historical FX-vs-underlying P&L attribution (which would require per-lot execution-time FX tracking — a v2 feature).

Implementation: the daemon now asks for `$LEDGER:ALL` alongside the existing tags, which makes IBKR emit one block per currency. The data was always available — we just weren't requesting it. No extra round trip on `ibkr positions`: the per-currency snapshot is read from the connector's continuously-fresh `reqAccountUpdates` state.

## v0.9.3 — 2026-05-11 16:14 CEST

### Discovery now fails over to alternate ports

When both IB Gateway and TWS are running on localhost, the daemon used to pick whichever responded to the TCP probe first (4001 → 4002 → 7496 → 7497) and stop there. If that app's API socket accepted TCP but never completed the IBKR handshake — the textbook signature of "Gateway is up but not logged in" or "checkbox unchecked" — the daemon stayed degraded indefinitely, even though the other app was sitting in `Endpoint.Alternates` ready to talk.

The connect path now walks the primary endpoint *then each alternate* in preference order. Each candidate gets a hard 25s budget — long enough for a healthy slow handshake (~sub-second to ~20s) but short enough that the loop reliably advances even when the SDK's TLS retry would otherwise hang against a black-hole peer. The first candidate to complete the handshake wins; the alternates that responded to TCP but never handshook are torn down cleanly between attempts. On exhaustion, `ibkr status` shows a verdict that names every endpoint that was tried — not just the original probe winner — so the user knows where the real problem is.

```
ibkr daemon v0.9.3  ·  uptime 24s  ·  degraded — gateway not connected

  Reason: none of 2 discovered endpoint(s) completed TWS handshake
          (tried 127.0.0.1:4001, 127.0.0.1:7496); confirm the IBKR app
          you intend to use has 'Enable ActiveX and Socket Clients' on
          and is logged in
```

User-visible effect: a stale Gateway window left over from earlier in the day no longer blocks `ibkr` from picking up the active TWS session.

## v0.9.2 — 2026-05-11 15:39 CEST

### Daemon no longer deadlocks on shutdown

`Connector.Stop` was holding the connector's mutex across `pool.ReleaseLease()`, which fires the registered `onDisconnect` callback synchronously. That callback calls back into `onConnectionLost`, which tries to lock the same mutex — deadlock. Effect: every daemon idle-shutdown (and every SIGTERM) hung the daemon process indefinitely; you had to `kill -9` it. Now `c.running` is flipped to false and the lock is released *before* the lease releases, so the disconnect callback can acquire the mutex cleanly. The user-visible win is that idle daemons actually exit when they say they do.

### Autospawn handles shutdown-race gracefully

The v0.9.1 pre-check refused to spawn a duplicate daemon when the lock file pointed at a live PID. That was correct for the genuinely-stuck case but misfired on the legitimate shutdown window: the daemon's `Stop` sequence removes the socket *before* releasing the lock, so a CLI invocation arriving mid-shutdown saw "PID alive + lock present + socket gone" and emitted a misleading "PID is stuck, run `kill X`" error. Fixed: the pre-check now polls PID liveness alongside socket appearance. When the daemon finishes exiting during the wait, the CLI falls through to spawn a fresh daemon automatically. Only a daemon whose PID stays alive through the full budget surfaces the stuck-daemon error.

Combined with the deadlock fix above, the user experience is: after idle-shutdown (or any SIGTERM), the next CLI invocation transparently spawns a fresh daemon — no manual intervention, no confusing errors. The whole round trip is under 5 seconds.

## v0.9.1 — 2026-05-11 14:03 CEST

### Better chain timeout error

`ibkr chain SYM` now surfaces a useful, action-oriented message when the IBKR security-definition data farm is degraded (typical pre-market / post-maintenance state):

```
ibkr: chain: timeout: option chain unavailable for AMD: gateway did not deliver
  security definitions in time. This is usually transient — try again in a
  moment, or run `ibkr status` to verify the gateway connection.
```

Replaces the previous generic `ibkr: chain: internal: timeout waiting for contract details`, which left users guessing whether it was a daemon bug, an invalid symbol, or something they could retry. Other surfaces classify the underlying timeout (`ibkrlib.ErrContractDetailsTimeout`) as `rpc.CodeTimeout` instead of falling through to `CodeInternal`, so JSON consumers get a meaningful error code too.

### Single-daemon enforcement, harder

Autospawn now refuses to spawn a duplicate daemon when the lock file points at a live PID — it either connects to the existing daemon's socket when it appears, or surfaces a clear "daemon PID X is running but never opened the socket" error with a `kill X` hint. Pre-fix, racing CLI invocations would each spawn a daemon process; most exited cleanly via the existing flock contention, but a deleted lock file (manual `rm`, aggressive cleanup script) could let two daemons co-exist with two gateway connections fighting over the same client ID. The flock layer remains the final defense — this just stops us from making the race in the first place.

### CLI ↔ daemon version drift warning

Every CLI invocation (other than `status`) now runs a fast `status.health` round-trip after connect and prints a stderr warning when the daemon was built from a different revision than the CLI binary:

```
ibkr: warning: CLI version v0.9.1 does not match daemon version v0.9.0 —
  restart the daemon to pick up the new binary (kill the running ibkr daemon;
  the next CLI call will respawn it).
```

The warning is silenced when either side stamps the literal `dev` placeholder so working-tree builds don't nag against themselves. The check uses a 1-second timeout and silently skips on any RPC failure — it must never interfere with the user's actual command.

## v0.9.0 — 2026-05-11 12:58 CEST

### Quote & positions show "change vs prev close"

`ibkr quote` now renders three new columns: **PREV CLOSE**, **CHG**, **CHG%** — the daily anchor every retail platform shows by default. The fields land in JSON as `prev_close`, `change`, `change_pct`. Pre-market, where regular-session ticks may not be flowing yet, prev-close arrives reliably so the user sees "yesterday closed at 455.19, no live print yet" instead of a row of em-dashes.

`ibkr positions` gains a **DAY CHG** column showing `±$X.XX (±Y.YY%)` between the position's mark and the underlying's prev close — separates today's move from cumulative P&L. The daemon pre-warms a per-symbol prev-close cache (12 h TTL) on the first call, so subsequent invocations are instant. JSON gets `prev_close`, `day_change`, `day_change_pct` on each `PositionView`. Options' `PrevClose` reflects the underlying's prev close (anchor only — contract-level option prev close is not tracked).

All new columns paint green/red by sign with em-dash placeholders when source data is missing — no fabrication, never substitute a proxy.

### Chain expiry list now shows ATM IV by default

`ibkr chain SYM` (no `--expiry`) now fetches and renders ATM implied volatility per expiry **by default**, so the answer to "which expiry has the richest premium?" appears without an extra flag. Three behaviours are new:

- **Default cap of 12 nearest expiries.** A typical equity lists 25–40 expiries; the back half (LEAPS) is rarely on the decision path. The renderer's footer flags when the cap was applied and points at `--all-expiries` to expand.
- **Daemon-side cache.** Per-(symbol, expiry) IV memoized with phase-aware TTL: 60 s during RTH (9:30–16:00 ET, weekdays), 4 h otherwise. First call pays the round trip; subsequent ones within the TTL are instant — and survive across CLI invocations because the daemon is persistent.
- **Parallel ATM IV fetch.** 4 concurrent workers (matches the chain strikes loop) reduce the typical fan-out from ~30 s sequential to ~5 s parallel.

Flag changes:
- `--with-iv` is gone — IV is now the default.
- `--no-iv` added for the fast skeleton (date list only).
- `--all-expiries` added to lift the default cap.

MCP `ibkr_chain` tool: `with_iv` is gone, replaced by `no_iv` + `all_expiries` JSON args (both default false, both opt-in).

### Chain strikes table now shows IV pre-market and after hours

The strikes-table view (`ibkr chain SYM --expiry YYYY-MM-DD`) used to leave the **IV** column blank for most legs when bid/ask/last weren't flowing — typical pre-market and after-hours. Two fixes:

- **`SubscribeOption` now explicitly requests generic tick 106** (Option Implied Volatility), mirroring what `SubscribeOptionIV` already does. Without 106 the strikes table relied on opportunistic model-computation ticks, which only arrive when the book is recomputing.
- **The IV poll runs regardless of whether prices arrived.** Pre-market, a dead option book has no quotes but IBKR can still deliver IV via the model-computation path — the previous code's "only poll IV if pricesArrived" guard threw those away.

## v0.8.2 — 2026-05-11 09:17 CEST

### Color-coded output

Tables now paint sign-meaningful values when stdout is a terminal: P&L green for gains, red for losses, dim for zero. `ibkr quote --watch` colors each Last tick green/red/dim by direction vs. the previous tick. `ibkr account` paints negative balances (cash debit) red and zero placeholders dim — positive balances stay uncolored to keep balance views from looking celebratory. Non-live data badges (`data=delayed ⚠`, `data=frozen ⚠`) and the `ibkr size` `⚠ status:` warning render in yellow.

Color is opt-out: pipes, file redirects, and `--json` are always plain. `NO_COLOR=1` disables; `IBKR_COLOR=always|never` overrides. Top-level help advertises both env vars.

### Column alignment fixes

Quote, positions, options, history, and chain tables now line up labels precisely over their data — across both populated and empty cells. The em-dash placeholder used for missing values now matches the configured column width visually (the bug: `—` is one terminal column but three UTF-8 bytes, so `%Ns` byte-count padding shifted downstream columns left whenever a value was nil). Table headers are now generated from the same field widths as the data row, so any future width tweak only edits one verb instead of a hand-spaced label string.

### Better help on a typo

A mistyped subcommand (`ibkr quotee`) now prints the full top-level usage to stderr instead of just the bare error line — matches the git/kubectl/gh pattern. The top-level help itself has a new footer pointing at `ibkr <subcommand> --help` for per-command flags.

## v0.8.1 — 2026-05-11 08:07 CEST

### Faster, friendlier "where's my gateway?" failure

When the daemon can't reach an IBKR endpoint, the error now names the real cause instead of timing out generically. Two cases, two hints:

- **TWS / IB Gateway / IBKR Desktop is running but the API socket isn't open** — the daemon detects the process and tells you so, with the PID. Most likely 'Enable ActiveX and Socket Clients' is unchecked under Global Configuration → API → Settings, login hasn't completed (2FA / day-end dialog), or you set a non-default Socket port and need to pin it in `~/.config/ibkr/config.toml` under `[gateway]`. The API checkbox is known to silently un-tick itself when more than one of TWS / Gateway / Desktop is launched against the same login.
- **No IBKR app is running at all** — the daemon says so directly. Start one and the daemon reconnects automatically; no daemon restart needed.

`ibkr status`'s degraded-state block is now a single line pointing at the daemon log; the verdict itself goes in `Reason:`. The reconnect loop's `WARN` line is now emitted once per distinct verdict instead of every ~500 ms while `ibkr status` polls.

### Strict TOML config

`~/.config/ibkr/config.toml` is now parsed strictly: unknown top-level keys or section names cause the daemon to fail at startup with a message that names the offending keys. Previously the TOML library silently dropped unknown sections, so a stale-schema config (e.g. one using `[profiles.live]` from an older proposal) parsed cleanly but every `[gateway]` field stayed `nil` — the daemon then fell back to AUTO discovery with `client_id = 15`, masking the misconfiguration. Supported schema is unchanged: `[gateway]`, `[daemon]`, `[scans.<name>]`.

## v0.8.0 — 2026-05-11 07:56 CEST

### MCP streaming subscriptions

Live streaming quotes are now an MCP resource. Two URI templates:

- `ibkr://quote/{symbol}` — stocks / ETFs
- `ibkr://option/{symbol}/{expiry}/{right}/{strike}` — option contracts (`expiry` is `YYMMDD`, `right` is `C` or `P`)

`resources/templates/list` advertises both. `resources/read` returns the current snapshot in a single text content block; `resources/subscribe` delivers coalesced ticks via `notifications/resources/updated`, with the JSON frame embedded in `params.contents`. Unsubscribe explicitly via `resources/unsubscribe`, or close the MCP server's stdio — the subscription drops either way.

No transparent reconnect: a gateway disconnect, daemon shutdown, or IBKR rejection emits a structured terminal frame (one of `gateway_lost`, `entitlement_lost`, `subscription_rejected`, `daemon_shutdown`) and closes the subscription. The MCP client decides whether to re-subscribe.

### Daemon-internal subscription fan-out

The daemon now refcounts market-data subscriptions above the `pkg/ibkr` layer. Two `quote --watch AAPL` watchers, an MCP subscriber, and a snapshot poll on the same symbol now share **one** IBKR market-data line — pre-`v0.8.0`, the second concurrent subscriber would error with "already subscribed" or silently truncate the first. The line is released the moment the last subscriber goes away.

Wire-protocol-additive: `rpc.Frame` gains an optional `error` field (`omitempty`). Tick frames look the same as before; a frame with `error` populated is the last frame on the subscription. Older parsers that ignore unknown fields keep working.

### Other

- After upgrading the binary, restart any long-running daemon (`pkill -x ibkr`, then re-invoke any subcommand) — the daemon's subscription-state shape changed and the daemon-restart-on-upgrade rule from the README applies.
- `internal/mcp.ExcludedCLI` no longer carries a `quote` entry: streaming `quote --watch` is now a real MCP surface gated by `TestStreamingParity`.

## v0.7.0 — 2026-05-10 22:21 CEST

### Surface

- CLI subcommands: `account`, `positions`, `quote`, `chain`, `history`, `scan`, `size`, `status`, `setup`, `version`, plus the system subcommands `mcp` (stdio MCP server) and `daemon` (long-running gateway connection). Every user-facing command supports `--json`.
- Stateful daemon (same binary, `ibkr daemon`) auto-spawned on first call, idle-shuts after 5 minutes.
- Auto-discovery across the four standard IB Gateway / TWS ports (4001/4002/7496/7497), with strict pinning when configured.
- Two-command install (`install.sh` + `ibkr setup claude-desktop`) for the common case. `go install`, manual tarball, or local build for everything else.

### Safety

Read-only by design. Four independent layers refuse `order`, `trade`, `cancel`:

1. The daemon's order-handler dispatch is stubbed via `//go:build !trading`. `MethodOrderPlace` and `MethodOrderCancel` always return `ErrTradingDisabled`.
2. The bundled `settings/ibkr.settings.json` denies the verbs in `permissions.deny`.
3. The plugin's `PreToolUse` hook hard-blocks the verb patterns and fails closed if `jq` is missing from PATH.
4. A unit test in `internal/mcp` refuses to ship the MCP server with any tool whose name contains `order`, `trade`, `cancel`, `submit`, or `place`.

`pkg/ibkr` exposes order types for forward compatibility, but no CLI subcommand reaches them. A future major release may add trading behind an explicit build tag.

Per [semver](https://semver.org/#spec-item-4), 0.x releases may break compatibility between minor versions. 1.0 is reserved for the first stable read-only line.
