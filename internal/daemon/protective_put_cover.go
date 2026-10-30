package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// The 2026-10-30 release review found that successive put buys could each
// reuse the same held shares. Preserve the existing covered-put exemption,
// but charge working purchases and share sales before granting it. The
// inventory and fill receipt are prepared outside the wire lock; its final
// guard reads the same inventory and durable intents without broker I/O.

// readAddPutCover warms the existing shared inventory once, with the same
// short bound as orders.open. Missing evidence leaves the optional view
// unavailable; it never says that all shares have already been covered.
func (s *Server) readAddPutCover(ctx context.Context, contract rpc.ContractParams) *rpc.AddPutCover {
	wait, cancel := context.WithTimeout(ctx, ordersOpenInventoryWait)
	defer cancel()
	if _, _, err := s.brokerOpenOrderInventory(wait, false); err != nil {
		return nil
	}
	return s.addPutCover(contract)
}

func heldPutCoverIn(input *mandate.Input, symbol, currency string) (shares, covered float64, ok bool) {
	if input == nil || normSym(symbol) == "" {
		return 0, 0, false
	}
	ccy := normCcy(currency)
	for _, l := range input.Legs {
		if normSym(l.Underlying) != normSym(symbol) || l.Quantity <= 0 || l.Kind != mandate.KindStock && !(l.Kind == mandate.KindOption && strings.EqualFold(l.Right, "P")) {
			continue
		}
		if ccy == "" {
			ccy = normCcy(l.Currency)
		}
		// A symbol alone cannot establish that another currency's listing
		// supplies the option's deliverable. Unknown identity gets no exemption.
		if ccy == "" || normCcy(l.Currency) != ccy || !positiveFinite(l.Quantity) || mandateLegConID(l.ID) <= 0 {
			return 0, 0, false
		}
		if l.Kind == mandate.KindStock {
			shares += l.Quantity
		} else {
			if !positiveFinite(l.Multiplier) {
				return 0, 0, false
			}
			covered += l.Quantity * l.Multiplier
		}
	}
	return shares, covered, !math.IsNaN(shares) && !math.IsInf(shares, 0) && !math.IsNaN(covered) && !math.IsInf(covered, 0)
}

// currentMandateCommitments is the already-prepared all-client receipt and
// durable journal. Caller-supplied broker OrderRef text never establishes a
// replacement identity; only the authenticated journal match can supply it.
func (s *Server) currentMandateCommitments() (ibkr.OpenOrderSnapshot, journalInventory, []orderJournalEvent, error) {
	scope := s.currentBrokerStateScope()
	var snapshot ibkr.OpenOrderSnapshot
	if s.openOrderInventoryForTest != nil {
		var got brokerStateScope
		var err error
		snapshot, got, err = s.openOrderInventoryForTest(context.Background(), false)
		if err != nil || !sameBrokerScope(got, scope) {
			return snapshot, journalInventory{}, nil, fmt.Errorf("current broker purchase commitments are unavailable")
		}
	} else {
		binding := s.currentProtectionOrderSnapshotBinding()
		var ok bool
		snapshot, ok = s.cachedProtectionOrderSnapshot(binding, s.orderNow())
		if !ok || !sameBrokerScope(binding.scope, scope) || !s.protectionOrderSnapshotBindingCurrent(binding) {
			return snapshot, journalInventory{}, nil, fmt.Errorf("current broker purchase commitments are unavailable")
		}
	}
	if !protectionOrderSnapshotUsable(snapshot, s.orderNow()) || s.orderJournal == nil {
		return snapshot, journalInventory{}, nil, fmt.Errorf("current broker or durable purchase commitments are unavailable")
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return snapshot, journalInventory{}, nil, fmt.Errorf("durable purchase commitments are unavailable")
	}
	return snapshot, journalOrderInventoryFromEvents(events), events, nil
}

// pendingPutCover reserves every remaining put buy and share sale, including
// uncertain local sends absent from the receipt. Pending put sales and stock
// buys never create cover before positions show their fills.
func (s *Server) pendingPutCover(draft rpc.OrderDraft) (sold, bought float64, err error) {
	snapshot, journal, _, err := s.currentMandateCommitments()
	if err != nil {
		return 0, 0, err
	}
	scope := s.currentBrokerStateScope()
	addContract := func(c rpc.ContractParams, action string, remaining float64) error {
		sec := strings.ToUpper(strings.TrimSpace(c.SecType))
		switch sec {
		case "STK", "ETF", "OPT":
		case "FUT", "FOP", "CASH", "BOND", "BILL", "IND", "CFD":
			return nil
		default:
			return fmt.Errorf("a working order has unknown protection identity")
		}
		if normSym(c.Symbol) == "" {
			return fmt.Errorf("a working order has unknown protection identity")
		}
		if normSym(c.Symbol) != normSym(draft.Contract.Symbol) {
			return nil
		}
		if c.ConID <= 0 || normCcy(c.Currency) != normCcy(draft.Contract.Currency) || !positiveFinite(remaining) {
			return fmt.Errorf("a working order has unmeasured protection commitments")
		}
		if !strings.EqualFold(action, rpc.OrderActionBuy) && !strings.EqualFold(action, rpc.OrderActionSell) {
			return fmt.Errorf("a working order has unknown protection direction")
		}
		if sec == "OPT" {
			if c.Right != "C" && c.Right != "P" || c.Multiplier <= 0 {
				return fmt.Errorf("a working option has unknown protection terms")
			}
			if c.Right == "P" && strings.EqualFold(action, rpc.OrderActionBuy) {
				bought += remaining * float64(c.Multiplier)
			}
		} else if strings.EqualFold(action, rpc.OrderActionSell) {
			sold += remaining
		}
		return nil
	}
	add := func(view rpc.OrderView, remaining float64, legs map[int]rpc.StrategyOrderLeg) error {
		if view.OrderRef != "" && view.OrderRef == draft.OrderRef {
			return nil
		}
		if strings.EqualFold(view.SecType, "BAG") {
			if len(legs) == 0 || !positiveFinite(remaining) {
				return fmt.Errorf("a working combination has unknown protection commitments")
			}
			for _, leg := range legs {
				if leg.Ratio <= 0 {
					return fmt.Errorf("a working combination has unknown leg quantities")
				}
				if err := addContract(leg.Contract, leg.Action, remaining*float64(leg.Ratio)); err != nil {
					return err
				}
			}
			return nil
		}
		return addContract(rpc.ContractParams{ConID: view.ConID, Symbol: view.Symbol, SecType: view.SecType, Currency: view.Currency, Right: view.Right, Multiplier: view.Multiplier}, view.Action, remaining)
	}
	seen := map[int]bool{}
	legsOf := func(view *rpc.OrderView) map[int]rpc.StrategyOrderLeg {
		if view == nil || view.OrderRef == "" || view.PreviewTokenID == "" {
			return nil
		}
		return journal.legsByRef[view.OrderRef]
	}
	for _, row := range brokerWorkingOrders(snapshot, journal.views, scope) {
		view := untrackedOrderView(row.Order, scope, snapshot.AsOf)
		if row.Journal != nil {
			view.OrderRef = row.Journal.OrderRef
			if view.OrderRef != draft.OrderRef && (view.Quantity != row.Journal.Quantity || view.Action != row.Journal.Action || view.ConID != row.Journal.ConID) {
				return 0, 0, fmt.Errorf("a modified protection commitment is not reconciled")
			}
		}
		if err := add(view, brokerOrderRemaining(row.Order), legsOf(row.Journal)); err != nil {
			return 0, 0, err
		}
		for i := range journal.views {
			if row.Journal == &journal.views[i] {
				seen[i] = true
			}
		}
	}
	for i, view := range journal.views {
		if seen[i] || !view.Open || !orderViewMatchesBrokerScope(view, scope) {
			continue
		}
		if err := add(view, orderViewRemainingQuantity(view), legsOf(&view)); err != nil {
			return 0, 0, err
		}
	}
	return sold, bought, nil
}
