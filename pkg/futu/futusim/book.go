package futusim

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // the market session tables resolve their zones without the host's database

	"github.com/osauer/canary/v2/pkg/futu"
)

//go:embed testdata/book.json
var defaultBook []byte

// Clock is the simulator's time: it starts where the book says and moves
// only when told, so a session is reproducible. BuildClock from the wall
// clock when a witness wants live time.
type Clock struct {
	mu  sync.Mutex
	at  time.Time
	run bool
	// base is the wall-clock instant at which a running clock was at `at`.
	base time.Time
}

// NewClock starts a stopped clock at the instant.
func NewClock(at time.Time) *Clock { return &Clock{at: at.UTC()} }

// RunningClock starts a clock at the instant that then follows the wall
// clock from now on; the simulator process uses it for `-clock now`.
func RunningClock(at time.Time) *Clock {
	return &Clock{at: at.UTC(), run: true, base: time.Now()}
}

// Now is the simulator's current instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run {
		return c.at.Add(time.Since(c.base))
	}
	return c.at
}

// Advance moves a stopped clock forward (or a running clock's offset).
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// Env is an account's trading environment, OpenD's trdEnv.
type Env string

const (
	// EnvSimulate is OpenD's paper environment (trdEnv 0).
	EnvSimulate Env = "simulate"
	// EnvReal is a live account (trdEnv 1).
	EnvReal Env = "real"
)

// Account is one trading account the simulator reports.
type Account struct {
	ID      uint64   `json:"acc_id"`
	Env     Env      `json:"env"`
	Markets []string `json:"markets"`
	Type    string   `json:"type"`
	Firm    string   `json:"firm"`
	// Locked is a real account's trade lock; the simulator never unlocks
	// one, so a locked account refuses every order for the session.
	Locked bool               `json:"locked"`
	Cash   map[string]float64 `json:"cash"`
}

// Instrument is one listed security with the parameters of its quote walk.
type Instrument struct {
	// Code is OpenD's market-prefixed code, HK.00700.
	Code     string `json:"code"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	LotSize  int64  `json:"lot_size"`
	Currency string `json:"currency"`
	Listed   string `json:"listed"`
	// Exchange is the US exchange of a US line: NASDAQ, NYSE or AMEX.
	Exchange   string  `json:"exchange,omitempty"`
	Start      float64 `json:"start"`
	Tick       float64 `json:"tick"`
	Volatility float64 `json:"volatility"`
}

// Market is the instrument's market prefix.
func (i Instrument) Market() string {
	market, _, _ := strings.Cut(i.Code, ".")
	return market
}

// Position is a holding of one account.
type Position struct {
	AccountID uint64  `json:"acc_id"`
	Code      string  `json:"code"`
	Quantity  int64   `json:"quantity"`
	Cost      float64 `json:"cost"`
}

// Side is an order's direction.
type Side string

const (
	// Buy opens or adds to a position.
	Buy Side = "buy"
	// Sell reduces one.
	Sell Side = "sell"
)

// OrderStatus is where an order stands in the simulator.
type OrderStatus string

const (
	// OrderOpen is working against the walk.
	OrderOpen OrderStatus = "open"
	// OrderFilled is done.
	OrderFilled OrderStatus = "filled"
	// OrderCancelled was withdrawn before it filled.
	OrderCancelled OrderStatus = "cancelled"
)

// Order is one order placed in the book. A nil Limit is a market order.
type Order struct {
	ID        uint64
	AccountID uint64
	Code      string
	Side      Side
	Quantity  int64
	Limit     *float64
	Status    OrderStatus
	Filled    int64
	PlacedAt  time.Time
	UpdatedAt time.Time
}

// Fill is one execution of an order.
type Fill struct {
	ID        uint64
	OrderID   uint64
	AccountID uint64
	Code      string
	Side      Side
	Quantity  int64
	Price     float64
	At        time.Time
}

// Quote is the walk's state for one instrument at one instant.
type Quote struct {
	Code   string
	At     time.Time
	Last   float64
	Bid    float64
	Ask    float64
	Volume int64
	// DayOpen, High, Low and PrevClose are the day's figures, derived from the
	// instrument's start and the walk so far.
	DayOpen   float64
	High      float64
	Low       float64
	PrevClose float64
	// Open reports whether the instrument's market is in a trading session.
	Open bool
}

// Errors the book answers with; the server maps each to OpenD's reply.
var (
	// ErrNoAccount is an account id the book does not hold.
	ErrNoAccount = errors.New("futusim: no such account")
	// ErrNoInstrument is a code the book does not list.
	ErrNoInstrument = errors.New("futusim: no such instrument")
	// ErrLocked is an order against a locked real account.
	ErrLocked = errors.New("futusim: account is locked for trading")
	// ErrLotSize is a quantity that is not a whole number of lots.
	ErrLotSize = errors.New("futusim: quantity is not a multiple of the lot size")
	// ErrNoOrder is an order id the book does not hold.
	ErrNoOrder = errors.New("futusim: no such order")
	// ErrOrderClosed is a change to an order that is no longer open.
	ErrOrderClosed = errors.New("futusim: order is not open")
	// ErrQuota is a subscription past the OpenD-wide quota.
	ErrQuota = errors.New("futusim: subscription quota spent")
	// ErrHold is a release inside the one-minute hold.
	ErrHold = errors.New("futusim: subscription held for its minimum time")
	// ErrNotSubscribed is a release of a line that is not held.
	ErrNotSubscribed = errors.New("futusim: not subscribed")
	// ErrHistoryQuota is a history request past the seven-day quota.
	ErrHistoryQuota = errors.New("futusim: history quota spent")
)

// bookFile is the fixture's shape.
type bookFile struct {
	Seed              uint64       `json:"seed"`
	Clock             time.Time    `json:"clock"`
	SubscriptionQuota int          `json:"subscription_quota"`
	HistoryQuota      int          `json:"history_quota"`
	Accounts          []Account    `json:"accounts"`
	Instruments       []Instrument `json:"instruments"`
	Positions         []Position   `json:"positions"`
	// FX is the book's synthetic rate of one unit of a currency in HKD.
	FX map[string]float64 `json:"fx"`
}

// Book is the simulator's state. Every method is safe for concurrent use;
// the server's handlers call them from the frame loop.
type Book struct {
	Clock *Clock

	mu                sync.Mutex
	seed              uint64
	epoch             time.Time
	subscriptionQuota int
	historyQuota      int
	accounts          []Account
	instruments       []Instrument
	positions         []Position
	fx                map[string]float64
	orders            []Order
	fills             []Fill
	walks             map[string]*walk
	subscriptions     map[string]time.Time
	history           map[string]time.Time
	nextID            uint64
}

// Default is the book of testdata/book.json on a stopped clock.
func Default() *Book {
	b, err := Parse(defaultBook)
	if err != nil {
		panic("futusim: embedded book: " + err.Error())
	}
	return b
}

// AddHolding lists an instrument and holds it on an account.
func (b *Book) AddHolding(in Instrument, account uint64, quantity int64, cost float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.instruments = append(b.instruments, in)
	b.walks[in.Code] = newWalk(b.seed, in)
	b.positions = append(b.positions, Position{AccountID: account, Code: in.Code, Quantity: quantity, Cost: cost})
}

// Load reads a book file.
func Load(path string) (*Book, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse builds a book from fixture JSON. The clock starts at the fixture's
// instant, stopped; the quote walk's epoch is that instant.
func Parse(raw []byte) (*Book, error) {
	var f bookFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("futusim: book: %w", err)
	}
	if f.Clock.IsZero() {
		return nil, errors.New("futusim: book: clock missing")
	}
	if f.SubscriptionQuota == 0 {
		f.SubscriptionQuota = futu.SubscriptionQuotaTiers[0]
	}
	if f.HistoryQuota == 0 {
		f.HistoryQuota = futu.SubscriptionQuotaTiers[0]
	}
	b := &Book{
		Clock:             NewClock(f.Clock),
		seed:              f.Seed,
		epoch:             f.Clock.UTC(),
		subscriptionQuota: f.SubscriptionQuota,
		historyQuota:      f.HistoryQuota,
		accounts:          f.Accounts,
		instruments:       f.Instruments,
		positions:         f.Positions,
		fx:                f.FX,
		walks:             make(map[string]*walk),
		subscriptions:     make(map[string]time.Time),
		history:           make(map[string]time.Time),
	}
	for _, in := range f.Instruments {
		if in.Code == "" || in.Start <= 0 || in.Tick <= 0 || in.LotSize <= 0 {
			return nil, fmt.Errorf("futusim: book: instrument %q needs a code, start, tick and lot size", in.Code)
		}
		if _, ok := sessionTables[in.Market()]; !ok {
			return nil, fmt.Errorf("futusim: book: instrument %q: market %q has no session table", in.Code, in.Market())
		}
		b.walks[in.Code] = newWalk(f.Seed, in)
	}
	for _, a := range f.Accounts {
		if a.ID == 0 || (a.Env != EnvSimulate && a.Env != EnvReal) {
			return nil, fmt.Errorf("futusim: book: account %d needs an id and an env of simulate or real", a.ID)
		}
	}
	for _, p := range f.Positions {
		if _, err := b.account(p.AccountID); err != nil {
			return nil, fmt.Errorf("futusim: book: position %q: %w", p.Code, err)
		}
		if _, err := b.instrument(p.Code); err != nil {
			return nil, fmt.Errorf("futusim: book: position: %w", err)
		}
	}
	return b, nil
}

// Seed is the walk's seed.
func (b *Book) Seed() uint64 { return b.seed }

// SubscriptionQuota is the OpenD-wide subscription quota the book grants.
func (b *Book) SubscriptionQuota() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subscriptionQuota
}

// HistoryQuota is the seven-day historical candlestick quota the book grants.
func (b *Book) HistoryQuota() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.historyQuota
}

// Accounts lists the accounts, in fixture order.
func (b *Book) Accounts() []Account {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.accounts)
}

// Account returns one account.
func (b *Book) Account(id uint64) (Account, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.account(id)
	if err != nil {
		return Account{}, err
	}
	return *a, nil
}

func (b *Book) account(id uint64) (*Account, error) {
	for i := range b.accounts {
		if b.accounts[i].ID == id {
			return &b.accounts[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %d", ErrNoAccount, id)
}

// Instruments lists the instruments, in fixture order.
func (b *Book) Instruments() []Instrument {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.instruments)
}

// Instrument returns one instrument by code.
func (b *Book) Instrument(code string) (Instrument, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.instrument(code)
}

func (b *Book) instrument(code string) (Instrument, error) {
	for _, in := range b.instruments {
		if in.Code == code {
			return in, nil
		}
	}
	return Instrument{}, fmt.Errorf("%w: %q", ErrNoInstrument, code)
}

// Quote is the walk's state for the instrument at the clock's instant.
func (b *Book) Quote(code string) (Quote, error) {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.quote(code, now)
}

func (b *Book) quote(code string, now time.Time) (Quote, error) {
	in, err := b.instrument(code)
	if err != nil {
		return Quote{}, err
	}
	w := b.walks[code]
	steps := openSeconds(in.Market(), b.epoch, now)
	last, volume := w.at(steps)
	return Quote{
		Code:      code,
		At:        now,
		Last:      last,
		Bid:       roundTick(last-in.Tick, in.Tick),
		Ask:       roundTick(last+in.Tick, in.Tick),
		Volume:    volume,
		Open:      marketOpen(in.Market(), now),
		DayOpen:   in.Start,
		High:      max(in.Start, last),
		Low:       min(in.Start, last),
		PrevClose: roundTick(in.Start*prevCloseFactor, in.Tick),
	}, nil
}

// MarketOpen reports whether the market is in a trading session at the
// clock's instant. Weekends close every market; holidays are not modelled.
func (b *Book) MarketOpen(market string) bool {
	return marketOpen(market, b.Clock.Now())
}

// Positions lists an account's holdings with a non-zero quantity.
func (b *Book) Positions(accountID uint64) ([]Position, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.account(accountID); err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range b.positions {
		if p.AccountID == accountID && p.Quantity != 0 {
			out = append(out, p)
		}
	}
	return out, nil
}

// Cash is an account's cash by currency.
func (b *Book) Cash(accountID uint64) (map[string]float64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.account(accountID)
	if err != nil {
		return nil, err
	}
	return maps.Clone(a.Cash), nil
}

// PlaceOrder enters an order and fills what the walk fills at once; a
// market order fills whole at the touch, a limit order when the touch
// crosses it. The fills, if any, are returned with the order.
func (b *Book) PlaceOrder(accountID uint64, code string, side Side, quantity int64, limit *float64) (Order, []Fill, error) {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	a, err := b.account(accountID)
	if err != nil {
		return Order{}, nil, err
	}
	if a.Locked {
		return Order{}, nil, fmt.Errorf("%w: %d", ErrLocked, accountID)
	}
	in, err := b.instrument(code)
	if err != nil {
		return Order{}, nil, err
	}
	if side != Buy && side != Sell {
		return Order{}, nil, fmt.Errorf("futusim: side %q", side)
	}
	if quantity <= 0 || quantity%in.LotSize != 0 {
		return Order{}, nil, fmt.Errorf("%w: %d of %d", ErrLotSize, quantity, in.LotSize)
	}
	if limit != nil {
		l := roundTick(*limit, in.Tick)
		limit = &l
	}
	b.nextID++
	o := Order{ID: b.nextID, AccountID: accountID, Code: code, Side: side, Quantity: quantity, Limit: limit, Status: OrderOpen, PlacedAt: now, UpdatedAt: now}
	b.orders = append(b.orders, o)
	fills := b.settle(now)
	return b.orders[len(b.orders)-1], fills, nil
}

// CancelOrder withdraws an open order.
func (b *Book) CancelOrder(orderID uint64) (Order, error) {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range b.orders {
		if b.orders[i].ID != orderID {
			continue
		}
		if b.orders[i].Status != OrderOpen {
			return b.orders[i], fmt.Errorf("%w: %d is %s", ErrOrderClosed, orderID, b.orders[i].Status)
		}
		b.orders[i].Status = OrderCancelled
		b.orders[i].UpdatedAt = now
		return b.orders[i], nil
	}
	return Order{}, fmt.Errorf("%w: %d", ErrNoOrder, orderID)
}

// Settle fills every open order the walk now crosses and returns the fills.
// The server calls it after the clock moves.
func (b *Book) Settle() []Fill {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.settle(now)
}

func (b *Book) settle(now time.Time) []Fill {
	var fills []Fill
	for i := range b.orders {
		o := &b.orders[i]
		if o.Status != OrderOpen {
			continue
		}
		q, err := b.quote(o.Code, now)
		if err != nil || !q.Open {
			continue
		}
		price := q.Ask
		if o.Side == Sell {
			price = q.Bid
		}
		if o.Limit != nil {
			if (o.Side == Buy && price > *o.Limit) || (o.Side == Sell && price < *o.Limit) {
				continue
			}
			price = *o.Limit
		}
		remaining := o.Quantity - o.Filled
		b.nextID++
		f := Fill{ID: b.nextID, OrderID: o.ID, AccountID: o.AccountID, Code: o.Code, Side: o.Side, Quantity: remaining, Price: price, At: now}
		fills = append(fills, f)
		b.fills = append(b.fills, f)
		o.Filled = o.Quantity
		o.Status = OrderFilled
		o.UpdatedAt = now
		b.apply(f)
	}
	return fills
}

// apply books a fill into the position and the cash of its account.
func (b *Book) apply(f Fill) {
	in, _ := b.instrument(f.Code)
	a, _ := b.account(f.AccountID)
	signed := f.Quantity
	if f.Side == Sell {
		signed = -signed
	}
	if a.Cash == nil {
		a.Cash = map[string]float64{}
	}
	a.Cash[in.Currency] -= float64(signed) * f.Price
	for i := range b.positions {
		p := &b.positions[i]
		if p.AccountID != f.AccountID || p.Code != f.Code {
			continue
		}
		next := p.Quantity + signed
		switch {
		case next == 0:
			p.Cost = 0
		case (p.Quantity >= 0) == (signed > 0):
			// Adding to the position: the cost averages in.
			p.Cost = (p.Cost*float64(p.Quantity) + f.Price*float64(signed)) / float64(next)
		}
		p.Quantity = next
		return
	}
	b.positions = append(b.positions, Position{AccountID: f.AccountID, Code: f.Code, Quantity: signed, Cost: f.Price})
}

// Orders lists an account's orders, oldest first.
func (b *Book) Orders(accountID uint64) ([]Order, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.account(accountID); err != nil {
		return nil, err
	}
	var out []Order
	for _, o := range b.orders {
		if o.AccountID == accountID {
			out = append(out, o)
		}
	}
	return out, nil
}

// Fills lists an account's fills, oldest first.
func (b *Book) Fills(accountID uint64) ([]Fill, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.account(accountID); err != nil {
		return nil, err
	}
	var out []Fill
	for _, f := range b.fills {
		if f.AccountID == accountID {
			out = append(out, f)
		}
	}
	return out, nil
}

// Subscribe takes one unit of the subscription quota for the line
// (instrument and data type); a line already held costs nothing more.
func (b *Book) Subscribe(code string, subType int32) error {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.instrument(code); err != nil {
		return err
	}
	key := subscriptionKey(code, subType)
	if _, held := b.subscriptions[key]; held {
		return nil
	}
	if len(b.subscriptions) >= b.subscriptionQuota {
		return fmt.Errorf("%w: %d of %d", ErrQuota, len(b.subscriptions), b.subscriptionQuota)
	}
	b.subscriptions[key] = now
	return nil
}

// Unsubscribe releases a line held for at least the documented minimum.
func (b *Book) Unsubscribe(code string, subType int32) error {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	key := subscriptionKey(code, subType)
	since, held := b.subscriptions[key]
	if !held {
		return fmt.Errorf("%w: %s", ErrNotSubscribed, key)
	}
	if now.Sub(since) < futu.SubscriptionMinimumHold {
		return fmt.Errorf("%w: %s held %s", ErrHold, key, now.Sub(since))
	}
	delete(b.subscriptions, key)
	return nil
}

// Subscribed reports whether the line is held.
func (b *Book) Subscribed(code string, subType int32) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, held := b.subscriptions[subscriptionKey(code, subType)]
	return held
}

// SubscriptionUsage is the quota used and remaining, OpenD-wide.
func (b *Book) SubscriptionUsage() (used, remaining int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscriptions), b.subscriptionQuota - len(b.subscriptions)
}

func subscriptionKey(code string, subType int32) string {
	return fmt.Sprintf("%s/%d", code, subType)
}

// RequestHistory spends one unit of the seven-day history quota for the
// instrument, or none when the instrument was requested inside the window.
func (b *Book) RequestHistory(code string) error {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, err := b.instrument(code); err != nil {
		return err
	}
	b.expireHistory(now)
	if _, inWindow := b.history[code]; inWindow {
		return nil
	}
	if len(b.history) >= b.historyQuota {
		return fmt.Errorf("%w: %d of %d", ErrHistoryQuota, len(b.history), b.historyQuota)
	}
	b.history[code] = now
	return nil
}

// HistoryUsage is the history quota used and remaining in the window, and
// the instruments it was spent on with the time of each request.
func (b *Book) HistoryUsage() (used, remaining int, detail map[string]time.Time) {
	now := b.Clock.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.expireHistory(now)
	detail = maps.Clone(b.history)
	return len(b.history), b.historyQuota - len(b.history), detail
}

func (b *Book) expireHistory(now time.Time) {
	for code, at := range b.history {
		if now.Sub(at) >= futu.HistoryQuotaWindow {
			delete(b.history, code)
		}
	}
}

// walk is one instrument's deterministic price path: step k is a function
// of the seed, the code and k alone, so every book with the same fixture
// and clock reads the same quote.
type walk struct {
	in     Instrument
	rng    *rand.Rand
	seed   uint64
	step   int64
	price  float64
	volume int64
}

func newWalk(seed uint64, in Instrument) *walk {
	h := fnv.New64a()
	h.Write([]byte(in.Code))
	w := &walk{in: in, seed: seed ^ h.Sum64()}
	w.reset()
	return w
}

func (w *walk) reset() {
	w.rng = rand.New(rand.NewPCG(w.seed, w.seed>>1|1))
	w.step = 0
	w.price = w.in.Start
	w.volume = 0
}

// at returns the price and the day's cumulative volume after k steps,
// replaying from the start when asked for an earlier step.
func (w *walk) at(k int64) (float64, int64) {
	if k < w.step {
		w.reset()
	}
	for w.step < k {
		move := 1 + w.in.Volatility*(2*w.rng.Float64()-1)
		w.price = roundTick(w.price*move, w.in.Tick)
		if w.price < w.in.Tick {
			w.price = w.in.Tick
		}
		w.volume += w.rng.Int64N(10) * w.in.LotSize
		w.step++
	}
	return w.price, w.volume
}

func roundTick(price, tick float64) float64 {
	return math.Round(math.Round(price/tick)*tick*1e6) / 1e6
}

// session is one trading session of a market in its local clock.
type session struct{ open, close int } // minutes from local midnight

// sessionTables are the regular sessions per market, Monday to Friday.
var sessionTables = map[string]struct {
	zone     string
	sessions []session
}{
	"HK": {"Asia/Hong_Kong", []session{{9*60 + 30, 12 * 60}, {13 * 60, 16 * 60}}},
	"US": {"America/New_York", []session{{9*60 + 30, 16 * 60}}},
	"SG": {"Asia/Singapore", []session{{9 * 60, 12 * 60}, {13 * 60, 17 * 60}}},
}

var zones sync.Map

func zone(name string) *time.Location {
	if loc, ok := zones.Load(name); ok {
		return loc.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("futusim: zone " + name + ": " + err.Error())
	}
	zones.Store(name, loc)
	return loc
}

func marketOpen(market string, at time.Time) bool {
	table, ok := sessionTables[market]
	if !ok {
		return false
	}
	local := at.In(zone(table.zone))
	if wd := local.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	minute := local.Hour()*60 + local.Minute()
	for _, s := range table.sessions {
		if minute >= s.open && minute < s.close {
			return true
		}
	}
	return false
}

// openSeconds counts the trading seconds of the market between two
// instants, the walk's step count: a closed market holds its price.
func openSeconds(market string, from, to time.Time) int64 {
	table, ok := sessionTables[market]
	if !ok || !to.After(from) {
		return 0
	}
	loc := zone(table.zone)
	var total int64
	day := time.Date(from.In(loc).Year(), from.In(loc).Month(), from.In(loc).Day(), 0, 0, 0, 0, loc)
	for !day.After(to) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			for _, s := range table.sessions {
				start := day.Add(time.Duration(s.open) * time.Minute)
				end := day.Add(time.Duration(s.close) * time.Minute)
				if start.Before(from) {
					start = from
				}
				if end.After(to) {
					end = to
				}
				if end.After(start) {
					total += int64(end.Sub(start) / time.Second)
				}
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return total
}
