package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func trailModifyTestView() rpc.OrderView {
	return rpc.OrderView{
		OrderRef:        "ord-trail",
		ReservedOrderID: 1001,
		Symbol:          "DTE",
		SecType:         "STK",
		Exchange:        "IBIS",
		Currency:        "EUR",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        5,
		Remaining:       5,
		Trail: &rpc.OrderTrailSpec{
			Basis:            rpc.OrderTrailBasisInstrumentPrice,
			OffsetType:       rpc.OrderTrailOffsetPercent,
			TrailingPercent:  new(3.0),
			InitialStopPrice: 198.5,
		},
	}
}

func trailModifyTestDraft() rpc.OrderDraft {
	return rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Contract:  rpc.ContractParams{Symbol: "DTE", SecType: "STK", Exchange: "IBIS", Currency: "EUR"},
		Quantity:  5,
		OrderType: rpc.OrderTypeTRAIL,
		TIF:       rpc.OrderTIFGTC,
		Strategy:  rpc.OrderStrategyBrokerTrail,
		OrderRef:  "ibkr-20260610-090000",
		Trail: &rpc.OrderTrailSpec{
			Basis:            rpc.OrderTrailBasisInstrumentPrice,
			OffsetType:       rpc.OrderTrailOffsetPercent,
			TrailingPercent:  new(2.0),
			InitialStopPrice: 199.25,
		},
	}
}

func TestValidateModifyDraftTrailReprice(t *testing.T) {
	t.Parallel()
	if err := validateModifyDraft(trailModifyTestView(), trailModifyTestDraft()); err != nil {
		t.Fatalf("trail re-price draft should validate, got %v", err)
	}

	// Quantity reduction stays allowed; switching the offset style is a
	// legitimate trail change as long as exactly one offset is present.
	draft := trailModifyTestDraft()
	draft.Quantity = 3
	draft.Trail.TrailingPercent = nil
	draft.Trail.TrailingAmount = new(4.0)
	draft.Trail.OffsetType = rpc.OrderTrailOffsetAmount
	if err := validateModifyDraft(trailModifyTestView(), draft); err != nil {
		t.Fatalf("trail amount re-price draft should validate, got %v", err)
	}
}

func TestValidateModifyDraftTrailFrozenFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(view *rpc.OrderView, draft *rpc.OrderDraft)
		wantErr string
	}{
		{"order type change", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.OrderType = rpc.OrderTypeTRAILLIMIT
			draft.Trail.LimitOffset = new(0.2)
		}, "cannot change order type"},
		{"tif change", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.TIF = rpc.OrderTIFDay
		}, "cannot change time-in-force"},
		{"trigger method change", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.TriggerMethod = rpc.OrderTriggerMethodLast
		}, "cannot change trigger_method"},
		{"action change", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Action = rpc.OrderActionBuy
		}, "cannot change action"},
		{"exchange change", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Contract.Exchange = "SMART"
		}, "cannot change exchange"},
		{"quantity above remaining", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Quantity = 6
		}, "no more than remaining"},
		{"missing trail fields", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Trail = nil
		}, "requires trail fields"},
		{"both trail offsets", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Trail.TrailingAmount = new(4.0)
		}, "exactly one of trailing_percent or trailing_amount"},
		{"limit price on trail", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.LimitPrice = 199
		}, "must not include limit_price"},
		{"limit offset on plain trail", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Trail.LimitOffset = new(0.2)
		}, "must not include limit_offset"},
		{"missing initial stop", func(_ *rpc.OrderView, draft *rpc.OrderDraft) {
			draft.Trail.InitialStopPrice = 0
		}, "positive initial stop price"},
		{"unsupported view order type", func(view *rpc.OrderView, draft *rpc.OrderDraft) {
			view.OrderType = "STP"
			draft.OrderType = "STP"
		}, "supports LMT, TRAIL, and TRAIL LIMIT"},
		{"gtc lmt stays rejected", func(view *rpc.OrderView, draft *rpc.OrderDraft) {
			view.OrderType = rpc.OrderTypeLMT
			view.Trail = nil
			view.TIF = rpc.OrderTIFGTC
			draft.OrderType = rpc.OrderTypeLMT
			draft.Trail = nil
			draft.TIF = rpc.OrderTIFGTC
			draft.LimitPrice = 199
		}, "DAY time-in-force only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			view := trailModifyTestView()
			draft := trailModifyTestDraft()
			tc.mutate(&view, &draft)
			err := validateModifyDraft(view, draft)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateModifyDraft err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateModifyDraftTrailLimitRequiresOffset(t *testing.T) {
	t.Parallel()
	view := trailModifyTestView()
	view.OrderType = rpc.OrderTypeTRAILLIMIT
	view.Trail.LimitOffset = new(0.2)
	draft := trailModifyTestDraft()
	draft.OrderType = rpc.OrderTypeTRAILLIMIT
	err := validateModifyDraft(view, draft)
	if err == nil || !strings.Contains(err.Error(), "requires positive limit_offset") {
		t.Fatalf("validateModifyDraft err = %v, want limit_offset requirement", err)
	}
	draft.Trail.LimitOffset = new(0.3)
	if err := validateModifyDraft(view, draft); err != nil {
		t.Fatalf("TRAIL LIMIT re-price draft should validate, got %v", err)
	}
}
