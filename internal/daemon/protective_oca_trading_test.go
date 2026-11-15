//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// closeLinkRig is a paper trading server holding one line, with Canary's own
// working sell stop on it (order 1002, "ord-prot") and seams for the
// projection, the broker's open-order inventory, the preview and the broker.
// Every broker send is recorded in order, and the fake broker acknowledges a
// modify or a cancel of the stop in the journal the way IBKR's callbacks do.
type closeLinkRig struct {
	srv      *Server
	mu       sync.Mutex
	now      time.Time
	position float64
	// stop is what the inventory reports for order 1002: absent when
	// stopRemaining is zero. stopGroup and stopType are the broker's echo
	// of its one-cancels-all membership, empty for a stop placed before
	// Canary gave stops a group.
	stopRemaining float64
	stopGroup     string
	stopType      int
	// placedRemaining is what the inventory reports for each order the
	// rig placed, by broker order ID; absent when zero.
	placedRemaining      map[int]float64
	hand                 []ibkrlib.OrderLifecycleEvent
	sends                []string
	placed, modified     []*ibkrlib.RawOrder
	cancelled            []int
	failModify, failCank error
	// silent makes the fake broker send no acknowledgement back.
	silent bool
	// option makes the line an option (5 contracts) instead of a stock.
	option bool
}

const (
	closeLinkStockConID  = 910001
	closeLinkOptionConID = 9001
)

func closeLinkContract(option bool) rpc.ContractParams {
	if option {
		return rpc.ContractParams{ConID: closeLinkOptionConID, Symbol: "SYW", SecType: "OPT", Exchange: "SMART", Currency: "USD",
			Expiry: "20261218", Strike: 150, Right: "C", Multiplier: 100}
	}
	return rpc.ContractParams{ConID: closeLinkStockConID, Symbol: "SYW", SecType: "STK", Exchange: "SMART", Currency: "USD"}
}

// seedCanarySellStop journals Canary's own working sell stop, acknowledged
// by the broker, with the one-cancels-all group it was sent in (empty for a
// stop from before groups existed).
func seedCanarySellStop(t *testing.T, srv *Server, now time.Time, option bool, qty float64, group string) {
	t.Helper()
	contract := closeLinkContract(option)
	amount := 8.5
	tif := rpc.OrderTIFGTC
	if option {
		amount = 0.6
		tif = rpc.OrderTIFDay
	}
	ev := orderJournalEvent{
		At: now.Add(-time.Minute), Type: orderJournalEventBrokerAcknowledged,
		OrderRef: "ord-prot", PreviewTokenID: "tok-prot", ReservedOrderID: 1002, ClientID: 31,
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Source: proposalOrderSource, OpenClose: "C",
		Symbol: contract.Symbol, SecType: contract.SecType, ConID: contract.ConID, Exchange: contract.Exchange, Currency: contract.Currency,
		Expiry: contract.Expiry, Strike: contract.Strike, Right: contract.Right, Multiplier: contract.Multiplier,
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TIF: tif,
		Quantity: qty, Remaining: qty, Trail: &rpc.OrderTrailSpec{TrailingAmount: &amount, InitialStopPrice: 90},
		Status: "Submitted", SendState: orderSendStateBrokerAcknowledged,
	}
	if group != "" {
		ev.OCAGroup, ev.OCAType = group, ibkrlib.OcaTypeReduceWithBlock
	}
	if err := srv.orderJournal.Append(ev); err != nil {
		t.Fatalf("seed stop: %v", err)
	}
}

func newCloseLinkRig(t *testing.T, option bool, stopGroup string) *closeLinkRig {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	held := 70.0
	if option {
		// Under the test rig's option cap of 5 contracts.
		held = 5
	}
	r := &closeLinkRig{srv: srv, now: time.Date(2026, 10, 12, 15, 40, 0, 0, time.UTC), position: held,
		stopRemaining: held, stopGroup: stopGroup, option: option, placedRemaining: map[int]float64{}}
	if stopGroup != "" {
		r.stopType = ibkrlib.OcaTypeReduceWithBlock
	}
	srv.now = r.clock
	srv.decisions = newDecisionLog(filepath.Join(t.TempDir(), "daemon.db"))
	t.Cleanup(func() { _ = srv.decisions.Close() })
	seedCanarySellStop(t, srv, r.now, option, held, stopGroup)
	contract := closeLinkContract(option)
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var orders []ibkrlib.OrderLifecycleEvent
		if r.stopRemaining > 0 {
			orders = append(orders, ibkrlib.OrderLifecycleEvent{
				Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 31, ClientIDPresent: true, Account: "DU1234567",
				Symbol: contract.Symbol, SecType: contract.SecType, ConID: contract.ConID, Currency: contract.Currency,
				Expiry: contract.Expiry, Strike: contract.Strike, Right: contract.Right, Multiplier: contract.Multiplier,
				Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TotalQuantity: held, Remaining: r.stopRemaining,
				Filled: held - r.stopRemaining, Status: "Submitted", OcaGroup: r.stopGroup, OcaType: r.stopType,
			})
		}
		// The broker lists what the rig placed, as reqAllOpenOrders would.
		for _, placed := range r.placed {
			remaining, ok := r.placedRemaining[placed.OrderID]
			if !ok {
				remaining = float64(placed.TotalQty)
			}
			if remaining <= 0 {
				continue
			}
			orders = append(orders, ibkrlib.OrderLifecycleEvent{
				Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: placed.OrderID, ClientID: 31, ClientIDPresent: true, Account: "DU1234567",
				Symbol: contract.Symbol, SecType: contract.SecType, ConID: contract.ConID, Currency: contract.Currency,
				Expiry: contract.Expiry, Strike: contract.Strike, Right: contract.Right, Multiplier: contract.Multiplier,
				Action: placed.Action, OrderType: placed.OrderType, TotalQuantity: float64(placed.TotalQty), Remaining: remaining,
				Filled: float64(placed.TotalQty) - remaining, Status: "Submitted", OcaGroup: placed.OcaGroup, OcaType: placed.OcaType,
			})
		}
		orders = append(orders, r.hand...)
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: r.now, Orders: orders}, scope, nil
	}
	srv.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return []*ibkrlib.RawPosition{{Account: "DU1234567", Position: r.position,
			Contract: ibkrlib.Contract{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Currency: contract.Currency,
				Expiry: contract.Expiry, Strike: contract.Strike, Right: contract.Right, Multiplier: contract.Multiplier}}}, true
	}
	srv.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		after := r.position - float64(qty)
		if action == rpc.OrderActionBuy {
			after = r.position + float64(qty)
		}
		return rpc.OrderPositionImpact{Before: r.position, After: after, Effect: classifyPositionEffect(r.position, after)}, nil
	}
	srv.orderPreviewQuote = fixedPreviewQuote(99.9, 100.1)
	if option {
		srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
	}
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1003, nil }
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		copied := *order
		if order.OrderID == 1002 {
			if r.failModify != nil {
				return r.failModify
			}
			r.modified = append(r.modified, &copied)
			r.sends = append(r.sends, fmt.Sprintf("modify 1002 to %d", order.TotalQty))
			if !r.silent {
				go r.answerLater("Submitted", copied)
			}
			return nil
		}
		r.placed = append(r.placed, &copied)
		r.sends = append(r.sends, fmt.Sprintf("place %d", order.OrderID))
		return nil
	}
	srv.orderCancelBroker = func(_ context.Context, orderID int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.failCank != nil {
			return r.failCank
		}
		r.cancelled = append(r.cancelled, orderID)
		r.sends = append(r.sends, fmt.Sprintf("cancel %d", orderID))
		if !r.silent {
			go r.answerLater("Cancelled", ibkrlib.RawOrder{OrderID: 1002, TotalQty: int(held)})
		}
		return nil
	}
	return r
}

// answerLater is the broker's answer to a change of order 1002, arriving
// after the daemon's send returned, through the daemon's own lifecycle
// handler: an openOrder with the order's terms and an orderStatus.
func (r *closeLinkRig) answerLater(status string, order ibkrlib.RawOrder) {
	time.Sleep(30 * time.Millisecond)
	contract := closeLinkContract(r.option)
	base := ibkrlib.OrderLifecycleEvent{OrderID: 1002, ClientID: 31, ClientIDPresent: true, Account: "DU1234567", OrderRef: "ord-prot",
		Symbol: contract.Symbol, SecType: contract.SecType, ConID: contract.ConID, Currency: contract.Currency,
		Expiry: contract.Expiry, Strike: contract.Strike, Right: contract.Right, Multiplier: contract.Multiplier,
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TotalQuantity: float64(order.TotalQty), Status: status}
	if status != "Cancelled" {
		open := base
		open.Type = ibkrlib.OrderLifecycleEventOpenOrder
		open.TIF, open.AuxPrice, open.TrailStopPrice, open.OcaGroup, open.OcaType = order.TIF, order.AuxPrice, order.TrailStopPrice, order.OcaGroup, order.OcaType
		r.srv.appendOrderLifecycleEvent(open)
	}
	st := base
	st.Type, st.Remaining = ibkrlib.OrderLifecycleEventStatus, float64(order.TotalQty)
	r.srv.appendOrderLifecycleEvent(st)
}

// stopGone journals the stop as cancelled earlier and takes it out of the
// inventory, for a line with no stop of Canary's own.
func (r *closeLinkRig) stopGone(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.srv.orderJournal.Append(orderJournalEvent{At: r.now, Type: orderJournalEventStatusUpdated, OrderRef: "ord-prot", PreviewTokenID: "tok-prot",
		ReservedOrderID: 1002, ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Status: "Cancelled",
		Quantity: r.stopRemaining, Remaining: r.stopRemaining, SendState: orderSendStateTerminal}); err != nil {
		t.Fatalf("stop gone: %v", err)
	}
	r.stopRemaining = 0
}

func (r *closeLinkRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *closeLinkRig) set(position, stopRemaining float64) {
	r.mu.Lock()
	r.position, r.stopRemaining = position, stopRemaining
	r.mu.Unlock()
}

// setPlaced is what the inventory reports left of a placed order.
func (r *closeLinkRig) setPlaced(orderID int, remaining float64) {
	r.mu.Lock()
	r.placedRemaining[orderID] = remaining
	r.mu.Unlock()
}

// guardPass runs the guard twice, settle apart, the way two cadence ticks do.
func (r *closeLinkRig) guardPass(t *testing.T) {
	t.Helper()
	r.srv.runProtectiveStopGuard(t.Context())
	r.mu.Lock()
	r.now = r.now.Add(protectiveStopGuardSettle + time.Second)
	r.mu.Unlock()
	r.srv.runProtectiveStopGuard(t.Context())
}

// previewSell previews a limit sale of qty units of the line.
func (r *closeLinkRig) previewSell(t *testing.T, qty int) string {
	t.Helper()
	limit := 100.0
	if r.option {
		limit = 2.5
	}
	res, err := r.srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Quantity: qty, LimitPrice: &limit, Contract: closeLinkContract(r.option)})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	return res.PreviewToken
}

func (r *closeLinkRig) place(t *testing.T, token string) (*rpc.OrderPlaceResult, error) {
	t.Helper()
	return r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
}

func (r *closeLinkRig) sent() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sends...)
}

func (r *closeLinkRig) stopView(t *testing.T) rpc.OrderView {
	t.Helper()
	views, _, err := r.srv.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	for _, v := range views {
		if v.OrderRef == "ord-prot" {
			return v
		}
	}
	t.Fatal("the stop's journal row is gone")
	return rpc.OrderView{}
}

func decisionEvents(t *testing.T, srv *Server, event string) []decisionEvent {
	t.Helper()
	var out []decisionEvent
	for _, line := range readDecisionLines(t, srv.decisions.path) {
		if line.Event == event {
			out = append(out, line)
		}
	}
	return out
}

// ocaGroupModel is the broker's one-cancels-all group of type 2 (reduce the
// others with block) as IBKR describes it, for showing what the broker does
// after a member fills. IBKR reduces the other members when one fills in
// whole or in part; with "block" one member is routed at a time, so two can
// never fill at once. The model reduces by the filled amount; IBKR's own
// wording is "proportionately", and under either reading no member can
// sell what another has sold (protective-stop-guard.md).
type ocaGroupModel struct {
	remaining map[int]float64
}

func newOCAGroupModel(orders ...*ibkrlib.RawOrder) (*ocaGroupModel, error) {
	m := &ocaGroupModel{remaining: map[int]float64{}}
	group := ""
	for _, o := range orders {
		if o.OcaGroup == "" || o.OcaType != ibkrlib.OcaTypeReduceWithBlock || (group != "" && o.OcaGroup != group) {
			return nil, fmt.Errorf("order %d is not in the one group with type 2: group %q type %d", o.OrderID, o.OcaGroup, o.OcaType)
		}
		group = o.OcaGroup
		m.remaining[o.OrderID] = float64(o.TotalQty)
	}
	return m, nil
}

// fill executes qty of orderID and reduces every other member by it.
func (m *ocaGroupModel) fill(orderID int, qty float64) {
	m.remaining[orderID] = max(0, m.remaining[orderID]-qty)
	for id := range m.remaining {
		if id != orderID {
			m.remaining[id] = max(0, m.remaining[id]-qty)
		}
	}
}

// A full close sent while Canary's own full-position stop works joins the
// stop's one-cancels-all group, so the first fill reduces the other at the
// broker: the close fills first and the stop is gone; the stop fills first
// and the close is gone; a partial fill reduces the other by the amount.
// Nothing is modified or cancelled by Canary, and the guard, as the second
// line, finds nothing to do afterwards.
func TestCloseJoinsCanarysOwnStopOCAGroup(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		closeQty  int
		fillOrder int
		fillQty   float64
		wantStop  float64
		wantClose float64
	}{
		{name: "close fills first", closeQty: 70, fillOrder: 1003, fillQty: 70, wantStop: 0, wantClose: 0},
		{name: "stop fills first", closeQty: 70, fillOrder: 1002, fillQty: 70, wantStop: 0, wantClose: 0},
		{name: "a partial close fill reduces the stop", closeQty: 70, fillOrder: 1003, fillQty: 30, wantStop: 40, wantClose: 40},
		{name: "a partial reduction fills beside the stop", closeQty: 30, fillOrder: 1003, fillQty: 30, wantStop: 40, wantClose: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCloseLinkRig(t, false, "canary-oca-ord-prot")
			res, err := r.place(t, r.previewSell(t, tc.closeQty))
			if err != nil || !res.Accepted {
				t.Fatalf("place = %+v %v, want the sale of %d of 70 held accepted", res, err, tc.closeQty)
			}
			if got := r.sent(); len(got) != 1 || got[0] != "place 1003" {
				t.Fatalf("sends = %v, want the close alone: the stop is linked, not changed", got)
			}
			close := r.placed[0]
			if close.OcaGroup != "canary-oca-ord-prot" || close.OcaType != ibkrlib.OcaTypeReduceWithBlock {
				t.Fatalf("close OCA = %q/%d, want the stop's group with reduce-with-block (type 2)", close.OcaGroup, close.OcaType)
			}
			if res.OCAGroup != close.OcaGroup || res.OCAType != close.OcaType || res.Draft.OCAGroup != "" {
				t.Fatalf("result OCA = %q/%d draft OCA %q, want the membership sent reported beside the reviewed draft", res.OCAGroup, res.OCAType, res.Draft.OCAGroup)
			}
			if ev, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventSendAttempted); !ok || ev.OCAGroup != close.OcaGroup || ev.OCAType != close.OcaType {
				t.Fatalf("journal send-attempted = %+v (found %v), want the membership journaled", ev, ok)
			}
			if links := decisionEvents(t, r.srv, "protective_oca.link"); len(links) != 1 || links[0].Outcome != decisionSent || links[0].IDs.OrderRef != res.OrderRef {
				t.Fatalf("link decisions = %+v, want one sent link for the close", links)
			}
			stop := &ibkrlib.RawOrder{OrderID: 1002, TotalQty: 70, OcaGroup: r.stopGroup, OcaType: r.stopType}
			group, err := newOCAGroupModel(stop, close)
			if err != nil {
				t.Fatal(err)
			}
			group.fill(tc.fillOrder, tc.fillQty)
			if group.remaining[1002] != tc.wantStop || group.remaining[1003] != tc.wantClose {
				t.Fatalf("after the fill the group holds stop %.4g close %.4g, want %.4g and %.4g", group.remaining[1002], group.remaining[1003], tc.wantStop, tc.wantClose)
			}
			r.set(70-tc.fillQty, group.remaining[1002])
			r.setPlaced(1003, group.remaining[1003])
			r.guardPass(t)
			if got := r.sent(); len(got) != 1 {
				t.Fatalf("sends after the fill = %v, want nothing more: the broker reduced the other order", got)
			}
		})
	}
}

// A stop placed before stops had a group cannot be joined, and the IB API
// gives no way to put a working order into a group, so the stop is shrunk
// to what the close leaves (cancelled at nothing) through the gated modify
// and cancel path, the broker's answer is awaited, and only then is the
// close sent, plain.
func TestCloseShrinksAnUnlinkedStopBeforeItIsSent(t *testing.T) {
	t.Parallel()
	t.Run("a full close cancels the stop first", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, false, "")
		res, err := r.place(t, r.previewSell(t, 70))
		if err != nil || !res.Accepted {
			t.Fatalf("place = %+v %v, want the full close accepted", res, err)
		}
		if got := r.sent(); strings.Join(got, ", ") != "cancel 1002, place 1003" {
			t.Fatalf("sends = %v, want the stop cancelled, confirmed, then the close", got)
		}
		if close := r.placed[0]; close.OcaGroup != "" || close.OcaType != 0 {
			t.Fatalf("close OCA = %q/%d, want none: nothing is left to link", close.OcaGroup, close.OcaType)
		}
		if view := r.stopView(t); view.Open {
			t.Fatalf("stop view = %+v, want it closed by the confirmed cancel", view)
		}
		ev, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventCancelRequested)
		if !ok || ev.Origin != rpc.OrderOriginDaemonProtectiveGuard {
			t.Fatalf("journal cancel = %+v (found %v), want the guard origin", ev, ok)
		}
		if cancels := decisionEvents(t, r.srv, "protective_oca.cancel"); len(cancels) != 1 || cancels[0].Outcome != decisionSent || cancels[0].IDs.OrderRef != "ord-prot" {
			t.Fatalf("cancel decisions = %+v, want one sent cancel of the stop", cancels)
		}
	})
	t.Run("a reduction shrinks the stop to what it leaves", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, false, "")
		res, err := r.place(t, r.previewSell(t, 30))
		if err != nil || !res.Accepted {
			t.Fatalf("place = %+v %v, want the sale of 30 of 70 accepted", res, err)
		}
		if got := r.sent(); strings.Join(got, ", ") != "modify 1002 to 40, place 1003" {
			t.Fatalf("sends = %v, want the stop shrunk to 40, confirmed, then the sale", got)
		}
		sent := r.modified[0]
		if sent.OrderType != rpc.OrderTypeTRAIL || sent.TIF != rpc.OrderTIFGTC || sent.AuxPrice != 8.5 || sent.TrailStopPrice != 90 {
			t.Fatalf("modify = %+v, want the stop's own trail kept", sent)
		}
		if view := r.stopView(t); !view.Open || view.Quantity != 40 {
			t.Fatalf("stop view = %+v, want it working at 40", view)
		}
		if shrinks := decisionEvents(t, r.srv, "protective_oca.shrink"); len(shrinks) != 1 || shrinks[0].Outcome != decisionSent {
			t.Fatalf("shrink decisions = %+v, want one sent shrink", shrinks)
		}
	})
	t.Run("a sale that fits beside the stop needs no change", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, false, "")
		r.set(100, 70)
		res, err := r.place(t, r.previewSell(t, 30))
		if err != nil || !res.Accepted {
			t.Fatalf("place = %+v %v, want the sale of 30 beside a stop for 70 of 100 accepted", res, err)
		}
		if got := r.sent(); strings.Join(got, ", ") != "place 1003" {
			t.Fatalf("sends = %v, want the sale alone: 70 and 30 together sell no more than the 100 held", got)
		}
	})
}

// When the stop cannot be changed the close is refused with the reason and
// the stop stays as it was; an answer the broker never gives, or a change
// whose send is uncertain, refuses the close the same way. An unlinked close
// is never sent beside Canary's own stop.
func TestCloseIsRefusedWhenTheStopCannotBeChanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		setup    func(*closeLinkRig)
		wantErr  string
		wantOpen bool
	}{
		{name: "the cancel is refused by the broker", setup: func(r *closeLinkRig) {
			r.failCank = ibkrlib.WithSendDisposition(errors.New("gateway refused the frame"), ibkrlib.SendDispositionDefinitelyUnsent)
		}, wantErr: "could not be cancelled", wantOpen: true},
		{name: "the cancel's send is uncertain", setup: func(r *closeLinkRig) {
			r.failCank = ibkrlib.WithSendDisposition(errors.New("socket closed mid-frame"), ibkrlib.SendDispositionMayHaveWritten)
		}, wantErr: "may or may not have reached the broker", wantOpen: true},
		{name: "the broker never answers the cancel", setup: func(r *closeLinkRig) { r.silent = true }, wantErr: "has not confirmed", wantOpen: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCloseLinkRig(t, false, "")
			r.srv.protectiveOCAConfirmWaitForTest = 150 * time.Millisecond
			tc.setup(r)
			token := r.previewSell(t, 70)
			_, err := r.place(t, token)
			if err == nil || !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "ord-prot") {
				t.Fatalf("place err = %v, want a refusal naming the stop and %q", err, tc.wantErr)
			}
			if len(r.placed) != 0 {
				t.Fatalf("the close reached the broker beside its own stop: %v", r.sent())
			}
			if view := r.stopView(t); view.Open != tc.wantOpen {
				t.Fatalf("stop view open = %v, want %v", view.Open, tc.wantOpen)
			}
			if refusals := decisionEvents(t, r.srv, "protective_oca.refuse"); len(refusals) != 1 || refusals[0].Outcome != decisionRefused {
				t.Fatalf("refusals = %+v, want one refused close", refusals)
			}
		})
	}
}

// With no stop of Canary's own on the line, the sale goes out plain; a stop
// placed by hand in TWS is the owner's: the sale is still plain, nothing is
// linked or changed, and the guard reports the hand stop once.
func TestCloseWithoutCanarysOwnStopIsSentPlain(t *testing.T) {
	t.Parallel()
	t.Run("no stop at all", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, false, "canary-oca-ord-prot")
		r.stopGone(t)
		res, err := r.place(t, r.previewSell(t, 70))
		if err != nil || !res.Accepted {
			t.Fatalf("place = %+v %v, want the sale accepted", res, err)
		}
		if got := r.sent(); strings.Join(got, ", ") != "place 1003" || r.placed[0].OcaGroup != "" {
			t.Fatalf("sends = %v OCA %q, want the plain sale alone", got, r.placed[0].OcaGroup)
		}
	})
	t.Run("a hand stop in TWS", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, false, "canary-oca-ord-prot")
		r.stopGone(t)
		r.mu.Lock()
		r.hand = []ibkrlib.OrderLifecycleEvent{{
			Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: 7777, ClientID: 0, ClientIDPresent: true, Account: "DU1234567",
			Symbol: "SYW", SecType: "STK", ConID: closeLinkStockConID, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
			TotalQuantity: 70, Remaining: 70, Status: "Submitted", OcaGroup: "owner-group", OcaType: 1,
		}}
		r.mu.Unlock()
		res, err := r.place(t, r.previewSell(t, 70))
		if err != nil || !res.Accepted {
			t.Fatalf("place = %+v %v, want the sale accepted beside a hand stop", res, err)
		}
		if got := r.sent(); strings.Join(got, ", ") != "place 1003" || r.placed[0].OcaGroup != "" {
			t.Fatalf("sends = %v OCA %q, want the plain sale alone: the hand stop is not Canary's to link or change", got, r.placed[0].OcaGroup)
		}
		r.guardPass(t)
		if got := r.sent(); len(got) != 1 {
			t.Fatalf("sends after the guard = %v, want nothing: a hand stop is reported, never changed", got)
		}
		if reports := decisionEvents(t, r.srv, "protective_stop_guard.report"); len(reports) != 1 {
			t.Fatalf("reports = %+v, want the hand stop reported once", reports)
		}
	})
}

// A Canary-placed option profit-lock trail beside an option close is linked
// by the same rule.
func TestOptionCloseJoinsCanarysProfitTrail(t *testing.T) {
	t.Parallel()
	r := newCloseLinkRig(t, true, "canary-oca-ord-prot")
	res, err := r.place(t, r.previewSell(t, 5))
	if err != nil || !res.Accepted {
		t.Fatalf("place = %+v %v, want the close of 5 held calls accepted", res, err)
	}
	if got := r.sent(); strings.Join(got, ", ") != "place 1003" {
		t.Fatalf("sends = %v, want the close alone", got)
	}
	if close := r.placed[0]; close.OcaGroup != "canary-oca-ord-prot" || close.OcaType != ibkrlib.OcaTypeReduceWithBlock {
		t.Fatalf("option close OCA = %q/%d, want the trail's group with type 2", close.OcaGroup, close.OcaType)
	}
}

// A stop Canary places is born in its own one-cancels-all group, named
// after its order reference, so a later sale can join it by name.
func TestCanaryStopIsBornInItsOwnOCAGroup(t *testing.T) {
	t.Parallel()
	r := newCloseLinkRig(t, false, "canary-oca-ord-prot")
	r.stopGone(t)
	pct := 5.0
	res, err := r.srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Quantity: 70, OrderType: rpc.OrderTypeTRAIL,
		Trail: &rpc.OrderTrailSpec{OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &pct}, Strategy: rpc.OrderStrategyBrokerTrail,
		TIF: rpc.OrderTIFGTC, Contract: closeLinkContract(false)})
	if err != nil {
		t.Fatalf("preview trail: %v", err)
	}
	placed, err := r.place(t, res.PreviewToken)
	if err != nil || !placed.Accepted {
		t.Fatalf("place = %+v %v, want the stop accepted", placed, err)
	}
	stop := r.placed[0]
	if stop.OcaGroup != "canary-oca-"+placed.OrderRef || stop.OcaType != ibkrlib.OcaTypeReduceWithBlock {
		t.Fatalf("stop OCA = %q/%d, want its own group canary-oca-%s with type 2", stop.OcaGroup, stop.OcaType, placed.OrderRef)
	}
}

// The guard is the second line: a Canary sale working beside the stop
// without a link commits part of the position, so the stop is shrunk to the
// rest; a linked sale commits nothing, because the broker reduces the stop
// itself.
func TestProtectiveStopGuardCountsAnUnlinkedCanarySale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		group    string
		wantSend string
	}{
		{name: "unlinked sale shrinks the stop", group: "", wantSend: "modify 1002 to 40"},
		{name: "linked sale leaves the stop", group: "canary-oca-ord-prot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newCloseLinkRig(t, false, "canary-oca-ord-prot")
			sale := orderJournalEvent{At: r.now, Type: orderJournalEventBrokerAcknowledged, OrderRef: "ord-sale", PreviewTokenID: "tok-sale",
				ReservedOrderID: 1003, ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: "SYW", SecType: "STK",
				ConID: closeLinkStockConID, Exchange: "SMART", Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
				Quantity: 30, Remaining: 30, LimitPrice: 101, Status: "Submitted", SendState: orderSendStateBrokerAcknowledged, OCAGroup: tc.group}
			if tc.group != "" {
				sale.OCAType = ibkrlib.OcaTypeReduceWithBlock
			}
			if err := r.srv.orderJournal.Append(sale); err != nil {
				t.Fatal(err)
			}
			r.mu.Lock()
			r.hand = []ibkrlib.OrderLifecycleEvent{{
				Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1003, ClientID: 31, ClientIDPresent: true, Account: "DU1234567",
				Symbol: "SYW", SecType: "STK", ConID: closeLinkStockConID, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT,
				TotalQuantity: 30, Remaining: 30, Status: "Submitted", OcaGroup: tc.group, OcaType: sale.OCAType,
			}}
			r.mu.Unlock()
			r.guardPass(t)
			got := strings.Join(r.sent(), ", ")
			if got != tc.wantSend {
				t.Fatalf("sends = %q, want %q", got, tc.wantSend)
			}
		})
	}
}

// The guard covers a Canary-placed option trail too: fewer contracts held
// than the trail sells shrinks it; none cancels it.
func TestProtectiveStopGuardKeepsAnOptionTrailToThePosition(t *testing.T) {
	t.Parallel()
	t.Run("shrink", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, true, "canary-oca-ord-prot")
		r.set(3, 5)
		r.guardPass(t)
		if got := strings.Join(r.sent(), ", "); got != "modify 1002 to 3" {
			t.Fatalf("sends = %q, want the trail shrunk to the 3 contracts held", got)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		r := newCloseLinkRig(t, true, "canary-oca-ord-prot")
		r.set(0, 5)
		r.guardPass(t)
		if got := strings.Join(r.sent(), ", "); got != "cancel 1002" {
			t.Fatalf("sends = %q, want the trail cancelled at a flat position", got)
		}
	})
}
