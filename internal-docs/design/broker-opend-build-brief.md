# Build brief: Futu OpenD as a second broker

Updated: 2026-11-04 15:55 CET (after four cold reviews on 2026-11-04)
Status: brief for the implementing session; follows
[broker-opend.md](broker-opend.md), which is the design and the contract.
Owner decision 2026-11-03 09:49 CET. Nothing is built.

Read first, in this order: `AGENTS.md`, the architecture handbook
(`docs/docs/internals/architecture.md`, "Broker seam"), the trading contract
template (`.agents/docs/daemon-cli-trading-contract.md`),
[platform-settings.md](platform-settings.md), then the design. Work in a
worktree branched from local main; the main checkout carries other sessions'
edits. Commit per work package; land on local main by fast-forward after the
gate; push only on the owner's instruction.

## Order of landing

The seam lands first, the simulator and the client second, the daemon on
OpenD third. The order is forced by proof: the seam is proven by the
daemon's own tests and the next live session before any OpenD code exists;
the client is proven by the simulator before the daemon depends on it; the
daemon's OpenD leg is proven against the simulator in process mode before
any paper account exists.

```
WP1  internal/broker types                        (A1)  no caller, builds alone
WP2  IBKR adapter: session, quotes, bars          (A2)
WP3  IBKR adapter: account, positions, observation (A3)
WP4  pkg/futu frames, limits, book model, legs     (B1)  no protobuf yet
WP5  protobuf: vendored, generated, manifest, gate (B2)  needs the dependency ack
WP6  pkg/futu session + futusim server             (B3)
WP7  pkg/futu reads                                (B4, B5)
WP8  daemon on OpenD: kind, discovery, scope, adapter   (C1, C2)
WP9  surfaces: status, data health, capability, MCP, app, docs (C3)
WP10 rpc instrument identity                       (C4)  needs the identity ack
WP11 quota budgeting and snapshot delivery         (C5)
WP12 cmd/opend-sim and the witness run             (C done)
```

Parallel work and file sets. WP4 may run beside WP2 and WP3 in its own
worktree: WP2 and WP3 touch `internal/broker/ibkr` and the daemon files
named below and nothing under `pkg/futu` or the `Makefile`; WP4 touches
only new files under `pkg/futu` plus the `Makefile`'s `test-pkg` and
`pages-coverage` targets. WP5 follows WP4 and also edits the `Makefile`,
`go.mod`, `tools/go.mod` and `scripts/check-go-dependencies.sh`; WP6 and
WP7 follow WP5. WP8 follows WP3 and WP7 and is where the two file sets first
meet. WP10 waits for the identity ack and may land after WP9; WP9's OpenD
goldens are then regenerated once, as a reviewed diff. Two writers never
share a file set; when a package in the seam set and one in the client set
run at once, each is in its own worktree.

## Gate tiers

| Tier | Command | When |
| --- | --- | --- |
| Normal | `make check` | before every commit; it includes the Go test legs (`CHECK_TEST_LEGS`), so the `pkg/futu` coverage step in `test-pkg` is binding from WP4 on |
| Go and runtime | `make test` | WP1, WP4 to WP7, WP10, WP12: every work package here changes Go; one tier per package, and a package's gate line names it |
| Release, IBKR path | `make test`; then, in the next session with a Gateway, `make smoke`, `make restart-daemon`, and the redacted before and after artifacts (`canary status --json`, the changed command `--json`) | WP2, WP3 |
| Release, OpenD path | `make test`; then the witness stack below against the process simulator (WP12's binary; before WP12, the in-process witness test) and its before and after artifacts; the Gateway smoke proves nothing about OpenD code and is not claimed, and `make restart-daemon` is never run for OpenD work: it installs the worktree build to `$(PREFIX)/bin` and restarts the operator's own IBKR daemon with the operator's config | WP8, WP9, WP11 |
| Spine | `make regression-spine-check` by hand on local main after each landing (it mutates committed HEAD, so before a commit it would prove the previous one; pushes happen only on the owner's instruction, so CI is not where it runs) | after WP2, WP3, WP8, WP11 land |
| Docs | `make docs-regen` then `make docs-check`; `make account-data-check product-identity-check` | WP8 (config keys), WP9, WP10 (MCP schemas), and any Markdown |
| App | `make app-check` and `make app-render-check`, the rendered result inspected at real size | WP9 |

The witness stack for the OpenD path is a second daemon that never touches
the operator's daemon, config or state (it meets what the architecture
handbook requires under "Running more than one instance"): a scratch
directory `<dir>` holding `config.toml` with `[broker] kind = "opend"` and
`[opend]` pointed at the simulator's loopback port, and one environment
for every command of the witness, daemon and CLI alike: `XDG_CONFIG_HOME`,
`XDG_STATE_HOME`, `XDG_CACHE_HOME` and `XDG_RUNTIME_DIR` under `<dir>`,
`CANARY_CONFIG=<dir>/config.toml`, `CANARY_SOCKET=<dir>/canary.sock`, and
`HOME=<dir>/home`: until WP8 the four policy files and their backups
resolve under `$HOME/.config/ibkr/policies/` (`policy_files.go:283-288`),
and `Start` migrates and backs them up on every boot (`server.go:1542`),
so a witness run with the operator's `HOME` would rewrite the operator's
policy files (fifth read, 2026-11-04 16:30 CET; the brief had listed six
variables). WP8 moves the four defaults onto the XDG config root
`config.toml` already uses; the witness keeps `HOME` for the paths that
still resolve through it.
The daemon is the worktree's own build, `bin/canary daemon --foreground
--config <dir>/config.toml --socket <dir>/canary.sock --log stderr`; the
CLI commands are the same `bin/canary` by path, never `canary` from
`PATH`. The environment matters on the CLI side as much as the daemon's: a
CLI call that finds no socket autospawns `<executable> daemon` with no
flags (`cmd/canary/main.go:139-142`, `internal/dial/autospawn.go:361`),
which reads its config from `CANARY_CONFIG` or `$XDG_CONFIG_HOME` and
`daemon.db` from `$XDG_STATE_HOME`, so a `canary status` with only the
socket set would start the operator's IBKR configuration on the scratch
socket. With the full environment an autospawn, should one happen, starts
the same OpenD witness daemon; WP12's dual-daemon witness proves it (the
multi-broker review, 2026-11-04). The lock and startup lock derive from the
socket directory, `--log stderr` writes no crash file, `--foreground`
disables idle shutdown, and no app process or LaunchAgent is involved. The
report names `<dir>` and the simulator flags. The `make opend-sim` target
of WP12 starts the simulator; the stack itself is seven environment
variables and one command, so it gets no target that could be mistaken for
`restart-daemon`.

Report every gate with the HEAD it ran at and its stamp line (`gate check at
<describe> exit N`). A live proof that needs a Gateway is "proof pending:
next live session" until it ran; never "fixed". If a spine patch stops
applying after a work package (the positions-identity patch targets
`handlers.go`, which WP3 edits), re-basing it is a safety-test change: run
the spine on the previous revision first, re-base the patch in the same
commit, and say so in the commit body.

## Work packages

### WP1 `internal/broker` types (phase A1)

Files: `internal/broker/broker.go` (Kind, Instrument, SessionBinding,
Quote, Bar, Position, AccountSummary, Provenance, Load, SessionHealth, the
six interfaces and `Broker`), `internal/broker/doc.go`,
`internal/broker/broker_test.go`.

Tests: value semantics of `SessionBinding` (comparable; zero value is not
current, which holds because the adapter reads the generation after the
increment at `server.go:1949`; two bindings with the same epoch and
different generations are not equal); `Instrument` canonical string per broker (`ibkr:265598`,
`opend:HK.00700`); a compile-time assertion file that nothing in
`internal/broker` imports `pkg/ibkr`, `pkg/futu` or `internal/rpc` (`go
list -deps` in the test).

Gate: `make test`. Done: the package builds and is imported by nobody.

Before the first edit: read cold the fourth review's five responses in
[the review record](../reviews/2026-11-04-broker-opend-design-review.md)
(the witness stack's own `HOME`, the per-kind account accessor, the
key-form constructor for `core_rebalance.go`, the `CI_ONLY_TARGETS` line,
the TWS order-control dial path) against the code they name, and record
what you find in that file. No fifth reviewer read them (owner decision
2026-11-04 15:53 CET).

### WP2 IBKR adapter: session, quotes, bars (A2)

Files: `internal/broker/ibkr/adapter.go` (the struct over one
`*ibkrlib.Connector` and the daemon's generation of it, built per
connector in `server.go:1948` and discarded with it; the per-epoch side
table of concrete session bindings), `instrument.go` (Instrument to and from `ibkrlib.Contract`),
`quotes.go` (the subscription key is the symbol, as today), `bars.go`,
`session.go`, tests beside each. Daemon: `server.go` (the `broker` field,
construction under `ibkr`), `subs.go` (`ibkrMarketConnector` becomes
`broker.Quotes`; the downcast to `*ibkrlib.Connector` at `subs.go:131-136`
and `164-166` goes, the entry stores the neutral binding `Subscribe`
returned and rechecks it with `Current`), `market_data.go` (`chartReader` becomes `broker.Bars`;
the delayed retry moves into the adapter), `setups.go`,
`setup_resolve.go`, `mandate_history.go` (bars and resolve through the
seam), and the test fakes that implement those interfaces
(`setups_fake_test.go` and the fakes in the gateway-session-loss,
listener-rejection, peer-close and auto-endpoint tests).

Tests: the adapter's mapping is total for every `SecType` the daemon uses
(table test over STK, OPT, FUT, IND, BOND, BILL, CASH, BAG) in both
directions; quote and bar mappings against `pkg/ibkr` values; a neutral
binding captured on one epoch is not current after a reconnect and the
side table drops it; a binding captured before a connector rebuild
(`server.go:1948-1949`, new connector, bumped generation) is not current on
the new adapter even when the new connection's epoch counter repeats the
old value; the subscription manager's recheck across a reconnect and a
rebuild keeps today's resubscribe behaviour; `Resolve` with a stale binding fails as the concrete
path does today; every substituted daemon test keeps its assertions.

Gate: release tier, IBKR path; spine on the landed revision; `git diff
--exit-code internal/cli/testdata/golden pkg/ibkr` as the side check. Done:
the mapping and daemon tests above green; golden and `pkg/ibkr` side checks
zero; the live part recorded as pending the next live session.

### WP3 IBKR adapter: account, positions and the observation (A3)

Files: `internal/broker/ibkr/account.go`, `positions.go`; daemon
`account_authority.go` (`readAccountSnapshot` takes `broker.Account`),
`handlers.go` (positions through `broker.Positions`; the portfolio stream
health stays a typed field of `PositionsHealth`),
`data_health_rpc_observation.go` (`observeAccountRPC` takes the neutral
binding), `broker_evidence.go` untouched here (the kind is WP8).

Tests: the account snapshot's provenance and `as_of` survive the mapping;
nil money fields stay nil; positions health (stream current, download
complete) maps one to one; the data-health observation records the same
freshness and scope it does today for the same inputs; the daily P&L path
still reads the concrete connector and is untouched.

Gate: as WP2. Done: as WP2.

### WP4 `pkg/futu` frames, limits, book model, test legs (B1)

Files: `pkg/futu/doc.go`, `frame.go` (the 44-byte header: encode, decode,
SHA-1 check, `ErrFrame`), `limits.go` (the pacing table with source and
date per row; `Pacer`), `errors.go` (`Error`, `ErrPaced`,
`ErrServerVersion`, `ErrTradingDisabled`), `pkg/futu/futusim/book.go` (the
synthetic book: accounts, instruments, quotes walk, positions, orders,
fills, clock), `pkg/futu/futusim/testdata/book.json`, tests beside each;
`Makefile`: `test-pkg` gains `./pkg/futu/...` under `-race` in the default
build (the trading-tagged build of `pkg/futu` arrives in phase D, with its
own line outside `TRADING_PKG_TESTS` so `trading-package-scope-check`
stays exact) and a coverage step that runs `go test -coverpkg=./pkg/futu
./pkg/futu` and fails below 80 percent (the `awk` the Pages badge uses);
`pages-coverage` writes `pkg-futu.cov`, `.html` and `.json` beside the
`pkg/ibkr` set.

Tests: frame round-trip and every malformed-header case (bad flag, length
mismatch, SHA-1 mismatch, JSON format refused); the pacer refuses the
(n+1)th request in a window and admits again after it; the book is
deterministic for a fixed clock and seed; the book holds no value that
looks like an account figure (a test runs the account-data gate's
patterns over the fixture).

Gate: `make test`. Done: builds without protobuf; coverage at or
above 80 on `./pkg/futu`; `ci-gate-scope-check` green (no new target, the
steps live inside `test-pkg`).

### WP5 protobuf: vendored, generated, manifest, gate (B2)

Needs: the owner's ack of the dependency (below).

Files: `pkg/futu/internal/pb/proto/*.proto` (only the files WP6 and WP7
use: `Common`, `InitConnect`, `KeepAlive`, `GetGlobalState`, `Notify`,
`Qot_Common`, `Qot_Sub`, `Qot_RegQotPush`, `Qot_GetSubInfo`,
`Qot_GetBasicQot`, `Qot_UpdateBasicQot`, `Qot_GetSecuritySnapshot`,
`Qot_GetKL`, `Qot_RequestHistoryKL`, `Qot_RequestHistoryKLQuota`,
`Qot_GetStaticInfo`, `Trd_Common`, `Trd_GetAccList`, `Trd_GetFunds`,
`Trd_GetPositionList`, `Trd_GetOrderList`, `Trd_GetOrderFillList`,
`Trd_SubAccPush`, `Trd_UpdateOrder`, `Trd_UpdateOrderFill`,
`Trd_GetMaxTrdQtys`; phase D adds `Trd_PlaceOrder` and `Trd_ModifyOrder`;
`Trd_UnlockTrade.proto` is never vendored, its absence is part of the
guarantee), with `LICENSE` and a `NOTICE` naming the source repository and
commit; `pkg/futu/internal/pb/MANIFEST` (the SHA-256 of each vendored
`.proto`, the SHA-256 of each generated `.pb.go`, the `protoc` version and
the `protoc-gen-go` version used, and the SHA-256 of the `protoc` release
archive per supported platform; the `protoc` version in both of its
forms, `libprotoc 32.1` as `protoc --version` prints it and the
`protoc v5.32.1` form the generated header carries, because the regen
compares the first and `futu-proto-check` the second);
`pkg/futu/internal/pb/<lowername>/<File>.pb.go` generated, one Go package
per vendored file, because every Futu definition declares its own
`package` and most declare top-level `Request`, `Response`, `C2S` and
`S2C`, which one package cannot hold (the generator is run with
`--go_opt=module=github.com/osauer/canary/v2` and one
`--go_opt=M<File>.proto=<import path>` per vendored file, the imported
`Common`, `Qot_Common` and `Trd_Common` included, derived from the
vendored list, so the vendored files stay byte-identical to upstream and
their unresolvable `go_package` is never used); `tools/go.mod` pins
`google.golang.org/protobuf/cmd/protoc-gen-go` (resolved as `go -C tools
tool -n protoc-gen-go` and passed to `protoc` with `--plugin`; `protoc`
itself from `PATH`); `go.mod` adds `google.golang.org/protobuf`, and
`futu-proto-check` compares the tool pin, the runtime pin and the manifest
line; `scripts/check-go-dependencies.sh` allowlist and printed count;
`Makefile` targets `futu-proto-regen` (a developer action: refuses unless
`protoc --version` equals the manifest's, then regenerates and rewrites
the file hashes and version lines while preserving the hand-kept archive
checksum lines) and `futu-proto-check` (no `protoc`: recompute the
vendored and the generated hashes, compare both sets with the manifest,
check every generated file's header names the manifest's generator
versions), the check added to `CHECK_TARGETS` (an open list; no `ci.yml`
line is needed for it); a CI job (`.github/workflows/ci.yml`) that fetches
the `protoc` release named in the manifest, refuses it unless the
archive's SHA-256 matches the manifest, runs `make futu-proto-regen` as
its own `run:` line (the scope check's regex is line-anchored) and fails
in a separate step on `git diff --exit-code pkg/futu/internal/pb`. What
each check binds is stated in the design: `futu-proto-check` binds files
to the manifest and headers to the generator versions, and only the
regeneration proves the generated code came from these definitions.

Tests: `futu-proto-check` green, and red on each of two one-byte changes
in a scratch copy without a manifest update: a vendored definition and a
generated file; the regeneration zero-diff, run locally before the commit
and named in its body, and green in the CI job at the landed revision;
`go-dependencies-check` green with eight modules; `govulncheck` green;
the `vet`, `staticcheck` and `modernize` legs green on the generated
packages (they take every tracked package and the modernize analysers do
not skip generated files; if one is red, the remedy is to exclude
`./pkg/futu/internal/pb/...` from that analyser's package list, a change
outside this package's file set, named in the commit body).

Gate: `make test`. Done: one commit that builds, with the
dependency's purpose in one sentence in the commit body.

### WP6 `pkg/futu` session and the simulator server (B3)

Files: `pkg/futu/connection.go` (socket, serial numbers, dispatch,
pushes, request and reply matching by serial, the envelope to `Error`),
`connector.go` (lifecycle, `Start`, `CaptureSession`, `IsReady`, reconnect
with the heartbeat kept by received frames; constructed with the pinned
`accID`, takes the account's `trdEnv` from the first `Trd_GetAccList`
after `InitConnect` (a request without a `TrdHeader`), stamps both into
every `TrdHeader`, and is never re-scoped: before that reply, or when the
list does not report the pinned account, the connector is unscoped and
every trade-class request fails typed; the list is re-read on the daemon's
status poll inside the pacing table, and a list that drops the pinned
account or changes its `trdEnv` drops the session with a typed reason), `session.go` (`InitConnect`, `KeepAlive`,
`GetGlobalState`, `Notify` handling, `minServerVersion` declared from the
documentation and confirmed at the first real contact, `ErrServerVersion`,
the RSA-required refusal), `pkg/futu/futusim/server.go`
(listener, frame loop, `InitConnect` reply, keep-alive, global state,
notify pushes, the switches), `pkg/futu/futusim/record.go` (`-record` to
`pkg/futu/testdata/frames/`), `pkg/futu/testdata/frames/*.hex` (first set:
`init_connect_ok`, `init_connect_rsa_required`, `init_connect_old_server`,
`keep_alive`, `global_state_ready`, `global_state_disclaimer`,
`notify_kicked_out`); the client-and-simulator tests in package
`futu_test`.

Tests: connect, handshake, version gate below and at the minimum, keep-alive
cadence, a silent OpenD drops the session after two intervals, kicked-out
closes the session with a typed reason, the fixtures replay byte-exact and
the simulator re-emits them byte-exact; a test fails if any encoder in
`pkg/futu` can write protocol 2005; a `TrdHeader` built by the connector
always carries the pinned account and the environment read for it; an
account the simulator's list does not report leaves every trade-class
request refused with the typed error.

Gate: `make test`. Done: coverage at or above 80; the simulator starts in
process from a test in under a second.

### WP7 `pkg/futu` reads (B4, B5)

Files: `pkg/futu/quotes.go` (subscribe, push, basic quote, snapshot batches
of up to 400, the one-minute hold), `quota.go` (`Qot_GetSubInfo`, quota
pushes, `Load`), `bars.go` (`Qot_GetKL`, `Qot_RequestHistoryKL`, history
quota), `instruments.go` (`Qot_GetStaticInfo`), `accounts.go`
(`Trd_GetAccList`, `Trd_GetFunds`), `positions.go`, `orders.go` (order and
fill lists, `Trd_SubAccPush`, update pushes), `max_qtys.go`
(`Trd_GetMaxTrdQtys` as a read in both builds; no lock-state logic, which
is phase D); `futusim` handlers for each protocol with the quota and pacing
enforcement from `limits.go`; fixtures per protocol and case.

Tests: every read against the simulator with a scripted book; quota
exhaustion refuses the next subscribe with the typed error and the daemon-
facing `Load` reports it; the pacer and the simulator agree (one table,
asserted); a snapshot of 401 instruments is split into two requests; a
history request inside the seven-day window costs no quota on repeat;
`PacketID` carries the session's `connID` and an increasing serial on every
trade request.

Gate: `make test`. Done: coverage at or above 80; `pkg/futu`'s exported API
is documented (`go-doc-check`).

### WP8 the daemon on OpenD (C1, C2)

Files: `internal/config/config.go` (`Broker{Kind}` and `OpenD{Host, Port,
Account, ClientID}` with doc strings; the three policy-file defaults at
lines 208, 214 and 302 and their doc strings resolve under the XDG config
root `DefaultPath` already uses, `$XDG_CONFIG_HOME/ibkr/policies/` else
`$HOME/.config/ibkr/policies/`, no new key), `internal/daemon/risk_policy_manager.go`
(`riskPolicyDefaultPath` at line 23 resolves the same way; the operator's
paths stay identical because this Mac's daemon sets no `XDG_*` variable,
and the changelog carries a one-time note for an operator whose daemon
environment sets `XDG_CONFIG_HOME` elsewhere; the multi-broker review,
2026-11-04, for a permanently running second daemon under its own
LaunchAgent), `internal/broker/broker.go` (`Broker` gains `Scope() Scope`,
kind, account and mode, read from the instance's own current observation;
the daemon holds one instance per process, constructed from config at
start with the daemon's lifetime under both kinds, so the sites with no
connector still have a scope; the earlier rejection of this method assumed
a per-connector instance and is withdrawn, the multi-broker review,
2026-11-04), `docs/docs/reference/config.md`
(regenerated), `internal/discover/discover.go` and `process.go` (the OpenD
probe and process name, behind the kind), `internal/daemon/server.go`
(construct one broker by kind; under `opend` the concrete `connector` stays
nil, `Start` runs no IBKR discovery or connect (`server.go:1573`, `1642`),
and the reconnect gate is one early `return false` in `triggerReconnect`
(`server.go:2563-2590`), which covers all six of its callers
(`gatewayConnector` 2812, `gateway_app_watch.go:300`, `handlers.go:3610`,
`order_risk_authority.go:84`, `answer_path.go:221` and `274`): the gate
must sit there and not in `newConnector`, because `reconnectFlow` calls
`discover.Resolve` at `server.go:2627` and `2671` before any constructor
and, with `[gateway] port` unpinned, probes the four standard ports and on
no listener starts the app-watch poller; `newConnector` at
`server.go:1816` is the backstop, its signature becomes
`(*ibkrlib.Connector, error)` (a nil connector wrapped in
`connectAttempter` is a typed nil that `tryOneHandshake` would call) and
both callers, `buildAttempter` and `breadthConnectFlow`, handle the error;
the breadth flow (`1128`, `1247`, `1285`) is unreachable because
`s.breadth` is not constructed under `opend`; `gatewayConnector()` returns
the typed unavailable reason without arming the trigger for its 46
callers; the TWS order control's own client-0 dial at
`tws_order_control.go:245-265` is unreachable because its write
transaction refuses first (`broker_write_transaction.go:252`, `connector
== nil || !sessionCaptured`); the Flex fetch (gated by `cfg.Flex.Enabled`)
and the borrow-fee FTP dial are gated by kind here, so the claim is
"never opens the IBKR API socket and never dials an IBKR host"),
`broker_evidence.go` (`brokerStateScope.Kind` with the zero value meaning
`ibkr`, normalised in `sameBrokerScope` and every comparison; a per-kind
identity built once from config and held on the `Server`, the kind and the
pinned account, and the broker instance's `Scope()` method, which yields
the current scope from the instance's own observation: under `ibkr` the
endpoint account, port and connected account as today, under `opend` the
`trdEnv` the session's account list reported for the pin, or an unknown
mode before it; no free per-kind function, so a daemon holding N brokers
later changes the identity's source only; the current scope's reads at `broker_evidence.go:53` and `223`, and the same
`[gateway] account` reads at `broker_write_transaction.go:214` and `283`,
`mandate_verdict.go:174`, `handlers.go:3725`, `alert_shadow_cadence.go:455`
and `protection_order_snapshot.go:74`, go through it, as do
`currentBrokerStateScope` and the nine `brokerStateScopeFromSnapshot`
callers, whose free `accountModeForStatus` (`DU` prefix, else the IBKR
ports) would class every OpenD account `live`; the readers that derive a
mode from a stored account alone, `fill_quotes.go:318`, `534` and `561`,
`broker_permissions.go:310` and `409`, `handlers.go:3709`, use the same
per-kind mode (a stored OpenD record's account equals the pin, so its mode
is the observed `trdEnv` or unknown); the 27 empty literals stay; the
readers that rebuild a scope from a persisted record's body, about 25 of
the 50 field-naming literals, among them `mandate_reduction.go:192`,
`policy_identity.go:64` and `93`, `exercise_reservations.go:109`,
`desk_override.go:107`, `proposal_queue.go:231`, go through one
constructor that takes account, mode and kind from the body, and every
record body with an account and a mode gains the `omitempty` `broker`
field (the `rpc` proposal and opportunity snapshots, the daemon-local
exercise reservation and resolution records, and the four terms below);
the twelve sites that build a scope from `rpc.TradingStatus`
(`desk_execution.go:55`, `order_risk_authority.go:236` and `328`,
`exercise_reservations.go:141`, `protective_exit.go:402`,
`protective_oca_trading.go:37`, `tws_order_control.go:203`,
`delta_reduction.go:643`, `fill_quotes.go:676`,
`broker_permissions.go:310` and `409`) go through a status form of the
constructor, for which `rpc.TradingStatus` gains the additive `Broker`
kind; the one key reader, `core_rebalance.go:858-869`, uses the
constructor's key form (in a `|` key the account field is cut at its first
`:`, which no IBKR account contains; in a `:`-joined key the segment after
the family prefix is a kind when it equals a known non-IBKR kind name, else
the account); the 23 non-test daemon files, plus
`internal/risk`, that write the scope into a persisted key, fingerprint,
journal line or typed term go through one helper per shape, three shapes:
a string key carries the kind inside the account field, never in front of
a family prefix (`|` keys `strategy|<id>|<kind>:<account>|<mode>|<con>`
at `strategy_target.go:460`, `469` and `strategy_kill_live.go:157`; `:`
keys `core:<kind>:<account>:<mode>:<month>` at `core_rebalance.go:271`,
whose reader at `281` trims `core:` first); a hashed identity with a
format validator (`marketEventFeeRateScopeFingerprint` at
`market_events_fee_rate.go:1021`, validated at
`market_events_fee_rate_store.go:131-132` and `172-174`;
`risk.BuildAlertAuthorityScope`, `internal/risk/alert_candidate.go:309-342`;
`riskCapitalScopeKey`, `risk_capital_state.go:240-245`;
`recon_engine.go:273`; `cash_sweep_trace.go:82-83`; `dailyPnLScopeSource`,
`daily_pnl_close_capture.go:31-33`; `edgeScopeFingerprint`,
`edge_authority.go:496-504`) takes the kind as an extra hash input only
for a non-IBKR kind (any such kind, not `opend` by name), empty for IBKR
so the bytes are unchanged, formats and validators untouched; and the `omitempty` `Broker` field written for
any non-`ibkr` kind (never for `opend` by name; same bytes today) on
`rpc.DeskAuthorityTerms` (its field-wise account and mode
checks at `desk_execution_submit.go:204`, `desk_authority_fence.go:41` and
`desk_authority.go:179` move to the constructor and `sameBrokerScope`; its
byte and digest comparisons at `desk_authority_device.go:81` and
`desk_authority.go:149`, `162-166` are unchanged),
`rpc.QueuedAuthTerms` (`proposal_queue.go:231`), `rpc.DeskOverrideTerms`
(`desk_override.go:107`), `rpc.LevelingBundleTerms`
(`proposal_currency_leveling_bundle.go:213`, `233`) and
`rpc.AccountDataScope` (`internal/rpc/account_data_authority.go:49`);
`orderJournalRouteIdentity` (`order_read_model.go:1832-1836`) is in-memory
and order-path and stays with phase D), `trading_status.go` (the mode
comes from the per-kind scope function above, `accountModeForStatus`
keeping its IBKR body as the `ibkr` arm; the pin block at lines 134-149,
`gateway_account_unconfirmed`, `_unpinned`, `_not_concrete` and
`_mismatch`, reads its pins from `[opend]` and its connected account from
the OpenD session under `opend`, with hints that name `[opend]`;
`gateway_client_id_unpinned` and the client-id mismatch never fire under
`opend`, because OpenD assigns `connID` itself and `[opend] client_id` is
only a label Canary sends; `gateway_unavailable` at lines 125-127 fires
from the OpenD session's health under `opend`, with OpenD wording; the
`AccountOrigin` and `ClientIDOrigin` rows at lines 62, 78 and 80 read the
same per-kind pins, and the host, port and origin rows at lines 54-61,
71-76 and 84-90 read `[opend]` under `opend`; the `cfg.Gateway.Account !=
""` guards at lines 153 and 157 become the per-kind pin, so the paper and
live endpoint checks still run under `opend`; and the
`*_endpoint_unconfirmed` blockers gain the OpenD reading; `[opend]
account` is required at startup, so `_unpinned` cannot fire under `opend`,
and a pinned account the list does not report is `_mismatch` and fails
closed with no mode), `internal/broker/opend/*.go` (the adapter over
`*futu.Connector`), `auto_endpoint.go` (rediscovery only for IBKR).

Tests: config decode with and without `[broker]`; a kind other than the two
refuses startup with a clear error; `kind = "opend"` without `[opend]
account` refuses startup with the documented message; a daemon under
`opend` constructs no IBKR connector and every IBKR-only read answers
unavailable with the typed reason; the listening test does not bind the
four standard ports (on the operator's machine the skip would remove the
test exactly where a wrong gate would dial the live TWS): it opens four
`127.0.0.1:0` listeners whose `Accept` fails the test, injects them
through `discover.StandardPorts` with the restore pattern of
`auto_endpoint_test.go:283-288`, leaves `[gateway] port` unpinned so the
probe would run, replaces neither `discover.Probe` nor
`s.attempterFactory`, leaves `CashLedger.URL` empty and `Flex.Enabled`
false, runs without `t.Parallel()`, and drives an `opend` daemon through
`Start`, the status and brief reads, a breadth question, a reconnect
trigger, the auto-endpoint path, the app watch and a TWS cancel attempt;
under `opend` with `[gateway]` absent the current scope's account equals
`[opend] account` before any session and its mode is unknown, and after
the account list reports the pin the scope is concrete with the list's
`trdEnv` (both states pinned); a simulate account never passes the live
endpoint check; a core decision event stored under `opend` matches its
frozen scope; the trading-status pin block under `opend` names `[opend]`
in every hint and never `[gateway]`; a fixture test pins the IBKR key and
fingerprint bytes of every scoped document family (broker permissions,
proposal and opportunity snapshots, desk executions, core rebalance, the
frozen strategy scope, the fee-rate, alert-authority, risk-capital,
reconciliation, cash-sweep, daily P&L and Edge fingerprints) and the IBKR
JSON of the four terms and `rpc.AccountDataScope` to today's values,
asserts that the zero kind and `ibkr` produce the same bytes, that no IBKR
account in the fixtures contains `:`, that a record stored under `opend`
rebuilds to a scope equal to the current one, that an OpenD scope's keys
carry the kind inside the account field and its terms the `Broker` field,
and that the same account id under `ibkr` and `opend` never produces the
same key, fingerprint or term bytes in any family, with the IBKR key bytes
unchanged (owner ask 2026-11-04 16:03 CET); the four policy defaults
resolve under `$XDG_CONFIG_HOME/ibkr/policies/` when the variable is set
and under the home directory when it is not, and `Start` in a stack with
its own `XDG_CONFIG_HOME` writes its templates there; `Scope()` under
`ibkr` reads as `currentBrokerStateScope` did before (the existing scope
tests substitute the instance); the Asia synthetic book keeps its IBKR scope; the OpenD
adapter against the simulator for every seam method.

Gate: release tier, OpenD path; spine on local main after landing; docs.
Done: `canary status --json` against the in-process simulator reports
`connected`, `connected_account`, `account_mode`, `gateway_host` and
`gateway_port` for the OpenD session (the OpenD block itself is WP9); the
before and after artifacts are in the report, redacted.

### WP9 surfaces (C3)

Files: `internal/rpc/rpc.go` (`HealthResult` gains `Broker`, the kind, a
stable field Desk binds as the order route under one daemon per broker
(the multi-broker review, 2026-11-04), and an `OpenD` block: program status, server version and build, logins,
markets authorised, quote rights, quotas; `TradingStatus.BrokerCapabilities`;
`PositionView.PriceSource`),
`internal/rpc/data_health.go` (source state `not_applicable`),
`internal/cli/status.go` (the OpenD rows in place of the TWS and API rows;
the `Not on OpenD` row; `Broker access` from the market list),
`internal/cli/trading.go` (`formatTradingCapabilities` gains the broker
leg), `internal/cli/render_golden_test.go` and new goldens
(`status_opend.txt`, `status_opend_disclaimer.txt`,
`trading_status_opend.txt`; the IBKR `trading_status*.txt` change by the
words `(what-if)`, a reviewed diff), `internal/daemon/data_health*.go` (the
IBKR-only sources report `not_applicable` under `opend`),
`internal/daemon/brief_overview.go` (the brief's coverage section names
Edge and reconciliation as not available on this broker where it would
otherwise show their figures),
`internal/mcp/tools.go` and the per-tool files (descriptions for the
IBKR-only tools; the typed `not_applicable` result),
`internal/app/live/service.go` (the live snapshot carries the kind),
`internal/app/http/routes.go` (`/api/bootstrap` at line 120 carries it),
the alert dispatcher (passes the kind into the presentation map),
`internal/app/alerts/presentation.go` (the connection-issue copy at lines
63-67 names the right process), `web/app/strategies.js` and
`web/app/opportunities.js` (the TWS strings read the kind from the
bootstrap),
`docs/docs/reference/mcp-tools.md` and `cli.md` (regenerated),
`docs/docs/internals/architecture.md` ("Broker seam" rewritten to the new
state, the overview's "one Interactive Brokers session" sentence widened,
and "Running more than one instance" naming the home directory beside the
XDG roots, since the policy files resolve through it; this is public
writing, so it gets a cold read by a reviewer who did not
write it and ships only on the owner's explicit go in that turn).

Tests: goldens; the MCP tools return the typed result, not an error, under
`opend`; parity check; data health never reports an IBKR-only source as
stale under `opend`; the brief's coverage section under both kinds; the app alert copy and the SPA strings under both
kinds (`make app-check`, `make app-render-check`).

Gate: release tier, OpenD path; docs; app, with the rendered SPA inspected
at real size and the screenshot named in the report; the handbook edit
waits for its go. Done: every new row
has a golden at 80 columns, colour on and off; the `Capabilities` row diff
is in the report as the one intentional IBKR golden change.

### WP10 `rpc` instrument identity (C4)

Needs: the owner's ack of the additive identity.

Files: `internal/rpc/rpc.go` (`InstrumentID`; the optional pointer on
`ContractParams`, `PositionView`, `Quote`, order and opportunity rows,
watchlist rows; `Market` vocabulary grows to `hk`, `sg`, `cn`, `jp`),
`internal/daemon` request decoding (accept `instrument` or `con_id`;
resolve through `broker.Instruments`), the IBKR adapter (fill `Broker:
ibkr, ID: <conid>` on responses), the OpenD adapter (fill
`opend:HK.00700`); `InstrumentID` carries no listing symbol, so the join
for netting one stock across brokers is the row's own `Symbol` with
`InstrumentID.Venue` (`PositionView.Symbol` and the symbol field of the
quote, order and opportunity rows), stated in the type's doc comment and
filled by both adapters for every row (the multi-broker review,
2026-11-04); `internal/mcp/tools.go` and `stock_add.go` (schemas),
`docs/docs/reference/mcp-tools.md` (regenerated).

Tests: JSON round-trip with and without the field; an older client's
request without it still resolves; the human renderers print no identity
struct (goldens unchanged for IBKR); every IBKR and OpenD row that carries
an identity also carries the join fields, symbol and venue; the MCP schema is additive (a test
diffs the required lists before and after).

Gate: `make test`; docs (the additive contract is proven by the round-trip
and schema tests above; no broker path or endpoint behaviour changes, so
it is not release tier). Done: Desk's re-pin is named in the report as the
follow-up, not done here.

### WP11 quota budgeting and snapshot delivery (C5)

Files: `internal/daemon/subs.go` (the budget decision at acquire: stream
for held and regime names, recomputed from the book, snapshot for the
rest, the typed fallback to snapshot when OpenD reports the quota spent,
the one-minute hold on release), `internal/daemon/market_data.go`
(snapshot batches on the OpenD leg), `internal/rpc/rpc.go`
(`Quote.Delivery`; `PositionView.PriceSource` set to `snapshot` by the
positions path when the mark came from a snapshot batch),
`internal/daemon/brief_overview.go` (the snapshot and quota sentence). No policy file and no setting: there is no number to
own.

Tests: a book whose names exceed the quota streams the held and regime
names and snapshots the rest, in that order; a new holding always takes a
line; a quota the simulator reports as spent by another client turns the
next acquire into a snapshot with the typed reason; a release before one
minute is deferred; `Delivery` is `snapshot` on snapshot-priced quotes and
absent on IBKR; `PriceSource` is `snapshot` on a position whose mark came
from a batch and absent otherwise; the brief's sentence appears only under `opend`.

Gate: release tier, OpenD path; spine on local main after landing. Done:
the brief against the simulator with a 100-line tier and 130 names reads
correctly.

### WP12 `cmd/opend-sim` and the witness run (C done)

Files: `cmd/opend-sim/main.go` (flags: `-listen`, `-book`, `-clock
fixed|now`, `-tier`, `-record`, `-lock-real`), `Makefile` target
`opend-sim` (build and run on a free loopback port), a short runbook in the
design doc's simulator section.

Witness: the witness stack from the gate table (its own config, XDG roots
and socket; the worktree's `bin/canary daemon --foreground`), against the
process simulator; `canary status`, `canary account`, `canary positions`,
`canary brief`, and the MCP tools through `canary mcp` from an MCP client,
each captured at real size (a screenshot or the terminal text at 80
columns); nothing to redact because the book is synthetic. The report names
the captures. The dual-daemon witness (owner ask 2026-11-04 16:03 CET):
an IBKR-shaped daemon on the existing simulation path and an OpenD daemon
on the simulator run at the same time, each with its own scratch
directory, XDG roots, `HOME`, config and socket; the witness proves that
neither sees the other's state, socket or policy files (the files under
each `<dir>` listed before and after, the other stack's untouched), that
`canary status --json` on each socket names its own broker kind and
account, and that a client of the two sockets receives distinct `Broker`
and account fields in `HealthResult` (a test in the integration package).
It then proves the autospawn hazard closed (the multi-broker review,
2026-11-04): with the OpenD daemon stopped and its socket gone, `bin/canary
status --json` under the OpenD stack's environment with autospawn on
reports `broker` `opend` and the scratch account, never an IBKR daemon in
its place, and the daemon it started is stopped by the witness; the
architecture handbook names the environment a second client must carry.

Gate: `make test`. Done: the five commands run against the simulator and
the brief names what is missing.

## What done looks like, overall

- Phases A to C landed on local main in the order above, each commit with
  its gate tier, HEAD and stamp line in its report, and the spine green on
  local main after each landing, run by hand and quoted.
- `pkg/futu` coverage at or above 80 percent at every commit; `pkg/ibkr`
  and the IBKR goldens unchanged except the `Capabilities` row's broker
  leg.
- The simulator runs in process for tests and as a process for witnesses;
  its fixtures are the client's regression fixtures against the simulator,
  with the first real capture still to come.
- `canary status`, `account`, `positions`, `brief` and the MCP tools work
  against the simulator under `opend` and say what is not available, on
  the CLI, in MCP, in data health, in the app and in the SPA.
- Live proofs recorded as pending where no Gateway was reachable.
- Follow-ups named, not done: the Desk re-pin after WP10; phase D (trading
  on OpenD) after the owner's decision and the paper account.

## Owner decisions

Two are the owner's alone and stay open; nothing in WP1 to WP12 waits on
them: which Futu account and which OpenD build the owner runs, and whether
the OpenAPI agreement permits Canary at all (read inside OpenD at first
start). The build is against the simulator and touches no OpenD.

Three are proposed here for the owner's ack, each with where the build can
proceed without it.

**Accept `google.golang.org/protobuf` as a direct dependency (gates WP5
and after).** OpenD's bodies are protobuf, 77 message files in proto2 with
required fields; a hand-written encoder would be more code than the client
and would drift from the definitions on every OpenD release, which is the
failure the dependency exists to prevent. No upstream Go binding is
published as a module, so there is nothing lighter to take. The module is
maintained by the protobuf project, has a stable v1 API, and is the single
addition: the generator is pinned in `tools/go.mod`, generated code is
committed with a manifest, and a check leg binds each file to the manifest
without running `protoc`, so ordinary builds and the gate never need it.
`protoc` itself runs in two places only: on a developer's machine for a
regeneration, and in one CI job that downloads the release the manifest
names, refuses it unless the archive's SHA-256 matches the manifest, and
fails on any diff after regenerating; that download is part of what this
ack accepts. The dependency gate's allowlist grows from seven modules to
eight in the same commit. WP1 to WP4 proceed without the ack; WP5 is the first commit that
needs it.

**Accept the additive instrument identity in `internal/rpc` (gates
WP10).** Every cross-surface contract names instruments by ConID, SMART-
style exchange and a two-value market, which Futu's `HK.00700` cannot fill.
One optional `InstrumentID` struct (broker, id, market, venue, currency),
embedded as a pointer with `omitempty`, lets OpenD rows carry a real identity
and IBKR rows carry both, without removing or renaming a field: an older
reader ignores it, the human renderers do not print it, so the IBKR goldens
are unchanged, and the MCP schema change is additive. The alternative,
overloading `con_id` or `local_symbol` with Futu codes, would make an IBKR
identifier lie. Desk compiles Canary at a pinned version and sees the field
after a re-pin; that is a Desk change, after WP10 lands. WP1 to WP9 and WP11
proceed without the ack; OpenD rows carry `symbol`, `exchange` and
`currency` until WP10 and their goldens are regenerated once as a reviewed
diff.

**Ship phases A to C without Edge and reconciliation, the gap named on
the surface (gates WP9's wording only).** Both rest on IBKR Flex
statements; OpenD has order and fill history but no statement feed of that
shape, and building a statement-grade truth from fills is a project of its
own that should wait for a real account and a real question. Saying so in
`canary status`, in data health as `not_applicable`, in the brief's
coverage, in the MCP descriptions and in the app keeps the surface honest: a
reader never sees a stale or empty Edge where there is no Edge. The code is
the same whether the owner accepts or asks for more; only the wording of the
`Not on OpenD` row and the descriptions depend on it, so WP9 proceeds with
the wording in the design and changes it on the owner's word.
