package broker

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Kind names a broker. The zero value means IBKR, so a persisted record or
// configuration that predates the second broker keeps its meaning.
type Kind string

const (
	// KindIBKR is Interactive Brokers through TWS or IB Gateway.
	KindIBKR Kind = "ibkr"
	// KindOpenD is Futu's OpenD gateway (moomoo, Futubull).
	KindOpenD Kind = "opend"
)

// Normalized returns the kind with the zero value read as IBKR.
func (k Kind) Normalized() Kind {
	if strings.TrimSpace(string(k)) == "" {
		return KindIBKR
	}
	return Kind(strings.ToLower(strings.TrimSpace(string(k))))
}

// Valid reports whether k names a broker this build knows.
func (k Kind) Valid() bool {
	switch k.Normalized() {
	case KindIBKR, KindOpenD:
		return true
	}
	return false
}

// String returns the normalized kind.
func (k Kind) String() string { return string(k.Normalized()) }

// Leg is one exact instrument inside a combination (an IBKR BAG). Ratio is a
// positive integer; Action is the leg's held direction.
type Leg struct {
	ID        string
	Ratio     int
	Action    string
	Venue     string
	OpenClose int
}

// Instrument is the broker-neutral identity of one tradable thing. ID is the
// broker's own identifier: the ConID as text for IBKR, the market-prefixed
// code ("HK.00700") for OpenD. The remaining fields carry what the daemon
// reads off a contract today, so a mapping to a broker client's type and back
// is total for every security type the daemon uses.
type Instrument struct {
	Broker   Kind
	ID       string
	Symbol   string
	SecType  string // STK | OPT | FUT | IND | BOND | BILL | CASH | BAG, as rpc uses them
	Market   string // us | de | hk | sg | cn | jp: the route family rpc.ContractParams.Market carries
	Venue    string // the routing or listing venue as the broker names it (SMART, SEHK, NASDAQ, SGX, XETRA)
	Primary  string // the primary listing venue when the route is a smart route
	Currency string
	// LocalSymbol and TradingClass are the broker's own listing names; IBKR
	// keys option classes and bonds by them.
	LocalSymbol  string
	TradingClass string
	// Option and future fields as today.
	Expiry     string
	Strike     float64
	Right      string
	Multiplier int
	// SecIDType and SecID name an instrument by an external identifier
	// (ISIN for bonds).
	SecIDType string
	SecID     string
	// Legs are a combination's legs; empty for a single instrument.
	Legs []Leg
}

// String is the canonical "<kind>:<id>" form, "ibkr:265598" or
// "opend:HK.00700". An instrument without a broker id names itself by its
// symbol and security type so a log line stays readable.
func (in Instrument) String() string {
	kind := in.Broker.Normalized()
	if id := strings.TrimSpace(in.ID); id != "" {
		return string(kind) + ":" + id
	}
	sec := strings.ToUpper(strings.TrimSpace(in.SecType))
	if sec == "" {
		sec = "STK"
	}
	return string(kind) + ":" + strings.ToUpper(strings.TrimSpace(in.Symbol)) + "/" + sec
}

// IDInt returns the broker id as an integer when it is one (an IBKR ConID).
func (in Instrument) IDInt() (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(in.ID))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// SessionBinding is a value: the broker kind, the daemon's generation of the
// connector it was captured on, and that connector's own session epoch. It
// carries nothing to dereference. The zero value is never current: adapters
// read the generation after the daemon increments it, so a real binding
// always carries a generation of at least one.
type SessionBinding struct {
	Kind       Kind
	Generation uint64
	Epoch      uint64
}

// IsZero reports whether b is the zero binding.
func (b SessionBinding) IsZero() bool { return b == SessionBinding{} }

// SessionHealth is the broker session as the status surface reads it. It
// holds no reference to the connection.
type SessionHealth struct {
	Kind Kind
	// Connected is the transport; Ready means the session can serve reads;
	// BackendDown means the broker's own link behind its gateway is lost
	// while the socket is up.
	Connected   bool
	Ready       bool
	BackendDown bool
	// Account is the account the broker confirmed for the session, as the
	// broker spells it (for IBKR the managed-accounts answer, possibly a
	// comma-separated list for a multi-account login).
	Account string
	// ServerVersion is the broker's protocol or server version.
	ServerVersion int
	// LastError is the last connect or session error as text; broker free
	// text is data, never a decision.
	LastError string
}

// Session is the broker session as every read binds to it.
type Session interface {
	// Capture returns the binding of the current ready session, or false
	// when there is none.
	Capture() (SessionBinding, bool)
	// Current reports whether binding still names the ready session.
	Current(SessionBinding) bool
	// Health describes the session for the status surface.
	Health() SessionHealth
}

// Provenance says how an account summary was obtained.
type Provenance string

const (
	// ProvenanceRequest means a one-shot request supplied a complete row set.
	ProvenanceRequest Provenance = "request"
	// ProvenanceCachedFallback means an unstamped streaming cache answered.
	ProvenanceCachedFallback Provenance = "cached_fallback"
)

// BaseCurrencyProvenance names the evidence that proved an account's base
// currency.
type BaseCurrencyProvenance string

const (
	// BaseCurrencyUnknown means no broker field proved the base currency.
	BaseCurrencyUnknown BaseCurrencyProvenance = "unknown"
	// BaseCurrencyExplicitTag means a dedicated currency field supplied it.
	BaseCurrencyExplicitTag BaseCurrencyProvenance = "explicit_currency_tag"
	// BaseCurrencyValueSuffix means an aggregate value's currency suffix did.
	BaseCurrencyValueSuffix BaseCurrencyProvenance = "account_value_suffix"
	// BaseCurrencyUnitExchangeRate is kept for wire compatibility; it never
	// proves the base currency.
	BaseCurrencyUnitExchangeRate BaseCurrencyProvenance = "unit_exchange_rate"
)

// Proven reports whether a broker field established the base currency.
func (p BaseCurrencyProvenance) Proven() bool {
	return p == BaseCurrencyExplicitTag || p == BaseCurrencyValueSuffix
}

// CurrencyLedger is one currency's rollup inside an account summary.
type CurrencyLedger struct {
	NetLiquidationObserved   bool
	ExchangeRateObserved     bool
	NetLiquidationByCurrency float64
	CashBalance              float64
	StockMarketValueText     string
	OptionMarketValueText    string
	StockMarketValue         float64
	OptionMarketValue        float64
	UnrealizedPnL            float64
	RealizedPnL              float64
	ExchangeRate             float64
	SettledCash              float64
	SettledCashObserved      bool
	// ExchangeRateImplied says the rate is worked out from the account's own totals,
	// not quoted. It measures a holding; it is not authority for sizing an order.
	ExchangeRateImplied bool
}

// SettlementRow is one currency's settlement observation: the broker's
// label and whether the value was finite, never the value.
type SettlementRow struct {
	Currency string
	Source   string
	Finite   bool
}

// SettlementObservation is sanitized callback provenance for cash
// settlement, never cash authority.
type SettlementObservation struct {
	AsOf      time.Time
	Callbacks int
	Rows      []SettlementRow
}

// AccountSummary is one account's summary as the broker answered it. Money
// fields are pointers: nil means the broker did not send the field, never
// zero. Raw keeps the broker's own keyed values for diagnostics.
type AccountSummary struct {
	AccountID            string
	AccountType          string
	NetLiquidation       *float64
	BuyingPower          *float64
	AvailableFunds       *float64
	ExcessLiquidity      *float64
	TotalCashValue       *float64
	MaintenanceMargin    *float64
	InitMarginReq        *float64
	GrossPositionValue   *float64
	UnrealizedPnL        *float64
	RealizedPnL          *float64
	Cushion              *float64
	LookAheadInitMargin  *float64
	LookAheadMaintMargin *float64
	LookAheadAvailable   *float64
	LookAheadExcess      *float64
	// Currency is the deterministic fallback unit for numeric rows; it is
	// never proof of the base currency. BaseCurrency and its provenance are.
	Currency               string
	BaseCurrency           string
	BaseCurrencyProvenance BaseCurrencyProvenance
	CurrencyLedger         map[string]CurrencyLedger
	Settlement             *SettlementObservation
	AsOf                   time.Time
	Raw                    map[string]string
}

// Account reads one account's summary.
type Account interface {
	// Summary performs one bounded summary read of the pinned account.
	Summary(ctx context.Context, timeout time.Duration) (*AccountSummary, Provenance, error)
}

// Position is one held line as the broker's portfolio stream reports it.
type Position struct {
	ValuationAt   time.Time
	Account       string
	Instrument    Instrument
	Quantity      float64
	MarketPrice   float64
	MarketValue   float64
	AverageCost   float64
	UnrealizedPnL float64
	RealizedPnL   float64
}

// PositionsHealth is the portfolio stream's own receipt: whether the
// download completed, when it last moved, and the conflicts it recorded.
type PositionsHealth struct {
	Account            string
	RequestedAt        time.Time
	InitialCompletedAt time.Time
	LastUpdateAt       time.Time
	// ProjectionGeneration advances only when the structural portfolio
	// changes: scope, completeness, contract set or held quantities.
	ProjectionGeneration uint64
	// ScopeConflictAt is set when the stream reported a blank or foreign
	// account; InvalidPayloadAt when a generation was malformed;
	// DownloadShortAt when a completed download did not foot.
	ScopeConflictAt  time.Time
	InvalidPayloadAt time.Time
	DownloadShortAt  time.Time
}

// Positions reads the held book from the broker's cache.
type Positions interface {
	Positions() ([]Position, PositionsHealth, error)
}

// Quote is one instrument's quote line as the broker's cache holds it.
// FeedType is the broker's data mode (for IBKR 1 live to 4 delayed-frozen).
// Delivery says whether the line streams or came from a snapshot batch.
type Quote struct {
	Symbol    string
	FeedType  int
	Delivery  string
	Timestamp time.Time
	LastAt    time.Time
	BidAt     time.Time
	AskAt     time.Time
	MarkAt    time.Time
	CloseAt   time.Time

	Last      float64
	Bid       float64
	Ask       float64
	Mid       float64
	MarkPrice float64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	VWAP      float64

	Volume         int64
	VolumeObserved bool
	VolumeAt       time.Time
	AvgVolume      int64
	BidSize        int
	AskSize        int
	LastTickAt     time.Time
	LastPriceTick  time.Time
	LastTradeTime  time.Time
}

// ErrSnapshotOnly is an instrument the broker's budget gives no streaming line:
// it reads as one snapshot. It is a delivery, not a failure.
var ErrSnapshotOnly = errors.New("no streaming line for this instrument: it is served by snapshot")

// ReleaseHeldError is a release the broker refuses inside its minimum hold; the
// manager releases the line again after RetryAfter.
type ReleaseHeldError struct {
	Key        string
	RetryAfter time.Duration
}

// Error names the line and the wait.
func (e *ReleaseHeldError) Error() string {
	return "line " + e.Key + " is inside its minimum hold for another " + e.RetryAfter.String()
}

// Load is the broker's line and quota usage as the status surface reads it.
// Quotas are the broker's own facts, shared with every other client of the
// same gateway, so a refusal is reported and never predicted from them.
type Load struct {
	SubscriptionsUsed int
	SubscriptionQuota int
	HistoryUsed       int
	HistoryQuota      int
	LastRefusalAt     time.Time
	LastRefusal       string
}

// Quotes is the streaming-quote surface the daemon's subscription manager
// drives. Keys are the broker's own subscription keys: for IBKR the symbol
// or routed contract key the manager has always keyed on, so one line per
// key is held exactly as today.
type Quotes interface {
	// Key returns the subscription key the broker files in under; empty
	// when the instrument cannot be subscribed.
	Key(in Instrument) string
	// SubscribeSymbol opens a line by bare symbol (the broker resolves the
	// default route). Subscribe opens a line for an exact instrument. Both
	// return the key and the session binding the line belongs to.
	SubscribeSymbol(ctx context.Context, symbol string, fields []string) (key string, binding SessionBinding, err error)
	Subscribe(ctx context.Context, in Instrument, fields []string) (key string, binding SessionBinding, err error)
	// Unsubscribe releases a line regardless of session. UnsubscribeForSession
	// releases a shared line only while binding is still the session that
	// opened it.
	Unsubscribe(ctx context.Context, key string) error
	UnsubscribeForSession(ctx context.Context, binding SessionBinding, key string) error
	// Snapshot is the cache of every held line, keyed by subscription key.
	Snapshot() map[string]*Quote
	// Load reports lines held and quota, when the broker knows them.
	Load() (Load, bool)
}

// Bar is one historical bar. BarCount is the broker's trade count for the
// bar: IBKR sends -1 for none, and 0 beside a positive volume means unknown.
type Bar struct {
	Time     time.Time
	Date     string
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   int64
	Average  float64
	BarCount int
}

// BarSeries is a chart read: the instrument the bars belong to and what the
// bars show (trades, midpoint, or the broker's own name for it).
type BarSeries struct {
	Instrument Instrument
	Bars       []Bar
	WhatToShow string
}

// Bars reads history. ChartBars is the chart shape (a window of days at an
// interval); SessionBars is the intraday session shape (five-minute bars
// between two instants) the setup evaluations read.
type Bars interface {
	ChartBars(ctx context.Context, in Instrument, days int, interval string, timeout time.Duration) (BarSeries, error)
	SessionBars(ctx context.Context, in Instrument, from, to time.Time, timeout time.Duration) ([]Bar, error)
}

// DailyBars reads daily bars for an instrument named by symbol and kind, the
// regime's history read. IBKR keys it by symbol, resolving the default route
// as it always has; a broker that does not supply the kind answers an error
// wrapping [ErrNotSupplied].
type DailyBars interface {
	DailyBars(ctx context.Context, in Instrument, days int) ([]Bar, error)
}

// BriefQuote is the line as one brief look sees it: the price ticks, the
// previous regular-session close, the 52-week high when it was asked for, the
// broker's data mode (FeedType, numbered as on [Quote]) and when the last
// price was observed.
type BriefQuote struct {
	Bid, Ask, Last, Mark, Close, Week52High float64
	FeedType                                int
	ObservedAt                              time.Time
}

// Snapshots takes one brief look at an instrument named by symbol and kind:
// it hands done the line as it stands at each read until done says it holds
// enough or timeout passes, and releases what it opened. The last line read
// is returned with any error from opening it. A broker that does not supply
// the kind answers an error wrapping [ErrNotSupplied] and asks nothing of the
// wire.
type Snapshots interface {
	BriefQuote(ctx context.Context, in Instrument, fields []string, timeout time.Duration, done func(BriefQuote) bool) (BriefQuote, error)
}

// ErrNotSupplied is an instrument kind this broker gives no data for, such as
// an index or a currency pair on OpenD.
var ErrNotSupplied = errors.New("this broker does not supply the instrument")

// NoQuoteRightError is a quote the broker refused because the login holds no
// quote right for the instrument's market. Market is the broker's market name
// (US, HK, SG).
type NoQuoteRightError struct {
	Market string
	Err    error
}

// Error names the market and the broker's own refusal.
func (e *NoQuoteRightError) Error() string {
	return "the login has no " + e.Market + " quote right: " + e.Err.Error()
}

// Unwrap is the broker's own refusal.
func (e *NoQuoteRightError) Unwrap() error { return e.Err }

// Instruments resolves a requested instrument to the broker's exact one for
// a session. A binding that is no longer current fails.
type Instruments interface {
	Resolve(ctx context.Context, binding SessionBinding, in Instrument, timeout time.Duration) (Instrument, error)
}

// Scope is the identity scoped state binds to: the broker kind, the account and
// the account's paper or live mode, read from the instance's own current
// observation.
type Scope struct {
	Kind    Kind
	Account string
	Mode    string
}

// Scoper yields the current scope. It lives as long as the daemon: a site with no
// session still has one, and a daemon holding N brokers later changes this one
// source and none of the code that reads a scope.
type Scoper interface {
	Scope() Scope
}

// Reads is every read seam together, with the kind it serves.
type Reads interface {
	Kind() Kind
	Session
	Account
	Positions
	Quotes
	Snapshots
	Bars
	DailyBars
	Instruments
}

// Broker is one broker as a daemon holds it: its reads and its scope. The IBKR
// reads are served per connector and its scope per daemon, because a rebuilt
// connector invalidates a session binding and never the identity.
type Broker interface {
	Reads
	Scoper
}
