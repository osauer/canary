// Package rpc defines the stable method names, envelopes, and typed payloads
// shared by the daemon and its CLI, MCP, and app adapters.
//
// The daemon protocol is custom newline-delimited JSON, not JSON-RPC 2.0. Each
// request and response is one JSON object. An id correlates replies, ok selects
// either result or error, and a streaming response emits frame objects followed
// by an end marker. Types in this package are transport contracts; runtime and
// broker authority remain in the daemon.
package rpc

import (
	"encoding/json"
	"strings"
	"time"
)

// Daemon method names are stable wire identifiers shared by every adapter.
const (
	MethodAccountSummary      = "account.summary"
	MethodPositionsList       = "positions.list"
	MethodQuoteSnapshot       = "quote.snapshot"
	MethodQuoteSubscribe      = "quote.subscribe"
	MethodChainFetch          = "chain.fetch"
	MethodChainExpiries       = "chain.expiries"
	MethodScanRun             = "scan.run"
	MethodScanList            = "scan.list"
	MethodScanParams          = "scan.params"
	MethodHistoryDaily        = "history.daily"
	MethodTechnical           = "technical.snapshot"
	MethodMarketCalendar      = "market.calendar"
	MethodStatusHealth        = "status.health"
	MethodTradingStatus       = "trading.status"
	MethodTradingPaperSmoke   = "trading.paper_smoke"
	MethodSettingsGet         = "settings.get"
	MethodSettingsUpdate      = "settings.update"
	MethodOrdersOpen          = "orders.open"
	MethodOrdersHistory       = "orders.history"
	MethodOrderStatus         = "order.status"
	MethodOrderPreview        = "order.preview"
	MethodBreadthSPX          = "breadth.spx"
	MethodGammaZeroSPX        = "gamma.zero_spx"
	MethodRegimeSnapshot      = "regime.snapshot"
	MethodCancel              = "cancel"
	MethodOrderPlace          = "order.place"
	MethodOrderModify         = "order.modify"
	MethodOrderCancel         = "order.cancel"
	MethodPurgeStatus         = "purge.status"
	MethodPurgeExecute        = "purge.execute"
	MethodPurgeRestorePreview = "purge.restore.preview"
	MethodPurgeRestoreExecute = "purge.restore.execute"
)

// Error codes classify terminal request failures carried by Error.Code.
const (
	CodeUnknownMethod      = "unknown_method"
	CodeBadRequest         = "bad_request"
	CodeDaemonUnavailable  = "daemon_unavailable"
	CodeGatewayUnavailable = "gateway_unavailable"
	CodeSymbolInactive     = "symbol_inactive"
	CodeTimeout            = "timeout"
	CodeTradingDisabled    = "trading_disabled"
	CodeInternal           = "internal"
)

// CodeRegimeUnavailable means the daemon has no complete, validated
// last-good regime snapshot to serve. It is deliberately distinct from
// gateway_unavailable: a disconnected gateway does not make a persisted
// last-good snapshot disappear, while a cold authority cannot return a
// partial dashboard as if it were current state.
const CodeRegimeUnavailable = "regime_unavailable"

// MarketDataType values carried on Quote.DataType, Frame.DataType, and
// ChainResult.DataType. IBKR's tickMarketDataType message (58) maps
// gateway feed state into one of these strings; adapters render a badge
// based on the value. HealthResult.DataType remains on the wire shape
// (omitempty) for renderer-fallback compatibility but is no longer
// written by the daemon: status has no per-request data type to report.
//
// Empty string means "the gateway hasn't sent a notice yet" — typically a
// few hundred ms after a fresh subscription. Treated as live for
// rendering purposes (see IsLiveDataType).
const (
	MarketDataLive          = "live"
	MarketDataFrozen        = "frozen"
	MarketDataDelayed       = "delayed"
	MarketDataDelayedFrozen = "delayed-frozen"
	MarketDataPrevClose     = "prev_close"
	MarketDataClosed        = "closed"
)

// IsLiveDataType reports whether the gateway's per-reqID feed state is
// "live ticks", treating empty-string the same as live (no notice yet).
// Used by renderers to decide whether to dim a row or show a phase badge.
func IsLiveDataType(dt string) bool {
	return dt == "" || dt == MarketDataLive
}

// IsOptionRTH reports whether the given instant falls within U.S. listed-
// equity-option regular trading hours: weekdays 09:30–16:00 ET.
//
// Used in preference to IsLiveDataType for option-context renderers (chain,
// option quotes) because the underlying ETF can stay "live" via extended-
// hours quoting on SMART/ARCA while the option markets themselves are
// closed — IsLiveDataType won't fire in that window, but the chain has
// no bid/ask and IVs come from IBKR's model-computation engine off
// prior-session prices. The clock-gated check captures that state.
//
// Holidays are NOT modeled. The fall-through is "open" on those days; the
// existing model-tick → BS-IV fallback chain keeps results usable, and a
// missed disclosure on a holiday is preferable to mis-flagging a regular
// session.
//
// Display cadence only — never use this for policy blockers or eligibility
// gates. Those must go through a marketcal-backed authority (the daemon's
// optionSessionOpen), which models holidays, early closes, and the 16:15
// options close, and reports unknown coverage instead of guessing.
//
// Fail-open: if the America/New_York zone can't be loaded (e.g. tzdata
// missing in a minimal container), returns true so the banner stays
// suppressed rather than firing during RTH.
func IsOptionRTH(now time.Time) bool {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return true
	}
	t := now.In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, ny)
	closeT := time.Date(t.Year(), t.Month(), t.Day(), 16, 0, 0, 0, ny)
	return !t.Before(open) && t.Before(closeT)
}

// SessionClass classifies an instant by its U.S. equity-options session
// phase. Callers that need different cadence in pre vs RTH vs post vs
// closed (most prominently the gamma cache's session-aware soft-TTL)
// branch on this rather than re-deriving the boundaries themselves.
//
// Boundaries (America/New_York):
//   - Pre   : weekdays 04:00–09:30
//   - RTH   : weekdays 09:30–16:00
//   - Post  : weekdays 16:00–20:00
//   - Closed: everything else (overnight + weekends)
//
// Holidays are NOT modeled — same fall-through policy as IsOptionRTH.
type SessionClass int

// Session classes partition the U.S. equity-options day using the boundaries
// documented on [SessionClass].
const (
	SessionClosed SessionClass = iota
	SessionPre
	SessionRTH
	SessionPost
)

// String renders the session class for log lines and debug output.
// Not load-bearing on the wire (the gamma cache holds the enum value
// directly), but used in test failure messages and warning logs.
func (c SessionClass) String() string {
	switch c {
	case SessionPre:
		return "pre"
	case SessionRTH:
		return "rth"
	case SessionPost:
		return "post"
	default:
		return "closed"
	}
}

// ClassifySession returns the SessionClass containing now. Fail-safe:
// if America/New_York can't be loaded (minimal container, missing
// tzdata), returns SessionRTH — the broadest "treat as active" answer
// so refresh cadence isn't accidentally disabled under degraded zone
// data. Mirrors IsOptionRTH's fail-open policy.
func ClassifySession(now time.Time) SessionClass {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		return SessionRTH
	}
	t := now.In(ny)
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return SessionClosed
	}
	pre := time.Date(t.Year(), t.Month(), t.Day(), 4, 0, 0, 0, ny)
	open := time.Date(t.Year(), t.Month(), t.Day(), 9, 30, 0, 0, ny)
	closeT := time.Date(t.Year(), t.Month(), t.Day(), 16, 0, 0, 0, ny)
	post := time.Date(t.Year(), t.Month(), t.Day(), 20, 0, 0, 0, ny)
	switch {
	case t.Before(pre):
		return SessionClosed
	case t.Before(open):
		return SessionPre
	case t.Before(closeT):
		return SessionRTH
	case t.Before(post):
		return SessionPost
	default:
		return SessionClosed
	}
}

// Frame-level error codes used in FrameError.Code. These are terminal: a
// frame carrying any of these is the last frame the consumer will receive
// on its subscription. Distinct from the request-envelope error codes
// because the wire shape (frame, not Error) and lifecycle (mid-stream
// vs synchronous) are different concerns.
const (
	FrameErrGatewayLost          = "gateway_lost"
	FrameErrEntitlementLost      = "entitlement_lost"
	FrameErrSubscriptionRejected = "subscription_rejected"
	FrameErrDaemonShutdown       = "daemon_shutdown"
)

// SecType values carried on PositionView.SecType. The daemon maps IBKR's
// raw three-letter SecType codes ("STK", "OPT") onto the canonical wire
// values below in positionSecType — full words, not the short forms IBKR
// accepts on ContractParams (a different path; see the doc-comment there).
//
// Compare against these constants in renderers and filters rather than
// literal strings. The constants prevent adapters from drifting between
// broker and canonical spellings.
const (
	SecTypeStock  = "STOCK"
	SecTypeOption = "OPTION"
	SecTypeFuture = "FUTURE"
	SecTypeIndex  = "INDEX"
)

// PositionQuotesAsStock reports whether a non-option position row is an
// equity — the only secType for which a stock quote on the bare symbol
// describes the holding. The non-option slice of PositionsResult carries
// every secType that is not OPT (BOND, BILL, FUND, FUT, CASH); those rows
// share the slice because they are not options, not because they are stocks,
// and joining one to an equity surface by symbol decorates a treasury
// symbolled "T" with AT&T's quote. This is the one shared classification for
// that join — daemon, CLI, and MCP must all use it rather than re-deriving
// it from the slice name. Unknown and absent secTypes classify false: absence
// of a type is not stock authority.
func PositionQuotesAsStock(row PositionView) bool {
	switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
	case SecTypeStock, "STK", "ETF":
		return true
	default:
		return false
	}
}

// Request is one custom daemon-protocol request. Params contains the typed
// method payload; an absent Params value is distinct only where that method's
// decoder says so.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one custom daemon-protocol response. Unary success uses Result;
// streams emit Frame values and terminate with End. Error is terminal when Ok
// is false.
type Response struct {
	ID     string          `json:"id"`
	Ok     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Frame  json.RawMessage `json:"frame,omitempty"`
	Stream bool            `json:"stream,omitempty"`
	End    bool            `json:"end,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Error is the structured error payload for a failed request.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Fingerprint is a semantic identity for alert/dedupe surfaces. The Key is a
// stable sha256 over classified state, not raw prices, timestamps, or rendered
// prose. Monitors should use it to suppress duplicate alerts.
type Fingerprint struct {
	Version string `json:"version"`
	Key     string `json:"key"`
}

// Error implements the error interface so callers can return *Error.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// ContractParams names a tradeable instrument on the REQUEST side.
//
// Asymmetry to watch for: SecType here uses the IBKR API's three-letter
// short form ("STK", "OPT", "FUT", "IND") because that's what the gateway
// accepts in reqMktData / reqContractDetails. The RESPONSE side
// (PositionView.SecType) uses the full word ("STOCK", "OPTION", ...) —
// see the SecType* constants above. The two shapes flow on different
// paths and the gateway uses different vocabularies at each end; this
// type uses the request vocabulary.
//
// Market is an optional shorthand for stock routing defaults. Empty / "us"
// preserves the legacy US SMART/USD path; "de" selects the German/Xetra
// EUR route used by IBKR's IBIS listing codes. Exchange/Currency/PrimaryExch
// override the shorthand when supplied.
//
// SecType "STK" for stocks/ETFs; "OPT" for options (Expiry, Strike,
// Right required).
type ContractParams struct {
	ConID        int     `json:"con_id,omitempty"`
	Symbol       string  `json:"symbol"`
	SecType      string  `json:"sec_type,omitempty"` // STK | OPT | FUT | IND (request-side; see asymmetry note)
	Market       string  `json:"market,omitempty"`   // us | de
	Exchange     string  `json:"exchange,omitempty"` // SMART, IBIS, ...
	PrimaryExch  string  `json:"primary_exchange,omitempty"`
	Currency     string  `json:"currency,omitempty"`
	LocalSymbol  string  `json:"local_symbol,omitempty"`
	TradingClass string  `json:"trading_class,omitempty"`
	Expiry       string  `json:"expiry,omitempty"` // YYYYMMDD
	Strike       float64 `json:"strike,omitempty"`
	Right        string  `json:"right,omitempty"` // C | P
	Multiplier   int     `json:"multiplier,omitempty"`
	// MinTick is the venue minimum price increment, enriched daemon-side from
	// broker contract details when known. Zero means unresolved: price
	// rounding falls back to a static grid (US 0.01 / EUR MiFID band).
	MinTick float64 `json:"min_tick,omitempty"`
}

// QuoteSnapshotParams is the input for MethodQuoteSnapshot.
type QuoteSnapshotParams struct {
	Contract         ContractParams `json:"contract"`
	TimeoutMs        int            `json:"timeout_ms,omitempty"`
	IncludeLiquidity bool           `json:"include_liquidity,omitempty"`
}

// QuoteSubscribeParams is the input for MethodQuoteSubscribe.
type QuoteSubscribeParams struct {
	Contract ContractParams `json:"contract"`
}

// PositionsListParams filters the positions response. Both fields are
// honoured by the daemon (`internal/daemon/handlers.go::handlePositionsList`).
// Symbol matches the underlying (or the synthetic option key); empty returns
// every position. Type narrows to stocks ("stk") or options ("opt"); empty
// returns both. Filters are applied before the FX / Greeks decoration, so a
// narrowed query is also faster.
type PositionsListParams struct {
	Symbol string `json:"symbol,omitempty"`
	Type   string `json:"type,omitempty"` // stk | opt
}

// ChainFetchParams selects strikes around the spot price for an expiry.
type ChainFetchParams struct {
	Symbol       string `json:"symbol"`
	Expiry       string `json:"expiry"`                  // YYYY-MM-DD
	Width        int    `json:"width"`                   // ATM ± width
	Side         string `json:"side"`                    // calls | puts | both
	TradingClass string `json:"trading_class,omitempty"` // SPX | SPXW for multi-class index chains; empty = auto
}

// ScanRunParams runs a scanner. Two modes:
//
//  1. Preset shorthand: set Preset to the name of a [scans.<name>] block
//     from config.toml (or one of the built-in defaults). Type/Exchange
//     are ignored.
//  2. Ad-hoc: leave Preset empty and set Type (scanCode), Exchange
//     (locationCode), and optionally Instrument directly. Useful for agents that don't want to
//     persist a preset to the user's config file.
//
// Exactly one of Preset or Type is required. Limit is optional in both
// modes; <=0 falls back to the preset's configured Limit (mode 1) or
// the daemon's hard cap of 50 (mode 2).
type ScanRunParams struct {
	Preset          string  `json:"preset,omitempty"`
	Type            string  `json:"type,omitempty"`
	Exchange        string  `json:"exchange,omitempty"`
	Instrument      string  `json:"instrument,omitempty"`
	Limit           int     `json:"limit,omitempty"`
	MinPrice        float64 `json:"min_price,omitempty"`
	MinVolume       int64   `json:"min_volume,omitempty"`
	MinDollarVolume float64 `json:"min_dollar_volume,omitempty"`
	RequireLive     bool    `json:"require_live,omitempty"`
	ExcludePenny    bool    `json:"exclude_penny,omitempty"`
}

// ScanParamsParams requests the gateway's full scanner catalog. Instrument
// filters the ScanTypes list to those valid for the given instrument
// (e.g. "STK"); empty returns every type. IncludeRawXML attaches the raw
// XML payload to the response for callers that want to grep for fields
// not surfaced in the parsed struct (filter values, instrument flags,
// etc.). The XML is typically ~200 KB on a US Pro gateway.
type ScanParamsParams struct {
	Instrument    string `json:"instrument,omitempty"`
	IncludeRawXML bool   `json:"include_raw_xml,omitempty"`
}

// ScanParamsResult mirrors pkg/ibkr.ScannerParameters but stays in the
// rpc package so consumers (CLI, MCP) don't need to import pkg/ibkr.
// Code comments on the wire-level types live with the parser.
type ScanParamsResult struct {
	Instruments []ScanParamInstrument `json:"instruments"`
	Locations   []ScanParamLocation   `json:"locations"`
	ScanTypes   []ScanParamScanType   `json:"scan_types"`
	RawXML      string                `json:"raw_xml,omitempty"`
	AsOf        time.Time             `json:"as_of"`
}

// ScanParamInstrument is one row in ScanParamsResult.Instruments.
type ScanParamInstrument struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ScanParamLocation is one row in ScanParamsResult.Locations.
type ScanParamLocation struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
}

// ScanParamScanType is one row in ScanParamsResult.ScanTypes. Instruments
// is the list of instrument-type tokens this scan is valid for (e.g.
// ["STK", "ETF"]); empty means "all".
type ScanParamScanType struct {
	Code        string   `json:"code"`
	DisplayName string   `json:"display_name"`
	Instruments []string `json:"instruments,omitempty"`
}

// CancelParams cancels an in-flight stream by id.
type CancelParams struct {
	ID string `json:"id"`
}

// HistoryDailyParams requests N days of daily OHLCV bars for a symbol.
type HistoryDailyParams struct {
	Symbol     string `json:"symbol"`
	Days       int    `json:"days,omitempty"` // default 90 when zero
	WhatToShow string `json:"what_to_show,omitempty"`
}

// HistoryBar is one daily OHLCV row.
type HistoryBar struct {
	Date   string  `json:"date"` // YYYY-MM-DD
	Open   float64 `json:"open"`
	High   float64 `json:"high"`
	Low    float64 `json:"low"`
	Close  float64 `json:"close"`
	Volume int64   `json:"volume"`
}

// HistoryDailyResult wraps the daily bars for the CLI. Historical daily
// bars are gateway-stored data with no live/delayed dimension; DataType
// is therefore unused on this response and kept only as a reserved field
// (omitempty) for shape parity with the streaming surfaces.
type HistoryDailyResult struct {
	Symbol     string       `json:"symbol"`
	Days       int          `json:"days"`
	WhatToShow string       `json:"what_to_show,omitempty"`
	DataType   string       `json:"data_type,omitempty"`
	Bars       []HistoryBar `json:"bars"`
	AsOf       time.Time    `json:"as_of"`
}

// TechnicalParams asks the daemon to compute weekly-screening indicators from
// daily bars. Symbols may be passed as a comma-separated string by the CLI or
// as an array by MCP after normalisation.
type TechnicalParams struct {
	Symbols      []string `json:"symbols"`
	Benchmark    string   `json:"benchmark,omitempty"`     // default SPY
	LookbackDays int      `json:"lookback_days,omitempty"` // calendar days, default 420
	Market       string   `json:"market,omitempty"`        // us | de; applies to Symbols, not Benchmark
	Exchange     string   `json:"exchange,omitempty"`
	PrimaryExch  string   `json:"primary_exchange,omitempty"`
	Currency     string   `json:"currency,omitempty"`
	LocalSymbol  string   `json:"local_symbol,omitempty"`
	TradingClass string   `json:"trading_class,omitempty"`
}

// TechnicalRow is one symbol's trend, relative-strength, volatility, and
// liquidity summary. Percentage-like fields are decimal fractions:
// 0.10 means +10%. RS is symbol return minus benchmark return over the same
// trading-bar window.
type TechnicalRow struct {
	Symbol              string    `json:"symbol"`
	Price               *float64  `json:"price,omitempty"`
	PriceAsOf           string    `json:"price_as_of,omitempty"`
	Bars                int       `json:"bars"`
	SMA50               *float64  `json:"sma_50,omitempty"`
	SMA200              *float64  `json:"sma_200,omitempty"`
	PctAbove50DMA       *float64  `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA      *float64  `json:"pct_above_200dma,omitempty"`
	Return21D           *float64  `json:"return_21d,omitempty"`
	Return63D           *float64  `json:"return_63d,omitempty"`
	Return126D          *float64  `json:"return_126d,omitempty"`
	BenchmarkReturn63D  *float64  `json:"benchmark_return_63d,omitempty"`
	BenchmarkReturn126D *float64  `json:"benchmark_return_126d,omitempty"`
	RS63D               *float64  `json:"rs_63d,omitempty"`
	RS126D              *float64  `json:"rs_126d,omitempty"`
	ATR14               *float64  `json:"atr_14,omitempty"`
	ATRPct              *float64  `json:"atr_pct,omitempty"`
	AvgVolume20D        *int64    `json:"avg_volume_20d,omitempty"`
	AvgDollarVolume20D  *float64  `json:"avg_dollar_volume_20d,omitempty"`
	LiquiditySampleDays int       `json:"liquidity_sample_days,omitempty"`
	TrendState          string    `json:"trend_state,omitempty"`
	DataQuality         string    `json:"data_quality,omitempty"` // ok | partial | insufficient_data | error
	MissingReasons      []string  `json:"missing_reasons,omitempty"`
	Error               string    `json:"error,omitempty"`
	AsOf                time.Time `json:"as_of,omitzero"`
}

// TechnicalResult is MethodTechnical's payload.
type TechnicalResult struct {
	Benchmark      string         `json:"benchmark"`
	LookbackDays   int            `json:"lookback_days"`
	Market         string         `json:"market,omitempty"`
	Exchange       string         `json:"exchange,omitempty"`
	PrimaryExch    string         `json:"primary_exchange,omitempty"`
	Currency       string         `json:"currency,omitempty"`
	Rows           []TechnicalRow `json:"rows"`
	WarningDetails []DataWarning  `json:"warning_details,omitempty"`
	AsOf           time.Time      `json:"as_of"`
}

// MarketCalendarParams requests official exchange-session context. Market is
// one of "us", "us-equity", "us-options", "de", or "de-xetra" aliases; the
// daemon normalizes it to the stable result.market token. Date is YYYY-MM-DD
// in the market's local timezone. At, when non-zero, wins over Date and asks
// for the market state at that exact instant. Days controls how many calendar
// days to include in Sessions; default 14, capped daemon-side at 400.
type MarketCalendarParams struct {
	Market string    `json:"market,omitempty"`
	Date   string    `json:"date,omitempty"`
	At     time.Time `json:"at,omitzero"`
	Days   int       `json:"days,omitempty"`
}

// MarketSession is one official market-calendar row. Open/Close are present
// for trading days; NextOpen/NextClose are present when the requested instant
// is closed but a next session is known within coverage.
type MarketSession struct {
	Market        string     `json:"market"`
	Label         string     `json:"label,omitempty"`
	Date          string     `json:"date"`
	Timezone      string     `json:"timezone"`
	State         string     `json:"state"`
	IsOpen        bool       `json:"is_open"`
	Reason        string     `json:"reason,omitempty"`
	Open          time.Time  `json:"open,omitzero"`
	Close         time.Time  `json:"close,omitzero"`
	NextOpen      *time.Time `json:"next_open,omitempty"`
	NextClose     *time.Time `json:"next_close,omitempty"`
	Source        string     `json:"source,omitempty"`
	SourceURL     string     `json:"source_url,omitempty"`
	CoverageStart string     `json:"coverage_start,omitempty"`
	CoverageEnd   string     `json:"coverage_end,omitempty"`
	Notes         string     `json:"notes,omitempty"`
}

// MarketCalendarResult is MethodMarketCalendar's payload.
type MarketCalendarResult struct {
	Market        string          `json:"market"`
	Label         string          `json:"label"`
	Timezone      string          `json:"timezone"`
	AsOf          time.Time       `json:"as_of"`
	CoverageStart string          `json:"coverage_start"`
	CoverageEnd   string          `json:"coverage_end"`
	Source        string          `json:"source"`
	SourceURL     string          `json:"source_url"`
	Session       MarketSession   `json:"session"`
	Sessions      []MarketSession `json:"sessions,omitempty"`
}

// BreadthSPXParams is the input for MethodBreadthSPX. All fields are
// optional with sensible defaults — the dashboard generator calls this
// with empty params for the canonical view.
type BreadthSPXParams struct {
	// HistoryDays bounds the trailing daily series. Default 30 when
	// zero or negative; capped at 90 to keep the wire payload bounded.
	HistoryDays int `json:"history_days,omitempty"`
	// TimeoutMs bounds the wait when the engine has a fresh value but
	// the wire envelope is still being assembled. Default 5000 ms when
	// zero. Does not affect the multi-minute cold-start fan-out; that
	// path returns immediately with State="computing".
	TimeoutMs int `json:"timeout_ms,omitempty"`
}

// BreadthDailyValue is one trailing daily breadth reading. The two
// SMA readings (50-day and 200-day) plus the constituent counts for
// new 52-week highs/lows are carried per session so a renderer can
// chart all four series in one history call. Units: percentages in
// [0, 100] for the SMA readings; raw counts for the highs/lows.
type BreadthDailyValue struct {
	Date           string  `json:"date"` // YYYY-MM-DD
	PctAbove50DMA  float64 `json:"pct_above_50dma"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighs       int     `json:"new_highs,omitempty"`
	NewLows        int     `json:"new_lows,omitempty"`
}

// BreadthState classifies the engine's compute-pipeline state at the
// moment a result envelope was assembled. Distinct from a generic
// "status" because the consumer's branching logic depends on which
// state the engine is in, not just whether the value is present:
//
//   - cold: no snapshot has ever been computed AND no refresh is in
//     flight. The engine exists but hasn't been kicked yet. Treat as
//     "indicator not yet available" — typically only seen during the
//     ~few-second window between daemon start and postConnectSetup
//     launching the scheduler.
//   - computing: no snapshot exists yet and a refresh is in flight. Renderers
//     show a loading state.
//   - ready: a snapshot exists. Value/History are authoritative enough to
//     rank; Refreshing says whether a newer snapshot is being computed.
//   - degraded: a snapshot exists but its coverage is below the
//     engine's threshold (e.g. partial fan-out completed). Value is
//     present but should be rendered with a warning — the underlying
//     constituent coverage is insufficient.
//
// Codified on the wire (rather than left as a side-channel via
// engine.IsRefreshing) so every consumer reads the same state without
// remembering to call a sibling method or infer readiness from zero values.
type BreadthState string

// Breadth states distinguish startup and computation progress from usable and
// coverage-degraded snapshots.
const (
	BreadthStateCold      BreadthState = "cold"
	BreadthStateComputing BreadthState = "computing"
	BreadthStateReady     BreadthState = "ready"
	BreadthStateDegraded  BreadthState = "degraded"
)

// BreadthRefreshFailure is a redacted allowlisted reason for the latest
// breadth refresh problem. Raw per-symbol broker errors remain daemon-local.
type BreadthRefreshFailure string

// Breadth refresh failure values are the allowlisted wire vocabulary.
const (
	BreadthRefreshFailureFetch     BreadthRefreshFailure = "fetch_failed"
	BreadthRefreshFailurePersist   BreadthRefreshFailure = "persist_failed"
	BreadthRefreshFailureCancelled BreadthRefreshFailure = "cancelled"
)

// BreadthRefreshProgress makes the long IBKR-paced fan-out observable without
// exposing symbols or broker text. Processed includes successful and failed
// fetches; Deadline is the calendar-based publication SLA, not an ETA.
type BreadthRefreshProgress struct {
	SessionKey  string                `json:"session_key"`
	StartedAt   time.Time             `json:"started_at"`
	Processed   int                   `json:"processed"`
	Total       int                   `json:"total"`
	Deadline    time.Time             `json:"deadline,omitzero"`
	LastFailure BreadthRefreshFailure `json:"last_failure,omitempty"`
}

// BreadthSPXResult is the payload for MethodBreadthSPX. The two
// SMA percentages plus the new-highs/lows count are surfaced as
// separate fields so consumers can read each independently;
// History carries the trailing daily series for sparkline
// rendering. Threshold derivation (green / yellow / red) is left to
// the renderer — the spec itself says thresholds should be tunable,
// so the daemon stays out of that policy choice.
//
// The Source / Method strings name the data provenance and
// computation path so renderers can disclose how the number was
// derived. Method is a short token; longer methodology disclosure
// lives in the spec doc.
type BreadthSPXResult struct {
	// State classifies the engine pipeline at the moment this envelope
	// was assembled (cold / computing / ready / degraded). Consumers
	// should branch on this, not on (value==0 && history==[]) heuristics.
	// See BreadthState docs for semantics.
	State BreadthState `json:"state"`
	// Refreshing is true when a newer breadth run is in flight or waiting to
	// retry while this envelope serves the last good snapshot.
	Refreshing bool `json:"refreshing,omitempty"`
	// Refresh is the current or most recently completed paced-pass progress.
	// It remains available after completion so operators can distinguish a
	// slow advancing run, a stopped run, and the last classified failure.
	Refresh *BreadthRefreshProgress `json:"refresh,omitempty"`
	// PctAbove50DMA is the current fast-window reading: percentage of
	// S&P 500 constituents trading above their own 50-day SMA. 0-100;
	// 50 is the symmetric midpoint. Spec rule of thumb: > 55 healthy,
	// 40-55 watch, < 40 with SPX at highs is the classic late-cycle
	// divergence. Zero is meaningful only when State == "ready" (a
	// State other than "ready" can carry the field at 0 as the "no
	// data yet" sentinel).
	PctAbove50DMA float64 `json:"pct_above_50dma"`
	// PctAbove200DMA is the slow-window reading: percentage above the
	// 200-day SMA. Caught the 1999 and 2021 cyclical tops cleanly.
	// Bands per locked plan: below 40% = red / 40-60% = yellow / above
	// 60% = green (calibrated to the post-Mag-7 era).
	PctAbove200DMA float64 `json:"pct_above_200dma"`
	// NewHighsToday is the count of constituents whose latest close
	// strictly exceeded their trailing 252-bar max (~1 year of
	// trading sessions ≈ "52-week high"). Names with < 252 sessions
	// of cached history are skipped.
	NewHighsToday int `json:"new_highs_today"`
	// NewLowsToday is the symmetric count for new 252-bar lows.
	NewLowsToday int `json:"new_lows_today"`
	// NetNewHighsPct is (NewHighsToday - NewLowsToday) / coverage × 100
	// where coverage is the count of names with enough history to
	// contribute. The classic "narrow rally" pattern is SPX near
	// highs with NetNewHighsPct near zero or negative — a small
	// number of mega-caps carrying the index while the median name
	// is rolling over.
	NetNewHighsPct float64 `json:"net_new_highs_pct"`
	// History is the trailing daily series, oldest first. Length is
	// bounded by BreadthSPXParams.HistoryDays. Each point carries
	// both SMA readings plus the new-highs/lows counts.
	History []BreadthDailyValue `json:"history,omitempty"`
	// Source identifies the data provenance for the headline value.
	// Free-form; renderers display verbatim.
	Source string `json:"source"`
	// Method is a short token naming the computation path so renderers
	// can disclose methodology. Current token:
	// "constituent-fanout-50/200dma+nh-v2"
	// (50-DMA + 200-DMA + new highs/lows over 252-bar rolling
	// max/min — all computed locally from constituent daily closes
	// pulled via IBKR's historical-bar feed, since IBKR doesn't
	// redistribute the underlying S&P DJI / NYSE breadth indices on
	// retail subscriptions).
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the result was assembled.
	AsOf time.Time `json:"as_of"`
	// SessionKey is the US-equity session date represented by the
	// computed daily bars. It may differ from AsOf on weekends,
	// holidays, and before the current session's close is settled.
	SessionKey string `json:"session_key,omitempty"`
	// SpotAt is the gateway-observation timestamp for the headline,
	// distinct from AsOf which covers history + headline.
	SpotAt time.Time `json:"spot_at,omitzero"`
	// DataType reflects the gateway's feed state when the headline
	// was captured — "live", "delayed", "frozen", "delayed-frozen",
	// or "" when no notice has arrived yet. Renderers use this to
	// dim the headline.
	DataType string `json:"data_type,omitempty"`
}

// GammaZeroSPXStatus values are returned on GammaZeroSPXResult.Status and
// drive the dashboard generator's "render the number" vs "render a
// loading state" choice. The compute is heavy (several minutes against
// hundreds of option legs) and runs on a daemon-internal goroutine, so the
// wire shape always carries a state. The daemon normally prewarms after
// gateway startup. Successful last-good data is served while a newer compute
// refreshes behind it after the 15-minute RTH soft TTL; outside regular option
// hours automatic refresh is not due.
//
// The four states mirror BreadthState's cold/computing/ready/error
// semantics so consumers can branch on Status uniformly across the two
// state-machine engines.
const (
	// GammaZeroStatusCold — no usable last-good exists and no compute is in
	// flight. This can persist off-hours because automatic refresh is not due.
	// Distinct from Computing, which will resolve without another kick.
	GammaZeroStatusCold = "cold"
	// GammaZeroStatusComputing — a background compute is in flight and no
	// usable last-good can be served; the EtaSeconds / Progress fields carry
	// refresh hints. Callers who can wait may set GammaZeroSPXParams.WaitMs >
	// 0 on the request to block up to that budget for the result.
	GammaZeroStatusComputing = "computing"
	// GammaZeroStatusReady — Result is the served successful last-good. During
	// RTH a newer calculation may refresh behind it; off-hours it can be
	// retained as typed closed-session context.
	GammaZeroStatusReady = "ready"
	// GammaZeroStatusError — the last compute failed; Error carries the
	// classified reason. Callers retry by re-invoking the method.
	GammaZeroStatusError = "error"
)

// Scope values for GammaZeroSPXParams.Scope. Empty Scope defaults to
// "spy+spx". The combined scope prefers fresh SPY+SPX; when a fresh SPX
// slice is unavailable it may compose fresh/cached SPY with the last
// successful SPX slice and mark the result degraded. If no usable SPX
// slice exists, combined degrades to SPY-only with a structured warning.
const (
	GammaZeroScopeSPY      = "spy"
	GammaZeroScopeSPX      = "spx"
	GammaZeroScopeCombined = "spy+spx"
)

// GammaZeroSPXParams is the input for MethodGammaZeroSPX. All fields are
// optional; defaults match the v1 calibration window documented in
// docs/docs/internals/regime-dashboard.md.
type GammaZeroSPXParams struct {
	// WaitMs is the maximum time the daemon blocks on an in-flight or
	// just-kicked-off compute before returning the current state. 0
	// (the default) means "return immediately with whatever state we
	// have." A non-zero value is capped daemon-side to keep the RPC
	// under the per-method deadline.
	WaitMs int `json:"wait_ms,omitempty"`
	// Force, when true, starts a fresh diagnostic compute. If a good
	// cached result is already serving, the daemon keeps serving it and
	// promotes the forced compute only on success. Useful for diagnostics;
	// dashboards should leave this off and let the daily cache handle
	// freshness.
	Force bool `json:"force,omitempty"`
	// Scope selects which underlying(s) to compute. One of GammaZeroScopeSPY
	// ("spy"), GammaZeroScopeSPX ("spx"), or GammaZeroScopeCombined ("spy+spx").
	// Empty defaults to "spy+spx" with SPX-skipped fallback.
	Scope string `json:"scope,omitempty"`
	// IncludeProfiles asks clients/renderers to retain the full profile
	// arrays in JSON/MCP responses. The daemon compute always produces
	// profiles where meaningful; thin adapters may strip them by default
	// to keep agent/tool payloads compact.
	IncludeProfiles bool `json:"include_profiles,omitempty"`
}

// GammaZeroParams echoes the v1 calibration window back to the caller so
// renderers can show "computed over N expirations within ±X%." Future
// versions can add fields here without breaking the result shape — every
// renderer-relevant tuning parameter lives on this echo.
type GammaZeroParams struct {
	// ExpiryCount is the number of nearest non-0DTE-post-settlement
	// expirations included in the aggregation.
	ExpiryCount int `json:"expiry_count"`
	// StrikeWidthPct is the half-width of the strike grid around spot,
	// expressed as a fraction (0.10 = ATM ± 10 %).
	StrikeWidthPct float64 `json:"strike_width_pct"`
	// SweepRangePct is the half-range of the spot sweep used to find
	// the zero-crossing (0.15 = [0.85, 1.15] × spot).
	SweepRangePct float64 `json:"sweep_range_pct"`
	// WorkerCount is the per-leg fan-out concurrency. 4 matches the
	// documented safe gateway throttle; bumping it requires retuning
	// AcquireMarketDataSlot.
	WorkerCount int `json:"worker_count"`
}

// GammaProfilePoint is one (spot, dealer_gex) sample from the sweep.
// GEX is signed under the Perfiliev convention (call gamma positive,
// put gamma negative); a sign flip across two adjacent points is what
// the renderer reads as a "zero crossing."
type GammaProfilePoint struct {
	Spot float64 `json:"spot"`
	GEX  float64 `json:"gex"`
}

// StrikeConcentration is one row of the "where dealer hedging
// concentrates" table — the top strikes ranked by sign-agnostic gamma
// notional |Γ| × OI × multiplier × spot² × 0.01. This is the more
// robust signal in regimes where the Perfiliev dealer-sign assumption
// can invert (covered-call ETF flow, autocall barrier proximity); the
// renderer can present it alongside ZeroGamma as a "call wall / put
// wall" view that's sign-convention agnostic.
type StrikeConcentration struct {
	// Underlying identifies which index this strike belongs to —
	// "SPY" or "SPX" today. Populated by single-underlying computes
	// with that compute's sym, and carried through the combined-scope
	// merge so the renderer can label per-row in the top-strikes
	// table without re-deriving from the trading class. Empty means
	// the only underlying in scope.
	Underlying string `json:"underlying,omitempty"`
	// TradingClass is the listed class on the contract — "SPY",
	// "SPX" (AM-settled monthly), "SPXW" (PM-settled weekly).
	// Distinct from Underlying for SPX which lists both classes.
	// Empty in single-class results that don't need disambiguation.
	TradingClass string  `json:"trading_class,omitempty"`
	Strike       float64 `json:"strike"`
	Expiry       string  `json:"expiry"` // YYYY-MM-DD
	Right        string  `json:"right"`  // "C" | "P"
	AbsGEX       float64 `json:"abs_gex"`
	OI           int64   `json:"open_interest"`
}

// GammaLegDiagnosticCounts splits the priced-leg funnel into the
// conditions required for dealer GEX contribution. A leg can price and
// still fail to contribute when open interest is missing/zero, gamma is
// degenerate, or the resulting OI-weighted absolute GEX is zero. Missing
// OI is unknown, not zero; OpenInterestObservedLegs keeps that distinct
// from an observed zero-OI tick.
type GammaLegDiagnosticCounts struct {
	PricedLegs               int `json:"priced_legs"`
	ModelTickLegs            int `json:"model_tick_legs,omitempty"`
	DerivedLiveMidLegs       int `json:"derived_live_mid_legs,omitempty"`
	DerivedPrevCloseLegs     int `json:"derived_prev_close_legs,omitempty"`
	OpenInterestObservedLegs int `json:"oi_observed_legs,omitempty"`
	OILiveObservedLegs       int `json:"oi_live_observed_legs,omitempty"`
	OICarriedForwardLegs     int `json:"oi_carried_forward_legs,omitempty"`
	OpenInterestLegs         int `json:"oi_positive_legs"`
	GammaPositiveLegs        int `json:"gamma_positive_legs"`
	AbsGEXLegs               int `json:"abs_gex_positive_legs"`
}

// GammaLegDiagnostics carries the leg-quality funnel for the whole
// result plus splits that identify whether the drop happened at the
// underlying or trading-class level (for example SPX vs SPXW).
type GammaLegDiagnostics struct {
	Total          GammaLegDiagnosticCounts            `json:"total"`
	ByUnderlying   map[string]GammaLegDiagnosticCounts `json:"by_underlying,omitempty"`
	ByTradingClass map[string]GammaLegDiagnosticCounts `json:"by_trading_class,omitempty"`
}

// GammaCollectionDiagnostic exposes the source-level option-chain funnel for
// one (underlying, trading class, expiry) slice. It separates contract
// discovery/prewarm from option market-data and OI capture so a blocked gamma
// result can name the source failure rather than only reporting a rankability
// gate.
type GammaCollectionDiagnostic struct {
	Underlying               string `json:"underlying"`
	TradingClass             string `json:"trading_class,omitempty"`
	Expiry                   string `json:"expiry,omitempty"` // YYYY-MM-DD
	QualifiedContracts       int    `json:"qualified_contracts"`
	RequestedLegs            int    `json:"requested_legs"`
	PricedLegs               int    `json:"priced_legs"`
	ModelTickLegs            int    `json:"model_tick_legs,omitempty"`
	DerivedLiveMidLegs       int    `json:"derived_live_mid_legs,omitempty"`
	DerivedPrevCloseLegs     int    `json:"derived_prev_close_legs,omitempty"`
	MarketDataGenericTicks   string `json:"market_data_generic_ticks,omitempty"`
	OIGenericTickRequested   bool   `json:"oi_generic_tick_101_requested,omitempty"`
	OILiveObservedLegs       int    `json:"oi_live_observed_legs,omitempty"`
	OICarriedForwardLegs     int    `json:"oi_carried_forward_legs,omitempty"`
	OIPositiveLegs           int    `json:"oi_positive_legs,omitempty"`
	OIMissingLegs            int    `json:"oi_missing_legs,omitempty"`
	ContractMissingLegs      int    `json:"contract_missing_legs,omitempty"`
	Timeouts                 int    `json:"timeouts,omitempty"`
	PacingErrors             int    `json:"pacing_errors,omitempty"`
	FarmErrors               int    `json:"farm_errors,omitempty"`
	EntitlementErrors        int    `json:"entitlement_errors,omitempty"`
	SubscriptionRejects      int    `json:"subscription_rejects,omitempty"`
	StrikeCandidates         int    `json:"strike_candidates,omitempty"`
	StrikeSelected           int    `json:"strike_selected,omitempty"`
	StrikeCap                int    `json:"strike_cap,omitempty"`
	StrikeCapTruncated       bool   `json:"strike_cap_truncated,omitempty"`
	ExpiryCapTruncated       bool   `json:"expiry_cap_truncated,omitempty"`
	CollectionDurationMS     int64  `json:"collection_duration_ms,omitempty"`
	OISourceStatus           string `json:"oi_source_status,omitempty"` // live_observed | carried_forward | mixed | missing
	CarriedForwardSource     string `json:"carried_forward_source,omitempty"`
	CarriedForwardObservedAt string `json:"carried_forward_observed_at,omitempty"`
}

// Gamma rankability and quality-gate values keep displayable context separate
// from evidence that may participate in downstream regime decisions.
const (
	GammaRankabilityRankable    = "rankable"
	GammaRankabilityContextOnly = "context_only"
	GammaRankabilityBlocked     = "blocked"
	GammaRankabilityUnavailable = "unavailable"

	GammaQualityGatePass    = "pass"
	GammaQualityGateContext = "context"
	GammaQualityGateBlock   = "block"

	// GammaQualityGateFreshness, GammaQualityGateSPXCoverage, and
	// GammaFreshnessSessionMismatch cross the package boundary: the regime
	// input-currency model asks whether a non-rankable gamma result is blocked
	// on its publication cadence alone.
	GammaQualityGateFreshness     = "freshness"
	GammaQualityGateSPXCoverage   = "spx_coverage"
	GammaFreshnessSessionMismatch = "session_mismatch"
)

// GammaSignalQuality is the trading-grade gate for the gamma payload.
// The signed regime and sign-agnostic magnitude can be displayed whenever
// Result is present, but downstream regime/Stress consumers must only count
// the gamma band as market evidence when Rankability == "rankable".
type GammaSignalQuality struct {
	Rankability       string                        `json:"rankability"`
	RankabilityReason string                        `json:"rankability_reason,omitempty"`
	Freshness         string                        `json:"freshness,omitempty"`
	Session           string                        `json:"session,omitempty"`
	SessionKey        string                        `json:"session_key,omitempty"`
	CurrentSessionKey string                        `json:"current_session_key,omitempty"`
	AsOf              time.Time                     `json:"as_of,omitzero"`
	AgeSeconds        int64                         `json:"age_seconds,omitempty"`
	MaxAgeSeconds     int64                         `json:"max_age_seconds,omitempty"`
	Coverage          GammaQualityCoverage          `json:"coverage"`
	Gates             []GammaQualityGate            `json:"gates,omitempty"`
	Blockers          []string                      `json:"blockers,omitempty"`
	Context           []string                      `json:"context,omitempty"`
	ByUnderlying      map[string]GammaSignalQuality `json:"by_underlying,omitempty"`
}

// GammaQualityCoverage carries the numeric diagnostics used by the quality
// gates. Ratios are percentages in human units (95.0 means 95%).
type GammaQualityCoverage struct {
	PricedLegs           int     `json:"priced_legs"`
	RequestedLegs        int     `json:"requested_legs,omitempty"`
	FanoutCompletePct    float64 `json:"fanout_complete_pct,omitempty"`
	ModelTickLegs        int     `json:"model_tick_legs,omitempty"`
	DerivedLiveMidLegs   int     `json:"derived_live_mid_legs,omitempty"`
	DerivedPrevCloseLegs int     `json:"derived_prev_close_legs,omitempty"`
	OIObservedLegs       int     `json:"oi_observed_legs"`
	OILiveObservedLegs   int     `json:"oi_live_observed_legs,omitempty"`
	OICarriedForwardLegs int     `json:"oi_carried_forward_legs,omitempty"`
	OIPositiveLegs       int     `json:"oi_positive_legs"`
	GEXLegs              int     `json:"gex_legs"`
	OIObservedPct        float64 `json:"oi_observed_pct,omitempty"`
	OILiveObservedPct    float64 `json:"oi_live_observed_pct,omitempty"`
	OICarriedForwardPct  float64 `json:"oi_carried_forward_pct,omitempty"`
	OIPositivePct        float64 `json:"oi_positive_pct,omitempty"`
	DerivedIVPct         float64 `json:"derived_iv_pct,omitempty"`
	TopConcentrationPct  float64 `json:"top_concentration_pct,omitempty"`
	ExpirationCount      int     `json:"expiration_count,omitempty"`
	Has0DTE              bool    `json:"has_0dte"`
	Has1To7DTE           bool    `json:"has_1to7_dte"`
	HasTerm              bool    `json:"has_term"`
	SkewFitExpiries      int     `json:"skew_fit_expiries,omitempty"`
	MedianSkewRSquared   float64 `json:"median_skew_r_squared,omitempty"`
	MinSkewRSquared      float64 `json:"min_skew_r_squared,omitempty"`
}

// GammaQualityGate is one explicit quality decision. Status is "pass",
// "context", or "block"; block gates prevent rankability, context gates
// preserve the payload as context only.
type GammaQualityGate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// GammaWarningDetail is the human/agent-facing warning surface. The
// daemon may use compact codes internally, but the wire carries this
// scoped explanation so renderers do not have to decode raw tokens.
type GammaWarningDetail struct {
	// Code is the stable warning token, without lossy prose parsing.
	// Examples: "throttled", "0dte_no_legs",
	// "spx_unavailable:354", "oi_missing".
	Code string `json:"code"`
	// Scope names the affected slice: "SPY", "SPX", "SPY+SPX", or a
	// narrower trading class / expiry when the condition is that local.
	Scope string `json:"scope,omitempty"`
	// Severity is one of "info", "data_quality", or "methodology".
	// Renderers can show data_quality prominently and tuck info under
	// an expanded view.
	Severity string `json:"severity,omitempty"`
	// Message is a short user-facing explanation of the condition.
	Message string `json:"message"`
	// Impact explains how to read the gamma result in light of the
	// warning. Empty when the message is self-contained.
	Impact string `json:"impact,omitempty"`
	// Action is an optional non-advisory operational next step, such as
	// retrying during RTH or suppressing a known SPX entitlement banner.
	Action string `json:"action,omitempty"`
}

// GammaIndexSummary is a compact interpretation of one per-underlying
// gamma compute. It gives agents and text renderers the answer they
// usually need without walking profile arrays or knowing the raw
// sign-convention details.
type GammaIndexSummary struct {
	Underlying      string   `json:"underlying,omitempty"`
	SpotUnderlying  float64  `json:"spot_underlying,omitempty"`
	ZeroGamma       *float64 `json:"zero_gamma,omitempty"`
	ZeroGammaStatus string   `json:"zero_gamma_status,omitempty"`
	Regime          string   `json:"regime,omitempty"`
	SweepLowAbs     float64  `json:"sweep_low_abs,omitempty"`
	SweepHighAbs    float64  `json:"sweep_high_abs,omitempty"`
	LegCount        int      `json:"leg_count,omitempty"`
	PricedLegCount  int      `json:"priced_leg_count,omitempty"`
	GammaTotalAbs   float64  `json:"gamma_total_abs,omitempty"`
	Confidence      string   `json:"confidence,omitempty"`
	Interpretation  string   `json:"interpretation,omitempty"`
}

// GammaZeroSummary is the compact, non-advisory readout of a gamma
// result. The raw fields remain canonical for charting and backtests;
// Summary is for humans and agents answering "which gamma zero, if
// any, did we identify?"
type GammaZeroSummary struct {
	PrimaryStatement string                       `json:"primary_statement,omitempty"`
	ZeroGammaStatus  string                       `json:"zero_gamma_status,omitempty"`
	Regime           string                       `json:"regime,omitempty"`
	Confidence       string                       `json:"confidence,omitempty"`
	NotAdvice        string                       `json:"not_advice,omitempty"`
	PerIndex         map[string]GammaIndexSummary `json:"per_index,omitempty"`
}

// SkewFitInfo is the per-expiry diagnostic for the sticky-moneyness
// skew curve fitted at snapshot time. Populated only when SkewModel
// reports a fitted model (e.g. "sticky-moneyness-v1"); a renderer can
// surface it as "skew fit: 12 pts · R² 0.94 · m ∈ [-0.12, +0.09]" so
// the reader can audit how well the curve actually fit the observed
// IVs before acting on the zero-gamma level it implies.
//
// Range is the moneyness window m = ln(K/S) the curve was fitted over;
// scenario spots that push a leg's moneyness outside the window are
// clamped to the boundary during the sweep.
type SkewFitInfo struct {
	Points   int     `json:"points"`
	RSquared float64 `json:"r_squared"`
	// ResidualRMS is the root-mean-square fit residual in IV (vol)
	// units over the fitted legs. R² is amplitude-relative — a flat
	// smile reads as a poor fit even when residuals are tiny, and a
	// steep one as a good fit despite large absolute errors — so the
	// RMS is the number that actually bounds the repricing error the
	// sticky-moneyness sweep consumes. Zero means no residual was recorded.
	ResidualRMS float64    `json:"residual_rms,omitempty"`
	Range       [2]float64 `json:"range"`
}

// GammaZeroComputed is the actual zero-gamma payload — populated when
// GammaZeroSPXResult.Status is GammaZeroStatusReady. Kept as a separate
// struct so the envelope (Status / Eta / Progress / Result pointer)
// can evolve independently of the computation payload.
//
// Sign convention: ZeroGamma assumes the 2018-era "dealers long calls,
// short puts" Perfiliev convention. This is a defensible baseline at
// the SPX index level but can invert near autocallable barriers and
// when covered-call ETF flow dominates. Treat ZeroGamma as a regime
// hint, not a precise level; consult TopStrikes (sign-agnostic) for
// the more robust positioning view. See docs/docs/internals/regime-dashboard.md
// for the full methodology disclosure.
//
// Combined-scope semantics:
//
// When Scope == "spy+spx", there is intentionally no top-level
// ZeroGamma, GapPct, GammaSign, SpotUnderlying, or horizon bucket.
// SPY and SPX live on different price scales, so consumers must read
// per_index.SPY and per_index.SPX for price-level regime detail. The
// combined top level is limited to scale-safe diagnostics: summary,
// regime agreement, sign-agnostic magnitude, top strikes, counts,
// warnings, method/source, citations, timestamps, and the per-index map.
type GammaZeroComputed struct {
	// SpotUnderlying is the price of the underlying instrument
	// (currently SPY — see Source) at which the aggregation was
	// anchored. Field was renamed from SpotSPX when the compute moved
	// from SPX to the more liquid SPY chain (SPY has continuous
	// extended-hours quoting and a single trading class, which keeps
	// the compute robust off-hours).
	SpotUnderlying float64 `json:"spot_underlying,omitempty"`
	// SpotAt is the gateway-observation timestamp for SpotUnderlying.
	// Distinct from AsOf which covers the whole computation.
	SpotAt time.Time `json:"spot_at,omitzero"`

	// ZeroGamma is the dealer γ-zero level under the Perfiliev convention
	// (the spot where dealer net gamma crosses zero). nil when no
	// crossing exists within the sweep window — inspect GammaSign in
	// that case to learn whether the whole sweep is long-γ or short-γ.
	ZeroGamma *float64 `json:"zero_gamma,omitempty"`
	// GapPct is (SpotUnderlying − ZeroGamma) / ZeroGamma × 100. nil iff
	// ZeroGamma is nil. Sign convention: positive = spot above γ-zero
	// (dampening regime); negative = below γ-zero (amplifying regime).
	GapPct *float64 `json:"gap_pct,omitempty"`
	// GammaSign is "positive" or "negative" and is meaningful only when
	// ZeroGamma is nil — it tells the renderer which side of zero the
	// whole sweep landed on so the UI can say "all long-gamma" or "all
	// short-gamma in window."
	GammaSign string `json:"gamma_sign,omitempty"`
	// Profile is the full (spot, gex) sweep, oldest first. 60 points
	// over [0.85, 1.15] × SpotUnderlying. Renderers chart this as the
	// gamma-exposure curve and visually confirm the zero crossing.
	Profile []GammaProfilePoint `json:"profile,omitempty"`

	// GammaTotalAbs is the sign-agnostic magnitude signal at
	// SpotUnderlying: Σ |Γ| × OI × 100 × SpotUnderlying² × 0.01. In
	// dollar gamma terms — the total notional dealer hedging flow for
	// a 1% underlying move, independent of any positioning assumption.
	// Larger = market is more sensitive to dealer rebalancing.
	GammaTotalAbs float64 `json:"gamma_total_abs"`
	// GammaTotalAbsConvention names the sign-handling for GammaTotalAbs
	// so downstream renderers can label it without re-deriving
	// methodology. Today's value is "sign-agnostic" — every leg's
	// magnitude |Γ|·OI·100·spot²·0.01 is summed unconditionally. This
	// is the convention-free read the M2 methodology refresh promotes
	// to co-primary alongside the signed γ-zero level.
	GammaTotalAbsConvention string `json:"gamma_total_abs_convention,omitempty"`
	// TopStrikes is the top-N strikes ranked by absolute gamma notional.
	// Concentration here is more reliable than the signed ZeroGamma in
	// regimes where the dealer-sign assumption may invert.
	TopStrikes []StrikeConcentration `json:"top_strikes"`
	// TopConcentrationPct is TopStrikes[0].AbsGEX / GammaTotalAbs × 100 —
	// what share of the sign-agnostic |Γ|·OI sum is parked at the single
	// largest strike. Renderers surface it as a one-line "this strike
	// dominates" cue alongside the table. Zero when TopStrikes is empty
	// or GammaTotalAbs is zero.
	TopConcentrationPct float64 `json:"top_concentration_pct,omitempty"`

	// SweepLowAbs / SweepHighAbs are the absolute spot bounds of the
	// sweep window in dollars: SpotUnderlying × (1 ± Params.SweepRangePct).
	// Surfaced for renderers that want to print "γ-zero outside swept
	// range $A.AA-$C.CC" without re-deriving the multiplication.
	SweepLowAbs  float64 `json:"sweep_low_abs,omitempty"`
	SweepHighAbs float64 `json:"sweep_high_abs,omitempty"`

	// Expirations is the YYYY-MM-DD list of expirations actually
	// included in the aggregation (after 0DTE-post-settlement filtering
	// and SPXW/SPX merging).
	Expirations []string `json:"expirations"`
	// LegCount is the number of option legs that contributed non-zero
	// open-interest-weighted gamma exposure to the profile. It excludes
	// priced legs whose IV landed but whose OI was missing/zero. Missing
	// OI is unknown, not zero, so those legs are omitted from dealer GEX
	// rather than zero-substituted.
	LegCount int `json:"leg_count"`
	// PricedLegCount is the number of option legs that delivered IV (or
	// a BS-IV fallback) and were usable for skew fitting. It can exceed
	// LegCount when IBKR supplied prices/IV but not open interest.
	PricedLegCount int `json:"priced_leg_count,omitempty"`
	// DerivedIVLegs counts how many priced legs used the BS-IV
	// Newton-Raphson fallback because the gateway never pushed a
	// model-computation tick. Pre-market this is often equal to
	// PricedLegCount (the model engine is idle); during regular hours it
	// should stay at 0. Renderers surface a "compute used N derived
	// IVs" disclosure so readers can tell those IVs came from option
	// quote/close inversion rather than live model ticks.
	DerivedIVLegs int `json:"derived_iv_legs,omitempty"`
	// ModelTickLegs counts priced legs whose IV came from IBKR's
	// option model-computation tick. DerivedLiveMidLegs and
	// DerivedPrevCloseLegs split the BS-IV fallback by price anchor:
	// live bid/ask midpoint versus prior-session option close. The
	// split is optional and additive; legacy consumers can continue to
	// read DerivedIVLegs as the total fallback count.
	ModelTickLegs        int `json:"model_tick_legs,omitempty"`
	DerivedLiveMidLegs   int `json:"derived_live_mid_legs,omitempty"`
	DerivedPrevCloseLegs int `json:"derived_prev_close_legs,omitempty"`
	// LegDiagnostics explains how priced legs flowed through the
	// GEX-contribution funnel. It is especially useful when a forced
	// off-hours run prices legs but every row has missing/zero OI.
	LegDiagnostics *GammaLegDiagnostics `json:"leg_diagnostics,omitempty"`
	// CollectionDiagnostics exposes the source-level request funnel per
	// underlying/tradingClass/expiry: contracts qualified, market-data legs
	// requested, priced legs, live-vs-carried OI, timeouts, rejects, and cap
	// truncation. This is the production diagnostic surface for deciding
	// whether gamma is source-limited or merely gate-blocked.
	CollectionDiagnostics []GammaCollectionDiagnostic `json:"collection_diagnostics,omitempty"`
	// Quality is the explicit rankability contract for gamma as an
	// algo-trading signal. Result can be present while Quality says
	// "context_only" or "blocked"; regime/Stress consumers must not
	// count the gamma band unless this says "rankable".
	Quality *GammaSignalQuality `json:"quality,omitempty"`
	// Warnings is the daemon-internal list of non-fatal condition codes:
	// "no_crossing_in_window", "spxw_partial_oi", "throttled",
	// "all_iv_derived". Empty when the computation was clean. It is not
	// serialized; wire consumers read WarningDetails instead.
	// Runs whose leg coverage falls below the MinLegCoverageFraction
	// persist threshold are surfaced as Status="error" with no
	// Result, not as a warning — see gamma_zero_compute's coverage
	// gate (mirror of breadth's MinCoverageFraction=0.80 pattern).
	Warnings []string `json:"-"`
	// WarningDetails is the serialized warning surface: scoped,
	// user-facing explanations plus optional impact/action text.
	WarningDetails []GammaWarningDetail `json:"warning_details,omitempty"`
	// Summary is a compact interpretation of the result. It is designed
	// for CLI/MCP consumers that need to answer "what did the model
	// identify?" before drilling into profile arrays.
	Summary *GammaZeroSummary `json:"summary,omitempty"`

	// ZeroGamma0DTE / Profile0DTE / GammaSign0DTE / LegCount0DTE are the
	// same headline triple computed over legs with DTE == 0 only —
	// same-day expiries before their settlement cutoff. Captures the
	// short-fuse flow that Cboe's 2025 data shows is ~59% of SPX
	// volume, isolated from the longer-dated weeklies and monthlies.
	// Nil when no 0DTE legs fell in the bucket (e.g. mid-week after
	// Monday's daily settled, before the next daily lists) OR when
	// the sweep over those legs had no crossing inside the ±10% band.
	// GammaSign0DTE="no_data" plus a "0dte_no_legs" warning communicate
	// the empty-bucket case.
	ZeroGamma0DTE *float64            `json:"zero_gamma_0dte,omitempty"`
	Profile0DTE   []GammaProfilePoint `json:"profile_0dte,omitempty"`
	GammaSign0DTE string              `json:"gamma_sign_0dte,omitempty"`
	LegCount0DTE  int                 `json:"leg_count_0dte,omitempty"`

	// ZeroGamma1to7 / Profile1to7 / GammaSign1to7 / LegCount1to7 are the
	// matching triple for legs with 0 < DTE ≤ 7 days — overnight
	// through one calendar week. Captures end-of-week dynamics
	// (weeklies, EOW Friday flow) without commingling with the 0DTE
	// term that swamps the bucket on a third Friday.
	ZeroGamma1to7 *float64            `json:"zero_gamma_1to7,omitempty"`
	Profile1to7   []GammaProfilePoint `json:"profile_1to7,omitempty"`
	GammaSign1to7 string              `json:"gamma_sign_1to7,omitempty"`
	LegCount1to7  int                 `json:"leg_count_1to7,omitempty"`

	// ZeroGammaTerm / ProfileTerm / GammaSignTerm / LegCountTerm are the
	// matching triple for legs with DTE > 7 days — monthly OPEX and
	// quarterly horizons. Slower-moving than the two near buckets;
	// dominated by collar/structured-product positioning rather than
	// dealer-flow speed.
	ZeroGammaTerm *float64            `json:"zero_gamma_term,omitempty"`
	ProfileTerm   []GammaProfilePoint `json:"profile_term,omitempty"`
	GammaSignTerm string              `json:"gamma_sign_term,omitempty"`
	LegCountTerm  int                 `json:"leg_count_term,omitempty"`

	// MethodologyCitations is the short bibliography backing the
	// methodology disclosure. Each entry is a single line of the form
	// "Author (Year) — short claim". Surfaced on the result envelope so
	// renderers can show the citations alongside the headline numbers
	// without the user having to consult out-of-band documentation.
	MethodologyCitations []string `json:"methodology_citations,omitempty"`

	// SkewModel names the IV model used during the sweep. v2 cutover:
	// "sticky-moneyness-v1" means a quadratic skew curve in
	// log-moneyness was fitted per expiry and σ was looked up at each
	// (scenario spot, strike) pair. Empty when the curve fell back to
	// sticky-IV everywhere (degenerate fits across every expiry); a
	// per-expiry fallback shows up in warning_details.
	SkewModel string `json:"skew_model,omitempty"`
	// SkewFitQuality is one SkewFitInfo per expiry that fitted a curve
	// successfully. Keyed by compact YYYYMMDD. Renderers can show fit
	// quality alongside the headline so the reader can audit how
	// confident the underlying skew curve is.
	SkewFitQuality map[string]SkewFitInfo `json:"skew_fit_quality,omitempty"`

	// Params echoes the v1 calibration window so a renderer can show
	// "computed over 6 expirations inside ATM ± 10%" without consulting
	// out-of-band documentation. The daemon caps the live fan-out to
	// the nearest 80 listed strikes per expiry; warning_details includes
	// strike_budget_capped when the candidate window exceeded that cap.
	Params GammaZeroParams `json:"params"`
	// Source identifies the data provenance for the headline numbers.
	Source string `json:"source"`
	// AuthorityProvenance is empty for a current-code compute. A non-empty
	// value identifies an explicit persistence recovery path and is consumed
	// by the gamma quality gate; recovered evidence remains displayable but
	// cannot become a rankable trading signal merely because its timestamp is
	// inside the current session.
	AuthorityProvenance string `json:"authority_provenance,omitempty"`
	// Method is a short stable token for the computation path. v3:
	// "bs-gamma-profile-v3-stickymoneyness-0dte-split". The v3 bump
	// signals two semantic changes from v2:
	//   - horizon split is now 0DTE / 1-7 / >7 (was ≤7 / >7), because
	//     Cboe 2025 data shows 0DTE = ~59% of SPX volume and lumping
	//     it with weeklies muddies the signal.
	//   - the per-leg snapshot gamma is BS-recomputed from captured IV
	//     rather than read from the gateway's optional Greeks tick;
	//     fixes a v2 race where IV-but-no-Greeks legs contributed 0 to
	//     GammaTotalAbs.
	// "perfiliev" is dropped from the token because Perfiliev's
	// published method used sticky-IV; the sticky-moneyness refinement
	// is citable to Derman / Daglish-Hull-Suo (see MethodologyCitations).
	// Full disclosure lives in docs/docs/internals/regime-dashboard.md so
	// renderers can deep-link.
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the compute finished.
	AsOf time.Time `json:"as_of"`
	// DurationMS is honest about how long the compute took on the wall
	// clock; useful for tuning ExpiryCount / StrikeWidthPct.
	DurationMS int64 `json:"duration_ms"`

	// Scope is the discriminator for combined-vs-single-underlying
	// payloads:
	//   "spy"     — SPY-only; PerIndex is nil
	//   "spx"     — SPX-only (--only=spx); PerIndex is nil
	//   "spy+spx" — combined; price-level fields stay under PerIndex
	//               because there is no meaningful combined price scale.
	// Empty is treated as Scope="spy" by legacy renderers only.
	Scope string `json:"scope,omitempty"`

	// PerIndex carries the per-underlying detail when Scope="spy+spx".
	// Nil for single-underlying scopes. Keys are uppercased symbols
	// ("SPY", "SPX"). Each entry is a self-contained GammaZeroComputed
	// with its own Scope ("spy" or "spx"), so a renderer can recurse on
	// the per-index slice and reuse the single-underlying formatting.
	//
	// Pointer values rather than struct values so the field can be
	// nil-checked rather than length-tested in renderers, and so the
	// recursive type doesn't bloat the SPY-only path's payload.
	PerIndex map[string]*GammaZeroComputed `json:"per_index,omitempty"`

	// PartialClasses surfaces per-trading-class entitlement gaps when
	// one class of an underlying lands but the other 354s. Keyed by
	// the unreachable trading class (e.g. {"SPX": "354"} when SPX-class
	// AM-monthlies return "not subscribed" but SPXW-class weeklies
	// land). Empty when both classes land cleanly OR when neither
	// lands (the latter surfaces as Status="error" upstream).
	PartialClasses map[string]string `json:"partial_classes,omitempty"`

	// RegimeAgreement classifies whether the SPY and SPX dealer-gamma
	// regimes agree, populated only on Scope="spy+spx" runs. One of:
	//
	//   "agree:long-gamma"  — both indices' sweeps stay positive (dealer
	//                         long-γ across the ±15% window, stabilizing).
	//   "agree:short-gamma" — both stay negative (short-γ, amplifying).
	//   "agree:transition-gamma" — both are within ±2% of their γ-zero
	//                         crossings. The per-index ZeroGamma levels
	//                         carry the precise prices.
	//   "disagree"          — one index is long-γ, the other short-γ
	//                         (or transition while the other isn't).
	//                         The actionable signal: institutional SPX
	//                         book and retail/ETF SPY book are positioned
	//                         opposite, which the regime-call use case
	//                         cares about more than any combined number.
	//   ""                  — at least one bucket has no data; can't
	//                         classify. Renderers fall back to per-index.
	//
	// Replaces the earlier DecoupledCorr field, which gated on 20-day
	// price correlation. Price correlation stays > 0.99 essentially
	// always; that gate never fired and missed the actual case worth
	// flagging — gamma regimes that decouple while prices stay tightly
	// correlated.
	RegimeAgreement string `json:"regime_agreement,omitempty"`
}

// GammaZeroSPXResult is the envelope returned by MethodGammaZeroSPX.
// Always carries a Status; Result is populated when Status is "ready".
// The split (envelope vs computed payload) keeps the wire stable while
// the compute pipeline can evolve — adding fields to GammaZeroComputed
// doesn't churn the polling contract.
type GammaZeroSPXResult struct {
	// Status is one of GammaZeroStatusComputing / Ready / Error.
	Status string `json:"status"`
	// Refreshing is true when a newer compute is in flight while this
	// envelope serves the last good result. Ready + StartedAt cannot express
	// that: StartedAt describes the compute that produced Result, so without
	// this field a consumer cannot tell a current-session compute in flight
	// from one that never started. Mirrors BreadthSPXResult.Refreshing.
	Refreshing bool `json:"refreshing,omitempty"`
	// StartedAt is when the currently-relevant compute kicked off — for
	// "computing", that's the in-flight job; for "ready", it's the
	// compute that produced Result. Nil if no compute has ever started.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// EtaSeconds is an initial estimate of the total wall-clock the
	// compute will need from kickoff. Used by renderers to show a
	// progress meter or set a polling cadence. 0 when Status != computing.
	EtaSeconds int `json:"eta_seconds,omitempty"`
	// Progress is a 0-100 hint, best-effort. 0 when Status != computing.
	Progress int `json:"progress,omitempty"`
	// Result is populated when Status == "ready".
	Result *GammaZeroComputed `json:"result,omitempty"`
	// DiagnosticResult is populated when a compute failed after collecting
	// source-level evidence (for example priced option legs with no usable OI),
	// or when a preserved ready cache has a newer failed diagnostic refresh.
	// It must not be treated as a trading signal; it exists to expose the
	// option-chain/OI source blocker that prevented Result from updating.
	DiagnosticResult *GammaZeroComputed `json:"diagnostic_result,omitempty"`
	// Error is populated when Status == "error".
	Error string `json:"error,omitempty"`
	// ColdReasonCode / ColdReason / ColdAction are populated when
	// Status == "cold" and the daemon knows why no result can be
	// served. This distinguishes a true first-run cold cache from a
	// persisted snapshot that existed but was rejected by schema,
	// methodology, or data-quality gates.
	ColdReasonCode string `json:"cold_reason_code,omitempty"`
	ColdReason     string `json:"cold_reason,omitempty"`
	ColdAction     string `json:"cold_action,omitempty"`
	// RetryOfErrorAt + RetryOfErrorSummary are non-nil/non-empty only
	// when Status == "computing" AND the in-flight compute was kicked
	// because the previous attempt failed past gammaErrorRetryTTL. The
	// renderer surfaces them as "computing · retry of <summary> at
	// HH:MM:SS" so the user sees the prior failure context — without
	// this, the dashboard silently switched from "error" to "computing"
	// and the user had to grep the daemon log to understand why.
	RetryOfErrorAt      *time.Time `json:"retry_of_error_at,omitempty"`
	RetryOfErrorSummary string     `json:"retry_of_error_summary,omitempty"`
}

// StripGammaProfiles removes chart-sized sweep arrays from a gamma result
// while preserving headline levels, summaries, warning details, counts, and
// top strikes. CLI JSON and MCP use this by default so agents do not receive
// tens of kilobytes of points unless they explicitly ask for profiles.
func StripGammaProfiles(r *GammaZeroSPXResult) {
	if r == nil {
		return
	}
	stripGammaComputedProfiles(r.Result)
	stripGammaComputedProfiles(r.DiagnosticResult)
}

// StripRegimeGammaProfiles removes large gamma profiles from every regime
// projection while preserving its summary and quality evidence.
func StripRegimeGammaProfiles(r *RegimeSnapshotResult) {
	if r == nil {
		return
	}
	StripGammaProfiles(&r.GammaZero.Envelope)
}

// CompactRegimeSnapshot removes methodology prose and chart/history payloads
// from a regime response while preserving the current measurements, summary,
// composite counts, streaks, quality provenance, scoped warnings, and gamma
// headline diagnostics. CLI --json and MCP use this default shape so agent
// consumers get the decision surface without multi-kilobyte notes blocks.
func CompactRegimeSnapshot(r *RegimeSnapshotResult) {
	if r == nil {
		return
	}
	StripRegimeGammaProfiles(r)
	r.VIXTermStructure.Notes = ""
	r.VolOfVol.Notes = ""
	r.HYGSPYDivergence.Notes = ""
	r.CreditSpreads.Notes = ""
	r.FundingStress.Notes = ""
	r.USDJPY.Notes = ""
	r.GammaZero.Notes = ""
	r.Breadth.Notes = ""
	r.Breadth.Envelope.History = nil
}

func stripGammaComputedProfiles(c *GammaZeroComputed) {
	if c == nil {
		return
	}
	c.Profile = nil
	c.Profile0DTE = nil
	c.Profile1to7 = nil
	c.ProfileTerm = nil
	for _, sub := range c.PerIndex {
		stripGammaComputedProfiles(sub)
	}
}

// RegimeIndicatorStatus is the high-level availability/freshness state
// for one row of the regime snapshot. Renderers branch on it; the
// daemon never derives green/yellow/red status from raw values (the
// spec calls those thresholds user-tunable). Specific values:
//
//   - "ok"          — the indicator carries a real, fresh measurement
//   - "stale"       — measurement returned but the gateway labeled it
//     delayed/frozen; renderer should dim
//   - "computing"   — a heavy compute is in-flight with no usable served
//     value yet; the daemon normally prewarms gamma after startup
//   - "unavailable" — IBKR doesn't carry the feed on this account; the
//     `notes` field explains why and what to do
//   - "error"       — fetch failed; `error_message` carries the reason
const (
	RegimeStatusOK          = "ok"
	RegimeStatusStale       = "stale"
	RegimeStatusComputing   = "computing"
	RegimeStatusUnavailable = "unavailable"
	RegimeStatusError       = "error"
)

// Quality is the provenance + freshness envelope for one scalar regime
// field. Attached as a sibling pointer to each value pointer; nil
// Quality means "no provenance recorded" (legacy/migration only). The
// envelope is purely additive to the row-level Status/DataType —
// renderers prefer Quality when present.
//
// FreshnessClass values:
//   - "live"     — gateway live tick observed at AsOf
//   - "frozen"   — gateway frozen/delayed tick (typically last
//     regular-session close) at AsOf
//   - "derived"  — computed from historical bars (e.g.
//     max(High) over 252 daily bars)
//   - "modelled" — computed from a model with documented caveats (the
//     gamma sweep's perfiliev-bs-sweep-v1)
//
// Confidence values:
//   - "firm"     — direct gateway measurement
//   - "estimate" — derived from historical bars
//   - "proxy"    — modelled with methodology disclosure (caller's
//     Method/warning_details document the assumptions)
type Quality struct {
	AsOf           time.Time `json:"as_of"`
	FreshnessClass string    `json:"freshness_class"`
	Confidence     string    `json:"confidence"`
	// Source is a one-line human-readable provenance description, e.g.
	// "VIX tick", "SPY 252d max(High) fallback", "perfiliev-bs-sweep-v1".
	// Surfaced by --explain; renderers can ignore it for the compact
	// in-row annotation.
	Source string `json:"source,omitempty"`
}

// Quality vocabulary separates observation provenance from confidence; these
// values are descriptive and do not independently establish authority.
const (
	FreshnessLive     = "live"
	FreshnessFrozen   = "frozen"
	FreshnessDerived  = "derived"
	FreshnessModelled = "modelled"

	ConfidenceFirm     = "firm"
	ConfidenceEstimate = "estimate"
	ConfidenceProxy    = "proxy"
)

// StreakInfo tells a consumer how many consecutive trading sessions
// an indicator has been in its current band. Closes the wire-shape
// gap with the spec's repeated "sustained 2-3 days, not single
// spikes" language — a single snapshot can't distinguish day 1 of a
// stress regime from day 5, but the streak counter makes that
// difference visible inline ("yellow · day 3").
//
// Band classification IS done daemon-side for streak purposes (a small
// violation of the "daemon doesn't derive bands" principle the spec
// states for the wire surface — but necessary for streak persistence).
// The bands used here mirror the spec's default cutoffs verbatim; a
// renderer that wants to apply a different threshold can still ignore
// Band and compute its own coloring from the row's raw measurement.
//
// Since is the YYYY-MM-DD NY-tz session key for when the current
// streak began. Sessions ≥ 1; the first session in a band is day 1.
// Indicator unavailable/computing/error states freeze the counter
// rather than reset it — a stale data point shouldn't end a streak.
type StreakInfo struct {
	Band     string `json:"band"`
	Sessions int    `json:"sessions"`
	Since    string `json:"since"`
}

// RegimeIndicatorMeta is the compact interpretation/provenance layer shared
// by every regime row. The fields are embedded into each indicator's JSON so
// agents do not have to derive bands, thresholds, or freshness from prose.
type RegimeIndicatorMeta struct {
	Band       string             `json:"band,omitempty"`
	BandReason string             `json:"band_reason,omitempty"`
	Thresholds *RegimeThresholds  `json:"thresholds,omitempty"`
	AsOf       *RegimeAsOfSummary `json:"as_of,omitempty"`
	// Freshness is the cadence-relative freshness verdict for the row's
	// banding input: "fresh" when no newer observation should exist under
	// the indicator's native cadence, "not_due" when the source's native
	// publication window has not opened yet, and "overdue" otherwise.
	// Neither not-due nor overdue evidence can make a red confirmation-
	// eligible. MaxAgeSeconds documents the
	// served staleness policy so renderers never hardcode a twin.
	Freshness *RegimeFreshness `json:"freshness,omitempty"`
	// Eligibility says whether a red band may CONFIRM stress (depth +
	// persistence + freshness gates) or is provisional. A provisional red is
	// visible and can warn only while the required input set is usable; broken
	// or overdue inputs produce lifecycle=data_quality. Nil on non-red rows.
	Eligibility *RegimeEligibility `json:"eligibility,omitempty"`
}

// RegimeFreshness is the cadence-relative freshness verdict plus the served
// staleness policy for one regime row. No wall-clock age field by design —
// ages tick every poll and would churn fingerprints and SSE hashes; clients
// derive age from the row's as_of.
type RegimeFreshness struct {
	Class         string `json:"class"`
	MaxAgeSeconds int64  `json:"max_age_seconds,omitempty"`
}

// Regime freshness values compare a row with its native publication cadence.
// Only fresh is confirmation-eligible; the rest are context or defect
// (internal-docs/design/regime-input-currency.md).
//
//   - fresh    — a current observation under the row's own cadence.
//   - not_due  — the source's publication window is closed, so no newer
//     observation can exist yet.
//   - pending  — the current period's refresh is in flight, evidenced by a
//     typed marker, inside a bounded window anchored to the period start.
//   - stale    — a known value older than its window, or a due refresh that
//     failed, inside an explicit tolerance.
//   - overdue  — a newer observation should exist and no bounded excuse
//     applies; also the fail-closed class for missing or untyped evidence.
const (
	RegimeFreshnessFresh   = "fresh"
	RegimeFreshnessNotDue  = "not_due"
	RegimeFreshnessPending = "pending"
	RegimeFreshnessStale   = "stale"
	RegimeFreshnessOverdue = "overdue"
)

// RegimeEligibility is the confirmation-eligibility verdict for a red row.
// Eligible reds may confirm stress (count toward confirmed_stress/panic,
// rescue other clusters, appear in confirmed_by); provisional reds stay
// visible and drive early_warning only. Latched reports that eligibility is
// held by the streak-lifetime latch even though the measurement wobbled back
// inside the minimum depth. Reasons name the failed gates when not eligible:
// "depth_below_min", "streak_N_of_M", "data_not_due", "data_overdue".
type RegimeEligibility struct {
	Eligible bool     `json:"eligible"`
	Latched  bool     `json:"latched,omitempty"`
	Reasons  []string `json:"reasons,omitempty"`
}

// RegimeThresholds names the heuristic threshold set used to classify an
// indicator. The string bands are intentionally compact and heterogeneous:
// each row has different units, so a label plus per-band text is friendlier
// than forcing everything into one numeric schema.
type RegimeThresholds struct {
	Label  string `json:"label,omitempty"`
	Green  string `json:"green,omitempty"`
	Yellow string `json:"yellow,omitempty"`
	Red    string `json:"red,omitempty"`
	// Trip is the compact display form of Red: the trigger a gauge face
	// prints beside its reading ("trips <40% (50d)"). It restates Red's own
	// threshold in fewer words and never introduces a second number — the
	// two are authored together at the single call site that owns this
	// indicator's bands, so a renderer never has to parse Red or invent a
	// cutoff of its own.
	Trip            string `json:"trip,omitempty"`
	Heuristic       bool   `json:"heuristic,omitempty"`
	PendingBacktest bool   `json:"pending_backtest,omitempty"`
}

// RegimeAsOfSummary is the row-level freshness badge rendered in the CLI and
// exposed in JSON/MCP. Label is the user-facing compact form ("live",
// "15m delayed", "close D-1", "cached 11:42", "2d old", "unavailable").
// Time is present when a real timestamp exists; Date is present for official
// daily files whose observation date is more meaningful than midnight UTC.
type RegimeAsOfSummary struct {
	Label      string    `json:"label"`
	Time       time.Time `json:"time,omitzero"`
	Date       string    `json:"date,omitempty"`
	Freshness  string    `json:"freshness,omitempty"`
	Source     string    `json:"source,omitempty"`
	AgeSeconds int64     `json:"age_seconds,omitempty"`
}

// RegimeVIXTerm is Indicator 1: VIX/VIX3M ratio. Watch for sustained
// inversion (ratio > 1.0) over 2-3 sessions, not a single spike.
//
// FieldsMissing is an advisory list of pointer-typed fields above
// (e.g. "vix3m", "ratio") that did NOT land within the fetch budget
// even though the row's primary measurement succeeded. Absent when
// nothing is missing. Use it to dim a sub-cell without re-classifying
// the whole row as `error`.
type RegimeVIXTerm struct {
	RegimeIndicatorMeta
	Status        string   `json:"status"`
	VIX           *float64 `json:"vix"`
	VIX3M         *float64 `json:"vix3m"`
	Ratio         *float64 `json:"ratio"` // VIX / VIX3M
	DataType      string   `json:"data_type,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// VIX previous regular-session close and the day's percent change.
	// Trading dates: tick 9 alongside the price tick — pre-open this is
	// typically the only useful daily anchor since VIX itself doesn't
	// trade. Official non-trading dates: pinned to the official daily
	// closes of the last two completed sessions, because the gateway's
	// tick-9 anchor and last print can each reset independently while
	// the market is closed. Both value fields nil when no anchor was
	// resolvable.
	VIXPrevClose *float64 `json:"vix_prev_close,omitempty"`
	VIXChangePct *float64 `json:"vix_change_pct,omitempty"` // (vix − prev_close) / prev_close × 100
	// VIXChangeBasis is the day-change provenance on closed dates. Empty on
	// trading dates (live print vs tick-9 close).
	VIXChangeBasis string `json:"vix_change_basis,omitempty"`
	// Per-scalar provenance. Each *Quality is nil when the corresponding
	// value pointer is nil; otherwise the daemon populates it at the
	// fetch site so renderers can show "firm live", "frozen", or
	// "estimate · 18s" without re-deriving from DataType.
	VIXQuality   *Quality `json:"vix_quality,omitempty"`
	VIX3MQuality *Quality `json:"vix3m_quality,omitempty"`
	// Cboe's published VIX3M daily close, read independently of the broker,
	// and what comparing it against the broker leg established. VIX3MSource
	// names which of the two observations the served VIX3M came from. The
	// broker keeps answering with a value off-window whatever its real
	// vintage; only a dated official close can settle that, so these fields
	// are what an off-window age bound and a stuck-subscription check read.
	// VIX3MGatewayLast retains the broker's own reading when the official close
	// supersedes it, so a disagreement carries both numbers rather than an
	// unevidenced claim.
	VIX3MSource       string   `json:"vix3m_source,omitempty"`
	VIX3MGatewayLast  *float64 `json:"vix3m_gateway_last,omitempty"`
	VIX3MOfficial     *float64 `json:"vix3m_official,omitempty"`
	VIX3MOfficialDate string   `json:"vix3m_official_date,omitempty"`
	VIX3MCrossCheck   string   `json:"vix3m_cross_check,omitempty"`
	// VIX3MAnchorVIX is the VIX print observed together with the served VIX3M
	// leg, and travels with it when the leg is carried across a missed poll.
	// Without the anchor, "how far has VIX moved since this VIX3M was
	// observed" is unanswerable after the first carry, and the in-session
	// carry tolerance degrades to a bare timer. Nil unless both legs were
	// observed live together.
	VIX3MAnchorVIX *float64 `json:"vix3m_anchor_vix,omitempty"`
	// Streak counts how many consecutive sessions this row's value has
	// been in its current band. Persisted across daemon restarts in daemon.db.
	// Nil when the band can't
	// be determined (computing / unavailable / error) — the streak
	// freezes rather than resets.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// Provenance of the served VIX3M leg.
const (
	VIX3MSourceGateway  = "gateway"
	VIX3MSourceOfficial = "cboe_official_close"
)

// VIX3M cross-source verdicts. In frozen mode the broker re-sends its last
// known value on request, so arrival time says nothing about a value's age and
// an index carries no trade timestamp. Cboe's dated close is therefore the only
// thing that establishes an off-window leg's vintage, and these name what that
// comparison found:
//
//   - agree: both sources described the last completed publication window and
//     matched.
//   - official_only: Cboe covered that window and the broker produced no VIX3M
//     at all, so the official close IS the leg and nothing was compared.
//   - pending_publication: Cboe has not published the last completed window's
//     close yet — it lands after the session ends. The broker leg stands in,
//     bounded to one session.
//   - disagree: both described the same window and differed. The broker leg is
//     not the close it claims to be: a stuck subscription, a lapsed market-data
//     entitlement, or a contract id that no longer resolves to VIX3M.
//   - unverified: no usable official close within one session of the last
//     completed window, so nothing corroborates the broker leg.
const (
	VIX3MCrossCheckAgree              = "agree"
	VIX3MCrossCheckOfficialOnly       = "official_only"
	VIX3MCrossCheckPendingPublication = "pending_publication"
	VIX3MCrossCheckDisagree           = "disagree"
	VIX3MCrossCheckUnverified         = "unverified"
)

// VIX3MCrossCheckVouches reports whether the verdict established the served
// off-window leg's vintage. Only a vouched leg may read not_due, because
// not_due exempts a row from every age bound; everything else fails closed.
func VIX3MCrossCheckVouches(verdict string) bool {
	switch verdict {
	case VIX3MCrossCheckAgree, VIX3MCrossCheckOfficialOnly, VIX3MCrossCheckPendingPublication:
		return true
	default:
		return false
	}
}

// RegimeHYGSPYDivergence is Indicator 2: HYG vs SPY context. The
// daemon surfaces raw measurements; the consumer compares HYG's
// current to its 50-day SMA and SPY's current to its 52-week high.
//
// FieldsMissing carries optional sub-fields ("spy_52w_high",
// "hyg_50dma") that didn't land — both are best-effort and don't
// downgrade the row's primary status.
type RegimeHYGSPYDivergence struct {
	RegimeIndicatorMeta
	Status     string   `json:"status"`
	HYGPrice   *float64 `json:"hyg_price"`
	HYG50DMA   *float64 `json:"hyg_50dma"` // 50-day SMA of HYG daily close
	SPYPrice   *float64 `json:"spy_price"`
	SPY52WHigh *float64 `json:"spy_52w_high"`
	// SPY previous regular-session close plus the day's dollar and
	// percent change. Trading dates: tick 9, emitted automatically
	// alongside the price triple. Official non-trading dates: pinned to
	// the official daily closes of the last two completed sessions,
	// because the gateway's tick-9 anchor and last print can each reset
	// independently while the market is closed. All three nil when no
	// anchor was resolvable.
	SPYPrevClose *float64 `json:"spy_prev_close,omitempty"`
	SPYChange    *float64 `json:"spy_change,omitempty"`     // last − prev_close (dollars)
	SPYChangePct *float64 `json:"spy_change_pct,omitempty"` // (last − prev_close) / prev_close × 100
	// SPYChangeBasis is the day-change provenance on closed dates. Empty on
	// trading dates (live print vs tick-9 close).
	SPYChangeBasis string   `json:"spy_change_basis,omitempty"`
	HYGDataType    string   `json:"hyg_data_type,omitempty"`
	Notes          string   `json:"notes,omitempty"`
	ErrorMessage   string   `json:"error_message,omitempty"`
	FieldsMissing  []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. SPY52WHigh has two paths (live tick 165 vs
	// history fallback); the Quality field is what the renderer reads to
	// distinguish firm-tick from derived-fallback.
	HYGQuality        *Quality `json:"hyg_quality,omitempty"`
	HYG50DMAQuality   *Quality `json:"hyg_50dma_quality,omitempty"`
	SPYQuality        *Quality `json:"spy_quality,omitempty"`
	SPY52WHighQuality *Quality `json:"spy_52w_high_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeVolOfVol is the VVIX vol-of-vol row. It uses Cboe's official
// daily VVIX time series rather than a retail-gateway quote, because
// VVIX is itself an index calculation and end-of-day source quality is
// better than pretending there is a continuously tradable instrument.
type RegimeVolOfVol struct {
	RegimeIndicatorMeta
	Status       string      `json:"status"`
	Symbol       string      `json:"symbol,omitempty"` // "VVIX"
	Last         *float64    `json:"last"`
	Change20D    *float64    `json:"change_20d_pct,omitempty"` // (last − t-20) / t-20 × 100
	AsOfDate     string      `json:"as_of_date,omitempty"`     // YYYY-MM-DD observation date
	Source       string      `json:"source,omitempty"`
	Notes        string      `json:"notes,omitempty"`
	ErrorMessage string      `json:"error_message,omitempty"`
	ValueQuality *Quality    `json:"value_quality,omitempty"`
	Streak       *StreakInfo `json:"streak,omitempty"`
}

// RegimeCreditSpreads is the official cash-credit companion to the HYG
// ETF proxy. Values are ICE BofA option-adjusted spread series retrieved
// via FRED/St. Louis Fed. Units are percentage points; e.g. 4.25 means
// 425 bp.
type RegimeCreditSpreads struct {
	RegimeIndicatorMeta
	Status        string      `json:"status"`
	HYOAS         *float64    `json:"hy_oas"`
	IGOAS         *float64    `json:"ig_oas"`
	HYIGSpread    *float64    `json:"hy_ig_spread,omitempty"`
	HY20DChange   *float64    `json:"hy_oas_20d_change,omitempty"` // percentage points
	AsOfDate      string      `json:"as_of_date,omitempty"`
	Source        string      `json:"source,omitempty"`
	Notes         string      `json:"notes,omitempty"`
	ErrorMessage  string      `json:"error_message,omitempty"`
	FieldsMissing []string    `json:"fields_missing,omitempty"`
	HYOASQuality  *Quality    `json:"hy_oas_quality,omitempty"`
	IGOASQuality  *Quality    `json:"ig_oas_quality,omitempty"`
	SpreadQuality *Quality    `json:"spread_quality,omitempty"`
	Streak        *StreakInfo `json:"streak,omitempty"`
}

// RegimeFundingStress is the OFR-style U.S. funding spread row:
// 90-day AA financial commercial paper rate minus 3-month Treasury bill
// rate, both from official Federal Reserve/FRED series. Units are basis
// points.
type RegimeFundingStress struct {
	RegimeIndicatorMeta
	Status         string      `json:"status"`
	CP3M           *float64    `json:"cp_3m_rate"`
	TBill3M        *float64    `json:"tbill_3m_rate"`
	SpreadBps      *float64    `json:"spread_bps"`
	AsOfDate       string      `json:"as_of_date,omitempty"`
	Source         string      `json:"source,omitempty"`
	Notes          string      `json:"notes,omitempty"`
	ErrorMessage   string      `json:"error_message,omitempty"`
	FieldsMissing  []string    `json:"fields_missing,omitempty"`
	CP3MQuality    *Quality    `json:"cp_3m_quality,omitempty"`
	TBill3MQuality *Quality    `json:"tbill_3m_quality,omitempty"`
	SpreadQuality  *Quality    `json:"spread_quality,omitempty"`
	Streak         *StreakInfo `json:"streak,omitempty"`
}

// RegimeUSDJPY is the FX-carry stress row: USD/JPY exchange rate. Spec measures
// "weekly move" — daemon surfaces last and 7-trading-days-ago close so
// the consumer can compute the change. Source is FX-pair routing
// (CASH/IDEALPRO); routing arrives in a sibling commit.
type RegimeUSDJPY struct {
	RegimeIndicatorMeta
	Status        string   `json:"status"`
	Symbol        string   `json:"symbol"` // "USD.JPY" canonical form
	Last          *float64 `json:"last"`
	Close7DAgo    *float64 `json:"close_7d_ago"`      // close from 7 trading days ago
	WeeklyChange  *float64 `json:"weekly_change_pct"` // (last − close_7d_ago) / close_7d_ago × 100
	DataType      string   `json:"data_type,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	ErrorMessage  string   `json:"error_message,omitempty"`
	FieldsMissing []string `json:"fields_missing,omitempty"`
	// Per-scalar provenance. Last is firm-live (or firm-frozen);
	// Close7DAgo is always estimate-derived (MIDPOINT historical bar).
	LastQuality       *Quality `json:"last_quality,omitempty"`
	Close7DAgoQuality *Quality `json:"close_7d_ago_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeGammaZero is the existing gamma.zero_spx
// envelope embedded inline. Auto-kicked by regime.snapshot on first
// call of an NY trading day; subsequent calls return the cached
// result. Method token + warning_details carry methodology disclosures.
type RegimeGammaZero struct {
	RegimeIndicatorMeta
	Status        string             `json:"status"`
	Envelope      GammaZeroSPXResult `json:"envelope"`
	Notes         string             `json:"notes,omitempty"`
	FieldsMissing []string           `json:"fields_missing,omitempty"`
	// Per-scalar provenance for the two values the renderer prints:
	// ZeroGamma is proxy-modelled (carries the perfiliev-bs-sweep
	// methodology); GammaTotalAbs is estimate-derived (sign-agnostic
	// notional summed over observed OI+IV).
	ZeroGammaQuality     *Quality `json:"zero_gamma_quality,omitempty"`
	GammaTotalAbsQuality *Quality `json:"gamma_total_abs_quality,omitempty"`
	// HorizonAgreement names how a single-underlying envelope's three
	// horizon-bucketed γ-zero readings (0DTE, 1-7, term) relate. Empty
	// for combined SPY+SPX results, where horizon buckets live under
	// per_index.SPY / per_index.SPX. One of:
	//
	//   - "all_long"             every usable bucket is long-γ
	//   - "all_short"            every usable bucket is short-γ
	//   - "all_transition"       every usable bucket is within ±2% of
	//                            its γ-zero
	//   - "diverge:0dte_vs_term"  0DTE and term buckets disagree
	//                            (highest-information case — short-fuse
	//                            flow disagrees with monthly positioning)
	//   - "diverge:partial"       other mixed cases (1-7 alone disagrees,
	//                            only two usable buckets disagree, etc.)
	//   - "0dte_only" / "1to7_only" / "term_only" — only one bucket is
	//                            usable
	//   - ""                     no bucket has a usable signal
	//
	// The renderer annotates the row whenever the value starts with
	// "diverge:" or ends in "_only" — those are the cases where the
	// combined headline doesn't tell the full story.
	HorizonAgreement string `json:"horizon_agreement,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeBreadth is Indicator 5: the existing breadth.spx envelope
// embedded inline. The daemon's local 50-DMA engine computes S5FI from
// constituent daily closes (see BreadthSPXResult.Method). On the first
// call against a fresh daemon the cold-start fan-out runs (~60 min,
// IBKR-paced), so this row typically surfaces Status="computing" with
// a notes pointer; subsequent calls return Status="ok" from the
// persisted cache.
type RegimeBreadth struct {
	RegimeIndicatorMeta
	Status        string           `json:"status"`
	Envelope      BreadthSPXResult `json:"envelope"`
	Notes         string           `json:"notes,omitempty"`
	FieldsMissing []string         `json:"fields_missing,omitempty"`
	// PctAbove50DMA / PctAbove200DMA / NewHighsToday / NewLowsToday /
	// NetNewHighsPct are surfaced directly on the regime row so a
	// consumer doesn't have to dig into Envelope for the four-number
	// breadth view that informs the band. Echoed from Envelope; same
	// values, same units.
	PctAbove50DMA  float64 `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighsToday  int     `json:"new_highs_today,omitempty"`
	NewLowsToday   int     `json:"new_lows_today,omitempty"`
	NetNewHighsPct float64 `json:"net_new_highs_pct,omitempty"`
	// Per-scalar provenance for the breadth percentage. firm-live or
	// firm-frozen when ranked, depending on the envelope's DataType;
	// nil during cold start or when the engine refused to persist
	// because constituent coverage fell below the safety threshold.
	ValueQuality *Quality `json:"value_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	// RegimeVIXTerm.Streak for the semantics.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeSnapshotParams is the input for MethodRegimeSnapshot. Empty
// body means "fetch all regime indicators with default parameters." A
// future caller could trim to a subset, but v1 always returns all
// rows — partial responses are surfaced via per-indicator Status.
type RegimeSnapshotParams struct{}

// RegimeSnapshotResult is the wire payload for the dashboard
// generator and the MCP natural-language interface. One JSON
// envelope, all rows. Each row carries:
//   - raw measurements plus compact band/as-of metadata for agents
//   - a `notes` field embedding the full methodology prose for
//     explain-mode consumers
//   - a structured Status the renderer branches on for UI state
//
// Compatibility note for renderers: the daemon never returns nil for
// any indicator field — empty / unavailable indicators surface
// Status="unavailable" with populated Notes. Numerical fields are
// pointers so "not arrived yet" vs "exactly zero" stays
// distinguishable.
type RegimeSnapshotResult struct {
	AsOf time.Time `json:"as_of"`
	// AuthorityHealth describes how the daemon obtained this response. It is
	// response/cache metadata, not classified market evidence: semantic regime
	// fingerprints must ignore it. Nil preserves compatibility with older
	// daemons; the Phase 1 authority populates it on every served last-good
	// snapshot. Cold authorities fail with CodeRegimeUnavailable instead of
	// manufacturing an empty or partial RegimeSnapshotResult.
	AuthorityHealth *RegimeAuthorityHealth `json:"authority_health,omitempty"`
	// TapeSessionState classifies the official US cash-equity calendar date
	// this snapshot was taken on (TapeSessionFor). On a closed date
	// (weekend/holiday) the direct SPY/VIX day-change prints are frozen
	// last-session values — the anchors can even reset independently while
	// closed — so they carry evidence but cannot enter or hold tape-driven
	// lifecycle stages. Empty means outside embedded calendar coverage:
	// tape terms keep full effect (fail-open). Excluded from the regime
	// fingerprint projection so weekday/weekend tickover alone never
	// re-fires alerts.
	TapeSessionState  string                 `json:"tape_session_state,omitempty"`
	TapeSessionReason string                 `json:"tape_session_reason,omitempty"`
	TapeNextOpen      *time.Time             `json:"tape_next_open,omitempty"`
	Fingerprint       Fingerprint            `json:"fingerprint"`
	Lifecycle         LifecycleState         `json:"lifecycle,omitzero"`
	Summary           RegimeSummary          `json:"summary"`
	Posture           RegimePosture          `json:"posture,omitzero"`
	VIXTermStructure  RegimeVIXTerm          `json:"vix_term_structure"`
	VolOfVol          RegimeVolOfVol         `json:"vol_of_vol"`
	HYGSPYDivergence  RegimeHYGSPYDivergence `json:"hyg_spy_divergence"`
	CreditSpreads     RegimeCreditSpreads    `json:"credit_spreads"`
	FundingStress     RegimeFundingStress    `json:"funding_stress"`
	USDJPY            RegimeUSDJPY           `json:"usd_jpy"`
	GammaZero         RegimeGammaZero        `json:"gamma_zero"`
	Breadth           RegimeBreadth          `json:"breadth"`
	// Composite carries the daemon-side rollup the CLI shows above the
	// indicator rows (verdict + ranked/unranked counts), so MCP consumers
	// don't have to recompute it from per-row Status fields. Populated on
	// every response.
	Composite RegimeComposite `json:"composite"`
	// WarningDetails carries structured, row-scoped data-quality issues
	// that affected this snapshot but did not make the whole RPC fail.
	// Agent surfaces should prefer these over parsing ErrorMessage strings:
	// each warning states what happened, what it changes in the composite,
	// and the next useful action.
	WarningDetails []RegimeWarning `json:"warning_details,omitempty"`
	// DataQuality carries the same high-level data-quality summary used by
	// status.health: degraded gamma and stale regime clusters. It is coarser
	// than WarningDetails by design, so humans and agents can decide whether
	// to interpret the regime read carefully without walking every row.
	DataQuality []DataQualityHealth `json:"data_quality,omitempty"`
	// SourceHealth is the orchestration-facing freshness/readiness summary
	// for each broad-market source cluster. It avoids timestamp-sensitive
	// alert churn: fingerprints hash the classified status, not age.
	SourceHealth []SourceHealth `json:"source_health,omitempty"`
	// SpecDoc points consumers (especially LLM-driven ones) at the
	// canonical methodology + threshold reference so they don't
	// hallucinate band edges. It is the published URL rather than a
	// repository path: a remote MCP client can fetch the former and
	// cannot open the latter. Same value on every response.
	SpecDoc string `json:"spec_doc"`
}

// RegimeSummary is the compact, agent-first reading of a regime snapshot.
// It deliberately avoids probabilities or trade instructions: the fields
// describe the evidence balance, coverage, and current condition only.
type RegimeSummary struct {
	Label             string   `json:"label"`
	Evidence          string   `json:"evidence"` // cluster-level balance
	IndicatorEvidence string   `json:"indicator_evidence,omitempty"`
	PunchLine         string   `json:"punch_line"`
	Confidence        string   `json:"confidence"`
	DominantRisks     []string `json:"dominant_risks,omitempty"`
	NotAdvice         string   `json:"not_advice,omitempty"`
}

// RegimePosture is the canonical display/policy read for market-regime
// consumers. UIs should render this instead of deriving their own tone from raw
// cluster counts.
type RegimePosture struct {
	Label      string `json:"label,omitempty"`
	Tone       string `json:"tone,omitempty"`
	Stage      string `json:"stage,omitempty"`
	Severity   string `json:"severity,omitempty"`
	Readiness  string `json:"readiness,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
}

// RegimeWarning is a structured data-quality or availability issue scoped
// to one regime indicator. Severity is "info", "warning", or "error" from
// the point of view of interpreting the snapshot, not the RPC transport.
type RegimeWarning struct {
	Code     string `json:"code"`
	Scope    string `json:"scope"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Impact   string `json:"impact"`
	Action   string `json:"action"`
}

// RegimeComposite is the daemon-side rollup of the regime rows.
// Verdict mirrors the CLI's text rendering verbatim ("Normal regime",
// "Stress signal present", etc.) so consumers can show the same
// non-advisory headline without re-implementing the band logic. The raw
// row counts are exposed alongside cluster counts so related signals
// (e.g. VIX term structure + VVIX, HYG proxy + cash credit spreads) do
// not double-count as independent macro confirmations.
//
// RankedCount + UnrankedCount sum to the indicator count; cluster counts
// sum to the cluster count. Verdict is based on clusters, not raw rows.
type RegimeComposite struct {
	Verdict              string `json:"verdict"`
	GreenCount           int    `json:"green_count"`
	YellowCount          int    `json:"yellow_count"`
	RedCount             int    `json:"red_count"`
	RankedCount          int    `json:"ranked_count"`
	UnrankedCount        int    `json:"unranked_count"`
	ClusterGreenCount    int    `json:"cluster_green_count"`
	ClusterYellowCount   int    `json:"cluster_yellow_count"`
	ClusterRedCount      int    `json:"cluster_red_count"`
	ClusterRankedCount   int    `json:"cluster_ranked_count"`
	ClusterUnrankedCount int    `json:"cluster_unranked_count"`
	// ClusterEligibleRedCount counts red clusters whose evidence passed the
	// confirmation-eligibility gates (depth + persistence + freshness) and
	// survived the isolated-red downgrades. Only these reds may confirm
	// stress. ClusterProvisionalRedCount counts the remaining raw reds —
	// visible, early-warning evidence, never confirmation.
	ClusterEligibleRedCount    int `json:"cluster_eligible_red_count"`
	ClusterProvisionalRedCount int `json:"cluster_provisional_red_count,omitempty"`
}

// Quote is the daemon's snapshot result.
//
// RegularClose is the latest completed regular-session close (daily bars when
// the market is closed; the gateway close tick during regular hours).
// QuotePrice is the current live/pre/post/overnight indication selected from
// last → mark → bid/ask midpoint → bid → ask. Price/PriceSource are retained
// as the legacy selected-price pair and mirror QuotePrice when an indicative
// quote exists, otherwise RegularClose. PrevClose is the comparison anchor for
// Price: usually RegularClose for an indicative/live quote, and
// PriorRegularClose for a historical-close-only row. No fabrication: missing
// price families stay nil.
//
// Unit conventions:
//   - ChangePct is in PERCENT units (0.70 means 0.70 %, not 70 %). The
//     CLI renderer appends a trailing % without multiplying.
//   - IV is a DECIMAL FRACTION (0.247 means 24.7 %). The CLI renderer
//     multiplies by 100 before printing. Same convention across every
//     IV-bearing field in this package (chain expiries, chain strikes,
//     scan rows, position rows).
type Quote struct {
	Symbol   string         `json:"symbol"`
	Contract ContractParams `json:"contract"`
	Bid      *float64       `json:"bid"`
	Ask      *float64       `json:"ask"`
	Last     *float64       `json:"last"`
	Mark     *float64       `json:"mark,omitempty"`
	// Price is the legacy selected price: QuotePrice when the gateway has a
	// current indication, otherwise RegularClose. PriceSource names the
	// selected input so consumers can avoid treating a close-only fallback
	// as a live last trade.
	Price               *float64  `json:"price,omitempty"`
	PriceSource         string    `json:"price_source,omitempty"`
	RegularClose        *float64  `json:"regular_close,omitempty"`
	RegularCloseAt      time.Time `json:"regular_close_at,omitzero"`
	PriorRegularClose   *float64  `json:"prior_regular_close,omitempty"`
	RegularChange       *float64  `json:"regular_change,omitempty"`
	RegularChangePct    *float64  `json:"regular_change_pct,omitempty"`
	QuotePrice          *float64  `json:"quote_price,omitempty"`
	QuotePriceSource    string    `json:"quote_price_source,omitempty"`
	QuotePriceAt        time.Time `json:"quote_price_at,omitzero"`
	QuotePriceAsOf      string    `json:"quote_price_as_of,omitempty"`
	QuoteChange         *float64  `json:"quote_change,omitempty"`
	QuoteChangePct      *float64  `json:"quote_change_pct,omitempty"`
	PrevClose           *float64  `json:"prev_close"`
	Change              *float64  `json:"change"`
	ChangePct           *float64  `json:"change_pct"`
	DayHigh             *float64  `json:"day_high,omitempty"`
	DayLow              *float64  `json:"day_low,omitempty"`
	Week52High          *float64  `json:"week_52_high,omitempty"`
	Week52Low           *float64  `json:"week_52_low,omitempty"`
	BidSize             *int      `json:"bid_size,omitempty"`
	AskSize             *int      `json:"ask_size,omitempty"`
	Volume              *int64    `json:"volume,omitempty"`
	AvgVolume           *int64    `json:"avg_volume,omitempty"`
	AvgVolume20D        *int64    `json:"avg_volume_20d,omitempty"`
	AvgDollarVolume20D  *float64  `json:"avg_dollar_volume_20d,omitempty"`
	LiquidityStatus     string    `json:"liquidity_status,omitempty"` // ok | partial | unavailable
	LiquiditySource     string    `json:"liquidity_source,omitempty"` // daily_bars
	LiquidityAsOf       time.Time `json:"liquidity_as_of,omitzero"`
	LiquiditySampleDays int       `json:"liquidity_sample_days,omitempty"`
	IV                  *float64  `json:"iv"`
	IVStatus            string    `json:"iv_status"`
	DataType            string    `json:"data_type"`
	FeedType            string    `json:"feed_type,omitempty"`
	SpreadPct           *float64  `json:"spread_pct,omitempty"`
	// QuoteQuality is a compact machine hint: "firm", "indicative",
	// "wide", "prev_close", "stale", or "missing". It summarizes the
	// selected price and spread/session context; WarningDetails carries
	// the explainable reasons.
	QuoteQuality string `json:"quote_quality,omitempty"`
	Indicative   bool   `json:"indicative,omitempty"`
	VolumePhase  string `json:"volume_phase,omitempty"`
	// PriceAt is the best timestamp for Price. For last trades this is
	// IBKR tick-string 45 when delivered; for prev_close fallbacks it is
	// the official prior regular-session close; otherwise it is the local
	// observation time. PriceAsOf is the preformatted human label renderers
	// can show directly ("At close: May 22 at 04:01:02 PM EDT").
	PriceAt        time.Time     `json:"price_at,omitzero"`
	PriceAsOf      string        `json:"price_as_of,omitempty"`
	Stale          bool          `json:"stale,omitempty"`
	StaleReason    string        `json:"stale_reason,omitempty"`
	WarningDetails []DataWarning `json:"warning_details,omitempty"`
	AsOf           time.Time     `json:"as_of"`
	// SessionContext explains whether the relevant market was open at the
	// quote time. Populated when the context is useful for interpreting a
	// stale/frozen/missing quote; omitted on ordinary live in-session rows.
	SessionContext *MarketSession `json:"session_context,omitempty"`
}

// WatchlistResult is the daemon-backed rich watchlist surface used by
// `canary watch --quotes --json` and MCP `canary_watch` when include_quotes is
// true. The persisted local watchlist remains [watchlist.Snapshot]; this
// type is a read-only market-data view over those saved symbols.
type WatchlistResult struct {
	Name    string         `json:"name"`
	Symbols []string       `json:"symbols"`
	Rows    []WatchlistRow `json:"rows"`
	AsOf    time.Time      `json:"as_of"`
}

// WatchlistRow flattens the Quote fields and optionally annotates a saved
// symbol with the user's held stock position. Error is per-row so one dead
// symbol does not hide the rest of a watchlist.
type WatchlistRow struct {
	Quote
	Holding *WatchlistHolding `json:"holding,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// WatchlistHolding is intentionally compact and stock-focused so watchlist
// refreshes can reuse the cheap `positions.list type=stk` path instead of
// the full option-Greeks prewarm.
type WatchlistHolding struct {
	Quantity      float64  `json:"quantity"`
	AvgCost       float64  `json:"avg_cost"`
	Mark          float64  `json:"mark"`
	MarketValue   float64  `json:"market_value_ccy"`
	UnrealizedPnL float64  `json:"unrealized_pnl_ccy"`
	DailyPnL      *float64 `json:"daily_pnl_ccy,omitempty"`
	Exchange      string   `json:"exchange,omitempty"`
	Currency      string   `json:"currency,omitempty"`
}

// Frame is a single streaming tick. DataType carries the gateway's
// per-reqID market-data-type notice (live / frozen / delayed /
// delayed-frozen) so adapters can render a badge — important after
// hours, where frozen mode delivers a single snapshot and then goes
// silent. Empty string means "unknown" (the gateway hasn't sent the
// notice yet); the CLI treats that the same as "live" for rendering.
//
// Error is the terminal-error variant: when populated, the price/size
// fields are nil and this is the last frame the consumer will receive
// on the subscription. Codes are the FrameErr* constants. Backward-
// compatible because of omitempty — older consumers parsing tick frames
// see no Error field and continue to work.
type Frame struct {
	T        time.Time   `json:"t"`
	Bid      *float64    `json:"bid,omitempty"`
	Ask      *float64    `json:"ask,omitempty"`
	Last     *float64    `json:"last,omitempty"`
	BidSize  *int        `json:"bid_size,omitempty"`
	AskSize  *int        `json:"ask_size,omitempty"`
	DataType string      `json:"data_type,omitempty"`
	Error    *FrameError `json:"error,omitempty"`
}

// FrameError is the terminal error payload carried in Frame.Error. Code is
// one of the FrameErr* constants; Message is a single-sentence human
// description suitable for surfacing in CLI/MCP client output.
type FrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// PositionView is the wire shape of one position returned to adapters.
//
// DayChange / DayChangePct describe how far the account valuation mark sits
// from RegularClose. Pointers so "no data" (no daily bar yet, options where
// we don't track contract-level prev close) is distinct from "exactly flat".
// The daemon caches close anchors per underlying so the first call pre-warms
// and subsequent ones are instant — no fabrication.
//
// MarketValue is serialized as market_value_ccy: the broker's own market value
// for the position, in the contract currency named by the row's Currency field.
// It is NOT derived from Mark and Quantity. A bond quotes as a percentage of
// par against a face-value quantity, so the product overstates such a row by
// roughly 100×; the two agree only for equities and options, which is what made
// the old identity look like a definition.
// MarketValueBase / UnrealizedPnLBase / RealizedPnLBase / DailyPnLBase are
// populated when the daemon knows the account base currency and either the row
// is already in base currency or FXRate is available. nil = conversion
// unavailable, never zero-substituted.
//
// FXRate is the gateway-reported BASE/CCY conversion. It stays nil on
// same-currency books because the conversion is implicitly 1.0.
//
// Delta/Gamma/Theta/Vega populate on option positions when the daemon
// captured a valid model-computation tick within budget. nil = unavailable
// (illiquid leg, OOH model abstention, subscribe slot churn) — never zero-
// substituted.
type PositionView struct {
	Symbol       string  `json:"symbol"`
	SecType      string  `json:"sec_type"`
	ConID        int     `json:"con_id,omitempty"`
	Exchange     string  `json:"exchange,omitempty"`
	Currency     string  `json:"currency,omitempty"`
	LocalSymbol  string  `json:"local_symbol,omitempty"`
	TradingClass string  `json:"trading_class,omitempty"`
	Quantity     float64 `json:"quantity"`
	// Multiplier is the contract multiplier — 1 for stocks, 100 for standard
	// equity options, sometimes higher for index options. Needed by JSON
	// consumers to convert between per-share Mark and per-contract AvgCost
	// on options (IBKR's averageCost is multiplier-inclusive on OPT).
	Multiplier    int     `json:"multiplier"`
	AvgCost       float64 `json:"avg_cost"`
	Mark          float64 `json:"mark"`
	ValuationMark float64 `json:"valuation_mark,omitempty"`
	DataType      string  `json:"data_type,omitempty"`
	// PriceSource names the quote input that produced the row's quote
	// context (last, mark, prev_close, historical_close, ...). Optional
	// because position marks can arrive from the portfolio stream before
	// the daemon has enriched the row with quote data.
	PriceSource       string    `json:"price_source,omitempty"`
	RegularClose      *float64  `json:"regular_close,omitempty"`
	RegularCloseAt    time.Time `json:"regular_close_at,omitzero"`
	PriorRegularClose *float64  `json:"prior_regular_close,omitempty"`
	RegularChange     *float64  `json:"regular_change,omitempty"`
	RegularChangePct  *float64  `json:"regular_change_pct,omitempty"`
	QuotePrice        *float64  `json:"quote_price,omitempty"`
	QuotePriceSource  string    `json:"quote_price_source,omitempty"`
	QuotePriceAt      time.Time `json:"quote_price_at,omitzero"`
	QuotePriceAsOf    string    `json:"quote_price_as_of,omitempty"`
	QuoteChange       *float64  `json:"quote_change,omitempty"`
	QuoteChangePct    *float64  `json:"quote_change_pct,omitempty"`
	PrevClose         *float64  `json:"prev_close,omitempty"`
	Bid               *float64  `json:"bid,omitempty"`
	Ask               *float64  `json:"ask,omitempty"`
	// DayChange is per-share for stocks (Mark − stock prev close); for
	// options it stays nil because we don't track contract-level prev
	// close on the underlying-grouped path. DayChangePct is the same
	// ratio expressed as a percent. DayChangeMoney is the *position*-level
	// dollar impact: quantity × DayChange for stocks; quantity × multiplier
	// × (Mark − OptionPrevClose) for options when OptionPrevClose is
	// populated. nil when any input is missing — never fabricated.
	DayChange      *float64      `json:"day_change,omitempty"`
	DayChangePct   *float64      `json:"day_change_pct,omitempty"`
	DayChangeMoney *float64      `json:"day_change_money,omitempty"`
	DayHigh        *float64      `json:"day_high,omitempty"`
	DayLow         *float64      `json:"day_low,omitempty"`
	Week52High     *float64      `json:"week_52_high,omitempty"`
	Week52Low      *float64      `json:"week_52_low,omitempty"`
	Volume         *int64        `json:"volume,omitempty"`
	AvgVolume      *int64        `json:"avg_volume,omitempty"`
	PriceAt        time.Time     `json:"price_at,omitzero"`
	PriceAsOf      string        `json:"price_as_of,omitempty"`
	FeedType       string        `json:"feed_type,omitempty"`
	SpreadPct      *float64      `json:"spread_pct,omitempty"`
	QuoteQuality   string        `json:"quote_quality,omitempty"`
	Indicative     bool          `json:"indicative,omitempty"`
	VolumePhase    string        `json:"volume_phase,omitempty"`
	Stale          bool          `json:"stale,omitempty"`
	StaleReason    string        `json:"stale_reason,omitempty"`
	WarningDetails []DataWarning `json:"warning_details,omitempty"`
	// QuoteExpectation classifies whether market data should exist for this
	// contract at all. QuoteExpectationNone means a missing quote, mark, or
	// market-event flag is the expected and correct state, so consumers must
	// not render it as a data-quality defect or as exposure of unknown size.
	// The position stays account truth either way.
	QuoteExpectation       string `json:"quote_expectation,omitempty"`
	QuoteExpectationReason string `json:"quote_expectation_reason,omitempty"`
	// SessionContext explains the trading-calendar state behind PriceAt.
	// Populated when the quote context needs interpretation (closed,
	// pre-market, frozen/stale/missing), matching Quote.SessionContext.
	SessionContext    *MarketSession `json:"session_context,omitempty"`
	MarketValue       float64        `json:"market_value_ccy"`
	MarketValueBase   *float64       `json:"market_value_base,omitempty"`
	FXRate            *float64       `json:"fx_rate,omitempty"`
	UnrealizedPnL     float64        `json:"unrealized_pnl_ccy"`
	UnrealizedPnLBase *float64       `json:"unrealized_pnl_base,omitempty"`
	RealizedPnL       float64        `json:"realized_pnl_ccy"`
	RealizedPnLBase   *float64       `json:"realized_pnl_base,omitempty"`

	// DailyPnL is the start-of-trading-day to now P&L for this single
	// contract, sourced from IBKR's reqPnLSingle stream (TWS msg 95).
	// Distinct from UnrealizedPnL above, which is session-running.
	// nil means "no subscription yet" (daemon hasn't pre-warmed this
	// conId), "no frame received yet", "no entitlement", or "DBL_MAX
	// sentinel". Never zero-substituted. For options, the daily figure
	// can swing dramatically on small underlying moves; consumers
	// rendering a per-leg value should pair it with the option's
	// effective delta to interpret responsibly.
	DailyPnL     *float64 `json:"daily_pnl_ccy,omitempty"`
	DailyPnLBase *float64 `json:"daily_pnl_base,omitempty"`

	// Option-only fields (zero values when not applicable).
	Expiry string  `json:"expiry,omitempty"`
	Strike float64 `json:"strike,omitempty"`
	Right  string  `json:"right,omitempty"`

	Delta *float64 `json:"delta,omitempty"`
	Gamma *float64 `json:"gamma,omitempty"`
	Theta *float64 `json:"theta,omitempty"`
	Vega  *float64 `json:"vega,omitempty"`

	// Option-only contract-level fields populated from the per-leg
	// market-data subscription that captures Greeks (msg 21 tickType 13)
	// plus tick 1/2/9 for the option itself. Nil when the subscription
	// budget expired without delivering the tick — never zero-substituted.
	//
	// OptionBid / OptionAsk let callers detect a wide spread on illiquid
	// contracts where the mark is a midpoint that may not be tradable.
	// OptionPrevClose is the option contract's own prior settle (NOT the
	// underlying's PrevClose above); required for option-level daily P&L
	// without the underlying-vs-option confusion the agent-feedback flagged.
	// IV is the model-computation implied volatility for this leg.
	OptionBid         *float64 `json:"option_bid,omitempty"`
	OptionAsk         *float64 `json:"option_ask,omitempty"`
	OptionPrevClose   *float64 `json:"option_prev_close,omitempty"`
	IV                *float64 `json:"iv,omitempty"`
	MarkOutsideBidAsk bool     `json:"mark_outside_bid_ask,omitempty"`

	// Underlying is the model-computation underlying spot IBKR sent alongside
	// the Greeks (msg 21 tickType 13). The portfolio aggregator pairs delta
	// with this spot to compute dollar delta, so the dollar figure stays
	// consistent with the delta it was modelled against — pairing today's
	// delta with the underlying's prior close gives an apples-and-oranges
	// answer that lies by the size of any overnight gap. nil when the per-
	// leg Greeks tick didn't carry a spot (illiquid leg, model abstention)
	// — never zero-substituted.
	Underlying *float64 `json:"underlying,omitempty"`
}

// PositionsResult wraps the array so the daemon can attach metadata later.
// ByUnderlying groups stock + option legs per underlying — always populated
// so JSON consumers can rely on it. The CLI's `--by underlying` view renders
// from this; the default view keeps the flat Stocks/Options arrays.
//
// Portfolio is populated when at least one option leg has captured Greeks
// and/or any non-base currency holding has a known FX rate. Aggregates are
// computed daemon-side so JSON consumers and the CLI render the same
// numbers. Always-non-nil pointer; fields inside are nil when their inputs
// were unavailable — see PositionsPortfolio doc for the contract.
type PositionsResult struct {
	// DataType reflects the per-position mark-price feed when the daemon
	// can summarise it; left empty (omitted) when positions arrive purely
	// from the portfolio update stream without per-symbol feed state.
	DataType           string                     `json:"data_type,omitempty"`
	AsOf               time.Time                  `json:"as_of"`
	Stocks             []PositionView             `json:"stocks"`
	Options            []PositionView             `json:"options"`
	ByUnderlying       []PositionGroup            `json:"by_underlying"`
	Portfolio          *PositionsPortfolio        `json:"portfolio,omitempty"`
	ProtectionCoverage *ProtectionCoverageSummary `json:"protection_coverage,omitempty"`
	AccountID          string                     `json:"account_id,omitempty"`
}

// PositionsPortfolio is the daemon-side aggregator across all open legs.
//
// EffectiveDelta sums per-leg signed share-equivalent exposure:
//   - stocks contribute their signed quantity (long 100 sh => +100)
//   - options contribute delta × signed contract qty × multiplier
//
// DollarDelta multiplies each leg's share-equivalent exposure by the
// leg's contract-currency spot and sums; result is reported in
// DollarDeltaCurrency. For a mixed-currency book this is the dominant
// contract currency (e.g. USD when every option references a USD
// underlying); callers that need a single base-currency rollup combine
// it with the AccountResult.CurrencyExposure FX rate.
//
// DailyTheta is Σ (theta × signed contract qty × multiplier). IBKR
// already reports theta as daily decay, so the sum is the daily P&L
// from time decay assuming everything else holds. The value is in
// DailyThetaCurrency, computed with the same single-ccy-or-"MIX"
// convention as DollarDeltaCurrency: a single ISO code when every
// contributing option leg agrees, "MIX" when not. Renderers should
// surface "MIX" rather than picking a symbol — the sum is genuinely
// undefined in mixed-currency books.
//
// GreeksCoverage is the count of option legs whose Greeks were captured
// over the total — useful for the renderer to flag partial coverage
// when the model tick didn't arrive for some legs.
type PositionsPortfolio struct {
	EffectiveDelta          *float64             `json:"effective_delta,omitempty"`
	DollarDelta             *float64             `json:"dollar_delta_ccy,omitempty"`
	DollarDeltaCurrency     string               `json:"dollar_delta_ccy_currency,omitempty"`
	DollarDeltaBase         *float64             `json:"dollar_delta_base,omitempty"`
	DollarDeltaBaseCurrency string               `json:"dollar_delta_base_currency,omitempty"`
	DailyTheta              *float64             `json:"daily_theta_ccy,omitempty"`
	DailyThetaCurrency      string               `json:"daily_theta_ccy_currency,omitempty"`
	DailyThetaBase          *float64             `json:"daily_theta_base,omitempty"`
	DailyThetaBaseCurrency  string               `json:"daily_theta_base_currency,omitempty"`
	Gamma                   *float64             `json:"gamma,omitempty"`
	Vega                    *float64             `json:"vega,omitempty"`
	GreeksCoverage          int                  `json:"greeks_coverage"`
	GreeksTotal             int                  `json:"greeks_total"`
	BaseCurrency            string               `json:"base_currency,omitempty"`
	NetLiquidationBase      *float64             `json:"net_liquidation_base,omitempty"`
	ExposureBase            []UnderlyingExposure `json:"exposure_base,omitempty"`

	// ExposureUnmeasured names the held underlyings absent from ExposureBase:
	// no base-currency market value could be computed for the group, so it
	// carries no row at all rather than a zero one. A consumer that compares an
	// ExposureBase subtotal against a threshold must read a non-empty list as
	// proof the subtotal is partial — the sum understates, and understatement
	// is the quiet direction. Empty on a fully measured book.
	ExposureUnmeasured []string `json:"exposure_unmeasured,omitempty"`

	// FXSensitivityPerPct estimates the change in base-currency P&L for a 1%
	// move in the non-base contract currency, holding everything else
	// constant. Computed as Σ (non-base market value in base) × 0.01.
	// Useful as a quick answer to "how much of my book is FX-exposed?".
	FXSensitivityPerPct *float64 `json:"fx_sensitivity_per_pct,omitempty"`
	FXBaseCurrency      string   `json:"fx_base_currency,omitempty"`
}

// PositionGroup aggregates the stock leg (if any) and option legs per
// underlying. GroupUnrealizedPnL/GroupMarketValue are serialized with *_ccy
// names because they are local/security-currency sums across all legs. *_base
// fields are filled only when every contributing row can be converted to the
// account base currency. GroupEffectiveDelta / GroupDollarDelta are coherent
// per-underlying exposures (unlike portfolio-level share-equivalent sums
// across unrelated symbols).
type PositionGroup struct {
	Underlying               string         `json:"underlying"`
	Stock                    *PositionView  `json:"stock,omitempty"`
	Options                  []PositionView `json:"options"`
	GroupMarketValue         float64        `json:"group_market_value_ccy"`
	GroupMarketValueBase     *float64       `json:"group_market_value_base,omitempty"`
	GroupMarketValuePctNLV   *float64       `json:"group_market_value_pct_nlv,omitempty"`
	GroupUnrealizedPnL       float64        `json:"group_unrealized_pnl_ccy"`
	GroupUnrealizedPnLBase   *float64       `json:"group_unrealized_pnl_base,omitempty"`
	GroupDailyPnLBase        *float64       `json:"group_daily_pnl_base,omitempty"`
	GroupEffectiveDelta      *float64       `json:"group_effective_delta,omitempty"`
	GroupDollarDelta         *float64       `json:"group_dollar_delta_ccy,omitempty"`
	GroupDollarDeltaCurrency string         `json:"group_dollar_delta_ccy_currency,omitempty"`
	GroupDollarDeltaBase     *float64       `json:"group_dollar_delta_base,omitempty"`
}

// UnderlyingExposure is the compact base-currency exposure table embedded in
// PositionsPortfolio. Rows are sorted by absolute MarketValueBase descending
// so agents can read the dominant exposures without re-aggregating.
type UnderlyingExposure struct {
	Underlying        string   `json:"underlying"`
	MarketValueBase   float64  `json:"market_value_base"`
	MarketValuePctNLV *float64 `json:"market_value_pct_nlv,omitempty"`
	EffectiveDelta    *float64 `json:"effective_delta,omitempty"`
	DollarDeltaBase   *float64 `json:"dollar_delta_base,omitempty"`
	UnrealizedPnLBase *float64 `json:"unrealized_pnl_base,omitempty"`
	DailyPnLBase      *float64 `json:"daily_pnl_base,omitempty"`
	BaseCurrency      string   `json:"base_currency,omitempty"`
}

// AccountResult is the wire shape of MethodAccountSummary.
//
// CurrencyExposure decomposes the portfolio by contract currency: one
// row per non-base currency the gateway reported via $LEDGER:ALL. Rows
// reconcile within ~0.5%: NetLiquidationCcy × ExchangeRate ≈ contribution
// to base NetLiquidation. Empty array on a same-currency account.
//
// UnrealizedPnL / RealizedPnL are the gateway-reported base-currency
// session totals. Cushion is ExcessLiquidity / NetLiquidation as
// reported by the gateway (not derived locally) — a ratio, unitless.
// AccountType is one of IBKR's account-type strings ("INDIVIDUAL",
// "IB-MARGIN", "REG-T-MARGIN", "PORTFOLIO", "CASH", …); empty when the
// gateway didn't deliver it (older server versions or non-margin accounts).
// LookAhead* fields project the post-overnight-margin-cycle state — useful
// to spot "fine now, blown by tonight" cases on portfolio-margin books.
// All scalar fields are zero (not nil) on absence — the renderer treats
// zero as "show em-dash" for non-money fields like Cushion to avoid
// fabricating signal.
//
// DailyPnL / PnLUnrealizedTotal / PnLRealizedTotal are populated from
// the gateway's reqPnL stream (TWS msg 94). DailyPnL is start-of-
// trading-day to now — the figure TWS shows in the portfolio header.
// PnLUnrealizedTotal / PnLRealizedTotal come from the same msg 94 frame
// (fields 3 & 4) but are the account's TOTAL unrealized / realized P&L
// (inception to now), NOT a decomposition of DailyPnL — they do not sum
// to it. They measure the same quantity as the session-running
// UnrealizedPnL / RealizedPnL above but arrive on a different feed
// (reqPnL vs account-updates), so the two can legitimately differ.
// All three are *float64 — nil means "no data yet" (pre-handshake,
// before the first stream frame), "no entitlement" (the gateway doesn't
// emit PnL for unentitled accounts), or "DBL_MAX sentinel" (gateway
// hasn't computed the slice). Never zero-substituted. PnLUnrealizedTotal
// / PnLRealizedTotal stay nil on older server versions that emit only
// the bare dailyPnL field. DailyPnLObservation carries the redacted source
// state so a regular-session failure cannot become healthy merely because
// the market closed.
type AccountResult struct {
	AccountID            string               `json:"account_id"`
	AccountType          string               `json:"account_type,omitempty"`
	BaseCurrency         string               `json:"base_currency"`
	NetLiquidation       float64              `json:"net_liquidation"`
	BuyingPower          float64              `json:"buying_power"`
	AvailableFunds       float64              `json:"available_funds"`
	ExcessLiquidity      float64              `json:"excess_liquidity"`
	TotalCash            float64              `json:"total_cash"`
	MaintenanceMargin    float64              `json:"maintenance_margin"`
	InitialMargin        float64              `json:"initial_margin"`
	GrossPositionValue   float64              `json:"gross_position_value"`
	UnrealizedPnL        float64              `json:"unrealized_pnl"`
	RealizedPnL          float64              `json:"realized_pnl"`
	Cushion              float64              `json:"cushion"`
	LookAheadInitMargin  float64              `json:"look_ahead_init_margin"`
	LookAheadMaintMargin float64              `json:"look_ahead_maint_margin"`
	LookAheadAvailable   float64              `json:"look_ahead_available_funds"`
	LookAheadExcess      float64              `json:"look_ahead_excess_liquidity"`
	DailyPnL             *float64             `json:"daily_pnl,omitempty"`
	DailyPnLObservation  *DailyPnLObservation `json:"daily_pnl_observation,omitempty"`
	PnLUnrealizedTotal   *float64             `json:"pnl_unrealized_total,omitempty"`
	PnLRealizedTotal     *float64             `json:"pnl_realized_total,omitempty"`
	CurrencyExposure     []CurrencyExposure   `json:"currency_exposure,omitempty"`
	// DataType is reserved for account-feed state; the account-summary
	// path is gateway-direct with no live/delayed dimension and the field
	// is currently left empty (omitted). Kept for shape parity with the
	// market-data surfaces.
	DataType string    `json:"data_type,omitempty"`
	AsOf     time.Time `json:"as_of"`
}

// DailyPnLObservation is the value-free health record for the account Daily
// P&L feed. SessionKey is the official US-equity session the state belongs to.
type DailyPnLObservation struct {
	Status     DailyPnLObservationStatus `json:"status"`
	SessionKey string                    `json:"session_key,omitempty"`
	AsOf       time.Time                 `json:"as_of"`
}

// DailyPnLObservationStatus is the closed set of Daily P&L feed states.
type DailyPnLObservationStatus string

// Daily P&L observation statuses exposed across daemon surfaces.
const (
	DailyPnLObservationOK      DailyPnLObservationStatus = "ok"
	DailyPnLObservationMissing DailyPnLObservationStatus = "missing"
	DailyPnLObservationInvalid DailyPnLObservationStatus = "invalid"
	DailyPnLObservationStale   DailyPnLObservationStatus = "stale"
	DailyPnLObservationNotDue  DailyPnLObservationStatus = "not_due"
)

// CurrencyExposure is one row in AccountResult.CurrencyExposure.
// Values are reported in the named currency (the "Ccy" suffix); the
// ExchangeRate field is BASE per CCY (i.e. "how many base-currency
// units 1 unit of the named currency converts to" — matches IBKR's
// $LEDGER semantics so reconciliation works without inversion).
// Fields are populated only when the gateway delivered them; absent
// fields are 0, never fabricated.
type CurrencyExposure struct {
	Currency             string  `json:"currency"`
	NetLiquidationCcy    float64 `json:"net_liquidation_ccy"`
	CashCcy              float64 `json:"cash_ccy"`
	StockMarketValueCcy  float64 `json:"stock_market_value_ccy"`
	OptionMarketValueCcy float64 `json:"option_market_value_ccy"`
	UnrealizedPnLCcy     float64 `json:"unrealized_pnl_ccy"`
	RealizedPnLCcy       float64 `json:"realized_pnl_ccy"`
	ExchangeRate         float64 `json:"exchange_rate"`
	NetLiquidationBase   float64 `json:"net_liquidation_base"`
}

// ChainStrike is one strike row in a chain.
type ChainStrike struct {
	Strike float64 `json:"strike"`
	IsATM  bool    `json:"is_atm,omitempty"`

	CallBid        *float64  `json:"call_bid"`
	CallAsk        *float64  `json:"call_ask"`
	CallLast       *float64  `json:"call_last"`
	CallPrevClose  *float64  `json:"call_prev_close,omitempty"`
	CallIV         *float64  `json:"call_iv"`
	CallDelta      *float64  `json:"call_delta"`
	CallOI         *int64    `json:"call_oi"`
	CallAsOf       time.Time `json:"call_as_of,omitzero"`
	CallDataStatus string    `json:"call_data_status,omitempty"`
	CallIVStatus   string    `json:"call_iv_status,omitempty"`
	CallOIStatus   string    `json:"call_oi_status,omitempty"`

	PutBid        *float64  `json:"put_bid"`
	PutAsk        *float64  `json:"put_ask"`
	PutLast       *float64  `json:"put_last"`
	PutPrevClose  *float64  `json:"put_prev_close,omitempty"`
	PutIV         *float64  `json:"put_iv"`
	PutDelta      *float64  `json:"put_delta"`
	PutOI         *int64    `json:"put_oi"`
	PutAsOf       time.Time `json:"put_as_of,omitzero"`
	PutDataStatus string    `json:"put_data_status,omitempty"`
	PutIVStatus   string    `json:"put_iv_status,omitempty"`
	PutOIStatus   string    `json:"put_oi_status,omitempty"`
}

// ChainExpiriesParams is the input for MethodChainExpiries.
//
// WithIV asks the daemon to fetch ATM implied volatility per expiry. The
// daemon caches results, picks the ATM strike per expiry, and runs the
// per-expiry subscribes through a bounded worker pool — first call costs
// a few seconds for a typical name, subsequent calls within the cache
// TTL are instant.
//
// AllExpiries lifts the default cap (the 12 nearest expiries). Off by
// default because the back-half LEAPS are rarely consulted and pay the
// IV-fetch cost for no decision value.
//
// MinDTE/MaxDTE/TargetDTE narrow expiry-list mode before any IV fan-out.
// TargetDTE returns the single listed expiry closest to the requested day
// count, after MinDTE/MaxDTE filtering when those are also set. This lets
// agents ask for "roughly 120 DTE" without fetching IV for every weekly.
//
// Empty Symbol → bad_request.
type ChainExpiriesParams struct {
	Symbol        string `json:"symbol"`
	WithIV        bool   `json:"with_iv,omitempty"`
	AllExpiries   bool   `json:"all_expiries,omitempty"`
	RequireLiveIV bool   `json:"require_live_iv,omitempty"`
	MinDTE        int    `json:"min_dte,omitempty"`
	MaxDTE        int    `json:"max_dte,omitempty"`
	TargetDTE     int    `json:"target_dte,omitempty"`
}

// ChainExpiry is one row in MethodChainExpiries' response. IV is nil when
// --with-iv wasn't requested or when the per-strike IV fetch timed out;
// IVStatus disambiguates ("ok" | "unavailable" | "timeout"). IVSource names
// how the value was obtained ("live_model" | "cached" | "unavailable");
// IVQuality tells consumers whether to trust it for term-structure decisions
// ("live_model" | "cached" | "reused_fallback" | "unavailable").
//
// DTE is the integer day count from "today (local)" to the expiry date,
// inclusive of the expiry day (so a same-day expiry has DTE=0, next-day
// has DTE=1). Surfaced separately from ImpliedMove so consumers can
// derive their own term-structure math.
//
// ImpliedMove is the 1-σ expected dollar move by expiration, computed as
// spot × IV × √(DTE/365). Populated only when IV and spot are both
// available; otherwise nil. The matching ImpliedMovePct is the same value
// expressed as a fraction of spot (so `0.042` means 4.2%).
type ChainExpiry struct {
	Date           string    `json:"date"` // YYYY-MM-DD
	DTE            int       `json:"dte,omitempty"`
	IV             *float64  `json:"iv,omitempty"`
	IVStatus       string    `json:"iv_status,omitempty"`
	IVSource       string    `json:"iv_source,omitempty"`
	IVQuality      string    `json:"iv_quality,omitempty"`
	IVAsOf         time.Time `json:"iv_as_of,omitzero"`
	ImpliedMove    *float64  `json:"implied_move,omitempty"`
	ImpliedMovePct *float64  `json:"implied_move_pct,omitempty"`
}

// ChainExpiriesResult is MethodChainExpiries' payload. Expiries are sorted
// ascending and deduped across exchanges by the daemon.
//
// Spot is the underlying mid the daemon used to pick the per-expiry ATM
// strike and to compute ImpliedMove. Zero when the spot probe failed or
// WithIV wasn't requested. SpotSource names the selected-price source
// ("last", "mid", "prev_close", "historical_close", ...); SpotAsOf is the
// best timestamp known for that selected price.
type ChainExpiriesResult struct {
	Symbol         string        `json:"symbol"`
	Spot           float64       `json:"spot,omitempty"`
	SpotSource     string        `json:"spot_source,omitempty"`
	SpotAsOf       time.Time     `json:"spot_as_of,omitzero"`
	Expiries       []ChainExpiry `json:"expiries"`
	WarningDetails []DataWarning `json:"warning_details,omitempty"`
	AsOf           time.Time     `json:"as_of"`
}

// ChainLegSummary is a compact executable-leg descriptor used by chain
// liquidity summaries. It duplicates a subset of ChainStrike side fields so a
// consumer can answer "which leg should I inspect first?" without walking the
// whole grid.
type ChainLegSummary struct {
	Right     string   `json:"right"` // C | P
	Strike    float64  `json:"strike"`
	Bid       float64  `json:"bid,omitempty"`
	Ask       float64  `json:"ask,omitempty"`
	Mid       float64  `json:"mid,omitempty"`
	Spread    float64  `json:"spread,omitempty"`
	SpreadPct float64  `json:"spread_pct,omitempty"`
	OI        *int64   `json:"oi,omitempty"`
	Delta     *float64 `json:"delta,omitempty"`
}

// ChainTradableSummary is the top-level option-chain census a trader needs
// before reading strike rows. LiveBidAskLegs is the executable count; stale,
// model-only, subscribe-error, and no-quote counts explain why the rest are
// not immediately tradable.
type ChainTradableSummary struct {
	TotalLegs          int     `json:"total_legs"`
	LiveBidAskLegs     int     `json:"live_bid_ask_legs"`
	OneSidedLiveLegs   int     `json:"one_sided_live_legs"`
	StaleLegs          int     `json:"stale_legs"`
	ModelOnlyLegs      int     `json:"model_only_legs"`
	SubscribeErrorLegs int     `json:"subscribe_error_legs"`
	NoQuoteLegs        int     `json:"no_quote_legs"`
	OICoveredLegs      int     `json:"oi_covered_legs"`
	OICoveragePct      float64 `json:"oi_coverage_pct"`
	OptionsTradable    bool    `json:"options_tradable"`
	FeedGap            string  `json:"feed_gap,omitempty"` // stale_close_only | thin_contract | unknown_feed_gap
}

// ChainLiquiditySummary surfaces the decision-grade option-liquidity facts
// before the raw grid. Spread percentages are fractions (0.10 = 10%).
type ChainLiquiditySummary struct {
	LiquidityGrade           string           `json:"liquidity_grade"` // good | fair | poor | untradable
	ATMSpreadPct             *float64         `json:"atm_spread_pct,omitempty"`
	NearestLiveCall          *ChainLegSummary `json:"nearest_live_call,omitempty"`
	NearestLivePut           *ChainLegSummary `json:"nearest_live_put,omitempty"`
	MinSpreadLiveStrike      *ChainLegSummary `json:"min_spread_live_strike,omitempty"`
	OICoveragePct            float64          `json:"oi_coverage_pct"`
	RecommendedStructureHint string           `json:"recommended_structure_hint"` // stock_only | shares_or_spreads | calls_ok | untradable_chain
}

// ChainResult is MethodChainFetch's payload. SpotSource names the selected
// underlying price source used to anchor ATM ("last", "mid", "prev_close",
// "historical_close", ...); SpotAsOf is the best timestamp known for that
// selected price.
type ChainResult struct {
	Symbol           string                 `json:"symbol"`
	TradingClass     string                 `json:"trading_class,omitempty"`
	Spot             float64                `json:"spot"`
	SpotSource       string                 `json:"spot_source,omitempty"`
	SpotAsOf         time.Time              `json:"spot_as_of,omitzero"`
	Expiry           string                 `json:"expiry"`
	DTE              int                    `json:"dte"`
	DataType         string                 `json:"data_type"`
	FeedType         string                 `json:"feed_type,omitempty"`
	SessionState     string                 `json:"session_state,omitempty"`
	TradableSummary  *ChainTradableSummary  `json:"tradable_summary,omitempty"`
	LiquiditySummary *ChainLiquiditySummary `json:"liquidity_summary,omitempty"`
	Strikes          []ChainStrike          `json:"strikes"`
	WarningDetails   []DataWarning          `json:"warning_details,omitempty"`
	AsOf             time.Time              `json:"as_of"`
}

// ScanRow is one row of a scanner result. The IBKR scanner subscription
// only returns rank+symbol+three-mostly-empty-comment-fields per row, so
// every numeric field below is populated by the daemon via a follow-up
// snapshot subscribe on the symbol. Pointers (not scalars) so consumers
// can distinguish "the gateway didn't deliver this tick within the
// enrichment window" from "the value is genuinely zero" — the no-fabrication
// invariant. Comment carries the raw scanner-side text when non-empty
// (rare; most scan types leave it blank).
//
// Currency is the ISO-4217 code for Last / PrevClose / Change / Week52*
// — needed so non-US ad-hoc scans (e.g. --exchange STK.EU.IBIS) render
// with the right symbol instead of a hardcoded $. Empty string means
// "the daemon couldn't resolve a currency for this row"; renderers
// should fall back to $ in that case for back-compat with old daemons.
//
// Unit conventions follow Quote: ChangePct is in PERCENT units (5.41
// means 5.41 %), IV is a DECIMAL FRACTION (0.342 means 34.2 %).
type ScanRow struct {
	Rank               int           `json:"rank"`
	Symbol             string        `json:"symbol"`
	SecType            string        `json:"sec_type,omitempty"`
	Exchange           string        `json:"exchange,omitempty"`
	Currency           string        `json:"currency,omitempty"`
	LocalSymbol        string        `json:"local_symbol,omitempty"`
	TradingClass       string        `json:"trading_class,omitempty"`
	InstrumentTags     []string      `json:"instrument_tags,omitempty"`
	Last               *float64      `json:"last,omitempty"`
	PrevClose          *float64      `json:"prev_close,omitempty"`
	Change             *float64      `json:"change,omitempty"`
	ChangePct          *float64      `json:"change_pct,omitempty"`
	Volume             *int64        `json:"volume,omitempty"`
	AvgVolume20D       *int64        `json:"avg_volume_20d,omitempty"`
	AvgDollarVolume20D *float64      `json:"avg_dollar_volume_20d,omitempty"`
	IV                 *float64      `json:"iv,omitempty"`
	Week52High         *float64      `json:"week_52_high,omitempty"`
	Week52Low          *float64      `json:"week_52_low,omitempty"`
	DataType           string        `json:"data_type,omitempty"`
	FeedType           string        `json:"feed_type,omitempty"`
	PriceAt            time.Time     `json:"price_at,omitzero"`
	PriceAsOf          string        `json:"price_as_of,omitempty"`
	AsOf               time.Time     `json:"as_of,omitzero"`
	VolumePhase        string        `json:"volume_phase,omitempty"`
	WarningDetails     []DataWarning `json:"warning_details,omitempty"`
	Comment            string        `json:"comment,omitempty"`
}

// ScanResult wraps the rows.
type ScanResult struct {
	Preset string    `json:"preset"`
	Type   string    `json:"type"`
	Rows   []ScanRow `json:"rows"`
	AsOf   time.Time `json:"as_of"`
}

// ScanListResult enumerates configured presets.
type ScanListResult struct {
	Presets []ScanPresetSummary `json:"presets"`
}

// ScanPresetSummary describes a single preset entry in scan list.
type ScanPresetSummary struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Exchange   string `json:"exchange"`
	Instrument string `json:"instrument,omitempty"`
	Limit      int    `json:"limit"`
}

// BackgroundTaskStatus names a daemon-internal long-running task that
// is currently executing or waiting for a scheduled retry. Used by
// `canary status` to surface activity that would otherwise be invisible
// — a fresh autospawned daemon mid-bootstrap looks identical to an
// idle one from outside. The surface deliberately carries no required
// state enum: presence in the HealthResult.BackgroundTasks list IS
// the state ("this task is still active"). Tasks that are
// idle/ready/cold are omitted entirely, keeping the wire payload
// bounded and the user-facing rendering compact.
//
// Current task names:
//   - "breadth-spx" — the SPX 50-DMA breadth engine is running a
//     refresh or waiting for a below-threshold bootstrap retry.
//   - "gamma-zero" — the SPX zero-gamma compute is fanning out
//     across option legs.
//
// Renderers should treat unknown names as informational rather than
// errors; new background tasks added in future versions are
// forward-compatible by design.
type BackgroundTaskStatus struct {
	// Name is a stable token identifying the task. Stable across
	// daemon versions; one of the documented values above.
	Name       string    `json:"name"`
	Status     string    `json:"status,omitempty"`
	Scope      string    `json:"scope,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	EtaSeconds int       `json:"eta_seconds,omitempty"`
	Progress   int       `json:"progress,omitempty"`
}

// DataWarning is the common structured warning shape used by price-level
// tools. It deliberately matches RegimeWarning / GammaWarningDetail's JSON
// contract so agents can handle freshness and provenance uniformly.
type DataWarning struct {
	Code     string `json:"code"`
	Scope    string `json:"scope,omitempty"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
	Impact   string `json:"impact,omitempty"`
	Action   string `json:"action,omitempty"`
}

// SubsystemHealth is a compact status.health diagnostic for tool families
// that can degrade independently while the gateway remains connected.
type SubsystemHealth struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Message     string    `json:"message,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	StartedAt   time.Time `json:"started_at,omitzero"`
	EtaSeconds  int       `json:"eta_seconds,omitempty"`
	Progress    int       `json:"progress,omitempty"`
}

// DataQualityHealth is a compact status.health diagnostic for decision
// surfaces that can serve a snapshot while carrying degraded or stale inputs.
// It is intentionally higher-level than regime.warning_details or
// gamma.warning_details: status answers whether downstream reads need extra
// caution, not every low-level row that caused it.
type DataQualityHealth struct {
	Surface          string    `json:"surface"`
	Status           string    `json:"status"`
	CadenceState     string    `json:"cadence_state,omitempty"`
	Summary          string    `json:"summary,omitempty"`
	StaleClusters    []string  `json:"stale_clusters,omitempty"`
	PartialClusters  []string  `json:"partial_clusters,omitempty"`
	DegradedClusters []string  `json:"degraded_clusters,omitempty"`
	AsOf             time.Time `json:"as_of,omitzero"`
}

// Data cadence values summarize whether a decision surface has current,
// expected-but-not-due, missed, absent, or unclassified evidence.
const (
	DataCadenceCurrent       = "current"
	DataCadenceNotDue        = "not_due"
	DataCadenceMissedSession = "missed_session"
	DataCadenceNoLastGood    = "no_last_good"
	DataCadenceUnknown       = "unknown"
)

// DataFarmHealth is emitted on status.health only for data farms that
// currently need operator attention. Healthy farms are intentionally omitted
// to keep the normal status surface quiet.
type DataFarmHealth struct {
	Name    string    `json:"name"`
	Type    string    `json:"type,omitempty"`
	Status  string    `json:"status"`
	Code    int       `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
	AsOf    time.Time `json:"as_of,omitzero"`
}

// Account modes classify the connected broker account; they do not describe
// market-data freshness or grant broker-write authority.
const (
	AccountModeUnknown = "unknown"
	AccountModePaper   = "paper"
	AccountModeLive    = "live"
)

// HealthResult is the response to MethodStatusHealth.
//
// PortOrigin / TLSOrigin record how the daemon arrived at the values
// shown — "pinned" (user wrote them in config), "discovered" (probe found
// the gateway), or "default" (built-in fallback). Alternates lists other
// ports that responded during discovery but lost the first-hit race.
// Empty alternates is the common case (single gateway up).
type HealthResult struct {
	DaemonVersion string    `json:"daemon_version"`
	DaemonStarted time.Time `json:"daemon_started"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Account       string    `json:"account,omitempty"`
	// ConnectedAccount is the one account the connected TWS/Gateway session is
	// scoped to, never a list. It is the code the session advertises via
	// managedAccounts / accountSummary when that is a single account, and the
	// configured [gateway].account pin when the login holds several and the
	// advertised value is the comma-joined inventory. It is empty when neither
	// is one concrete account — an unpinned multi-account login has no account
	// to name. It differs from Account when [gateway].account is empty and the
	// daemon auto-detected the account after handshake. The session's full
	// inventory is not served here.
	ConnectedAccount string `json:"connected_account,omitempty"`
	// AccountMode is the daemon's best classification of the connected
	// endpoint/account: "paper", "live", or "unknown".
	AccountMode   string `json:"account_mode,omitempty"`
	GatewayHost   string `json:"gateway_host"`
	GatewayPort   int    `json:"gateway_port"`
	GatewayTLS    bool   `json:"gateway_tls"`
	NegotiatedTLS bool   `json:"negotiated_tls"`
	PortOrigin    string `json:"port_origin"`
	TLSOrigin     string `json:"tls_origin"`
	Alternates    []int  `json:"alternates,omitempty"`
	ClientID      int    `json:"client_id"`
	Connected     bool   `json:"connected"`
	DataType      string `json:"data_type,omitempty"`
	ServerVersion int    `json:"server_version,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// BackgroundTasks lists daemon-internal long-running computes that
	// are running or waiting for a scheduled retry. Empty when nothing
	// is active. Always present on the wire (never omitted) so
	// consumers can rely on `len(result.background_tasks) == 0` to
	// mean "idle" without inferring from absence.
	BackgroundTasks []BackgroundTaskStatus `json:"background_tasks"`
	Subsystems      []SubsystemHealth      `json:"subsystems,omitempty"`
	DataQuality     []DataQualityHealth    `json:"data_quality,omitempty"`
	DataFarms       []DataFarmHealth       `json:"data_farms,omitempty"`
	// Members carries the runtime SPX-membership state: source
	// (cache vs embedded), count, as-of timestamp, refresh health.
	// Populated unconditionally — even when the daemon falls back
	// to the embedded list, the user needs to see WHICH list it's
	// using so silent parser rot / disabled-refresh shows up in
	// `canary status`. Zero-value Source means the daemon doesn't
	// know yet (engine construction failed); the CLI hides the row
	// in that case.
	Members MembersHealth `json:"members"`
	Trading TradingStatus `json:"trading"`
}

// Trading status values describe MCP exposure and live-override readiness;
// readiness is local evidence, not broker permission or submit authority.
const (
	TradingMCPDisabled = "disabled"

	TradingLiveOverrideBlocked = "blocked"
	TradingLiveOverrideReady   = "ready"
)

// TradingBlocker explains one local reason an order write cannot proceed.
type TradingBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Action  string `json:"action,omitempty"`
}

// TradingStatus is the local order-entry readiness surface. It is deliberately
// separate from broker permission: TWS / IB Gateway can still reject writes
// after all local gates pass.
type TradingStatus struct {
	Mode               string           `json:"mode"`
	Endpoint           string           `json:"endpoint,omitempty"`
	GatewayHost        string           `json:"gateway_host,omitempty"`
	GatewayPort        int              `json:"gateway_port,omitempty"`
	PortOrigin         string           `json:"port_origin,omitempty"`
	Account            string           `json:"account,omitempty"`
	AccountOrigin      string           `json:"account_origin,omitempty"`
	ClientID           int              `json:"client_id,omitempty"`
	ClientIDOrigin     string           `json:"client_id_origin,omitempty"`
	MCPTrading         string           `json:"mcp_trading"`
	CanPreview         bool             `json:"can_preview"`
	CanWrite           bool             `json:"can_write"`
	WriteBlockers      []TradingBlocker `json:"write_blockers,omitempty"`
	OpenOrders         int              `json:"open_orders,omitempty"`
	LastOrderEvent     string           `json:"last_order_event,omitempty"`
	PaperSmoke         string           `json:"paper_smoke,omitempty"`
	PaperSmokeAt       *time.Time       `json:"paper_smoke_at,omitempty"`
	PaperSmokeMaxAge   string           `json:"paper_smoke_max_age,omitempty"`
	PaperSmokeAccount  string           `json:"paper_smoke_account,omitempty"`
	PaperSmokeEndpoint string           `json:"paper_smoke_endpoint,omitempty"`
	PaperSmokeClientID int              `json:"paper_smoke_client_id,omitempty"`
	PaperSmokeVersion  string           `json:"paper_smoke_version,omitempty"`
	LiveOverride       string           `json:"live_override,omitempty"`
	Blocked            bool             `json:"blocked"`
	Blockers           []TradingBlocker `json:"blockers,omitempty"`
}

// TradingPaperSmokeParams is the input for MethodTradingPaperSmoke: a
// daemon-observed paper order round-trip (place a 1-share far-off-market
// LMT, wait for broker acknowledgement, cancel, wait for the cancel to
// confirm) that produces the MAC'd evidence the live gate requires.
type TradingPaperSmokeParams struct {
	// TimeoutMs caps the wait for broker acknowledgement. 0 means the
	// daemon default (30 s); values are capped daemon-side (60 s) so the
	// whole round-trip stays under the per-method deadline.
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// Origin identifies who is asking (OrderOrigin*). Paper-smoke mints
	// the last live precondition, so non-human origins are refused even
	// though the order itself transmits on the paper route.
	Origin string `json:"origin,omitempty"`
}

// TradingPaperSmokeResult reports the round-trip outcome. Passed means the
// daemon observed both the broker acknowledgement and the cancel
// confirmation and saved signed evidence; any other outcome after a
// transmit attempt saves result=failed evidence, deliberately revoking
// prior valid evidence (fail closed).
type TradingPaperSmokeResult struct {
	Passed                bool          `json:"passed"`
	Result                string        `json:"result"`
	Mode                  string        `json:"mode"`
	Account               string        `json:"account,omitempty"`
	Endpoint              string        `json:"endpoint,omitempty"`
	ClientID              int           `json:"client_id,omitempty"`
	Version               string        `json:"version,omitempty"`
	Symbol                string        `json:"symbol,omitempty"`
	OrderRef              string        `json:"order_ref,omitempty"`
	ReservedOrderID       int           `json:"reserved_order_id,omitempty"`
	LimitPrice            float64       `json:"limit_price,omitempty"`
	Quantity              int           `json:"quantity,omitempty"`
	AckLifecycleStatus    string        `json:"ack_lifecycle_status,omitempty"`
	CancelLifecycleStatus string        `json:"cancel_lifecycle_status,omitempty"`
	EvidenceSaved         bool          `json:"evidence_saved"`
	EvidenceAt            *time.Time    `json:"evidence_at,omitempty"`
	EvidenceMaxAge        string        `json:"evidence_max_age,omitempty"`
	Message               string        `json:"message,omitempty"`
	Warnings              []DataWarning `json:"warnings,omitempty"`
	AsOf                  time.Time     `json:"as_of"`
}

// Order constants are the allowlisted action, order-type, time-in-force,
// strategy, and trailing-offset vocabulary accepted by daemon order requests.
const (
	OrderActionBuy  = "BUY"
	OrderActionSell = "SELL"

	OrderTypeLMT        = "LMT"
	OrderTypeTRAIL      = "TRAIL"
	OrderTypeTRAILLIMIT = "TRAIL LIMIT"

	OrderTIFDay = "DAY"
	// OrderTIFGTC persists until filled or cancelled. Accepted for broker
	// trail orders only: a protective stop that dies at the session close
	// is absent exactly when the overnight gap it covers opens up.
	OrderTIFGTC = "GTC"

	OrderStrategyPatientLimit  = "patient-limit"
	OrderStrategyExplicitLimit = "explicit-limit"
	OrderStrategyBrokerTrail   = "broker-trail"

	OrderTrailBasisInstrumentPrice = "instrument_price"
	OrderTrailOffsetPercent        = "percent"
	OrderTrailOffsetAmount         = "amount"

	// OrderTriggerMethodDefault is an allowlisted broker trigger method.
	OrderTriggerMethodDefault = 0
	// OrderTriggerMethodDoubleBidAsk is an allowlisted broker trigger method.
	OrderTriggerMethodDoubleBidAsk = 1
	// OrderTriggerMethodLast is an allowlisted broker trigger method.
	OrderTriggerMethodLast = 2
	// OrderTriggerMethodDoubleLast is an allowlisted broker trigger method.
	OrderTriggerMethodDoubleLast = 3
	// OrderTriggerMethodBidAsk is an allowlisted broker trigger method.
	OrderTriggerMethodBidAsk = 4
	// OrderTriggerMethodLastOrBidAsk is an allowlisted broker trigger method.
	OrderTriggerMethodLastOrBidAsk = 7
	// OrderTriggerMethodMidpoint is an allowlisted broker trigger method.
	OrderTriggerMethodMidpoint = 8

	// OrderOriginAgent identifies an audited request origin; it does not grant authority by itself.
	// Request origins for broker writes. Adapters stamp every write request;
	// the daemon journals origin for audit and may apply origin-specific
	// policy. A missing or unknown origin is treated as OrderOriginAgent, so
	// new adapters must opt in to a human origin.
	OrderOriginAgent = "agent"
	// OrderOriginHumanTTY identifies an audited request origin; it does not grant authority by itself.
	OrderOriginHumanTTY = "human-tty"
	// OrderOriginPairedDevice identifies an audited request origin; it does not grant authority by itself.
	OrderOriginPairedDevice = "human-paired-device"

	OrderPositionEffectOpen      = "open"
	OrderPositionEffectIncrease  = "increase"
	OrderPositionEffectReduce    = "reduce"
	OrderPositionEffectClose     = "close"
	OrderPositionEffectFlip      = "flip"
	OrderPositionEffectOpenShort = "open_short"

	// OrderWhatIfStatusUnavailable classifies broker WhatIf evidence.
	OrderWhatIfStatusUnavailable = "unavailable"
	// OrderWhatIfStatusAccepted classifies broker WhatIf evidence.
	OrderWhatIfStatusAccepted = "accepted"
	// OrderWhatIfStatusRejected classifies broker WhatIf evidence.
	OrderWhatIfStatusRejected = "rejected"

	// OrderTokenScopePlace binds a preview token to one order operation.
	OrderTokenScopePlace = "place"
	// OrderTokenScopeModify binds a preview token to one order operation.
	OrderTokenScopeModify = "modify"

	// OrderLifecyclePreviewed is a durable order-lifecycle classification.
	OrderLifecyclePreviewed = "previewed"
	// OrderLifecyclePendingSubmit is a durable order-lifecycle classification.
	OrderLifecyclePendingSubmit = "pending_submit"
	// OrderLifecyclePreSubmitted is a durable order-lifecycle classification.
	OrderLifecyclePreSubmitted = "pre_submitted"
	// OrderLifecycleSubmitted is a durable order-lifecycle classification.
	OrderLifecycleSubmitted = "submitted"
	// OrderLifecyclePartiallyFilled is a durable order-lifecycle classification.
	OrderLifecyclePartiallyFilled = "partially_filled"
	// OrderLifecycleFilled is a durable order-lifecycle classification.
	OrderLifecycleFilled = "filled"
	// OrderLifecyclePendingCancel is a durable order-lifecycle classification.
	OrderLifecyclePendingCancel = "pending_cancel"
	// OrderLifecycleCancelled is a durable order-lifecycle classification.
	OrderLifecycleCancelled = "cancelled"
	// OrderLifecycleRejected is a durable order-lifecycle classification.
	OrderLifecycleRejected = "rejected"
	// OrderLifecycleInactive is a durable order-lifecycle classification.
	OrderLifecycleInactive = "inactive"
	// OrderLifecycleUnknownReconcileRequired is a durable order-lifecycle classification.
	OrderLifecycleUnknownReconcileRequired = "unknown_reconcile_required"
	// OrderLifecycleExpiredInferred marks a DAY order whose effective session
	// closed without a terminal broker callback. It is local calendar
	// inference — never broker-confirmed — and such rows stay cancel- and
	// modify-ineligible. (GTC rows instead heal via the open-order snapshot
	// reconcile, see OrderLifecycleClosedReconciled.)
	OrderLifecycleExpiredInferred = "expired_inferred"
	// OrderLifecycleClosedReconciled marks a journal row that a complete
	// broker open-order snapshot no longer reported: the terminal callback
	// (fill, cancel, broker-side expiry) was missed while the daemon was not
	// listening. The final broker state is unknown — cancelled or filled
	// outside the daemon's view — so broker statements stay authoritative;
	// this status only closes the local row.
	OrderLifecycleClosedReconciled = "closed_reconciled"

	OrderReconciliationKindShortEntryFull   = "short_entry_full"
	OrderReconciliationKindShortEntryExcess = "short_entry_excess"
	OrderReconciliationSeverityCritical     = "critical"
)

// OrdersOpenParams reads the current broker account/mode open-order view.
type OrdersOpenParams struct{}

// OrdersHistoryParams reads bounded local order-journal history for the
// current broker account/mode. Since and Until accept RFC3339 timestamps or
// YYYY-MM-DD UTC dates; Limit caps returned grouped order rows, while
// EventLimit caps returned lifecycle events per grouped order row.
type OrdersHistoryParams struct {
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	EventLimit int    `json:"event_limit,omitempty"`
}

// OrderStatusParams identifies one journal-backed order view by order ref,
// IBKR order ID, or permanent ID.
type OrderStatusParams struct {
	ID string `json:"id"`
}

// OrderPreviewParams asks the daemon to validate and price an order draft,
// then mint a short-lived preview token. The preview path never places the
// order; place/modify/cancel remain separate gated RPCs.
type OrderPreviewParams struct {
	Action     string          `json:"action"` // BUY | SELL, case-insensitive
	Contract   ContractParams  `json:"contract"`
	Quantity   int             `json:"quantity"`
	OrderType  string          `json:"order_type,omitempty"` // LMT | TRAIL | TRAIL LIMIT
	LimitPrice *float64        `json:"limit_price,omitempty"`
	Trail      *OrderTrailSpec `json:"trail,omitempty"`
	// TriggerMethod is the IBKR stop/trigger method integer. Zero delegates
	// to IBKR defaults; protective stock/ETF trails default to LAST (2).
	TriggerMethod int    `json:"trigger_method,omitempty"`
	Strategy      string `json:"strategy,omitempty"` // default patient-limit for stocks/ETFs
	TIF           string `json:"tif,omitempty"`      // default DAY
	OutsideRTH    bool   `json:"outside_rth,omitempty"`
	ReplaceID     string `json:"replace_id,omitempty"`
	TimeoutMs     int    `json:"timeout_ms,omitempty"`
	Source        string `json:"source,omitempty"`
}

// OrderDraft is the canonical local intent bound into a preview token.
type OrderDraft struct {
	Action        string          `json:"action"`
	Contract      ContractParams  `json:"contract"`
	Quantity      int             `json:"quantity"`
	OrderType     string          `json:"order_type"`
	LimitPrice    float64         `json:"limit_price"`
	Trail         *OrderTrailSpec `json:"trail,omitempty"`
	TriggerMethod int             `json:"trigger_method,omitempty"`
	TIF           string          `json:"tif"`
	OutsideRTH    bool            `json:"outside_rth"`
	Strategy      string          `json:"strategy"`
	OrderRef      string          `json:"order_ref"`
	OpenClose     string          `json:"open_close,omitempty"`
	Source        string          `json:"source,omitempty"`
}

// OrderTrailSpec is the canonical broker-side trailing-stop intent. Percent
// values use IBKR API semantics: 2 means 2%, and 0.50 means 0.50%.
type OrderTrailSpec struct {
	Basis            string   `json:"basis,omitempty"`
	OffsetType       string   `json:"offset_type"`
	TrailingPercent  *float64 `json:"trailing_percent,omitempty"`
	TrailingAmount   *float64 `json:"trailing_amount,omitempty"`
	InitialStopPrice float64  `json:"initial_stop_price"`
	LimitOffset      *float64 `json:"limit_offset,omitempty"`
}

// OrderQuoteSnapshot captures the market-data inputs used by preview pricing.
type OrderQuoteSnapshot struct {
	Symbol         string         `json:"symbol"`
	Bid            *float64       `json:"bid,omitempty"`
	Ask            *float64       `json:"ask,omitempty"`
	Last           *float64       `json:"last,omitempty"`
	Mark           *float64       `json:"mark,omitempty"`
	Midpoint       *float64       `json:"midpoint,omitempty"`
	DataType       string         `json:"data_type,omitempty"`
	QuoteQuality   string         `json:"quote_quality,omitempty"`
	SpreadPct      *float64       `json:"spread_pct,omitempty"`
	PriceAt        time.Time      `json:"price_at,omitzero"`
	PriceAsOf      string         `json:"price_as_of,omitempty"`
	Stale          bool           `json:"stale,omitempty"`
	StaleReason    string         `json:"stale_reason,omitempty"`
	AsOf           time.Time      `json:"as_of,omitzero"`
	SessionContext *MarketSession `json:"session_context,omitempty"`
	Warnings       []DataWarning  `json:"warnings,omitempty"`
}

// OrderPositionImpact reports local position-effect math. Broker permissions
// and margin remain authoritative; this is a disclosure and local safety gate.
type OrderPositionImpact struct {
	Before float64 `json:"before"`
	After  float64 `json:"after"`
	Effect string  `json:"effect"`
}

// OrderMarginImpact is populated from IBKR WhatIf once the raw preview-only
// path is available in the trading build.
type OrderMarginImpact struct {
	Currency                string   `json:"currency,omitempty"`
	InitialMarginBefore     *float64 `json:"initial_margin_before,omitempty"`
	InitialMarginAfter      *float64 `json:"initial_margin_after,omitempty"`
	MaintenanceMarginBefore *float64 `json:"maintenance_margin_before,omitempty"`
	MaintenanceMarginAfter  *float64 `json:"maintenance_margin_after,omitempty"`
	EquityWithLoanBefore    *float64 `json:"equity_with_loan_before,omitempty"`
	EquityWithLoanAfter     *float64 `json:"equity_with_loan_after,omitempty"`
	Commission              *float64 `json:"commission,omitempty"`
	MinCommission           *float64 `json:"min_commission,omitempty"`
	MaxCommission           *float64 `json:"max_commission,omitempty"`
	CommissionCurrency      string   `json:"commission_currency,omitempty"`
	WarningText             string   `json:"warning_text,omitempty"`
	CompletedStatus         string   `json:"completed_status,omitempty"`
	CompletedTime           string   `json:"completed_time,omitempty"`
}

// OrderWhatIfResult is the broker preview surface. Status is "accepted" only
// after IBKR returns a successful WhatIf response for this exact draft.
type OrderWhatIfResult struct {
	Status             string             `json:"status"`
	RequiredForSubmit  bool               `json:"required_for_submit"`
	Available          bool               `json:"available"`
	Message            string             `json:"message,omitempty"`
	Action             string             `json:"action,omitempty"`
	AdvancedRejectJSON string             `json:"advanced_reject_json,omitempty"`
	Margin             *OrderMarginImpact `json:"margin,omitempty"`
}

// OrderPreviewResult is returned by order.preview. PreviewToken is a daemon-
// signed bearer token for a later place flow; this RPC itself does not submit
// anything to IBKR.
type OrderPreviewResult struct {
	PreviewToken          string    `json:"preview_token,omitempty"`
	PreviewTokenID        string    `json:"preview_token_id,omitempty"`
	PreviewTokenScope     string    `json:"preview_token_scope,omitempty"`
	PreviewTokenExpiresAt time.Time `json:"preview_token_expires_at,omitzero"`
	TokenMinted           bool      `json:"token_minted"`
	SubmitEligible        bool      `json:"submit_eligible"`
	// Executable is retained for older clients and is equivalent to
	// SubmitEligible. A minted preview token is not executable unless an
	// accepted broker WhatIf result is bound into the token.
	Executable bool                `json:"executable"`
	Mode       string              `json:"mode"`
	Account    string              `json:"account"`
	Endpoint   string              `json:"endpoint"`
	ClientID   int                 `json:"client_id"`
	Draft      OrderDraft          `json:"draft"`
	Quote      OrderQuoteSnapshot  `json:"quote"`
	Position   OrderPositionImpact `json:"position"`
	Notional   float64             `json:"notional"`
	// Notional is expressed in NotionalCurrency. NotionalBase is the same
	// amount converted through current typed FX evidence for comparison with
	// MaxNotional, which is always denominated in BaseCurrency.
	NotionalCurrency string            `json:"notional_currency,omitempty"`
	NotionalBase     float64           `json:"notional_base,omitempty"`
	BaseCurrency     string            `json:"base_currency,omitempty"`
	FXRate           float64           `json:"fx_rate,omitempty"` // BaseCurrency per NotionalCurrency.
	FXEvidenceAt     time.Time         `json:"fx_evidence_at,omitzero"`
	FXDataType       string            `json:"fx_data_type,omitempty"`
	FXSource         string            `json:"fx_source,omitempty"`
	MaxNotional      float64           `json:"max_notional,omitempty"`
	WhatIf           OrderWhatIfResult `json:"what_if"`
	Warnings         []DataWarning     `json:"warnings,omitempty"`
	AsOf             time.Time         `json:"as_of"`
}

// OrderPlaceParams redeems a submit-eligible preview token for a broker
// transmit. The daemon revalidates the local trading gate and token binding
// before any socket write.
type OrderPlaceParams struct {
	PreviewToken string `json:"preview_token"`
	TimeoutMs    int    `json:"timeout_ms,omitempty"`
	// Origin identifies who is asking (OrderOrigin*) for audit and any
	// origin-specific policy.
	Origin string `json:"origin,omitempty"`
}

// OrderModifyParams applies a constrained modify to a locally tracked open
// order. The preview token must describe the replacement draft; the daemon
// reuses the existing broker order ID instead of creating a new one.
type OrderModifyParams struct {
	ID           string `json:"id"`
	PreviewToken string `json:"preview_token"`
	TimeoutMs    int    `json:"timeout_ms,omitempty"`
	Origin       string `json:"origin,omitempty"`
}

// OrderCancelParams requests cancellation of a locally tracked order. Cancel
// is intentionally identified by local order_ref, IBKR order ID, or permanent
// ID so the daemon can bind the request to journal evidence first.
type OrderCancelParams struct {
	ID        string `json:"id"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	// Origin is journaled for audit. Cancel is exempt from the live
	// agent-origin block: refusing a cancel can never reduce risk less than
	// allowing it, though cancelling a protective stop does remove
	// protection — see SECURITY.md.
	Origin string `json:"origin,omitempty"`
}

// OrderPlaceResult reports the durable local and broker-send state of a gated
// placement attempt. Accepted does not imply a fill; LifecycleStatus and
// SendState describe subsequent authority.
type OrderPlaceResult struct {
	Accepted        bool       `json:"accepted"`
	Mode            string     `json:"mode"`
	Account         string     `json:"account"`
	Endpoint        string     `json:"endpoint"`
	ClientID        int        `json:"client_id"`
	OrderRef        string     `json:"order_ref"`
	PreviewTokenID  string     `json:"preview_token_id"`
	ReservedOrderID int        `json:"reserved_order_id"`
	Draft           OrderDraft `json:"draft"`
	Status          string     `json:"status,omitempty"`
	LifecycleStatus string     `json:"lifecycle_status,omitempty"`
	SendState       string     `json:"send_state,omitempty"`
	Message         string     `json:"message,omitempty"`
	AsOf            time.Time  `json:"as_of"`
}

// PurgeExecuteParams selects an explicit purge workflow. It is a broker-write
// request and remains subject to daemon preview, origin, freeze, and limit gates.
type PurgeExecuteParams struct {
	PurgeID       string            `json:"purge_id"`
	All           bool              `json:"all,omitempty"`
	Symbols       []string          `json:"symbols,omitempty"`
	Legs          []PurgeExecuteLeg `json:"legs,omitempty"`
	BypassPreview *bool             `json:"bypass_preview,omitempty"`
	WaitMs        int               `json:"wait_ms,omitempty"`
	Origin        string            `json:"origin,omitempty"`
}

// PurgeExecuteLeg identifies one exact position leg selected for purge.
type PurgeExecuteLeg struct {
	LegID        string         `json:"leg_id"`
	Symbol       string         `json:"symbol"`
	SecType      string         `json:"sec_type"`
	Contract     ContractParams `json:"contract"`
	OriginalSide string         `json:"original_side"`
	PurgeAction  string         `json:"purge_action"`
	Quantity     float64        `json:"quantity"`
	Multiplier   int            `json:"multiplier,omitempty"`
}

// PurgeExecuteResult reports per-leg submission, skips, blockers, and monitor
// guidance. Submitted legs are not necessarily filled.
type PurgeExecuteResult struct {
	Kind                 string                   `json:"kind"`
	PurgeID              string                   `json:"purge_id"`
	Status               string                   `json:"status"`
	Mode                 string                   `json:"mode,omitempty"`
	Account              string                   `json:"account,omitempty"`
	Endpoint             string                   `json:"endpoint,omitempty"`
	ClientID             int                      `json:"client_id,omitempty"`
	BypassPreview        bool                     `json:"bypass_preview"`
	SelectedLegs         int                      `json:"selected_legs"`
	SubmittedLegs        int                      `json:"submitted_legs"`
	SkippedLegs          int                      `json:"skipped_legs"`
	ErrorLegs            int                      `json:"error_legs"`
	Orders               []PurgeExecuteOrder      `json:"orders,omitempty"`
	Skipped              []PurgeExecuteSkippedLeg `json:"skipped,omitempty"`
	Warnings             []string                 `json:"warnings,omitempty"`
	Blockers             []TradingBlocker         `json:"blockers,omitempty"`
	Message              string                   `json:"message,omitempty"`
	MonitorCommand       string                   `json:"monitor_command,omitempty"`
	RestoreReviewCommand string                   `json:"restore_review_command,omitempty"`
	AsOf                 time.Time                `json:"as_of"`
}

// PurgeExecuteOrder reports the sanitized order state for one submitted leg.
type PurgeExecuteOrder struct {
	LegID           string             `json:"leg_id"`
	Symbol          string             `json:"symbol"`
	SecType         string             `json:"sec_type"`
	Contract        ContractParams     `json:"contract"`
	Action          string             `json:"action"`
	Quantity        int                `json:"quantity"`
	LimitPrice      float64            `json:"limit_price"`
	OrderRef        string             `json:"order_ref"`
	ReservedOrderID int                `json:"reserved_order_id,omitempty"`
	Status          string             `json:"status,omitempty"`
	LifecycleStatus string             `json:"lifecycle_status,omitempty"`
	SendState       string             `json:"send_state,omitempty"`
	Message         string             `json:"message,omitempty"`
	Quote           OrderQuoteSnapshot `json:"quote"`
}

// PurgeExecuteSkippedLeg records a selected leg that was not submitted and its
// stable reason.
type PurgeExecuteSkippedLeg struct {
	LegID    string         `json:"leg_id"`
	Symbol   string         `json:"symbol"`
	SecType  string         `json:"sec_type"`
	Contract ContractParams `json:"contract"`
	Reason   string         `json:"reason"`
}

// PurgeStatusParams selects a workflow or account-scoped ledger window. Empty
// values use daemon defaults and never broaden broker-write authority.
type PurgeStatusParams struct {
	PurgeID string `json:"purge_id,omitempty"`
	Account string `json:"account,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// PurgeStatusResult is the daemon's durable purge-ledger and observed-order
// view. It is not a broker statement.
type PurgeStatusResult struct {
	Kind            string            `json:"kind"`
	PurgeID         string            `json:"purge_id,omitempty"`
	Status          string            `json:"status"`
	Account         string            `json:"account,omitempty"`
	Rows            []PurgeLedgerRow  `json:"rows,omitempty"`
	Totals          PurgeLedgerTotals `json:"totals"`
	TotalOrders     int               `json:"total_orders"`
	OpenOrders      int               `json:"open_orders"`
	FilledOrders    int               `json:"filled_orders"`
	CancelledOrders int               `json:"cancelled_orders"`
	AttentionOrders int               `json:"attention_orders"`
	Orders          []OrderView       `json:"orders,omitempty"`
	Message         string            `json:"message,omitempty"`
	AsOf            time.Time         `json:"as_of"`
}

// PurgeLedgerTotals summarizes durable purge and restore quantities and values.
type PurgeLedgerTotals struct {
	ActiveRows        int     `json:"active_rows"`
	RestoredRows      int     `json:"restored_rows"`
	PurgedQuantity    float64 `json:"purged_quantity"`
	RestoredQuantity  float64 `json:"restored_quantity"`
	RemainingQuantity float64 `json:"remaining_quantity"`
	PurgeValue        float64 `json:"purge_value"`
	RestoreValue      float64 `json:"restore_value"`
	ShadowPnL         float64 `json:"shadow_pnl"`
}

// PurgeLedgerRow is one durable leg record reduced from purge, restore, and
// observed fill events.
type PurgeLedgerRow struct {
	LegID               string         `json:"leg_id"`
	PurgeID             string         `json:"purge_id,omitempty"`
	Symbol              string         `json:"symbol"`
	SecType             string         `json:"sec_type"`
	Contract            ContractParams `json:"contract"`
	Account             string         `json:"account,omitempty"`
	Mode                string         `json:"mode,omitempty"`
	Currency            string         `json:"currency,omitempty"`
	OriginalSide        string         `json:"original_side"`
	OriginalQuantity    float64        `json:"original_quantity"`
	PurgeAction         string         `json:"purge_action"`
	RestoreAction       string         `json:"restore_action"`
	Multiplier          int            `json:"multiplier"`
	PurgedQuantity      float64        `json:"purged_quantity"`
	RestoredQuantity    float64        `json:"restored_quantity"`
	RemainingQuantity   float64        `json:"remaining_quantity"`
	PurgeAvgPrice       float64        `json:"purge_avg_price,omitempty"`
	RestoreAvgPrice     float64        `json:"restore_avg_price,omitempty"`
	PurgeValue          float64        `json:"purge_value,omitempty"`
	RestoreValue        float64        `json:"restore_value,omitempty"`
	ShadowPnL           float64        `json:"shadow_pnl,omitempty"`
	Status              string         `json:"status"`
	LastPurgeOrderRef   string         `json:"last_purge_order_ref,omitempty"`
	LastRestoreOrderRef string         `json:"last_restore_order_ref,omitempty"`
	CreatedAt           time.Time      `json:"created_at,omitzero"`
	UpdatedAt           time.Time      `json:"updated_at,omitzero"`
	Warnings            []string       `json:"warnings,omitempty"`
}

// PurgeRestoreParams selects a proportional restore request. It is a new
// broker-write request and remains subject to current daemon gates.
type PurgeRestoreParams struct {
	PurgeID   string   `json:"purge_id,omitempty"`
	All       bool     `json:"all,omitempty"`
	Symbols   []string `json:"symbols,omitempty"`
	Scale     float64  `json:"scale,omitempty"`
	WaitMs    int      `json:"wait_ms,omitempty"`
	TimeoutMs int      `json:"timeout_ms,omitempty"`
	Origin    string   `json:"origin,omitempty"`
}

// PurgeRestoreResult reports modeled legs, submissions, skips, and durable
// ledger state. Submitted legs are not necessarily filled.
type PurgeRestoreResult struct {
	Kind           string                   `json:"kind"`
	PurgeID        string                   `json:"purge_id,omitempty"`
	Status         string                   `json:"status"`
	Mode           string                   `json:"mode,omitempty"`
	Account        string                   `json:"account,omitempty"`
	Endpoint       string                   `json:"endpoint,omitempty"`
	ClientID       int                      `json:"client_id,omitempty"`
	Scale          float64                  `json:"scale"`
	SelectedLegs   int                      `json:"selected_legs"`
	SubmittedLegs  int                      `json:"submitted_legs"`
	SkippedLegs    int                      `json:"skipped_legs"`
	ErrorLegs      int                      `json:"error_legs"`
	EstimatedValue float64                  `json:"estimated_value,omitempty"`
	ShadowPnL      float64                  `json:"shadow_pnl,omitempty"`
	Legs           []PurgeRestoreLeg        `json:"legs,omitempty"`
	Orders         []PurgeExecuteOrder      `json:"orders,omitempty"`
	Skipped        []PurgeExecuteSkippedLeg `json:"skipped,omitempty"`
	Warnings       []string                 `json:"warnings,omitempty"`
	Blockers       []TradingBlocker         `json:"blockers,omitempty"`
	Message        string                   `json:"message,omitempty"`
	LedgerRows     []PurgeLedgerRow         `json:"ledger_rows,omitempty"`
	AsOf           time.Time                `json:"as_of"`
}

// PurgeRestoreLeg reports the modeled preview and outcome for one restore leg.
// WhatIf remains broker evidence rather than a fill guarantee.
type PurgeRestoreLeg struct {
	LegID           string              `json:"leg_id"`
	Symbol          string              `json:"symbol"`
	SecType         string              `json:"sec_type"`
	Contract        ContractParams      `json:"contract"`
	Action          string              `json:"action"`
	Quantity        int                 `json:"quantity"`
	RemainingBefore float64             `json:"remaining_before"`
	LimitPrice      float64             `json:"limit_price,omitempty"`
	EstimatedValue  float64             `json:"estimated_value,omitempty"`
	ShadowPnL       float64             `json:"shadow_pnl,omitempty"`
	Quote           OrderQuoteSnapshot  `json:"quote,omitzero"`
	Position        OrderPositionImpact `json:"position,omitzero"`
	WhatIf          OrderWhatIfResult   `json:"what_if,omitzero"`
	Status          string              `json:"status"`
	Warnings        []string            `json:"warnings,omitempty"`
}

// OrderModifyResult reports the durable local and broker-send state of a gated
// modification attempt. Accepted does not imply broker acknowledgement.
type OrderModifyResult struct {
	Accepted        bool       `json:"accepted"`
	Mode            string     `json:"mode"`
	Account         string     `json:"account"`
	Endpoint        string     `json:"endpoint"`
	ClientID        int        `json:"client_id"`
	OrderRef        string     `json:"order_ref"`
	PreviewTokenID  string     `json:"preview_token_id"`
	ReservedOrderID int        `json:"reserved_order_id"`
	Draft           OrderDraft `json:"draft"`
	Status          string     `json:"status,omitempty"`
	LifecycleStatus string     `json:"lifecycle_status,omitempty"`
	SendState       string     `json:"send_state,omitempty"`
	Message         string     `json:"message,omitempty"`
	AsOf            time.Time  `json:"as_of"`
}

// OrderCancelResult reports the observed state after a cancellation request.
// Accepted does not imply the broker has confirmed cancellation.
type OrderCancelResult struct {
	Accepted        bool      `json:"accepted"`
	Order           OrderView `json:"order"`
	Status          string    `json:"status,omitempty"`
	LifecycleStatus string    `json:"lifecycle_status,omitempty"`
	SendState       string    `json:"send_state,omitempty"`
	Message         string    `json:"message,omitempty"`
	AsOf            time.Time `json:"as_of"`
}

// OrderEvent is the read-only lifecycle/audit row exposed from the private
// journal. It redacts full preview tokens and never implies a broker write
// unless Type/SendState explicitly say one was attempted.
type OrderEvent struct {
	At              time.Time       `json:"at"`
	Type            string          `json:"type"`
	OrderRef        string          `json:"order_ref,omitempty"`
	PreviewTokenID  string          `json:"preview_token_id,omitempty"`
	ReservedOrderID int             `json:"reserved_order_id,omitempty"`
	ClientID        int             `json:"client_id,omitempty"`
	PermID          int             `json:"perm_id,omitempty"`
	Account         string          `json:"account,omitempty"`
	Endpoint        string          `json:"endpoint,omitempty"`
	Mode            string          `json:"mode,omitempty"`
	Source          string          `json:"source,omitempty"`
	PurgeID         string          `json:"purge_id,omitempty"`
	LegID           string          `json:"leg_id,omitempty"`
	BypassPreview   bool            `json:"bypass_preview,omitempty"`
	Symbol          string          `json:"symbol,omitempty"`
	SecType         string          `json:"sec_type,omitempty"`
	ConID           int             `json:"con_id,omitempty"`
	Exchange        string          `json:"exchange,omitempty"`
	PrimaryExch     string          `json:"primary_exch,omitempty"`
	Currency        string          `json:"currency,omitempty"`
	LocalSymbol     string          `json:"local_symbol,omitempty"`
	TradingClass    string          `json:"trading_class,omitempty"`
	Expiry          string          `json:"expiry,omitempty"`
	Strike          float64         `json:"strike,omitempty"`
	Right           string          `json:"right,omitempty"`
	Multiplier      int             `json:"multiplier,omitempty"`
	Action          string          `json:"action,omitempty"`
	OrderType       string          `json:"order_type,omitempty"`
	TIF             string          `json:"tif,omitempty"`
	TriggerMethod   int             `json:"trigger_method,omitempty"`
	OutsideRTH      bool            `json:"outside_rth,omitempty"`
	Quantity        float64         `json:"quantity,omitempty"`
	LimitPrice      float64         `json:"limit_price,omitempty"`
	Trail           *OrderTrailSpec `json:"trail,omitempty"`
	OpenClose       string          `json:"open_close,omitempty"`
	Status          string          `json:"status,omitempty"`
	LifecycleStatus string          `json:"lifecycle_status,omitempty"`
	Filled          float64         `json:"filled,omitempty"`
	Remaining       float64         `json:"remaining,omitempty"`
	AvgFillPrice    float64         `json:"avg_fill_price,omitempty"`
	LastFillPrice   float64         `json:"last_fill_price,omitempty"`
	WhyHeld         string          `json:"why_held,omitempty"`
	MktCapPrice     float64         `json:"mkt_cap_price,omitempty"`
	ExecID          string          `json:"exec_id,omitempty"`
	ExecTime        string          `json:"exec_time,omitempty"`
	ErrorCode       int             `json:"error_code,omitempty"`
	SendState       string          `json:"send_state,omitempty"`
	Message         string          `json:"message,omitempty"`
}

// OrderView is the daemon's read-only product state for one locally observed
// order intent. It is reduced from the append-only journal; broker callbacks
// remain authoritative for acknowledgement/fill/cancel status.
type OrderView struct {
	OrderRef        string          `json:"order_ref,omitempty"`
	PreviewTokenID  string          `json:"preview_token_id,omitempty"`
	ReservedOrderID int             `json:"reserved_order_id,omitempty"`
	ClientID        int             `json:"client_id,omitempty"`
	PermID          int             `json:"perm_id,omitempty"`
	Account         string          `json:"account,omitempty"`
	Endpoint        string          `json:"endpoint,omitempty"`
	Mode            string          `json:"mode,omitempty"`
	Source          string          `json:"source,omitempty"`
	PurgeID         string          `json:"purge_id,omitempty"`
	LegID           string          `json:"leg_id,omitempty"`
	BypassPreview   bool            `json:"bypass_preview,omitempty"`
	Symbol          string          `json:"symbol,omitempty"`
	SecType         string          `json:"sec_type,omitempty"`
	ConID           int             `json:"con_id,omitempty"`
	Exchange        string          `json:"exchange,omitempty"`
	PrimaryExch     string          `json:"primary_exch,omitempty"`
	Currency        string          `json:"currency,omitempty"`
	LocalSymbol     string          `json:"local_symbol,omitempty"`
	TradingClass    string          `json:"trading_class,omitempty"`
	Expiry          string          `json:"expiry,omitempty"`
	Strike          float64         `json:"strike,omitempty"`
	Right           string          `json:"right,omitempty"`
	Multiplier      int             `json:"multiplier,omitempty"`
	Action          string          `json:"action,omitempty"`
	OrderType       string          `json:"order_type,omitempty"`
	TIF             string          `json:"tif,omitempty"`
	TriggerMethod   int             `json:"trigger_method,omitempty"`
	OutsideRTH      bool            `json:"outside_rth,omitempty"`
	Quantity        float64         `json:"quantity,omitempty"`
	LimitPrice      float64         `json:"limit_price,omitempty"`
	Trail           *OrderTrailSpec `json:"trail,omitempty"`
	OpenClose       string          `json:"open_close,omitempty"`
	Status          string          `json:"status,omitempty"`
	LifecycleStatus string          `json:"lifecycle_status"`
	Filled          float64         `json:"filled,omitempty"`
	Remaining       float64         `json:"remaining,omitempty"`
	AvgFillPrice    float64         `json:"avg_fill_price,omitempty"`
	LastFillPrice   float64         `json:"last_fill_price,omitempty"`
	WhyHeld         string          `json:"why_held,omitempty"`
	MktCapPrice     float64         `json:"mkt_cap_price,omitempty"`
	SendState       string          `json:"send_state,omitempty"`
	LastEvent       string          `json:"last_event,omitempty"`
	// LastErrorCode is populated only when LastEvent is broker-error. It is
	// typed audit evidence; LastMessage remains untrusted display text.
	LastErrorCode       int    `json:"last_error_code,omitempty"`
	LastMessage         string `json:"last_message,omitempty"`
	ReconciliationState string `json:"reconciliation_state,omitempty"`
	// ReconciliationKind classifies a position_mismatch by consequence:
	// short_entry_full (no coverage left; triggering opens a fresh
	// opposite-direction position of the full remaining quantity) or
	// short_entry_excess (partial coverage; triggering closes the position
	// and opens the excess). Severity is always "critical" for both — the
	// damaging event is identical; the kinds differ only in the offered fix
	// (cancel vs reduce). ReduceToQuantity is set only for the excess kind:
	// the exact quantity a reduce-modify must target.
	ReconciliationKind     string    `json:"reconciliation_kind,omitempty"`
	ReconciliationSeverity string    `json:"reconciliation_severity,omitempty"`
	ShortRiskQuantity      float64   `json:"short_risk_quantity,omitempty"`
	ReduceToQuantity       float64   `json:"reduce_to_quantity,omitempty"`
	BrokerTruthAsOf        time.Time `json:"broker_truth_as_of,omitzero"`
	UpdatedAt              time.Time `json:"updated_at,omitzero"`
	Open                   bool      `json:"open"`
	ModifyEligible         bool      `json:"modify_eligible"`
	CancelEligible         bool      `json:"cancel_eligible"`
}

// OrdersOpenResult is the daemon's locally reduced view of currently open
// orders. It is explicitly not a broker statement.
type OrdersOpenResult struct {
	Orders             []OrderView `json:"orders"`
	AsOf               time.Time   `json:"as_of"`
	Account            string      `json:"account,omitempty"`
	Mode               string      `json:"mode,omitempty"`
	LastLocalEventAt   time.Time   `json:"last_local_event_at,omitzero"`
	NotBrokerStatement string      `json:"not_broker_statement"`
	Limitations        []string    `json:"limitations"`
}

// OrderStatusResult returns local product state and bounded audit events for one
// order identity. Found false leaves Order at its zero value.
type OrderStatusResult struct {
	Found              bool         `json:"found"`
	Order              OrderView    `json:"order,omitzero"`
	Events             []OrderEvent `json:"events,omitempty"`
	AsOf               time.Time    `json:"as_of"`
	Account            string       `json:"account,omitempty"`
	Mode               string       `json:"mode,omitempty"`
	LastLocalEventAt   time.Time    `json:"last_local_event_at,omitzero"`
	NotBrokerStatement string       `json:"not_broker_statement"`
	Limitations        []string     `json:"limitations"`
}

// OrdersHistoryRow combines one reduced order with a bounded event window.
type OrdersHistoryRow struct {
	Order            OrderView    `json:"order"`
	Events           []OrderEvent `json:"events"`
	EventsCount      int          `json:"events_count"`
	TotalEventsCount int          `json:"total_events_count"`
	EventsTruncated  bool         `json:"events_truncated"`
}

// OrdersHistoryResult is a bounded local history query. Truncated and
// EventsTruncated disclose omitted rows and events; it is not a broker statement.
type OrdersHistoryResult struct {
	Orders             []OrdersHistoryRow `json:"orders"`
	AsOf               time.Time          `json:"as_of"`
	Since              time.Time          `json:"since"`
	Until              time.Time          `json:"until"`
	Account            string             `json:"account,omitempty"`
	Mode               string             `json:"mode,omitempty"`
	Count              int                `json:"count"`
	TotalCount         int                `json:"total_count"`
	EventsCount        int                `json:"events_count"`
	TotalEventsCount   int                `json:"total_events_count"`
	Limit              int                `json:"limit"`
	EventLimit         int                `json:"event_limit"`
	Truncated          bool               `json:"truncated"`
	EventsTruncated    bool               `json:"events_truncated"`
	NotBrokerStatement string             `json:"not_broker_statement"`
	Limitations        []string           `json:"limitations"`
}

// MembersHealth is the wire shape for the SPX-members surface
// rendered in `canary status`. Distinct from BreadthSPXResult: that
// carries the COMPUTED breadth value; this carries metadata about
// the constituent LIST.
//
// Source is "cache" when the daemon loaded the runtime-refreshed
// file, "embedded" when it fell back to the binary's compiled-in
// baseline. AsOf is the date the active list was generated.
// RefreshState is one of the spx.RefreshState constants ("healthy",
// "network_failed", "parse_failed", "disabled (config)", "disabled
// (env)"). Healthy is the steady-state; renderer omits the
// `refresh:` segment when healthy.
type MembersHealth struct {
	Source       string    `json:"source"`
	AsOf         time.Time `json:"as_of"`
	Count        int       `json:"count"`
	RefreshState string    `json:"refresh_state"`
}
