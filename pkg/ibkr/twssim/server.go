package twssim

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ServerVersion is the TWS API version the simulator speaks: the first with
// protocol-buffer orders, the version pkg/ibkr requires to place one.
const ServerVersion = 203

// Options configure a [Server].
type Options struct {
	// Listen is the loopback address to listen on; empty is 127.0.0.1:0.
	Listen string
	// Book is the broker's state; nil is [Default].
	Book *Book
	// Now is the broker's clock; nil is the wall clock. Fill times, bar dates
	// and the handshake time read it.
	Now func() time.Time
	// Recorder receives every message a client sends; nil keeps a private one.
	Recorder *Recorder
	// AccountTimeEvery is how often a subscribed session hears
	// updateAccountTime; zero is one minute, as the portfolio stream's
	// five-minute freshness needs.
	AccountTimeEvery time.Duration
	// RejectAttachedOrders answers a placeOrder that carries
	// PlaceOrderRequest.4 (attachedOrders) with error 10354, as a TWS whose
	// attached-order feature is off does (read from the 10.50 bytecode; the
	// condition that turns the feature off is not known, so it is an option).
	RejectAttachedOrders bool
}

// Server is a simulated TWS listening on a loopback TCP port.
type Server struct {
	ln             net.Listener
	book           *Book
	now            func() time.Time
	rec            *Recorder
	every          time.Duration
	rejectAttached bool
	done           chan struct{}
	closeOne       sync.Once

	mu       sync.Mutex
	sessions []*session
	nextID   int // order ids handed out by nextValidId
	accepted int
}

// Listen starts a simulator on a loopback address. It refuses any other
// address and IBKR's API ports, where a real client would take it for a TWS.
func Listen(opts Options) (*Server, error) {
	addr := opts.Listen
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if err := CheckLoopbackListen(addr); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, book: opts.Book, now: opts.Now, rec: opts.Recorder, every: opts.AccountTimeEvery, rejectAttached: opts.RejectAttachedOrders, done: make(chan struct{}), nextID: 1}
	if s.book == nil {
		s.book = Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.rec == nil {
		s.rec = &Recorder{}
	}
	if s.every <= 0 {
		s.every = time.Minute
	}
	go s.acceptLoop()
	go s.accountClock()
	return s, nil
}

// Start is [Listen] for a test: it fails the test on error and closes the
// simulator at cleanup.
func Start(tb testing.TB, opts Options) *Server {
	tb.Helper()
	s, err := Listen(opts)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(s.Close)
	return s
}

// LiveAPIPorts are the ports TWS and IB Gateway listen on for live and paper
// API clients. The simulator never listens there, and a harness pointed at
// one refuses to start.
var LiveAPIPorts = []int{4001, 4002, 7496, 7497}

// CheckLoopbackListen refuses an address that is not loopback or that names
// one of [LiveAPIPorts].
func CheckLoopbackListen(addr string) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("twssim: listen %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("twssim: listen %q: the simulator serves loopback only", addr)
	}
	if port, err := strconv.Atoi(portText); err == nil && slices.Contains(LiveAPIPorts, port) {
		return fmt.Errorf("twssim: listen %q: %d is an IBKR API port", addr, port)
	}
	return nil
}

// Addr is the host:port the simulator listens on.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Port is the TCP port the simulator listens on.
func (s *Server) Port() int { return s.ln.Addr().(*net.TCPAddr).Port }

// Book is the simulator's state.
func (s *Server) Book() *Book { return s.book }

// Recorder holds every message the clients sent.
func (s *Server) Recorder() *Recorder { return s.rec }

// Close stops listening and ends every session.
func (s *Server) Close() {
	s.closeOne.Do(func() {
		close(s.done)
		_ = s.ln.Close()
		s.mu.Lock()
		sessions := slices.Clone(s.sessions)
		s.mu.Unlock()
		for _, ss := range sessions {
			_ = ss.c.Close()
		}
	})
}

// Sessions is the number of API sessions currently connected.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

type session struct {
	srv      *Server
	c        net.Conn
	n        int
	clientID int
	wmu      sync.Mutex
	// accountUpdates is set while the client subscribes to reqAccountUpdates.
	accountUpdates bool
}

func (ss *session) send(bodies ...[]byte) {
	var buf []byte
	for _, b := range bodies {
		buf = appendFrame(buf, b)
	}
	ss.wmu.Lock()
	defer ss.wmu.Unlock()
	_, _ = ss.c.Write(buf)
}

func (ss *session) text(id int, fields ...string) { ss.send(textMessage(id, fields...)) }

// fail sends an error, as protocol buffers (ErrorMessage) the way TWS does
// from server version 203.
func (ss *session) fail(reqID, code int, message string) {
	var w protoWriter
	w = w.varint(1, int64(reqID)).varint(2, ss.srv.now().UnixMilli()).varint(3, int64(code)).str(4, message)
	ss.send(protoMessage(msgError, w))
}

func (s *Server) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepted++
		ss := &session{srv: s, c: c, n: s.accepted, clientID: -1}
		s.sessions = append(s.sessions, ss)
		s.mu.Unlock()
		go ss.serve()
	}
}

func (s *Server) drop(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = slices.DeleteFunc(s.sessions, func(x *session) bool { return x == ss })
}

// accountClock sends updateAccountTime to every subscribed session, as TWS
// does while account updates stream.
func (s *Server) accountClock() {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		for _, ss := range s.subscribed() {
			ss.text(msgAccountTime, "1", s.now().Format("15:04"))
		}
	}
}

func (s *Server) subscribed() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*session
	for _, ss := range s.sessions {
		if ss.accountUpdates {
			out = append(out, ss)
		}
	}
	return out
}

func (ss *session) serve() {
	defer ss.srv.drop(ss)
	defer ss.c.Close()
	r := bufio.NewReader(ss.c)
	prefix := make([]byte, 4)
	if _, err := io.ReadFull(r, prefix); err != nil || string(prefix) != "API\x00" {
		return
	}
	if _, err := readFrame(r); err != nil { // the client's version range
		return
	}
	ss.send(fmt.Appendf(nil, "%d\x00%s\x00", ServerVersion, ss.srv.now().Format("20060102 15:04:05 MST")))
	for {
		body, err := readFrame(r)
		if err != nil {
			return
		}
		f := decodeFrame(body, ss.n, ss.clientID, ss.srv.now())
		ss.srv.rec.add(f)
		if f.MsgID == startAPI && !f.Proto {
			if !ss.start(f.Fields) {
				return
			}
			continue
		}
		ss.dispatch(f)
	}
}

// start answers startAPI the way TWS does: a client id already in use gets
// error 326 and the socket is closed; otherwise the startup burst ends with
// the next order id and the managed account.
func (ss *session) start(fields []string) bool {
	if len(fields) > 1 {
		ss.clientID, _ = strconv.Atoi(fields[1])
	}
	s := ss.srv
	s.mu.Lock()
	for _, other := range s.sessions {
		if other != ss && other.clientID == ss.clientID {
			s.mu.Unlock()
			ss.fail(-1, 326, "Unable to connect as the client id is already in use. Retry with a unique client id.")
			return false
		}
	}
	next := s.nextID
	s.mu.Unlock()
	ss.text(msgNextValidID, "1", itoa(next))
	ss.text(msgManagedAccounts, "1", s.book.Account())
	ss.fail(-1, 2104, "Market data farm connection is OK:usfarm")
	ss.fail(-1, 2106, "HMDS data farm connection is OK:ushmds")
	ss.fail(-1, 2158, "Sec-def data farm connection is OK:secdefil")
	return true
}

func field(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

func atoi(s string) int {
	v, _ := strconv.Atoi(strings.TrimSpace(s))
	return v
}

func (ss *session) dispatch(f Frame) {
	s, fs := ss.srv, f.Fields
	if f.Proto {
		switch f.MsgID {
		case placeOrder:
			if f.Order == nil {
				ss.fail(-1, 320, "Error reading request: placeOrder could not be decoded")
				return
			}
			ss.place(*f.Order)
		case cancelOrder:
			id, _ := cancelOrderID(f.Raw[4:])
			ss.cancel(id)
		default:
			ss.fail(-1, 503, "The TWS is out of date and must be upgraded: "+f.Name)
		}
		return
	}
	switch f.MsgID {
	case reqCurrentTime:
		ss.text(msgCurrentTime, "1", strconv.FormatInt(s.now().Unix(), 10))
	case reqIDs:
		s.mu.Lock()
		next := s.nextID
		s.mu.Unlock()
		ss.text(msgNextValidID, "1", itoa(next))
	case reqManagedAccts:
		ss.text(msgManagedAccounts, "1", s.book.Account())
	case reqMarketDataType, cancelMktData, cancelHistorical, cancelAccountSumm, cancelPositions, cancelPnL, cancelHeadTimestamp, reqAutoOpenOrders:
		// No answer: TWS confirms none of these.
	case reqAccountUpdates:
		ss.accountUpdatesRequest(field(fs, 1) == "1", field(fs, 2))
	case reqAccountSummary:
		ss.accountSummary(atoi(field(fs, 1)), strings.Split(field(fs, 3), ","))
	case reqPnL:
		ss.pnl(atoi(field(fs, 0)))
	case reqPnLSingle:
		ss.pnlSingle(atoi(field(fs, 0)), atoi(field(fs, 3)))
	case reqWSHMetaData, reqWSHEventData:
		// The simulated account has no Wall Street Horizon subscription.
		ss.fail(atoi(field(fs, 0)), 10276, "News feed is not allowed")
	case cancelWSHMetaData, cancelWSHEventData, cancelPnLSingle:
	case reqPositions:
		ss.positions()
	case reqContractDetails:
		ss.contractDetails(atoi(field(fs, 1)), atoi(field(fs, 2)), field(fs, 3), field(fs, 4), field(fs, 11))
	case reqMktData:
		ss.marketData(atoi(field(fs, 1)), atoi(field(fs, 2)), field(fs, 3), field(fs, 4), field(fs, 11), field(fs, 16) == "1")
	case reqHistoricalData:
		ss.history(fs)
	case reqHeadTimestamp:
		ss.text(msgHeadTimestamp, field(fs, 0), "20100104-14:30:00")
	case reqMarketRule:
		ss.text(msgMarketRule, field(fs, 0), "1", "0", "0.01")
	case reqSecDefOptParams:
		// The simulator lists no option chains.
		ss.text(msgSecDefOptEnd, field(fs, 0))
	case reqMatchingSymbols:
		ss.text(msgSymbolSamples, field(fs, 0), "0")
	case reqAllOpenOrders, reqOpenOrders:
		ss.openOrders(f.MsgID == reqAllOpenOrders)
	case reqCompletedOrders:
		ss.text(msgCompletedOrdersEnd)
	case reqExecutions:
		ss.executions(atoi(field(fs, 1)))
	case reqGlobalCancel:
		for _, o := range s.book.Orders() {
			ss.cancel(o.OrderID)
		}
	default:
		// An unmodelled request is answered, never left hanging, so a reader
		// waiting on it ends at once and the session stays healthy.
		reqID := -1
		if len(fs) > 1 {
			if v, err := strconv.Atoi(fs[1]); err == nil {
				reqID = v
			}
		}
		ss.fail(reqID, 10000+f.MsgID, "twssim: request "+f.Name+" is not simulated")
	}
}

func (ss *session) accountUpdatesRequest(subscribe bool, account string) {
	s := ss.srv
	s.mu.Lock()
	ss.accountUpdates = subscribe
	s.mu.Unlock()
	if !subscribe {
		return
	}
	if account != "" && account != s.book.Account() {
		ss.fail(-1, 321, "Error validating request.-'bW' : cause - Invalid account code")
		return
	}
	ss.send(ss.srv.accountFrames()...)
	ss.text(msgAccountDownloadEnd, "1", s.book.Account())
}

// accountFrames are the account values, every portfolio row and the account
// time, as reqAccountUpdates streams them.
func (s *Server) accountFrames() [][]byte {
	b := s.book
	b.mu.Lock()
	values := b.accountValuesLocked()
	rows := b.portfolioLocked()
	b.mu.Unlock()
	var out [][]byte
	for _, v := range values {
		out = append(out, textMessage(msgAccountValue, "2", v.key, v.value, v.currency, b.Account()))
	}
	for _, r := range rows {
		in, p := r.in, r.pos
		value := p.Quantity * in.Price
		unreal := (in.Price - p.AvgCost) * p.Quantity
		out = append(out, textMessage(msgPortfolioValue, "8", itoa(in.ConID), in.Symbol, in.SecType, "", "0", "", "", in.PrimaryExch, in.Currency, in.LocalSymbol, in.TradingClass,
			num(p.Quantity), num(in.Price), num2(value), num(p.AvgCost), num2(unreal), "0", b.Account()))
	}
	out = append(out, textMessage(msgAccountTime, "1", s.now().Format("15:04")))
	return out
}

func (ss *session) accountSummary(reqID int, tags []string) {
	b := ss.srv.book
	b.mu.Lock()
	rows := b.summaryRowsLocked(tags)
	b.mu.Unlock()
	var out [][]byte
	for _, r := range rows {
		out = append(out, textMessage(msgAccountSummary, "1", itoa(reqID), b.Account(), r.key, r.value, r.currency))
	}
	out = append(out, textMessage(msgAccountSummaryEnd, "1", itoa(reqID)))
	ss.send(out...)
}

func (ss *session) pnl(reqID int) {
	b := ss.srv.book
	b.mu.Lock()
	f := b.figuresLocked()
	b.mu.Unlock()
	ss.text(msgPnL, itoa(reqID), "0", num2(f.unrealized), "0")
}

func (ss *session) pnlSingle(reqID, conID int) {
	b := ss.srv.book
	b.mu.Lock()
	p, held := b.pos[conID]
	in := b.instr[conID]
	b.mu.Unlock()
	if !held || in == nil {
		ss.text(msgPnLSingle, itoa(reqID), "0", "0", "0", "0", "0")
		return
	}
	ss.text(msgPnLSingle, itoa(reqID), num(p.Quantity), "0", num2((in.Price-p.AvgCost)*p.Quantity), "0", num2(p.Quantity*in.Price))
}

func (ss *session) positions() {
	b := ss.srv.book
	b.mu.Lock()
	rows := b.portfolioLocked()
	b.mu.Unlock()
	var out [][]byte
	for _, r := range rows {
		out = append(out, textMessage(msgPosition, "3", b.Account(), itoa(r.in.ConID), r.in.Symbol, r.in.SecType, "", "0", "", "", r.in.Exchange, r.in.Currency, r.in.LocalSymbol, r.in.TradingClass, num(r.pos.Quantity), num(r.pos.AvgCost)))
	}
	out = append(out, textMessage(msgPositionEnd, "1"))
	ss.send(out...)
}

const noSecurityDefinition = "No security definition has been found for the request"

func (ss *session) contractDetails(reqID, conID int, symbol, secType, currency string) {
	found := ss.srv.book.lookup(conID, symbol, secType, currency)
	if len(found) == 0 {
		ss.fail(reqID, 200, noSecurityDefinition)
		return
	}
	var out [][]byte
	for _, in := range found {
		out = append(out, textMessage(msgContractDetails, contractDetailsFields(reqID, in)...))
	}
	out = append(out, textMessage(msgContractDetailsEnd, "1", itoa(reqID)))
	ss.send(out...)
}

// contractDetailsFields is the server-version-203 contractData layout (no
// version field; lastTradeDate, size rules and ineligibility reasons present).
func contractDetailsFields(reqID int, in Instrument) []string {
	orderTypes := "LMT,MKT,STP,STPLMT,TRAIL,TRAILLIMIT,REL"
	valid := "SMART," + in.PrimaryExch
	if in.SecType != "STK" {
		valid = in.Exchange
	}
	stockType := ""
	if in.SecType == "STK" {
		stockType = "COMMON"
	}
	return []string{itoa(reqID), in.Symbol, in.SecType, "", "", "0", "", in.Exchange, in.Currency, in.LocalSymbol, in.TradingClass, in.TradingClass, itoa(in.ConID), num(in.MinTick), "", orderTypes, valid, "1", "0", in.LongName, in.PrimaryExch, "", "", "", "", "US/Eastern", "", "", "", "0", "0", "1", in.Symbol, in.SecType, "26", "", stockType, "1", "1", "1", "0"}
}

// Tick types of the TWS API.
const (
	tickBidSize = 0
	tickBid     = 1
	tickAsk     = 2
	tickAskSize = 3
	tickLast    = 4
	tickLastSz  = 5
	tickVolume  = 8
	tickClose   = 9
)

func (ss *session) marketData(reqID, conID int, symbol, secType, currency string, snapshot bool) {
	found := ss.srv.book.lookup(conID, symbol, secType, currency)
	if len(found) != 1 {
		ss.fail(reqID, 200, noSecurityDefinition)
		return
	}
	q := quoteOf(found[0])
	id := itoa(reqID)
	out := [][]byte{textMessage(msgMarketDataType, "1", id, "1")}
	price := func(tick int, v float64) {
		if v > 0 {
			out = append(out, textMessage(msgTickPrice, "6", id, itoa(tick), num(v), "100", "0"))
		}
	}
	size := func(tick int, v float64) {
		out = append(out, textMessage(msgTickSize, "6", id, itoa(tick), num(v)))
	}
	price(tickBid, q.bid)
	price(tickAsk, q.ask)
	price(tickLast, q.last)
	price(tickClose, q.close)
	if q.bid > 0 {
		size(tickBidSize, 100)
		size(tickAskSize, 100)
	}
	size(tickLastSz, 100)
	size(tickVolume, 1_000_000)
	if snapshot {
		out = append(out, textMessage(msgTickSnapshotEnd, "1", id))
	}
	ss.send(out...)
}

// history answers reqHistoricalData with daily bars ending on the broker
// clock's last completed weekday: a deterministic path that drifts up gently
// and settles on the contract's price.
func (ss *session) history(fs []string) {
	reqID := atoi(field(fs, 0))
	found := ss.srv.book.lookup(atoi(field(fs, 1)), field(fs, 2), field(fs, 3), field(fs, 10))
	if len(found) != 1 {
		ss.fail(reqID, 200, noSecurityDefinition)
		return
	}
	in := found[0]
	duration := ""
	for i := 14; i+1 < len(fs); i++ {
		if barSize(fs[i]) {
			duration = fs[i+1]
			break
		}
	}
	days := durationDays(duration)
	end := ss.srv.now().UTC()
	var dates []time.Time
	for d := end.AddDate(0, 0, -1); len(dates) < days*5/7+1 && d.After(end.AddDate(0, 0, -days-1)); d = d.AddDate(0, 0, -1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			dates = append(dates, d)
		}
	}
	slices.Reverse(dates)
	fields := []string{itoa(reqID), itoa(len(dates))}
	for i, d := range dates {
		k := float64(len(dates) - 1 - i) // trading days before the last bar
		closeP := roundTo(in.Price/(1+0.0003*k)*(1+0.004*math.Sin(k/5)), in.MinTick)
		openP := roundTo(closeP*(1-0.001*math.Cos(k/3)), in.MinTick)
		high := roundTo(max(openP, closeP)*1.004, in.MinTick)
		low := roundTo(min(openP, closeP)*0.996, in.MinTick)
		fields = append(fields, d.Format("20060102"), num(openP), num(high), num(low), num(closeP), "1000000", num(closeP), "1000")
	}
	first, last := "", ""
	if len(dates) > 0 {
		first, last = dates[0].Format("20060102"), dates[len(dates)-1].Format("20060102")
	}
	ss.send(textMessage(msgHistoricalData, fields...), textMessage(msgHistoricalDataEnd, itoa(reqID), first, last))
}

func barSize(s string) bool {
	for _, unit := range []string{" sec", " min", " hour", " day", " week", " month"} {
		if strings.Contains(s, unit) {
			return true
		}
	}
	return false
}

// durationDays reads a TWS duration ("30 D", "2 W", "6 M", "1 Y") as calendar
// days, at most ten years.
func durationDays(s string) int {
	n, unit, _ := strings.Cut(strings.TrimSpace(s), " ")
	v := max(atoi(n), 1)
	switch strings.TrimSpace(unit) {
	case "W":
		v *= 7
	case "M":
		v *= 31
	case "Y":
		v *= 366
	case "S":
		v = 1
	}
	return min(v, 3660)
}

// Order frames. The protocol-buffer field numbers are IBKR's Contract.proto,
// Order.proto, OrderState.proto, OrderStatus.proto and Execution.proto.
func contractProto(in Instrument, exchange string) []byte {
	var w protoWriter
	w = w.varint(1, int64(in.ConID)).str(2, in.Symbol).str(3, in.SecType)
	return w.str(8, exchange).str(9, in.PrimaryExch).str(10, in.Currency).str(11, in.LocalSymbol).str(12, in.TradingClass)
}

func openOrderFrame(o Order, in Instrument, account string, w *whatIf) []byte {
	exchange := o.Exchange
	if exchange == "" {
		exchange = in.Exchange
	}
	var order protoWriter
	order = order.varint(1, int64(o.ClientID)).varint(2, int64(o.OrderID)).varint(3, int64(o.PermID))
	if o.ParentID > 0 {
		order = order.varint(4, int64(o.ParentID))
	}
	order = order.str(5, o.Action).str(6, num(o.Quantity)).str(8, o.OrderType)
	if o.LimitPrice != 0 || o.OrderType == "LMT" {
		order = order.double(9, o.LimitPrice)
	}
	if o.AuxPrice != 0 {
		order = order.double(10, o.AuxPrice)
	}
	order = order.str(11, o.TIF).str(12, account).boolean(19, o.OutsideRTH).str(27, o.OcaGroup).str(28, o.OrderRef).str(61, o.AlgoStrategy)
	order = order.boolean(65, w != nil).boolean(66, true)
	var state protoWriter
	state = state.str(1, o.Status)
	if w != nil {
		for _, v := range []struct {
			n int
			v float64
		}{
			{2, w.initBefore}, {3, w.maintBefore}, {4, w.equityBefore},
			{5, w.initAfter - w.initBefore}, {6, w.maintAfter - w.maintBefore}, {7, w.equityAfter - w.equityBefore},
			{8, w.initAfter}, {9, w.maintAfter}, {10, w.equityAfter},
			{11, w.commission}, {12, w.commission}, {13, w.commission},
		} {
			state = state.double(v.n, v.v)
		}
		state = state.str(14, w.commissionCcy).str(15, w.marginCcy)
	}
	var body protoWriter
	body = body.varint(1, int64(o.OrderID)).bytes(2, contractProto(in, exchange)).bytes(3, order).bytes(4, state)
	return protoMessage(msgOpenOrder, body)
}

func orderStatusFrame(o Order, lastFill float64) []byte {
	var w protoWriter
	w = w.varint(1, int64(o.OrderID)).str(2, o.Status).str(3, num(o.Filled)).str(4, num(o.Remaining())).double(5, o.AvgPrice).varint(6, int64(o.PermID))
	w = w.varint(7, int64(o.ParentID)).double(8, lastFill).varint(9, int64(o.ClientID))
	return protoMessage(msgOrderStatus, w)
}

func execFrame(reqID int, ex Execution) []byte {
	var e protoWriter
	e = e.varint(1, int64(ex.OrderID)).str(2, ex.ExecID).str(3, ex.Time.UTC().Format("20060102-15:04:05")).str(4, ex.Account)
	e = e.str(5, ex.Exchange).str(6, ex.Side).str(7, num(ex.Shares)).double(8, ex.Price).varint(9, int64(ex.PermID)).varint(10, int64(ex.ClientID))
	e = e.str(12, num(ex.CumQty)).double(13, ex.AvgPrice).str(14, ex.OrderRef)
	var body protoWriter
	body = body.varint(1, int64(reqID)).bytes(2, contractProto(ex.Contract, ex.Exchange)).bytes(3, e)
	return protoMessage(msgExecDetails, body)
}

func commissionFrame(ex Execution) []byte {
	return textMessage(msgCommissionReport, "1", ex.ExecID, num(ex.Fee), ex.FeeCcy, "1.7976931348623157E308", "1.7976931348623157E308", "")
}

func (ss *session) place(o PlaceOrder) {
	s, b := ss.srv, ss.srv.book
	in, ok := b.instrument(o.ConID)
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
	if o.WhatIf {
		b.mu.Lock()
		w := b.whatIfLocked(o, in)
		b.mu.Unlock()
		order := Order{PlaceOrder: o, Status: "PreSubmitted"}
		order.ClientID = ss.clientID
		ss.send(openOrderFrame(order, in, b.Account(), &w))
		return
	}
	order, ex := b.accept(o, ss.clientID, s.now())
	if order.Status == "Inactive" {
		ss.fail(o.OrderID, 201, "Order rejected - reason:twssim cannot route this order")
		ss.send(orderStatusFrame(*order, 0))
		return
	}
	if ex == nil {
		ss.send(openOrderFrame(*order, in, b.Account(), nil), orderStatusFrame(*order, 0))
		return
	}
	// A marketable order is acknowledged as working, then fills.
	working := *order
	working.Status, working.Filled, working.AvgPrice = "Submitted", 0, 0
	ss.send(openOrderFrame(working, in, b.Account(), nil), orderStatusFrame(working, 0))
	ss.send(execFrame(-1, *ex), openOrderFrame(*order, in, b.Account(), nil), orderStatusFrame(*order, ex.Price), commissionFrame(*ex))
	s.pushAccount()
}

// pushAccount streams the account and portfolio to every subscribed session
// after the book changed.
func (s *Server) pushAccount() {
	frames := s.accountFrames()
	for _, ss := range s.subscribed() {
		ss.send(frames...)
	}
}

func (ss *session) cancel(orderID int) {
	o, ok := ss.srv.book.cancel(orderID)
	if !ok {
		ss.fail(orderID, 10147, fmt.Sprintf("OrderId %d that needs to be cancelled is not found.", orderID))
		return
	}
	ss.send(orderStatusFrame(o, 0))
	ss.fail(orderID, 202, "Order Canceled - reason:")
}

func (ss *session) openOrders(all bool) {
	b := ss.srv.book
	var out [][]byte
	for _, o := range b.Orders() {
		if !o.Working() || (!all && o.ClientID != ss.clientID) {
			continue
		}
		in, _ := b.instrument(o.ConID)
		out = append(out, openOrderFrame(o, in, b.Account(), nil), orderStatusFrame(o, 0))
	}
	out = append(out, protoMessage(msgOpenOrderEnd, nil))
	ss.send(out...)
}

func (ss *session) executions(reqID int) {
	var out [][]byte
	for _, ex := range ss.srv.book.Executions() {
		out = append(out, execFrame(reqID, ex), commissionFrame(ex))
	}
	var end protoWriter
	end = end.varint(1, int64(reqID))
	out = append(out, protoMessage(msgExecDetailsEnd, end))
	ss.send(out...)
}
