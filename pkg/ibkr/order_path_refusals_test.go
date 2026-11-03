package ibkr

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Order-path refusals: every entry that could write a broker instruction
// turns the request away before a byte reaches the socket. These tests run
// in the default (trading-disabled) build and assert, for each refusal, the
// definitely-unsent disposition, an untouched socket and no retained order
// identity. The WhatIf preview is an order-path read (WhatIf and Transmit
// are set before the frame is encoded); its refusals and its uncertainty
// outcome are covered here too.

func ordTEntryContract() *Contract {
	return &Contract{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}
}

func ordTEntry(gate PaperOrderGate) *RawOrder {
	return &RawOrder{OrderID: 101, ClientID: gate.ClientID, Account: gate.Account, OrderRef: "entry-1", Action: "BUY", OrderType: "LMT", LmtPrice: 80, TotalQty: 10, TIF: "DAY"}
}

func ordTAssertUnsent(t *testing.T, name string, err error, socket *safeBuffer) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: accepted", name)
	}
	if SendDispositionOf(err) != SendDispositionDefinitelyUnsent {
		t.Fatalf("%s: disposition %v for %v", name, SendDispositionOf(err), err)
	}
	if socket.Len() != 0 {
		t.Fatalf("%s: %d bytes reached the socket", name, socket.Len())
	}
}

func TestStockEntryStopRefusalsWriteNothing(t *testing.T) {
	conn, c, socket, _, gate := newQueuedInstructionReconnectFixture(t)
	session, ok := c.CaptureSession()
	if !ok {
		t.Fatal("capture session")
	}
	ctx := t.Context()
	contract := ordTEntryContract()

	// The unrestricted entry is build-tag guarded.
	err := c.SubmitStockEntryStopForSessionGuarded(ctx, session, contract, ordTEntry(gate), 102, "child-1", 76, nil)
	if tradingEnabled {
		t.Skip("default disabled guard is not active in trading-tag builds")
	}
	if !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("unrestricted entry: %v", err)
	}
	ordTAssertUnsent(t, "trading disabled", err, socket)

	// The paper entry has its own gate, then the shared refusals.
	badGate := gate
	badGate.Mode = "live"
	ordTAssertUnsent(t, "gate mode", c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, badGate, contract, ordTEntry(gate), 102, "child-1", 76, nil), socket)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for name, call := range map[string]func() error{
		"nil contract": func() error {
			return c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, nil, ordTEntry(gate), 102, "child-1", 76, nil)
		},
		"nil entry": func() error {
			return c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, nil, 102, "child-1", 76, nil)
		},
		"nil context": func() error {
			//lint:ignore SA1012 the nil-context refusal is the behaviour under test
			return c.SubmitPaperStockEntryStopForSessionGuarded(nil, session, gate, contract, ordTEntry(gate), 102, "child-1", 76, nil)
		},
		"cancelled context": func() error {
			return c.SubmitPaperStockEntryStopForSessionGuarded(cancelled, session, gate, contract, ordTEntry(gate), 102, "child-1", 76, nil)
		},
		"foreign binding": func() error {
			return c.SubmitPaperStockEntryStopForSessionGuarded(ctx, ConnectorSessionBinding{connector: c, connection: conn, epoch: session.epoch + 1}, gate, contract, ordTEntry(gate), 102, "child-1", 76, nil)
		},
		"account off the gate": func() error {
			entry := ordTEntry(gate)
			entry.Account = "DU6666666"
			return c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, entry, 102, "child-1", 76, nil)
		},
		"client off the gate": func() error {
			entry := ordTEntry(gate)
			entry.ClientID = gate.ClientID + 1
			return c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, entry, 102, "child-1", 76, nil)
		},
	} {
		ordTAssertUnsent(t, name, call(), socket)
	}

	// Invalid terms: each clause of the single admission predicate.
	terms := map[string]func(contract *Contract, entry *RawOrder) (childID int, childRef string, stop float64){
		"no contract id":  func(ct *Contract, e *RawOrder) (int, string, float64) { ct.ConID = 0; return 102, "child-1", 76 },
		"option contract": func(ct *Contract, e *RawOrder) (int, string, float64) { ct.SecType = "OPT"; return 102, "child-1", 76 },
		"combo contract": func(ct *Contract, e *RawOrder) (int, string, float64) {
			ct.ComboLegs = []ComboLeg{{ConID: 1}}
			return 102, "child-1", 76
		},
		"sell entry":    func(ct *Contract, e *RawOrder) (int, string, float64) { e.Action = "SELL"; return 102, "child-1", 76 },
		"market entry":  func(ct *Contract, e *RawOrder) (int, string, float64) { e.OrderType = "MKT"; return 102, "child-1", 76 },
		"gtc entry":     func(ct *Contract, e *RawOrder) (int, string, float64) { e.TIF = "GTC"; return 102, "child-1", 76 },
		"zero quantity": func(ct *Contract, e *RawOrder) (int, string, float64) { e.TotalQty = 0; return 102, "child-1", 76 },
		"oversized quantity": func(ct *Contract, e *RawOrder) (int, string, float64) {
			e.TotalQty = 1000001
			return 102, "child-1", 76
		},
		"no entry id":         func(ct *Contract, e *RawOrder) (int, string, float64) { e.OrderID = 0; return 102, "child-1", 76 },
		"child before parent": func(ct *Contract, e *RawOrder) (int, string, float64) { return 100, "child-1", 76 },
		"child equals parent": func(ct *Contract, e *RawOrder) (int, string, float64) { return 101, "child-1", 76 },
		"modify identity": func(ct *Contract, e *RawOrder) (int, string, float64) {
			e.Changes = &OrderChangeIdentity{PermID: 1}
			return 102, "child-1", 76
		},
		"oca group":             func(ct *Contract, e *RawOrder) (int, string, float64) { e.OcaGroup = "g"; return 102, "child-1", 76 },
		"oca type":              func(ct *Contract, e *RawOrder) (int, string, float64) { e.OcaType = 1; return 102, "child-1", 76 },
		"outside rth":           func(ct *Contract, e *RawOrder) (int, string, float64) { e.OutsideRth = true; return 102, "child-1", 76 },
		"no account":            func(ct *Contract, e *RawOrder) (int, string, float64) { e.Account = ""; return 102, "child-1", 76 },
		"blank reference":       func(ct *Contract, e *RawOrder) (int, string, float64) { e.OrderRef = " "; return 102, "child-1", 76 },
		"blank child reference": func(ct *Contract, e *RawOrder) (int, string, float64) { return 102, " ", 76 },
		"shared reference":      func(ct *Contract, e *RawOrder) (int, string, float64) { return 102, e.OrderRef, 76 },
		"zero limit":            func(ct *Contract, e *RawOrder) (int, string, float64) { e.LmtPrice = 0; return 102, "child-1", 76 },
		"nan limit": func(ct *Contract, e *RawOrder) (int, string, float64) {
			e.LmtPrice = math.NaN()
			return 102, "child-1", 76
		},
		"zero stop":            func(ct *Contract, e *RawOrder) (int, string, float64) { return 102, "child-1", 0 },
		"infinite stop":        func(ct *Contract, e *RawOrder) (int, string, float64) { return 102, "child-1", math.Inf(1) },
		"stop at the limit":    func(ct *Contract, e *RawOrder) (int, string, float64) { return 102, "child-1", 80 },
		"stop above the limit": func(ct *Contract, e *RawOrder) (int, string, float64) { return 102, "child-1", 81 },
	}
	for name, f := range terms {
		ct, entry := ordTEntryContract(), ordTEntry(gate)
		childID, childRef, stop := f(ct, entry)
		err := c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, ct, entry, childID, childRef, stop, nil)
		ordTAssertUnsent(t, name, err, socket)
		if !strings.Contains(err.Error(), "invalid stock entry and attached stop terms") {
			t.Fatalf("%s: refused for another reason: %v", name, err)
		}
	}

	// A legacy broker protocol cannot carry the attached stop.
	conn.serverVersion = minServerVerProtoBufPlaceOrder - 1
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ordTEntry(gate), 102, "child-1", 76, nil)
	ordTAssertUnsent(t, "old protocol", err, socket)
	if !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("old protocol refused for another reason: %v", err)
	}
	conn.serverVersion = minServerVerProtoBufPlaceOrder

	// Past the terms and the protocol check, identity still refuses: a
	// non-ASCII reference, a gate that does not name the connection, an
	// entry id this process already placed, an id behind the broker's
	// order-id frontier, and a reference the pair journal already holds.
	// Each leaves no reservation behind.
	ahead := func(entryID int) *RawOrder { e := ordTEntry(gate); e.OrderID = entryID; return e }
	noReservations := func(name string) {
		t.Helper()
		conn.reqIDMu.Lock()
		left := len(conn.reservedOrderIDs)
		conn.reqIDMu.Unlock()
		if left != 0 {
			t.Fatalf("%s: %d order-id reservations left", name, left)
		}
	}
	nonASCII := ahead(5000001)
	nonASCII.OrderRef = "entry-é"
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, nonASCII, 5000002, "child-1", 76, nil)
	ordTAssertUnsent(t, "non-ascii reference", err, socket)
	if !strings.Contains(err.Error(), "ASCII") {
		t.Fatalf("non-ascii reference refused for another reason: %v", err)
	}
	offGate := gate
	offGate.Host = "10.0.0.5"
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, offGate, contract, ahead(5000001), 5000002, "child-1", 76, nil)
	ordTAssertUnsent(t, "gate off the connection", err, socket)
	if !strings.Contains(err.Error(), "host") {
		t.Fatalf("gate host refused for another reason: %v", err)
	}
	noReservations("before the claims")
	c.orderMu.Lock()
	c.placedOrderIDs = map[int]struct{}{5000001: {}}
	c.orderMu.Unlock()
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ahead(5000001), 5000002, "child-1", 76, nil)
	ordTAssertUnsent(t, "entry id already placed", err, socket)
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("placed id refused for another reason: %v", err)
	}
	c.orderMu.Lock()
	c.placedOrderIDs = nil
	c.orderMu.Unlock()
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ahead(50), 51, "child-1", 76, nil)
	ordTAssertUnsent(t, "id behind the frontier", err, socket)
	if !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("frontier refused for another reason: %v", err)
	}
	noReservations("after a refused claim")
	c.orderMu.Lock()
	c.openOrders["entry-1"] = &trackedOrder{ID: "entry-1"}
	c.orderMu.Unlock()
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ahead(5000001), 5000002, "child-1", 76, nil)
	ordTAssertUnsent(t, "reference in use", err, socket)
	if !strings.Contains(err.Error(), "reference already in use") {
		t.Fatalf("reference refused for another reason: %v", err)
	}
	// The refused pair's ids were claimed and so moved the frontier; the
	// next case needs fresh ids to reach the index check.
	c.orderMu.Lock()
	delete(c.openOrders, "entry-1")
	c.brokerOrderIndex["5000004"] = "other"
	c.orderMu.Unlock()
	err = c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ahead(5000003), 5000004, "child-1", 76, nil)
	ordTAssertUnsent(t, "child id indexed", err, socket)
	if !strings.Contains(err.Error(), "reference already in use") {
		t.Fatalf("indexed child id refused for another reason: %v", err)
	}
	c.orderMu.Lock()
	delete(c.brokerOrderIndex, "5000004")
	c.orderMu.Unlock()
	noReservations("after refused references")
	for _, id := range []int{5000001, 5000002, 5000003, 5000004} {
		if c.PlacedOrderID(id) {
			t.Fatalf("order %d retained after refusals", id)
		}
	}

	// Backend connectivity loss (IBKR 1100) refuses before any identity claim.
	c.setBackendConnectivityDown(true, time.Now())
	ordTAssertUnsent(t, "backend down", c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ordTEntry(gate), 102, "child-1", 76, nil), socket)
	c.setBackendConnectivityDown(false, time.Now())

	// Disconnecting the socket generation retires the binding.
	conn.status = StatusDisconnected
	ordTAssertUnsent(t, "disconnected", c.SubmitPaperStockEntryStopForSessionGuarded(ctx, session, gate, contract, ordTEntry(gate), 102, "child-1", 76, nil), socket)

	for _, id := range []int{101, 102} {
		if c.PlacedOrderID(id) {
			t.Fatalf("order %d retained after refusals", id)
		}
	}
	if !isPositiveFiniteStockPrice(0.01) || isPositiveFiniteStockPrice(0) || isPositiveFiniteStockPrice(math.NaN()) || isPositiveFiniteStockPrice(math.Inf(-1)) {
		t.Fatal("isPositiveFiniteStockPrice")
	}
}

func TestOptionExerciseConnectorRefusalsWriteNothing(t *testing.T) {
	conn, c, socket, _, gate := newQueuedInstructionReconnectFixture(t)
	session, _ := c.CaptureSession()
	req := OptionExerciseRequest{Contract: &Contract{ConID: 4242, Symbol: "SYNX", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C", Multiplier: 100},
		ExerciseAction: OptionExerciseActionExercise, ExerciseQuantity: 1, Account: gate.Account}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		//lint:ignore SA1012 the nil-context refusal is the behaviour under test
		"nil context":       {c.ExerciseOptions(nil, req), nil},
		"cancelled context": {c.ExerciseOptions(cancelled, req), context.Canceled},
		//lint:ignore SA1012 the nil-context refusal is the behaviour under test
		"session nil context": {c.ExerciseOptionsForSession(nil, session, req), nil},
		"session cancelled":   {c.ExerciseOptionsForSession(cancelled, session, req), context.Canceled},
		//lint:ignore SA1012 the nil-context refusal is the behaviour under test
		"connection nil context": {conn.exerciseOptionsForEpochGuarded(nil, req, nil, nil), nil},
		"connection cancelled":   {conn.exerciseOptionsForEpochGuarded(cancelled, req, nil, nil), context.Canceled},
	} {
		ordTAssertUnsent(t, name, tc.err, socket)
		if tc.want != nil && !errors.Is(tc.err, tc.want) {
			t.Fatalf("%s: %v", name, tc.err)
		}
	}
	if tradingEnabled {
		t.Skip("default disabled guard is not active in trading-tag builds")
	}
	for name, err := range map[string]error{
		"connector":  c.ExerciseOptions(t.Context(), req),
		"session":    c.ExerciseOptionsForSession(t.Context(), session, req),
		"guarded":    c.ExerciseOptionsForSessionGuarded(t.Context(), session, req, nil),
		"connection": conn.ExerciseOptions(req),
	} {
		ordTAssertUnsent(t, name, err, socket)
		if !errors.Is(err, ErrTradingDisabled) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Below the authority gate, an invalid request and a dead connection are
	// still refused before the encoder.
	bad := req
	bad.ExerciseQuantity = 0
	ordTAssertUnsent(t, "invalid request", conn.exerciseOptionsForEpochGuarded(t.Context(), bad, nil, nil), socket)
	var none *Connection
	ordTAssertUnsent(t, "nil connection", none.exerciseOptionsForEpochGuarded(t.Context(), OptionExerciseRequest{TickerID: 1, Contract: req.Contract, ExerciseAction: 1, ExerciseQuantity: 1, Account: gate.Account}, nil, nil), socket)
	conn.status = StatusDisconnected
	ordTAssertUnsent(t, "disconnected", conn.exerciseOptionsForEpochGuarded(t.Context(), OptionExerciseRequest{TickerID: 1, Contract: req.Contract, ExerciseAction: 1, ExerciseQuantity: 1, Account: gate.Account}, nil, nil), socket)
}

func TestPaperOrderGateValidation(t *testing.T) {
	good := PaperOrderGate{Mode: " Paper ", Account: "DU1234567", Host: "127.0.0.1", Port: 7497, ClientID: 15}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		gate PaperOrderGate
		want string
	}{
		"live mode":          {PaperOrderGate{Mode: "live", Account: "DU1234567", Host: "h", Port: 1, ClientID: 1}, "mode=paper"},
		"no account":         {PaperOrderGate{Mode: "paper", Host: "h", Port: 1, ClientID: 1}, "account"},
		"aggregate account":  {PaperOrderGate{Mode: "paper", Account: "all", Host: "h", Port: 1, ClientID: 1}, "All"},
		"no host":            {PaperOrderGate{Mode: "paper", Account: "DU1234567", Port: 1, ClientID: 1}, "host"},
		"no port":            {PaperOrderGate{Mode: "paper", Account: "DU1234567", Host: "h", ClientID: 1}, "port"},
		"client zero":        {PaperOrderGate{Mode: "paper", Account: "DU1234567", Host: "h", Port: 1}, "client ID"},
		"live account shape": {PaperOrderGate{Mode: "paper", Account: "U1234567", Host: "h", Port: 1, ClientID: 1}, "DU"},
		"endpoint no port":   {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "127.0.0.1", ClientID: 1}, "port"},
	}
	for name, tc := range cases {
		err := tc.gate.validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
	// Endpoint parsing: host:port wins over Host and Port; a bare host fills
	// only the host; an unparsable port keeps the configured one.
	for name, tc := range map[string]struct {
		gate       PaperOrderGate
		host       string
		port       int
		validation bool
	}{
		"endpoint wins":      {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "10.0.0.5:4002", Host: "h", Port: 1, ClientID: 1}, "10.0.0.5", 4002, true},
		"bare endpoint":      {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "10.0.0.5", Port: 4002, ClientID: 1}, "10.0.0.5", 4002, true},
		"bare with host":     {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "10.0.0.5", Host: "h", Port: 4002, ClientID: 1}, "h", 4002, true},
		"unparsable port":    {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "10.0.0.5:x", Port: 4002, ClientID: 1}, "10.0.0.5", 4002, true},
		"ipv6 endpoint":      {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "[::1]:4002", ClientID: 1}, "::1", 4002, true},
		"endpoint port zero": {PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "10.0.0.5:0", ClientID: 1}, "10.0.0.5", 0, false},
	} {
		host, port := tc.gate.endpointParts()
		if host != tc.host || port != tc.port {
			t.Errorf("%s: endpoint parts %q %d", name, host, port)
		}
		if err := tc.gate.validate(); (err == nil) != tc.validation {
			t.Errorf("%s: validate %v", name, err)
		}
	}

	// Against a connection the gate must name the same account, host, port
	// and client; an unconfigured connection field is not compared.
	conn := &Connection{config: &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 15, Account: "DU1234567"}}
	if err := good.validateConnection(conn); err != nil {
		t.Fatal(err)
	}
	if err := good.validateConnection(nil); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil connection: %v", err)
	}
	if err := (PaperOrderGate{Mode: "live"}).validateConnection(conn); err == nil {
		t.Fatal("invalid gate passed the connection check")
	}
	for name, tc := range map[string]struct {
		gate PaperOrderGate
		want string
	}{
		"account": {PaperOrderGate{Mode: "paper", Account: "DU7654321", Host: "127.0.0.1", Port: 7497, ClientID: 15}, "account"},
		"host":    {PaperOrderGate{Mode: "paper", Account: "DU1234567", Host: "10.0.0.5", Port: 7497, ClientID: 15}, "host"},
		"port":    {PaperOrderGate{Mode: "paper", Account: "DU1234567", Host: "127.0.0.1", Port: 4002, ClientID: 15}, "port"},
		"client":  {PaperOrderGate{Mode: "paper", Account: "DU1234567", Host: "127.0.0.1", Port: 7497, ClientID: 16}, "client ID"},
	} {
		err := tc.gate.validateConnection(conn)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
	loose := &Connection{config: &ConnectionConfig{}}
	if err := good.validateConnection(loose); err != nil {
		t.Fatalf("unconfigured connection compared: %v", err)
	}
	if err := (PaperOrderGate{Mode: "paper", Account: "du1234567", Endpoint: "127.0.0.1:7497", ClientID: 15}).validateConnection(conn); err != nil {
		t.Fatalf("endpoint gate against the same connection: %v", err)
	}
}

// Regression: the connection check read Host and Port directly, so a gate
// naming only an Endpoint compared the connection with itself and passed
// against any connection.
// The type's contract says the endpoint "takes precedence over Host and Port
// during validation" and that it "must identify the configured connection";
// validate() honours that through endpointParts, validateConnection reads
// g.Host and g.Port only, which are empty for an endpoint-only gate, and so
// falls back to the connection's own host and port and compares them with
// themselves.
func TestPaperOrderGateEndpointIsCheckedAgainstTheConnection(t *testing.T) {
	conn := &Connection{config: &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 15, Account: "DU1234567"}}
	gate := PaperOrderGate{Mode: "paper", Account: "DU1234567", Endpoint: "10.0.0.5:4002", ClientID: 15}
	if err := gate.validate(); err != nil {
		t.Fatalf("endpoint-only gate is valid on its own: %v", err)
	}
	if err := gate.validateConnection(conn); err == nil {
		t.Fatal("an endpoint naming 10.0.0.5:4002 passed against the connection on 127.0.0.1:7497")
	}
}

func TestValidateOrderTable(t *testing.T) {
	base := func() *IBKROrder {
		return &IBKROrder{Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD", Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 10}
	}
	ok := base()
	if err := ValidateOrder(ok); err != nil || ok.TIF != "DAY" {
		t.Fatalf("default TIF: %q err %v", ok.TIF, err)
	}
	gtc := base()
	gtc.TIF = "GTC"
	if err := ValidateOrder(gtc); err != nil || gtc.TIF != "GTC" {
		t.Fatalf("explicit TIF overwritten: %q err %v", gtc.TIF, err)
	}
	for name, tc := range map[string]struct {
		f    func(o *IBKROrder)
		want string
	}{
		"no symbol":             {func(o *IBKROrder) { o.Symbol = "" }, "symbol"},
		"zero quantity":         {func(o *IBKROrder) { o.TotalQty = 0 }, "quantity"},
		"action HOLD":           {func(o *IBKROrder) { o.Action = "HOLD" }, "action"},
		"no order type":         {func(o *IBKROrder) { o.OrderType = "" }, "order type"},
		"LMT without price":     {func(o *IBKROrder) { o.LmtPrice = 0 }, "limit price"},
		"combo LMT without set": {func(o *IBKROrder) { o.SecType = "BAG"; o.LmtPrice = 0 }, "combo"},
		"combo LMT infinite":    {func(o *IBKROrder) { o.SecType = "BAG"; o.LmtPriceSet = true; o.LmtPrice = math.Inf(1) }, "combo"},
		"STP without stop":      {func(o *IBKROrder) { o.OrderType = "STP"; o.LmtPrice = 0 }, "stop price"},
		"STP LMT without stop":  {func(o *IBKROrder) { o.OrderType = "STP LMT" }, "stop price"},
		"TRAIL without stop":    {func(o *IBKROrder) { o.OrderType = "TRAIL"; o.LmtPrice = 0; o.AuxPrice = 1 }, "trail stop price"},
		"TRAIL neither":         {func(o *IBKROrder) { o.OrderType = "TRAIL"; o.LmtPrice = 0; o.TrailStopPrice = 9 }, "exactly one"},
		"TRAIL both": {func(o *IBKROrder) {
			o.OrderType = "TRAIL"
			o.LmtPrice = 0
			o.TrailStopPrice = 9
			o.AuxPrice = 1
			o.TrailingPercent = 1
		}, "exactly one"},
		"TRAIL LIMIT no offset": {func(o *IBKROrder) { o.OrderType = "TRAIL LIMIT"; o.LmtPrice = 0; o.TrailStopPrice = 9; o.AuxPrice = 1 }, "offset"},
		"TRAIL with offset": {func(o *IBKROrder) {
			o.OrderType = "TRAIL"
			o.LmtPrice = 0
			o.TrailStopPrice = 9
			o.AuxPrice = 1
			o.LmtPriceOffset = 1
		}, "offset"},
		"TRAIL with limit price":   {func(o *IBKROrder) { o.OrderType = "TRAIL"; o.TrailStopPrice = 9; o.AuxPrice = 1 }, "limit price must not"},
		"bond without rules":       {func(o *IBKROrder) { o.SecType = "BOND"; o.ConID = 1 }, "rules"},
		"adaptive unknown":         {func(o *IBKROrder) { o.AdaptivePriority = "Fast"; o.TIF = "DAY" }, "Adaptive priority"},
		"adaptive on an option":    {func(o *IBKROrder) { o.AdaptivePriority = "Normal"; o.SecType = "OPT"; o.TIF = "DAY" }, "Adaptive algorithm"},
		"adaptive off SMART":       {func(o *IBKROrder) { o.AdaptivePriority = "Normal"; o.Exchange = "ISLAND"; o.TIF = "DAY" }, "Adaptive algorithm"},
		"adaptive outside rth":     {func(o *IBKROrder) { o.AdaptivePriority = "Normal"; o.OutsideRth = true; o.TIF = "DAY" }, "Adaptive algorithm"},
		"adaptive without default": {func(o *IBKROrder) { o.AdaptivePriority = "Normal" }, "Adaptive algorithm"},
	} {
		o := base()
		tc.f(o)
		err := ValidateOrder(o)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
	if err := ValidateOrder(nil); err == nil {
		t.Fatal("nil order")
	}
	trail := base()
	trail.OrderType, trail.LmtPrice, trail.TrailStopPrice, trail.TrailingPercent = "TRAIL LIMIT", 0, 9, 2
	trail.LmtPriceOffset = 0.5
	if err := ValidateOrder(trail); err != nil {
		t.Fatalf("trail limit: %v", err)
	}
	adaptive := base()
	adaptive.AdaptivePriority, adaptive.TIF = "Patient", "DAY"
	if err := ValidateOrder(adaptive); err != nil {
		t.Fatalf("adaptive: %v", err)
	}
}

// Preview refusals: nothing reaches the wire for a nil input, a missing
// connection, a stale binding or a non-positive explicit order id.
func TestPreviewOrderWhatIfRefusalsWriteNothing(t *testing.T) {
	conn, c, socket, _, gate := newQueuedInstructionReconnectFixture(t)
	session, _ := c.CaptureSession()
	contract := ordTEntryContract()
	order := &RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account}
	ctx := t.Context()

	if _, err := c.PreviewOrderWhatIf(ctx, nil, order); err == nil {
		t.Fatal("nil contract")
	}
	if _, err := c.PreviewOrderWhatIf(ctx, contract, nil); err == nil {
		t.Fatal("nil order")
	}
	if _, err := c.PreviewOrderWhatIfWithOrderID(ctx, contract, order, 0); err == nil {
		t.Fatal("order id 0")
	}
	if _, err := c.PreviewOrderWhatIfWithOrderIDForSession(ctx, session, contract, order, -1); err == nil {
		t.Fatal("order id -1")
	}
	stale := ConnectorSessionBinding{connector: c, connection: conn, epoch: session.epoch + 1}
	result, err := c.PreviewOrderWhatIfForSession(ctx, stale, contract, order)
	if err != nil || result.Status != OrderWhatIfStatusUnavailable || !strings.Contains(result.Message, "session changed") {
		t.Fatalf("stale binding: %+v err %v", result, err)
	}
	// A validation failure inside the connection is an error, not a result.
	bad := *order
	bad.TotalQty = 0
	if _, err := c.PreviewOrderWhatIfForSession(ctx, session, contract, &bad); err == nil {
		t.Fatal("invalid order previewed")
	}
	// An explicit id owned by an active read-only (FEE) request is refused
	// with the namespace conflict.
	c.historicalMu.Lock()
	if c.historicalRouteReqs == nil {
		c.historicalRouteReqs = make(map[int]chan error)
	}
	c.historicalRouteReqs[700] = make(chan error, 1)
	c.historicalMu.Unlock()
	if _, err := c.PreviewOrderWhatIfWithOrderID(ctx, contract, order, 700); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("fee-owned id: %v", err)
	}
	c.historicalMu.Lock()
	delete(c.historicalRouteReqs, 700)
	c.historicalMu.Unlock()

	// A disconnected socket generation and a missing connection are reads
	// that cannot be answered, not errors.
	conn.status = StatusDisconnected
	result, err = c.PreviewOrderWhatIf(ctx, contract, order)
	if err != nil || result.Status != OrderWhatIfStatusUnavailable || !strings.Contains(result.Message, "not connected") {
		t.Fatalf("disconnected: %+v err %v", result, err)
	}
	result, err = c.PreviewOrderWhatIfForSession(ctx, session, contract, order)
	if err != nil || result.Status != OrderWhatIfStatusUnavailable {
		t.Fatalf("disconnected binding: %+v err %v", result, err)
	}
	conn.status = StatusConnected
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	result, err = c.PreviewOrderWhatIf(ctx, contract, order)
	if err != nil || result.Status != OrderWhatIfStatusUnavailable {
		t.Fatalf("no connection: %+v err %v", result, err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	if socket.Len() != 0 {
		t.Fatalf("%d bytes reached the socket", socket.Len())
	}
}

// The uncertainty seam: an order whose status became ambiguous during a
// modify preview is not previewed again; the caller is told to refresh and
// WorkingOrderUncertain is set. The refresh is a reqOpenOrders read, never a
// placeOrder.
func TestPreviewOrderWhatIfReportsWorkingOrderUncertain(t *testing.T) {
	conn, c, socket, _, gate := newQueuedInstructionReconnectFixture(t)
	epoch := conn.BrokerSessionEpoch()
	conn.reqIDMu.Lock()
	conn.modifyStatusUncertain = map[int]uint64{700: 1}
	conn.reqIDMu.Unlock()
	if !conn.modifyStatusIsUncertain(700, epoch) || conn.modifyStatusIsUncertain(700, epoch+1) || conn.modifyStatusIsUncertain(701, epoch) {
		t.Fatal("modifyStatusIsUncertain")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	result, err := c.PreviewOrderWhatIfWithOrderID(ctx, ordTEntryContract(), &RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account}, 700)
	if err != nil || result.Status != OrderWhatIfStatusUnavailable || !result.WorkingOrderUncertain {
		t.Fatalf("uncertain preview: %+v err %v", result, err)
	}
	for _, frame := range decodeOutboundFrames(t, conn, socket.Bytes()) {
		if len(frame) > 0 && (frame[0] == strconv.Itoa(protoPlaceOrderMsgID) || frame[0] == strconv.Itoa(placeOrder)) {
			t.Fatalf("an uncertain order was previewed on the wire: %#v", frame)
		}
	}
}

// Modify-preview bookkeeping is epoch-scoped and refuses a second preview of
// an order while one is active or its status is uncertain.
func TestModifyPreviewBookkeeping(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{ClientID: 41})
	t.Cleanup(conn.rateLimiter.Stop)
	epoch := conn.BrokerSessionEpoch()
	if err := conn.beginModifyPreview(700, epoch+1); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("other epoch: %v", err)
	}
	if err := conn.beginModifyPreview(700, epoch); err != nil {
		t.Fatal(err)
	}
	if err := conn.beginModifyPreview(700, epoch); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("second preview of an active id: %v", err)
	}
	if !conn.modifyPreviewActive(700, epoch) || conn.modifyPreviewActive(700, epoch+1) {
		t.Fatal("modifyPreviewActive")
	}
	conn.markWhatIfOrderID(701)
	if d := conn.orderPreviewDisposition(700, epoch); d != previewStatusUncertain {
		t.Fatalf("active modify preview disposition %v", d)
	}
	if d := conn.orderPreviewDisposition(701, epoch); d != previewStatusIgnore {
		t.Fatalf("new-order preview disposition %v", d)
	}
	if d := conn.orderPreviewDisposition(702, epoch); d != previewStatusOrdinary {
		t.Fatalf("ordinary disposition %v", d)
	}
	if d := conn.orderPreviewDisposition(700, epoch+1); d != previewStatusOrdinary {
		t.Fatalf("other epoch disposition %v", d)
	}
	if _, _, valid := conn.modifyStatusRecoveryTokens(epoch); valid {
		t.Fatal("recovery tokens valid while a preview is active")
	}
	if conn.finishModifyPreview(700, epoch+1) {
		t.Fatal("finish on another epoch reported uncertainty")
	}
	if !conn.modifyPreviewActive(700, epoch) {
		t.Fatal("finish on another epoch cleared the preview")
	}
	if conn.finishModifyPreview(700, epoch) {
		t.Fatal("clean finish reported uncertainty")
	}
	if conn.modifyPreviewActive(700, epoch) {
		t.Fatal("preview still active after finish")
	}
	conn.reqIDMu.Lock()
	conn.modifyStatusUncertain = map[int]uint64{700: 3}
	conn.reqIDMu.Unlock()
	if err := conn.beginModifyPreview(700, epoch); !errors.Is(err, ErrBrokerIDNamespaceConflict) {
		t.Fatalf("preview of an uncertain id: %v", err)
	}
	if !conn.finishModifyPreview(700, epoch) {
		t.Fatal("finish did not report the uncertain status")
	}
	tokens, generation, valid := conn.modifyStatusRecoveryTokens(epoch)
	if !valid || tokens[700] != 3 {
		t.Fatalf("recovery tokens %v valid=%t", tokens, valid)
	}

	// Recovery from an open-order snapshot clears the marker only for a
	// working order this client placed under the identity it reported.
	conn.reqIDMu.Lock()
	conn.reportedOwnOrderIDs = map[int]ReportedOwnOrder{700: {PermID: 9001, Account: "DU1234567", OrderRef: "ref-1"}}
	conn.reqIDMu.Unlock()
	working := OrderLifecycleEvent{OrderID: 700, PermID: 9001, Account: "DU1234567", OrderRef: "ref-1", ClientID: 41, ClientIDPresent: true, Status: "Submitted"}
	if conn.recoverModifyStatusFromSnapshot(epoch, tokens, generation+1, []OrderLifecycleEvent{working}) {
		t.Fatal("stale generation accepted")
	}
	if conn.recoverModifyStatusFromSnapshot(epoch+1, tokens, generation, []OrderLifecycleEvent{working}) {
		t.Fatal("other epoch accepted")
	}
	for name, ev := range map[string]OrderLifecycleEvent{
		"whatif echo":      {OrderID: 700, PermID: 9001, Account: "DU1234567", OrderRef: "ref-1", ClientID: 41, ClientIDPresent: true, Status: "Submitted", WhatIf: true},
		"other client":     {OrderID: 700, PermID: 9001, Account: "DU1234567", OrderRef: "ref-1", ClientID: 0, ClientIDPresent: true, Status: "Submitted"},
		"legacy no client": {OrderID: 700, PermID: 9001, Account: "DU1234567", OrderRef: "ref-1", Status: "Submitted"},
		"filled":           {OrderID: 700, PermID: 9001, Account: "DU1234567", OrderRef: "ref-1", ClientID: 41, ClientIDPresent: true, Status: "Filled"},
		"other permId":     {OrderID: 700, PermID: 9002, Account: "DU1234567", OrderRef: "ref-1", ClientID: 41, ClientIDPresent: true, Status: "Submitted"},
		"other order":      {OrderID: 701, PermID: 9001, Account: "DU1234567", OrderRef: "ref-1", ClientID: 41, ClientIDPresent: true, Status: "Submitted"},
	} {
		if !conn.recoverModifyStatusFromSnapshot(epoch, tokens, generation, []OrderLifecycleEvent{ev}) || !conn.modifyStatusIsUncertain(700, epoch) {
			t.Errorf("%s cleared the uncertain status", name)
		}
	}
	if !conn.recoverModifyStatusFromSnapshot(epoch, tokens, generation, []OrderLifecycleEvent{working}) || conn.modifyStatusIsUncertain(700, epoch) {
		t.Fatal("exact working report did not clear the uncertain status")
	}
}

// Client 0 inherits every hand order, so its recovery needs the exact
// identity of the order this process placed, remembered before the preview.
func TestClientZeroPreviewIdentityRecovery(t *testing.T) {
	conn := NewConnection(&ConnectionConfig{ClientID: 0})
	t.Cleanup(conn.rateLimiter.Stop)
	epoch := conn.BrokerSessionEpoch()
	want := OrderChangeIdentity{PermID: 9001, Account: "DU1234567", OrderRef: "ref-1"}
	// Not locally placed, incomplete identity, other epoch: nothing remembered.
	conn.rememberLocalPreviewIdentity(700, epoch, &want, false)
	conn.rememberLocalPreviewIdentity(700, epoch, &OrderChangeIdentity{PermID: 9001}, true)
	conn.rememberLocalPreviewIdentity(700, epoch, nil, true)
	conn.rememberLocalPreviewIdentity(700, epoch+1, &want, true)
	if len(conn.modifyLocalIdentity) != 0 {
		t.Fatalf("identity remembered without grounds: %v", conn.modifyLocalIdentity)
	}
	conn.rememberLocalPreviewIdentity(700, epoch, &want, true)
	other := want
	other.OrderRef = "ref-2"
	conn.rememberLocalPreviewIdentity(700, epoch, &other, true)
	if got := conn.modifyLocalIdentity[700]; got != want {
		t.Fatalf("first identity not kept: %+v", got)
	}
	// A client with a non-zero id has adoption and remembers nothing here.
	named := NewConnection(&ConnectionConfig{ClientID: 41})
	t.Cleanup(named.rateLimiter.Stop)
	named.rememberLocalPreviewIdentity(700, named.BrokerSessionEpoch(), &want, true)
	if len(named.modifyLocalIdentity) != 0 {
		t.Fatal("client 41 remembered a local identity")
	}

	ev := func(id, perm int, ref, status string, whatIf bool, client int) OrderLifecycleEvent {
		return OrderLifecycleEvent{OrderID: id, PermID: perm, Account: "DU1234567", OrderRef: ref, Status: status, WhatIf: whatIf, ClientID: client, ClientIDPresent: true}
	}
	if clientZeroSnapshotNames(700, want, nil) {
		t.Fatal("empty snapshot names the order")
	}
	if !clientZeroSnapshotNames(700, want, []OrderLifecycleEvent{ev(700, 9001, "ref-1", "Submitted", false, 0), ev(701, 1, "x", "Submitted", false, 0)}) {
		t.Fatal("exact client-0 report not recognised")
	}
	for name, orders := range map[string][]OrderLifecycleEvent{
		"whatif echo only": {ev(700, 9001, "ref-1", "Submitted", true, 0)},
		"other client":     {ev(700, 9001, "ref-1", "Submitted", false, 41)},
		"other permId":     {ev(700, 9002, "ref-1", "Submitted", false, 0)},
		"other ref":        {ev(700, 9001, "ref-2", "Submitted", false, 0)},
		"filled":           {ev(700, 9001, "ref-1", "Filled", false, 0)},
		"one of two wrong": {ev(700, 9001, "ref-1", "Submitted", false, 0), ev(700, 9002, "ref-1", "Submitted", false, 0)},
	} {
		if clientZeroSnapshotNames(700, want, orders) {
			t.Errorf("%s named the order", name)
		}
	}
	conn.reqIDMu.Lock()
	conn.modifyStatusUncertain = map[int]uint64{700: 1}
	conn.reqIDMu.Unlock()
	tokens, generation, valid := conn.modifyStatusRecoveryTokens(epoch)
	if !valid {
		t.Fatal("tokens")
	}
	if !conn.recoverModifyStatusFromSnapshot(epoch, tokens, generation, []OrderLifecycleEvent{ev(700, 9001, "ref-1", "Submitted", false, 41)}) || !conn.modifyStatusIsUncertain(700, epoch) {
		t.Fatal("another client's report cleared client 0's uncertainty")
	}
	if !conn.recoverModifyStatusFromSnapshot(epoch, tokens, generation, []OrderLifecycleEvent{ev(700, 9001, "ref-1", "Submitted", false, 0)}) || conn.modifyStatusIsUncertain(700, epoch) {
		t.Fatal("the exact client-0 report did not clear the uncertainty")
	}
}

// whatIfTapFrame keeps only the frames that name the previewed order.
func TestWhatIfTapFrameNamesOnlyTheOrder(t *testing.T) {
	now := time.Now()
	line, ok := whatIfTapFrame(timestampedSystemNotice(77, now, 10349, "Order\tpreview  rejected"), 77)
	if !ok || line != "notice 10349: Order preview rejected" {
		t.Fatalf("notice: %q %t", line, ok)
	}
	if _, ok := whatIfTapFrame(timestampedSystemNotice(78, now, 10349, "x"), 77); ok {
		t.Fatal("another order's notice kept")
	}
	if _, ok := whatIfTapFrame([]string{"204", "\xff"}, 77); ok {
		t.Fatal("malformed notice kept")
	}
	line, ok = whatIfTapFrame([]string{"4", "2", "77", "201", "rejected"}, 77)
	if !ok || line != "error 201: rejected" {
		t.Fatalf("error: %q %t", line, ok)
	}
	if _, ok := whatIfTapFrame([]string{"4", "2", "78", "201", "rejected"}, 77); ok {
		t.Fatal("another order's error kept")
	}
	if _, ok := whatIfTapFrame([]string{"4", "2", "77", "0", "x"}, 77); ok {
		t.Fatal("code 0 kept")
	}
	line, ok = whatIfTapFrame([]string{"3", "77", "Submitted"}, 77)
	if !ok || line != "msg 3, 3 fields" {
		t.Fatalf("status by position 1: %q %t", line, ok)
	}
	if line, ok := whatIfTapFrame([]string{"5", "45", "77", "x"}, 77); !ok || line != "msg 5, 4 fields" {
		t.Fatalf("open order by position 2: %q %t", line, ok)
	}
	line, ok = whatIfTapFrame([]string{"5", "protobuf", "orderId=77", "whatIf=true", "status=PreSubmitted", "symbol=SYNX"}, 77)
	if !ok || line != "msg 5, 6 fields (whatIf=true, status=PreSubmitted, symbol=SYNX)" {
		t.Fatalf("proto summary: %q %t", line, ok)
	}
	for name, fields := range map[string][]string{
		"other order":    {"5", "protobuf", "orderId=78"},
		"no id":          {"5"},
		"non-numeric id": {"x", "77"},
	} {
		if _, ok := whatIfTapFrame(fields, 77); ok {
			t.Errorf("%s kept", name)
		}
	}
	if _, ok := whatIfTapFrame([]string{"3", "0"}, 0); ok {
		t.Fatal("order 0 kept")
	}
	trace := &whatIfFrameTrace{}
	for i := range whatIfFrameTraceMax + 3 {
		trace.add("line " + strconv.Itoa(i))
	}
	if got := trace.snapshot(); len(got) != whatIfFrameTraceMax || got[0] != "line 0" {
		t.Fatalf("trace kept %d lines, first %q", len(got), got[0])
	}
}
