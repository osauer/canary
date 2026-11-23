package twssim

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// The order engine. A transmitted order is acknowledged (openOrder and
// orderStatus to the client that placed it) and, while its market's session
// is open, a marketable limit fills whole at the touch. While the session is
// closed a DAY order rests PreSubmitted and becomes Submitted, and fillable,
// when it opens (assumed). An order sent with transmit false is held at the
// broker untransmitted and answered with nothing until a child with transmit
// true transmits the pair (assumed). A child (ParentID) works only once its
// parent has filled. A Fault armed for the next order replaces the normal
// answer.

// Market sessions a [Server] can be told about.
const (
	MarketUSEquity  = "us_equity"
	MarketUSOptions = "us_options"
	MarketDEXetra   = "de_xetra"
	MarketUKLSE     = "uk_lse"
	MarketHKHKEX    = "hk_hkex"
)

// marketOf is the exchange session an instrument trades in.
func marketOf(in Instrument) string {
	switch {
	case in.SecType == "OPT":
		return MarketUSOptions
	case in.Currency == "EUR":
		return MarketDEXetra
	case in.Currency == "GBP":
		return MarketUKLSE
	case in.Currency == "HKD":
		return MarketHKHKEX
	default:
		return MarketUSEquity
	}
}

// Fault kinds: what the broker does with the next order instead of the
// normal answer. Every one is assumed broker behaviour unless its doc says
// otherwise.
const (
	// FaultReject answers the order with Code and Message (default 201
	// "Order rejected - reason:...") and an Inactive status.
	FaultReject = "reject"
	// FaultRejectChild rejects the next child order (ParentID) and leaves
	// its parent held untransmitted at the broker.
	FaultRejectChild = "reject-child"
	// FaultRejectAfterAck acknowledges the order Submitted, then rejects it
	// with Code (default 201) and an Inactive status.
	FaultRejectAfterAck = "reject-after-ack"
	// FaultPreSubmitted acknowledges the order PreSubmitted and holds it.
	FaultPreSubmitted = "presubmitted"
	// FaultHold acknowledges the order Submitted and never fills it.
	FaultHold = "hold"
	// FaultPartial acknowledges the order and fills Quantity of it.
	FaultPartial = "partial"
	// FaultSilent records the order as working and answers nothing.
	FaultSilent = "silent"
	// FaultDrop closes the client's connection on receipt, before any answer.
	FaultDrop = "drop"
	// FaultWhatIfReject answers the next WhatIf with Code (default 201) and
	// Message, as a margin rejection.
	FaultWhatIfReject = "whatif-reject"
	// FaultWhatIfNoFee answers the next WhatIf without commission figures.
	FaultWhatIfNoFee = "whatif-no-fee"
	// FaultError answers with Code and Message only (110 price increment,
	// 200 no security definition, 103 duplicate id, 354 or 10089 market
	// data, ...) and keeps no order.
	FaultError = "error"
)

// Fault is one scripted broker answer to the next transmitted order (or the
// next child order, for FaultRejectChild).
type Fault struct {
	Kind     string  `json:"kind"`
	Code     int     `json:"code,omitempty"`
	Message  string  `json:"message,omitempty"`
	Quantity float64 `json:"quantity,omitempty"`
}

// ArmFault scripts the broker's answer to the next transmitted order.
// Faults are used in the order armed.
func (s *Server) ArmFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f)
}

func (s *Server) takeFault(o PlaceOrder) (Fault, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	whatIf := func(kind string) bool { return kind == FaultWhatIfReject || kind == FaultWhatIfNoFee }
	for i, f := range s.faults {
		if whatIf(f.Kind) != o.WhatIf || (!o.WhatIf && (f.Kind == FaultRejectChild) != (o.ParentID > 0)) {
			continue
		}
		s.faults = slices.Delete(s.faults, i, i+1)
		return f, true
	}
	return Fault{}, false
}

// SetSession sets a market's session: open (true) or closed. Opening it
// releases the orders resting PreSubmitted in it and fills the marketable
// ones.
func (s *Server) SetSession(market string, open bool) {
	s.mu.Lock()
	s.closed[market] = !open
	s.mu.Unlock()
	if open {
		s.releaseResting(market)
	}
}

func (s *Server) sessionOpen(market string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed[market]
}

// sessionFor is the connected session of a client id.
func (s *Server) sessionFor(clientID int) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range s.sessions {
		if ss.clientID == clientID {
			return ss
		}
	}
	return nil
}

func (s *Server) tell(clientID int, frames ...[]byte) {
	if ss := s.sessionFor(clientID); ss != nil {
		ss.send(frames...)
	}
}

func (ss *session) place(o PlaceOrder) {
	s, b := ss.srv, ss.srv.book
	in, ok := b.instrument(o.ConID)
	if o.SecType == "BAG" {
		b.mu.Lock()
		bag, err := b.bagInstrumentLocked(o)
		b.mu.Unlock()
		if err != nil {
			ss.fail(o.OrderID, 200, noSecurityDefinition)
			return
		}
		in, ok = bag, true
		if s.rejectLegOpenClose {
			for _, leg := range o.Legs {
				if leg.OpenClose == 1 || leg.OpenClose == 2 {
					ss.fail(o.OrderID, 10123, "twssim: the account does not accept an open/close instruction on a combination leg")
					return
				}
			}
		}
	}
	if !ok {
		ss.fail(o.OrderID, 200, noSecurityDefinition)
		return
	}
	if o.AttachedOrders && s.rejectAttached {
		ss.fail(o.OrderID, 10354, "twssim: attached orders are not accepted by this TWS")
		return
	}
	if o.Account != "" && o.Account != b.Account() {
		ss.fail(o.OrderID, 321, "Error validating request.-'bN' : cause - The account code is invalid")
		return
	}
	s.mu.Lock()
	s.nextID = max(s.nextID, o.OrderID+1)
	s.mu.Unlock()
	// A stock order off its listing's board lot is rejected, on WhatIf and on
	// place, with a reason naming the lot (assumed: IBKR's wording and code
	// for a Hong Kong odd lot are not captured).
	if lot := in.lot(); in.SecType == "STK" && lot > 1 && math.Mod(o.Quantity, lot) != 0 {
		ss.fail(o.OrderID, 201, fmt.Sprintf("Order rejected - reason:The order size %g is not a multiple of the board lot of %g shares for %s.", o.Quantity, lot, in.Symbol))
		return
	}
	if o.WhatIf {
		if s.whatIfDelay > 0 {
			time.Sleep(s.whatIfDelay)
		}
		s.mu.Lock()
		hook := s.beforeWhatIf
		s.beforeWhatIf = nil
		s.mu.Unlock()
		if hook != nil {
			hook()
		}
		if f, ok := s.takeFault(o); ok && f.Kind == FaultWhatIfReject {
			ss.fail(o.OrderID, codeOr(f.Code, 201), nonEmpty(f.Message, "Order rejected - reason:YOUR ORDER IS NOT ACCEPTED. IN ORDER TO OBTAIN THE DESIRED POSITION YOUR EQUITY WITH LOAN VALUE MUST EXCEED THE INITIAL MARGIN"))
			return
		} else if ok {
			b.mu.Lock()
			w := b.whatIfLocked(o, in)
			b.mu.Unlock()
			w.noFee = true
			order := Order{PlaceOrder: o, Status: "PreSubmitted"}
			order.ClientID = ss.clientID
			ss.send(openOrderFrame(order, in, b.Account(), &w))
			return
		}
		b.mu.Lock()
		w := b.whatIfLocked(o, in)
		if o.SecType == "BAG" {
			w = b.bagWhatIfLocked(o, in)
		}
		b.mu.Unlock()
		order := Order{PlaceOrder: o, Status: "PreSubmitted"}
		order.ClientID = ss.clientID
		ss.send(openOrderFrame(order, in, b.Account(), &w))
		return
	}
	o.ClientID = ss.clientID
	if !o.Transmit {
		// Held at the broker until a child transmits it; nothing is said.
		s.store(o, "Untransmitted")
		return
	}
	fault, faulted := s.takeFault(o)
	// A child transmits its untransmitted parent first. A child the broker
	// rejects leaves the parent held untransmitted; a parent the broker
	// rejects takes its child with it (both assumed).
	if o.ParentID > 0 {
		if parent, ok := s.order(o.ParentID); ok && parent.Status == "Untransmitted" {
			if faulted && fault.Kind == FaultRejectChild {
				s.transmit(o, fault, true)
				return
			}
			pf, pfaulted := s.takeFault(parent.PlaceOrder)
			if !s.transmit(parent.PlaceOrder, pf, pfaulted) {
				child := s.store(o, "Cancelled")
				ss.send(openOrderFrame(child, in, b.Account(), nil), orderStatusFrame(child, 0))
				ss.fail(o.OrderID, 202, "Order Canceled - reason:parent order rejected")
				return
			}
		}
	}
	s.transmit(o, fault, faulted)
}

func codeOr(code, fallback int) int {
	if code == 0 {
		return fallback
	}
	return code
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// store records an order at the broker with a status and returns it.
func (s *Server) store(o PlaceOrder, status string) Order {
	b := s.book
	b.mu.Lock()
	defer b.mu.Unlock()
	prior := b.orders[o.OrderID]
	order := &Order{PlaceOrder: o, Status: status, Placed: s.now()}
	if prior != nil {
		order.PermID, order.Filled, order.AvgPrice = prior.PermID, prior.Filled, prior.AvgPrice
	} else {
		b.permID++
		order.PermID = b.permID
	}
	b.orders[o.OrderID] = order
	return *order
}

func (s *Server) order(id int) (Order, bool) {
	b := s.book
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.orders[id]
	if !ok {
		return Order{}, false
	}
	return *o, true
}

// transmit works a transmitted order: acknowledge, then fill what the
// session and the fault allow.
func (s *Server) transmit(o PlaceOrder, fault Fault, faulted bool) bool {
	b := s.book
	in := s.instrumentOf(o)
	ss := s.sessionFor(o.ClientID)
	if faulted && ss != nil {
		switch fault.Kind {
		case FaultDrop:
			_ = ss.c.Close()
			return false
		case FaultError:
			ss.fail(o.OrderID, fault.Code, nonEmpty(fault.Message, fmt.Sprintf("twssim: error %d", fault.Code)))
			return false
		case FaultRejectChild, FaultReject:
			order := s.store(o, "Inactive")
			ss.fail(o.OrderID, codeOr(fault.Code, 201), nonEmpty(fault.Message, "Order rejected - reason:twssim rejects this order"))
			ss.send(openOrderFrame(order, in, b.Account(), nil), orderStatusFrame(order, 0))
			return false
		case FaultSilent:
			s.store(o, "Submitted")
			return true
		}
	}
	status := "Submitted"
	parentWorking := false
	if o.ParentID > 0 {
		if parent, ok := s.order(o.ParentID); ok && parent.Status != "Filled" {
			parentWorking = true
		}
	}
	switch {
	case parentWorking, !s.sessionOpen(marketOf(in)):
		status = "PreSubmitted"
	case faulted && fault.Kind == FaultPreSubmitted:
		status = "PreSubmitted"
	}
	order := s.store(o, status)
	s.tell(o.ClientID, openOrderFrame(order, in, b.Account(), nil), orderStatusFrame(order, 0))
	if faulted {
		switch fault.Kind {
		case FaultRejectAfterAck:
			s.setStatus(o.OrderID, "Inactive")
			order, _ = s.order(o.OrderID)
			if ss != nil {
				ss.fail(o.OrderID, codeOr(fault.Code, 201), nonEmpty(fault.Message, "Order rejected - reason:twssim rejects this order after acknowledging it"))
				ss.send(orderStatusFrame(order, 0))
			}
			return false
		case FaultPreSubmitted, FaultHold:
			return true
		case FaultPartial:
			s.fill(o.OrderID, fault.Quantity, 0)
			return true
		}
	}
	if status == "Submitted" {
		s.fillIfMarketable(o.OrderID)
	}
	return true
}

func (s *Server) setStatus(id int, status string) {
	b := s.book
	b.mu.Lock()
	defer b.mu.Unlock()
	if o := b.orders[id]; o != nil {
		o.Status = status
	}
}

// fillIfMarketable fills a working limit whole at the touch when it crosses:
// a buy at or above the ask, a sell at or below the bid. Stops never fill.
func (s *Server) fillIfMarketable(id int) {
	o, ok := s.order(id)
	if !ok || o.Status != "Submitted" || o.OrderType != "LMT" {
		return
	}
	in := s.instrumentOf(o.PlaceOrder)
	q, _ := s.feedFor(in, quoteOf(in))
	if s.midFills(in.ConID) && q.bid > 0 && q.ask > q.bid {
		// A contract set to fill inside the spread fills a limit at or
		// through the midpoint at its limit.
		mid := (q.bid + q.ask) / 2
		switch {
		case o.Action == "SELL" && o.LimitPrice <= mid+1e-9:
			s.fill(id, 0, o.LimitPrice)
			return
		case o.Action == "BUY" && o.LimitPrice+1e-9 >= mid:
			s.fill(id, 0, o.LimitPrice)
			return
		}
	}
	switch {
	case o.Action == "BUY" && q.ask > 0 && o.LimitPrice >= q.ask:
		s.fill(id, 0, q.ask)
	case o.Action == "SELL" && q.bid > 0 && o.LimitPrice <= q.bid:
		s.fill(id, 0, q.bid)
	}
}

// Fill fills shares of a working order (all that remains when shares is
// zero) at price (the touch when zero), as the exchange would, and reports
// the execution to the client that placed it.
func (s *Server) Fill(orderID int, shares, price float64) error {
	o, ok := s.order(orderID)
	if !ok || !o.Working() {
		return fmt.Errorf("twssim: order %d is not working", orderID)
	}
	s.fill(orderID, shares, price)
	return nil
}

func (s *Server) fill(id int, shares, price float64) {
	b := s.book
	b.mu.Lock()
	order := b.orders[id]
	if order == nil {
		b.mu.Unlock()
		return
	}
	if order.SecType == "BAG" {
		in, _ := b.bagInstrumentLocked(order.PlaceOrder)
		execs := b.fillBagLocked(order, shares)
		filled := *order
		b.mu.Unlock()
		var frames [][]byte
		for _, ex := range execs {
			frames = append(frames, execFrame(-1, ex), commissionFrame(ex))
		}
		frames = append(frames, openOrderFrame(filled, in, b.Account(), nil), orderStatusFrame(filled, filled.AvgPrice))
		s.tell(filled.ClientID, frames...)
		s.pushAccount()
		return
	}
	in := *b.instr[order.ConID]
	if price <= 0 {
		q := quoteOf(in)
		price = q.ask
		if order.Action == "SELL" {
			price = q.bid
		}
		if price <= 0 {
			price = in.Price
		}
	}
	if order.Status == "PreSubmitted" {
		order.Status = "Submitted"
	}
	ex := b.fillLocked(order, in, shares, price, s.now())
	filled := *order
	var released []Order
	if filled.Status == "Filled" {
		for _, child := range b.orders {
			if child.ParentID == id && child.Status == "PreSubmitted" {
				child.Status = "Submitted"
				released = append(released, *child)
			}
		}
	}
	b.mu.Unlock()
	s.tell(filled.ClientID, execFrame(-1, *ex), openOrderFrame(filled, in, b.Account(), nil), orderStatusFrame(filled, ex.Price), commissionFrame(*ex))
	for _, child := range released {
		cin := s.instrumentOf(child.PlaceOrder)
		s.tell(child.ClientID, openOrderFrame(child, cin, b.Account(), nil), orderStatusFrame(child, 0))
	}
	s.pushAccount()
}

func (ss *session) cancel(orderID int) {
	o, ok := ss.srv.endOrder(orderID, "Cancelled")
	if !ok {
		ss.fail(orderID, 10147, fmt.Sprintf("OrderId %d that needs to be cancelled is not found.", orderID))
		return
	}
	ss.send(orderStatusFrame(o, 0))
	ss.fail(orderID, 202, "Order Canceled - reason:")
}

// endOrder ends a working or untransmitted order with a final status.
func (s *Server) endOrder(id int, status string) (Order, bool) {
	b := s.book
	b.mu.Lock()
	defer b.mu.Unlock()
	o := b.orders[id]
	if o == nil || (!o.Working() && o.Status != "Untransmitted") {
		return Order{}, false
	}
	o.Status = status
	return *o, true
}

// Child handling when a parent ends the day partly filled.
const (
	// ChildCancelled cancels the children with their parent.
	ChildCancelled = "cancel"
	// ChildResized activates the children for the quantity the parent filled.
	ChildResized = "resize"
)

// EndDay ends every working DAY order at the close, as the exchange does:
// each is reported Cancelled to the client that placed it. A child of a
// parent that filled in part is cancelled with it or activated for the
// filled quantity, by the server's ChildOnPartialExpiry option (assumed;
// IBKR does not document which).
func (s *Server) EndDay() {
	b := s.book
	for _, o := range b.Orders() {
		if o.TIF != "DAY" || !o.Working() {
			continue
		}
		ended, ok := s.endOrder(o.OrderID, "Cancelled")
		if !ok {
			continue
		}
		s.tell(ended.ClientID, orderStatusFrame(ended, 0))
		for _, child := range b.Orders() {
			if child.ParentID != o.OrderID || !child.Working() {
				continue
			}
			cin := s.instrumentOf(child.PlaceOrder)
			if ended.Filled > 0 && s.childOnExpiry == ChildResized {
				b.mu.Lock()
				c := b.orders[child.OrderID]
				c.Quantity, c.Status = ended.Filled, "Submitted"
				resized := *c
				b.mu.Unlock()
				s.tell(child.ClientID, openOrderFrame(resized, cin, b.Account(), nil), orderStatusFrame(resized, 0))
				continue
			}
			if cancelled, ok := s.endOrder(child.OrderID, "Cancelled"); ok {
				s.tell(cancelled.ClientID, orderStatusFrame(cancelled, 0))
			}
		}
	}
}

// releaseResting moves a market's PreSubmitted orders (not children waiting
// on a parent) to Submitted and fills the marketable ones.
func (s *Server) releaseResting(market string) {
	b := s.book
	for _, o := range b.Orders() {
		in := s.instrumentOf(o.PlaceOrder)
		if o.Status != "PreSubmitted" || marketOf(in) != market {
			continue
		}
		if parent, ok := s.order(o.ParentID); o.ParentID > 0 && (!ok || parent.Status != "Filled") {
			continue
		}
		s.setStatus(o.OrderID, "Submitted")
		now, _ := s.order(o.OrderID)
		s.tell(o.ClientID, orderStatusFrame(now, 0))
		s.fillIfMarketable(o.OrderID)
	}
}

// DropSessions ends every client connection, as a TWS restart does.
func (s *Server) DropSessions() {
	s.mu.Lock()
	sessions := slices.Clone(s.sessions)
	s.mu.Unlock()
	for _, ss := range sessions {
		_ = ss.c.Close()
	}
}

// PushAccount streams the account values and portfolio to every subscribed
// client, as TWS does when a value changes.
func (s *Server) PushAccount() { s.pushAccount() }

// instrumentOf is an order's contract: the listed contract, or a
// combination priced from its legs now.
func (s *Server) instrumentOf(o PlaceOrder) Instrument {
	b := s.book
	if o.SecType == "BAG" {
		b.mu.Lock()
		defer b.mu.Unlock()
		in, _ := b.bagInstrumentLocked(o)
		return in
	}
	in, _ := b.instrument(o.ConID)
	return in
}
