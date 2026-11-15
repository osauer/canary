package daemon

import (
	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
	"path/filepath"
	"testing"
)

func TestReportingCostsRequiresConfirmedAccount(t *testing.T) {
	recs := fillQuoteTestRecords()[1:3]
	recs[0].Mode, recs[1].Mode = "disabled", "disabled"
	rt := &fillQuoteRuntime{records: map[string]fillQuoteStored{
		"one": {Account: "SYNTHETIC_ACCOUNT_A", Record: recs[0]},
		"two": {Account: "SYNTHETIC_ACCOUNT_B", Record: recs[1]},
	}}
	srv := &Server{fillQuotes: rt}
	res, err := srv.handleReportingCosts(t.Context(), &rpc.Request{Params: []byte(`{}`)})
	if err == nil || res != nil {
		t.Fatalf("unknown account must refuse report, got %+v, %v", res, err)
	}
}

func TestCostTotalsKeepHistoricalCurrenciesSeparate(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		records := fillQuoteTestRecords()[1:3]
		records[0].BaseCurrency, records[0].FXRate = "EUR", new(0.9)
		records[1].BaseCurrency, records[1].FXRate = "USD", new(1.0)
		if reverse {
			records[0], records[1] = records[1], records[0]
		}
		got := buildReportingCosts(records, "", "paper", 15, fillQuotePoll{}, false, fillQuoteTestAt)
		if err := rpc.ValidateReportingCostsResult(got); err != nil {
			t.Fatal(err)
		}
		totals := got.ByMonth[0]
		if got.BaseCurrency != "" || totals.HalfSpreadBase != nil || len(totals.ByCurrency) != 2 || totals.ByCurrency[0].Currency != "EUR" || !closeTo(totals.ByCurrency[0].HalfSpreadBase, 9) || totals.ByCurrency[1].Currency != "USD" || !closeTo(totals.ByCurrency[1].HalfSpreadBase, 10) {
			t.Fatalf("mixed units: %+v", got)
		}
	}
}
func TestCostMeasurementRequiresExplicitLiveFeed(t *testing.T) {
	for _, feed := range []string{rpc.MarketDataLive, rpc.MarketDataDelayed, rpc.MarketDataFrozen, rpc.MarketDataDelayedFrozen, "", "unexpected"} {
		rec := fillQuoteTestRecords()[1]
		rec.Quote.DataType = feed
		got := costFillFromRecord(rec)
		if feed == rpc.MarketDataLive {
			if got.Status != rpc.CostFillMeasured {
				t.Fatal(got)
			}
		} else if got.Status != rpc.CostFillUnknown || got.HalfSpread != nil || got.HalfSpreadBase != nil || got.Reason == "" {
			t.Fatalf("%s: %+v", feed, got)
		}
	}
	rec := fillQuoteTestRecords()[1]
	rec.BaseCurrency = ""
	got := costFillFromRecord(rec)
	if got.HalfSpreadBase != nil || got.FXRate != nil {
		t.Fatal("currency-free conversion", got)
	}
}

func TestCostsSurviveTradingAuthorityChangesAndReload(t *testing.T) {
	prior, core := newFillQuoteTestServer(t, filepath.Join(t.TempDir(), "daemon.db"))
	for i, mode := range []string{"live", "disabled", ""} {
		rec := fillQuoteTestRecords()[i]
		rec.Mode = mode
		if err := prior.fillQuotes.append("DU1234567", rec); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := bindFillQuoteRuntime(t.Context(), core)
	if err != nil {
		t.Fatal(err)
	}
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModeDisabled})
	s.fillQuotes = rt
	s.cfg.Gateway.Account = ""
	s.endpoint.Account = ""
	s.gatewayAccountForTrading = func() string { return "DU1234567" }
	for _, mode := range []string{config.TradingModeDisabled, config.TradingModePaper} {
		s.cfg.Trading.Mode = mode
		got, err := s.handleReportingCosts(t.Context(), &rpc.Request{Params: []byte(`{}`)})
		if err != nil || len(got.Fills) != 3 || got.Mode != "paper" {
			t.Fatalf("mode=%s result=%+v err=%v", mode, got, err)
		}
	}
	if len(rt.list("DU1234567", "live")) != 0 || len(rt.list("DU7654321", "paper")) != 0 {
		t.Fatal("crossed account or mode")
	}
	captured := fillQuoteFillFromExecution(ibkrlib.OrderLifecycleEvent{ExecID: "synthetic", Account: "DU1234567"}, rpc.TradingStatus{Mode: "disabled", Account: "DU1234567"}, fillQuoteTestAt)
	if captured.record.Mode != "paper" {
		t.Fatalf("captured mode=%s", captured.record.Mode)
	}
	s.gatewayAccountForTrading = func() string { return "All" }
	if _, err := s.handleReportingCosts(t.Context(), &rpc.Request{Params: []byte(`{}`)}); err == nil {
		t.Fatal("accepted aggregate account")
	}
}

func TestCostsPollEvidenceCannotCrossAccountOrClient(t *testing.T) {
	rt := &fillQuoteRuntime{}
	a := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	b := brokerStateScope{Account: "DU7654321", Mode: "paper"}
	rt.recordPoll(fillQuotePoll{scope: a, clientID: 15, otherClientsSeen: true})
	rt.recordPoll(fillQuotePoll{scope: a, clientID: 15, complete: true})
	if !rt.pollStatus().otherClientsSeen {
		t.Fatal("forgot same-account evidence")
	}
	rt.recordPoll(fillQuotePoll{scope: b, clientID: 15, complete: true})
	if rt.pollStatus().otherClientsSeen {
		t.Fatal("inherited another account's visibility")
	}
	rt.recordPoll(fillQuotePoll{scope: b, clientID: 15, otherClientsSeen: true})
	rt.recordPoll(fillQuotePoll{scope: b, clientID: 16, complete: true})
	if rt.pollStatus().otherClientsSeen {
		t.Fatal("inherited another API client's visibility")
	}
}
