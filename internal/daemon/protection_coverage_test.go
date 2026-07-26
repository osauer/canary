package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestBuildProtectionCoverageCoveredLong(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", 100, 10_000, 50_000)
	order := protectionCoverageTestOrder("MSFT", rpc.OrderActionSell, 100)

	got := buildProtectionCoverage(pos, []rpc.OrderView{order}, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if row.State != rpc.ProtectionCoverageStateCovered || row.ProtectedQuantity != 100 || row.UnprotectedQuantity != 0 {
		t.Fatalf("row = %+v, want covered 100/0", row)
	}
	if got.Counts.Covered != 1 || got.UnprotectedNotionalBase != nil {
		t.Fatalf("summary = %+v, want covered count and no unprotected notional", got)
	}
}

func TestBuildProtectionCoverageCoveredShort(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", -50, -5_000, 50_000)
	order := protectionCoverageTestOrder("MSFT", rpc.OrderActionBuy, 50)

	got := buildProtectionCoverage(pos, []rpc.OrderView{order}, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if row.State != rpc.ProtectionCoverageStateCovered || row.ProtectedQuantity != 50 || row.UnprotectedQuantity != 0 {
		t.Fatalf("row = %+v, want covered short 50/0", row)
	}
	if got.Counts.Covered != 1 || got.Counts.Unprotected != 0 {
		t.Fatalf("summary = %+v, want covered short count", got)
	}
}

func TestBuildProtectionCoveragePartialQuantity(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", 100, 10_000, 50_000)
	order := protectionCoverageTestOrder("MSFT", rpc.OrderActionSell, 40)

	got := buildProtectionCoverage(pos, []rpc.OrderView{order}, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if row.State != rpc.ProtectionCoverageStatePartial || row.ProtectedQuantity != 40 || row.UnprotectedQuantity != 60 {
		t.Fatalf("row = %+v, want partial 40/60", row)
	}
	if row.UnprotectedNotionalBase == nil || *row.UnprotectedNotionalBase != 6000 {
		t.Fatalf("row unprotected base = %+v, want 6000", row.UnprotectedNotionalBase)
	}
	if got.UnprotectedNotionalBase == nil || *got.UnprotectedNotionalBase != 6000 || got.Counts.Partial != 1 {
		t.Fatalf("summary = %+v, want 6000 unprotected partial", got)
	}
}

func TestBuildProtectionCoverageUnprotectedNoOrder(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", 100, 10_000, 50_000)

	got := buildProtectionCoverage(pos, nil, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if row.State != rpc.ProtectionCoverageStateUnprotected || row.ProtectedQuantity != 0 || row.UnprotectedQuantity != 100 {
		t.Fatalf("row = %+v, want unprotected 0/100", row)
	}
	if got.Counts.Unprotected != 1 || len(got.LargestUnprotected) != 1 {
		t.Fatalf("summary = %+v, want one largest unprotected exposure", got)
	}
}

func TestBuildProtectionCoverageDoesNotCountPlainLimitClose(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", 100, 10_000, 50_000)
	order := protectionCoverageTestOrder("MSFT", rpc.OrderActionSell, 100)
	order.OrderType = rpc.OrderTypeLMT
	order.LimitPrice = 120

	got := buildProtectionCoverage(pos, []rpc.OrderView{order}, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if row.State != rpc.ProtectionCoverageStateUnprotected || row.ProtectedQuantity != 0 || row.UnprotectedQuantity != 100 {
		t.Fatalf("row = %+v, want plain close limit not counted as stop protection", row)
	}
	if len(row.Orders) != 0 || got.Counts.Covered != 0 || got.Counts.Unprotected != 1 {
		t.Fatalf("summary = %+v row=%+v, want unprotected with no counted LMT order", got, row)
	}
}

func TestBuildProtectionCoverageReconcileRequiredNotCounted(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", 100, 10_000, 50_000)
	order := protectionCoverageTestOrder("MSFT", rpc.OrderActionSell, 150)
	orders := []rpc.OrderView{order}
	reconcileFlatPositionProtectiveOrders(orders, pos, time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	got := buildProtectionCoverage(pos, orders, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if row.State != rpc.ProtectionCoverageStateReconcileRequired || row.ProtectedQuantity != 0 || row.UnprotectedQuantity != 100 {
		t.Fatalf("row = %+v, want reconcile_required with no counted protection", row)
	}
	if got.Counts.ReconcileRequired != 1 || len(got.ReconcileRequiredOrders) != 1 {
		t.Fatalf("summary = %+v, want one reconcile-required order", got)
	}
}

func TestBuildProtectionCoverageOrphanedOrder(t *testing.T) {
	t.Parallel()
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{}, Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"}}
	order := protectionCoverageTestOrder("PBLS", rpc.OrderActionSell, 200)
	order.ReconciliationState = "position_mismatch"
	order.LifecycleStatus = rpc.OrderLifecycleUnknownReconcileRequired

	got := buildProtectionCoverage(pos, []rpc.OrderView{order}, true, "", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "PBLS")
	if row.State != rpc.ProtectionCoverageStateOrphanedOrder || len(row.Orders) != 1 {
		t.Fatalf("row = %+v, want orphaned_order with the stale order attached", row)
	}
	if got.Counts.OrphanedOrder != 1 || got.Counts.Covered != 0 || len(got.OrphanedOrders) != 1 {
		t.Fatalf("summary = %+v, want orphaned order not counted as coverage", got)
	}
}

func TestBuildProtectionCoverageUnknownWhenOrdersUnavailable(t *testing.T) {
	t.Parallel()
	pos := protectionCoverageTestPositions("MSFT", 100, 10_000, 50_000)

	got := buildProtectionCoverage(pos, nil, false, "order journal unavailable", time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC))

	row := requireProtectionCoverageRow(t, got, "MSFT")
	if got.Status != rpc.ProtectionCoverageStateUnknown || row.State != rpc.ProtectionCoverageStateUnknown || got.Counts.Unknown != 1 {
		t.Fatalf("coverage = %+v row=%+v, want unknown", got, row)
	}
}

func protectionCoverageTestPositions(symbol string, qty, marketValueBase, nlv float64) *rpc.PositionsResult {
	return &rpc.PositionsResult{
		Stocks: []rpc.PositionView{{
			Symbol:          symbol,
			SecType:         "STK",
			Quantity:        qty,
			MarketValueBase: &marketValueBase,
		}},
		Portfolio: &rpc.PositionsPortfolio{
			BaseCurrency:       "USD",
			NetLiquidationBase: &nlv,
		},
	}
}

func protectionCoverageTestOrder(symbol, action string, remaining float64) rpc.OrderView {
	return rpc.OrderView{
		Open:            true,
		ModifyEligible:  true,
		CancelEligible:  true,
		Source:          proposalOrderSource,
		OrderRef:        "ibkr-test-" + symbol,
		Symbol:          symbol,
		SecType:         "STK",
		Action:          action,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        remaining,
		Remaining:       remaining,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
	}
}

func requireProtectionCoverageRow(t *testing.T, summary *rpc.ProtectionCoverageSummary, underlying string) rpc.ProtectionCoverageRow {
	t.Helper()
	if summary == nil {
		t.Fatal("coverage summary is nil")
	}
	for _, row := range summary.ByUnderlying {
		if row.Underlying == underlying {
			return row
		}
	}
	t.Fatalf("coverage rows = %+v, want %s", summary.ByUnderlying, underlying)
	return rpc.ProtectionCoverageRow{}
}
