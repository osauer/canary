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
	SecType      string // STK, IND or CASH
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
		{ConID: 13455763, Symbol: "VIX", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC VOLATILITY INDEX", MinTick: 0.01, Price: 15},
		{ConID: 47511905, Symbol: "VIX3M", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC 3M VOLATILITY INDEX", MinTick: 0.01, Price: 19.4},
		{ConID: 416904, Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC 500 INDEX", MinTick: 0.01, Price: 6000},
		{ConID: 900203, Symbol: "VVIX", SecType: "IND", Exchange: "CBOE", Currency: "USD", LongName: "SYNTHETIC VOL OF VOL INDEX", MinTick: 0.01, Price: 88},
		{ConID: 15016059, Symbol: "USD", SecType: "CASH", Exchange: "IDEALPRO", Currency: "JPY", LocalSymbol: "USD.JPY", TradingClass: "USD.JPY", LongName: "SYNTHETIC USD.JPY", MinTick: 0.005, Price: 150, Spread: 0.01},
	} {
		b.AddInstrument(in)
	}
	b.SetPosition(900001, 100, 48)
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
	}
}

// Position returns a holding and whether the book holds it.
func (b *Book) Position(conID int) (Position, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pos[conID]
	if !ok {
		return Position{}, false
	}
	return *p, true
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
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Instrument
	for _, in := range b.instr {
		switch {
		case conID > 0:
			if in.ConID != conID {
				continue
			}
		case !strings.EqualFold(in.Symbol, symbol):
			continue
		case secType != "" && !strings.EqualFold(in.SecType, secType):
			continue
		case currency != "" && !strings.EqualFold(in.Currency, currency):
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
	stockValue map[string]float64 // native market value per currency
	gross      float64            // base
	cashBase   float64
	nlv        float64
	unrealized float64
	initMargin float64
	maintMargn float64
}

// The simulated account is a Reg T margin account: 25 percent maintenance
// and initial margin on long stock (assumed; IBKR's per-instrument rates
// are not modelled).
const marginRate = 0.25

func (b *Book) figuresLocked() accountFigures {
	f := accountFigures{stockValue: map[string]float64{}}
	for ccy, amount := range b.cash {
		f.cashBase += amount * b.fx[ccy]
	}
	for _, p := range b.pos {
		in := b.instr[p.ConID]
		if in == nil {
			continue
		}
		value := p.Quantity * in.Price
		f.stockValue[in.Currency] += value
		f.gross += math.Abs(value) * b.fx[in.Currency]
		f.unrealized += (in.Price - p.AvgCost) * p.Quantity * b.fx[in.Currency]
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
			accountValue{"NetLiquidationByCurrency", num2(b.cash[ccy] + f.stockValue[ccy]), ccy},
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
				accountValue{"$LEDGER-NetLiquidationByCurrency", num2(b.cash[ccy] + f.stockValue[ccy]), ccy},
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
}

func (b *Book) fee(shares, price float64) float64 {
	return math.Round(min(max(b.feeMin, shares*b.feePerShare), shares*price*0.01)*100) / 100
}

func (b *Book) whatIfLocked(o PlaceOrder, in Instrument) whatIf {
	f := b.figuresLocked()
	price := o.LimitPrice
	if price <= 0 {
		price = in.Price
	}
	notionalBase := o.Quantity * price * b.fx[in.Currency]
	fee := b.fee(o.Quantity, price)
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
	fee := b.fee(shares, price)
	b.cash[in.Currency] -= signed*price + fee
	p := b.pos[in.ConID]
	if p == nil {
		p = &Position{ConID: in.ConID}
		b.pos[in.ConID] = p
	}
	if (p.Quantity >= 0) == (signed > 0) && p.Quantity+signed != 0 {
		p.AvgCost = (p.AvgCost*math.Abs(p.Quantity) + price*shares) / (math.Abs(p.Quantity) + shares)
	}
	p.Quantity += signed
	if p.Quantity == 0 {
		delete(b.pos, in.ConID)
	}
	b.execSeq++
	ex := Execution{
		ExecID: fmt.Sprintf("0000sim.%08d.01.01", b.execSeq), OrderID: order.OrderID, PermID: order.PermID, ClientID: order.ClientID,
		ConID: in.ConID, Side: side, Shares: shares, Price: price, CumQty: order.Filled, AvgPrice: order.AvgPrice, Time: now,
		OrderRef: order.OrderRef, Fee: fee, FeeCcy: in.Currency, Account: b.account, Exchange: in.PrimaryExch, Contract: in,
	}
	b.execs = append(b.execs, ex)
	return &ex
}
