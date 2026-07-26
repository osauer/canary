package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func mismatchTestView(action string, quantity float64) rpc.OrderView {
	return rpc.OrderView{
		Open:            true,
		ModifyEligible:  true,
		CancelEligible:  true,
		Source:          proposalOrderSource,
		Symbol:          "AMD",
		SecType:         "STK",
		Action:          action,
		OrderType:       rpc.OrderTypeTRAIL,
		Quantity:        quantity,
		Remaining:       quantity,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
	}
}

func TestClassifyProtectiveMismatchPartialSellIsExcess(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	views := []rpc.OrderView{mismatchTestView(rpc.OrderActionSell, 100)}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "AMD", SecType: "STK", Quantity: 50}}}

	reconcileFlatPositionProtectiveOrders(views, pos, now)
	got := views[0]
	if got.ReconciliationKind != rpc.OrderReconciliationKindShortEntryExcess {
		t.Fatalf("kind = %q, want short_entry_excess", got.ReconciliationKind)
	}
	if got.ReconciliationSeverity != rpc.OrderReconciliationSeverityCritical {
		t.Fatalf("severity = %q, want critical", got.ReconciliationSeverity)
	}
	if got.ShortRiskQuantity != 50 || got.ReduceToQuantity != 50 {
		t.Fatalf("short_risk=%v reduce_to=%v, want 50/50", got.ShortRiskQuantity, got.ReduceToQuantity)
	}
}

func TestClassifyProtectiveMismatchFlatIsFull(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	views := []rpc.OrderView{mismatchTestView(rpc.OrderActionSell, 100)}

	reconcileFlatPositionProtectiveOrders(views, &rpc.PositionsResult{}, now)
	got := views[0]
	if got.ReconciliationKind != rpc.OrderReconciliationKindShortEntryFull {
		t.Fatalf("kind = %q, want short_entry_full", got.ReconciliationKind)
	}
	if got.ShortRiskQuantity != 100 || got.ReduceToQuantity != 0 {
		t.Fatalf("short_risk=%v reduce_to=%v, want 100/absent", got.ShortRiskQuantity, got.ReduceToQuantity)
	}
}

func TestClassifyProtectiveMismatchBuyCoverMirror(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	views := []rpc.OrderView{mismatchTestView(rpc.OrderActionBuy, 100)}
	views[0].OpenClose = "C"
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "AMD", SecType: "STK", Quantity: -40}}}

	reconcileFlatPositionProtectiveOrders(views, pos, now)
	got := views[0]
	if got.ReconciliationKind != rpc.OrderReconciliationKindShortEntryExcess {
		t.Fatalf("kind = %q, want short_entry_excess for oversized cover", got.ReconciliationKind)
	}
	if got.ShortRiskQuantity != 60 || got.ReduceToQuantity != 40 {
		t.Fatalf("short_risk=%v reduce_to=%v, want 60/40", got.ShortRiskQuantity, got.ReduceToQuantity)
	}
}

func TestCoveredProtectiveOrderMatchesWireStockSecType(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	// Position rows use the wire label "STOCK" while order views use "STK";
	// a conid-less order must still match its position (2026-07-19 paper
	// rehearsal caught the false "flat" flag this mismatch produced).
	views := []rpc.OrderView{mismatchTestView(rpc.OrderActionSell, 1)}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "AMD", SecType: "STOCK", Quantity: 1}}}

	reconcileFlatPositionProtectiveOrders(views, pos, now)
	if got := views[0]; got.ReconciliationKind != "" {
		t.Fatalf("covered STOCK-labeled position flagged: %+v", got)
	}
}

func TestCoveredProtectiveOrderCarriesNoMismatchKind(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	views := []rpc.OrderView{mismatchTestView(rpc.OrderActionSell, 100)}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "AMD", SecType: "STK", Quantity: 100}}}

	reconcileFlatPositionProtectiveOrders(views, pos, now)
	if got := views[0]; got.ReconciliationKind != "" || got.ReconciliationSeverity != "" {
		t.Fatalf("covered order carries mismatch fields: %+v", got)
	}
}

func TestValidateProtectiveModifyQuantityGate(t *testing.T) {
	t.Parallel()
	amount := 8.5
	view := mismatchTestView(rpc.OrderActionSell, 100)
	view.Trail = &rpc.OrderTrailSpec{TrailingAmount: &amount, InitialStopPrice: 90}
	draft := rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Quantity:  50,
		OrderType: rpc.OrderTypeTRAIL,
		Trail:     &rpc.OrderTrailSpec{TrailingAmount: &amount, InitialStopPrice: 90},
	}

	// Healthy coverage: gate stays out of the way, any allowed quantity.
	if err := validateProtectiveModifyQuantity(view, draft, 100); err != nil {
		t.Fatalf("covered position rejected: %v", err)
	}
	// Mismatch: exactly the coverage passes.
	if err := validateProtectiveModifyQuantity(view, draft, 50); err != nil {
		t.Fatalf("reduce-to-coverage rejected: %v", err)
	}
	// Mismatch: any other quantity fails.
	wrongQty := draft
	wrongQty.Quantity = 60
	if err := validateProtectiveModifyQuantity(view, wrongQty, 50); err == nil {
		t.Fatal("quantity above coverage must be rejected")
	}
	// Mismatch: trail change fails.
	otherAmount := 9.5
	rePriced := draft
	rePriced.Trail = &rpc.OrderTrailSpec{TrailingAmount: &otherAmount, InitialStopPrice: 90}
	if err := validateProtectiveModifyQuantity(view, rePriced, 50); err == nil {
		t.Fatal("trail change on a mismatched order must be rejected")
	}
	// Flat: nothing modifiable.
	if err := validateProtectiveModifyQuantity(view, draft, 0); err == nil {
		t.Fatal("flat position must reject modify entirely")
	}
	// Non-protective orders are untouched by the gate.
	plain := view
	plain.Source = ""
	plain.OpenClose = ""
	if err := validateProtectiveModifyQuantity(plain, wrongQty, 50); err != nil {
		t.Fatalf("non-protective order gated: %v", err)
	}
}
