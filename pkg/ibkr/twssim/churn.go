package twssim

import "fmt"

// AddOutsideOrder lists a working limit order placed outside the client,
// as a hand order in TWS is (client 0): every all-client open-order read
// reports it, and it never fills by itself.
func (s *Server) AddOutsideOrder(conID int, action string, quantity, limit float64) (int, error) {
	return s.addOutside(conID, action, "LMT", quantity, limit, 0)
}

// AddOutsideStop lists a working sell stop placed outside the client, as a
// protective stop entered in TWS is.
func (s *Server) AddOutsideStop(conID int, quantity, stop float64) (int, error) {
	return s.addOutside(conID, "SELL", "STP", quantity, 0, stop)
}

func (s *Server) addOutside(conID int, action, orderType string, quantity, limit, aux float64) (int, error) {
	in, ok := s.book.instrument(conID)
	if !ok {
		return 0, fmt.Errorf("twssim: contract %d is not listed", conID)
	}
	s.mu.Lock()
	s.outsideID++
	id := 900000 + s.outsideID
	s.mu.Unlock()
	s.store(PlaceOrder{OrderID: id, ClientID: 0, ConID: in.ConID, Symbol: in.Symbol, SecType: in.SecType, Exchange: in.Exchange, Currency: in.Currency,
		Action: action, Quantity: quantity, OrderType: orderType, LimitPrice: limit, AuxPrice: aux, TIF: "GTC"}, "Submitted")
	return id, nil
}

// Churn re-sends openOrder and orderStatus for every working order to every
// connected client n times, as TWS does when orders are modified or the
// market moves them: a burst of order frames for the client's reader.
func (s *Server) Churn(n int) int {
	frames := 0
	for range n {
		var out [][]byte
		for _, o := range s.book.Orders() {
			if !o.Working() {
				continue
			}
			out = append(out, openOrderFrame(o, s.instrumentOf(o.PlaceOrder), s.book.Account(), nil), orderStatusFrame(o, 0))
		}
		for _, ss := range s.subscribedAll() {
			ss.send(out...)
		}
		frames += len(out)
	}
	return frames
}

func (s *Server) subscribedAll() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*session(nil), s.sessions...)
}

// BeforeNextWhatIfAnswer runs fn once, before the next WhatIf answer is
// sent: whatever it sends the client (an account update, say) arrives ahead
// of that answer on the same socket.
func (s *Server) BeforeNextWhatIfAnswer(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeWhatIf = fn
}

// AddOutsideTrail lists a working GTC sell trailing stop placed outside the
// client, as one entered in TWS is: trailing amount (in price units) below
// the highest price since it was placed, starting at stop.
func (s *Server) AddOutsideTrail(conID int, quantity, amount, stop float64) (int, error) {
	in, ok := s.book.instrument(conID)
	if !ok {
		return 0, fmt.Errorf("twssim: contract %d is not listed", conID)
	}
	s.mu.Lock()
	s.outsideID++
	id := 900000 + s.outsideID
	s.mu.Unlock()
	s.store(PlaceOrder{OrderID: id, ClientID: 0, ConID: in.ConID, Symbol: in.Symbol, SecType: in.SecType, Exchange: in.Exchange, Currency: in.Currency,
		Action: "SELL", Quantity: quantity, OrderType: "TRAIL", AuxPrice: amount, TrailStopPrice: stop, TIF: "GTC"}, "Submitted")
	return id, nil
}

// MovePrice moves a contract's last price, as SetPrice on the book does, and
// works its sell trailing stops as the exchange does: each stop rises to
// the price less its trailing amount (or percent) and never falls, and a
// price at or below the stop triggers it, filling whole at the bid. The
// broker reports the ratcheted stop as trailStopPrice in the next openOrder
// a client reads (reqOpenOrders, reqAllOpenOrders); it pushes no frame when
// only the stop moves (assumed: IBKR documents neither way).
func (s *Server) MovePrice(conID int, price float64) {
	b := s.book
	b.SetPrice(conID, price)
	var triggered []int
	b.mu.Lock()
	for id, o := range b.orders {
		if o.ConID != conID || o.Action != "SELL" || o.OrderType != "TRAIL" || o.Status != "Submitted" {
			continue
		}
		offset := o.AuxPrice
		if o.TrailingPercent > 0 {
			offset = price * o.TrailingPercent / 100
		}
		if stop := price - offset; stop > o.TrailStopPrice {
			o.TrailStopPrice = stop
		}
		if price <= o.TrailStopPrice {
			triggered = append(triggered, id)
		}
	}
	b.mu.Unlock()
	for _, id := range triggered {
		o, ok := s.order(id)
		if !ok {
			continue
		}
		q, _ := s.feedFor(s.instrumentOf(o.PlaceOrder), quoteOf(s.instrumentOf(o.PlaceOrder)))
		s.fill(id, 0, q.bid)
	}
}
