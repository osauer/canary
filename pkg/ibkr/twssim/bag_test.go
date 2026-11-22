package twssim

import (
	"slices"
	"testing"
	"time"
)

func bagOrder(orderID int, long, short int, limit float64, whatIf bool, legOpenClose int) []byte {
	var contract protoWriter
	contract = contract.varint(1, 0).str(2, "SYNB").str(3, "BAG").str(8, "SMART").str(10, "USD")
	for _, leg := range []struct {
		id     int
		action string
	}{{long, "BUY"}, {short, "SELL"}} {
		var l protoWriter
		l = l.varint(1, int64(leg.id)).varint(2, 1).str(3, leg.action).str(4, "SMART").varint(5, int64(legOpenClose)).varint(6, 0).varint(8, -1)
		contract = contract.bytes(20, l)
	}
	var order protoWriter
	order = order.varint(1, 7).str(5, "BUY").str(6, "1").str(8, "LMT").double(9, limit).str(11, "DAY").str(12, DefaultAccount).boolean(65, whatIf).boolean(66, true).str(68, "O")
	var body protoWriter
	body = body.varint(1, int64(orderID)).bytes(2, contract).bytes(3, order)
	return protoMessage(placeOrder, body)
}

// A bear put spread as one combination: the official table reads its legs,
// a WhatIf answers with margin and commission, and a limit at the net ask
// fills both legs under the BAG's order id, moving each position.
func TestServerWorksABagOrder(t *testing.T) {
	now := time.Now()
	book := OptionsBook(now)
	exps := OptionExpiries(now)
	long, _ := book.OptionConIDFor(900002, exps[3], 120, "P")
	short, _ := book.OptionConIDFor(900002, exps[3], 110, "P")
	s := Start(t, Options{Book: book})
	cl := dialClient(t, s, 7)
	cl.until(msgManagedAccounts)

	cl.send(bagOrder(1, long, short, 10, true, 0))
	cl.until(msgOpenOrder + protoOffset)
	recorded := s.Recorder().PlaceOrders()
	if len(recorded) != 1 || len(recorded[0].Legs) != 2 || len(recorded[0].IgnoredByBroker) != 0 {
		t.Fatalf("decoded BAG: %+v", recorded)
	}
	if l := recorded[0].Legs[1]; l.ConID != short || l.Action != "SELL" || l.Ratio != 1 || l.OpenClose != 0 || l.ExemptCode != -1 || !l.ExemptCodeSent {
		t.Fatalf("short leg: %+v", l)
	}

	cl.send(bagOrder(2, long, short, 10, false, 0))
	seen, _ := cl.until(msgOrderStatus + protoOffset)
	for slices.Index(seen, msgExecDetails+protoOffset) < 0 {
		seen, _ = cl.until(msgOrderStatus + protoOffset)
	}
	if p, _ := book.Position(long); p.Quantity != 16 {
		t.Fatalf("long leg position: %+v", p)
	}
	if p, _ := book.Position(short); p.Quantity != -16 {
		t.Fatalf("short leg position: %+v", p)
	}
	if ex := book.Executions(); len(ex) != 2 || ex[0].OrderID != 2 || ex[1].OrderID != 2 {
		t.Fatalf("executions: %+v", ex)
	}
}

// With RejectLegOpenClose, a leg carrying open (1) or close (2) is refused
// with 10123, as the old frame would have been on such an account.
func TestServerRefusesLegOpenClose(t *testing.T) {
	now := time.Now()
	book := OptionsBook(now)
	exps := OptionExpiries(now)
	long, _ := book.OptionConIDFor(900002, exps[3], 120, "P")
	short, _ := book.OptionConIDFor(900002, exps[3], 110, "P")
	s := Start(t, Options{Book: book, RejectLegOpenClose: true})
	cl := dialClient(t, s, 7)
	cl.send(bagOrder(1, long, short, 10, false, 1))
	for code := uint64(0); code != 10123; {
		_, body := cl.until(msgError + protoOffset)
		fields, _ := decodeProto(body)
		for _, f := range fields {
			if f.num == 3 {
				code = f.u
			}
		}
	}
	if len(book.Orders()) != 0 {
		t.Fatal("the broker kept the refused combination")
	}
}
