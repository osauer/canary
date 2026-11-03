package ibkr

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func histTBoundOrder() OrderLifecycleEvent {
	return OrderLifecycleEvent{
		Type: OrderLifecycleEventOpenOrder, OrderID: 7, PermID: 31, ClientIDPresent: true,
		Symbol: "SYNX", SecType: "STK", ConID: 4242, Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", LocalSymbol: "SYNX", TradingClass: "NMS",
		Action: "BUY", TotalQuantity: 100, OrderType: "LMT", LimitPrice: 150, TIF: "DAY", Status: "Submitted", Account: "DU1234567",
		ModifyTermsKnown: true, AlgoKnown: true, Multiplier: 0,
	}
}

func histTTarget() TWSOrderControlTarget {
	return TWSOrderControlTarget{PermID: 31, ConID: 4242, Symbol: "SYNX", SecType: "STK", Action: "BUY", Quantity: 100, OrderType: "LMT", LimitPrice: 150, TIF: "DAY"}
}

func histTGateError(t *testing.T, err error, fragment string) {
	t.Helper()
	if !errors.Is(err, ErrTWSOrderControlGate) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("err = %v, want a gate refusal mentioning %q", err, fragment)
	}
}

// Every zero timeout takes its default; the gate refuses a blank or aggregate
// account, an unknown mode, and a nil context before any connection.
func TestTWSOrderControlDefaultsAndGate(t *testing.T) {
	cfg := TWSOrderControlConfig{}.withDefaults()
	if cfg.ConnectTimeout != defaultTWSOrderControlConnectTimeout || cfg.BindTimeout != defaultTWSOrderControlBindTimeout || cfg.AckTimeout != defaultTWSOrderControlAckTimeout {
		t.Fatalf("defaults = %+v", cfg)
	}
	kept := TWSOrderControlConfig{ConnectTimeout: time.Second, BindTimeout: 2 * time.Second, AckTimeout: 3 * time.Second}.withDefaults()
	if kept.ConnectTimeout != time.Second || kept.BindTimeout != 2*time.Second || kept.AckTimeout != 3*time.Second {
		t.Fatalf("explicit timeouts changed: %+v", kept)
	}

	base := TWSOrderControlConfig{Host: "127.0.0.1", Port: 7497, Mode: "paper", Account: "DU1234567"}
	if err := base.gate(); err != nil {
		t.Fatalf("paper DU account refused: %v", err)
	}
	for _, tc := range []struct {
		name, fragment string
		mutate         func(*TWSOrderControlConfig)
	}{
		{"blank account", "concrete account", func(c *TWSOrderControlConfig) { c.Account = "  " }},
		{"aggregate account", "concrete account", func(c *TWSOrderControlConfig) { c.Account = "all" }},
		{"unknown mode", "paper or live", func(c *TWSOrderControlConfig) { c.Mode = "demo" }},
		{"blank host", "endpoint", func(c *TWSOrderControlConfig) { c.Host = " " }},
	} {
		cfg := base
		tc.mutate(&cfg)
		histTGateError(t, cfg.gate(), tc.fragment)
	}
	live := base
	live.Mode, live.Account = "LIVE", "U1234567"
	if err := live.gate(); tradingEnabled != (err == nil) {
		t.Fatalf("live gate = %v with tradingEnabled=%t", err, tradingEnabled)
	}

	var nilCtx context.Context
	receipt, err := RunTWSOrderControl(nilCtx, base, TWSOrderControlCancel, histTTarget(), TWSOrderControlReplacement{})
	histTGateError(t, err, "context is nil")
	if receipt.Outcome != TWSOrderControlRefused || receipt.PermID != 31 {
		t.Fatalf("receipt = %+v", receipt)
	}
}

// The reviewed target and the replacement are checked field by field before
// anything is sent.
func TestTWSOrderControlTargetAndReplacementValidation(t *testing.T) {
	for _, tc := range []struct {
		name, fragment string
		mutate         func(*TWSOrderControlTarget)
	}{
		{"blank side", "symbol, side and order type", func(x *TWSOrderControlTarget) { x.Action = " " }},
		{"blank order type", "symbol, side and order type", func(x *TWSOrderControlTarget) { x.OrderType = "" }},
		{"zero quantity", "quantity must be positive", func(x *TWSOrderControlTarget) { x.Quantity = 0 }},
		{"NaN quantity", "quantity must be positive", func(x *TWSOrderControlTarget) { x.Quantity = math.NaN() }},
		{"infinite quantity", "quantity must be positive", func(x *TWSOrderControlTarget) { x.Quantity = math.Inf(1) }},
	} {
		target := histTTarget()
		tc.mutate(&target)
		histTGateError(t, target.validate(), tc.fragment)
	}
	if err := histTTarget().validate(); err != nil {
		t.Fatalf("reviewed target refused: %v", err)
	}

	good := TWSOrderControlReplacement{Quantity: 60, LimitPrice: 149}
	if err := good.validate(histTTarget()); err != nil {
		t.Fatalf("replacement refused: %v", err)
	}
	for _, tc := range []struct {
		name, fragment string
		target         func() TWSOrderControlTarget
		replacement    TWSOrderControlReplacement
	}{
		{"GTC", "only DAY limit orders", func() TWSOrderControlTarget { x := histTTarget(); x.TIF = "GTC"; return x }, good},
		{"option", "only stock and ETF orders", func() TWSOrderControlTarget { x := histTTarget(); x.SecType = "OPT"; return x }, good},
		{"fractional", "positive whole number", histTTarget, TWSOrderControlReplacement{Quantity: 60.5, LimitPrice: 149}},
		{"NaN quantity", "positive whole number", histTTarget, TWSOrderControlReplacement{Quantity: math.NaN(), LimitPrice: 149}},
		{"at the fill", "exceed the filled quantity", func() TWSOrderControlTarget { x := histTTarget(); x.Filled = 60; return x }, good},
		{"zero price", "limit price must be positive", histTTarget, TWSOrderControlReplacement{Quantity: 60}},
		{"infinite price", "limit price must be positive", histTTarget, TWSOrderControlReplacement{Quantity: 60, LimitPrice: math.Inf(1)}},
	} {
		if err := tc.replacement.validate(tc.target()); !errors.Is(err, ErrTWSOrderControlGate) || !strings.Contains(err.Error(), tc.fragment) {
			t.Fatalf("%s: err = %v, want a gate refusal mentioning %q", tc.name, err, tc.fragment)
		}
	}
	etf := histTTarget()
	etf.SecType, etf.TIF = "etf", ""
	if err := good.validate(etf); err != nil {
		t.Fatalf("ETF with no TIF refused: %v", err)
	}
}

// A bound order is acted on only when TWS still reports it working and on
// the reviewed terms; the refusal names the first difference.
func TestFindTWSOrderRefusalsNameTheDifference(t *testing.T) {
	bound := histTBoundOrder()
	if got, err := findTWSOrder([]OrderLifecycleEvent{bound}, histTTarget()); err != nil || got.OrderID != 7 {
		t.Fatalf("matching order = %+v, %v", got, err)
	}
	flexible := histTTarget()
	flexible.SecType, flexible.TIF, flexible.ConID = "", "", 0
	if _, err := findTWSOrder([]OrderLifecycleEvent{bound}, flexible); err != nil {
		t.Fatalf("target without optional fields refused: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*OrderLifecycleEvent)
		target func(*TWSOrderControlTarget)
		want   error
		reason string
	}{
		{name: "other API client", mutate: func(ev *OrderLifecycleEvent) { ev.ClientID = 5 }, want: ErrTWSOrderControlNotFound, reason: "API client 5"},
		{name: "no order id", mutate: func(ev *OrderLifecycleEvent) { ev.OrderID = 0 }, want: ErrTWSOrderControlBind, reason: "without an order id"},
		{name: "cancelled", mutate: func(ev *OrderLifecycleEvent) { ev.Status = "Cancelled" }, want: ErrTWSOrderControlNotWorking, reason: "status Cancelled"},
		{name: "fully filled", mutate: func(ev *OrderLifecycleEvent) { ev.Filled = 100 }, want: ErrTWSOrderControlNotWorking, reason: "filled 100 of 100"},
		{name: "deactivated", mutate: func(ev *OrderLifecycleEvent) { ev.Deactivated = new(true) }, want: ErrTWSOrderControlNotWorking, reason: "status Submitted"},
		{name: "contract id", mutate: func(ev *OrderLifecycleEvent) { ev.ConID = 4343 }, want: ErrTWSOrderControlChanged, reason: "contract id is 4343, reviewed 4242"},
		{name: "re-priced", mutate: func(ev *OrderLifecycleEvent) { ev.LimitPrice = 151 }, want: ErrTWSOrderControlChanged, reason: "limit price is 151, reviewed 150"},
		{name: "partial fill", mutate: func(ev *OrderLifecycleEvent) { ev.Filled = 40 }, want: ErrTWSOrderControlChanged, reason: "filled is 40, reviewed 0"},
		{name: "stop price", mutate: func(ev *OrderLifecycleEvent) { ev.AuxPrice = 1 }, want: ErrTWSOrderControlChanged, reason: "stop or trail price is 1, reviewed 0"},
		{name: "time in force", mutate: func(ev *OrderLifecycleEvent) { ev.TIF = "GTC" }, want: ErrTWSOrderControlChanged, reason: "time in force is GTC, reviewed DAY"},
		{name: "symbol blank", mutate: func(ev *OrderLifecycleEvent) { ev.Symbol = "" }, want: ErrTWSOrderControlChanged, reason: "symbol is empty, reviewed SYNX"},
		{name: "absent", target: func(x *TWSOrderControlTarget) { x.PermID = 99 }, want: ErrTWSOrderControlNotFound, reason: "filled, cancelled or placed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := histTBoundOrder()
			if tc.mutate != nil {
				tc.mutate(&ev)
			}
			target := histTTarget()
			if tc.target != nil {
				tc.target(&target)
			}
			_, err := findTWSOrder([]OrderLifecycleEvent{ev}, target)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %v, want %v mentioning %q", err, tc.want, tc.reason)
			}
		})
	}
	for _, status := range []string{"Filled", "ApiCancelled", "Inactive", "PendingCancel", " canceled "} {
		ev := histTBoundOrder()
		ev.Status = status
		if twsOrderWorking(ev) {
			t.Fatalf("status %q read as working", status)
		}
	}
	working := histTBoundOrder()
	working.Status, working.Filled = "PreSubmitted", 40
	if !twsOrderWorking(working) {
		t.Fatal("a partially filled PreSubmitted order read as not working")
	}
}

// The replacement is the bound order under its own id with only quantity and
// limit changed; terms the encoder cannot carry refuse the modify.
func TestTWSReplacementOrderKeepsTheBoundTerms(t *testing.T) {
	bound := histTBoundOrder()
	bound.Exchange, bound.Multiplier, bound.Account, bound.TIF = "", 1, "", ""
	order, err := twsReplacementOrder(bound, TWSOrderControlReplacement{Quantity: 60, LimitPrice: 149}, "DU1234567")
	if err != nil {
		t.Fatalf("replacement = %v", err)
	}
	if order.OrderID != 7 || order.ClientID != 0 || order.Account != "DU1234567" || order.Exchange != "SMART" || order.TIF != "DAY" || order.Multiplier != "1" {
		t.Fatalf("order = %+v, want the bound id, client 0, the configured account and SMART/DAY defaults", order)
	}
	if order.TotalQty != 60 || order.LmtPrice != 149 || order.OrderType != "LMT" || order.Symbol != "SYNX" || order.ConID != 4242 || !order.Transmit {
		t.Fatalf("order terms = %+v", order)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*OrderLifecycleEvent)
		qty    float64
		want   error
	}{
		{"stop order", func(ev *OrderLifecycleEvent) { ev.OrderType = "STP" }, 60, ErrTWSOrderControlGate},
		{"GTC", func(ev *OrderLifecycleEvent) { ev.TIF = "GTC" }, 60, ErrTWSOrderControlGate},
		{"terms unknown", func(ev *OrderLifecycleEvent) { ev.ModifyTermsKnown = false }, 60, ErrTWSOrderControlGate},
		{"unsupported term", func(ev *OrderLifecycleEvent) { ev.ModifyUnsupported = "trailing stop" }, 60, ErrTWSOrderControlGate},
		{"algo unknown", func(ev *OrderLifecycleEvent) { ev.AlgoKnown = false }, 60, ErrTWSOrderControlGate},
		{"foreign algo", func(ev *OrderLifecycleEvent) { ev.AlgoStrategy = "Vwap" }, 60, ErrTWSOrderControlGate},
		{"priority without algo", func(ev *OrderLifecycleEvent) { ev.AdaptivePriority = "Urgent" }, 60, ErrTWSOrderControlGate},
		{"odd adaptive priority", func(ev *OrderLifecycleEvent) { ev.AlgoStrategy, ev.AdaptivePriority = "Adaptive", "Fast" }, 60, ErrTWSOrderControlGate},
		{"filled since review", func(ev *OrderLifecycleEvent) { ev.Filled = 60 }, 60, ErrTWSOrderControlChanged},
	} {
		ev := histTBoundOrder()
		tc.mutate(&ev)
		if _, err := twsReplacementOrder(ev, TWSOrderControlReplacement{Quantity: tc.qty, LimitPrice: 149}, "DU1234567"); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	adaptive := histTBoundOrder()
	adaptive.AlgoStrategy, adaptive.AdaptivePriority = "Adaptive", "Patient"
	if order, err := twsReplacementOrder(adaptive, TWSOrderControlReplacement{Quantity: 60, LimitPrice: 149}, "DU1234567"); err != nil || order.AdaptivePriority != "Patient" {
		t.Fatalf("adaptive replacement = %+v, %v", order, err)
	}
}

func histTAnswerSession(answers ...twsOrderControlAnswer) *twsOrderControlSession {
	s := newTWSOrderControlSession()
	for _, a := range answers {
		s.offer(a)
	}
	return s
}

func histTStatus(orderID int, status string) twsOrderControlAnswer {
	return twsOrderControlAnswer{event: OrderLifecycleEvent{Type: OrderLifecycleEventStatus, OrderID: orderID, Status: status}}
}

// The broker's answer settles the receipt: warnings travel with acceptance,
// answers for other ids are ignored, a pending acknowledgement outlives the
// timer as pending, a terminal status on a modify is a refusal, the openOrder
// echo must carry the new terms, and silence or a cancelled wait is uncertain.
func TestAwaitTWSOrderControlAnswerSettlesByBrokerAnswer(t *testing.T) {
	receipt := TWSOrderControlReceipt{Action: TWSOrderControlCancel, PermID: 31, BoundOrderID: 7}
	replacement := TWSOrderControlReplacement{Quantity: 60, LimitPrice: 149}

	s := histTAnswerSession(
		twsOrderControlAnswer{event: OrderLifecycleEvent{Type: OrderLifecycleEventError, OrderID: 7}, code: 2109, msg: "outside RTH warning"},
		histTStatus(8, "Cancelled"),
		histTStatus(7, "ApiCancelled"),
	)
	got, err := awaitTWSOrderControlAnswer(t.Context(), s, receipt, TWSOrderControlCancel, replacement, time.Second)
	if err != nil || got.Outcome != TWSOrderControlConfirmed || got.BrokerStatus != "ApiCancelled" || got.BrokerCode != 0 {
		t.Fatalf("cancel confirmation = %+v, %v", got, err)
	}

	s = histTAnswerSession(histTStatus(7, "PendingCancel"))
	got, err = awaitTWSOrderControlAnswer(t.Context(), s, receipt, TWSOrderControlCancel, replacement, 30*time.Millisecond)
	if err != nil || got.Outcome != TWSOrderControlPending || got.BrokerStatus != "PendingCancel" {
		t.Fatalf("pending cancel = %+v, %v", got, err)
	}

	got, err = awaitTWSOrderControlAnswer(t.Context(), histTAnswerSession(), receipt, TWSOrderControlCancel, replacement, 30*time.Millisecond)
	if !errors.Is(err, ErrTWSOrderControlUncertain) || got.Outcome != TWSOrderControlUncertain {
		t.Fatalf("silence = %+v, %v", got, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err = awaitTWSOrderControlAnswer(ctx, histTAnswerSession(), receipt, TWSOrderControlCancel, replacement, time.Second)
	if !errors.Is(err, ErrTWSOrderControlUncertain) || !errors.Is(err, context.Canceled) || got.Outcome != TWSOrderControlUncertain {
		t.Fatalf("cancelled wait = %+v, %v", got, err)
	}
	// An acknowledgement already heard survives a wait the caller gives up on.
	ctx, cancel = context.WithCancel(t.Context())
	stop := time.AfterFunc(50*time.Millisecond, cancel)
	got, err = awaitTWSOrderControlAnswer(ctx, histTAnswerSession(histTStatus(7, "PendingCancel")), receipt, TWSOrderControlCancel, replacement, 10*time.Second)
	stop.Stop()
	cancel()
	if err != nil || got.Outcome != TWSOrderControlPending {
		t.Fatalf("cancelled wait after an acknowledgement = %+v, %v", got, err)
	}

	modify := receipt
	modify.Action = TWSOrderControlModify
	oldTerms := histTBoundOrder()
	newTerms := histTBoundOrder()
	newTerms.TotalQuantity, newTerms.LimitPrice = 60, 149
	s = histTAnswerSession(twsOrderControlAnswer{event: oldTerms}, histTStatus(7, "Submitted"), twsOrderControlAnswer{event: newTerms})
	got, err = awaitTWSOrderControlAnswer(t.Context(), s, modify, TWSOrderControlModify, replacement, time.Second)
	if err != nil || got.Outcome != TWSOrderControlConfirmed || got.BrokerStatus != "Submitted" {
		t.Fatalf("modify confirmation = %+v, %v", got, err)
	}
	s = histTAnswerSession(histTStatus(7, "PreSubmitted"))
	got, err = awaitTWSOrderControlAnswer(t.Context(), s, modify, TWSOrderControlModify, replacement, 30*time.Millisecond)
	if err != nil || got.Outcome != TWSOrderControlPending {
		t.Fatalf("modify acknowledgement = %+v, %v", got, err)
	}
	s = histTAnswerSession(histTStatus(7, "Filled"))
	got, err = awaitTWSOrderControlAnswer(t.Context(), s, modify, TWSOrderControlModify, replacement, time.Second)
	if !errors.Is(err, ErrTWSOrderControlNotWorking) || got.Outcome != TWSOrderControlRefused {
		t.Fatalf("modify of a filled order = %+v, %v", got, err)
	}
	s = histTAnswerSession(twsOrderControlAnswer{event: OrderLifecycleEvent{Type: OrderLifecycleEventError, OrderID: 7}, code: 201, msg: "Order rejected - reason:"})
	got, err = awaitTWSOrderControlAnswer(t.Context(), s, modify, TWSOrderControlModify, replacement, time.Second)
	if !errors.Is(err, ErrTWSOrderControlRefused) || got.Outcome != TWSOrderControlRefused || got.BrokerCode != 201 || got.BrokerMessage != "Order rejected - reason:" {
		t.Fatalf("broker refusal = %+v, %v", got, err)
	}
}

// The control session hears the broker through every frame shape the
// connection decodes: legacy error frames and protocol-buffer notices carry
// the id and code, bind-time statuses update the bound orders, and a new
// bind forgets the previous answers.
func TestTWSOrderControlSessionCollectsBrokerAnswers(t *testing.T) {
	conn, _ := newReadyWireTestConnection(t)
	s := newTWSOrderControlSession()
	s.attach(conn)
	conn.dispatchHandlers(msgErrMsg, []string{"4", "2", "7", "202", "Order Canceled - reason:"}, conn.BrokerSessionEpoch())
	conn.dispatchHandlers(msgErrMsg, []string{"4", "2", "7"}, conn.BrokerSessionEpoch())
	select {
	case a := <-s.answers:
		if a.event.OrderID != 7 || a.code != 202 || a.msg != "Order Canceled - reason:" || a.event.Type != OrderLifecycleEventError {
			t.Fatalf("legacy error answer = %+v", a)
		}
	default:
		t.Fatal("the legacy error frame produced no answer")
	}
	select {
	case a := <-s.answers:
		t.Fatalf("a truncated error frame produced %+v", a)
	default:
	}

	end := s.beginBind()
	conn.dispatchHandlers(msgOpenOrder, conn.decodeMessage(twsGatewayOpenOrder(twsGatewayOrder{PermID: 31, Bound: 7, Symbol: "SYNX", Action: "BUY", Quantity: 100, LimitPrice: 150, Status: "Submitted"}, 0)), conn.BrokerSessionEpoch())
	conn.dispatchHandlers(msgOrderStatus, conn.decodeMessage(twsGatewayOrderStatus(twsGatewayOrder{PermID: 31, Bound: 7, Quantity: 100, Filled: 40, Status: "Submitted"})), conn.BrokerSessionEpoch())
	select {
	case <-end:
		t.Fatal("the bind ended before openOrderEnd")
	default:
	}
	conn.dispatchHandlers(msgOpenOrderEnd, []string{"53", "1"}, conn.BrokerSessionEpoch())
	<-end
	bound := s.boundOrders()
	if len(bound) != 1 || bound[0].OrderID != 7 || bound[0].PermID != 31 || bound[0].Filled != 40 || bound[0].Remaining != 60 || bound[0].Status != "Submitted" {
		t.Fatalf("bound orders = %+v, want the status folded into the openOrder", bound)
	}
	if len(s.answers) == 0 {
		t.Fatal("the bind produced no answers to drain")
	}
	s.beginBind()
	if len(s.answers) != 0 || len(s.boundOrders()) != 0 {
		t.Fatal("a new bind kept the previous answers or orders")
	}
}
