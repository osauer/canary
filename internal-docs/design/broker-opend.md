# Futu OpenD as a second broker

Updated: 2026-11-08 21:01 CET (the regime and portfolio stress run on OpenD);
earlier 2026-11-04 12:55 CET (after four cold reviews on 2026-11-04,
[reviews/2026-11-04-broker-opend-design-review.md](../reviews/2026-11-04-broker-opend-design-review.md))
Status: proposed design, awaiting the owner's ack of three decisions (the
last section). Owner decision 2026-11-03 09:49 CET: Futu OpenD becomes the
second broker. Nothing is built. This note is the technical design and the
contract the build follows; the build brief is
[broker-opend-build-brief.md](broker-opend-build-brief.md).

Facts about Canary are measured at `84e99e6a` on 2026-11-04, with the
commands given beside them. Facts about OpenD come from Futu's public
documentation (read 2026-11-03 and 2026-11-04) and from the Apache-2.0
protocol definitions in the public `py-futu-api` repository
(`futu/common/pb/`, 77 `.proto` files, read 2026-11-04). The build confirms
each wire fact against those definitions before it is coded; the section
"Confirm at build" lists the ones that matter. No OpenD exists to test
against yet, so the connector is built and proven against the simulator in
this note; conformance against a real OpenD begins with the first capture.

## What changes, in one paragraph

The daemon keeps one broker per process. A `[broker] kind` setting selects
`ibkr` (the default, today's behaviour) or `opend`. A new `internal/broker`
package names the reads every broker must serve: session health, account,
positions, quotes, bars and instrument lookup. `pkg/ibkr` is adapted to it
first, with no change in behaviour for IBKR: the daemon's own tests, the
regression spine and the next live session prove it, and the golden render
tests are a side check that no rendered byte moved. A new `pkg/futu` is the
hermetic OpenD wire client in the shape of `pkg/ibkr`, with an OpenD
simulator beside it. The cross-surface contract in `internal/rpc` gains a
broker-neutral instrument identity as additive fields. Everything
IBKR-specific stays IBKR-specific and says so on every surface when the
daemon runs on OpenD.

## The seam today, measured

An earlier measurement (2026-11-03, at `12601c2a`) is the baseline this
design was asked to verify. The figures below are from `84e99e6a`; where the
earlier figure differs it is in parentheses. Counting method: non-test files
are `ls internal/daemon/*.go | grep -v _test.go`; a reference is a match of
`\b(ibkrlib|ibkr)\.[A-Z][A-Za-z_]*` in those files (both import aliases,
exported identifiers only); a distinct identifier is the same match with the
alias stripped.

| Fact | At `84e99e6a` |
| --- | --- |
| Non-test Go files in `internal/daemon` | 330 (earlier: 344) |
| … that import `pkg/ibkr` | 134 (earlier: 132); 300 of 757 once tests count |
| … that name the concrete `*ibkrlib.Connector` | 43 (earlier: 41) |
| References to exported `pkg/ibkr` identifiers; distinct identifiers | 1,388; 181 (earlier: about 1,400) |
| Exported `Connector` methods; called by name from the daemon | 160; 130 (a name match, an upper bound) |
| Packages outside `internal/daemon` that import `pkg/ibkr` | none |
| Interface types in `internal/daemon`; slices of the connector | 31; 19 |
| Non-test daemon files that use `brokerStateScope` | 81 |

The narrow interfaces hold at the same lines as before: the market connector
in `subs.go:38`, the chart reader in `market_data.go:193`, the setup bar
source in `setups.go:19`, the setup resolver in `setup_resolve.go:13`, the
chain expiries connector in `chain.go:328`. They are typed on `pkg/ibkr`
structs (`Contract`, `MarketData`, `ChartSeries`, `HistoricalBar`,
`ConnectorSessionBinding`, `ResolvedOrderContract`). Two interfaces are
already broker-neutral: the breadth bar fetcher in
`internal/breadth/spx/fetcher.go:45` and the chain recorder's broker in
`internal/chainrec/run.go:14`; the recorder's `Load()` is the shape the quota
reporting below reuses.

Account summary and positions have no interface at all: the account snapshot
takes the concrete connector (`account_authority.go:194`) and the positions
list reads `CachedPositionsWithHealth()` on it (`handlers.go:437-467`),
then reports the read to data health through `observeAccountRPC`
(`data_health_rpc_observation.go:11`), which also takes the concrete
connector and binding. The daemon's server holds the concrete connector in
`server.go:174` and hands it out through `gatewayConnector()`
(`server.go:2805`), which also triggers the reconnect. The paper-or-live
classification keys on the IBKR account prefix and, before identity
arrives, on the IBKR port (`trading_status.go:433`). The account-and-mode
scope every scoped state document is keyed by is `brokerStateScope{Account,
Mode}` in `broker_evidence.go:207`; its two fields are written into
persisted keys and fingerprints in 81 files, among them the queued-order
authorisation terms (`owner_order_queue.go:223`), broker permissions
(`broker_permissions.go:63`), proposal and opportunity snapshots, desk
executions and the Edge scope fingerprint.

Two corrections to the earlier measurement. Exchange calendars already
exist for Singapore and China A-shares: `internal/marketcal` covers nine
markets, Hong Kong, Singapore, Tokyo and the A-share market among them, so
no calendar work is in scope. And `canary status` prints its TWS rows at
`internal/cli/status.go:89-98` and the API protocol rows at `104-106`, not
at 91-105.

Discovery (`internal/discover`) probes the four IBKR ports and names the
IBKR process (`process.go`); it knows nothing else. `internal/rpc` carries
IBKR identifiers throughout: `ContractParams` (`rpc.go:238`) is ConID,
SecType, SMART-style exchange and a `Market` of `us | de`; `PositionView`
(`rpc.go:1713`) carries ConID, exchange and primary exchange; the MCP
schemas describe `con_id` as the exact broker contract id.

Surfaces outside the daemon and the CLI also name the broker: the app's
push-alert copy (`internal/app/alerts/presentation.go:63-67`, "TWS
connection issue") and the SPA (`web/app/strategies.js:416-419`,
`web/app/opportunities.js:310`).

## The ownership rule, unchanged

The daemon owns the broker session, observed state and every write gate;
CLI, MCP and the app adapt typed contracts. A second broker changes which
process the daemon talks to, not who decides. In particular:

- Broker writes stay behind the agent-origin gated CLI with an explicit
  transaction-specific instruction in the current turn. Nothing in this
  design adds a write path, an MCP write tool or an app route.
- The preview token keeps its meaning: a daemon-signed bearer token for a
  later place flow, never submit eligibility.
- `[trading].mode` stays the owner's declaration of `disabled`, `paper` or
  `live`. The broker reports which environment an account is; the daemon
  checks the two agree before a write.
- Canary never holds the trading password and never sends OpenD's unlock
  request (protocol 2005). The owner unlocks in OpenD.
- Broker free text (`retMsg`) is data: it is quoted on the surface and
  never changes a decision or a state.

## One broker per daemon

`[broker] kind` selects the broker at daemon start: `ibkr` (default) or
`opend`. The daemon constructs one broker and one order path; a daemon never
holds both. Running IBKR and OpenD side by side is two daemons with separate
XDG roots and sockets, which is what "Running more than one instance" in
the architecture handbook already requires. The product identity's `ibkr`
namespace for the socket, lock and state directory is a pin and stays; it
names the product's history, not the broker.

The broker kind enters the state scope without touching a single IBKR key.
`brokerStateScope` gains `Kind`, and the zero kind means `ibkr`:
`sameBrokerScope` and every comparison normalise the empty kind to `ibkr`
first, and the kind is never written for IBKR. The daemon has 77
`brokerStateScope{` literals outside tests: 27 are empty and stay; of the
50 that name account and mode, about half build the current scope from
the session and go through `currentBrokerStateScope`, which carries the
kind, and the other half rebuild a scope from a persisted record to
compare it with the current one (`mandate_reduction.go:192`,
`policy_identity.go:64`, `exercise_reservations.go:109`,
`desk_override.go:107`, `proposal_queue.go:231` and their kind). Those readers rebuild from the record's body, so the body is where the
kind must live: every record body that carries an account and a mode (the
trade proposal snapshot the mandate-reduction engine holds, the proposal
and opportunity snapshots, the daemon-local exercise reservation and
resolution records, the queued authorisation terms, the desk authority
terms, the account-data receipt scope) gains the same `omitempty`
`broker` field, written only under `opend`, and the readers go through
one constructor that takes account, mode and kind from the body. One
reader parses a key instead: `coreDecisionEventMatchesScope`
(`core_rebalance.go:858-869`) splits the frozen core scope string on `|`
and takes its first segment as the account, so it goes through the
constructor's key form: in a `|` key the account field is cut at its first
`:` (an IBKR account never contains one; the fixture asserts it), and in a
`:`-joined key the segment after the family prefix is a kind when it
equals a known non-IBKR kind name, else the account. Twelve more
sites build a scope from an `rpc.TradingStatus`, which carries no kind
(`desk_execution.go:55`, `order_risk_authority.go:236` and `328`,
`exercise_reservations.go:141`, `protective_exit.go:402`,
`protective_oca_trading.go:37`, `tws_order_control.go:203`,
`delta_reduction.go:643`, `fill_quotes.go:676`, `broker_permissions.go:310`
and `409`); the status gains an additive `Broker` kind and those sites go
through a status form of the constructor. Under `opend` a stored record
then compares equal to the current scope; an IBKR record names no kind,
the constructor yields the zero kind, and the comparison normalises it.
Of the 81 files that use the scope, 23 non-test daemon files and
`internal/risk` write its fields into a persisted key, a fingerprint, a
journal line or a typed term; each keeps producing today's bytes for IBKR
and carries the kind only when it is not `ibkr`, in one of three shapes
per family; every helper is written for any non-`ibkr` kind, never for
`opend` by name, so a third kind adds no pass over these files (the
multi-broker review, 2026-11-04; the bytes under `opend` are the same). A string key carries the kind inside the account field, never
in front of a family prefix, because two readers parse by prefix first:
the frozen strategy scope `strategy|<id>|<account>|<mode>|<con>`
(`strategy_target.go:460`, `469`, `strategy_kill_live.go:157`), whose
reader strips `strategy|<id>|` before it splits, reads
`strategy|<id>|<kind>:<account>|<mode>|<con>`; the `:`-joined core
episode key (`core_rebalance.go:271`), whose reader at `281` trims
`core:` first, reads `core:<kind>:<account>:<mode>:<month>`; the broker
permissions, proposal and opportunity snapshots and desk executions keys
the same way. A hashed identity with a format validator takes the kind
inside the account it already hashes, only for a non-IBKR kind, so IBKR's input
and bytes are unchanged and no validator changes (`keyAccount`, the one helper
the string keys use too; WP8, 2026-11-04): the fee-rate fingerprint
(`market_events_fee_rate.go:1021`, `sha256:<hex>` validated at
`market_events_fee_rate_store.go:131-132` and `172-174`),
`risk.BuildAlertAuthorityScope` (`internal/risk/alert_candidate.go:309-342`),
`riskCapitalScopeKey` (`risk_capital_state.go:240-245`),
`recon_engine.go:273`, `cash_sweep_trace.go:82-83`, `dailyPnLScopeSource`
(`daily_pnl_close_capture.go:31-33`) and `edgeScopeFingerprint`
(`edge_authority.go:496-504`). A typed term that carries `AccountID` and
`AccountMode` gains the `omitempty` `Broker` field above, so an IBKR term
serialises as today. Four such terms exist: `rpc.DeskAuthorityTerms`
(which already has a `Kind` field naming the mandate kind, hence the name
`Broker`), `rpc.QueuedAuthTerms`, `rpc.DeskOverrideTerms`
(`desk_override.go:107`) and `rpc.LevelingBundleTerms`
(`proposal_currency_leveling_bundle.go:213`, `233`). Sort order is not a
hazard: no prefix scan or ordered query runs over an account-bearing key.
The desk terms are compared two ways: as bytes or
a digest of the daemon-built JSON (`desk_authority_device.go:81`,
`desk_authority.go:149` and `162-166`), which `omitempty` keeps identical
for IBKR; and field-wise on account and mode against the current scope
(`desk_execution_submit.go:204`, `desk_authority_fence.go:41`,
`desk_authority.go:179`), which move to the constructor and
`sameBrokerScope` so the kind takes part. The queued terms are compared
through the scope at `desk_override.go:107` and `proposal_queue.go:231`. `rpc.AccountDataScope`
(`account_data_authority.go:49`) gains the same optional field under the
same rule, so an OpenD receipt can never match an IBKR scope by
construction rather than by account-id format. The current scope's account comes from `[gateway] account` today
(`broker_evidence.go:53` and `223`, and the same read at
`broker_write_transaction.go:214` and `283`, `mandate_verdict.go:174`,
`handlers.go:3725`, `alert_shadow_cadence.go:455`,
`protection_order_snapshot.go:74`), and its mode from the free
`accountModeForStatus` (`DU` prefix, else the IBKR ports), which an
account accessor alone cannot fix: an OpenD account is never `DU`, so
every site would class it `live`. The identity therefore lives on the
broker instance: `broker.Broker` gains `Scope() Scope` (kind, account,
mode), and the daemon holds one broker instance per process, constructed
from config at start (the kind and the pinned account, `[opend] account`
under `opend`) with the daemon's lifetime under both kinds, so the sites
that need a scope with no connector at all still have one. The method
reads the instance's own current observation: under `ibkr` the endpoint
account, port and connected account as today (the IBKR instance holds the
identity from config and reads the live session, whichever connector
generation it wraps); under `opend` the `trdEnv` the session's account
list reported for the pin. The eight reads above, `currentBrokerStateScope`
and the nine `brokerStateScopeFromSnapshot` callers read `Scope()`, and
`accountModeForStatus` keeps its IBKR body as the `ibkr` arm inside that
method, never as a free function; the readers
that derive a mode from a stored account alone (`fill_quotes.go:318`,
`534`, `561`, `broker_permissions.go:310`, `409`, `handlers.go:3709`) use
the same per-kind mode, which under `opend` is the observed `trdEnv` of
the pin or unknown. So the scope has two states under `opend`:
account-concrete from config before any session, with the mode unknown;
and concrete once the account list has reported the pin. A `Scope()` on the broker
instance had been weighed and rejected on 2026-11-04 (the instance was
per IBKR connector, and seven sites need a scope with no connector); the
multi-broker review of 2026-11-04 asked for it so that a later daemon
holding N brokers never re-touches the 81 scoped files, and the objection
falls once the instance is per daemon rather than per connector, which is
what the paragraph above now says. The identity's source is then the one
place that changes for N brokers. There is no migration, no rewritten document and no
cutover step: a running `daemon.db` reads exactly as before, and a
fixture test pins the IBKR key and fingerprint bytes of every family and
the IBKR JSON of the four terms and of the receipt scope to today's values
and asserts that the zero kind and `ibkr` produce the same bytes, and
that the same account id under `ibkr` and `opend` never produces the same
bytes in any family.
An IBKR store is never read as OpenD state because no OpenD key can
collide with an IBKR key, and the Asia-style synthetic book in
`asia_simulation_test.go` keeps its IBKR scope.

Configuration for OpenD lives in its own `[opend]` table, because the
`[gateway]` keys and their generated reference describe IBKR ports, client
ids and account codes:

| Key | Meaning |
| --- | --- |
| `[opend] host` | OpenD host; empty is loopback. |
| `[opend] port` | OpenD `api_port`; absent probes 11111. |
| `[opend] account` | The Futu account id (`accID`) the daemon is scoped to. Required: a Futu login carries several account ids across both environments, and the client is built with its scope before it connects, so there is no unpinned read path. A daemon with `kind = "opend"` and no account refuses to start and says so; a pinned account the session's account list does not report makes every read fail closed with no mode and the trading status carry the existing `gateway_account_mismatch` blocker with OpenD wording. |
| `[opend] client_id` | The `clientID` string sent in `InitConnect`; default `canary`. |

The config reference is generated from the struct comments (`make
docs-regen`), so these keys reach `docs/docs/reference/config.md` with their
doc strings.

Discovery gains an OpenD probe used only when the kind is `opend`: port
11111 on loopback, and the process name of OpenD in the process list, so
the "no listener" error can say "OpenD is running but its API port is
closed: it is still logging in, or `api_port` in its configuration is not
11111" versus "no OpenD process found". The IBKR probe order and hints are
untouched; the auto-rediscovery after a backend loss stays IBKR-only.

Under `opend` the daemon holds no IBKR connector, and the code that would
build one is gated by the kind, not by a nil check alone. Three paths
reach IBKR today: `Start` resolves the `[gateway]` endpoint over the four
standard ports (`discover.Resolve`, `server.go:1573`) and dials it
(`runConnectAttempt`, `server.go:1642`); the breadth connector builds its
own IBKR connector (`breadthGatewayConnector` 1128,
`triggerBreadthConnect` 1247, `breadthConnectFlow` 1285, reached from
`answer_path.go:279` and `server.go:1016`, `1133`, `1441`); and the
reconnect, whose gate is `triggerReconnect` (`server.go:2563-2590`) and
whose dial is `reconnectFlow` (2616) through `connectWithFailover` (1892)
to the candidate loop at 1948, armed by `gatewayConnector()` (2805, called
from 46 non-test daemon files) on a nil connector and by
`gateway_app_watch.go:300`. All three build through `newConnector`
(`server.go:1816`), the one constructor of an IBKR connector. A fourth
path does not: the TWS order control opens its one-request client-0
connection itself (`tws_order_control.go:245-265`,
`ibkrlib.RunTWSOrderControl` with the host and port from the status), but
only inside an authorised write transaction that refuses without a
captured IBKR session (`broker_write_transaction.go:252`), so under
`opend` the authorisation refuses before any dial. Under `opend`, `Start`
builds the OpenD broker and runs no IBKR discovery or connect, and the
reconnect gate is one early `return false` in `triggerReconnect`, which
has six callers (`gatewayConnector`, `gateway_app_watch.go:300`,
`handlers.go:3610`, `order_risk_authority.go:84`, `answer_path.go:221` and
`274`). The gate cannot sit in `newConnector` alone: `reconnectFlow` calls
`discover.Resolve` (`server.go:2627`, `2671`) before any constructor, and
with `[gateway] port` unpinned that probes the four standard ports with a
TCP connect `newConnector` never sees, and on no listener runs `ps` and
starts the app-watch poller, whose poll triggers the reconnect again.
`newConnector` is the backstop: its signature becomes `(*ibkrlib.Connector,
error)`, because a nil connector wrapped in `connectAttempter` is a typed
nil that `tryOneHandshake` would call, and both callers handle the error.
The breadth flow is unreachable because `breadthGatewayConnector` and
`triggerBreadthConnect` return early under `opend`; the engine itself stays
built, because `Start` attaches its store (corrected in WP8, 2026-11-04). Three IBKR hosts on other ports remain and are gated by
kind: the Flex fetch (HTTP, behind `cfg.Flex.Enabled`), the borrow-fee
FTP dial and the Web API cash ledger; so the claim is "never opens the
IBKR API socket and never dials an IBKR host", and the IBKR-only features
answer unavailable without dialling. The test does not bind the four
standard ports (on the operator's machine, where TWS holds them, the skip
would remove the test exactly where a wrong gate dials the live TWS): it
opens four `127.0.0.1:0` listeners whose `Accept` fails the test, injects
them through `discover.StandardPorts`, leaves `[gateway] port` unpinned so
the probe would run, replaces neither `discover.Probe` nor the attempter
factory, and drives an `opend` daemon through `Start`, the status and
brief reads, a breadth question, a reconnect trigger, the auto-endpoint
path, the app watch and a TWS cancel attempt.

## The read seam: `internal/broker`

`internal/broker` holds the broker-neutral types and the interfaces the
daemon reads through. It is a type package: no I/O, no policy, no state. The
interfaces are small and per concern, in the style of the daemon's existing
test seams, so the IBKR adapter can be checked against the current code one
interface at a time.

```go
package broker

type Kind string // "ibkr" | "opend"; the zero value reads as ibkr

// Instrument is the broker-neutral identity of one tradable thing. The
// fields beyond the id are what the daemon reads off a contract today, so
// the mapping to a client's contract type and back is total for every
// security type the daemon uses (STK, OPT, FUT, IND, BOND, BILL, CASH, BAG).
type Instrument struct {
    Broker   Kind
    ID       string // IBKR: the ConID as text; OpenD: "HK.00700"
    Symbol   string
    SecType  string // STK | OPT | FUT | IND | BOND | BILL | CASH | BAG, as rpc uses them
    Market   string // us | de | hk | sg | cn | jp (the route family rpc.ContractParams.Market already carries)
    Venue    string // the routing or listing venue as the broker names it (SMART, SEHK, NASDAQ, SGX, XETRA)
    Primary  string // the primary listing venue behind a smart route
    Currency string
    LocalSymbol, TradingClass string
    Expiry string; Strike float64; Right string; Multiplier int
    SecIDType, SecID string // ISIN for bonds
    Legs []Leg              // a combination's legs
}

// SessionBinding is a value: the broker kind, the daemon's generation of
// the connector it was captured on, and that connector's own session
// epoch. It carries nothing to dereference.
type SessionBinding struct { Kind Kind; Generation uint64; Epoch uint64 }

type Session interface {
    Capture() (SessionBinding, bool)
    Current(SessionBinding) bool
    Health() SessionHealth // connected, ready, confirmed account, server version, last error
}

type Account interface {
    Summary(ctx context.Context, timeout time.Duration) (*AccountSummary, Provenance, error)
}

type Positions interface {
    Positions() ([]Position, PositionsHealth, error)
}

type Quotes interface {
    // Key is the key the broker files a line under; for IBKR that is the
    // symbol or routed contract key today's subscription manager keys on,
    // so the manager's behaviour does not change. The two subscribes mirror
    // the two IBKR paths (bare symbol, exact contract) one to one.
    Key(in Instrument) string
    SubscribeSymbol(ctx context.Context, symbol string, fields []string) (key string, binding SessionBinding, err error)
    Subscribe(ctx context.Context, in Instrument, fields []string) (key string, binding SessionBinding, err error)
    Unsubscribe(ctx context.Context, key string) error
    UnsubscribeForSession(ctx context.Context, binding SessionBinding, key string) error
    Snapshot() map[string]*Quote // keyed by the subscription key
    Load() (Load, bool)          // lines held and capacity, last refusal
}

type Bars interface {
    // ChartBars is the chart shape (a window of days at an interval);
    // SessionBars is the intraday session shape the setup evaluations read.
    ChartBars(ctx context.Context, in Instrument, days int, interval string, timeout time.Duration) (BarSeries, error)
    SessionBars(ctx context.Context, in Instrument, from, to time.Time, timeout time.Duration) ([]Bar, error)
}

type Instruments interface {
    Resolve(ctx context.Context, binding SessionBinding, in Instrument, timeout time.Duration) (Instrument, error)
}

type Broker interface {
    Kind() Kind
    Session; Account; Positions; Quotes; Bars; Instruments
}
```

The sketch above is the shape as built in WP1 (2026-11-04 16:40 CET).
Two departures from the first draft, both so the IBKR adapter is a
mapping and not a reinterpretation: `Quotes` keeps IBKR's two subscribe
paths apart (a bare symbol resolves its default route inside the client;
an exact contract does not) and exposes the key function and the
session-bound release the manager already uses; `Bars` has the two read
shapes the daemon has (chart days at an interval, and session bars between
two instants), because `FetchChartBars` and `FetchSetupBars` are different
requests with different pacing and the OpenD adapter serves both from
`Qot_RequestHistoryKL` with its own windowing. `Instrument` carries the
listing names, the external id and the legs because the positions view and
the market-data key read them.

Rules for the seam:

- Nil means unavailable, never zero. `AccountSummary` keeps the typed
  money fields as pointers where IBKR already returns them so, and every
  result carries its `as_of`, data type and source health through to the
  surface.
- `ibkrlib.ConnectorSessionBinding` has unexported fields and no
  constructor (`pkg/ibkr/connector.go:1077-1081`), and `pkg/ibkr` does not
  change in phase A. The IBKR adapter therefore keeps the concrete binding
  of the current epoch in a side table and answers `Current` by handing the
  stored concrete binding to the connector; `Resolve` does the same. A
  neutral binding is never rebuilt into a concrete one. The session epoch
  is a counter of one `Connection` (`pkg/ibkr/connection.go:317`), and the
  daemon replaces the whole connector on a rebuild while keeping its own
  `connectorEpoch` (`server.go:1948-1949`), so an epoch alone could match
  across two connectors. The neutral binding therefore carries the daemon's
  connector generation as well, one adapter instance is built per
  connector and discarded with it, and a binding from an earlier generation
  is never current. That is the same triple the write binding keeps
  (`broker_write_transaction.go:29-31`: connector, `connectorEpoch`,
  session). The adapter reads the generation after the daemon's increment
  (`server.go:1949` follows the assignment at 1948), so a binding from
  the previous connector can never carry the new generation.
- The subscription manager reaches past today's narrow interface: it
  type-asserts the market connector to the concrete type to capture and
  recheck the session (`subs.go:131-136`, `164-166`) and keys its entries
  on that concrete session. Phase A removes the downcast: `Subscribe`
  returns the neutral binding, the manager stores it in the entry and
  rechecks with `Current`. This is the one place a seam already exists and
  is bypassed; its tests cover the recheck across a reconnect and a
  rebuild.
- The seam covers reads only. The order path (contract resolution for a
  session, WhatIf, order-id reservation, guarded submit and cancel,
  open-order snapshots, executions) keeps calling the concrete
  `*ibkrlib.Connector` in this design, through
  `brokerWriteTransactionBinding` as it stands
  (`broker_write_transaction.go:21-35`). OpenD's order path is phase D and
  gets its own narrow write interface and binding then; nothing in phases
  A to C touches `order_*.go`, `broker_write_transaction.go` or the
  trading-tagged files.

`internal/broker/ibkr` is the adapter: a struct holding `*ibkrlib.Connector`
and the mapping functions between `broker.Instrument` and
`ibkrlib.Contract`, between `broker.Quote` and `ibkrlib.MarketData`, and so
on. The daemon's server gains `broker broker.Broker` beside the concrete
`connector`; the reads in scope move to the seam, and the concrete
connector remains for the order path and the IBKR-only features.

What moves to the seam in phase A, and what stays IBKR-only:

| Daemon interface (file) | Phase A |
| --- | --- |
| `connectAttempter`, `lastErrorReporter`, `connectErrorReporter`, `sessionStartReporter` (`server.go`) | `broker.Session` |
| `ibkrMarketConnector` (`subs.go`) | `broker.Quotes` |
| `chartReader` (`market_data.go`) | `broker.Bars`; the delayed-data retry is an IBKR behaviour the adapter keeps inside itself |
| `setupBarSource`, `setupResolver`, `mandateHistorySession` (`setups.go`, `setup_resolve.go`, `mandate_history.go`) | `broker.Bars` plus `broker.Instruments` with the session binding |
| account snapshot (`account_authority.go:194`), positions (`handlers.go:437-467`), the data-health observation of both (`data_health_rpc_observation.go:11`) | `broker.Account`, `broker.Positions`; the observation takes the neutral binding |
| `accountDailyPnLReader`, `dailyPnLAuthorityConnector`, `dailyPnLCloseCaptureSource` | stays IBKR-only in phase A (the P&L stream has no OpenD equivalent in scope); off under `opend` |
| `chainExpiriesConnector`, `expiryStrikesClassedFetcher`, the WSH clients, `feeRateHistoricalConnector`, `lendingIdentityResolver` | IBKR-only; off under `opend` |

### What proves phase A

The golden render tests run the CLI against a fake daemon connection
(`internal/cli/render_golden_test.go:79-80`); they never execute daemon
code, and `pkg/ibkr` has no diff by construction because the adapter lives
outside it. Neither can catch a wrong adapter. The proof of "no behaviour
change" is, in this order:

1. The adapter's mapping tests: a table over every `SecType` the daemon
   uses, both directions, plus the quote, bar, position and account
   summary mappings against `pkg/ibkr` values, including nil money
   fields and the provenance.
2. The daemon tests that already substitute the narrow seams
   (`setups_fake_test.go`, `gateway_session_loss_test.go`,
   `listener_rejection_test.go`, `peer_close_test.go`,
   `auto_endpoint_test.go`, the add-market, borrow and gateway-schedule
   tests) with their assertions unchanged, and the hermetic lifecycle
   tests (`make test-integration`).
3. The regression spine on the landed commit: it mutates committed HEAD,
   so it runs after the commit, never as a pre-commit check, and since
   pushes happen only on the owner's instruction it is run by hand on
   local main after each landing (`make regression-spine-check` runs
   locally; it is only kept out of `check` as a CI-only target) and its
   stamp line quoted.
   Its daemon witnesses for positions identity,
   portfolio completeness, budget account evidence and gamma, and its
   `pkg/ibkr` witnesses, exercise code the seam moves or wraps. The
   positions-identity patch applies to `handlers.go`, which phase A edits;
   if the hunk stops applying, re-basing it is a safety-test change made
   in the same commit, after running the spine on the previous revision,
   as `AGENTS.md` requires.
4. The next live session: `make smoke` against a Gateway, then `make
   restart-daemon` and the redacted `canary status --json`, `canary
   account --json` and `canary positions --json` before and after, as the
   trading contract template asks. Until it ran, the report says "proof
   pending: next live session".

The golden diff (`git diff --exit-code internal/cli/testdata/golden`) and
the `pkg/ibkr` diff are side checks that nothing rendered or wire-level
moved; they are evidence of absence of a surface change, not of a correct
adapter.

## `pkg/futu`: the OpenD wire client

`pkg/futu` is a clean-room Go client for OpenD's TCP protocol, in the shape
of `pkg/ibkr`: a low-level `Connection` owning one socket, the handshake,
serial numbers, framing and message dispatch; a `Connector` on top with
lifecycle, subscriptions, request helpers and in-memory observations;
`NewConnector` and `Connector.Start`. The daemon never sees a protobuf type.

### Framing

Every packet is a fixed 44-byte header followed by the body, little-endian
(from `MESSAGE_HEAD_FMT = "<1s1sI2B2I20s8s"` in the public client):

| Offset | Size | Field |
| --- | --- | --- |
| 0 | 2 | header flag, the bytes `F` `T` |
| 2 | 4 | protocol id (u32) |
| 6 | 1 | body format: 0 protobuf, 1 JSON |
| 7 | 1 | protocol version, 0 |
| 8 | 4 | serial number (u32), the client's, echoed in the reply |
| 12 | 4 | body length (u32) |
| 16 | 20 | SHA-1 of the plaintext body |
| 36 | 8 | reserved, zero |

Canary speaks protobuf only, for requests, replies and pushes. The SHA-1 is
checked on every inbound frame and a mismatch drops the session (the public
client reports it as an error and the session is not trustworthy after it).
Encryption is not implemented: `InitConnect` asks for `PacketEncAlgo_None`
(-1), and an OpenD configured with an RSA key refuses the connection; the
status row says so and names the OpenD setting. This keeps `crypto/rsa` and
Futu's modified AES out of the client; the owner runs OpenD on the same
machine, where the loopback socket is the trust boundary exactly as it is
for IB Gateway today.

### The session

`InitConnect` (1001) carries `clientVer` (Canary's own, `major*100+minor`),
`clientID` (`[opend] client_id`), `recvNotify` true, `packetEncAlgo` -1,
`pushProtoFmt` 0 and `programmingLanguage` `Go`. The reply carries
`serverVer`, `loginUserID`, `connID`, `keepAliveInterval` and the AES
material Canary ignores. Every trade request from then on carries
`Common.PacketID{connID, serialNo}`, OpenD's replay guard.

The pin is enforced by construction at this layer: the `Connector` is
built with the pinned `accID` before it connects, stamps every `TrdHeader`
from it, and no method takes an account or environment argument, so no
caller can address another account through this client. The `trdEnv` is
not configured: after `InitConnect` the connector reads `Trd_GetAccList`
(which carries no `TrdHeader`, so no trade-class request precedes the
scope), finds the pinned account and takes its `trdEnv` as the one it
stamps; until that reply, and if the list does not report the pinned
account, the connector has no scope and every trade-class request fails
typed. The connector is never re-scoped: the daemon's status poll re-reads
the list inside the pacing table, and a list that no longer reports the
pinned account with the same `trdEnv` drops the session with a typed
reason, after which the next connect scopes afresh.

The `serverVer` gate: `pkg/futu` declares `minServerVersion`, a constant
declared from the documentation at build time and confirmed at the first
real contact, and refuses the session with a typed `ErrServerVersion`
below it. OpenD raises its own minimum client
version without notice (`ProgramStatusType_ForceUpdate`,
`GtwEventType_ForceUpdate`); both are typed session errors the status row
prints as "OpenD requires a newer Canary" with the version pair.

Keep-alive (1004) is sent every `keepAliveInterval` seconds with the client's
epoch time. The heartbeat is kept by received frames, never by a successful
send, the rule `pkg/ibkr` already follows: an OpenD that accepts requests
and answers none loses the session after two intervals and the daemon
redials it.

Session health is `GetGlobalState` (1002) at connect and on the status poll:
`qotLogined`, `trdLogined`, `serverVer`, `serverBuildNo`, `programStatus`
(`Ready`, or the login, verification and disclaimer states), `connID` and
the per-market states. `Notify` (1003) pushes keep it current between polls:
`ConnStatus`, `ProgramStatus`, `GtwEvent` (kicked out, password changed,
device lock), `QotRight`, `APIQuota` and `UsedQuota`. The daemon's gateway
phases map as: port down, port rejecting, `InitConnect` not answered,
program status not `Ready`, quote or trade server not logged in, ready.

### Reads

| Need | Protocol | Notes |
| --- | --- | --- |
| Instrument lookup | `Qot_GetStaticInfo` (3202) | Name, lot size, type, listing; the OpenD side of `broker.Instruments.Resolve`. |
| Streaming quotes | `Qot_Sub` (3001), `Qot_RegQotPush` (3002), `Qot_UpdateBasicQot` (3005) push, `Qot_GetBasicQot` (3004) | One `SubType_Basic` subscription per instrument. A line is held at least one minute before it may be released (documented rule). |
| Snapshot quotes | `Qot_GetSecuritySnapshot` (3203) | Up to 400 instruments per request; 60 requests per 30 seconds (public documentation, read 2026-11-03). Bid, ask, last, volume, lot size, suspension. |
| Quota | `Qot_GetSubInfo` (3003), `Notify` `APIQuota` and `UsedQuota` | `totalUsedQuota` and `remainQuota` are OpenD-wide facts, across all connections. |
| Bars | `Qot_GetKL` (3006) for subscribed candles, `Qot_RequestHistoryKL` (3103) for history, `Qot_RequestHistoryKLQuota` (3104) | History costs one unit of the seven-day quota per instrument; repeats for the same instrument are free inside the window. |
| Accounts | `Trd_GetAccList` (2001) | `trdEnv` (0 simulate, 1 real), `accID`, `trdMarketAuthList`, `accType`, `securityFirm`, `simAccType`. |
| Funds | `Trd_GetFunds` (2101) | Power, total assets, cash, market value, available funds, margins, `cashInfoList` per currency, risk level. |
| Positions | `Trd_GetPositionList` (2102) | Code, quantity, sellable quantity, price, cost, value, P&L, `secMarket`, `currency`. |
| Orders and fills | `Trd_GetOrderList` (2201), `Trd_GetOrderFillList` (2211), `Trd_SubAccPush` (2008) with `Trd_UpdateOrder` (2208) and `Trd_UpdateOrderFill` (2218) pushes, history 2221 and 2222 | Reads in every build. |
| Maximum tradable quantity | `Trd_GetMaxTrdQtys` (2111) | A read in every build; the preview leg of phase D. |

Every reply is the envelope `retType`, `retMsg`, `errCode`, `s2c`.
`retType` other than 0 becomes a typed `futu.Error{RetType, ErrCode,
Message}`; `-100` timeout, `-200` disconnect and `-500` malformed body are
session-level, the rest request-level. `retMsg` reaches the surface as a
quoted message and never changes a decision.

### Pacing

OpenD enforces a per-interface frequency limit over 30-second windows and
the per-account subscription and history quotas. `pkg/futu/limits.go` is one
table of protocol id, requests per 30 seconds and per-request size, with the
documentation page and date each number came from. The client refuses
locally before OpenD would, with a typed `ErrPaced` that names the window,
so Canary's own bursts never cost the session. The table bounds Canary's
spend only: the subscription and history quotas are OpenD-wide, another
client on the same OpenD spends them too, and the daemon learns that from
OpenD's answers (`Qot_GetSubInfo`, the quota pushes, a refusal), not from
the table. The simulator enforces the same table, and a test asserts the
two read one source.

### Writes (phase D only)

`Trd_PlaceOrder` (2202), `Trd_ModifyOrder` (2205) and the cancel form of
modify exist behind the `trading` build tag exactly as `pkg/ibkr`'s raw
methods do: in the default build they return `ErrTradingDisabled` before a
frame is written. When those files exist, `pkg/futu` joins
`TRADING_PKG_TESTS` in the `Makefile`, or `trading-package-scope-check`
fails. There is no method for `Trd_UnlockTrade` (2005): a test walks the
package for the protocol id and fails on any encoder for it, and the
simulator fails the test that owns it on any 2005 frame it receives.

### Protobuf

The `.proto` definitions ship under the Apache-2.0 licence. The build
vendors the files Canary uses under `pkg/futu/internal/pb/proto/` with the
licence and a NOTICE naming the source repository and commit, and generates
Go code with `protoc-gen-go` into one Go package per vendored file,
`pkg/futu/internal/pb/<lowername>/`: every Futu definition declares its own
`package <File>` and most declare top-level `Request`, `Response`, `C2S`
and `S2C`, which a single Go package cannot hold. The vendored files stay
byte-identical to upstream; their `go_package` points at a Futu path that
does not resolve, so the generator is run with
`--go_opt=module=github.com/osauer/canary/v2` and one
`--go_opt=M<File>.proto=<import path>` per vendored file (the imported
`Common`, `Qot_Common` and `Trd_Common` included), derived from the
vendored list. Generated code is committed. The `internal` path keeps every
protobuf type out of `pkg/futu`'s API. The `vet`, `staticcheck`,
`modernize` and `govulncheck` legs take every tracked package and the
modernize analysers do not skip generated files, so WP5 is done only with
those legs green on the generated packages; if one is red, the remedy is
to exclude `pkg/futu/internal/pb/...` from that analyser's package list.

`protoc` is a C++ binary and cannot be pinned in a Go module;
`protoc-gen-go` can and is, in `tools/go.mod`, never in the product module
(the dependency gate forbids a tool block there); the regeneration
resolves it as `go -C tools tool -n protoc-gen-go` and passes it to
`protoc` with `--plugin`, and `protoc` comes from `PATH`. Regeneration is a
developer action, `make futu-proto-regen`, which refuses to run unless
`protoc --version` matches the version recorded in
`pkg/futu/internal/pb/MANIFEST` beside the SHA-256 of every vendored
`.proto`, the SHA-256 of every generated `.pb.go`, the `protoc-gen-go`
version, and the SHA-256 of the `protoc` release archive for each platform
the regeneration supports. The manifest carries the `protoc` version in
both of its forms, because the two checks read different ones: `protoc
--version` prints `libprotoc 32.1`, and the generated header carries the
`protoc v6.32.1` form the plugin request reports (the C++ major of the
release, read from the first generation, 2026-11-04). The regeneration
rewrites the file hashes and the version lines and preserves the hand-kept
archive checksum lines, so the CI job's diff can be clean. The tool pin
and the runtime pin of `google.golang.org/protobuf` are two lines in two
`go.mod` files; `futu-proto-check` compares both with the manifest. Two
checks, and what each binds is stated
because neither does the other's job. `futu-proto-check`, a leg of `check`,
needs no `protoc`: it binds every vendored and generated file to its
manifest line and every generated header to the manifest's generator
versions, so a changed definition or a hand-edited generated file without
a manifest update fails on any machine. It cannot tell whether the
generated code came from these definitions; only a regeneration can. That
provenance is `make futu-proto-regen` followed by `git diff --exit-code
pkg/futu/internal/pb`: the developer runs it before a commit that touches
the directory and names the run in the commit body, and a CI job runs the
same two commands with a `protoc` fetched at the manifest's version and
refused unless its archive matches the manifest's checksum. That job is
the one place `protoc` runs unattended, and the checksum is what pins it.
The job runs `make futu-proto-regen` as its own `run:` line, because
`ci-gate-scope-check` matches `make <target>` lines with a line-anchored
expression, and the diff check is a separate step. `futu-proto-regen`
joins `CI_ONLY_TARGETS` with its reason (it needs `protoc`), which is what
the scope check requires of a `make` target that CI runs outside `check`;
`futu-proto-check` joins `CHECK_TARGETS`, an open list, and needs no
`ci.yml` line.

`google.golang.org/protobuf` becomes the eighth direct product dependency.
`scripts/check-go-dependencies.sh` carries the allowlist and prints the
count; both change in the same commit. The alternatives were weighed: a
hand-written encoder for proto2 messages with required fields across 77
files is more code than the client itself and would drift; and no upstream
Go binding is published as a module (the definitions declare a `go_package`
under a Futu path that does not resolve, checked 2026-11-04). The owner accepted
it on 2026-11-04 20:00 CET; see the last section.

## Mode pin and the paper proof

IBKR tells paper from live by port and by the `DU` account prefix. Futu
carries `trdEnv` on every trade request and several `accID` per login, in
both environments. The pin becomes broker-neutral:

- `brokerStateScope{Kind, Account, Mode}`: the kind, the concrete account
  the daemon is scoped to, and the mode the daemon observed for it. For
  IBKR the observation stays prefix-then-port; for OpenD it is the `trdEnv`
  of the pinned `accID` in the account list the session reported.
- `[trading].mode` is the owner's declaration. The trading status blockers
  `paper_endpoint_unconfirmed` and `live_endpoint_unconfirmed` keep their
  codes and gain the OpenD reading: `paper` requires an account whose
  `trdEnv` is simulate; `live` requires real.
- In phases A to C no write reaches OpenD, so the pin's enforcement on
  writes is phase D work, designed then on the pattern of
  `brokerWriteTransactionBinding`: the binding captures the OpenD
  connector generation, the session's `connID` and the scope at
  authorisation and revalidates all three immediately before broker I/O; a
  changed account list, a reconnect or a scope mismatch refuses with the
  existing drift error. Two things are already true by construction in
  phases B and C: the client stamps every `TrdHeader` with the pinned
  account and the environment it read for it, and the daemon's one broker
  is built from the pin.

The paper-proof pattern is the one the TWS order-control path established
(`tws_order_control.go:83`, [platform-settings.md](platform-settings.md)),
copied exactly for phase D:

- `trading.opend.enabled` is the owner's switch: registry class `safety`,
  human-terminal only, off by default; a change advances the
  trading-control generation.
- `trading.opend.paper_proven_at` is the daemon's: written once when a
  paper order request on OpenD ends confirmed or pending, read-only on the
  settings surface, in no writability class, so no patch from any origin
  can set it.
- A real account accepts a write only when both exist: the switch on and
  the proof recorded. Paper works once the switch is on.

Futu's paper environment needs no unlock, so the owner runs the proof
without a trading password anywhere near Canary. Both keys are phase D
work; they are named here so the settings document version and the
classes are decided with the rest of the pin.

## Unlock state

What the documentation says: live accounts must unlock before place, modify
or cancel; the unlock is an operation on OpenD, not on a connection ("as
long as one connection is unlocked, all other connections can call the
transaction interface"); paper needs none; the GUI build cannot be unlocked
through the API; there is no documented read of the current lock state, and
`Notify` carries no unlock event (its gateway events include a trade
password change, not an unlock). Whether `Trd_GetMaxTrdQtys` on a real
account needs the unlock is not documented either.

The design, then, for phase D:

- Canary never implements protocol 2005 and never asks for the password.
- The lock state gates nothing. Every live write-class request that passes
  Canary's own gates (instruction, origin, mode pin, freeze, limits,
  preview token, binding) is sent to OpenD; OpenD is the one that refuses
  a locked account, and that refusal is the order's typed outcome
  (`broker_locked`, with `retMsg` quoted and the action "unlock trading
  in OpenD, then preview and place again"). The owner's next explicit
  request is sent again; nothing in Canary remembers a lock in a way that
  could refuse it. The mechanism therefore always terminates: the exit is
  the owner's unlock plus the next instruction, and it never depends on
  whether `Trd_GetMaxTrdQtys` probes the lock.
- What Canary keeps is an observation for the surface, not a state:
  "last live write accepted at 10:02" or "last live write refused at
  10:02: locked", from the broker's answers only, never from an
  assumption. The status row prints exactly that, or "no live write this
  session"; paper prints "not needed on a paper account".
- The refusal is recognised by `errCode`, which is unknown until the first
  real contact. Until then every refusal is "OpenD refused: <retMsg>", and
  the simulator's lock switch answers with a placeholder code that the
  fixture names as unconfirmed; the first real capture replaces it.

## Preview capability and `GetMaxTrdQtys`

The trading contract assumes a broker what-if exists. It becomes a
per-broker capability, carried on `rpc.TradingStatus` as an additive field:

```go
type BrokerCapabilities struct {
    Broker         string `json:"broker"`          // ibkr | opend
    Preview        string `json:"preview"`         // whatif | max_quantity
    Unlock         string `json:"unlock"`          // not_required | owner_in_opend
    Statements     string `json:"statements"`      // flex | none
    Reconciliation bool   `json:"reconciliation"`
    Edge           bool   `json:"edge"`
}
```

For IBKR it reads `whatif`, `not_required`, `flex`, true, true. For OpenD
the broker leg of a preview (phase D) is `Trd_GetMaxTrdQtys`: maximum cash
buy, maximum cash-and-margin buy, maximum position sell, maximum short, and
the required initial margin for the order. Ten requests per 30 seconds per
account; cash accounts are not supported (a typed refusal the preview
reports as `unavailable`, not as a pass); paper accounts work.

The preview result keeps its shape. `OrderWhatIfResult.Status` is
`accepted` when the drafted quantity is within the broker's maximum for the
side, `rejected` when it exceeds it, `unavailable` when OpenD refused the
query; `Margin` carries the required initial margin as the broker's margin
leg; `Message` says in words that the leg is a capacity check, not a
what-if. Canary's own limits (the risk constitution's `[order_limits]`, the
rulebook, the freeze, the mode pin) apply before and after exactly as today.
`RequiredForSubmit` stays true: no preview, no token.

The broker-permission rows (`Broker access` in `canary status`, fed from
what-if answers by `broker_permissions.go:141`) have no OpenD source: there
is no per-class evidence from a capacity check. Under `opend` the row reads
the account's authorised markets from `trdMarketAuthList` ("stocks on HK,
US, SG; options and futures not served on OpenD") and the permission store
records nothing.

`canary trading status` already prints a `Capabilities` row,
`preview=true write=true` (`internal/cli/trading.go:131`). The broker leg
joins that row rather than a new one: `preview=true (what-if) write=true`
for IBKR and `preview=true (maximum quantity, no what-if) write=true` for
OpenD. The IBKR golden `trading_status.txt` changes by those words, an
intentional, reviewed diff in phase C; nothing visible is removed.

## Subscription quota and snapshot-sourced readings

The smallest documented subscription quota is 100 live subscriptions, each
instrument and data type costing one; the quota is OpenD-wide, so another
client on the same OpenD spends it too. The daemon budgets without a knob:

- Streaming lines (`Qot_Sub` basic) for the held names and the names the
  regime and the brief read every minute; one subscription type per
  instrument. That set is recomputed from the book on every acquire, so a
  new holding always takes a line; nothing is reserved by a number.
- Snapshots (`Qot_GetSecuritySnapshot`, batched to 400 per request under
  the 60-per-30-seconds pacing) for everything else: watchlist names,
  setups' candidates, the names a one-off `canary market` asks for.
- When OpenD reports the quota exhausted (another client, or a book larger
  than the tier), the next streaming acquire fails typed and the manager
  serves that name by snapshot until a line frees; the status `Quotas`
  row and the brief say so.
- The one-minute minimum hold before an unsubscribe is honoured by the
  subscription manager's release path.

The subscription manager (`subs.go`) already refcounts one line per key
and re-fetches the connector on every tick; the OpenD leg adds the budget
decision at acquire time, and `broker.Quotes.Load()` reports
`SubscriptionsUsed`, `SubscriptionQuota`, `HistoryUsed`, `HistoryQuota` and
the last refusal time, from `Qot_GetSubInfo` and the quota pushes.

A reading that came from a snapshot says so. `rpc.Quote` gains an additive
`Delivery string json:"delivery,omitempty"` with `stream` or `snapshot`;
`PositionView.PriceSource`, which already names the quote input that priced
the row, carries `snapshot` for a snapshot-priced row. `DataType` keeps its
meaning (live, delayed, frozen, closed) and is not overloaded. The brief's
overview names the split in the reader's words: "12 of 40 quotes are
snapshots; 97 of 100 subscriptions in use". IBKR responses omit `delivery`,
so their goldens are unchanged.

## Instrument identity in `internal/rpc`

`rpc.ContractParams`, `PositionView`, `Quote`, the order views and the
opportunity and watchlist rows identify instruments the IBKR way. The
additive identity is one struct, an optional field wherever an instrument is
named. It is a plain value with `omitzero`, not a pointer: the wire is the same
(an object or nothing), and `ContractParams` is compared with `==` and keys two
maps in the daemon, where two requests naming one instrument would become two
keys under a pointer (found building WP10, 2026-11-05):

```go
// InstrumentID is the broker-neutral identity of an instrument. Present
// when the daemon knows the id and the listing venue; absent otherwise and
// on responses from a daemon that predates it. ConID stays the IBKR
// identity and stays optional.
type InstrumentID struct {
    Broker   string `json:"broker"`              // ibkr | opend
    ID       string `json:"id"`                  // ibkr: ConID as text; opend: "HK.00700"
    Market   string `json:"market,omitempty"`    // us | de | hk | sg | cn | jp
    Venue    string `json:"venue,omitempty"`     // SEHK, NASDAQ, SGX, XETRA ...
    Currency string `json:"currency,omitempty"`
}
```

`ContractParams.Market` already exists; its vocabulary grows to `hk`, `sg`,
`cn`, `jp` under OpenD rather than a second market field. An IBKR daemon still
refuses those routes, because no IBKR path changes here. Under OpenD
`quote.snapshot` reads the new words; `market.history` and the other paths that
share the IBKR stock normaliser refuse them, and take the venue or the identity
instead.

Request-side, a caller may give `instrument` instead of `con_id` on
`quote.snapshot`, `market.history` and `add.plan`. The dispatcher resolves it
through `broker.Instruments` and rewrites the request to the plain contract
fields, so no handler and no echo ever carries it. It refuses an identity of
another broker and one that disagrees with the contract's own `con_id` or
symbol. Every other method, the order paths among them, refuses an identity
rather than ignore it, because a dropped identity would leave the symbol to
decide what is traded; the watchlist's writes accept and drop it, so a list read
and written back stores none.

Response-side, the daemon fills the identity as an answer leaves, on a copy of
the rows of `positions.list`, `quote.snapshot`, the three order reads, the
opportunity and proposal snapshots and the watchlist. Its own rows, and the
revisions and fingerprints computed from them, do not change. `Quote` and the
opportunity and proposal rows carry it beside their `contract`, which stays
bare so that a row's contract can be sent back to an order path. OpenD rows
carry `instrument` and no `con_id`; IBKR rows carry both (`Broker: ibkr, ID:
<conid>`). Other rows keep `con_id` and `symbol` as they were: order events, the
sweep legs, and a quote nested in another answer.

`InstrumentID` carries no listing symbol: the join for netting the same stock
across brokers is the row's own `Symbol` with `InstrumentID.Venue`, through
`InstrumentID.JoinKey`, and never `ID`, whose form is per broker (the
multi-broker review, 2026-11-04). Three things make that join true:

- The venue is the listing venue. A smart route names no listing, so an IBKR row
  with only `SMART`, usually an option or a routed order, carries no identity;
  `con_id` still names it. IBKR's own names map to the exchange's (`IBIS` to
  `XETRA`, `ISLAND` to `NASDAQ`, `TSEJ` to `TSE`).
- OpenD's family `US` is not a venue. A US row's exchange comes from `exchType`
  in `Qot_GetStaticInfo`, read once per code; a US row whose exchange OpenD does
  not name carries no identity.
- A numeric symbol joins without its leading zeros: IBKR's `700` and OpenD's
  `00700` are one listing.

Consequences:

- Goldens: the human renderers do not print the identity struct, so no golden
  changes, IBKR's or OpenD's (`status_opend.txt`, `trading_status_opend.txt`).
- MCP: the `contract` object of `canary_market_history` and the arguments of
  `canary_add` gain the optional `instrument` property; `con_id`'s description
  becomes "Exact IBKR contract id when known". `make
  docs-regen` rewrites `docs/docs/reference/mcp-tools.md`; `parity-check`
  stays green because no tool is added.
- Desk compiles Canary at a pinned pseudo-version and reads these structs
  through the exported Go client. Additive fields with `omitempty` are
  wire-compatible with an older Desk; Desk sees them only after it re-pins
  to a Canary revision that carries the change. The re-pin is a Desk
  change after phase C lands on local main; nothing here blocks on it.

## What OpenD users do not get at first

Edge and reconciliation rest on IBKR Flex statements; the Web API cash
ledger, the short-stock availability file, the exact-contract `FEE_RATE`
fallback, Wall Street Horizon earnings, the breadth history connection, the
bond and bill cash sweep, TWS order control and the Gateway diagnostics are
all IBKR inputs. Under `opend` each is off, typed, and visible on every
surface:

- Data health lists each such source with the additive source state
  `not_applicable` and the reason "IBKR only"; it is never `stale`.
- `canary status` prints one `Not on OpenD` row naming them in words:
  "regime, Edge, reconciliation, statement reporting, borrow availability,
  earnings (WSH), breadth, gamma, cash sweep". The row is informational; the
  verdict does not degrade for it.
- The brief's coverage section names the missing items where it would
  otherwise show their figures; a figure that depends on a missing source
  is absent with its `*_unavailable` reason, not zero.
- MCP tool descriptions for `canary_edge`, `canary_recon_status`,
  `canary_reporting*`, `canary_lending_*` and `canary_short_interest_screen`
  say "IBKR only; on OpenD the result reports `not_applicable`", and the
  tools return that typed result rather than an error, so an AI host never
  mistakes "off for this broker" for "broken".
- The app and the SPA: `HealthResult` gains `Broker` (the kind; kept as a
  stable field, since Desk binds it as the order route under one daemon
  per broker, the multi-broker review, 2026-11-04), the
  app's live snapshot and `/api/bootstrap` carry it, the alert dispatcher
  passes it to the presentation map so the push-alert copy that names a
  "TWS connection issue" (`internal/app/alerts/presentation.go:63-67`)
  names the right process, and the SPA strings that name TWS
  (`web/app/strategies.js:416-419`, `web/app/opportunities.js:310`) read
  the kind from the bootstrap; the rendered result is inspected at real
  size and the report names the screenshot.
- Protection proposals, option exercise opportunities, gamma and the option
  chain recorder depend on option chains and Greeks the OpenD leg does not
  serve in phases A to C; they are off the same way.

The regime runs under `opend` since 2026-11-08, by the owner's decision (see
Owner decisions). Its broker reads go through the broker connection: a brief
quote look (`broker.Snapshots`) and daily bars (`broker.DailyBars`), each named
by symbol and kind. IBKR serves both by symbol through the calls the regime has
always made; OpenD answers a quote from one snapshot request and refuses an
index or a currency pair without a request, so VIX, VIX3M and USD/JPY are
missing on OpenD and the gamma row, which needs SPX option chains, is too. The
publication names each missing input in a `broker_input_not_supplied` warning,
one per row, and the verdict rests on the rows that can rank. `regime_gate.go`
and strategy one read their daily bars the same way. The first text of this
section (2026-11-04 22:20 CET) had regime inputs served through the history
quota; the regime draws history for HYG and SPY only.

Portfolio stress composes from the regime and answers under `opend` with its
input health degraded. With VIX, USD/JPY and gamma always missing, two or more
row groups are impaired, so by the regime's own rule (`RegimeCurrencyBlankFloor`)
an OpenD regime publishes its figures and named gaps under "Market state
undefined — data incomplete" and gives no market verdict. Whether an OpenD
regime should rank on fewer rows is the owner's decision. A login without a
market's quote right is named the same way (`broker_quote_right_missing`).
`TestOpenDRegimeAndStressAnswerEndToEnd` and
`TestOpenDRegimeNamesAMissingQuoteRight` dispatch both on the simulator.

OpenD also supplies no FX rate today. A holding outside the account's base currency
has its value in its own currency, so the rulebook reports it as not measured rather
than converting it. The account's own figures come from `Funds` in the base currency
and are not affected.

## Status rows for OpenD

`canary status` under `opend` replaces the TWS and API rows with OpenD's
facts. Rows and their sources:

| Row | Content | Source | Phase |
| --- | --- | --- | --- |
| `Session` | `<accID> (PAPER, simulate) via 127.0.0.1:11111, conn <connID>` | pin, `Trd_GetAccList`, `InitConnect` | C |
| `OpenD` | `9.3 (build 1234) · quote server logged in · trade server logged in`, or the program state in words ("waiting for the disclaimer in OpenD", "login failed") | `GetGlobalState`, `Notify` | C |
| `Protocol` | `serverVer 930 · Canary minimum 920` | `InitConnect`, `minServerVersion` | C |
| `Markets` | `HK, US, SG tradable on this account` | `trdMarketAuthList` | C |
| `Quote rights` | `HK LV2 · US LV1 · SG none` | `Notify` `QotRight` | C |
| `Quotas` | `subscriptions 37 of 100 · history 12 of 100 (7-day window)` | `Qot_GetSubInfo`, quota pushes | C |
| `Not on OpenD` | the list above | capability | C |
| `Last attempt` | the daemon's verdict on the last connect, including "OpenD refuses unencrypted connections: remove the RSA key from its configuration" and "OpenD requires a newer Canary" | session errors | C |
| `Trading unlock` | as in "Unlock state" | the broker's answers | D |

Every row has a golden case rendered from a synthetic `HealthResult` the
simulator's book produces, at `COLUMNS=80` with colour on and off like the
existing status goldens. The phase D row gets its golden in phase D, when
the code that produces it exists.

## The simulator

`pkg/futu/futusim` is a hermetic OpenD simulator in Go. It speaks the real
wire protocol, the 44-byte frame and the protobuf bodies generated from the
same definitions, with no encryption. It serves a synthetic paper-like
account and runs two ways: in-process from a test (`futusim.Start(t,
opts)` returning the address and a handle to the book), and as a process
for Desk and CLI witnesses (`cmd/opend-sim`, not part of the release
binaries, which build `./cmd/canary` only). `futusim` imports `pkg/futu`
for the frame layer and the limits table; tests that need both the client
and the simulator live in the external package `futu_test`.

The book (`futusim.Book`), loaded from `pkg/futu/futusim/testdata/book.json`
or built in code:

- Accounts: one simulate account authorised for HK, US and SG, and one real
  account that is locked by default. Account ids and balances are
  synthetic and obviously so.
- Instruments: Hong Kong, US and Singapore equities and one ETF per market
  (`HK.00700`, `HK.02800`, `US.AAPL`, `US.SPY`, `SG.D05`, `SG.ES3` and a few
  more), each with name, lot size, currency, listing date and a market
  state driven by the simulator's clock and the market's session table.
- Quotes: a deterministic walk per instrument seeded from the book, so two
  runs with the same clock produce the same frames; a scripted override per
  test.
- Positions, orders and fills: a starting book plus the orders the test
  places; a placed order fills against the walk under simple rules (limit
  crosses the walk, market fills at the walk) and emits the update pushes.
- Quotas and pacing: the subscription quota tier (default 100), the
  seven-day history quota, and the per-interface frequency table from
  `pkg/futu/limits.go`; a refusal answers with the documented `retType`
  and a message, and a counter the test can read.
- Switches: `serverVer`, `keepAliveInterval`, a program status other than
  `Ready`, a kicked-out event mid-session, a market with no quote right,
  the real account's lock state, a dropped keep-alive (OpenD answers
  nothing), an RSA-required refusal of `InitConnect`.

Fixtures: the simulator records the frames of a scripted session byte for
byte into `pkg/futu/testdata/frames/<protocol>_<case>.hex` (`-record`);
`pkg/futu`'s client tests replay them against a fake socket, and the
simulator's own tests assert it still emits exactly those bytes for the
scripted book. One set of frames, two readers; a drift in either side fails
a test. They are regression fixtures for the client against the simulator,
both built from the same definitions; they are not conformance evidence
against OpenD. Conformance begins with the first real capture (phase D),
recorded into the same directory under `opend_<case>.hex`, and a fixture
whose simulator version disagrees with a real capture is fixed on the
simulator side.

The whole witness is one command, `scripts/opend-witness.sh <outdir>`, after
`make build`. It refuses to start unless every path is inside its scratch directory,
and it fails if the operator's socket, stop marker or policy directory changed during
the run. By hand, `make opend-sim` starts `cmd/opend-sim -clock now` and
prints its loopback address on one line. The flags are `-listen`, `-book` (a book
file), `-clock fixed|now`, `-tier` (the subscription quota), `-record` (write the
frames to a directory) and `-lock-real`; it refuses a listen address off loopback or
on an IBKR API port. Point a witness at that address with the
stack in the build brief: `[broker] kind = "opend"`, `[opend]` host, port and account,
and its own `HOME`, XDG roots, `CANARY_CONFIG` and `CANARY_SOCKET`. Then
`canary daemon --foreground --config <dir>/config.toml --socket <dir>/canary.sock
--log stderr` serves `canary status`, `account`, `positions` and `brief` from the
simulator, and the home directory stays empty.

The simulator never implements an unlock: a 2005 frame fails the test that
owns the server. It never reads anything from the machine it runs on: no
config, no XDG roots, no clock unless told `-clock now`.

## Coverage bar and the regression tests the refactor keeps green

- `pkg/futu` statement coverage is at or above 80 percent from its first
  commit, measured on the package `./pkg/futu` alone (`-coverpkg=./pkg/futu`),
  not on `futusim` or the generated code. The measurement is a step of
  `test-pkg` in the `Makefile`, which is one of `CHECK_TEST_LEGS` and runs
  as its own CI job, so a commit below the bar fails the binding gate on
  every machine and in CI; `pages-coverage` publishes `pkg/futu` beside
  `pkg/ibkr`.
- The IBKR golden suites are unchanged by the seam, as a side check (see
  "What proves phase A"). Phase C adds goldens; it changes an IBKR golden
  only for the `Capabilities` row's broker leg, reviewed as a diff.
- The regression spine (`make regression-spine-check`, CI-only, on the
  landed revision) stays green: its `pkg/ibkr` witnesses
  (`TestReconnectShortPortfolioDownloadIsNotCurrent`,
  `TestFrontFutureReusesCompletedResolution`,
  `TestShared354DuringFarmImpairmentDisclosesDelayedService`,
  `TestIndexDailyHistoryAsksTradesFirst`,
  `TestMarketDataCapacityDoesNotPoisonPacing`,
  `TestPrewarmEarlyCompletionAndWarmCache`,
  `TestPrewarmRejectedRouteCompletesWithoutTimeout`,
  `TestRetainedOptionsSurviveResetAndPartialRestore`) and its daemon
  witnesses for positions identity, portfolio completeness, budget account
  evidence and gamma all exercise code the seam moves or wraps.
- The daemon tests that substitute the narrow seams keep their
  assertions; only the fake's type changes.
- The hermetic lifecycle integration tests (`make test-integration`) and
  the synthetic Asia book (`asia_simulation_test.go`) run unchanged under
  `ibkr`.

## Phases and checkpoints

Each checkpoint leaves the tree building, every step is safe to re-run, and
each commit names its gate tier. Tiers follow the repository's own: `make
check` before a commit; `make test` for Go and runtime changes; the release
tier for a change to a broker path, an endpoint or policy behaviour: `make
test`, the live proof where the change touches the IBKR path (`make smoke`
against a Gateway, `make restart-daemon`, the redacted before and after
`--json` artifacts of the trading contract template), and for the OpenD
path a second daemon stack in place of the Gateway smoke, since no Gateway
exercises OpenD code and `make restart-daemon` installs over and restarts
the operator's own daemon with the operator's config. That stack meets
what the architecture handbook requires under "Running more than one
instance": the worktree's `bin/canary daemon --foreground` with its own
config file (`[broker] kind = "opend"`, `[opend]` pointed at the
simulator), its own `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, `XDG_CACHE_HOME`
and `XDG_RUNTIME_DIR` under a scratch directory, its own
`CANARY_CONFIG`, its own `CANARY_SOCKET`, and its own `HOME`. Until C1 the four policy files do not follow the XDG
roots: the rulebook, protection and opportunity defaults are
`~/.config/ibkr/policies/…` (`internal/config/config.go:208`, `214`,
`302`, expanded through `os.UserHomeDir`), the risk constitution is a
hard-coded `~/.config/ibkr/policies/risk-policy.toml` with no config key
(`risk_policy_manager.go:23`), and `Start` writes templates, migrations
and backups for all four (`server.go:1542`, `policy_files.go:283-288`).
C1 moves the four defaults onto the XDG config root `config.toml` already
resolves through (`config.DefaultPath`: `$XDG_CONFIG_HOME/ibkr`, else
`$HOME/.config/ibkr`), with no new key, because a permanently running
second daemon under its own LaunchAgent and XDG roots needs it and not only
the witness (the multi-broker review, 2026-11-04); the operator's paths
are unchanged, since this Mac's daemon sets no `XDG_*` variable. The
witness keeps its own `HOME` for the paths that still resolve through it
(`flex-token`, the backups' home expansion until each is moved), so
nothing of the operator's is ever rewritten. The CLI commands run from the same worktree binary with the
same seven variables, not the socket alone: a CLI call
that finds no socket autospawns a daemon with no flags
(`cmd/canary/main.go:139-142`, `internal/dial/autospawn.go:361`), which
would read the operator's config and `daemon.db` from the default roots
onto the scratch socket. The dual-daemon witness proves the hazard closed:
with the OpenD daemon stopped and its socket gone, a `bin/canary status
--json` carrying the OpenD stack's environment and autospawn on reports
`broker` `opend` and the scratch account, never an IBKR daemon in its
place; the only other defence is the environment itself, so the
architecture handbook names the variables a second client must carry
(the multi-broker review, 2026-11-04). The operator's daemon, config and
state are never touched by an OpenD witness. The
regression spine runs by hand on local main after each landing.

**Phase A, the read seam (release tier: it is a broker-path change).**
A1 `internal/broker` types and interfaces, no caller. A2 the IBKR adapter
with the session, quotes and bars seams moved. A3 account and positions
through the seam. Proof per "What proves phase A"; the live part is pending
the next live session.

**Phase B, the client and the simulator (`make test` tier, plus the
dependency ack).** B1 the frame layer, the limits table and the simulator's book model,
all of which build without protobuf. B2 vendored definitions, generated
code, the manifest, the dependency and the gate change, in one commit, after
the owner's ack. B3 the session: `InitConnect`, keep-alive,
`GetGlobalState`, `Notify`, the version gate, against the simulator. B4
quotes, quota, bars, instrument lookup. B5 accounts, funds, positions,
orders, fills and the maximum-quantity read. Coverage at or above 80
percent at every B commit.

**Phase C, the daemon on OpenD (release tier: endpoint and status
behaviour, proven on the simulator witness).** C1 `[broker] kind`,
`[opend]`, discovery, the scope kind with the IBKR key bytes pinned. C2
the OpenD adapter for `internal/broker`. C3 status rows, data health
`not_applicable`, the `Not on OpenD` row, the capability field and the
`Capabilities` row's broker leg, MCP descriptions, the app and SPA copy,
docs regen. C4 the additive instrument identity in `rpc` and the MCP
schemas. C5 quota budgeting and snapshot delivery. Done when `canary
status`, `canary account`, `canary positions`, `canary brief` and the MCP
tools run against the simulator in process mode and the brief names what is
missing. The Desk re-pin follows C4 on local main.

**Phase D, trading on OpenD (owner decision later; release tier).** The
switch and the paper proof keys, the write binding on the pattern of
`brokerWriteTransactionBinding`, the lock observation and its status row,
the `GetMaxTrdQtys` preview leg, place and modify behind the trading tag,
`pkg/futu` in `TRADING_PKG_TESTS`, all proven first against the simulator
and then on a paper account.

## Confirm at build

Facts this note states from documentation or the public client, to be
confirmed against the vendored definitions and, where marked, the first
OpenD contact:

- The 44-byte header layout and SHA-1 rule (from the public client's
  `MESSAGE_HEAD_FMT`); that OpenD refuses rather than ignores
  `PacketEncAlgo_None` when it has an RSA key (first contact).
- Protocol ids as listed, from the public client's `ProtoId` table.
- Per-interface frequency limits beyond those named here (snapshot 60 per
  30 seconds and 400 per request; `GetMaxTrdQtys` 10 per 30 seconds per
  account; unlock 10 per 30 seconds): each interface page.
- The `errCode` and `retMsg` of a write-class request refused for want of
  unlock, and whether `GetMaxTrdQtys` on a real account needs the unlock
  (the page does not say): first contact, phase D.
- The quote-right levels per market as `QotRight` reports them.
- The seven-day window of the historical candle quota and its repeat rule
  (from the quota page, read 2026-11-04): the quota page, and the first
  real `Qot_RequestHistoryKLQuota` answer.
- `minServerVersion`: the documented current OpenD version at build time;
  the first real `InitConnect`.

## Owner decisions

| Decision | State |
| --- | --- |
| The `broker` field on `rpc.QueuedAuthTerms` in Desk's and the companion's allowlists | Needed before a queued authorisation is signed under OpenD (phase D). The field is absent on every IBKR term, so nothing changes until then; `TestQueuedTermsFieldsAreTheSignedContract` names it (found in WP8, 2026-11-04). |
| Topology: one daemon per broker, with aggregation and routing in Desk (the multi-broker review's option A) | Decided by the owner, 2026-11-04 16:01 CET. Phases A to C stay as written plus the two C1 changes the review named: the policy files on the XDG config root and the scope on the broker instance (applied to this design and the brief 2026-11-04 17:54 CET). The route is a term of every Desk order; a cross-book measure is a later policy decision, after a real Futu book exists. |
| The regime on OpenD | Decided by the owner, 2026-11-08 19:50 CET: "Yes, on OpenD" (relayed by the multi-broker review session and checked in its transcript). The regime runs on OpenD; an input OpenD does not supply is named in the publication in the reader's words rather than leaving a thinner regime unexplained. The warning's words are a draft until the owner approves them; nothing with them is installed. |
| FX rates on OpenD | Proposed 2026-11-04 22:20 CET: with the regime package. Without a rate the rulebook reports holdings outside the base currency as not measured. Which source supplies the rate is the decision. |
| The OpenD build to run | Open, the owner's; nothing here depends on it until phase D's paper proof. |
| Whether the OpenAPI agreement permits Canary | Read by the coordinating session 2026-11-07, with what could not be read: see the build handoff's "Next". |
| `google.golang.org/protobuf` as a direct dependency | Accepted by the owner, 2026-11-04 20:00 CET ("Yes, add protobuf"), the eighth direct product dependency. Added at v1.36.12 in the WP5 commit, the one that carries this row. |
| The additive instrument identity in `internal/rpc` | Accepted by the owner, 2026-11-05 07:47 CET ("Yes, add InstrumentID"), as the brief proposes it. Built in the WP10 commit, the one that carries this row; a value field rather than a pointer, the wire unchanged, for the reason in the identity section. |
| Phases A to C shipped without Edge and reconciliation, the gap named on the surface | Proposed: accept. Gates C3's wording; the code is the same either way. |
| Policy files under the XDG config root that `config.toml` already uses (`$XDG_CONFIG_HOME/ibkr/policies/`, else `$HOME/.config/ibkr/policies/`) | Follows the topology decision: a permanently running second daemon under its own LaunchAgent needs it, not only the witness stack (the multi-broker review, 2026-11-04; applied to C1 2026-11-04 17:54 CET). No new key. A no-op for this Mac's daemon, which runs with `HOME` and no `XDG_*` variable, so the operator's paths stay identical; an operator whose daemon environment sets `XDG_CONFIG_HOME` elsewhere gets a one-time migration note in the changelog. The witness stack keeps its own `HOME` for the paths that still resolve through it. |

The reasoning for the proposed decisions is in the build brief.
