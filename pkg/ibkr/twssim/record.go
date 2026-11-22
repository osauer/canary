package twssim

import (
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PlaceOrder is a placeOrder request as the broker reads it: decoded by the
// official field table (schema.go), never by pkg/ibkr's encoder. A field the
// broker drops is absent here and named in IgnoredByBroker. Absent booleans
// are false (the Gateway reads transmit and whatIf as has() ? get() : false),
// the quantity is the decimal string's value, and openClose is its first
// character.
type PlaceOrder struct {
	OrderID         int
	ClientID        int
	ParentID        int
	ConID           int
	Symbol          string
	SecType         string
	Expiry          string
	Strike          float64
	Right           string
	Multiplier      float64
	Exchange        string
	PrimaryExch     string
	Currency        string
	LocalSymbol     string
	TradingClass    string
	ComboLegs       int
	Action          string
	Quantity        float64
	OrderType       string
	LimitPrice      float64
	AuxPrice        float64
	TrailingPercent float64
	TrailStopPrice  float64
	TriggerMethod   int
	TIF             string
	Account         string
	OrderRef        string
	OcaGroup        string
	OpenClose       string
	AlgoStrategy    string
	AlgoParams      map[string]string
	OutsideRTH      bool
	// AttachedOrders says the request carried PlaceOrderRequest.4.
	AttachedOrders bool
	WhatIf         bool
	Transmit       bool
	// IgnoredByBroker names each field the broker's parser drops, as
	// "message-path.field-number" (contract.7 is Contract.multiplier).
	IgnoredByBroker []string
}

// officialMessage is one decoded message: its fields by name, the last value
// winning as protobuf's does, and its message fields, repeated ones in order.
type officialMessage struct {
	scalar map[string]protoField
	nested map[string][]*officialMessage
}

func (m *officialMessage) str(name string) string { return string(m.scalar[name].b) }

func (m *officialMessage) integer(name string) int { return int(int32(m.scalar[name].u)) }

func (m *officialMessage) flag(name string) bool {
	f, ok := m.scalar[name]
	return ok && f.u != 0
}

func (m *officialMessage) float(name string) float64 {
	f, ok := m.scalar[name]
	if !ok {
		return 0
	}
	return f.double()
}

func (m *officialMessage) one(name string) *officialMessage {
	if subs := m.nested[name]; len(subs) > 0 {
		return subs[len(subs)-1]
	}
	return &officialMessage{}
}

// decodeOfficial decodes body as the named official message. path names the
// message in ignored entries.
func decodeOfficial(message string, body []byte, path string, ignored *[]string) (*officialMessage, error) {
	fields, err := decodeProto(body)
	if err != nil {
		return nil, err
	}
	table := map[int]officialField{}
	for _, f := range officialMessages[message] {
		table[f.num] = f
	}
	out := &officialMessage{scalar: map[string]protoField{}, nested: map[string][]*officialMessage{}}
	for _, f := range fields {
		def, known := table[f.num]
		if !known || def.wire != f.wire {
			*ignored = append(*ignored, path+"."+strconv.Itoa(f.num))
			continue
		}
		if def.msg == "" {
			out.scalar[def.name] = f
			continue
		}
		if _, defined := officialMessages[def.msg]; !defined {
			continue // a message the simulator does not read; the broker keeps it
		}
		sub, err := decodeOfficial(def.msg, f.b, def.name, ignored)
		if err != nil {
			return nil, err
		}
		out.nested[def.name] = append(out.nested[def.name], sub)
	}
	return out, nil
}

func decodePlaceOrder(body []byte) (PlaceOrder, error) {
	var o PlaceOrder
	req, err := decodeOfficial("PlaceOrderRequest", body, "placeOrder", &o.IgnoredByBroker)
	if err != nil {
		return o, err
	}
	c, x := req.one("contract"), req.one("order")
	o.OrderID = req.integer("orderId")
	o.ConID, o.Symbol, o.SecType = c.integer("conId"), c.str("symbol"), c.str("secType")
	o.Expiry, o.Strike, o.Right, o.Multiplier = c.str("lastTradeDateOrContractMonth"), c.float("strike"), c.str("right"), c.float("multiplier")
	o.Exchange, o.PrimaryExch, o.Currency = c.str("exchange"), c.str("primaryExch"), c.str("currency")
	o.LocalSymbol, o.TradingClass, o.ComboLegs = c.str("localSymbol"), c.str("tradingClass"), len(c.nested["comboLegs"])
	o.ClientID, o.ParentID, o.Action = x.integer("clientId"), x.integer("parentId"), x.str("action")
	o.Quantity, _ = strconv.ParseFloat(strings.TrimSpace(x.str("totalQuantity")), 64)
	o.OrderType, o.LimitPrice, o.AuxPrice = x.str("orderType"), x.float("lmtPrice"), x.float("auxPrice")
	o.TrailingPercent, o.TrailStopPrice, o.TriggerMethod = x.float("trailingPercent"), x.float("trailStopPrice"), x.integer("triggerMethod")
	o.TIF, o.Account, o.OrderRef, o.OcaGroup = x.str("tif"), x.str("account"), x.str("orderRef"), x.str("ocaGroup")
	if oc := x.str("openClose"); oc != "" {
		o.OpenClose = oc[:1]
	}
	o.AlgoStrategy = x.str("algoStrategy")
	for _, entry := range x.nested["algoParams"] {
		if o.AlgoParams == nil {
			o.AlgoParams = map[string]string{}
		}
		o.AlgoParams[entry.str("key")] = entry.str("value")
	}
	o.OutsideRTH, o.WhatIf, o.Transmit = x.flag("outsideRth"), x.flag("whatIf"), x.flag("transmit")
	o.AttachedOrders = len(req.nested["attachedOrders"]) > 0
	return o, nil
}

// cancelOrderID reads a cancelOrder request's order id by the official table.
func cancelOrderID(body []byte) (int, []string) {
	var ignored []string
	req, err := decodeOfficial("CancelOrderRequest", body, "cancelOrder", &ignored)
	if err != nil {
		return 0, append(ignored, "cancelOrder: "+err.Error())
	}
	return req.integer("orderId"), ignored
}

// Frame is one message the client sent, decoded.
type Frame struct {
	At       time.Time
	Session  int // the order the session was accepted in, from 1
	ClientID int
	MsgID    int // the base id; Proto says whether it came as protocol buffers
	Name     string
	Proto    bool
	Fields   []string    // text fields after the id; nil for protocol buffers
	Order    *PlaceOrder // set for placeOrder
	// IgnoredByBroker names the protocol-buffer fields the broker's parser
	// drops (see PlaceOrder.IgnoredByBroker).
	IgnoredByBroker []string
	Raw             []byte
}

// Recorder keeps every message the client sent, decoded, in arrival order.
type Recorder struct {
	mu     sync.Mutex
	frames []Frame
}

func (r *Recorder) add(f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frames = append(r.frames, f)
}

// Frames returns everything recorded so far.
func (r *Recorder) Frames() []Frame {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.frames)
}

// PlaceOrders returns the placeOrder requests recorded so far, previews
// (WhatIf) included.
func (r *Recorder) PlaceOrders() []PlaceOrder {
	var out []PlaceOrder
	for _, f := range r.Frames() {
		if f.Order != nil {
			out = append(out, *f.Order)
		}
	}
	return out
}

// Counts returns how many of each request the client sent, by name.
func (r *Recorder) Counts() map[string]int {
	out := map[string]int{}
	for _, f := range r.Frames() {
		out[f.Name]++
	}
	return out
}

func decodeFrame(body []byte, session, clientID int, at time.Time) Frame {
	f := Frame{At: at, Session: session, ClientID: clientID, Raw: body}
	if len(body) < 4 {
		f.Name = "short"
		return f
	}
	id := int(binary.BigEndian.Uint32(body[:4]))
	payload := body[4:]
	if id > protoOffset {
		f.MsgID, f.Proto = id-protoOffset, true
		switch f.MsgID {
		case placeOrder:
			if o, err := decodePlaceOrder(payload); err == nil {
				f.Order, f.IgnoredByBroker = &o, o.IgnoredByBroker
			} else {
				f.IgnoredByBroker = []string{"placeOrder: " + err.Error()}
			}
		case cancelOrder:
			_, f.IgnoredByBroker = cancelOrderID(payload)
		}
	} else {
		f.MsgID = id
		f.Fields = splitFields(payload)
	}
	f.Name = requestName(f.MsgID)
	return f
}

var requestNames = map[int]string{
	reqMktData: "reqMktData", cancelMktData: "cancelMktData", placeOrder: "placeOrder", cancelOrder: "cancelOrder",
	reqOpenOrders: "reqOpenOrders", reqAccountUpdates: "reqAccountUpdates", reqExecutions: "reqExecutions", reqIDs: "reqIds",
	reqContractDetails: "reqContractDetails", reqAutoOpenOrders: "reqAutoOpenOrders", reqAllOpenOrders: "reqAllOpenOrders",
	reqManagedAccts: "reqManagedAccts", reqHistoricalData: "reqHistoricalData", cancelHistorical: "cancelHistoricalData",
	reqCurrentTime: "reqCurrentTime", reqGlobalCancel: "reqGlobalCancel", reqMarketDataType: "reqMarketDataType",
	reqPositions: "reqPositions", reqAccountSummary: "reqAccountSummary", cancelAccountSumm: "cancelAccountSummary",
	cancelPositions: "cancelPositions", startAPI: "startApi", reqSecDefOptParams: "reqSecDefOptParams",
	reqMatchingSymbols: "reqMatchingSymbols", reqHeadTimestamp: "reqHeadTimestamp", cancelHeadTimestamp: "cancelHeadTimestamp",
	reqMarketRule: "reqMarketRule", reqPnL: "reqPnL", cancelPnL: "cancelPnL", reqCompletedOrders: "reqCompletedOrders",
}

func requestName(id int) string {
	if name, ok := requestNames[id]; ok {
		return name
	}
	return fmt.Sprintf("msg%d", id)
}
