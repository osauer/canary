package daemon

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// One rule decides whether an order can carry IBKR's Adaptive algorithm: a
// stock or ETF routed SMART, the broker layer's shape (an empty exchange
// defaults to SMART). Every other contract goes as a plain limit at the same
// reviewed price.
func TestAdaptiveSupportedTable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract rpc.ContractParams
		want     bool
	}{
		{"stock on SMART", rpc.ContractParams{SecType: "STK", Exchange: "SMART", Currency: "USD"}, true},
		{"ETF on SMART", rpc.ContractParams{SecType: "ETF", Exchange: "SMART", Currency: "USD"}, true},
		{"euro stock on SMART", rpc.ContractParams{SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR"}, true},
		{"stock with no exchange", rpc.ContractParams{SecType: "STK", Currency: "USD"}, true},
		{"option on SMART", rpc.ContractParams{SecType: "OPT", Exchange: "SMART", Currency: "USD"}, false},
		{"stock on IBIS", rpc.ContractParams{SecType: "STK", Exchange: "IBIS", PrimaryExch: "IBIS", Currency: "EUR"}, false},
		{"combination", rpc.ContractParams{SecType: "BAG", Exchange: "SMART", Currency: "USD"}, false},
		{"future", rpc.ContractParams{SecType: "FUT", Exchange: "CME", Currency: "USD"}, false},
	} {
		if got := adaptiveSupported(tc.contract); got != tc.want {
			t.Errorf("%s: adaptiveSupported = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// brokerLayerWhatIf stands in for the broker's WhatIf with the broker
// layer's own order check, which a plain fake skips: the call plan's Adaptive
// option preview passed every unit test and failed against the daemon.
func brokerLayerWhatIf(t *testing.T, accepted rpc.OrderWhatIfResult) func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
	t.Helper()
	return func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		multiplier := ""
		if d.Contract.Multiplier > 0 {
			multiplier = strconv.Itoa(d.Contract.Multiplier)
		}
		order := &ibkrlib.IBKROrder{
			ConID: d.Contract.ConID, Symbol: d.Contract.Symbol, SecType: d.Contract.SecType, Exchange: d.Contract.Exchange, Currency: d.Contract.Currency,
			Expiry: d.Contract.Expiry, Strike: d.Contract.Strike, Right: d.Contract.Right, Multiplier: multiplier, TradingClass: d.Contract.TradingClass,
			Action: d.Action, TotalQty: d.Quantity, OrderType: d.OrderType, LmtPrice: d.LimitPrice, LmtPriceSet: true, TIF: d.TIF,
			OutsideRth: d.OutsideRTH, AdaptivePriority: d.AdaptivePriority,
		}
		if err := ibkrlib.ValidateOrder(order); err != nil {
			return rpc.OrderWhatIfResult{}, err
		}
		return accepted, nil
	}
}

func adaptiveOptionPreviewServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newReducePreviewTestServer(t)
	srv.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, qty int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{Before: 0, After: float64(qty), Effect: rpc.OrderPositionEffectOpen}, nil
	}
	srv.orderPreviewWhatIf = brokerLayerWhatIf(t, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{MaxCommission: new(1.0), CommissionCurrency: "USD"}})
	return srv
}

func adaptiveTestCall() rpc.ContractParams {
	return rpc.ContractParams{ConID: 7301, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", Right: "C", Multiplier: 100, Strike: 30, Expiry: "20261218", TradingClass: "SYNA"}
}

// The call plan sizes an option through the real preview, which carries no
// Adaptive: the broker layer sends Adaptive only on a stock or ETF.
func TestAddCallPlanSizesAnOptionThroughTheRealPreview(t *testing.T) {
	srv := adaptiveOptionPreviewServer(t)
	p := rpc.AddCallPlanParams{Contract: adaptiveTestCall(), LimitPrice: 2.5, Budget: 1000}
	plan, err := planCallQuantity(t.Context(), p, 0, srv.previewOrder)
	if err != nil {
		t.Fatalf("call plan: %v", err)
	}
	if plan.Quantity != 3 || plan.Contract.ConID != 7301 {
		t.Fatalf("plan = %+v, want 3 contracts of 7301", plan)
	}
}

// Adaptive asked for on an option is the caller's mistake, refused before the
// broker in words the owner can act on, never an internal error.
func TestPreviewRefusesAdaptiveOnAnOptionAsABadRequest(t *testing.T) {
	srv := adaptiveOptionPreviewServer(t)
	limit := 2.5
	_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: rpc.OrderActionBuy, Contract: adaptiveTestCall(), Quantity: 1, OrderType: rpc.OrderTypeLMT, LimitPrice: &limit, TIF: rpc.OrderTIFDay, Strategy: "explicit-limit", AdaptivePriority: "Normal"})
	if code, msg := classifyError(err); code != rpc.CodeBadRequest || !strings.Contains(msg, adaptiveUnsupportedMessage) {
		t.Fatalf("err = %v, want a bad request saying %q", err, adaptiveUnsupportedMessage)
	}
	// Without Adaptive the same option previews as a plain limit.
	res, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: rpc.OrderActionBuy, Contract: adaptiveTestCall(), Quantity: 1, OrderType: rpc.OrderTypeLMT, LimitPrice: &limit, TIF: rpc.OrderTIFDay, Strategy: "explicit-limit"})
	if err != nil || res.Draft.AdaptivePriority != "" {
		t.Fatalf("plain option preview = %+v, %v", res, err)
	}
}

// Add sizing on a listing's own exchange with Adaptive asked for is held with
// the sentence naming the route, before any broker check (add preview turns a
// held plan into a bad request); without Adaptive the same stock sizes as a
// plain limit.
func TestStockAddPlanRefusesAdaptiveOffSMART(t *testing.T) {
	s, _ := stockAddTestServer(t)
	s.orderPreviewWhatIf = brokerLayerWhatIf(t, stockAddTestWhatIf(5))
	p := stockAddTestParams()
	p.Contract.Exchange, p.Contract.PrimaryExch = "IBIS", "IBIS"
	p.AdaptivePriority = "Normal"
	got, err := s.planStockAdd(t.Context(), p)
	if err != nil || got.Quantity != 0 || len(got.Blockers) != 1 || got.Blockers[0].Code != "add_adaptive_unsupported" ||
		!strings.Contains(got.Blockers[0].Message, "only on SMART") || !strings.Contains(got.Blockers[0].Message, "IBIS") {
		t.Fatalf("plan blockers = %+v, %v, want held with the SMART sentence naming IBIS", got.Blockers, err)
	}
	p.AdaptivePriority = ""
	if got, err := s.planStockAdd(t.Context(), p); err != nil || got.Quantity <= 0 {
		t.Fatalf("plain limit plan = %+v, %v", got, err)
	}
}
