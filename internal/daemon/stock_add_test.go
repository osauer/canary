package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func stockAddTestInput() risk.StockAddInput {
	pol := risk.DefaultRulebookPolicy()
	pol.SingleNameWatchPct, pol.SingleNameActPct = 80, 90
	pol.IlliquidWatchPct, pol.IlliquidActPct = 80, 90
	return risk.StockAddInput{Policy: &risk.StockAddPolicy{AdmissionContract: risk.StockAddAdmissionV1, MaxStockPctNLV: new(60.), MaxUnderlyingStockPctNLV: new(10.)}, Symbol: "SYNA", ConID: 101, Price: 100, FX: 1, FreeCash: 10000, OrderCapBase: 10000, Rulebook: pol, Rules: risk.RuleInputs{AsOf: time.Date(2026, 5, 28, 8, 45, 0, 0, time.UTC), BaseCurrency: "USD", Account: risk.SourceState{Healthy: true}, Positions: risk.SourceState{Healthy: true}, NLVBase: new(100000.), ExcessLiquidityBase: new(50000.), RiskCapital: &risk.RiskCapitalInput{EffectiveBase: new(100000.)}}}
}
func stockAddTestParams() rpc.AddParams {
	return rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "STK", Currency: "USD", Exchange: "SMART", ConID: 101}, LimitPrice: 100, Max: true}
}
func stockAddTestWhatIf(fee float64) rpc.OrderWhatIfResult {
	return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true, Margin: &rpc.OrderMarginImpact{Currency: "USD", InitialMarginBefore: new(50000.), InitialMarginAfter: new(50000.), MaintenanceMarginBefore: new(40000.), MaintenanceMarginAfter: new(40000.), EquityWithLoanBefore: new(90000.), EquityWithLoanAfter: new(90000.), CommissionCurrency: "USD", MaxCommission: new(fee)}}
}
func stockAddTestServer(t *testing.T) (*Server, *risk.StockAddInput) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	in := stockAddTestInput()
	s.rulebookPolicies = &rulebookPolicyManager{active: in.Rulebook, status: rpc.RulebookPolicyStatus{Status: "active"}}
	s.protectionPolicies = &protectionPolicyManager{active: defaultProtectionPolicy(), status: rpc.ProtectionPolicyStatus{Status: "active"}}
	s.stockAddEvidenceForTest = func(_ context.Context, p rpc.AddParams) (stockAddEvidence, error) {
		copy := in
		copy.Requested = p.Quantity
		copy.Price = p.LimitPrice
		return stockAddEvidence{input: copy, contract: p.Contract, review: rpc.AddReview{PolicyFingerprint: s.riskPolicies.active.FingerprintKey(), RulebookFingerprint: in.Rulebook.FingerprintKey(), CashPolicyFingerprint: fingerprintProtectionPolicy(s.protectionPolicies.active).Key, AsOf: s.orderNow()}}, nil
	}
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return stockAddTestWhatIf(5), nil
	}
	s.orderPreviewQuote = fixedPreviewQuote(99, 101)
	s.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, q int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{Before: in.CurrentQuantity, After: in.CurrentQuantity + float64(q), Effect: classifyPositionEffect(in.CurrentQuantity, in.CurrentQuantity+float64(q))}, nil
	}
	return s, &in
}

func TestStockAddPlanUsesExactFeesAndMintsNoToken(t *testing.T) {
	s, _ := stockAddTestServer(t)
	var quantities []int
	s.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		quantities = append(quantities, d.Quantity)
		return stockAddTestWhatIf(5), nil
	}
	got, err := s.planStockAdd(t.Context(), stockAddTestParams())
	if err != nil || got.Quantity != 99 || got.Cost != 9905 || got.Review == nil || fmt.Sprint(quantities) != "[100 99]" {
		t.Fatalf("%+v %v candidates %v", got, err, quantities)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "preview_token") {
		t.Fatal("planning minted execution authority")
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil || len(events) != 0 {
		t.Fatalf("planning reserved money or wrote an order: %v %v", events, err)
	}
}
func TestStockAddPlanHoldsUnknownAndChangingFees(t *testing.T) {
	for _, kind := range []string{"missing fee", "wrong currency", "rejected", "unstable", "evidence"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := stockAddTestServer(t)
			calls := 0
			s.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				calls++
				w := stockAddTestWhatIf(5)
				switch kind {
				case "missing fee":
					w.Margin.MaxCommission = nil
				case "wrong currency":
					w.Margin.CommissionCurrency = "EUR"
				case "rejected":
					w.Status = rpc.OrderWhatIfStatusRejected
				case "unstable":
					w.Margin.MaxCommission = new(float64(calls * 100))
				}
				return w, nil
			}
			if kind == "evidence" {
				s.stockAddEvidenceForTest = func(context.Context, rpc.AddParams) (stockAddEvidence, error) {
					return stockAddEvidence{}, fmt.Errorf("positions incomplete")
				}
			}
			got, err := s.planStockAdd(t.Context(), stockAddTestParams())
			if err != nil || got.Quantity != 0 || len(got.Blockers) == 0 || got.Review != nil || got.AsOf.IsZero() {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
}
func TestStockAddPreviewBindsReviewAndRefusesInventedPosition(t *testing.T) {
	s, in := stockAddTestServer(t)
	p := stockAddTestParams()
	p.Quantity, p.Max = 3, false
	raw, _ := json.Marshal(p)
	out, err := s.handleAddPreview(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	if out.Draft.Add == nil || out.Draft.Quantity != 3 || out.Draft.Add.Plan.Before != 0 || out.PreviewToken == "" {
		t.Fatalf("%+v", out)
	}
	payload, err := s.orderTokens.verify(out.PreviewToken)
	if err != nil || payload.Draft.Add == nil {
		t.Fatalf("unsigned Add evidence: %v", err)
	}
	in.CurrentQuantity = 1
	if _, err := s.validateStockAddDraft(t.Context(), payload.Draft, payload.WhatIf); err == nil {
		t.Fatal("accepted changed holdings")
	}
	for _, raw := range []string{`{"contract":{"symbol":"SYNA","currency":"USD"},"limit_price":100,"existing_position_size":0}`, `{"contract":{"symbol":"SYNA","currency":"USD"},"limit_price":100,"risk_budget":999999}`, `{} {}`} {
		if _, err := s.handleAddPlan(t.Context(), &rpc.Request{Params: json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted invented evidence %s", raw)
		}
	}
}
func TestStockAddAdmissionRecomputesAllAllowances(t *testing.T) {
	for _, kind := range []string{"cash", "stock allocation", "risk budget", "policy identity", "missing book"} {
		t.Run(kind, func(t *testing.T) {
			s, in := stockAddTestServer(t)
			p := stockAddTestParams()
			p.Quantity, p.Max = 3, false
			raw, _ := json.Marshal(p)
			out, err := s.handleAddPreview(t.Context(), &rpc.Request{Params: raw})
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "cash":
				in.FreeCash = 100
			case "stock allocation":
				in.StockValueBase = 60000
			case "risk budget":
				in.Rules.RiskCapital.EffectiveBase = new(100.)
			case "policy identity":
				out.Draft.Add.PolicyFingerprint = "previous"
			case "missing book":
				in.Rules.Positions.Healthy = false
			}
			if _, err := s.validateStockAddDraft(t.Context(), out.Draft, out.WhatIf); err == nil {
				t.Fatal("changed evidence was admitted")
			}
		})
	}
}
func TestStockAddPendingBuysConsumeRiskAndAllocations(t *testing.T) {
	in := stockAddTestInput()
	c := stockAddTestParams().Contract
	scope := brokerStateScope{Account: "DU0000000", Mode: "paper"}
	order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: scope.Account, Status: "Submitted", Action: "BUY", SecType: "STK", Symbol: c.Symbol, ConID: c.ConID, Currency: "USD", OrderType: "LMT", TotalQuantity: 30, Filled: 10, Remaining: 20, LimitPrice: 100}
	if err := stockAddPending(&in, []ibkrlib.OrderLifecycleEvent{order}, map[string]cashSweepLedgerRow{"USD": {ExchangeRate: 1}}, c, scope); err != nil {
		t.Fatal(err)
	}
	if in.CurrentQuantity != 0 || in.StockValueBase != 2000 || in.UnderlyingStockBase != 2000 || len(in.Rules.Names) != 1 || in.Rules.Names[0].StockQuantity != 20 {
		t.Fatalf("pending order treated as filled or absent: %+v", in)
	}
	if got := risk.SizeStockAdd(in); got.MaxQuantity != 80 {
		t.Fatalf("pending order created room: %+v", got)
	}
	for _, kind := range []string{"sell", "option", "unknown account", "unknown FX", "market", "identity"} {
		t.Run(kind, func(t *testing.T) {
			in := stockAddTestInput()
			bad := order
			ledger := map[string]cashSweepLedgerRow{"USD": {ExchangeRate: 1}}
			switch kind {
			case "sell":
				bad.Action = "SELL"
			case "option":
				bad.SecType = "OPT"
			case "unknown account":
				bad.Account = ""
			case "unknown FX":
				ledger = nil
			case "market":
				bad.OrderType = "MKT"
			case "identity":
				bad.ConID++
			}
			if stockAddPending(&in, []ibkrlib.OrderLifecycleEvent{bad}, ledger, c, scope) == nil {
				t.Fatal("unbounded pending activity accepted")
			}
		})
	}
}
func TestStockAddPolicyOptInCannotBeBypassedByGenericPreview(t *testing.T) {
	s, _ := stockAddTestServer(t)
	draft := rpc.OrderDraft{Contract: stockAddTestParams().Contract, Action: "BUY"}
	position := rpc.OrderPositionImpact{After: 1}
	if s.stockAddRequired(draft, position) {
		t.Fatal("legacy policy changed on installation")
	}
	s.riskPolicies.active.PositionAdd = &risk.StockAddPolicy{MaxStockPctNLV: new(60.), MaxUnderlyingStockPctNLV: new(10.)}
	if s.stockAddRequired(draft, position) {
		t.Fatal("allocation values silently activated trading admission")
	}
	s.riskPolicies.active.PositionAdd.AdmissionContract = risk.StockAddAdmissionV1
	if !s.stockAddRequired(draft, position) {
		t.Fatal("ordinary stock order bypasses active Add policy")
	}
	draft.Action = "SELL"
	if s.stockAddRequired(draft, rpc.OrderPositionImpact{Before: 2, After: 1}) {
		t.Fatal("Add trapped a reduction")
	}
	draft.Add = &rpc.AddReview{}
	s.riskPolicies.active.PositionAdd = nil
	if !s.stockAddRequired(draft, position) {
		t.Fatal("removing policy bypasses an existing Add token")
	}
}

func TestStockAddStopsCannotUsePendingBuysAsCover(t *testing.T) {
	in := stockAddTestInput()
	c := stockAddTestParams().Contract
	scope := brokerStateScope{Account: "DU0000000", Mode: "paper"}
	stop := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: scope.Account, Status: "Submitted", Action: "SELL", SecType: "STK", Symbol: c.Symbol, ConID: c.ConID, Currency: "USD", OrderType: "STP", TotalQuantity: 7, Remaining: 7}
	buy := stop
	buy.Action = "BUY"
	buy.OrderType = "LMT"
	buy.LimitPrice = 100
	ledger := map[string]cashSweepLedgerRow{"USD": {ExchangeRate: 1}}
	if stockAddPending(&in, []ibkrlib.OrderLifecycleEvent{buy, stop}, ledger, c, scope) == nil {
		t.Fatal("unfilled purchase was used as stop cover")
	}
	in = stockAddTestInput()
	in.Rules.Names = []risk.NameInput{{Symbol: c.Symbol, StockConID: c.ConID, HasStockLeg: true, StockQuantity: 10}}
	if err := stockAddPending(&in, []ibkrlib.OrderLifecycleEvent{stop}, ledger, c, scope); err != nil {
		t.Fatal(err)
	}
	if stockAddPending(&in, []ibkrlib.OrderLifecycleEvent{stop, stop}, ledger, c, scope) == nil {
		t.Fatal("two oversized stops both counted the same shares")
	}
}
func TestStockAddMissingPolicyHoldsBeforeAnyBrokerRead(t *testing.T) {
	s := newTestServer(t)
	p := stockAddTestParams()
	raw, _ := json.Marshal(p)
	result, err := s.handleAddPlan(t.Context(), &rpc.Request{Params: raw})
	if err != nil || result.Quantity != 0 || len(result.Blockers) != 1 || !strings.Contains(result.Blockers[0].Message, "unapproved") {
		t.Fatalf("%+v %v", result, err)
	}
}
