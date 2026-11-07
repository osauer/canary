package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// Health carries OpenD's own state and what the broker does not serve; the verdict
// does not degrade for the gap.
func TestOpenDHealthCarriesItsOwnRows(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	sim.PushQuoteRights(3, 2)
	h := srv.statusHealthSnapshot()
	o := h.OpenD
	if o == nil || o.ProgramStatus != "Ready" || o.ServerVersion != int(futu.MinServerVersion) || o.MinServerVersion != int(futu.MinServerVersion) ||
		!o.QuoteLoggedIn || !o.TradeLoggedIn || o.Environment != "simulate" || o.ConnID == 0 || len(o.Markets) != 3 || len(o.NotServed) == 0 {
		t.Fatalf("OpenD block %+v", o)
	}
	if h.GatewayAPI != nil {
		t.Fatal("the TWS API row survives under OpenD")
	}
	caps := h.Trading.BrokerCapabilities
	if caps == nil || caps.Broker != "opend" || caps.Preview != rpc.BrokerPreviewMaxQuantity || caps.Edge || caps.Reconciliation {
		t.Fatalf("capabilities %+v", caps)
	}
}

func TestIBKRCapabilitiesReadWhatIf(t *testing.T) {
	caps := newTestServer(t).brokerCapabilities()
	if caps.Broker != "ibkr" || caps.Preview != rpc.BrokerPreviewWhatIf || !caps.Edge || !caps.Reconciliation || caps.Statements != "flex" {
		t.Fatalf("capabilities %+v", caps)
	}
}

// Data health never reports an IBKR-only source as stale under OpenD, and such a
// source never counts against the summary.
func TestOpenDDataHealthMarksIBKROnlySourcesNotApplicable(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	report, err := srv.handleDataHealth(rpc.DataHealthParams{Limit: 64})
	if err != nil {
		t.Fatal(err)
	}
	var notApplicable, connection int
	for offset := 0; ; {
		page, err := srv.handleDataHealth(rpc.DataHealthParams{Limit: 64, Offset: offset, Revision: report.Revision})
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range page.Sources {
			if dataHealthIBKROnly(row.ID) {
				if row.State != rpc.SourceStatusNotApplicable || row.Required {
					t.Errorf("%s: state %s required %v", row.ID, row.State, row.Required)
				}
				notApplicable++
			}
			if row.ID == "broker_connection" {
				connection++
				if row.Provider != "OpenD" || row.State != "current" {
					t.Errorf("connection row %+v", row)
				}
			}
			if row.Provider == "IBKR" && row.State != rpc.SourceStatusNotApplicable {
				t.Errorf("%s still names IBKR as its provider: %+v", row.ID, row)
			}
		}
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if notApplicable == 0 || connection != 1 {
		t.Fatalf("not applicable %d, connection rows %d", notApplicable, connection)
	}
	for _, concern := range report.Concerns {
		if dataHealthIBKROnly(concern.SourceID) {
			t.Errorf("an IBKR-only source raised a concern: %+v", concern)
		}
	}
}

// These read the IBKR connector. With an OpenD session up they used to answer
// gateway_unavailable, and the CLI told the reader to start IB Gateway.
func TestMethodsOnTheIBKRConnectorAreNotServedWhileOpenDIsUp(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	for _, method := range []string{
		rpc.MethodMarketSnapshot, rpc.MethodPortfolioSnapshot, rpc.MethodMarketTape, rpc.MethodTechnical,
		rpc.MethodChainFetch, rpc.MethodChainExpiries, rpc.MethodGammaZeroSPX, rpc.MethodCoreResolve,
	} {
		res := dispatchRaw(t, srv, method, `{"symbol":"AAPL"}`)
		if res.Ok || res.Error.Code != rpc.CodeNotApplicable {
			t.Errorf("%s with the session up: %+v", method, res)
		}
	}
	if got := srv.brokerLinkBlockerCode(); got != brokerWritesNotServedCode {
		t.Errorf("readiness code with the session up: %s", got)
	}
	sim.Close()
	deadline := time.Now().Add(5 * time.Second)
	for srv.opend.Connector().IsReady() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := srv.brokerLinkBlockerCode(); got != "gateway_unavailable" {
		t.Errorf("readiness code with the session down: %s", got)
	}
}

// The tape joins on IBKR contract ids, so collecting it under OpenD would spend
// history quota and warn on every pass. It does not start.
func TestMarketTapeCollectionDoesNotStartUnderOpenD(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	time.Sleep(time.Second)
	load, err := srv.opend.Connector().RefreshLoad(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if load.HistoryUsed != 0 {
		t.Errorf("history quota spent at start: %d", load.HistoryUsed)
	}
}

// The WSH feed comes through the IBKR connector, so OpenD never tries it.
func TestOpenDHasNoWSHEarningsProvider(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	if srv.earnings.secondaryFetch != nil || srv.earnings.secondaryProvider != "" {
		t.Errorf("provider %q installed under OpenD", srv.earnings.secondaryProvider)
	}
}

// The borrow sources are off for this broker, not unknown.
func TestBorrowSourcesReadAsIBKROnlyUnderOpenD(t *testing.T) {
	c := &marketEventCache{noIBKRHosts: true, now: time.Now}
	_, fee, err := c.loadBorrowFees(context.Background())
	if err != nil || fee.Status != rpc.SourceStatusNotApplicable {
		t.Errorf("borrow fee: %+v %v", fee, err)
	}
	inventory := c.borrowInventory(context.Background(), nil, nil, nil, nil, time.Now(), nil)
	if inventory.Status != rpc.SourceStatusNotApplicable {
		t.Errorf("borrow inventory: %+v", inventory)
	}
}

// The IBKR-only methods answer a typed not_applicable under OpenD, not an error of
// any other kind; the same request under IBKR reaches its handler.
func TestIBKROnlyMethodsAnswerNotApplicable(t *testing.T) {
	for method := range ibkrOnlyMethods {
		if _, ok := rpc.LookupMethodTiming(method); !ok {
			t.Errorf("%s is not a known method", method)
		}
	}
	for _, method := range []string{rpc.MethodEdgeSnapshot, rpc.MethodReconStatus, rpc.MethodReportingStatus, rpc.MethodLendingMarket, rpc.MethodFinancingFees, rpc.MethodShortInterestScreen} {
		if !ibkrOnlyMethods[method] {
			t.Errorf("%s is not gated", method)
		}
	}
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	srv.dispatch(context.Background(), &rpc.Request{ID: "1", Method: rpc.MethodEdgeSnapshot}, enc, nil)
	var resp rpc.Response
	if err := json.Unmarshal([]byte(buf.String()), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ok || resp.Error == nil || resp.Error.Code != rpc.CodeNotApplicable {
		t.Fatalf("response %+v", resp)
	}
}

// The brief names what OpenD does not serve in its coverage, and IBKR's never does.
func TestBriefCoverageNamesTheGapOnlyUnderOpenD(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	res, err := srv.handleBriefSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !coverageNames(res, "Not on OpenD") {
		t.Fatalf("coverage %+v", res.Narrative.Overview.Coverage)
	}
	ibkr := composeBriefNarrative(&rpc.BriefResult{})
	for _, line := range ibkr.Overview.Coverage {
		for _, run := range line.Runs {
			if strings.Contains(run.Text, "Not on OpenD") {
				t.Fatalf("IBKR coverage names OpenD: %+v", line)
			}
		}
	}
}

func coverageNames(res *rpc.BriefResult, text string) bool {
	if res == nil || res.Narrative == nil || res.Narrative.Overview == nil {
		return false
	}
	for _, line := range res.Narrative.Overview.Coverage {
		for _, run := range line.Runs {
			if strings.Contains(run.Text, text) {
				return true
			}
		}
	}
	return false
}

// Each verdict is in the reader's words and does not depend on OpenD's free text.
func TestOpenDVerdictWords(t *testing.T) {
	for _, tc := range []struct {
		health futu.Health
		want   string
	}{
		{futu.Health{LastError: &futu.ServerVersionError{Server: 1000, Minimum: 1010}}, "below the 1010"},
		{futu.Health{LastError: &futu.HandshakeError{Reply: &futu.Error{Message: "x"}}}, "remove the RSA key"},
		{futu.Health{LastError: &futu.SessionEndError{Reason: futu.EndForceUpdate}}, "requires a newer Canary"},
		{futu.Health{LastError: &futu.SessionEndError{Reason: futu.EndKickedOut}}, "logged in elsewhere"},
		{futu.Health{LastError: &futu.SessionEndError{Reason: futu.EndDeviceLock}}, "log in to OpenD again"},
		{futu.Health{LastError: &futu.SessionEndError{Reason: futu.EndScopeLost}}, "no longer reports"},
		{futu.Health{LastError: &futu.SessionEndError{Reason: futu.EndHeartbeat}}, "stopped answering"},
		{futu.Health{Phase: futu.PhaseProgramStatus, State: futu.GlobalState{ProgramStatus: "Loging"}}, "program status is Loging"},
		{futu.Health{Phase: futu.PhaseNotLoggedIn}, "not logged in"},
		{futu.Health{Phase: futu.PhaseUnscoped}, "does not report"},
	} {
		if got := opendVerdict(context.Background(), tc.health, opendEndpoint(&configWithOpenD), false); !strings.Contains(got, tc.want) {
			t.Errorf("%+v: %q, want %q", tc.health, got, tc.want)
		}
	}
}

var configWithOpenD = config.Resolved{Broker: config.Broker{Kind: "opend"}, OpenD: config.OpenD{Account: opendTestAccount}}

// Only what this broker has is listed, so a healthy OpenD session reads ready and
// the rows that describe an IBKR session are absent rather than alarming.
func TestOpenDStatusListsOnlyWhatTheBrokerHas(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	statusOf := func(subsystems []rpc.SubsystemHealth) map[string]string {
		out := map[string]string{}
		for _, row := range subsystems {
			out[row.Name] = row.Status
		}
		return out
	}
	up := statusOf(srv.subsystemHealth(true, nil))
	if up["quote"] != "ready" || up["history"] != "ready" {
		t.Fatalf("connected subsystems %v", up)
	}
	for _, name := range []string{"gamma", "breadth", "edge", "recon", "regime", "hmds"} {
		if _, listed := up[name]; listed {
			t.Errorf("%q is listed under OpenD: %v", name, up)
		}
	}
	if down := statusOf(srv.subsystemHealth(false, nil)); down["quote"] != "unavailable" || down["history"] != "unavailable" {
		t.Fatalf("disconnected subsystems %v", down)
	}

	h := srv.statusHealthSnapshot()
	if h.GatewaySetup != nil || h.Executions != nil || h.AnswerPath != nil {
		t.Fatalf("an IBKR session row survives: setup %+v executions %+v answer path %+v", h.GatewaySetup, h.Executions, h.AnswerPath)
	}
}

// A brief under OpenD says which holdings its figures leave out.
func TestBriefNamesAHoldingItDoesNotAnalyse(t *testing.T) {
	book := futusim.Default()
	book.AddHolding(futusim.Instrument{Code: "HK.TCH261230C400000", Name: "SYNTH CALL", Type: "option", LotSize: 100, Currency: "HKD", Start: 12.5, Tick: 0.01}, 1000001, 2, 10)
	sim := futusim.Start(t, futusim.Options{Book: book})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	res, err := srv.handleBriefSnapshot(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !coverageNames(res, "TCH261230C400000") {
		t.Fatalf("coverage %+v", res.Narrative.Overview.Coverage)
	}
}
