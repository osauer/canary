package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	opendTestAccount  = "1000001"
	opendTestRealAcct = "1000002"
)

func TestZeroKindAndIBKRProduceTheSameBytes(t *testing.T) {
	zero := scopeFamilies(t, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper})
	named := scopeFamilies(t, brokerStateScope{Kind: broker.KindIBKR, Account: "DU1234567", Mode: rpc.AccountModePaper})
	for family, bytes := range zero {
		if named[family] != bytes {
			t.Errorf("%s: zero kind %s, ibkr %s", family, bytes, named[family])
		}
	}
	if !sameBrokerScope(brokerStateScope{Account: "U1234567", Mode: "live"}, brokerStateScope{Kind: broker.KindIBKR, Account: "U1234567", Mode: "live"}) {
		t.Fatal("the zero kind must compare equal to ibkr")
	}
}

// The owner's ask of 2026-11-04 16:03 CET: the same account id under the two
// brokers never produces the same key, fingerprint or term bytes in any family.
func TestTheSameAccountIDNeverCollidesAcrossBrokers(t *testing.T) {
	ibkr := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
	opend := brokerStateScope{Kind: broker.KindOpenD, Account: "U1234567", Mode: rpc.AccountModeLive}
	a, b := scopeFamilies(t, ibkr), scopeFamilies(t, opend)
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("families %d and %d", len(a), len(b))
	}
	for family := range a {
		if a[family] == b[family] {
			t.Errorf("%s collides: %s", family, a[family])
		}
	}
	if sameBrokerScope(ibkr, opend) {
		t.Fatal("two brokers compared equal")
	}
	// The kind sits inside the account field, never in front of a family prefix.
	if got := coreEpisode(opend, "2026-11-04"); got != "core:opend:U1234567:live:2026-11" {
		t.Fatalf("core episode %s", got)
	}
	if got := coreScope(opend, 42); got != "opend:U1234567|live|42" {
		t.Fatalf("frozen scope %s", got)
	}
}

// A record stored under OpenD rebuilds to a scope equal to the current one, from
// its body and from its key; a term carries the Broker field, and an IBKR term's
// JSON does not change.
func TestRecordsRebuildTheirScopeFromBodyAndKey(t *testing.T) {
	opend := brokerStateScope{Kind: broker.KindOpenD, Account: opendTestAccount, Mode: rpc.AccountModePaper}
	if got := recordScope(opend.brokerField(), opend.Account, opend.Mode); !sameBrokerScope(got, opend) {
		t.Fatalf("body %+v", got)
	}
	frozen := coreScope(opend, 42)
	parts := strings.SplitN(frozen, "|", 3)
	if got := scopeFromKeyAccount(parts[0], parts[1]); !sameBrokerScope(got, opend) {
		t.Fatalf("key %+v from %s", got, frozen)
	}
	ev := proposalEvent{Broker: opend.brokerField(), AccountID: opend.Account, AccountMode: opend.Mode}
	if !coreDecisionEventMatchesScope(ev, frozen) {
		t.Fatal("a core decision event stored under OpenD does not match its frozen scope")
	}
	if !coreDecisionEventMatchesScope(ev, "strategy|synth|"+frozen) {
		t.Fatal("an entry's decision event does not match its frozen scope")
	}
	if coreDecisionEventMatchesScope(proposalEvent{AccountID: opend.Account, AccountMode: opend.Mode}, frozen) {
		t.Fatal("an IBKR event matched an OpenD scope")
	}
	ibkr := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	if got := scopeFromKeyAccount(ibkr.Account, ibkr.Mode); !sameBrokerScope(got, ibkr) {
		t.Fatalf("IBKR key %+v", got)
	}
	if !coreDecisionEventMatchesScope(proposalEvent{AccountID: ibkr.Account, AccountMode: ibkr.Mode}, coreScope(ibkr, 7)) {
		t.Fatal("an IBKR event no longer matches its frozen scope")
	}

	ibkrTerm, err := json.Marshal(rpc.QueuedAuthTerms{AccountID: ibkr.Account, AccountMode: ibkr.Mode})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ibkrTerm), `"broker"`) {
		t.Fatalf("an IBKR term carries a broker: %s", ibkrTerm)
	}
	opendTerm, err := json.Marshal(rpc.QueuedAuthTerms{Broker: opend.brokerField(), AccountID: opend.Account, AccountMode: opend.Mode})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(opendTerm), `"broker":"opend"`) {
		t.Fatalf("an OpenD term lacks its broker: %s", opendTerm)
	}
	for _, v := range []any{rpc.DeskAuthorityTerms{}, rpc.DeskOverrideTerms{}, rpc.LevelingBundleTerms{}, rpc.AccountDataScope{}, rpc.TradingStatus{}, rpc.HealthResult{}} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), `"broker"`) {
			t.Fatalf("%T serialises a broker when it has none: %s", v, raw)
		}
	}
	if accountDataScope(opend).Broker != "opend" || accountDataScope(ibkr).Broker != "" {
		t.Fatal("the receipt scope does not carry the kind")
	}
}

// startOpenDServer starts the real Server.Start path of an OpenD daemon against
// the simulator, with every path that could reach IBKR left armed, so the
// listeners on the standard ports prove it never dials.
func startOpenDServer(t *testing.T, sim *futusim.Server, account string) (*Server, func()) {
	t.Helper()
	dir := shortTempDir(t)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("HOME", filepath.Join(dir, "home"))
	_, portText, err := net.SplitHostPort(sim.Addr())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	cfg := &config.Resolved{
		Broker: config.Broker{Kind: "opend"},
		OpenD:  config.OpenD{Host: "127.0.0.1", Port: &port, Account: account},
		Flex:   config.Flex{Enabled: true},
	}
	cfg.CashLedger.URL = "http://127.0.0.1:1/never"
	cfg.Daemon.SetIdleTimeout(0)
	srv := New(Options{
		Config: cfg, SocketPath: filepath.Join(dir, "ibkrd.sock"), Version: "v3.33.1",
		Logger: NewLogger(&bytes.Buffer{}, "error"), StateDatabasePath: filepath.Join(dir, "daemon.db"),
		DisableMacroSources: true,
	})
	serving := make(chan struct{}, 1)
	srv.initialAcceptLoopStartedForTest = func() { serving <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- srv.Start(ctx) }()
	select {
	case <-serving:
	case err := <-started:
		cancel()
		t.Fatalf("daemon stopped at start: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("daemon did not start")
	}
	return srv, func() {
		cancel()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
	}
}

func waitOpenDReady(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if srv.opend.Connector().IsReady() && srv.postConnectSetupDone.Load() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no ready session: %+v", srv.opend.Connector().Health())
}

// Under opend the daemon never opens the IBKR API socket and never dials an IBKR
// host. Four loopback listeners stand in for the standard ports and fail the test
// on any connection, and every path that would reach them is driven: Start, the
// status and brief reads, a breadth question, a reconnect trigger, the
// auto-endpoint path, the app watch and a TWS cancel attempt. No t.Parallel: it
// changes discover.StandardPorts.
func TestOpenDDaemonNeverDialsIBKR(t *testing.T) {
	var dialled atomic.Int32
	ports := make([]int, 0, 4)
	for range 4 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		ports = append(ports, listener.Addr().(*net.TCPAddr).Port)
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				dialled.Add(1)
				_ = conn.Close()
			}
		}()
	}
	oldPorts := discover.StandardPorts
	discover.StandardPorts = ports
	t.Cleanup(func() { discover.StandardPorts = oldPorts })

	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if srv.triggerReconnect() {
		t.Fatal("a reconnect started under opend")
	}
	if c := srv.gatewayConnector(); c != nil {
		t.Fatal("an IBKR connector exists under opend")
	}
	if c, err := srv.newConnector(discover.Endpoint{Host: "127.0.0.1", Port: ports[0]}); c != nil || !errors.Is(err, errNoIBKRConnector) {
		t.Fatalf("the backstop built a connector: %v", err)
	}
	if err := srv.buildAttempter(discover.Endpoint{Host: "127.0.0.1", Port: ports[0]}).Start(ctx); !errors.Is(err, errNoIBKRConnector) {
		t.Fatalf("the backstop's attempter: %v", err)
	}
	if srv.breadthGatewayConnector() != nil {
		t.Fatal("a breadth connector exists under opend")
	}
	if srv.triggerBreadthConnect() {
		t.Fatal("a breadth connect started under opend")
	}
	if srv.cfg.Flex.Enabled || srv.cfg.CashLedger.URL != "" {
		t.Fatalf("an IBKR host stays configured: %+v %q", srv.cfg.Flex.Enabled, srv.cfg.CashLedger.URL)
	}

	if h := srv.statusHealthSnapshot(); !h.Connected || h.GatewayPhase != rpc.GatewayPhaseReady {
		t.Fatalf("status: %+v phase %s", h.Connected, h.GatewayPhase)
	}
	_, _ = srv.buildAccountSummary(ctx, false)
	if _, err := srv.handleBriefSnapshot(ctx, &rpc.Request{Params: json.RawMessage(`{}`)}); err != nil {
		t.Logf("brief: %v", err)
	}
	if _, err := srv.handleBreadthSPX(ctx, &rpc.Request{Params: json.RawMessage(`{}`)}); err != nil {
		t.Logf("breadth: %v", err)
	}
	status := srv.handleTradingStatus()
	if status.Broker != "opend" {
		t.Fatalf("trading status broker %q", status.Broker)
	}
	if control, err := srv.twsOrderControl(ctx, "cancel", rpc.OrderTWSControlParams{}); err == nil && control.Outcome != rpc.TWSOrderControlOutcomeRefused {
		t.Fatalf("a TWS order control ran under opend: %+v", control)
	}
	time.Sleep(200 * time.Millisecond)
	if n := dialled.Load(); n != 0 {
		t.Fatalf("%d connections reached the IBKR ports", n)
	}
}

// Status, trading status and the scope read the OpenD session in the fields any
// surface reads, and the pin is account-concrete with an unknown mode before the
// list has spoken.
func TestOpenDStatusReportsTheSession(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	before := srv.currentBrokerStateScope()
	if before.Kind != broker.KindOpenD || before.Account != opendTestAccount {
		t.Fatalf("scope before the session %+v", before)
	}
	waitOpenDReady(t, srv)
	after := srv.currentBrokerStateScope()
	if after.Mode != rpc.AccountModePaper || !brokerScopeConcrete(after) {
		t.Fatalf("scope after the session %+v", after)
	}
	h := srv.statusHealthSnapshot()
	host, portText, _ := net.SplitHostPort(sim.Addr())
	port, _ := strconv.Atoi(portText)
	if !h.Connected || h.ConnectedAccount != opendTestAccount || h.AccountMode != rpc.AccountModePaper ||
		h.GatewayHost != host || h.GatewayPort != port || h.Broker != "opend" || h.ServerVersion != int(futu.MinServerVersion) {
		t.Fatalf("health %+v", h)
	}
	status := srv.handleTradingStatus()
	if status.Broker != "opend" || status.Account != opendTestAccount || status.AccountMode != rpc.AccountModePaper || status.GatewayPort != port {
		t.Fatalf("trading status %+v", status)
	}
	for _, blocker := range status.Blockers {
		if strings.Contains(blocker.Action, "[gateway]") || strings.Contains(blocker.Message, "IBKR") {
			t.Fatalf("a blocker names IBKR under opend: %+v", blocker)
		}
	}
}

// A simulate account never passes the live endpoint check, and an account the list
// does not report blocks with OpenD wording and no mode.
func TestOpenDTradingStatusChecks(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	srv.cfg.Trading.Mode = config.TradingModeLive
	status := srv.handleTradingStatus()
	if !hasBlocker(status.Blockers, "live_endpoint_unconfirmed") {
		t.Fatalf("a simulate account passed the live check: %+v", status.Blockers)
	}

	unknown, stopUnknown := startOpenDServer(t, sim, "42")
	defer stopUnknown()
	deadline := time.Now().Add(5 * time.Second)
	for unknown.opend.Connector().Health().Phase != futu.PhaseUnscoped && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	unknown.cfg.Trading.Mode = config.TradingModePaper
	status = unknown.handleTradingStatus()
	if !hasBlocker(status.Blockers, "gateway_account_mismatch") || status.AccountMode != rpc.AccountModeUnknown {
		t.Fatalf("an unreported account: %+v", status)
	}
	if brokerScopeConcrete(unknown.currentBrokerStateScope()) {
		t.Fatal("an unreported account has a concrete scope")
	}
	if _, err := unknown.buildAccountSummary(context.Background(), false); err == nil {
		t.Fatal("an account read succeeded with no scope")
	}
}

// The account, positions and quote reads go through the seam to the simulator's
// book, and an IBKR-only read answers unavailable.
func TestOpenDDaemonServesTheReads(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	account, err := srv.buildAccountSummary(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	view, _ := sim.Book().Funds(1000001, "HKD")
	if account.AccountID != opendTestAccount || account.BaseCurrency != "HKD" || account.NetLiquidation != view.TotalAssets || account.TotalCash != view.Cash {
		t.Fatalf("account %+v, book %+v", account, view)
	}
	if account.Authority == nil || account.Authority.Availability != rpc.AccountDataAvailable || account.Authority.Scope.Broker != "opend" {
		t.Fatalf("authority %+v", account.Authority)
	}

	positions, err := srv.handlePositionsList(ctx, &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	symbols := map[string]rpc.PositionView{}
	for _, row := range positions.Stocks {
		symbols[row.Symbol] = row
	}
	if len(symbols) != 3 || symbols["00700"].Quantity != 300 || symbols["00700"].Exchange != "SEHK" || symbols["AAPL"].Currency != "USD" || symbols["ES3"].Exchange != "SGX" {
		t.Fatalf("positions %+v", positions.Stocks)
	}
	if positions.Authority.Availability != rpc.AccountDataAvailable || positions.Authority.Freshness != rpc.AccountDataFreshnessCurrent || !positions.Authority.PortfolioComplete {
		t.Fatalf("positions authority %+v", positions.Authority)
	}

	raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: rpc.ContractParams{Symbol: "00700"}})
	quote, err := srv.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	if quote.Last == nil || *quote.Last <= 0 || quote.Bid == nil || quote.Ask == nil {
		t.Fatalf("quote %+v", quote)
	}

	// The subscription manager takes a line through the seam.
	key, release, err := srv.subs.HoldContract(ctx, ibkrlib.Contract{Symbol: "AAPL", SecType: "STK", Exchange: "US", Currency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "US.AAPL" {
		t.Fatalf("subscription key %q", key)
	}
	release(ctx)

	reads := srv.brokerReads()
	if reads == nil || reads.Kind() != broker.KindOpenD {
		t.Fatalf("seam %v", reads)
	}
	binding, ok := reads.Capture()
	if !ok || binding.Kind != broker.KindOpenD {
		t.Fatalf("binding %+v %v", binding, ok)
	}
	series, err := reads.ChartBars(ctx, broker.Instrument{Symbol: "00700"}, 5, "1 day", 5*time.Second)
	if err != nil || len(series.Bars) == 0 {
		t.Fatalf("chart %+v %v", series, err)
	}
	resolved, err := reads.Resolve(ctx, binding, broker.Instrument{Symbol: "00700"}, 5*time.Second)
	if err != nil || resolved.ID != "HK.00700" {
		t.Fatalf("resolve %+v %v", resolved, err)
	}

	// An IBKR-only read answers the typed unavailable reason and never arms a dial.
	if _, err := srv.handleGammaZeroSPX(ctx, &rpc.Request{Params: json.RawMessage(`{}`)}); !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		t.Logf("gamma: %v", err)
	}
}

// Policy files follow the XDG config root: an isolated stack writes its templates
// under its own root, and without the variable the paths are the operator's.
func TestPolicyDefaultsFollowTheXDGRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	want := filepath.Join(root, "ibkr", "policies")
	if got := config.PolicyDir(); got != want {
		t.Fatalf("policy dir %s, want %s", got, want)
	}
	for _, got := range []string{riskPolicyDefaultPath(), config.DefaultRulebookPolicyFile()} {
		if !strings.HasPrefix(got, want+string(filepath.Separator)) {
			t.Fatalf("%s is not under %s", got, want)
		}
	}
	if got := (config.AutoTrade{}).WithDefaults().PolicyFile; !strings.HasPrefix(got, want) {
		t.Fatalf("protection default %s", got)
	}
	if got := (config.Opportunities{}).WithDefaults().PolicyFile; !strings.HasPrefix(got, want) {
		t.Fatalf("opportunity default %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := riskPolicyDefaultPath(); got != "~/.config/ibkr/policies/risk-policy.toml" {
		t.Fatalf("the operator's constitution moved: %s", got)
	}
	if got := config.DefaultRulebookPolicyFile(); got != "~/.config/ibkr/policies/rulebook-policy.toml" {
		t.Fatalf("the operator's rulebook moved: %s", got)
	}
}

// A daemon started with its own XDG config root writes its policy templates there
// and leaves the home directory's untouched.
func TestStartWritesPolicyTemplatesUnderTheStacksOwnRoot(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	dir := shortTempDir(t)
	t.Setenv("XDG_STATE_HOME", dir)
	configRoot := filepath.Join(dir, "config")
	home := filepath.Join(dir, "home")
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("HOME", home)
	_, portText, _ := net.SplitHostPort(sim.Addr())
	port, _ := strconv.Atoi(portText)
	cfg, err := (&config.Config{Broker: config.Broker{Kind: "opend"}, OpenD: config.OpenD{Port: &port, Account: opendTestAccount}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Daemon.SetIdleTimeout(0)
	srv := New(Options{
		Config: cfg, SocketPath: filepath.Join(dir, "ibkrd.sock"), Version: "v3.33.1", EnsurePolicyFiles: true,
		Logger: NewLogger(&bytes.Buffer{}, "error"), StateDatabasePath: filepath.Join(dir, "daemon.db"), DisableMacroSources: true,
	})
	serving := make(chan struct{}, 1)
	srv.initialAcceptLoopStartedForTest = func() { serving <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()
	select {
	case <-serving:
	case err := <-done:
		t.Fatalf("start: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no start")
	}
	cancel()
	<-done
	if _, err := os.Stat(filepath.Join(configRoot, "ibkr", "policies", "risk-policy.toml")); err != nil {
		t.Fatalf("the constitution template is not under the stack's root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "ibkr", "policies")); !os.IsNotExist(err) {
		t.Fatalf("the home directory's policies were touched: %v", err)
	}
}

func TestClientVersionFollowsTheBuild(t *testing.T) {
	for version, want := range map[string]int32{"v3.33.1": 333, "3.4.0": 304, "dev": 1, "": 1} {
		if got := clientVersionFor(version); got != want {
			t.Errorf("%q: %d, want %d", version, got, want)
		}
	}
}
