package daemon

import (
	"cmp"
	"context"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const protectionCoverageQuantityEpsilon = 1e-9

func (s *Server) attachProtectionCoverage(ctx context.Context, res *rpc.PositionsResult, symbolFilter, typeFilter string, portfolioHealth ibkrlib.PortfolioStreamHealth) {
	if res == nil {
		return
	}
	now := time.Now()
	if s != nil {
		now = s.orderNow()
	}
	if s == nil {
		res.ProtectionCoverage = buildProtectionCoverage(res, nil, false, "server unavailable", now)
		return
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		res.ProtectionCoverage = buildProtectionCoverage(res, nil, false, err.Error(), now)
		return
	}
	scope := s.currentBrokerStateScope()
	orders := make([]rpc.OrderView, 0, len(views))
	for _, view := range views {
		if !orderViewMatchesBrokerScope(view, scope) || !protectionCoverageOrderPassesFilter(view, symbolFilter, typeFilter) {
			continue
		}
		orders = append(orders, view)
	}
	reconcileFlatPositionProtectiveOrders(orders, res, now)
	res.ProtectionCoverage = buildProtectionCoverage(res, orders, true, "", now)
}

func protectionCoverageOrderPassesFilter(view rpc.OrderView, symbolFilter, typeFilter string) bool {
	if symbolFilter = strings.ToUpper(strings.TrimSpace(symbolFilter)); symbolFilter != "" && !strings.EqualFold(view.Symbol, symbolFilter) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(typeFilter)) {
	case "", "stk":
		return protectionCoverageOrderIsStockLike(view)
	case "opt":
		return false
	default:
		return true
	}
}

func buildProtectionCoverage(pos *rpc.PositionsResult, orders []rpc.OrderView, ordersAvailable bool, unavailableMessage string, now time.Time) *rpc.ProtectionCoverageSummary {
	out := &rpc.ProtectionCoverageSummary{AsOf: now, Status: "ok"}
	if pos == nil {
		out.Status = rpc.ProtectionCoverageStateUnknown
		out.Message = "positions unavailable"
		out.WarningCodes = append(out.WarningCodes, "positions_unavailable")
		return out
	}
	if !ordersAvailable {
		out.Status = rpc.ProtectionCoverageStateUnknown
		out.Message = unavailableMessage
		out.WarningCodes = append(out.WarningCodes, "order_journal_unavailable")
		for _, row := range pos.Stocks {
			if !protectionCoveragePositionIsStockLike(row) || row.Quantity == 0 {
				continue
			}
			coverageRow := baseProtectionCoverageRow(row, pos)
			coverageRow.State = rpc.ProtectionCoverageStateUnknown
			coverageRow.Message = "open-order journal unavailable; protection coverage unknown"
			out.ByUnderlying = append(out.ByUnderlying, coverageRow)
			out.Counts.Unknown++
		}
		sortProtectionCoverageRows(out.ByUnderlying)
		return out
	}

	for _, row := range pos.Stocks {
		if !protectionCoveragePositionIsStockLike(row) || row.Quantity == 0 {
			continue
		}
		coverageRow := baseProtectionCoverageRow(row, pos)
		positionQty := math.Abs(row.Quantity)
		for _, order := range orders {
			if !protectionCoverageOrderMatchesPosition(order, row) {
				continue
			}
			coverageOrder := protectionCoverageOrder(order)
			if protectionCoverageOrderCounts(order, row) {
				coverageRow.ProtectedQuantity += orderViewRemainingQuantity(order)
			} else {
				coverageRow.Orders = append(coverageRow.Orders, coverageOrder)
				coverageRow.WarningCodes = appendCoverageCode(coverageRow.WarningCodes, "reconcile_required")
			}
		}
		if coverageRow.ProtectedQuantity > positionQty {
			coverageRow.ProtectedQuantity = positionQty
		}
		coverageRow.UnprotectedQuantity = math.Max(positionQty-coverageRow.ProtectedQuantity, 0)
		coverageRow.UnprotectedNotionalBase = protectionCoverageUnprotectedBase(row, coverageRow.UnprotectedQuantity)
		coverageRow.UnprotectedNotionalBaseCurrency = protectionCoverageBaseCurrency(pos)
		switch {
		case containsCoverageCode(coverageRow.WarningCodes, "reconcile_required"):
			coverageRow.State = rpc.ProtectionCoverageStateReconcileRequired
			coverageRow.Message = "open protective order no longer reconciles with the current position"
			out.Counts.ReconcileRequired++
		// The proposal engine already declines to propose for a defunct row,
		// so counting it as unprotected would raise an alarm no proposal can
		// answer — and its zero notional would suppress the euro figure for
		// the whole book.
		case !rpc.ExpectsMarketData(row):
			coverageRow.State = rpc.ProtectionCoverageStateNotProtectable
			coverageRow.WarningCodes = appendCoverageCode(coverageRow.WarningCodes, "zero_value_stock_position")
			coverageRow.Message = "defunct or unquoted holding; no mark to protect — reconcile with broker records"
			out.Counts.NotProtectable++
		case coverageRow.ProtectedQuantity <= protectionCoverageQuantityEpsilon:
			coverageRow.State = rpc.ProtectionCoverageStateUnprotected
			out.Counts.Unprotected++
		case coverageRow.ProtectedQuantity+protectionCoverageQuantityEpsilon < positionQty:
			coverageRow.State = rpc.ProtectionCoverageStatePartial
			out.Counts.Partial++
		default:
			coverageRow.State = rpc.ProtectionCoverageStateCovered
			out.Counts.Covered++
		}
		out.ByUnderlying = append(out.ByUnderlying, coverageRow)
		if coverageRow.UnprotectedNotionalBase != nil {
			out.UnprotectedNotionalBase = addFloatPtr(out.UnprotectedNotionalBase, *coverageRow.UnprotectedNotionalBase)
			if out.UnprotectedNotionalBaseCurrency == "" {
				out.UnprotectedNotionalBaseCurrency = coverageRow.UnprotectedNotionalBaseCurrency
			}
		}
	}

	for _, order := range orders {
		if !protectionCoverageOrderIsProblem(order) || !protectionCoverageOrderIsStopProtective(order) {
			continue
		}
		current := positionQuantityForOrderView(pos, order)
		coverageOrder := protectionCoverageOrder(order)
		if math.Abs(current) <= protectionCoverageQuantityEpsilon {
			row := rpc.ProtectionCoverageRow{
				Underlying:   strings.ToUpper(strings.TrimSpace(order.Symbol)),
				State:        rpc.ProtectionCoverageStateOrphanedOrder,
				Orders:       []rpc.ProtectionCoverageOrder{coverageOrder},
				WarningCodes: []string{"orphaned_order", "reconcile_required"},
				Message:      "protective order remains open but the current position is flat",
			}
			out.ByUnderlying = append(out.ByUnderlying, row)
			out.OrphanedOrders = append(out.OrphanedOrders, coverageOrder)
			out.Counts.OrphanedOrder++
			continue
		}
		if protectionCoverageRowByUnderlying(out.ByUnderlying, order.Symbol) < 0 {
			row := rpc.ProtectionCoverageRow{
				Underlying:       strings.ToUpper(strings.TrimSpace(order.Symbol)),
				State:            rpc.ProtectionCoverageStateReconcileRequired,
				PositionQuantity: current,
				Orders:           []rpc.ProtectionCoverageOrder{coverageOrder},
				WarningCodes:     []string{"reconcile_required"},
				Message:          "protective order no longer reconciles with the current position",
			}
			out.ByUnderlying = append(out.ByUnderlying, row)
			out.Counts.ReconcileRequired++
		}
		out.ReconcileRequiredOrders = append(out.ReconcileRequiredOrders, coverageOrder)
	}

	sortProtectionCoverageRows(out.ByUnderlying)
	out.LargestUnprotected = largestUnprotectedCoverageRows(out.ByUnderlying, 5)
	if out.Counts.Unknown > 0 {
		out.Status = rpc.ProtectionCoverageStateUnknown
	} else if out.Counts.OrphanedOrder > 0 || out.Counts.ReconcileRequired > 0 {
		out.Status = rpc.ProtectionCoverageStateReconcileRequired
	} else if out.Counts.Unprotected > 0 || out.Counts.Partial > 0 {
		out.Status = "review"
	}
	return out
}

func protectionCoveragePositionIsStockLike(row rpc.PositionView) bool {
	switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
	case rpc.SecTypeStock, "STK", "ETF":
		return true
	default:
		return false
	}
}

func protectionCoverageOrderIsStockLike(order rpc.OrderView) bool {
	switch strings.ToUpper(strings.TrimSpace(order.SecType)) {
	case rpc.SecTypeStock, "STK", "ETF":
		return true
	default:
		return false
	}
}

func baseProtectionCoverageRow(row rpc.PositionView, pos *rpc.PositionsResult) rpc.ProtectionCoverageRow {
	out := rpc.ProtectionCoverageRow{
		Underlying:        strings.ToUpper(strings.TrimSpace(row.Symbol)),
		PositionQuantity:  row.Quantity,
		MarketValueBase:   cloneFloat64Ptr(row.MarketValueBase),
		MarketValuePctNLV: protectionCoverageMarketValuePct(row, pos),
	}
	if out.MarketValueBase != nil {
		v := math.Abs(*out.MarketValueBase)
		out.MarketValueBase = &v
	}
	return out
}

func protectionCoverageMarketValuePct(row rpc.PositionView, pos *rpc.PositionsResult) *float64 {
	if row.MarketValueBase == nil || pos == nil || pos.Portfolio == nil || pos.Portfolio.NetLiquidationBase == nil || *pos.Portfolio.NetLiquidationBase <= 0 {
		return nil
	}
	v := math.Abs(*row.MarketValueBase) / *pos.Portfolio.NetLiquidationBase * 100
	return &v
}

func protectionCoverageUnprotectedBase(row rpc.PositionView, unprotectedQty float64) *float64 {
	if row.MarketValueBase == nil || row.Quantity == 0 || unprotectedQty <= protectionCoverageQuantityEpsilon {
		return nil
	}
	v := math.Abs(*row.MarketValueBase) * unprotectedQty / math.Abs(row.Quantity)
	return &v
}

func protectionCoverageBaseCurrency(pos *rpc.PositionsResult) string {
	if pos != nil && pos.Portfolio != nil {
		return strings.ToUpper(strings.TrimSpace(pos.Portfolio.BaseCurrency))
	}
	return ""
}

func protectionCoverageOrderMatchesPosition(order rpc.OrderView, row rpc.PositionView) bool {
	if !protectionCoverageOrderIsStopProtective(order) {
		return false
	}
	return positionViewMatchesOrderView(row, order)
}

func protectionCoverageOrderIsStopProtective(order rpc.OrderView) bool {
	if !order.Open || !protectionCoverageOrderIsStockLike(order) || !protectionCoverageOrderIsStopLike(order) {
		return false
	}
	return strings.EqualFold(order.OpenClose, "C") || strings.EqualFold(order.Source, proposalOrderSource)
}

func protectionCoverageOrderIsStopLike(order rpc.OrderView) bool {
	switch strings.ToUpper(strings.TrimSpace(order.OrderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT, "STP", "STP LMT", "STOP", "STOP LIMIT":
		return true
	default:
		return false
	}
}

func protectionCoverageOrderCounts(order rpc.OrderView, row rpc.PositionView) bool {
	if protectionCoverageOrderIsProblem(order) {
		return false
	}
	switch order.LifecycleStatus {
	case rpc.OrderLifecycleSubmitted, rpc.OrderLifecyclePreSubmitted:
	default:
		return false
	}
	return orderViewActionCanCloseQuantity(order, row.Quantity, orderViewRemainingQuantity(order))
}

func protectionCoverageOrderIsProblem(order rpc.OrderView) bool {
	return strings.EqualFold(order.ReconciliationState, "position_mismatch") ||
		order.LifecycleStatus == rpc.OrderLifecycleUnknownReconcileRequired
}

func protectionCoverageOrder(order rpc.OrderView) rpc.ProtectionCoverageOrder {
	out := rpc.ProtectionCoverageOrder{
		OrderRef:            order.OrderRef,
		Symbol:              order.Symbol,
		SecType:             order.SecType,
		Action:              order.Action,
		OrderType:           order.OrderType,
		TIF:                 order.TIF,
		Remaining:           orderViewRemainingQuantity(order),
		Quantity:            order.Quantity,
		LifecycleStatus:     order.LifecycleStatus,
		ReconciliationState: order.ReconciliationState,
		UpdatedAt:           order.UpdatedAt,
		LastMessage:         order.LastMessage,
	}
	if order.Trail != nil && order.Trail.InitialStopPrice > 0 {
		v := order.Trail.InitialStopPrice
		out.StopPrice = &v
	}
	if order.LimitPrice > 0 {
		v := order.LimitPrice
		out.LimitPrice = &v
	}
	return out
}

func appendCoverageCode(codes []string, code string) []string {
	if containsCoverageCode(codes, code) {
		return codes
	}
	return append(codes, code)
}

func containsCoverageCode(codes []string, code string) bool {
	return slices.Contains(codes, code)
}

func addFloatPtr(current *float64, next float64) *float64 {
	if current == nil {
		v := next
		return &v
	}
	*current += next
	return current
}

func protectionCoverageRowByUnderlying(rows []rpc.ProtectionCoverageRow, underlying string) int {
	underlying = strings.ToUpper(strings.TrimSpace(underlying))
	for i, row := range rows {
		if row.Underlying == underlying {
			return i
		}
	}
	return -1
}

func sortProtectionCoverageRows(rows []rpc.ProtectionCoverageRow) {
	slices.SortStableFunc(rows, func(a, b rpc.ProtectionCoverageRow) int {
		if c := cmp.Compare(protectionCoverageStateRank(a.State), protectionCoverageStateRank(b.State)); c != 0 {
			return c
		}
		return cmp.Compare(a.Underlying, b.Underlying)
	})
}

func protectionCoverageStateRank(state string) int {
	switch state {
	case rpc.ProtectionCoverageStateOrphanedOrder:
		return 0
	case rpc.ProtectionCoverageStateReconcileRequired:
		return 1
	case rpc.ProtectionCoverageStateUnprotected:
		return 2
	case rpc.ProtectionCoverageStatePartial:
		return 3
	case rpc.ProtectionCoverageStateUnknown:
		return 4
	case rpc.ProtectionCoverageStateCovered:
		return 5
	default:
		return 6
	}
}

func largestUnprotectedCoverageRows(rows []rpc.ProtectionCoverageRow, limit int) []rpc.ProtectionCoverageRow {
	candidates := make([]rpc.ProtectionCoverageRow, 0, len(rows))
	for _, row := range rows {
		if row.UnprotectedNotionalBase != nil && *row.UnprotectedNotionalBase > 0 {
			candidates = append(candidates, row)
		}
	}
	slices.SortStableFunc(candidates, func(a, b rpc.ProtectionCoverageRow) int {
		return cmp.Compare(*b.UnprotectedNotionalBase, *a.UnprotectedNotionalBase)
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}
