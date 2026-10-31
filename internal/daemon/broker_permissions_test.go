package daemon

import (
	"context"
	"errors"
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
	// Flat in the check's contract, so its buy opens.
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
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
	s.noteBrokerPermissionPreview(t.Context(), s.currentBrokerStateScope(), opt, rpc.OrderPositionEffectOpen, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "insufficient margin"}, s.nowUTC())
	if v := s.handleTradingStatus().BrokerPermissions; len(v) != 1 {
		t.Fatalf("a margin refusal of a preview showed options: %+v", v)
	}
	s.noteBrokerPermissionPreview(t.Context(), s.currentBrokerStateScope(), opt, rpc.OrderPositionEffectOpen, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted}, s.nowUTC())
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

// A check that gets no broker answer used to return without a word: no log
// line, no detail, futures read "not checked yet" for good (live install
// 2026-10-29 07:32 CET, two checks, nothing shown). It now says why in the
// status and is retried at the next pass.
func TestFuturesCheckWithoutAnAnswerSaysWhy(t *testing.T) {
	answer := rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable, Message: "Broker WhatIf did not return an accepted preview."}
	s, calls, now := brokerPermissionRig(t, func() rpc.OrderWhatIfResult { return answer })
	if s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) {
		t.Fatal("a check without an answer counted as done")
	}
	st := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures)
	if st.State != rpc.BrokerPermissionUnknown || !strings.Contains(st.Detail, "did not return an accepted preview") {
		t.Fatalf("after an unanswered check = %+v, want the reason", st)
	}
	if s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}); *calls != 1 {
		t.Fatalf("an unanswered check was retried at once: %d", *calls)
	}
	*now = now.Add(brokerPermissionRetryAfter)
	if s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}); *calls != 2 {
		t.Fatalf("an unanswered check was not retried after the wait: %d", *calls)
	}
	*now = now.Add(brokerPermissionRetryAfter)

	s.reduceHedgeFutureForTest = func(context.Context) (rpc.ContractParams, *rpc.Quote, error) {
		return rpc.ContractParams{}, nil, errors.New("no gateway")
	}
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})
	if st := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures); !strings.Contains(st.Detail, "no gateway") {
		t.Fatalf("a failed contract read left no reason: %+v", st)
	}
}

// A stored futures denial lifts when the daily check is refused for a reason
// that names no permission: the broker weighed the order, so the denial is no
// longer confirmed, and the hedge's own preview may ask again. Kept, it
// refused every hedge before the broker, and a check the broker keeps
// refusing on price could never lift it (review of 2026-10-29).
func TestFuturesDenialLiftsWhenTheCheckIsRefusedForAnotherReason(t *testing.T) {
	answer := rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "YOUR ACCOUNT HAS NO TRADING PERMISSIONS FOR THIS INSTRUMENT"}
	s, _, now := brokerPermissionRig(t, func() rpc.OrderWhatIfResult { return answer })
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})
	if s.brokerPermissionBlocker(testMESContract) == nil {
		t.Fatal("the permission refusal was not learned")
	}

	// The broker's answer to the check holds no news; the denial stays.
	answer = rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable, Message: "no answer in 10s"}
	*now = now.Add(25 * time.Hour)
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})
	if s.brokerPermissionBlocker(testMESContract) == nil {
		t.Fatal("a check with no answer lifted the denial")
	}

	// The owner enables futures; the check is now refused on its price.
	answer = rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "Order price is outside the allowed range"}
	*now = now.Add(brokerPermissionRetryAfter)
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true})
	st := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures)
	if st.State != rpc.BrokerPermissionUnknown || !strings.Contains(st.Detail, "no longer confirmed") || !strings.Contains(st.Detail, "outside the allowed range") {
		t.Fatalf("after a price refusal = %+v, want the denial lifted with the reason", st)
	}
	if b := s.brokerPermissionBlocker(testMESContract); b != nil {
		t.Fatalf("the hedge is still refused before the broker: %+v", b)
	}
	if got := s.indexHedgeCapRefusal(rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: testMESContract, Quantity: 1}, 0); len(got) > 0 && got[0].Code == rpc.BrokerPermissionNotEnabledCode {
		t.Fatalf("hedge sale refusal = %+v", got)
	}
}

// An accepted reduce or close shows nothing about the class: IBKR lets an
// account close what it may no longer open.
func TestBrokerPermissionAcceptedCloseShowsNothing(t *testing.T) {
	s := newTestServer(t)
	scope := s.currentBrokerStateScope()
	opt := rpc.OrderDraft{Contract: rpc.ContractParams{Symbol: "SPY", SecType: "OPT"}}
	s.noteBrokerPermissionPreview(t.Context(), scope, opt, rpc.OrderPositionEffectOpen, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "No trading permissions"}, s.nowUTC())
	for _, effect := range []string{rpc.OrderPositionEffectReduce, rpc.OrderPositionEffectClose} {
		s.noteBrokerPermissionPreview(t.Context(), scope, opt, effect, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted}, s.nowUTC())
		if st := s.brokerPermissions.state(scope, rpc.BrokerPermissionOptions); st.State != rpc.BrokerPermissionNotEnabled {
			t.Fatalf("an accepted %s showed options %s", effect, st.State)
		}
	}
	fresh := newTestServer(t)
	fresh.noteBrokerPermissionPreview(t.Context(), scope, opt, rpc.OrderPositionEffectClose, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted}, fresh.nowUTC())
	if v := fresh.handleTradingStatus().BrokerPermissions; len(v) != 1 {
		t.Fatalf("an accepted close showed a class: %+v", v)
	}
}

// With a short Micro S&P held, the daily check's one-contract buy closes or
// reduces it, and IBKR accepts a close the account may no longer open, so
// its acceptance showed futures enabled whatever the permission. The check
// then sells one contract far above the market: adding to the short opens,
// so the answer still shows the permission.
func TestFuturesCheckWithAShortHeldPreviewsAnOpeningSale(t *testing.T) {
	s, _, _ := brokerPermissionRig(t, func() rpc.OrderWhatIfResult {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}
	})
	s.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		after := -2.0 + float64(qty)
		if action == rpc.OrderActionSell {
			after = -2.0 - float64(qty)
		}
		return rpc.OrderPositionImpact{Before: -2, After: after, Effect: classifyPositionEffect(-2, after)}, nil
	}
	var sent []rpc.OrderDraft
	s.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		sent = append(sent, d)
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	if !s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) {
		t.Fatal("the check did not run")
	}
	if len(sent) != 1 || sent[0].Action != rpc.OrderActionSell || sent[0].Quantity != 1 || sent[0].LimitPrice != 13600 {
		t.Fatalf("check drafts = %+v, want one sale of one contract at twice the price", sent)
	}
}

// A check whose holding cannot be read does not guess an opening order: it
// says why and retries.
func TestFuturesCheckWithoutTheHoldingSaysWhy(t *testing.T) {
	s, calls, _ := brokerPermissionRig(t, func() rpc.OrderWhatIfResult {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}
	})
	s.orderPreviewPositionImpact = func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{}, errors.New("portfolio stream not current")
	}
	if s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{CanPreview: true}) || *calls != 0 {
		t.Fatalf("a check without the holding ran %d WhatIfs", *calls)
	}
	if st := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures); st.State != rpc.BrokerPermissionUnknown || !strings.Contains(st.Detail, "portfolio stream not current") {
		t.Fatalf("after the check = %+v, want unknown with the reason", st)
	}
}
