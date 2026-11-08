// Package opend adapts the Futu OpenD client in pkg/futu to the daemon's read
// seam. One Adapter serves one daemon for its whole life: the pkg/futu connector
// redials on its own, so the binding's generation stays one and its epoch moves
// with each session.
package opend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/pkg/futu"
)

// generation is the adapter's only connector generation: the connector lives
// as long as the daemon, and its epoch names each session.
const generation = 1

// Modes as the daemon's scope spells them.
const (
	modePaper   = "paper"
	modeLive    = "live"
	modeUnknown = "unknown"
)

// Adapter serves the seam over one connector.
type Adapter struct {
	conn *futu.Connector
	now  func() time.Time
	// allowLine is the daemon's line budget; nil admits every line.
	allowLine func(code string) bool
	// usVenues are the exchanges of the US lines read so far. A listing does not
	// move within a session, and a failed read stores nothing.
	usVenuesMu sync.Mutex
	usVenues   map[string]string
	// secTypes are the types of the held codes read so far, under the same lock.
	secTypes map[string]string
}

// SetLineBudget gives the adapter the daemon's line budget. Set once, before the
// session starts.
func (a *Adapter) SetLineBudget(allow func(code string) bool) { a.allowLine = allow }

// subscribe takes a line the budget admits; a name outside it, or past OpenD's
// quota, is served by snapshot.
func (a *Adapter) subscribe(ctx context.Context, code string) (string, broker.SessionBinding, error) {
	if a.allowLine != nil && !a.allowLine(code) {
		return code, a.currentBinding(), fmt.Errorf("%w: %s", broker.ErrSnapshotOnly, code)
	}
	session, err := a.conn.Subscribe(ctx, code)
	if errors.Is(err, futu.ErrQuotaExhausted) {
		return code, bindingOf(session), fmt.Errorf("%w: %w", broker.ErrSnapshotOnly, err)
	}
	return code, bindingOf(session), err
}

func (a *Adapter) currentBinding() broker.SessionBinding {
	session, ok := a.conn.CaptureSession()
	if !ok {
		return broker.SessionBinding{}
	}
	return bindingOf(session)
}

// heldRelease is the seam's form of a release inside the minimum hold.
func heldRelease(err error) error {
	if held, ok := errors.AsType[*futu.SubscriptionHeldError](err); ok {
		return &broker.ReleaseHeldError{Key: held.Code, RetryAfter: held.RetryAfter}
	}
	return err
}

// New builds the adapter over a connector, whose scope names the account.
func New(conn *futu.Connector, now func() time.Time) *Adapter {
	if now == nil {
		now = time.Now
	}
	return &Adapter{conn: conn, now: now}
}

var _ broker.Broker = (*Adapter)(nil)

// Connector is the client under the adapter, for the surfaces that report
// OpenD's own facts.
func (a *Adapter) Connector() *futu.Connector { return a.conn }

// Kind names the broker.
func (a *Adapter) Kind() broker.Kind { return broker.KindOpenD }

// Scope is the pin and the environment the session's account list reported for
// it: concrete once the list has, and account-concrete with an unknown mode
// before any session.
func (a *Adapter) Scope() broker.Scope {
	account, env := a.conn.Scope()
	mode := modeUnknown
	switch env {
	case futu.EnvironmentSimulate:
		mode = modePaper
	case futu.EnvironmentReal:
		mode = modeLive
	}
	return broker.Scope{Kind: broker.KindOpenD, Account: accountWords(account), Mode: mode}
}

// pin is the connector's account: the pin, or the one detection bound.
func (a *Adapter) pin() uint64 {
	account, _ := a.conn.Scope()
	return account
}

// accountWords is the seam's account: empty until a list has named one.
func accountWords(account uint64) string {
	if account == 0 {
		return ""
	}
	return strconv.FormatUint(account, 10)
}

// Capture returns the binding of the current ready session.
func (a *Adapter) Capture() (broker.SessionBinding, bool) {
	session, ok := a.conn.CaptureSession()
	if !ok {
		return broker.SessionBinding{}, false
	}
	return bindingOf(session), true
}

func bindingOf(session futu.SessionBinding) broker.SessionBinding {
	return broker.SessionBinding{Kind: broker.KindOpenD, Generation: generation, Epoch: session.Epoch}
}

func sessionOf(binding broker.SessionBinding) (futu.SessionBinding, bool) {
	if binding.Kind.Normalized() != broker.KindOpenD || binding.Generation != generation {
		return futu.SessionBinding{}, false
	}
	return futu.SessionBinding{Epoch: binding.Epoch}, true
}

// Current reports whether the binding still names the ready session.
func (a *Adapter) Current(binding broker.SessionBinding) bool {
	session, ok := sessionOf(binding)
	return ok && a.conn.Current(session)
}

// Health describes the session for the status surface. BackendDown is OpenD's
// own link to Futu: the socket is up and a server is logged out.
func (a *Adapter) Health() broker.SessionHealth {
	h := a.conn.Health()
	out := broker.SessionHealth{
		Kind:      broker.KindOpenD,
		Connected: h.Connected,
		Ready:     h.Ready,
	}
	if h.StateKnown {
		out.BackendDown = h.Connected && (!h.State.QuoteLoggedIn || !h.State.TradeLoggedIn)
	}
	if h.Environment != futu.EnvironmentUnknown {
		out.Account = strconv.FormatUint(h.Account, 10)
	}
	if h.Session.ServerVersion != 0 {
		out.ServerVersion = int(h.Session.ServerVersion)
	}
	if h.LastError != nil {
		out.LastError = h.LastError.Error()
	}
	return out
}

// Summary reads the pinned account's funds once. OpenD answers from its own
// cache, which its server keeps current, so this read does not spend the pacing
// window; every result is a request, never a fallback.
func (a *Adapter) Summary(ctx context.Context, timeout time.Duration) (*broker.AccountSummary, broker.Provenance, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	funds, err := a.conn.Funds(ctx, false)
	if err != nil {
		return nil, "", err
	}
	out := summaryOf(a.pin(), a.conn.Health().AccountType, funds, a.now().UTC())
	a.addImpliedRates(ctx, out, funds)
	return out, broker.ProvenanceRequest, nil
}

// addImpliedRates fills the ledger's rates for the currencies this account holds or
// has cash in. A failed read leaves them out, which readers take as not measured.
func (a *Adapter) addImpliedRates(ctx context.Context, out *broker.AccountSummary, funds futu.Funds) {
	held := []string{funds.Currency}
	for _, row := range funds.CashRows {
		if row.Cash != nil && *row.Cash != 0 && !slices.Contains(held, row.Currency) {
			held = append(held, row.Currency)
		}
	}
	if positions, err := a.conn.Positions(ctx, false); err == nil {
		for _, row := range positions {
			if row.Currency != "" && !slices.Contains(held, row.Currency) {
				held = append(held, row.Currency)
			}
		}
	}
	rates, err := a.conn.ImpliedRates(ctx, funds.Currency, held)
	if err != nil {
		return
	}
	if out.CurrencyLedger == nil {
		out.CurrencyLedger = make(map[string]broker.CurrencyLedger, len(rates))
	}
	for currency, rate := range rates {
		row := out.CurrencyLedger[currency]
		row.ExchangeRate, row.ExchangeRateObserved = rate, true
		row.ExchangeRateImplied = currency != funds.Currency
		out.CurrencyLedger[currency] = row
	}
}

// summaryOf maps OpenD's funds. Fields OpenD did not send stay nil.
func summaryOf(account uint64, accountType string, funds futu.Funds, asOf time.Time) *broker.AccountSummary {
	raw := map[string]string{}
	if funds.RiskStatus != "" {
		raw["RiskStatus"] = funds.RiskStatus
	}
	out := &broker.AccountSummary{
		AccountID:              strconv.FormatUint(account, 10),
		AccountType:            accountType,
		NetLiquidation:         new(funds.TotalAssets),
		BuyingPower:            new(funds.Power),
		TotalCashValue:         new(funds.Cash),
		GrossPositionValue:     new(funds.MarketValue),
		AvailableFunds:         funds.AvailableFunds,
		MaintenanceMargin:      funds.MaintenanceMargin,
		InitMarginReq:          funds.InitialMargin,
		UnrealizedPnL:          funds.UnrealizedPnL,
		RealizedPnL:            funds.RealizedPnL,
		Currency:               funds.Currency,
		BaseCurrency:           funds.Currency,
		BaseCurrencyProvenance: broker.BaseCurrencyExplicitTag,
		AsOf:                   asOf,
		Raw:                    raw,
	}
	if len(funds.CashRows) > 0 {
		out.CurrencyLedger = make(map[string]broker.CurrencyLedger, len(funds.CashRows))
		for _, row := range funds.CashRows {
			if row.Cash == nil {
				continue
			}
			out.CurrencyLedger[row.Currency] = broker.CurrencyLedger{CashBalance: *row.Cash}
		}
	}
	return out
}

// positionsTimeout bounds the positions read: the seam's Positions takes no
// context, because the IBKR read is a cache read.
const positionsTimeout = 5 * time.Second

// Positions reads the held book. OpenD answers from its own cache; a completed
// reply is a completed download, so its receipt says so.
func (a *Adapter) Positions() ([]broker.Position, broker.PositionsHealth, error) {
	ctx, cancel := context.WithTimeout(context.Background(), positionsTimeout)
	defer cancel()
	requested := a.now().UTC()
	rows, err := a.conn.Positions(ctx, false)
	if err != nil {
		return nil, broker.PositionsHealth{Account: accountWords(a.pin()), RequestedAt: requested}, err
	}
	at := a.now().UTC()
	out := make([]broker.Position, 0, len(rows))
	account := accountWords(a.pin())
	for _, row := range rows {
		out = append(out, positionOf(account, row, a.secTypeOf(ctx, row.Code), at))
	}
	return out, broker.PositionsHealth{Account: account, RequestedAt: requested, InitialCompletedAt: at, LastUpdateAt: at}, nil
}

func positionOf(account string, row futu.Position, secType string, at time.Time) broker.Position {
	in := instrumentOf(row.Code)
	in.Currency = row.Currency
	in.SecType = secType
	out := broker.Position{
		ValuationAt: at,
		Account:     account,
		Instrument:  in,
		Quantity:    row.Quantity,
		MarketPrice: row.Price,
		MarketValue: row.Value,
	}
	if row.AverageCost != nil {
		out.AverageCost = *row.AverageCost
	} else if row.DilutedCost != nil {
		out.AverageCost = *row.DilutedCost
	}
	if row.UnrealizedPnL != nil {
		out.UnrealizedPnL = *row.UnrealizedPnL
	} else {
		out.UnrealizedPnL = row.ProfitLoss
	}
	if row.RealizedPnL != nil {
		out.RealizedPnL = *row.RealizedPnL
	}
	return out
}

// The route families and venues of the markets OpenD serves.
var (
	marketFamilies = map[string]string{"HK": "hk", "US": "us", "SG": "sg", "SH": "cn", "SZ": "cn", "JP": "jp"}
	venues         = map[string]string{"HK": "SEHK", "US": "US", "SG": "SGX", "SH": "SSE", "SZ": "SZSE", "JP": "TSE"}
	currencies     = map[string]string{"HK": "HKD", "US": "USD", "SG": "SGD", "SH": "CNH", "SZ": "CNH", "JP": "JPY"}
	marketZones    = map[string]string{"HK": "Asia/Hong_Kong", "US": "America/New_York", "SG": "Asia/Singapore", "SH": "Asia/Shanghai", "SZ": "Asia/Shanghai", "JP": "Asia/Tokyo"}
)

// secTypeOf reads a held code's type: a position row names none. A listing does
// not change type, so a type once read is kept; one OpenD did not give is unknown
// and read again next time.
func (a *Adapter) secTypeOf(ctx context.Context, code string) string {
	a.usVenuesMu.Lock()
	known := a.secTypes[code]
	a.usVenuesMu.Unlock()
	if known != "" {
		return known
	}
	rows, err := a.conn.Instruments(ctx, code)
	if err != nil || len(rows) == 0 {
		return secTypeUnknown
	}
	secType := secTypeOfOpenD(rows[0].Type)
	if secType != secTypeUnknown {
		a.usVenuesMu.Lock()
		if a.secTypes == nil {
			a.secTypes = make(map[string]string)
		}
		a.secTypes[code] = secType
		a.usVenuesMu.Unlock()
	}
	return secType
}

// secTypeOfOpenD is the seam's name for a type in OpenD's static information. A
// trust is how OpenD types an ETF, and an ETF is a stock on the seam.
func secTypeOfOpenD(openD string) string {
	switch openD {
	case "Eqty", "Trust":
		return "STK"
	case "Drvt":
		return "OPT"
	case "Future":
		return "FUT"
	case "Warrant", "Bwrt":
		return "WAR"
	case "Bond":
		return "BOND"
	}
	return secTypeUnknown
}

// instrumentOf is the seam's identity of an OpenD code asked for as a stock.
// Symbol is the listing symbol without its market prefix.
func instrumentOf(code string) broker.Instrument {
	prefix, symbol, _ := strings.Cut(code, ".")
	return broker.Instrument{
		Broker:   broker.KindOpenD,
		ID:       code,
		Symbol:   symbol,
		SecType:  "STK",
		Market:   marketFamilies[prefix],
		Venue:    venues[prefix],
		Currency: currencies[prefix],
	}
}

// secTypeUnknown is a holding whose type OpenD has not told us.
const secTypeUnknown = "UNKNOWN"

// ErrNotServed is an instrument OpenD's seam does not serve: options, futures,
// bonds and combinations reach OpenD in a later phase.
var ErrNotServed = errors.New("instrument not served on OpenD")

// codeOf finds the OpenD code of an instrument: its own id when it has one,
// else its symbol under its venue, market or currency. A symbol with nothing
// to say which market lists it reads as a code when it carries a prefix, as
// Hong Kong when it is all digits, and as US otherwise; a Singapore name needs
// its code, its venue or its currency.
func codeOf(in broker.Instrument) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(in.SecType)) {
	case "", "STK", "ETF":
	default:
		return "", fmt.Errorf("%w: %s", ErrNotServed, in)
	}
	if id := strings.TrimSpace(in.ID); id != "" && strings.Contains(id, ".") && (in.Broker == "" || in.Broker.Normalized() == broker.KindOpenD) {
		return id, nil
	}
	symbol := strings.ToUpper(strings.TrimSpace(in.Symbol))
	if symbol == "" {
		return "", fmt.Errorf("%w: no symbol", futu.ErrInvalidCode)
	}
	if strings.Contains(symbol, ".") {
		return symbol, nil
	}
	// Shanghai and Shenzhen share a market family and a currency, so only the
	// venue tells them apart; a digit symbol in that family must not read as
	// Hong Kong.
	for _, prefix := range []string{"SH", "SZ"} {
		for _, venue := range []string{in.Venue, in.Primary} {
			if strings.EqualFold(strings.TrimSpace(venue), venues[prefix]) {
				return prefix + "." + symbol, nil
			}
		}
	}
	for _, try := range []struct {
		value string
		table map[string]string
	}{{in.Venue, venues}, {in.Primary, venues}, {in.Market, marketFamilies}, {in.Currency, currencies}} {
		want := strings.ToUpper(strings.TrimSpace(try.value))
		if want == "" {
			continue
		}
		for _, prefix := range []string{"HK", "US", "SG", "JP"} {
			if strings.ToUpper(try.table[prefix]) == want {
				return prefix + "." + symbol, nil
			}
		}
		if strings.ToUpper(try.table["SH"]) == want {
			return "", fmt.Errorf("%w: %s needs its code or its venue, SSE or SZSE", futu.ErrInvalidCode, symbol)
		}
	}
	return bareSymbolCode(symbol), nil
}

func bareSymbolCode(symbol string) string {
	if strings.Contains(symbol, ".") {
		return symbol
	}
	if strings.Trim(symbol, "0123456789") == "" {
		return "HK." + symbol
	}
	return "US." + symbol
}

// Listed gives each instrument's identity with its listing venue, or a zero
// Instrument when it cannot name one. A US line's exchange comes from OpenD's
// static information and is read once; every other market has one venue.
func (a *Adapter) Listed(ctx context.Context, in []broker.Instrument) []broker.Instrument {
	out := make([]broker.Instrument, len(in))
	codes := make([]string, len(in))
	var missing []string
	a.usVenuesMu.Lock()
	for i, instrument := range in {
		code, err := codeOf(instrument)
		if err != nil {
			continue
		}
		codes[i] = code
		if strings.HasPrefix(code, "US.") && a.usVenues[code] == "" && !slices.Contains(missing, code) {
			missing = append(missing, code)
		}
	}
	a.usVenuesMu.Unlock()
	if len(missing) > 0 {
		if rows, err := a.conn.Instruments(ctx, missing...); err == nil {
			a.usVenuesMu.Lock()
			if a.usVenues == nil {
				a.usVenues = make(map[string]string, len(rows))
			}
			for _, row := range rows {
				if row.USVenue != "" {
					a.usVenues[row.Code] = row.USVenue
				}
			}
			a.usVenuesMu.Unlock()
		}
	}
	a.usVenuesMu.Lock()
	defer a.usVenuesMu.Unlock()
	for i, code := range codes {
		if code == "" {
			continue
		}
		out[i] = instrumentOf(code)
		if strings.HasPrefix(code, "US.") {
			out[i].Venue = a.usVenues[code]
		}
		if out[i].Venue == "" {
			out[i] = broker.Instrument{}
		}
	}
	return out
}

// Key is the subscription key: the code. Empty when it cannot be subscribed.
func (a *Adapter) Key(in broker.Instrument) string {
	code, err := codeOf(in)
	if err != nil {
		return ""
	}
	return code
}

// SubscribeSymbol takes a line for a bare symbol or a code. OpenD has no default
// route, so the adapter resolves the symbol by the rule on [codeOf].
func (a *Adapter) SubscribeSymbol(ctx context.Context, symbol string, _ []string) (string, broker.SessionBinding, error) {
	return a.subscribe(ctx, bareSymbolCode(strings.ToUpper(strings.TrimSpace(symbol))))
}

// Subscribe takes a line for an exact instrument.
func (a *Adapter) Subscribe(ctx context.Context, in broker.Instrument, _ []string) (string, broker.SessionBinding, error) {
	code, err := codeOf(in)
	if err != nil {
		return "", broker.SessionBinding{}, err
	}
	return a.subscribe(ctx, code)
}

// Unsubscribe releases a line. Inside the documented minimum hold it answers
// [broker.ReleaseHeldError], for the caller to defer.
func (a *Adapter) Unsubscribe(ctx context.Context, key string) error {
	return heldRelease(a.conn.Unsubscribe(ctx, key))
}

// UnsubscribeForSession releases a line only while the binding is still the
// session that opened it; a closed session's lines are already gone.
func (a *Adapter) UnsubscribeForSession(ctx context.Context, binding broker.SessionBinding, key string) error {
	if !a.Current(binding) {
		return nil
	}
	return heldRelease(a.conn.Unsubscribe(ctx, key))
}

// Snapshot is the cache of every held line, keyed by subscription key.
func (a *Adapter) Snapshot() map[string]*broker.Quote {
	cache := a.conn.QuoteCache()
	out := make(map[string]*broker.Quote, len(cache))
	for code, quote := range cache {
		out[code] = quoteOf(quote)
	}
	return out
}

// ReadSnapshots reads any number of codes through OpenD's snapshot call, the
// source for names without a streaming line. A window spent partway returns the
// batches read with the pacing error.
func (a *Adapter) ReadSnapshots(ctx context.Context, codes []string) (map[string]*broker.Quote, error) {
	rows, err := a.conn.Snapshots(ctx, codes...)
	out := make(map[string]*broker.Quote, len(rows))
	for _, row := range rows {
		out[row.Code] = quoteOf(row)
	}
	return out, err
}

// BriefQuote reads one snapshot: OpenD answers a snapshot request without a
// line, so nothing is opened, held or released, and done sees the one answer.
// An instrument kind OpenD does not quote (an index, a currency pair) is
// refused without a request.
func (a *Adapter) BriefQuote(ctx context.Context, in broker.Instrument, _ []string, timeout time.Duration, done func(broker.BriefQuote) bool) (broker.BriefQuote, error) {
	code, err := suppliedCode(in)
	if err != nil {
		return broker.BriefQuote{}, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	rows, err := a.conn.Snapshots(ctx, code)
	if err != nil || len(rows) == 0 {
		return broker.BriefQuote{}, quoteRightRefusal(err)
	}
	q := rows[0]
	seen := broker.BriefQuote{
		Bid: q.Bid, Ask: q.Ask, Last: q.Last, Mark: q.Last, Close: q.PrevClose, Week52High: q.Week52High,
		FeedType:   1,
		ObservedAt: q.At,
	}
	if done != nil {
		done(seen)
	}
	return seen, nil
}

// DailyBars reads daily bars from OpenD's history call; an instrument kind
// OpenD does not quote is refused without a request.
func (a *Adapter) DailyBars(ctx context.Context, in broker.Instrument, days int) ([]broker.Bar, error) {
	code, err := suppliedCode(in)
	if err != nil {
		return nil, err
	}
	to := a.now()
	bars, err := a.history(ctx, code, futu.BarDay, to.AddDate(0, 0, -max(days, 1)), to, 0)
	return bars, quoteRightRefusal(err)
}

// quoteRightRefusal names a refusal for a missing quote right in the broker's
// neutral terms; any other error passes through.
func quoteRightRefusal(err error) error {
	if refusal, ok := errors.AsType[*futu.QuoteRightError](err); ok {
		return &broker.NoQuoteRightError{Market: refusal.Market, Err: err}
	}
	return err
}

// suppliedCode is codeOf for a read that names what is missing: a kind OpenD
// does not serve wraps [broker.ErrNotSupplied] as well as ErrNotServed.
func suppliedCode(in broker.Instrument) (string, error) {
	code, err := codeOf(in)
	if errors.Is(err, ErrNotServed) {
		return "", fmt.Errorf("%w: %w", broker.ErrNotSupplied, err)
	}
	return code, err
}

// CodeFor is the code an instrument reads under, for a caller that batches.
func (a *Adapter) CodeFor(in broker.Instrument) (string, error) { return codeOf(in) }

func quoteOf(q futu.Quote) *broker.Quote {
	out := &broker.Quote{
		Symbol:         q.Code,
		FeedType:       1,
		Delivery:       string(q.Delivery),
		Timestamp:      q.At,
		LastAt:         q.At,
		MarkAt:         q.At,
		CloseAt:        q.At,
		Last:           q.Last,
		Bid:            q.Bid,
		Ask:            q.Ask,
		MarkPrice:      q.Last,
		Open:           q.Open,
		High:           q.High,
		Low:            q.Low,
		Close:          q.PrevClose,
		Volume:         q.Volume,
		VolumeObserved: true,
		VolumeAt:       q.At,
		BidSize:        int(q.BidSize),
		AskSize:        int(q.AskSize),
		LastTickAt:     q.At,
		LastPriceTick:  q.At,
		LastTradeTime:  q.At,
	}
	if q.Bid > 0 {
		out.BidAt = q.At
	}
	if q.Ask > 0 {
		out.AskAt = q.At
	}
	if q.Bid > 0 && q.Ask > 0 {
		out.Mid = (q.Bid + q.Ask) / 2
	}
	return out
}

// Load reports the quota as OpenD last reported it.
func (a *Adapter) Load() (broker.Load, bool) {
	load := a.conn.Load()
	if load.SubscriptionQuota == 0 && load.HistoryQuota == 0 {
		return broker.Load{}, false
	}
	return broker.Load{
		SubscriptionsUsed: load.SubscriptionsUsed,
		SubscriptionQuota: load.SubscriptionQuota,
		HistoryUsed:       load.HistoryUsed,
		HistoryQuota:      load.HistoryQuota,
		LastRefusalAt:     load.LastRefusalAt,
		LastRefusal:       load.LastRefusal,
	}, true
}

// chartPeriods maps the daemon's chart intervals to OpenD's candle lengths.
var chartPeriods = map[string]futu.BarPeriod{
	"1 min": futu.Bar1Min, "1m": futu.Bar1Min,
	"3 mins": futu.Bar3Min, "3m": futu.Bar3Min,
	"5 mins": futu.Bar5Min, "5m": futu.Bar5Min,
	"15 mins": futu.Bar15Min, "15m": futu.Bar15Min,
	"30 mins": futu.Bar30Min, "30m": futu.Bar30Min,
	"1 hour": futu.Bar60Min, "1h": futu.Bar60Min, "60m": futu.Bar60Min,
	"1 day": futu.BarDay, "1d": futu.BarDay,
	"1 week": futu.BarWeek, "1w": futu.BarWeek,
	"1 month": futu.BarMonth, "1mo": futu.BarMonth,
}

// ChartBars reads a window of days at an interval, from OpenD's history call.
func (a *Adapter) ChartBars(ctx context.Context, in broker.Instrument, days int, interval string, timeout time.Duration) (broker.BarSeries, error) {
	code, err := codeOf(in)
	if err != nil {
		return broker.BarSeries{}, err
	}
	period, ok := chartPeriods[strings.ToLower(strings.TrimSpace(interval))]
	if !ok {
		return broker.BarSeries{}, fmt.Errorf("opend chart interval %q", interval)
	}
	to := a.now()
	bars, err := a.history(ctx, code, period, to.AddDate(0, 0, -max(days, 1)), to, timeout)
	if err != nil {
		return broker.BarSeries{}, err
	}
	return broker.BarSeries{Instrument: instrumentOf(code), Bars: bars, WhatToShow: "TRADES"}, nil
}

// SessionBars reads five-minute bars between two instants.
func (a *Adapter) SessionBars(ctx context.Context, in broker.Instrument, from, to time.Time, timeout time.Duration) ([]broker.Bar, error) {
	code, err := codeOf(in)
	if err != nil {
		return nil, err
	}
	return a.history(ctx, code, futu.Bar5Min, from, to, timeout)
}

func (a *Adapter) history(ctx context.Context, code string, period futu.BarPeriod, from, to time.Time, timeout time.Duration) ([]broker.Bar, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	prefix, _, _ := strings.Cut(code, ".")
	zone := time.UTC
	if name, ok := marketZones[prefix]; ok {
		if loc, err := time.LoadLocation(name); err == nil {
			zone = loc
		}
	}
	rows, err := a.conn.HistoryBars(ctx, code, period, from.In(zone), to.In(zone))
	if err != nil {
		return nil, err
	}
	bars := make([]broker.Bar, 0, len(rows))
	for _, row := range rows {
		if row.Blank {
			continue
		}
		bars = append(bars, broker.Bar{
			Time:   row.Time,
			Date:   row.Text,
			Open:   row.Open,
			High:   row.High,
			Low:    row.Low,
			Close:  row.Close,
			Volume: row.Volume,
		})
	}
	return bars, nil
}

// Resolve looks an instrument up for a session; a binding that is no longer
// current fails.
func (a *Adapter) Resolve(ctx context.Context, binding broker.SessionBinding, in broker.Instrument, timeout time.Duration) (broker.Instrument, error) {
	if !a.Current(binding) {
		return broker.Instrument{}, futu.ErrNotConnected
	}
	code, err := codeOf(in)
	if err != nil {
		return broker.Instrument{}, err
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	rows, err := a.conn.Instruments(ctx, code)
	if err != nil {
		return broker.Instrument{}, err
	}
	if len(rows) == 0 {
		return broker.Instrument{}, fmt.Errorf("%w: %s", futu.ErrInvalidCode, code)
	}
	out := instrumentOf(rows[0].Code)
	out.SecType = secTypeOfOpenD(rows[0].Type)
	if !a.Current(binding) {
		return broker.Instrument{}, futu.ErrNotConnected
	}
	return out, nil
}
