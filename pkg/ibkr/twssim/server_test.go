package twssim

import (
	"bufio"
	"encoding/binary"
	"net"
	"slices"
	"testing"
	"time"
)

// client is a bare TWS API client: framing only, no pkg/ibkr code.
type client struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dialClient(t *testing.T, s *Server, clientID int) *client {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	cl := &client{t: t, c: c, r: bufio.NewReader(c)}
	if _, err := c.Write(appendFrame([]byte("API\x00"), []byte("v100..203\x00"))); err != nil {
		t.Fatal(err)
	}
	if hello := cl.read(); string(hello[:4]) != "203\x00" {
		t.Fatalf("handshake answer %q", hello)
	}
	cl.send(textMessage(startAPI, "2", itoa(clientID), ""))
	return cl
}

func (cl *client) send(body []byte) {
	cl.t.Helper()
	if _, err := cl.c.Write(appendFrame(nil, body)); err != nil {
		cl.t.Fatal(err)
	}
}

func (cl *client) read() []byte {
	cl.t.Helper()
	body, err := readFrame(cl.r)
	if err != nil {
		cl.t.Fatal(err)
	}
	return body
}

// until reads frames until one with the wire id arrives and returns the
// wire ids seen on the way, that one included.
func (cl *client) until(wireID int) ([]int, []byte) {
	cl.t.Helper()
	var seen []int
	for {
		body := cl.read()
		id := int(binary.BigEndian.Uint32(body[:4]))
		seen = append(seen, id)
		if id == wireID {
			return seen, body[4:]
		}
	}
}

func buyLimit(orderID int, whatIf bool, extra func(order protoWriter) protoWriter) []byte {
	var contract protoWriter
	contract = contract.varint(1, 900001).str(2, "SYNA").str(3, "STK").str(8, "SMART").str(10, "USD")
	var order protoWriter
	order = order.varint(1, 7).str(5, "BUY").str(6, "10").str(8, "LMT").double(9, 50.10).str(11, "DAY").str(12, DefaultAccount).boolean(65, whatIf).boolean(66, true)
	if extra != nil {
		order = extra(order)
	}
	var body protoWriter
	body = body.varint(1, int64(orderID)).bytes(2, contract).bytes(3, order)
	return protoMessage(placeOrder, body)
}

// The order engine as a client sees it: a WhatIf answers with an openOrder
// and leaves nothing working; a marketable transmitted limit is acknowledged,
// fills at the ask, reports the execution and commission, and moves the
// book's position and cash.
func TestServerPreviewsThenFillsAMarketableBuy(t *testing.T) {
	s := Start(t, Options{})
	cl := dialClient(t, s, 7)
	cl.until(msgManagedAccounts)

	cl.send(buyLimit(1, true, nil))
	if _, body := cl.until(msgOpenOrder + protoOffset); len(body) == 0 {
		t.Fatal("empty WhatIf answer")
	}
	if orders := s.Book().Orders(); len(orders) != 0 {
		t.Fatalf("a WhatIf left orders: %+v", orders)
	}

	cash := s.Book().Cash("USD")
	cl.send(buyLimit(2, false, nil))
	seen, _ := cl.until(msgCommissionReport)
	for _, want := range []int{msgOpenOrder + protoOffset, msgOrderStatus + protoOffset, msgExecDetails + protoOffset} {
		if !slices.Contains(seen, want) {
			t.Fatalf("frames %v lack %d", seen, want)
		}
	}
	if p, ok := s.Book().Position(900001); !ok || p.Quantity != 110 {
		t.Fatalf("position after the fill: %+v", p)
	}
	ex := s.Book().Executions()
	if len(ex) != 1 || ex[0].Price != 50.01 || ex[0].Shares != 10 {
		t.Fatalf("executions: %+v", ex)
	}
	if got, want := s.Book().Cash("USD"), cash-10*50.01-ex[0].Fee; got != want {
		t.Fatalf("cash %v, want %v", got, want)
	}
	if o := s.Book().Orders(); len(o) != 1 || o[0].Status != "Filled" {
		t.Fatalf("orders: %+v", o)
	}
}

// A field the broker's Order message does not define, or one sent with the
// wrong wire type, is what the broker drops: the recorder names it.
func TestRecorderNamesFieldsTheBrokerDrops(t *testing.T) {
	s := Start(t, Options{})
	cl := dialClient(t, s, 7)
	cl.until(msgManagedAccounts)
	cl.send(buyLimit(3, true, func(o protoWriter) protoWriter {
		return o.varint(999, 1).str(9, "50.10") // unknown tag; lmtPrice as a string
	}))
	cl.until(msgOpenOrder + protoOffset)
	orders := s.Recorder().PlaceOrders()
	if len(orders) != 1 || !slices.Equal(orders[0].IgnoredByBroker, []string{"order.999", "order.9"}) {
		t.Fatalf("ignored fields: %+v", orders)
	}
}

// A second session under a client id in use is turned away with error 326.
func TestServerRefusesAClientIDInUse(t *testing.T) {
	s := Start(t, Options{})
	first := dialClient(t, s, 15)
	first.until(msgManagedAccounts)
	second := dialClient(t, s, 15)
	_, body := second.until(msgError + protoOffset)
	fields, err := decodeProto(body)
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	for _, f := range fields {
		if f.num == 3 {
			code = int(f.u)
		}
	}
	if code != 326 {
		t.Fatalf("error code %d, want 326", code)
	}
}

// The simulator never listens off loopback or on an IBKR API port.
func TestListenRefusesLiveAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7496", "127.0.0.1:7497", "127.0.0.1:4001", "127.0.0.1:4002", "0.0.0.0:0", "192.0.2.1:0"} {
		if _, err := Listen(Options{Listen: addr}); err == nil {
			t.Errorf("listened on %s", addr)
		}
	}
}
