package twssim

import "fmt"

// AddOutsideOrder lists a working limit order placed outside the client,
// as a hand order in TWS is (client 0): every all-client open-order read
// reports it, and it never fills by itself.
func (s *Server) AddOutsideOrder(conID int, action string, quantity, limit float64) (int, error) {
	in, ok := s.book.instrument(conID)
	if !ok {
		return 0, fmt.Errorf("twssim: contract %d is not listed", conID)
	}
	s.mu.Lock()
	s.outsideID++
	id := 900000 + s.outsideID
	s.mu.Unlock()
	s.store(PlaceOrder{OrderID: id, ClientID: 0, ConID: in.ConID, Symbol: in.Symbol, SecType: in.SecType, Exchange: in.Exchange, Currency: in.Currency,
		Action: action, Quantity: quantity, OrderType: "LMT", LimitPrice: limit, TIF: "DAY"}, "Submitted")
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
