package twssim

import (
	"bufio"
	"fmt"
	"io"
	"maps"
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
	// ChildOnPartialExpiry says what EndDay does with the children of a DAY
	// parent that filled in part: ChildCancelled (the default) or
	// ChildResized.
	ChildOnPartialExpiry string
	// WhatIfDelay holds each WhatIf answer back this long, as a broker's
	// margin simulation takes time (IBKR answers in about 50 to 150 ms).
	WhatIfDelay time.Duration
	// DefinitionDelay holds each contract-details and option-chain answer
	// back this long, each on its own, as the broker answers lookups in
	// parallel and each takes time (IBKR answered option contract details
	// in about one to three seconds a horizon on 2026-11-24).
	DefinitionDelay time.Duration
	// RejectLegOpenClose answers a BAG whose leg carries an open/close
	// instruction (1 open, 2 close) with error 10123, as an account that
	// accepts only Same on combination legs does (assumed).
	RejectLegOpenClose bool
	// ClosedMarkets lists the markets whose session starts closed (see
	// SetSession); every other market starts open.
	ClosedMarkets []string
}

// Server is a simulated TWS listening on a loopback TCP port.
type Server struct {
	ln                 net.Listener
	book               *Book
	now                func() time.Time
	rec                *Recorder
	every              time.Duration
	rejectAttached     bool
	childOnExpiry      string
	whatIfDelay        time.Duration
	definitionDelay    time.Duration
	rejectLegOpenClose bool
	done               chan struct{}
	closeOne           sync.Once
	feeds              quoteFeeds

	mu       sync.Mutex
	sessions []*session
	faults   []Fault
	closed   map[string]bool
	nextID   int // order ids handed out by nextValidId
	// outsideID numbers the orders AddOutsideOrder lists.
	outsideID int
	// beforeWhatIf runs once before the next WhatIf answer is sent.
	beforeWhatIf func()
	accepted     int
	// portfolioUnloaded and portfolioSocketBound model a TWS that has just
	// logged in (see SetPortfolioLoaded).
	portfolioUnloaded    bool
	portfolioSocketBound bool
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
	s := &Server{ln: ln, book: opts.Book, now: opts.Now, rec: opts.Recorder, every: opts.AccountTimeEvery, rejectAttached: opts.RejectAttachedOrders, childOnExpiry: opts.ChildOnPartialExpiry, whatIfDelay: opts.WhatIfDelay, definitionDelay: opts.DefinitionDelay, rejectLegOpenClose: opts.RejectLegOpenClose, closed: map[string]bool{}, done: make(chan struct{}), nextID: 1}
	if s.book == nil {
		s.book = Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.rec == nil {
		s.rec = &Recorder{}
	}
	if s.book.now == nil {
		s.book.now = s.now
	}
	for _, m := range opts.ClosedMarkets {
		s.closed[m] = true
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
	// portfolioWithheld is set on a subscription TWS answered before it had
	// loaded the portfolio: it hears account values but no portfolio rows
	// until it is unsubscribed. sawUnloaded records that this socket
	// subscribed while the portfolio was not loaded.
	portfolioWithheld bool
	sawUnloaded       bool
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
		ss.contractDetails(atoi(field(fs, 1)), contractQueryOf(fs, 2))
	case reqMktData:
		ss.marketData(atoi(field(fs, 1)), contractQueryOf(fs, 2), field(fs, 16) == "1")
	case reqHistoricalData:
		ss.history(fs)
	case reqHeadTimestamp:
		ss.text(msgHeadTimestamp, field(fs, 0), "20100104-14:30:00")
	case reqMarketRule:
		ss.text(msgMarketRule, append([]string{field(fs, 0)}, marketRuleFields(atoi(field(fs, 0)))...)...)
	case reqSecDefOptParams:
		ss.optionParams(atoi(field(fs, 0)), field(fs, 1), atoi(field(fs, 4)))
	case reqMatchingSymbols:
		ss.text(msgSymbolSamples, symbolSampleFields(atoi(field(fs, 0)), ss.srv.book.matchingStocks(field(fs, 1)))...)
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
	if !subscribe {
		ss.accountUpdates, ss.portfolioWithheld = false, false
		s.mu.Unlock()
		return
	}
	if ss.accountUpdates {
		// A subscribe on a live subscription is not a refresh: on 2026-11-21
		// TWS answered none of 21 repeats in ten minutes with a row or an end
		// marker.
		s.mu.Unlock()
		return
	}
	if account != "" && account != s.book.Account() {
		s.mu.Unlock()
		ss.fail(-1, 321, "Error validating request.-'bW' : cause - Invalid account code")
		return
	}
	ss.accountUpdates = true
	if s.portfolioUnloaded {
		ss.sawUnloaded = true
	}
	ss.portfolioWithheld = s.portfolioUnloaded || (s.portfolioSocketBound && ss.sawUnloaded)
	withheld := ss.portfolioWithheld
	s.mu.Unlock()
	ss.send(ss.srv.accountFrames(!withheld)...)
	ss.text(msgAccountDownloadEnd, "1", s.book.Account())
}

// SetPortfolioLoaded says whether the simulated TWS has loaded the account's
// portfolio. While it has not, as a TWS just after its login, a subscription
// is answered with the account values (gross position value included) and an
// end marker but no portfolio rows, and it hears no rows later either.
// Loading the portfolio changes nothing for a live subscription; an
// unsubscribe and a new subscribe, or a new session, downloads the book.
// socketBound is the pessimistic case: the withheld state sticks to every
// socket that subscribed while unloaded, so only a new session gets the book.
// Whether a real TWS answers an unsubscribe and subscribe on the same socket
// with the book is not on record; a new socket got it on 2026-11-24.
func (s *Server) SetPortfolioLoaded(loaded, socketBound bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.portfolioUnloaded = !loaded
	s.portfolioSocketBound = socketBound
}

// accountFrames are the account values, every portfolio row and the account
// time, as reqAccountUpdates streams them.
func (s *Server) accountFrames(withRows bool) [][]byte {
	b := s.book
	b.mu.Lock()
	values := b.accountValuesLocked()
	var rows []portfolioRow
	if withRows {
		rows = b.portfolioLocked()
	}
	b.mu.Unlock()
	var out [][]byte
	for _, v := range values {
		out = append(out, textMessage(msgAccountValue, "2", v.key, v.value, v.currency, b.Account()))
	}
	for _, r := range rows {
		in, p := r.in, r.pos
		value := p.Quantity * in.Price * multiplier(in)
		unreal := value - p.AvgCost*p.Quantity
		strike, mult, exch := "0", "", in.PrimaryExch
		if in.SecType == "OPT" {
			// TWS reports a held option on a concrete exchange; its route is SMART.
			strike, mult, exch = num(in.Strike), num(multiplier(in)), optionPositionExchange
		}
		out = append(out, textMessage(msgPortfolioValue, "8", itoa(in.ConID), in.Symbol, in.SecType, in.Expiry, strike, in.Right, mult, exch, in.Currency, in.LocalSymbol, in.TradingClass,
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

// contractQueryOf reads a request's contract fields from index at: conId,
// symbol, secType, expiry, strike, right, multiplier, exchange, primary
// exchange, currency.
func contractQueryOf(fs []string, at int) contractQuery {
	strike, _ := strconv.ParseFloat(field(fs, at+4), 64)
	return contractQuery{conID: atoi(field(fs, at)), symbol: field(fs, at+1), secType: field(fs, at+2), expiry: field(fs, at+3), strike: strike, right: field(fs, at+5), currency: field(fs, at+9)}
}

func (ss *session) contractDetails(reqID int, q contractQuery) {
	found := ss.srv.book.lookupContract(q)
	ss.afterDefinitionDelay(func() {
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
	})
}

// afterDefinitionDelay answers a lookup now, or after DefinitionDelay on its
// own goroutine, so lookups in flight overlap as the broker's do.
func (ss *session) afterDefinitionDelay(answer func()) {
	if ss.srv.definitionDelay <= 0 {
		answer()
		return
	}
	time.AfterFunc(ss.srv.definitionDelay, answer)
}

// contractDetailsFields is the server-version-203 contractData layout (no
// version field; lastTradeDate, size rules and ineligibility reasons present).
func contractDetailsFields(reqID int, in Instrument) []string {
	orderTypes := "LMT,MKT,STP,STPLMT,TRAIL,TRAILLIMIT,REL"
	valid := in.validExchanges()
	rule := in.MarketRule
	if rule == 0 {
		rule = 26
	}
	// One market rule per valid exchange, in the same order, as TWS lists them.
	rules := strings.TrimSuffix(strings.Repeat(itoa(rule)+",", strings.Count(valid, ",")+1), ",")
	stockType := ""
	if in.SecType == "STK" {
		stockType = "COMMON"
	}
	strike, mult, under, underSym, underType, month := "0", "", "0", in.Symbol, in.SecType, ""
	if in.SecType == "OPT" {
		strike, mult, under, underType, month = num(in.Strike), num(multiplier(in)), itoa(in.UnderConID), "STK", in.Expiry[:6]
		orderTypes = "LMT,MKT,STP,STPLMT,REL,COMBO"
	}
	return []string{itoa(reqID), in.Symbol, in.SecType, in.Expiry, in.Expiry, strike, in.Right, in.Exchange, in.Currency, in.LocalSymbol, in.TradingClass, in.TradingClass, itoa(in.ConID), num(in.MinTick), mult, orderTypes, valid, itoa(max(in.PriceMagnifier, 1)), under, in.LongName, in.PrimaryExch, month, "", "", "", timeZoneOf(in), "", "", "", "0", "0", "1", underSym, underType, rules, in.Expiry, stockType, num(in.lot()), num(in.lot()), num(in.lot()), "0"}
}

// optionPositionExchange is the exchange a held option's portfolio row
// names, as real TWS does (the owner's rows read AMEX); orders route SMART.
const optionPositionExchange = "AMEX"

// optionParams answers reqSecDefOptParams: one row of the underlying's
// chain on SMART (its trading class, multiplier, expiries and strikes), then
// the end; an underlying with no options gets the end alone.
func (ss *session) optionParams(reqID int, symbol string, underConID int) {
	class, expiries, strikes := ss.srv.book.optionChain(underConID, symbol)
	ss.afterDefinitionDelay(func() { ss.sendOptionParams(reqID, underConID, class, expiries, strikes) })
}

func (ss *session) sendOptionParams(reqID, underConID int, class string, expiries []string, strikes []float64) {
	var out [][]byte
	if len(expiries) > 0 {
		fields := []string{itoa(reqID), "SMART", itoa(underConID), class, "100", itoa(len(expiries))}
		fields = append(fields, expiries...)
		fields = append(fields, itoa(len(strikes)))
		for _, k := range strikes {
			fields = append(fields, num(k))
		}
		out = append(out, textMessage(msgSecDefOpt, fields...))
	}
	out = append(out, textMessage(msgSecDefOptEnd, itoa(reqID)))
	ss.send(out...)
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
	// Option ticks: computations for bid, ask, last and the model, and the
	// open interest of calls and puts.
	tickBidOptionComp   = 10
	tickAskOptionComp   = 11
	tickLastOptionComp  = 12
	tickModelOptionComp = 13
	tickOptionCallOI    = 27
	tickOptionPutOI     = 28
)

func (ss *session) marketData(reqID int, cq contractQuery, snapshot bool) {
	found := ss.srv.book.lookupContract(cq)
	if len(found) != 1 {
		ss.fail(reqID, 200, noSecurityDefinition)
		return
	}
	q, feed := ss.srv.feedFor(found[0], quoteOf(found[0]))
	if feed == FeedNone {
		return
	}
	id := itoa(reqID)
	dataType := "1"
	if feed == FeedDelayed {
		dataType = "3"
	}
	// An option whose market is closed answers as IBKR did on 2026-11-24
	// before the US open: frozen (type 2), both sides and the model, no
	// displayed bid or ask size.
	frozen := found[0].SecType == "OPT" && feed != FeedDelayed && !ss.srv.sessionOpen(marketOf(found[0]))
	if frozen {
		dataType = "2"
	}
	out := [][]byte{textMessage(msgMarketDataType, "1", id, dataType)}
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
	if q.bid > 0 && !frozen {
		size(tickBidSize, 100)
		size(tickAskSize, 100)
	}
	size(tickLastSz, 100)
	size(tickVolume, 1_000_000)
	if in := found[0]; in.SecType == "OPT" && feed != FeedNoGreeks {
		m, spot := ss.srv.book.greeks(in)
		size(tickOptionCallOI, 500)
		size(tickOptionPutOI, 500)
		for _, tick := range []int{tickBidOptionComp, tickAskOptionComp, tickLastOptionComp, tickModelOptionComp} {
			out = append(out, textMessage(msgTickOptionComputation, id, itoa(tick), "0", num(optionVolatility), num(roundTo(m.delta, 0.0001)), num(roundTo(m.price, 0.0001)), "0",
				num(roundTo(m.gamma, 0.0001)), num(roundTo(m.vega, 0.0001)), num(roundTo(m.theta, 0.0001)), num(spot)))
		}
	}
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
	if in.SecType == "OPT" {
		w = w.str(4, in.Expiry).double(5, in.Strike).str(6, in.Right).double(7, multiplier(in))
	}
	return w.str(8, exchange).str(9, in.PrimaryExch).str(10, in.Currency).str(11, in.LocalSymbol).str(12, in.TradingClass)
}

func openOrderFrame(o Order, in Instrument, account string, w *whatIf) []byte {
	exchange := o.Exchange
	if exchange == "" {
		exchange = in.Exchange
	}
	contract := contractProto(in, exchange)
	for _, leg := range o.Legs {
		var l protoWriter
		l = l.varint(1, int64(leg.ConID)).varint(2, int64(leg.Ratio)).str(3, leg.Action).str(4, leg.Exchange).varint(5, int64(leg.OpenClose)).varint(6, int64(leg.ShortSalesSlot)).varint(8, int64(leg.ExemptCode))
		contract = protoWriter(contract).bytes(20, l)
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
	if o.OrderType == "TRAIL" && o.TrailStopPrice != 0 {
		// The stop where the trail stands now (MovePrice ratchets it).
		order = order.double(23, o.TrailStopPrice)
	}
	order = order.str(11, o.TIF).str(12, account).boolean(19, o.OutsideRTH).str(27, o.OcaGroup).str(28, o.OrderRef).str(61, o.AlgoStrategy)
	for _, k := range slices.Sorted(maps.Keys(o.AlgoParams)) {
		var entry protoWriter
		order = order.bytes(62, entry.str(1, k).str(2, o.AlgoParams[k]))
	}
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
			if !w.noFee || v.n < 11 {
				state = state.double(v.n, v.v)
			}
		}
		if !w.noFee {
			state = state.str(14, w.commissionCcy)
		}
		state = state.str(15, w.marginCcy)
	}
	var body protoWriter
	body = body.varint(1, int64(o.OrderID)).bytes(2, contract).bytes(3, order).bytes(4, state)
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

// pushAccount streams the account and portfolio to every subscribed session
// after the book changed.
func (s *Server) pushAccount() {
	full, values := s.accountFrames(true), s.accountFrames(false)
	for _, ss := range s.subscribed() {
		s.mu.Lock()
		withheld := ss.portfolioWithheld
		s.mu.Unlock()
		if withheld {
			ss.send(values...)
			continue
		}
		ss.send(full...)
	}
}

func (ss *session) openOrders(all bool) {
	b := ss.srv.book
	var out [][]byte
	for _, o := range b.Orders() {
		if !o.Working() || (!all && o.ClientID != ss.clientID) {
			continue
		}
		out = append(out, openOrderFrame(o, ss.srv.instrumentOf(o.PlaceOrder), b.Account(), nil), orderStatusFrame(o, 0))
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

// timeZoneOf is the time zone id contract details carry for a listing.
func timeZoneOf(in Instrument) string {
	if marketOf(in) == MarketHKHKEX {
		return "Asia/Hong_Kong"
	}
	return "US/Eastern"
}

// MarketRuleHK is the simulator's id for a Hong Kong price grid.
const MarketRuleHK = 4

// marketRuleFields answers reqMarketRule: the count, then each band's low
// edge and increment. Rule MarketRuleHK is the Hong Kong spread table (as
// HKEX publishes it, here with invented ids); every other rule is one cent.
func marketRuleFields(rule int) []string {
	if rule != MarketRuleHK {
		return []string{"1", "0", "0.01"}
	}
	bands := [][2]string{{"0", "0.001"}, {"0.25", "0.005"}, {"0.5", "0.01"}, {"10", "0.02"}, {"20", "0.05"}, {"100", "0.1"}, {"200", "0.2"}, {"500", "0.5"}, {"1000", "1"}, {"2000", "2"}, {"5000", "5"}}
	out := []string{itoa(len(bands))}
	for _, b := range bands {
		out = append(out, b[0], b[1])
	}
	return out
}

// maxSymbolSamples is the most matches reqMatchingSymbols answers, as TWS.
const maxSymbolSamples = 16

// symbolSampleFields is the symbolSamples layout at server version 203
// (EDecoder.processSymbolSamplesMsg): reqId, count, then per contract conId,
// symbol, secType, primary exchange, currency, the derivative sec types
// (count and list), description and issuer id.
func symbolSampleFields(reqID int, found []Instrument) []string {
	out := []string{itoa(reqID), itoa(len(found))}
	for _, in := range found {
		out = append(out, itoa(in.ConID), in.Symbol, in.SecType, in.PrimaryExch, in.Currency, "0", in.LongName, "")
	}
	return out
}
