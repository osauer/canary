package daemon

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A put on a Rulebook hedge symbol is index protection for the whole long
// book, not cover for shares held (owner, 2026-11-24 07:00 CET: buying SPY
// puts must work from Add; the owner holds no SPY shares). The order gates
// already pass it as a hedge entry (riskAddingOrder). Add sizes it against
// rule 12's own over-hedge line (risk.HedgeLine, the line the hedge trim
// sells back from and the loss measure counts past), so Add never offers
// puts the measure would count as risk.
//
// When the line cannot be measured (a positions read that is not current
// and whole, a name or a held hedge put without its delta, no exchange rate,
// no model delta for the put, unread working orders), Add does not refuse
// the owner his protection: the size is held to the option cap in force and
// the budget, with Unmeasured saying why. What bounds it then: a long put
// loses at most its premium, which the owner's budget sets and the plan's
// debit includes; [order_limits] max_option_contracts and the money caps,
// which the plan holds and the preview re-checks; the device confirmation of
// the order. Worst case: up to the option cap of puts past the line, whose
// premium the loss measure counts at today's value once held. Without a
// known option cap nothing bounds the count but the budget, so the plan is
// refused. A book with no long exposure is measured, not unmeasured: an
// index put protects nothing there, and the plan is refused.

// addIndexNeed is rule 12's measurement for one index put read: the view
// Add returns and the rate that turns the put's delta into base currency.
type addIndexNeed struct {
	view rpc.AddIndexProtection
	fx   float64
}

func (n addIndexNeed) measured() bool { return n.view.Unmeasured == "" }

// readAddIndexProtection measures the long book, the held hedge puts and
// the working put buys on the symbol, after warming the shared inventory
// with the same short bound as orders.open.
func (s *Server) readAddIndexProtection(ctx context.Context, contract rpc.ContractParams) addIndexNeed {
	unmeasured := func(why string) addIndexNeed { return addIndexNeed{view: rpc.AddIndexProtection{Unmeasured: why}} }
	wait, cancel := context.WithTimeout(ctx, ordersOpenInventoryWait)
	defer cancel()
	if _, _, err := s.brokerOpenOrderInventory(wait, false); err != nil {
		return unmeasured("working orders cannot be read")
	}
	_, bought, err := s.pendingPutCover(rpc.OrderDraft{Contract: contract})
	if err != nil {
		return unmeasured("working orders cannot be read")
	}
	if s.tradeProposals == nil {
		return unmeasured("the rulebook cannot measure the book")
	}
	acct, err := s.buildAccountSummary(ctx, false)
	if err != nil || acct == nil {
		return unmeasured("the account cannot be read")
	}
	pos, err := s.mandatePositions(ctx)
	if err != nil {
		return unmeasured("positions cannot be read")
	}
	a, _, base := s.tradeProposals.hedgeTrimAssessment(ctx, acct, pos, s.nowUTC())
	working := 0
	if contract.Multiplier > 0 {
		working = int(math.Ceil(bought/float64(contract.Multiplier) - 1e-9))
	}
	return addIndexProtectionFrom(a, base, pos, contract.Currency, working)
}

// addIndexProtectionFrom turns rule 12's assessment into Add's view.
func addIndexProtectionFrom(a risk.HedgeTrimAssessment, base string, pos *rpc.PositionsResult, currency string, working int) addIndexNeed {
	switch a.State {
	case risk.HedgeTrimInside, risk.HedgeTrimPlanned, risk.HedgeTrimNoLongBook:
	default:
		why := a.Reason
		if why == "" {
			why = "the book cannot be measured"
		}
		return addIndexNeed{view: rpc.AddIndexProtection{Unmeasured: "index protection cannot be measured against the book: " + why}}
	}
	fx, ok := addContractRateToBase(pos, base, currency)
	if !ok {
		return addIndexNeed{view: rpc.AddIndexProtection{Unmeasured: "no " + normCcy(currency) + " exchange rate to the base currency"}}
	}
	l := a.Line
	return addIndexNeed{fx: fx, view: rpc.AddIndexProtection{BaseCurrency: base, LongBase: l.GrossLongBase, ProtectionBase: l.ShortBase,
		RatioPct: l.RatioPct(), BandTopPct: l.TopPct, LinePct: l.ActPct, RoomBase: l.RoomBase(), WorkingContracts: working}}
}

// addContractRateToBase is the rate a position in the currency carries to
// the base currency, the one rule 12 sums the book in.
func addContractRateToBase(pos *rpc.PositionsResult, base, currency string) (float64, bool) {
	if normCcy(base) == "" || normCcy(currency) == "" {
		return 0, false
	}
	if normCcy(base) == normCcy(currency) {
		return 1, true
	}
	if pos == nil {
		return 0, false
	}
	for _, rows := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
		for _, row := range rows {
			if normCcy(row.Currency) != normCcy(currency) {
				continue
			}
			if rate, ok := positionBaseRate(row, base); ok {
				return rate, true
			}
		}
	}
	return 0, false
}

// room is the contracts of a put with this delta and underlying price that
// still fit under the over-hedge line, after the working buys; false when
// the put's delta cannot be measured.
func (n addIndexNeed) room(delta, underlying *float64, multiplier int) (int, bool) {
	if !n.measured() || delta == nil || underlying == nil || multiplier <= 0 {
		return 0, false
	}
	per := math.Abs(*delta) * float64(multiplier) * *underlying * n.fx
	if !positiveFinite(per) {
		return 0, false
	}
	return max(0, int(math.Floor(n.view.RoomBase/per+1e-9))-n.view.WorkingContracts), true
}

// addIndexPutCover is Add's put_cover for an index put: the most contracts
// any of the rows may still add, each row's own written onto it. Unmeasured,
// or with no row's delta measured, the room is the option cap.
func addIndexPutCover(n addIndexNeed, rows []*rpc.AddCallCandidate, spot *float64, optionCap int) *rpc.AddPutCover {
	view := n.view
	out := &rpc.AddPutCover{Kind: rpc.AddPutCoverIndexProtection, IndexProtection: &view}
	measured := false
	for _, r := range rows {
		if r == nil || r.Contract == nil || r.Quote == nil {
			continue
		}
		if room, ok := n.room(r.Quote.Delta, spot, r.Contract.Multiplier); ok {
			r.HedgeRoomContracts = new(room)
			out.RoomContracts = max(out.RoomContracts, room)
			measured = true
		}
	}
	switch {
	case measured:
	case n.measured() && !(n.view.RoomBase > 0):
		// Measured with no room: no delta can make a put fit.
	default:
		out.RoomContracts = optionCap
		if n.measured() {
			view.Unmeasured = "the puts' deltas cannot be read"
		}
	}
	return out
}

// addIndexPutModel reads the exact put's model delta and underlying price,
// the inputs its short delta is measured with; nil when the broker gives none.
func (s *Server) addIndexPutModel(ctx context.Context, c rpc.ContractParams) (delta, underlying *float64) {
	// The read seam's IBKR connector (spreadReads): option models are IBKR's.
	_, conn, err := s.spreadReads()
	if err != nil {
		return nil, nil
	}
	binding, ok := conn.CaptureSession()
	if !ok {
		return nil, nil
	}
	read, err := s.readExactContractQuote(ctx, conn, binding, c, setupQuoteBudget, false, false, true)
	m := read.sizes.Model
	if err != nil || !conn.SessionCurrent(binding) || m == nil || m.Contract.ConID != c.ConID || m.ReceivedAt.After(time.Now()) || time.Since(m.ReceivedAt) > setupQuoteMaxAge {
		return nil, nil
	}
	return m.Delta, m.Underlying
}

// addIndexPutRoom is the plan's room for an index put: the contracts under
// the over-hedge line, or -1 (the option cap alone) with the line
// unmeasured. A measured book with no room, or an unmeasured one without a
// known option cap, refuses the plan.
func (s *Server) addIndexPutRoom(ctx context.Context, c rpc.ContractParams, optionCap int) (int, *rpc.AddIndexProtection, error) {
	n := s.readAddIndexProtection(ctx, c)
	var delta, underlying *float64
	if n.measured() && n.view.RoomBase > 0 {
		delta, underlying = s.addIndexPutModel(ctx, c)
	}
	return addIndexPutRoomFrom(n, delta, underlying, c.Multiplier, optionCap)
}

func addIndexPutRoomFrom(n addIndexNeed, delta, underlying *float64, multiplier, optionCap int) (int, *rpc.AddIndexProtection, error) {
	if n.measured() {
		if !(n.view.RoomBase > 0) {
			return 0, nil, addIndexPutFull(n.view)
		}
		room, ok := n.room(delta, underlying, multiplier)
		switch {
		case ok && room == 0:
			return 0, nil, addIndexPutFull(n.view)
		case ok:
			return room, &n.view, nil
		}
		n.view.Unmeasured = "the put's delta cannot be read"
	}
	if optionCap <= 0 {
		return 0, nil, errBadRequest("Index protection cannot be measured (" + n.view.Unmeasured + ") and the option contract limit is unknown, so no size can be bounded")
	}
	return -1, &n.view, nil
}

func addIndexPutFull(v rpc.AddIndexProtection) error {
	if !(v.LongBase > 0) {
		return errBadRequest("No long book is left for index protection to protect")
	}
	return errBadRequest(fmt.Sprintf("Index protection is %.0f%% of the long book; more puts would pass the %.0f%% over-hedge line", v.RatioPct, v.LinePct))
}
