package daemon

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func sweepFeePreview(free, principal, fee float64) (rpc.TradeProposal, *rpc.OrderPreviewResult) {
	prop := rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCashSweep, Action: rpc.OrderActionBuy, PositionEffect: rpc.OrderPositionEffectOpen,
		SecType: "BILL", Quantity: 1, MaxQuantity: 1, Contract: rpc.ContractParams{ConID: 7101, SecType: "BILL", Currency: "USD"},
		CashSweep: &rpc.TradeProposalCashSweep{Side: rpc.CashSweepSideInvest, Currency: "USD", Instrument: cashSweepInstrumentUSTBill,
			QuantityUnit: rpc.BondQuantityUnitFace1000, Free: free, ExchangeRate: 1, MaxOrderNotionalBase: principal,
			Bill: &rpc.TradeProposalCashSweepBill{ConID: 7101, Instrument: cashSweepInstrumentUSTBill}}}
	preview := &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Contract: prop.Contract, Action: rpc.OrderActionBuy, Quantity: 1,
		OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Source: proposalOrderSource, LimitPrice: principal / 10, Bond: cashSweepOrderTerms(prop)},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectOpen}, BaseCurrency: "USD", NotionalCurrency: "USD", Notional: principal, NotionalBase: principal,
		WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true,
			Margin: &rpc.OrderMarginImpact{Currency: "USD", InitialMarginBefore: new(0.0), InitialMarginAfter: new(principal),
				CommissionCurrency: "USD", Commission: new(fee), MinCommission: new(fee), MaxCommission: new(fee)}}}
	return prop, preview
}

func TestCashSweepFeesRefuseThroughProposalPreview(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		change     func(*rpc.OrderWhatIfResult)
	}{
		{"unknown upper", "cash_sweep_fees_unknown", func(w *rpc.OrderWhatIfResult) { w.Margin.MaxCommission = nil }},
		{"fee would consume reserve", "cash_sweep_cost_above_free_cash", func(w *rpc.OrderWhatIfResult) { w.Margin.MaxCommission = new(1000.0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC))
			original := rig.srv.orderPreviewWhatIf
			rig.srv.orderPreviewWhatIf = func(ctx context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				w, err := original(ctx, d)
				tc.change(&w)
				return w, err
			}
			out := rig.preview(t)
			if out.Accepted || out.SubmitEligible || !hasTradingBlocker(out.Blockers, tc.code) {
				t.Fatalf("proposal preview admitted unsafe fees: %+v", out)
			}
		})
	}
}

func TestCashSweepFeesConsumeFreeCashButNotPrincipalNotionalCap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		free    float64
		blocked bool
	}{
		{"fee cannot consume protected cash", 1000, true},
		{"fee fits exactly", 1010, false},
		{"fee exceeds cash by one cent", 1009.99, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prop, preview := sweepFeePreview(tc.free, 1000, 10)
			got := proposalPreviewSafetyBlockers(prop, preview)
			if blocked := hasTradingBlocker(got, "cash_sweep_cost_above_free_cash"); blocked != tc.blocked {
				t.Fatalf("all-in cash guard = %v, want blocked %v", got, tc.blocked)
			}
			if hasTradingBlocker(got, "cash_sweep_above_max_order_notional") {
				t.Fatalf("commission changed the principal-only cap: %v", got)
			}
			if !tc.blocked && len(got) != 0 {
				t.Fatalf("bounded all-in buy refused: %v", got)
			}
		})
	}
}

// A sweep proposal planned to open a long bill cannot gain permission by
// unexpectedly covering a short. The generic close/reduce branch must never
// bypass the typed buy's cash/fee envelope or accept that changed effect.
func TestCashSweepCoverBuyCannotBypassItsPlannedCashEffect(t *testing.T) {
	for _, effect := range []string{rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce} {
		for _, fees := range []string{"missing", "above free cash", "bounded"} {
			t.Run(effect+"/"+fees, func(t *testing.T) {
				rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC))
				before, after := -55.0, 0.0
				if effect == rpc.OrderPositionEffectReduce {
					before, after = -110, -55
				}
				rig.srv.orderPreviewPositionImpact = fixedPreviewPosition(before, after, effect)
				original := rig.srv.orderPreviewWhatIf
				rig.srv.orderPreviewWhatIf = func(ctx context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
					w, err := original(ctx, d)
					switch fees {
					case "missing":
						w.Margin.MaxCommission = nil
					case "above free cash":
						w.Margin.MaxCommission = new(1000.0)
					}
					return w, err
				}
				out := rig.preview(t)
				if out.Accepted || out.SubmitEligible || !hasTradingBlocker(out.Blockers, "preview_effect_not_close_reduce") {
					t.Fatalf("covering short bypassed the planned sweep-buy effect: accepted=%v eligible=%v blockers=%v", out.Accepted, out.SubmitEligible, out.Blockers)
				}
			})
		}
	}
}

func TestCashSweepFeesRequireExactAcceptedFiniteUpperEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*rpc.OrderPreviewResult)
	}{
		{"no margin or fees", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin = nil }},
		{"estimate only", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MaxCommission = nil }},
		{"lower bound only", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.Commission, p.WhatIf.Margin.MaxCommission = nil, nil }},
		{"unaccepted result", func(p *rpc.OrderPreviewResult) { p.WhatIf.Status = rpc.OrderWhatIfStatusRejected }},
		{"unavailable result", func(p *rpc.OrderPreviewResult) { p.WhatIf.Available = false }},
		{"missing currency", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.CommissionCurrency = "" }},
		{"foreign currency", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.CommissionCurrency = "EUR" }},
		{"negative upper", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MaxCommission = new(-1.0) }},
		{"infinite upper", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MaxCommission = new(math.Inf(1)) }},
		{"NaN upper", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MaxCommission = new(math.NaN()) }},
		{"broker sentinel upper", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MaxCommission = new(math.MaxFloat64) }},
		{"minimum above upper", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MinCommission = new(11.0) }},
		{"estimate above upper", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.Commission = new(11.0) }},
		{"estimate below minimum", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.Commission = new(9.0) }},
		{"negative estimate", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.Commission = new(-1.0) }},
		{"NaN minimum", func(p *rpc.OrderPreviewResult) { p.WhatIf.Margin.MinCommission = new(math.NaN()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prop, preview := sweepFeePreview(10000, 1000, 10)
			tc.change(preview)
			if got := proposalPreviewSafetyBlockers(prop, preview); !hasTradingBlocker(got, "cash_sweep_fees_unknown") {
				t.Fatalf("unbounded or contradictory fees passed: %v", got)
			}
		})
	}
	prop, preview := sweepFeePreview(1000, 1000, 0)
	if got := proposalPreviewSafetyBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("explicit broker zero was treated as missing: %v", got)
	}
	prop, preview = sweepFeePreview(1010, 1000, 10)
	preview.WhatIf.Margin.Commission, preview.WhatIf.Margin.MinCommission = nil, nil
	if got := proposalPreviewSafetyBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("a finite exact upper bound needs no lower estimate: %v", got)
	}
}

func TestCashSweepFeeGuardLeavesSellRedemptionsUnchanged(t *testing.T) {
	for _, effect := range []string{rpc.OrderPositionEffectClose, rpc.OrderPositionEffectReduce} {
		t.Run(effect, func(t *testing.T) {
			prop, preview := sweepFeePreview(1000, 1000, 0)
			prop.Action, prop.PositionEffect, prop.CashSweep.Side = rpc.OrderActionSell, effect, rpc.CashSweepSideRedeem
			prop.CashSweep.QuantityUnit = rpc.CashSweepQuantityPosition
			preview.Draft.Action, preview.Position.Effect, preview.WhatIf.Margin = rpc.OrderActionSell, effect, nil
			if got := proposalPreviewSafetyBlockers(prop, preview); len(got) != 0 {
				t.Fatalf("buy fee guard changed ordinary sell redemption: %v", got)
			}
		})
	}
}

func TestCashSweepOutstandingBuysCannotCertifyFeeInclusiveCommitments(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Account: scope.Account,
		Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, SecType: "STK", Currency: "USD", TotalQuantity: 1, Remaining: 1, LimitPrice: 1000}
	queue := queuedAuthRecord{State: rpc.QueuedAuthArmed, Terms: rpc.QueuedAuthTerms{AccountID: scope.Account, AccountMode: scope.Mode,
		Action: rpc.OrderActionBuy, MaxQuantity: 1, WorstPrice: 1000, Currency: "USD", Contract: rpc.ContractParams{SecType: "STK", Currency: "USD"}}}
	for _, tc := range []struct {
		name   string
		orders []ibkrlib.OrderLifecycleEvent
		queue  []queuedAuthRecord
	}{
		{"working", []ibkrlib.OrderLifecycleEvent{order}, nil},
		{"armed", nil, []queuedAuthRecord{queue}},
		{"held", nil, []queuedAuthRecord{{State: rpc.QueuedAuthHeld, Terms: queue.Terms}}},
		{"sending", nil, []queuedAuthRecord{{State: rpc.QueuedAuthSending, Terms: queue.Terms}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committed := cashSweepCommitmentsFrom(tc.orders, tc.queue, scope)
			if !strings.Contains(committed.Unknown["USD"], "commission") || committed.Unknown[""] == "" || committed.ByCurrency["USD"] != 1000 {
				t.Fatalf("principal-only commitments certified cash: %+v", committed)
			}
			in := cashSweepTestInput(map[string]float64{"USD": 20000, "EUR": 20000})
			in.Commitments = committed
			plan := cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeActive, 10000), in, cashSweepTestNow())
			for _, ccy := range []string{"USD", "EUR"} {
				cp := cashSweepCurrencyOf(t, plan, ccy)
				if cp.side != "" || cp.status.State != rpc.CashSweepStateSettlementUnknown {
					t.Fatalf("unknown fee currency allowed %s sweep: %+v", ccy, cp)
				}
			}
		})
	}
	order.Status = "Filled"
	queue.State = rpc.QueuedAuthPrepared
	if got := cashSweepCommitmentsFrom([]ibkrlib.OrderLifecycleEvent{order}, []queuedAuthRecord{queue}, scope); len(got.Unknown) != 0 {
		t.Fatalf("resolved or unarmed work blocks a sweep: %+v", got)
	}
}
