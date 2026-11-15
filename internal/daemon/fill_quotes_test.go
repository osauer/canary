package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/discover"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

var fillQuoteTestAt = time.Date(2026, 9, 5, 12, 0, 5, 0, time.UTC)

func closeTo(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

func newFillQuoteTestServer(t *testing.T, dbPath string) (*Server, *corestore.Store) {
	t.Helper()
	if err := os.Chmod(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	core, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	rt, err := bindFillQuoteRuntime(t.Context(), core)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{now: func() time.Time { return fillQuoteTestAt }, logger: NewLogger(nil, "error"), fillQuotes: rt}
	return s, core
}

func fillQuoteTestJournalFill(execID string) orderJournalEvent {
	return orderJournalEvent{
		Version: orderJournalFileVersion, At: fillQuoteTestAt, Type: orderJournalEventStatusUpdated, Status: "Execution",
		OrderRef: "synthetic-ref", PreviewTokenID: "tok-1", Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper",
		Symbol: "SYNX", SecType: "OPT", ConID: 900001, Exchange: "CBOE", Currency: "USD", LocalSymbol: "SYNX  261016C00100000",
		Expiry: "20261016", Strike: 100, Right: "C", Multiplier: 100, Action: "BUY", Quantity: 2, Filled: 2,
		ExecID: execID, ExecTime: "20260905-12:00:03", ExecShares: 2, ExecSide: "BOT", LastFillPrice: 1.30,
	}
}

func TestFillQuoteAtEventDescribesThePreviewQuoteAndItsGap(t *testing.T) {
	t.Parallel()
	eventAt := fillQuoteTestAt
	q := rpc.OrderQuoteSnapshot{Bid: new(1.20), Ask: new(1.30), Last: new(1.24), BidSize: new(12), AskSize: new(8), DataType: rpc.MarketDataLive,
		PriceAt: eventAt.Add(-800 * time.Millisecond), AsOf: eventAt.Add(-200 * time.Millisecond)}
	got := fillQuoteAtEvent(q, eventAt)
	if got == nil || got.Status != rpc.FillQuoteQuoted || got.Mid == nil || *got.Mid != 1.25 {
		t.Fatalf("preview quote = %+v, want quoted with mid 1.25", got)
	}
	if got.GapMS == nil || *got.GapMS != -800 || !got.QuoteAt.Equal(q.PriceAt) || *got.BidSize != 12 || *got.AskSize != 8 {
		t.Fatalf("preview quote lost its gap, time or sizes: %+v", got)
	}
	oneSided := fillQuoteAtEvent(rpc.OrderQuoteSnapshot{Ask: new(1.30), AsOf: eventAt}, eventAt)
	if oneSided.Status != rpc.FillQuoteUnavailable || oneSided.Reason != "quote_not_two_sided" || oneSided.Ask == nil {
		t.Fatalf("one-sided preview quote = %+v", oneSided)
	}
	if fillQuoteAtEvent(rpc.OrderQuoteSnapshot{}, eventAt) != nil {
		t.Fatal("an empty quote must not produce a record")
	}
	raw, err := json.Marshal(orderJournalEvent{Version: orderJournalFileVersion, Type: orderJournalEventPreviewed, Quote: got})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeOrderJournalLine(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Quote == nil || *decoded.Quote.Mid != 1.25 || *decoded.Quote.GapMS != -800 {
		t.Fatalf("journal round trip lost the quote: %+v", decoded.Quote)
	}
}

func TestFillQuoteCaptureRetainsTheQuoteOnceAndSurvivesRestart(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "daemon.db")
	s, core := newFillQuoteTestServer(t, dbPath)
	reads := 0
	s.fillQuotes.quote = func(_ context.Context, contract rpc.ContractParams, _ time.Duration) (fillQuoteRead, error) {
		reads++
		if contract.ConID != 900001 || contract.Exchange != "SMART" || contract.Multiplier != 100 {
			t.Fatalf("quote read for %+v, want the exact option routed through SMART", contract)
		}
		return fillQuoteRead{Bid: new(1.20), Ask: new(1.30), Last: new(1.22), BidSize: new(40), AskSize: new(25), DataType: rpc.MarketDataLive, QuoteAt: fillQuoteTestAt.Add(1200 * time.Millisecond)}, nil
	}
	s.fillQuotes.fx = func(context.Context, string) (string, *float64, string, string) { return "EUR", new(0.9), "cache", "" }

	fill := fillQuoteTestJournalFill("0000e0d5.01")
	s.offerFillQuoteCapture(fill)
	s.offerFillQuoteCapture(fill)
	s.offerFillQuoteCapture(orderJournalEvent{Type: orderJournalEventBrokerAcknowledged, ExecID: "ignored", ExecShares: 1})
	s.captureQueuedFillQuotes(t.Context())
	if reads != 1 {
		t.Fatalf("quote reads = %d, want exactly one per execution", reads)
	}
	rec, ok := s.FillQuoteByExecID("0000e0d5.01")
	if !ok {
		t.Fatal("captured fill missing from the lookup")
	}
	if rec.Source != rpc.FillQuoteSourceCanary || rec.Seen != rpc.FillQuoteSeenCallback || rec.Side != rpc.OrderActionBuy || rec.Quantity != 2 || rec.Price != 1.30 {
		t.Fatalf("record identity = %+v", rec)
	}
	if rec.ExecTimeSource != "broker_exec_time" || !rec.ExecTime.Equal(time.Date(2026, 9, 5, 12, 0, 3, 0, time.UTC)) {
		t.Fatalf("execution time = %s (%s)", rec.ExecTime, rec.ExecTimeSource)
	}
	if rec.Quote.Status != rpc.FillQuoteQuoted || *rec.Quote.Mid != 1.25 || *rec.Quote.GapMS != 3200 || *rec.Quote.BidSize != 40 {
		t.Fatalf("quote = %+v, want mid 1.25 read 3.2 s after the fill", rec.Quote)
	}
	if rec.BaseCurrency != "EUR" || rec.FXRate == nil || *rec.FXRate != 0.9 || rec.FXSource != "cache" {
		t.Fatalf("fx = %s %v %s", rec.BaseCurrency, rec.FXRate, rec.FXSource)
	}
	if _, ok := s.FillQuoteNear(900001, fillQuoteTestAt, 10*time.Second, nil); !ok {
		t.Fatal("lookup by contract and time missed the fill")
	}
	if _, ok := s.FillQuoteNear(900001, fillQuoteTestAt.Add(time.Hour), 10*time.Second, nil); ok {
		t.Fatal("lookup by contract and time matched outside the tolerance")
	}

	// A restart reloads the store; the poll then re-finding the same exec id
	// is a duplicate, and a new one is captured.
	reloaded, err := bindFillQuoteRuntime(t.Context(), core)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.records["0000e0d5.01"]; !ok {
		t.Fatal("restart lost the retained fill quote")
	}
	status := rpc.TradingStatus{Account: "DU1234567", Mode: "paper", ClientID: 15}
	known := fillQuoteFillFromExecution(ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventExecDetails, ExecID: "0000e0d5.01", ClientID: 15, ClientIDPresent: true, Shares: 2, Price: 1.3}, status, fillQuoteTestAt)
	if reloaded.enqueue(known) {
		t.Fatal("poll re-queued an execution the store already holds")
	}
	fresh := fillQuoteFillFromExecution(ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventExecDetails, ExecID: "0000e0d5.02", ClientID: 0, ClientIDPresent: true, Account: "DU1234567", Symbol: "AAA", SecType: "STK", ConID: 900002, Currency: "USD", ExecutionSide: "SLD", Shares: 100, Price: 10.4, ExecTime: "20260905-12:30:00"}, status, fillQuoteTestAt)
	if fresh.record.Source != rpc.FillQuoteSourceBroker || fresh.record.Seen != rpc.FillQuoteSeenPoll || fresh.record.Side != rpc.OrderActionSell || fresh.record.Contract.Multiplier != 1 {
		t.Fatalf("polled TWS execution = %+v", fresh.record)
	}
	if !reloaded.enqueue(fresh) {
		t.Fatal("new polled execution was not queued")
	}
}

func TestFillQuoteCaptureRecordsAnUnreadableQuoteWithItsReason(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	s.fillQuotes.quote = func(context.Context, rpc.ContractParams, time.Duration) (fillQuoteRead, error) {
		return fillQuoteRead{}, context.DeadlineExceeded
	}
	s.fillQuotes.fx = func(context.Context, string) (string, *float64, string, string) {
		return "", nil, "", "base_currency_unknown"
	}
	fill := fillQuoteTestJournalFill("0000e0d5.09")
	fill.OrderRef, fill.PreviewTokenID = "", ""
	s.offerFillQuoteCapture(fill)
	s.captureQueuedFillQuotes(t.Context())
	rec, ok := s.FillQuoteByExecID("0000e0d5.09")
	if !ok {
		t.Fatal("unreadable quote must still retain the fill")
	}
	if rec.Source != rpc.FillQuoteSourceBroker || rec.Quote.Status != rpc.FillQuoteUnavailable || rec.Quote.Reason != "timeout" || rec.Quote.Mid != nil {
		t.Fatalf("record = %+v", rec)
	}
	if rec.FXRate != nil || rec.FXReason != "base_currency_unknown" {
		t.Fatalf("fx = %v %q", rec.FXRate, rec.FXReason)
	}
	other := fillQuoteFromRead(fillQuoteRead{Bid: new(2.0)}, nil, fillQuoteTestAt, fillQuoteTestAt, fillQuoteTestAt)
	if other.Status != rpc.FillQuoteUnavailable || other.Reason != "quote_not_two_sided" || other.Bid == nil {
		t.Fatalf("one-sided read = %+v", other)
	}
	if got := fillQuoteFromRead(fillQuoteRead{}, ibkrlib.ErrIBKRUnavailable, fillQuoteTestAt, fillQuoteTestAt, fillQuoteTestAt); got.Reason != "broker_unavailable" {
		t.Fatalf("broker unavailable reason = %q", got.Reason)
	}
	if got := fillQuoteFromRead(fillQuoteRead{}, errors.New("rejected"), fillQuoteTestAt, fillQuoteTestAt, fillQuoteTestAt); got.Reason != "quote_read_failed" {
		t.Fatalf("other failure reason = %q", got.Reason)
	}
}

func fillQuoteTestRecords() []rpc.FillQuoteRecord {
	at := fillQuoteTestAt
	quoted := func(bid, ask float64, gap int64) rpc.FillQuote {
		return rpc.FillQuote{Status: rpc.FillQuoteQuoted, Bid: &bid, Ask: &ask, Mid: new((bid + ask) / 2), DataType: rpc.MarketDataLive, QuoteAt: at.Add(time.Duration(gap) * time.Millisecond), GapMS: &gap}
	}
	return []rpc.FillQuoteRecord{
		{Version: 1, ExecID: "e3", Source: rpc.FillQuoteSourceBroker, Seen: rpc.FillQuoteSeenPoll, Mode: "paper",
			Contract: rpc.ContractParams{ConID: 900003, Symbol: "BBB", SecType: "STK", Currency: "USD", Multiplier: 1}, Side: rpc.OrderActionBuy, Quantity: 10, Price: 5,
			ExecTime: at.Add(time.Hour), ObservedAt: at.Add(time.Hour), Quote: rpc.FillQuote{Status: rpc.FillQuoteUnavailable, Reason: "timeout"}},
		{Version: 1, ExecID: "e1", Source: rpc.FillQuoteSourceCanary, Seen: rpc.FillQuoteSeenCallback, Mode: "paper", OrderRef: "synthetic-ref",
			Contract: rpc.ContractParams{ConID: 900001, Symbol: "SYNX", SecType: "OPT", Currency: "USD", LocalSymbol: "SYNX  261016C00100000", Expiry: "20261016", Strike: 100, Right: "C", Multiplier: 100},
			Side:     rpc.OrderActionBuy, Quantity: 2, Price: 1.30, ExecTime: at, ObservedAt: at, Quote: quoted(1.20, 1.30, 800), BaseCurrency: "EUR", FXRate: new(0.9), FXSource: "cache"},
		{Version: 1, ExecID: "e2", Source: rpc.FillQuoteSourceCanary, Seen: rpc.FillQuoteSeenCallback, Mode: "paper", OrderRef: "synthetic-ref-2",
			Contract: rpc.ContractParams{ConID: 900002, Symbol: "AAA", SecType: "STK", Currency: "USD", Multiplier: 1},
			Side:     rpc.OrderActionSell, Quantity: 100, Price: 10.40, ExecTime: at.Add(5 * time.Minute), ObservedAt: at.Add(5 * time.Minute), Quote: quoted(10.40, 10.60, 1200), BaseCurrency: "EUR", FXRate: new(0.9), FXSource: "live"},
		{Version: 1, ExecID: "e4", Source: rpc.FillQuoteSourceBroker, Seen: rpc.FillQuoteSeenPoll, Mode: "paper",
			Contract: rpc.ContractParams{ConID: 900004, Symbol: "SYNX", SecType: "OPT", Currency: "USD", Expiry: "20261120", Strike: 90, Right: "P"},
			Side:     rpc.OrderActionSell, Quantity: 1, Price: 2.00, ExecTime: at.AddDate(0, 1, 0), ObservedAt: at.AddDate(0, 1, 0), Quote: quoted(1.90, 2.10, 300), BaseCurrency: "EUR", FXRate: new(0.9)},
	}
}

func TestBuildReportingCostsMeasuresHalfSpreadAndTotals(t *testing.T) {
	t.Parallel()
	records := fillQuoteTestRecords()
	poll := fillQuotePoll{at: fillQuoteTestAt, complete: true, executions: 3}
	result := buildReportingCosts(records, "", "paper", 15, poll, true, fillQuoteTestAt)
	if err := rpc.ValidateReportingCostsResult(result); err != nil {
		t.Fatal(err)
	}
	if len(result.Fills) != 4 || result.Fills[0].ExecID != "e1" || result.Fills[3].ExecID != "e4" {
		t.Fatalf("fills = %+v, want ascending by execution time", result.Fills)
	}
	// Records arrive unsorted; the report orders them.
	option, stock, unknown, noMultiplier := result.Fills[0], result.Fills[1], result.Fills[2], result.Fills[3]
	if option.Status != rpc.CostFillMeasured || !closeTo(*option.HalfSpread, 10) || !closeTo(*option.HalfSpreadBase, 9) || option.Class != rpc.CostClassOption || option.Multiplier != 100 {
		t.Fatalf("option fill = %+v, want (1.30-1.25)*2*100 = 10 USD, 9 EUR", option)
	}
	if stock.Status != rpc.CostFillMeasured || !closeTo(*stock.HalfSpread, 10) || !closeTo(*stock.HalfSpreadBase, 9) || stock.Class != rpc.CostClassStock {
		t.Fatalf("stock fill = %+v, want (10.40-10.50)*-1*100 = 10 USD", stock)
	}
	if unknown.Status != rpc.CostFillUnknown || unknown.Reason != "timeout" || unknown.HalfSpread != nil {
		t.Fatalf("unknown fill = %+v", unknown)
	}
	if noMultiplier.Status != rpc.CostFillUnknown || noMultiplier.Reason != "multiplier_unknown" || noMultiplier.Quote.Status != rpc.FillQuoteQuoted {
		t.Fatalf("option without multiplier = %+v", noMultiplier)
	}
	if result.Coverage.Fills != 4 || result.Coverage.Quoted != 3 || result.Coverage.Unknown != 1 || *result.Coverage.Ratio != 0.75 {
		t.Fatalf("coverage = %+v", result.Coverage)
	}
	if result.BaseCurrency != "EUR" || result.Mode != "paper" {
		t.Fatalf("base %q mode %q", result.BaseCurrency, result.Mode)
	}
	if len(result.ByMonth) != 2 || result.ByMonth[0].Key != "2026-09" || result.ByMonth[0].Fills != 3 || result.ByMonth[0].Measured != 2 || !closeTo(*result.ByMonth[0].HalfSpreadBase, 18) || result.ByMonth[1].Key != "2026-10" || result.ByMonth[1].HalfSpreadBase != nil {
		t.Fatalf("by month = %+v", result.ByMonth)
	}
	if len(result.BySource) != 2 || result.BySource[0].Key != rpc.FillQuoteSourceBroker || result.BySource[0].Measured != 0 || result.BySource[1].Key != rpc.FillQuoteSourceCanary || !closeTo(*result.BySource[1].HalfSpreadBase, 18) {
		t.Fatalf("by source = %+v", result.BySource)
	}
	if len(result.ByClass) != 2 || result.ByClass[0].Key != rpc.CostClassOption || result.ByClass[0].Fills != 2 || !closeTo(*result.ByClass[0].HalfSpreadBase, 9) || result.ByClass[1].Key != rpc.CostClassStock {
		t.Fatalf("by class = %+v", result.ByClass)
	}
	if result.Observation.Visibility != rpc.ExecutionVisibilityUnproven || result.Observation.Action == "" || result.Observation.IntervalSeconds != 60 || !result.Observation.LastPollComplete {
		t.Fatalf("observation = %+v", result.Observation)
	}

	filtered := buildReportingCosts(records, "2026-10", "paper", 15, fillQuotePoll{otherClientsSeen: true}, true, fillQuoteTestAt)
	if err := rpc.ValidateReportingCostsResult(filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Fills) != 1 || filtered.Fills[0].ExecID != "e4" || filtered.Coverage.Fills != 1 || len(filtered.ByMonth) != 1 {
		t.Fatalf("month filter = %+v", filtered.Fills)
	}
	if filtered.Observation.Visibility != rpc.ExecutionVisibilityAllClients || filtered.Observation.Action != "" {
		t.Fatalf("observation after another client's fill = %+v", filtered.Observation)
	}

	empty := buildReportingCosts(nil, "", "paper", 15, fillQuotePoll{}, false, fillQuoteTestAt)
	if err := rpc.ValidateReportingCostsResult(empty); err != nil {
		t.Fatal(err)
	}
	if empty.Coverage.Ratio != nil || empty.Observation.Polling {
		t.Fatalf("empty report = %+v", empty)
	}
}

func TestFillQuoteStoredRecordsAreListedByAccountAndMode(t *testing.T) {
	t.Parallel()
	s, _ := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	for i, rec := range fillQuoteTestRecords() {
		account := "DU1234567"
		if i == 0 {
			account = "DU7654321"
		}
		if err := s.fillQuotes.append(account, rec); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.fillQuotes.list("DU1234567", "paper"); len(got) != 3 || got[0].ExecID != "e1" {
		t.Fatalf("list = %+v, want the account's three fills ascending", got)
	}
	if got := s.fillQuotes.list("DU1234567", "live"); len(got) != 0 {
		t.Fatalf("live list = %+v, want none", got)
	}
}

// Visibility is proven by the poll that first saw another client's execution
// and stays proven, with that poll's time, across later polls of the same
// scope; another account, mode or client id starts unproven again.
func TestExecutionVisibilityIsProvenOnceAndKeepsItsTime(t *testing.T) {
	t.Parallel()
	rt := &fillQuoteRuntime{records: map[string]fillQuoteStored{}, claimed: map[string]bool{}}
	scope := brokerStateScope{Account: "DU0000000", Mode: rpc.AccountModePaper}
	status := rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, ClientID: 15}
	first := time.Date(2026, 10, 18, 11, 35, 0, 0, time.UTC)

	unproven := executionVisibility(15, rt.pollFor(status))
	if unproven.Visibility != rpc.ExecutionVisibilityUnproven || unproven.ClientID != 15 || unproven.Setting != rpc.ExecutionVisibilitySetting || !unproven.ProvenAt.IsZero() {
		t.Fatalf("unproven verdict: %+v", unproven)
	}
	if !strings.Contains(unproven.Action, rpc.ExecutionVisibilitySetting) || !strings.Contains(unproven.Action, "(15)") {
		t.Fatalf("unproven action must name the setting and the client id: %q", unproven.Action)
	}

	rt.recordPoll(fillQuotePoll{scope: scope, clientID: 15, at: first, complete: true, otherClientsSeen: true, otherClientsSeenAt: first})
	rt.recordPoll(fillQuotePoll{scope: scope, clientID: 15, at: first.Add(time.Minute), complete: true})
	proven := executionVisibility(15, rt.pollFor(status))
	if proven.Visibility != rpc.ExecutionVisibilityAllClients || !proven.ProvenAt.Equal(first) || proven.Action != "" {
		t.Fatalf("proven verdict: %+v", proven)
	}

	other := status
	other.ClientID = 7
	if got := executionVisibility(7, rt.pollFor(other)); got.Visibility != rpc.ExecutionVisibilityUnproven || !got.ProvenAt.IsZero() {
		t.Fatalf("another client id must start unproven: %+v", got)
	}
	rt.recordPoll(fillQuotePoll{scope: brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}, clientID: 15, at: first.Add(2 * time.Minute), complete: true})
	if got := executionVisibility(15, rt.pollFor(status)); got.Visibility != rpc.ExecutionVisibilityUnproven {
		t.Fatalf("another account must not inherit the proof: %+v", got)
	}

	s := &Server{fillQuotes: rt}
	if s.executionVisibilityHealth(discover.Endpoint{}) == nil {
		t.Fatal("status row must be served once the store is bound")
	}
	if (&Server{}).executionVisibilityHealth(discover.Endpoint{}) != nil {
		t.Fatal("status row must be absent while the store is not bound")
	}
}
