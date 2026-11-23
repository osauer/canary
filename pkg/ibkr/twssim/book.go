package twssim

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Instrument is one contract the simulated broker knows. Every figure is
// invented; Price is the last trade the deterministic quote is built around.
type Instrument struct {
	ConID        int
	Symbol       string
	SecType      string // STK, IND, CASH or OPT
	Exchange     string // the routing exchange the broker quotes, SMART for stock
	PrimaryExch  string
	Currency     string
	LocalSymbol  string
	TradingClass string
	LongName     string
	MinTick      float64
	Price        float64
	// Spread is the bid/ask width around Price; zero quotes the last only, as
	// an index does.
	Spread float64
	// Bid and Ask, when set, are quoted as they are instead of from Spread.
	Bid, Ask float64
	// An option's expiry (YYYYMMDD), strike, right (C or P), multiplier and
	// underlying; zero for other contracts (a zero multiplier counts as 1).
	Expiry     string
	Strike     float64
	Right      string
	Multiplier float64
	UnderConID int
	// Defunct is a dead line the account still holds: its row streams with
	// value 0, and every contract or quote request for it is answered "No
	// security definition", as IBKR answers for a delisted name (assumed).
	Defunct bool
	// PriceMagnifier is TWS's quote scale (100 for an LSE line quoted in
	// pence); zero counts as 1.
	PriceMagnifier int
	// SizeIncrement is the listing's board lot: contract details carry it as
	// minSize, sizeIncrement and suggestedSizeIncrement, and an order off a
	// multiple of it is rejected (assumed); zero counts as 1.
	SizeIncrement float64
	// MarketRule is the price-grid rule contract details name for every valid
	// exchange; zero is rule 26, one cent.
	MarketRule int
}

// lot is the board lot, at least 1.
func (in Instrument) lot() float64 { return max(in.SizeIncrement, 1) }

// validExchanges are the exchanges contract details list: SMART and the
// primary for a SMART-routed stock, the listing's own exchange alone for one
// that is not (an SEHK line), the routing exchange for anything else.
func (in Instrument) validExchanges() string {
	switch {
	case in.SecType == "OPT":
		return "SMART,AMEX,CBOE,ISE"
	case in.SecType == "STK" && in.Exchange == "SMART":
		return "SMART," + in.PrimaryExch
	default:
		return in.Exchange
	}
}

// Position is one holding of the book's account.
type Position struct {
	ConID    int
	Quantity float64
	AvgCost  float64
}

// Order is one order the broker holds, as the client sent it and as the
// broker's engine has moved it since.
type Order struct {
	PlaceOrder
	PermID   int
	Status   string // PreSubmitted, Submitted, Filled, Cancelled, Inactive
	Filled   float64
	AvgPrice float64
	Placed   time.Time
}

// Remaining is the quantity not yet filled.
func (o Order) Remaining() float64 { return max(0, o.Quantity-o.Filled) }

// Working reports whether the order is live at the exchange (an
// untransmitted order is held at the broker and is not).
func (o Order) Working() bool {
	return o.Status == "PreSubmitted" || o.Status == "Submitted"
}

// Execution is one fill.
type Execution struct {
	ExecID   string
	OrderID  int
	PermID   int
	ClientID int
	ConID    int
	Side     string // BOT or SLD
	Shares   float64
	Price    float64
	CumQty   float64
	AvgPrice float64
	Time     time.Time
	OrderRef string
	Fee      float64
	FeeCcy   string
	Account  string
	Exchange string
	Contract Instrument
}

// Book is the simulated broker's state: one paper account with cash per
// currency, positions, the contract catalogue, orders and executions. It is
// safe for concurrent use; the server and a test share it.
type Book struct {
	mu sync.Mutex

	account string
	base    string
	cash    map[string]float64 // native cash per currency
	fx      map[string]float64 // base per unit of the currency
	pos     map[int]*Position
	instr   map[int]*Instrument
	orders  map[int]*Order // by order id
	execs   []Execution
	permID  int
	execSeq int

	// now is the clock options are priced at; nil is the wall clock (a
	// server sets its own).
	now func() time.Time
	// feePerShare and feeMin price the commission (IBKR fixed-style tiers are
	// not modelled).
	feePerShare, feeMin float64
}

// NewBook returns an empty book for a paper account in the base currency.
func NewBook(account, base string) *Book {
	return &Book{
		account: account, base: base,
		cash: map[string]float64{base: 0}, fx: map[string]float64{base: 1},
		pos: map[int]*Position{}, instr: map[int]*Instrument{}, orders: map[int]*Order{},
		permID: 1_000_000, feePerShare: 0.005, feeMin: 1,
	}
}

// DefaultAccount is the account of [Default]: IBKR's documentation-style
// placeholder for a live account, never a real one.
const DefaultAccount = "U1234567"

// Default is the synthetic book every test and canary-sim start from: a USD
// account holding 100 SYNA bought at 48, with 100,000 USD cash, and the
// instruments the daemon's regime reads by name. Every figure is invented.
func Default() *Book {
	b := NewBook(DefaultAccount, "USD")
	b.SetCash("USD", 100_000)
	for _, in := range []Instrument{
		{ConID: 900001, Symbol: "SYNA", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", TradingClass: "NMS", LongName: "SYNTHETIC A INC", MinTick: 0.01, Price: 50, Spread: 0.02},
		{ConID: 900002, Symbol: "SYNB", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD", TradingClass: "SYNB", LongName: "SYNTHETIC B CORP", MinTick: 0.01, Price: 120, Spread: 0.04},
		// The regime's named inputs, at invented calm levels, under IBKR's
		// public contract ids, which the daemon seeds its contract cache with.
		{ConID: 756733, Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD", TradingClass: "SPY", LongName: "SYNTHETIC INDEX FUND", MinTick: 0.01, Price: 600, Spread: 0.02},
		{ConID: 43652089, Symbol: "HYG", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD", TradingClass: "HYG", LongName: "SYNTHETIC HIGH YIELD FUND", MinTick: 0.01, Price: 80, Spread: 0.02},
		// A synthetic index fund for index protection (the simulator's
		// Rulebook lists it as a hedge symbol), at the price the hedge recipe
		// in the guide assumes.
		{ConID: 900601, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD", TradingClass: "SYNX", LongName: "SYNTHETIC INDEX FUND X", MinTick: 0.01, Price: 600, Spread: 0.02},
		{ConID: 13455763, Symbol: "VIX", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC VOLATILITY INDEX", MinTick: 0.01, Price: 15},
		{ConID: 47511905, Symbol: "VIX3M", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC 3M VOLATILITY INDEX", MinTick: 0.01, Price: 19.4},
		{ConID: 416904, Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC 500 INDEX", MinTick: 0.01, Price: 6000},
		{ConID: 900203, Symbol: "VVIX", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC VOL OF VOL INDEX", MinTick: 0.01, Price: 88},
		{ConID: 15016059, Symbol: "USD", SecType: "CASH", Exchange: "IDEALPRO", Currency: "JPY", LocalSymbol: "USD.JPY", TradingClass: "USD.JPY", LongName: "SYNTHETIC USD.JPY", MinTick: 0.005, Price: 150, Spread: 0.01},
		// A Hong Kong listing not routed on SMART, in board lots of 100 on
		// the Hong Kong price grid, and its ADR on NYSE under the same company
		// name. The account holds no HKD until a buy or `ctl cash HKD`.
		{ConID: 900701, Symbol: "SYNH", SecType: "STK", Exchange: "SEHK", PrimaryExch: "SEHK", Currency: "HKD", LocalSymbol: "9901", TradingClass: "9901", LongName: "SYNTHETIC H HOLDINGS LTD", MinTick: 0.05, Price: 82.5, Spread: 0.1, SizeIncrement: 100, MarketRule: MarketRuleHK},
		{ConID: 900702, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD", TradingClass: "SYNH", LongName: "SYNTHETIC H HOLDINGS LTD", MinTick: 0.01, Price: 52.9, Spread: 0.02},
		{ConID: 900703, Symbol: "USD", SecType: "CASH", Exchange: "IDEALPRO", Currency: "HKD", LocalSymbol: "USD.HKD", TradingClass: "USD.HKD", LongName: "SYNTHETIC USD.HKD", MinTick: 0.00005, Price: 7.8, Spread: 0.0002},
	} {
		b.AddInstrument(in)
	}
	b.SetFX("HKD", 1/7.8)
	b.SetPosition(900001, 100, 48)
	return b
}

// matchingStocks answers reqMatchingSymbols from the catalogue: stocks whose
// symbol or company name starts with pattern (case-insensitive), at most
// maxSymbolSamples, by contract id.
func (b *Book) matchingStocks(pattern string) []Instrument {
	pattern = strings.ToUpper(strings.TrimSpace(pattern))
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Instrument
	if pattern == "" {
		return out
	}
	for _, in := range b.instr {
		if in.SecType == "STK" && !in.Defunct && (strings.HasPrefix(in.Symbol, pattern) || strings.HasPrefix(strings.ToUpper(in.LongName), pattern)) {
			out = append(out, *in)
		}
	}
	slices.SortFunc(out, func(x, y Instrument) int { return x.ConID - y.ConID })
	return out[:min(len(out), maxSymbolSamples)]
}

// EuropeanBook is [Default] with a EUR line on Xetra (SYNE, IBIS) and a GBP
// line on the LSE (SYNG, quoted in pence: price magnifier 100), native cash
// in both and their USD rates, with the IDEALPRO pairs listed.
func EuropeanBook() *Book {
	b := Default()
	b.SetCash("EUR", 20_000)
	b.SetFX("EUR", 1.08)
	b.SetCash("GBP", 15_000)
	b.SetFX("GBP", 1.27)
	b.AddInstrument(Instrument{ConID: 900401, Symbol: "SYNE", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR", TradingClass: "XETR", LongName: "SYNTHETIC E AG", MinTick: 0.01, Price: 40, Spread: 0.02})
	b.AddInstrument(Instrument{ConID: 900402, Symbol: "SYNG", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSE", Currency: "GBP", TradingClass: "SYNG", LongName: "SYNTHETIC G PLC", MinTick: 0.1, Price: 250, Spread: 0.5, PriceMagnifier: 100})
	b.AddInstrument(Instrument{ConID: 900501, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD", LocalSymbol: "EUR.USD", TradingClass: "EUR.USD", MinTick: 0.00005, Price: 1.08, Spread: 0.0001})
	b.AddInstrument(Instrument{ConID: 900502, Symbol: "GBP", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD", LocalSymbol: "GBP.USD", TradingClass: "GBP.USD", MinTick: 0.00005, Price: 1.27, Spread: 0.0001})
	return b
}

// Account is the account code the book answers for.
func (b *Book) Account() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.account
}

// SetAccount changes the account code the broker announces and answers for,
// as a login to another account would.
func (b *Book) SetAccount(account string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.account = account
}

// Base is the account's base currency.
func (b *Book) Base() string { return b.base }

// AddInstrument lists a contract; LocalSymbol and TradingClass default to the
// symbol.
func (b *Book) AddInstrument(in Instrument) {
	if in.LocalSymbol == "" {
		in.LocalSymbol = in.Symbol
	}
	if in.TradingClass == "" {
		in.TradingClass = in.Symbol
	}
	if in.MinTick == 0 {
		in.MinTick = 0.01
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.instr[in.ConID] = &in
}

// SetCash sets the native cash of a currency.
func (b *Book) SetCash(currency string, amount float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cash[currency] = amount
	if _, ok := b.fx[currency]; !ok {
		b.fx[currency] = 1
	}
}

// SetFX sets a currency's rate to the base currency (base per unit).
func (b *Book) SetFX(currency string, rate float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fx[currency] = rate
}

// SetPosition sets a holding of a listed contract.
func (b *Book) SetPosition(conID int, quantity, avgCost float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if quantity == 0 {
		delete(b.pos, conID)
		return
	}
	b.pos[conID] = &Position{ConID: conID, Quantity: quantity, AvgCost: avgCost}
}

// SetPrice moves a contract's last price.
func (b *Book) SetPrice(conID int, price float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if in := b.instr[conID]; in != nil {
		in.Price = price
		b.repriceLocked(conID)
	}
}

// SetQuote sets a listed symbol's quote: last, and bid and ask when given
// (zero keeps the spread around last).
func (b *Book) SetQuote(symbol string, bid, ask, last float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, in := range b.instr {
		if strings.EqualFold(in.Symbol, symbol) && in.SecType == "STK" {
			if last > 0 {
				in.Price = last
			}
			in.Bid, in.Ask = bid, ask
			b.repriceLocked(in.ConID)
			return nil
		}
	}
	return fmt.Errorf("twssim: %s is not listed", symbol)
}

// StockConID finds a listed stock by symbol.
func (b *Book) StockConID(symbol string) (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, in := range b.instr {
		if in.SecType == "STK" && strings.EqualFold(in.Symbol, symbol) {
			return in.ConID, true
		}
	}
	return 0, false
}

// Contract returns a listed contract.
func (b *Book) Contract(conID int) (Instrument, bool) { return b.instrument(conID) }

// Position returns a holding and whether the book holds it (a line closed
// this session is not held; it stays a zero row on the streams).
func (b *Book) Position(conID int) (Position, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pos[conID]
	if !ok || p.Quantity == 0 {
		return Position{}, false
	}
	return *p, true
}

// ClosePosition takes a holding flat during the session: it stays a zero
// row on the account and position streams, as a closed line does at TWS.
func (b *Book) ClosePosition(conID int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.pos[conID]; p != nil {
		p.Quantity = 0
	}
}

// Cash is the native cash of a currency.
func (b *Book) Cash(currency string) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cash[currency]
}

// Orders returns every order the broker holds, by order id.
func (b *Book) Orders() []Order {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Order, 0, len(b.orders))
	for _, o := range b.orders {
		out = append(out, *o)
	}
	slices.SortFunc(out, func(x, y Order) int { return x.OrderID - y.OrderID })
	return out
}

// Executions returns every fill so far.
func (b *Book) Executions() []Execution {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.execs)
}

func (b *Book) instrument(conID int) (Instrument, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	in, ok := b.instr[conID]
	if !ok {
		return Instrument{}, false
	}
	return *in, true
}

// lookup finds the contracts a request names: by contract id when it gives
// one, else by symbol, security type and, when given, currency.
func (b *Book) lookup(conID int, symbol, secType, currency string) []Instrument {
	return b.lookupContract(contractQuery{conID: conID, symbol: symbol, secType: secType, currency: currency})
}

// contractQuery is what a contract request names: a contract id, or a symbol,
// security type and currency, and for an option any of expiry (a YYYYMM or
// YYYYMMDD prefix), strike and right.
type contractQuery struct {
	conID                     int
	symbol, secType, currency string
	expiry, right             string
	strike                    float64
}

func (b *Book) lookupContract(q contractQuery) []Instrument {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Instrument
	for _, in := range b.instr {
		if in.Defunct {
			continue
		}
		switch {
		case q.conID > 0:
			if in.ConID != q.conID {
				continue
			}
		case !strings.EqualFold(in.Symbol, q.symbol):
			continue
		case q.secType != "" && !strings.EqualFold(in.SecType, q.secType):
			continue
		case q.secType == "" && in.SecType == "OPT":
			continue
		case q.currency != "" && !strings.EqualFold(in.Currency, q.currency):
			continue
		case q.expiry != "" && !strings.HasPrefix(in.Expiry, q.expiry):
			continue
		case q.strike > 0 && in.Strike != q.strike:
			continue
		case q.right != "" && !strings.EqualFold(in.Right[:min(1, len(in.Right))], q.right[:1]):
			continue
		}
		out = append(out, *in)
	}
	slices.SortFunc(out, func(x, y Instrument) int { return x.ConID - y.ConID })
	return out
}

// quote is the deterministic quote of a contract: the bid and ask straddle the
// last by half the spread.
type quote struct{ bid, ask, last, close float64 }

func quoteOf(in Instrument) quote {
	q := quote{last: in.Price, close: roundTo(in.Price*0.995, in.MinTick)}
	if in.Spread > 0 {
		q.bid = roundTo(in.Price-in.Spread/2, in.MinTick)
		q.ask = roundTo(in.Price+in.Spread/2, in.MinTick)
	}
	if in.Bid > 0 || in.Ask > 0 {
		q.bid, q.ask = in.Bid, in.Ask
	}
	return q
}

func roundTo(v, tick float64) float64 {
	if tick <= 0 {
		return v
	}
	return math.Round(math.Round(v/tick)*tick*1e8) / 1e8
}

// accountFigures are the account's measured values in the base currency.
type accountFigures struct {
	stockValue  map[string]float64 // native stock market value per currency
	optionValue map[string]float64 // native option market value per currency
	gross       float64            // base
	cashBase    float64
	nlv         float64
	unrealized  float64
	initMargin  float64
	maintMargn  float64
}

// The simulated account is a Reg T margin account: 25 percent maintenance
// and initial margin on long stock (assumed; IBKR's per-instrument rates
// are not modelled).
const marginRate = 0.25

func (b *Book) figuresLocked() accountFigures {
	f := accountFigures{stockValue: map[string]float64{}, optionValue: map[string]float64{}}
	for ccy, amount := range b.cash {
		f.cashBase += amount * b.fx[ccy]
	}
	for _, p := range b.pos {
		in := b.instr[p.ConID]
		if in == nil {
			continue
		}
		value := p.Quantity * in.Price * multiplier(*in)
		if in.SecType == "OPT" {
			f.optionValue[in.Currency] += value
		} else {
			f.stockValue[in.Currency] += value
		}
		f.gross += math.Abs(value) * b.fx[in.Currency]
		f.unrealized += (value - p.AvgCost*p.Quantity) * b.fx[in.Currency]
	}
	f.nlv = f.cashBase + f.gross
	f.initMargin = f.gross * marginRate
	f.maintMargn = f.gross * marginRate
	return f
}

// accountValue is one key/value/currency row of the account.
type accountValue struct{ key, value, currency string }

// accountValuesLocked lists the reqAccountUpdates rows: account totals in the
// base currency's suffix and the cash ledger per currency, plus the BASE
// rows IBKR sends.
func (b *Book) accountValuesLocked() []accountValue {
	f := b.figuresLocked()
	base := b.base
	rows := []accountValue{
		{"AccountCode", b.account, ""},
		{"AccountType", "INDIVIDUAL", ""},
		{"NetLiquidation", num2(f.nlv), base},
		{"TotalCashValue", num2(f.cashBase), base},
		{"SettledCash", num2(f.cashBase), base},
		{"GrossPositionValue", num2(f.gross), base},
		{"EquityWithLoanValue", num2(f.nlv), base},
		{"InitMarginReq", num2(f.initMargin), base},
		{"MaintMarginReq", num2(f.maintMargn), base},
		{"AvailableFunds", num2(f.nlv - f.initMargin), base},
		{"ExcessLiquidity", num2(f.nlv - f.maintMargn), base},
		{"BuyingPower", num2((f.nlv - f.initMargin) * 4), base},
		{"Cushion", num2((f.nlv - f.maintMargn) / max(f.nlv, 1)), ""},
		{"LookAheadInitMarginReq", num2(f.initMargin), base},
		{"LookAheadMaintMarginReq", num2(f.maintMargn), base},
		{"LookAheadAvailableFunds", num2(f.nlv - f.initMargin), base},
		{"LookAheadExcessLiquidity", num2(f.nlv - f.maintMargn), base},
		{"UnrealizedPnL", num2(f.unrealized), base},
		{"RealizedPnL", "0", base},
	}
	for _, ccy := range b.currenciesLocked() {
		rows = append(rows,
			accountValue{"CashBalance", num2(b.cash[ccy]), ccy},
			accountValue{"ExchangeRate", num(b.fx[ccy]), ccy},
			accountValue{"StockMarketValue", num2(f.stockValue[ccy]), ccy},
			accountValue{"OptionMarketValue", num2(f.optionValue[ccy]), ccy},
			accountValue{"NetLiquidationByCurrency", num2(b.cash[ccy] + f.stockValue[ccy] + f.optionValue[ccy]), ccy},
		)
	}
	rows = append(rows,
		accountValue{"CashBalance", num2(f.cashBase), "BASE"},
		accountValue{"NetLiquidationByCurrency", num2(f.nlv), "BASE"},
	)
	return rows
}

// summaryRowsLocked lists the reqAccountSummary answer: the requested account
// tags, then the per-currency ledger in Gateway 10.47's "$LEDGER-" dialect
// with the pinned account named on each row.
func (b *Book) summaryRowsLocked(tags []string) []accountValue {
	values := map[string]accountValue{}
	for _, row := range b.accountValuesLocked() {
		if row.currency == b.base || row.currency == "" {
			values[row.key] = row
		}
	}
	var rows []accountValue
	ledger := false
	for _, tag := range tags {
		if strings.HasPrefix(tag, "$LEDGER") {
			ledger = true
			continue
		}
		if row, ok := values[tag]; ok {
			rows = append(rows, row)
		}
	}
	if ledger {
		f := b.figuresLocked()
		for _, ccy := range b.currenciesLocked() {
			rows = append(rows,
				accountValue{"$LEDGER-CashBalance", num2(b.cash[ccy]), ccy},
				accountValue{"$LEDGER-SettledCash", num2(b.cash[ccy]), ccy},
				accountValue{"$LEDGER-ExchangeRate", num(b.fx[ccy]), ccy},
				accountValue{"$LEDGER-StockMarketValue", num2(f.stockValue[ccy]), ccy},
				accountValue{"$LEDGER-OptionMarketValue", num2(f.optionValue[ccy]), ccy},
				accountValue{"$LEDGER-NetLiquidationByCurrency", num2(b.cash[ccy] + f.stockValue[ccy] + f.optionValue[ccy]), ccy},
			)
		}
	}
	return rows
}

func (b *Book) currenciesLocked() []string {
	set := map[string]bool{}
	for ccy := range b.cash {
		set[ccy] = true
	}
	for _, p := range b.pos {
		if in := b.instr[p.ConID]; in != nil {
			set[in.Currency] = true
		}
	}
	out := make([]string, 0, len(set))
	for ccy := range set {
		out = append(out, ccy)
	}
	slices.Sort(out)
	return out
}

func num2(v float64) string { return fmt.Sprintf("%.2f", v) }

// portfolioRow is one updatePortfolio row.
type portfolioRow struct {
	in  Instrument
	pos Position
}

func (b *Book) portfolioLocked() []portfolioRow {
	var out []portfolioRow
	for _, p := range b.pos {
		if in := b.instr[p.ConID]; in != nil {
			out = append(out, portfolioRow{in: *in, pos: *p})
		}
	}
	slices.SortFunc(out, func(x, y portfolioRow) int { return x.in.ConID - y.in.ConID })
	return out
}

// whatIf is the broker's margin and commission preview of an order.
type whatIf struct {
	initBefore, maintBefore, equityBefore float64
	initAfter, maintAfter, equityAfter    float64
	commission                            float64
	commissionCcy, marginCcy              string
	// noFee leaves the commission figures out of the answer.
	noFee bool
}

// fee is the commission of a fill: per share for stock (capped at one
// percent of value), 0.65 per contract for options, at least feeMin.
func (b *Book) fee(in Instrument, shares, price float64) float64 {
	if in.SecType == "OPT" || in.SecType == "BAG" {
		return math.Round(max(b.feeMin, shares*0.65)*100) / 100
	}
	return math.Round(min(max(b.feeMin, shares*b.feePerShare), shares*price*0.01)*100) / 100
}

func (b *Book) whatIfLocked(o PlaceOrder, in Instrument) whatIf {
	f := b.figuresLocked()
	price := o.LimitPrice
	if price <= 0 {
		price = in.Price
	}
	notionalBase := o.Quantity * price * multiplier(in) * b.fx[in.Currency]
	fee := b.fee(in, o.Quantity, price)
	sign := 1.0
	if o.Action == "SELL" {
		sign = -1
	}
	afterGross := max(0, f.gross+sign*notionalBase)
	return whatIf{
		initBefore: f.initMargin, maintBefore: f.maintMargn, equityBefore: f.nlv,
		initAfter: afterGross * marginRate, maintAfter: afterGross * marginRate, equityAfter: f.nlv - fee*b.fx[in.Currency],
		commission: fee, commissionCcy: in.Currency, marginCcy: b.base,
	}
}

// fillLocked fills shares of an order at price (all that remains when shares
// is zero or more than remains) and moves cash and the position.
func (b *Book) fillLocked(order *Order, in Instrument, shares, price float64, now time.Time) *Execution {
	if shares <= 0 || shares > order.Remaining() {
		shares = order.Remaining()
	}
	order.AvgPrice = (order.AvgPrice*order.Filled + price*shares) / (order.Filled + shares)
	order.Filled += shares
	if order.Remaining() == 0 {
		order.Status = "Filled"
	}
	signed := shares
	side := "BOT"
	if order.Action == "SELL" {
		signed, side = -shares, "SLD"
	}
	fee := b.fee(in, shares, price)
	b.cash[in.Currency] -= signed*price*multiplier(in) + fee
	p := b.pos[in.ConID]
	if p == nil {
		p = &Position{ConID: in.ConID}
		b.pos[in.ConID] = p
	}
	if (p.Quantity >= 0) == (signed > 0) && p.Quantity+signed != 0 {
		p.AvgCost = (p.AvgCost*math.Abs(p.Quantity) + price*multiplier(in)*shares) / (math.Abs(p.Quantity) + shares)
	}
	// A line a fill takes flat stays as a zero row for the session: TWS keeps
	// sending updatePortfolio and position callbacks with position 0 for it
	// (assumed from the official client's callbacks, which carry closed
	// lines at zero).
	p.Quantity += signed
	b.execSeq++
	ex := Execution{
		ExecID: fmt.Sprintf("0000sim.%08d.01.01", b.execSeq), OrderID: order.OrderID, PermID: order.PermID, ClientID: order.ClientID,
		ConID: in.ConID, Side: side, Shares: shares, Price: price, CumQty: order.Filled, AvgPrice: order.AvgPrice, Time: now,
		OrderRef: order.OrderRef, Fee: fee, FeeCcy: in.Currency, Account: b.account, Exchange: in.PrimaryExch, Contract: in,
	}
	b.execs = append(b.execs, ex)
	return &ex
}
