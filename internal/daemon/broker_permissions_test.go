package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

var testMESContract = rpc.ContractParams{ConID: 990001, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218", LocalSymbol: "MESZ6", Multiplier: 5}

func brokerPermissionRig(t *testing.T, answer func() rpc.OrderWhatIfResult) (*Server, *int, *time.Time) {
	t.Helper()
	s := newTestServer(t)
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	mark := 6800.0
	s.reduceHedgeFutureForTest = func(context.Context) (rpc.ContractParams, *rpc.Quote, error) {
		return testMESContract, &rpc.Quote{Mark: &mark}, nil
	}
	calls := 0
	s.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		calls++
		if d.Action != rpc.OrderActionBuy || d.Quantity != 1 || d.OrderType != rpc.OrderTypeLMT || d.LimitPrice != 3400 || d.Contract.ConID != testMESContract.ConID {
			t.Errorf("check draft = %+v, want a one-contract buy limit at half the price", d)
		}
		return answer(), nil
	}
	return s, &calls, &now
}

// The owner's decision of 2026-10-29 07:14 CET: Canary learns the futures
// permission from a daily WhatIf, refuses an opening futures order the
// broker showed not permitted, and says so in the trading status.
func TestFuturesPermissionCheckShowsNotEnabledAndRefusesTheHedge(t *testing.T) {
	refusal := "Order rejected - reason: YOUR ACCOUNT HAS NO TRADING PERMISSIONS FOR THIS INSTRUMENT"
	s, calls, now := brokerPermissionRig(t, func() rpc.OrderWhatIfResult {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: refusal}
	})
	if st := s.handleTradingStatus().BrokerPermissions; len(st) != 1 || st[0].Class != rpc.BrokerPermissionFutures || st[0].State != rpc.BrokerPermissionUnknown {
		t.Fatalf("before any check = %+v, want futures not yet known", st)
	}
	if s.brokerPermissionBlocker(testMESContract) != nil {
		t.Fatal("an unknown state refused the preview that would learn it")
	}
	if !s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) || *calls != 1 {
		t.Fatalf("first check ran %d WhatIfs", *calls)
	}
	st := s.handleTradingStatus().BrokerPermissions[0]
	if st.State != rpc.BrokerPermissionNotEnabled || st.Source != brokerPermissionSourceCheck || !strings.Contains(st.Detail, "NO TRADING PERMISSIONS") || !strings.Contains(st.Action, "Trading Permissions") {
		t.Fatalf("after the check = %+v", st)
	}
	b := s.brokerPermissionBlocker(testMESContract)
	if b == nil || b.Code != rpc.BrokerPermissionNotEnabledCode || !strings.Contains(b.Message, "Futures trading is not enabled at IBKR") {
		t.Fatalf("blocker = %+v", b)
	}
	if got := s.indexHedgeCapRefusal(rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: testMESContract, Quantity: 1}, 0); len(got) != 1 || got[0].Code != rpc.BrokerPermissionNotEnabledCode {
		t.Fatalf("hedge sale refusal = %+v", got)
	}
	if s.brokerPermissionBlocker(rpc.ContractParams{Symbol: "SPY", SecType: "STK"}) != nil {
		t.Fatal("a futures refusal blocked stocks")
	}

	// Daily: not again within the day, again after it.
	*now = now.Add(23 * time.Hour)
	if s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) || *calls != 1 {
		t.Fatalf("checked again within the day: %d", *calls)
	}
	*now = now.Add(time.Hour)
	if !s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) || *calls != 2 {
		t.Fatalf("no check after a day: %d", *calls)
	}
}

// An accepted check shows futures enabled; a later answer that says nothing
// about permission keeps it; the trader's own previews show other classes.
func TestBrokerPermissionEvidenceKeepsWhatItKnows(t *testing.T) {
	answer := rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}
	s, _, now := brokerPermissionRig(t, func() rpc.OrderWhatIfResult { return answer })
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})
	if st := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures); st.State != rpc.BrokerPermissionEnabled {
		t.Fatalf("accepted check = %+v", st)
	}
	answer = rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "Order price is outside the allowed range"}
	*now = now.Add(25 * time.Hour)
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})
	if st := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures); st.State != rpc.BrokerPermissionEnabled {
		t.Fatalf("a price refusal overwrote enabled: %+v", st)
	}

	opt := rpc.OrderDraft{Contract: rpc.ContractParams{Symbol: "SPY", SecType: "OPT"}}
	s.noteBrokerPermissionPreview(t.Context(), opt, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "insufficient margin"})
	if v := s.handleTradingStatus().BrokerPermissions; len(v) != 1 {
		t.Fatalf("a margin refusal of a preview showed options: %+v", v)
	}
	s.noteBrokerPermissionPreview(t.Context(), opt, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted})
	v := s.handleTradingStatus().BrokerPermissions
	if len(v) != 2 || v[1].Class != rpc.BrokerPermissionOptions || v[1].State != rpc.BrokerPermissionEnabled || v[1].Source != brokerPermissionSourcePreview {
		t.Fatalf("options after an accepted preview = %+v", v)
	}
	if brokerPermissionRefusal("No market data permissions for CME") {
		t.Fatal("a market-data permission counted as a trading permission")
	}
}

// The states live in daemon.db: a restart keeps them, and checks futures
// again once.
func TestBrokerPermissionsSurviveARestart(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	s, calls, _ := brokerPermissionRig(t, func() rpc.OrderWhatIfResult {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "not permitted to trade futures"}
	})
	if err := s.brokerPermissions.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})

	restarted, calls2, _ := brokerPermissionRig(t, func() rpc.OrderWhatIfResult {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted}
	})
	if err := restarted.brokerPermissions.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if st := restarted.brokerPermissions.state(restarted.currentBrokerStateScope(), rpc.BrokerPermissionFutures); st.State != rpc.BrokerPermissionNotEnabled {
		t.Fatalf("after restart = %+v", st)
	}
	if !restarted.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) || *calls != 1 || *calls2 != 1 {
		t.Fatalf("a restart did not check once: %d %d", *calls, *calls2)
	}
	if st := restarted.brokerPermissions.state(restarted.currentBrokerStateScope(), rpc.BrokerPermissionFutures); st.State != rpc.BrokerPermissionEnabled {
		t.Fatalf("a granted permission was not learned at restart: %+v", st)
	}
}
