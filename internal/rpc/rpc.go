// Package rpc defines the stable method names, envelopes, and typed payloads
// broker authority remain in the daemon.
package rpc

import (
	"encoding/json"
	"strings"
	"time"
)

// Daemon method names are stable wire identifiers shared by every adapter.
const (
	MethodAccountSummary  = "account.summary"
	MethodPositionsList   = "positions.list"
	MethodQuoteSnapshot   = "quote.snapshot"
	MethodQuoteSubscribe  = "quote.subscribe"
	MethodChainFetch      = "chain.fetch"
	MethodChainExpiries   = "chain.expiries"
	MethodTechnical       = "technical.snapshot"
	MethodMarketCalendar  = "market.calendar"
	MethodStatusHealth    = "status.health"
	MethodTradingStatus   = "trading.status"
	MethodSettingsGet     = "settings.get"
	MethodSettingsUpdate  = "settings.update"
	MethodOrdersOpen      = "orders.open"
	MethodOrdersHistory   = "orders.history"
	MethodOrderStatus     = "order.status"
	MethodOrderPreview    = "order.preview"
	MethodStrategyPreview = "strategy.preview"
	MethodBreadthSPX      = "breadth.spx"
	MethodGammaZeroSPX    = "gamma.zero_spx"
	MethodRegimeSnapshot  = "regime.snapshot"
	MethodOrderPlace      = "order.place"
	MethodOrderModify     = "order.modify"
	MethodOrderCancel     = "order.cancel"
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
// based on the value. HealthResult.DataType remains on the wire shape
// (omitempty) for renderer-fallback compatibility but is no longer
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
// no bid/ask and IVs come from IBKR's model-computation engine off
// Display cadence only — never use this for policy blockers or eligibility
// gates. Those must go through a marketcal-backed authority (the daemon's
// Fail-open: if the America/New_York zone can't be loaded (e.g. tzdata
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
type SessionClass int

// Session classes partition the U.S. equity-options day using the boundaries
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
// because the wire shape (frame, not Error) and lifecycle (mid-stream
const (
	FrameErrGatewayLost          = "gateway_lost"
	FrameErrEntitlementLost      = "entitlement_lost"
	FrameErrSubscriptionRejected = "subscription_rejected"
	FrameErrDaemonShutdown       = "daemon_shutdown"
)

// SecType values carried on PositionView.SecType. The daemon maps IBKR's
// raw three-letter SecType codes ("STK", "OPT") onto the canonical wire
// values below in positionSecType — full words, not the short forms IBKR
// broker and canonical spellings.
const (
	SecTypeStock  = "STOCK"
	SecTypeOption = "OPTION"
	SecTypeFuture = "FUTURE"
	SecTypeIndex  = "INDEX"
)

// PositionQuotesAsStock reports whether a non-option position row is an
// equity — the only secType for which a stock quote on the bare symbol
// that join — daemon, CLI, and MCP must all use it rather than re-deriving
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
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is one custom daemon-protocol response. Unary success uses Result;
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
// Asymmetry to watch for: SecType here uses the IBKR API's three-letter
// preserves the legacy US SMART/USD path; "de" selects the German/Xetra
// EUR route used by IBKR's IBIS listing codes. Exchange/Currency/PrimaryExch
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
// daemon normalizes it to the stable result.market token. Date is YYYY-MM-DD
// for the market state at that exact instant. Days controls how many calendar
type MarketCalendarParams struct {
	Market string    `json:"market,omitempty"`
	Date   string    `json:"date,omitempty"`
	At     time.Time `json:"at,omitzero"`
	Days   int       `json:"days,omitempty"`
}

// MarketSession is one official market-calendar row. Open/Close are present
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
type BreadthRefreshProgress struct {
	SessionKey  string                `json:"session_key"`
	StartedAt   time.Time             `json:"started_at"`
	Processed   int                   `json:"processed"`
	Total       int                   `json:"total"`
	Deadline    time.Time             `json:"deadline,omitzero"`
	LastFailure BreadthRefreshFailure `json:"last_failure,omitempty"`
}

// BreadthSPXResult is the payload for MethodBreadthSPX. The two
// derived. Method is a short token; longer methodology disclosure
type BreadthSPXResult struct {
	// State classifies the engine pipeline at the moment this envelope
	State BreadthState `json:"state"`
	// Refreshing is true when a newer breadth run is in flight or waiting to
	// retry while this envelope serves the last good snapshot.
	Refreshing bool `json:"refreshing,omitempty"`
	// Refresh is the current or most recently completed paced-pass progress.
	// slow advancing run, a stopped run, and the last classified failure.
	Refresh *BreadthRefreshProgress `json:"refresh,omitempty"`
	// PctAbove50DMA is the current fast-window reading: percentage of
	// divergence. Zero is meaningful only when State == "ready" (a
	PctAbove50DMA float64 `json:"pct_above_50dma"`
	// PctAbove200DMA is the slow-window reading: percentage above the
	// 200-day SMA. Caught the 1999 and 2021 cyclical tops cleanly.
	// Bands per locked plan: below 40% = red / 40-60% = yellow / above
	// 60% = green (calibrated to the post-Mag-7 era).
	PctAbove200DMA float64 `json:"pct_above_200dma"`
	// NewHighsToday is the count of constituents whose latest close
	NewHighsToday int `json:"new_highs_today"`
	// NewLowsToday is the symmetric count for new 252-bar lows.
	NewLowsToday int `json:"new_lows_today"`
	// NetNewHighsPct is (NewHighsToday - NewLowsToday) / coverage × 100
	NetNewHighsPct float64 `json:"net_new_highs_pct"`
	// History is the trailing daily series, oldest first. Length is
	// bounded by BreadthSPXParams.HistoryDays. Each point carries
	// both SMA readings plus the new-highs/lows counts.
	History []BreadthDailyValue `json:"history,omitempty"`
	// Source identifies the data provenance for the headline value.
	Source string `json:"source"`
	// Method is a short token naming the computation path so renderers
	// can disclose methodology. Current token:
	// pulled via IBKR's historical-bar feed, since IBKR doesn't
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the result was assembled.
	AsOf time.Time `json:"as_of"`
	// SessionKey is the US-equity session date represented by the
	// computed daily bars. It may differ from AsOf on weekends,
	// holidays, and before the current session's close is settled.
	SessionKey string `json:"session_key,omitempty"`
	// Stale reports that SessionKey is not the latest completed US-equity
	Stale bool `json:"stale,omitempty"`
	// SpotAt is the gateway-observation timestamp for the headline,
	// distinct from AsOf which covers history + headline.
	SpotAt time.Time `json:"spot_at,omitzero"`
	// DataType reflects the gateway's feed state when the headline
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
	// promotes the forced compute only on success. Useful for diagnostics;
	Force bool `json:"force,omitempty"`
	// Scope selects which underlying(s) to compute. One of GammaZeroScopeSPY
	Scope string `json:"scope,omitempty"`
	// IncludeProfiles asks clients/renderers to retain the full profile
	IncludeProfiles bool `json:"include_profiles,omitempty"`
}

// GammaZeroParams echoes the v1 calibration window back to the caller so
// renderers can show "computed over N expirations within ±X%." Future
// versions can add fields here without breaking the result shape — every
// renderer-relevant tuning parameter lives on this echo.
type GammaZeroParams struct {
	// ExpiryCount is the number of nearest non-0DTE-post-settlement
	ExpiryCount int `json:"expiry_count"`
	// StrikeWidthPct is the half-width of the strike grid around spot,
	// expressed as a fraction (0.10 = ATM ± 10 %).
	StrikeWidthPct float64 `json:"strike_width_pct"`
	// SweepRangePct is the half-range of the spot sweep used to find
	SweepRangePct float64 `json:"sweep_range_pct"`
	// WorkerCount is the per-leg fan-out concurrency. 4 matches the
	// documented safe gateway throttle; bumping it requires retuning
	// AcquireMarketDataSlot.
	WorkerCount int `json:"worker_count"`
}

// GammaProfilePoint is one (spot, dealer_gex) sample from the sweep.
type GammaProfilePoint struct {
	Spot float64 `json:"spot"`
	GEX  float64 `json:"gex"`
}

// StrikeConcentration is one row of the "where dealer hedging
type StrikeConcentration struct {
	// Underlying identifies which index this strike belongs to —
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
type GammaLegDiagnostics struct {
	Total          GammaLegDiagnosticCounts            `json:"total"`
	ByUnderlying   map[string]GammaLegDiagnosticCounts `json:"by_underlying,omitempty"`
	ByTradingClass map[string]GammaLegDiagnosticCounts `json:"by_trading_class,omitempty"`
}

// GammaCollectionDiagnostic exposes the source-level option-chain funnel for
// result can name the source failure rather than only reporting a rankability
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
const (
	GammaRankabilityRankable    = "rankable"
	GammaRankabilityContextOnly = "context_only"
	GammaRankabilityBlocked     = "blocked"
	GammaRankabilityUnavailable = "unavailable"

	GammaQualityGatePass    = "pass"
	GammaQualityGateContext = "context"
	GammaQualityGateBlock   = "block"

	// GammaQualityGateFreshness, GammaQualityGateSPXCoverage, and
	GammaQualityGateFreshness     = "freshness"
	GammaQualityGateSPXCoverage   = "spx_coverage"
	GammaFreshnessSessionMismatch = "session_mismatch"
)

// GammaSignalQuality is the trading-grade gate for the gamma payload.
// Result is present, but downstream regime/Stress consumers must only count
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
	Code string `json:"code"`
	// Scope names the affected slice: "SPY", "SPX", "SPY+SPX", or a
	// narrower trading class / expiry when the condition is that local.
	Scope string `json:"scope,omitempty"`
	// Severity is one of "info", "data_quality", or "methodology".
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
type GammaIndexSummary struct {
	Underlying      string   `json:"underlying,omitempty"`
	SpotUnderlying  float64  `json:"spot_underlying,omitempty"`
	DataType        string   `json:"data_type,omitempty"`
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
type SkewFitInfo struct {
	Points   int     `json:"points"`
	RSquared float64 `json:"r_squared"`
	// ResidualRMS is the root-mean-square fit residual in IV (vol)
	ResidualRMS float64    `json:"residual_rms,omitempty"`
	Range       [2]float64 `json:"range"`
}

// GammaZeroComputed is the actual zero-gamma payload — populated when
// SPY and SPX live on different price scales, so consumers must read
type GammaZeroComputed struct {
	// SpotUnderlying is the price of the underlying instrument
	SpotUnderlying float64 `json:"spot_underlying,omitempty"`
	// SpotAt is the gateway-observation timestamp for SpotUnderlying.
	SpotAt time.Time `json:"spot_at,omitzero"`
	// DataType is the gateway feed state shared by the underlying spot and
	// option-model inputs. Delayed results are accepted only when the option
	// fan-out used IBKR's delayed model-computation tick 83 throughout.
	DataType string `json:"data_type,omitempty"`

	// ZeroGamma is the dealer γ-zero level under the Perfiliev convention
	ZeroGamma *float64 `json:"zero_gamma,omitempty"`
	// GapPct is (SpotUnderlying − ZeroGamma) / ZeroGamma × 100. nil iff
	GapPct *float64 `json:"gap_pct,omitempty"`
	// GammaSign is "positive" or "negative" and is meaningful only when
	// ZeroGamma is nil — it tells the renderer which side of zero the
	// whole sweep landed on so the UI can say "all long-gamma" or "all
	// short-gamma in window."
	GammaSign string `json:"gamma_sign,omitempty"`
	// Profile is the full (spot, gex) sweep, oldest first. 60 points
	Profile []GammaProfilePoint `json:"profile,omitempty"`

	// GammaTotalAbs is the sign-agnostic magnitude signal at
	// SpotUnderlying: Σ |Γ| × OI × 100 × SpotUnderlying² × 0.01. In
	// dollar gamma terms — the total notional dealer hedging flow for
	// a 1% underlying move, independent of any positioning assumption.
	// Larger = market is more sensitive to dealer rebalancing.
	GammaTotalAbs float64 `json:"gamma_total_abs"`
	// GammaTotalAbsConvention names the sign-handling for GammaTotalAbs
	GammaTotalAbsConvention string `json:"gamma_total_abs_convention,omitempty"`
	// TopStrikes is the top-N strikes ranked by absolute gamma notional.
	TopStrikes []StrikeConcentration `json:"top_strikes"`
	// TopConcentrationPct is TopStrikes[0].AbsGEX / GammaTotalAbs × 100 —
	TopConcentrationPct float64 `json:"top_concentration_pct,omitempty"`

	// SweepLowAbs / SweepHighAbs are the absolute spot bounds of the
	// sweep window in dollars: SpotUnderlying × (1 ± Params.SweepRangePct).
	// Surfaced for renderers that want to print "γ-zero outside swept
	// range $A.AA-$C.CC" without re-deriving the multiplication.
	SweepLowAbs  float64 `json:"sweep_low_abs,omitempty"`
	SweepHighAbs float64 `json:"sweep_high_abs,omitempty"`

	// Expirations is the YYYY-MM-DD list of expirations actually
	Expirations []string `json:"expirations"`
	// LegCount is the number of option legs that contributed non-zero
	LegCount int `json:"leg_count"`
	// PricedLegCount is the number of option legs that delivered IV (or
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
	// serialized; wire consumers read WarningDetails instead.
	Warnings []string `json:"-"`
	// WarningDetails is the serialized warning surface: scoped,
	// user-facing explanations plus optional impact/action text.
	WarningDetails []GammaWarningDetail `json:"warning_details,omitempty"`
	// Summary is a compact interpretation of the result. It is designed
	Summary *GammaZeroSummary `json:"summary,omitempty"`

	// ZeroGamma0DTE / Profile0DTE / GammaSign0DTE / LegCount0DTE are the
	// same headline triple computed over legs with DTE == 0 only —
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
	SkewModel string `json:"skew_model,omitempty"`
	// SkewFitQuality is one SkewFitInfo per expiry that fitted a curve
	SkewFitQuality map[string]SkewFitInfo `json:"skew_fit_quality,omitempty"`

	// Params echoes the v1 calibration window so a renderer can show
	Params GammaZeroParams `json:"params"`
	// Source identifies the data provenance for the headline numbers.
	Source string `json:"source"`
	// AuthorityProvenance is empty for a current-code compute. A non-empty
	AuthorityProvenance string `json:"authority_provenance,omitempty"`
	// Method is a short stable token for the computation path. v3:
	//     fixes a v2 race where IV-but-no-Greeks legs contributed 0 to
	// "perfiliev" is dropped from the token because Perfiliev's
	Method string `json:"method"`
	// AsOf is the daemon's wall-clock when the compute finished.
	AsOf time.Time `json:"as_of"`
	// DurationMS is honest about how long the compute took on the wall
	DurationMS int64 `json:"duration_ms"`

	// Scope is the discriminator for combined-vs-single-underlying
	// Empty is treated as Scope="spy" by legacy renderers only.
	Scope string `json:"scope,omitempty"`

	// PerIndex carries the per-underlying detail when Scope="spy+spx".
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
	// always; that gate never fired and missed the actual case worth
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
	// from one that never started. Mirrors BreadthSPXResult.Refreshing.
	Refreshing bool `json:"refreshing,omitempty"`
	// StartedAt is when the currently-relevant compute kicked off — for
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
	// ColdReasonCode / ColdReason / ColdAction are populated when the
	// also populated on Status == "error" when the failed attempt is
	// failure itself and is not softened.
	ColdReasonCode string `json:"cold_reason_code,omitempty"`
	ColdReason     string `json:"cold_reason,omitempty"`
	ColdAction     string `json:"cold_action,omitempty"`
	// RetryOfErrorAt + RetryOfErrorSummary are non-nil/non-empty only
	// because the previous attempt failed past gammaErrorRetryTTL. The
	// HH:MM:SS" so the user sees the prior failure context — without
	RetryOfErrorAt      *time.Time `json:"retry_of_error_at,omitempty"`
	RetryOfErrorSummary string     `json:"retry_of_error_summary,omitempty"`
}

// StripGammaProfiles removes chart-sized sweep arrays from a gamma result
func StripGammaProfiles(r *GammaZeroSPXResult) {
	if r == nil {
		return
	}
	stripGammaComputedProfiles(r.Result)
	stripGammaComputedProfiles(r.DiagnosticResult)
}

// StripRegimeGammaProfiles removes large gamma profiles from every regime
func StripRegimeGammaProfiles(r *RegimeSnapshotResult) {
	if r == nil {
		return
	}
	StripGammaProfiles(&r.GammaZero.Envelope)
}

// CompactRegimeSnapshot removes methodology prose and chart/history payloads
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
// daemon never derives green/yellow/red status from raw values (the
//   - "unavailable" — IBKR doesn't carry the feed on this account; the
//   - "error"       — fetch failed; `error_message` carries the reason
const (
	RegimeStatusOK          = "ok"
	RegimeStatusStale       = "stale"
	RegimeStatusComputing   = "computing"
	RegimeStatusUnavailable = "unavailable"
	RegimeStatusError       = "error"
)

// Quality is the provenance + freshness envelope for one scalar regime
// Quality means "no provenance recorded" (legacy/migration only). The
type Quality struct {
	AsOf           time.Time `json:"as_of"`
	FreshnessClass string    `json:"freshness_class"`
	Confidence     string    `json:"confidence"`
	// Source is a one-line human-readable provenance description, e.g.
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
// states for the wire surface — but necessary for streak persistence).
// Indicator unavailable/computing/error states freeze the counter
type StreakInfo struct {
	Band     string `json:"band"`
	Sessions int    `json:"sessions"`
	Since    string `json:"since"`
}

// RegimeIndicatorMeta is the compact interpretation/provenance layer shared
type RegimeIndicatorMeta struct {
	Band       string             `json:"band,omitempty"`
	BandReason string             `json:"band_reason,omitempty"`
	Thresholds *RegimeThresholds  `json:"thresholds,omitempty"`
	AsOf       *RegimeAsOfSummary `json:"as_of,omitempty"`
	// Freshness is the cadence-relative freshness verdict for the row's
	// served staleness policy so renderers never hardcode a twin.
	Freshness *RegimeFreshness `json:"freshness,omitempty"`
	// Eligibility says whether a red band may CONFIRM stress (depth +
	// visible and can warn only while the required input set is usable; broken
	Eligibility *RegimeEligibility `json:"eligibility,omitempty"`
}

// RegimeFreshness is the cadence-relative freshness verdict plus the served
type RegimeFreshness struct {
	Class         string `json:"class"`
	MaxAgeSeconds int64  `json:"max_age_seconds,omitempty"`
}

// Regime freshness values compare a row with its native publication cadence.
// Only fresh is confirmation-eligible; the rest are context or defect
//
//	failed, inside an explicit tolerance.
//	applies; also the fail-closed class for missing or untyped evidence.
const (
	RegimeFreshnessFresh   = "fresh"
	RegimeFreshnessNotDue  = "not_due"
	RegimeFreshnessPending = "pending"
	RegimeFreshnessStale   = "stale"
	RegimeFreshnessOverdue = "overdue"
)

// RegimeEligibility is the confirmation-eligibility verdict for a red row.
// inside the minimum depth. Reasons name the failed gates when not eligible:
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
	// typically the only useful daily anchor since VIX itself doesn't
	VIXPrevClose *float64 `json:"vix_prev_close,omitempty"`
	VIXChangePct *float64 `json:"vix_change_pct,omitempty"` // (vix − prev_close) / prev_close × 100
	// VIXChangeBasis is the day-change provenance on closed dates. Empty on
	// trading dates (live print vs tick-9 close).
	VIXChangeBasis string `json:"vix_change_basis,omitempty"`
	// Per-scalar provenance. Each *Quality is nil when the corresponding
	VIXQuality   *Quality `json:"vix_quality,omitempty"`
	VIX3MQuality *Quality `json:"vix3m_quality,omitempty"`
	// Cboe's published VIX3M daily close, read independently of the broker,
	// and what comparing it against the broker leg established. VIX3MSource
	// broker keeps answering with a value off-window whatever its real
	// vintage; only a dated official close can settle that, so these fields
	// VIX3MGatewayLast retains the broker's own reading when the official close
	VIX3MSource       string   `json:"vix3m_source,omitempty"`
	VIX3MGatewayLast  *float64 `json:"vix3m_gateway_last,omitempty"`
	VIX3MOfficial     *float64 `json:"vix3m_official,omitempty"`
	VIX3MOfficialDate string   `json:"vix3m_official_date,omitempty"`
	VIX3MCrossCheck   string   `json:"vix3m_cross_check,omitempty"`
	// VIX3MAnchorVIX is the VIX print observed together with the served VIX3M
	VIX3MAnchorVIX *float64 `json:"vix3m_anchor_vix,omitempty"`
	// Streak counts how many consecutive sessions this row's value has
	// freezes rather than resets.
	Streak *StreakInfo `json:"streak,omitempty"`
}

// Provenance of the served VIX3M leg.
const (
	VIX3MSourceGateway  = "gateway"
	VIX3MSourceOfficial = "cboe_official_close"
)

// VIX3M cross-source verdicts. In frozen mode the broker re-sends its last
//   - official_only: Cboe covered that window and the broker produced no VIX3M
//     close yet — it lands after the session ends. The broker leg stands in,
//   - disagree: both described the same window and differed. The broker leg is
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
	SPYChangeBasis string          `json:"spy_change_basis,omitempty"`
	HYGDataType    string          `json:"hyg_data_type,omitempty"`
	HYGRange52W    *RegimeRange52W `json:"hyg_range_52w,omitempty"`
	Notes          string          `json:"notes,omitempty"`
	ErrorMessage   string          `json:"error_message,omitempty"`
	FieldsMissing  []string        `json:"fields_missing,omitempty"`
	// Per-scalar provenance. SPY52WHigh has two paths (live tick 165 vs
	HYGQuality        *Quality `json:"hyg_quality,omitempty"`
	HYG50DMAQuality   *Quality `json:"hyg_50dma_quality,omitempty"`
	SPYQuality        *Quality `json:"spy_quality,omitempty"`
	SPY52WHighQuality *Quality `json:"spy_52w_high_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeRange52W situates an indicator's latest reading inside its trailing
// 52-week low-high range. Pos is 0 at the low and 100 at the high. It is
// display context only — bands and gates never read it — and is omitted when
// the available history does not actually span the year it claims.
type RegimeRange52W struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
	Pos  float64 `json:"pos"`
}

// RegimeVolOfVol is the VVIX vol-of-vol row. It uses Cboe's official
type RegimeVolOfVol struct {
	RegimeIndicatorMeta
	Status       string          `json:"status"`
	Symbol       string          `json:"symbol,omitempty"` // "VVIX"
	Last         *float64        `json:"last"`
	Change20D    *float64        `json:"change_20d_pct,omitempty"` // (last − t-20) / t-20 × 100
	Change5D     *float64        `json:"change_5d_pct,omitempty"`  // (last − t-5) / t-5 × 100; vvix_daily_v2 amber input
	AsOfDate     string          `json:"as_of_date,omitempty"`     // YYYY-MM-DD observation date
	Range52W     *RegimeRange52W `json:"range_52w,omitempty"`
	Source       string          `json:"source,omitempty"`
	Notes        string          `json:"notes,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
	ValueQuality *Quality        `json:"value_quality,omitempty"`
	Streak       *StreakInfo     `json:"streak,omitempty"`
}

// RegimeCreditSpreads is the official cash-credit companion to the HYG
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
type RegimeFundingStress struct {
	RegimeIndicatorMeta
	Status         string      `json:"status"`
	CP3M           *float64    `json:"cp_3m_rate"`
	TBill3M        *float64    `json:"tbill_3m_rate"`
	SpreadBps      *float64    `json:"spread_bps"`
	Change5Bps     *float64    `json:"change_5obs_bps,omitempty"` // spread now minus five CP publications back; funding_cp_tbill_v2 amber input
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
type RegimeUSDJPY struct {
	RegimeIndicatorMeta
	Status        string          `json:"status"`
	Symbol        string          `json:"symbol"` // "USD.JPY" canonical form
	Last          *float64        `json:"last"`
	Close7DAgo    *float64        `json:"close_7d_ago"`      // close from 7 trading days ago
	WeeklyChange  *float64        `json:"weekly_change_pct"` // (last − close_7d_ago) / close_7d_ago × 100
	Range52W      *RegimeRange52W `json:"range_52w,omitempty"`
	DataType      string          `json:"data_type,omitempty"`
	Notes         string          `json:"notes,omitempty"`
	ErrorMessage  string          `json:"error_message,omitempty"`
	FieldsMissing []string        `json:"fields_missing,omitempty"`
	// Per-scalar provenance. Last is firm-live (or firm-frozen);
	LastQuality       *Quality `json:"last_quality,omitempty"`
	Close7DAgoQuality *Quality `json:"close_7d_ago_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeGammaZero is the existing gamma.zero_spx
// result. Method token + warning_details carry methodology disclosures.
type RegimeGammaZero struct {
	RegimeIndicatorMeta
	Status        string             `json:"status"`
	Envelope      GammaZeroSPXResult `json:"envelope"`
	Notes         string             `json:"notes,omitempty"`
	FieldsMissing []string           `json:"fields_missing,omitempty"`
	// Per-scalar provenance for the two values the renderer prints:
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
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeBreadth is Indicator 5: the existing breadth.spx envelope
// IBKR-paced), so this row typically surfaces Status="computing" with
type RegimeBreadth struct {
	RegimeIndicatorMeta
	Status        string           `json:"status"`
	Envelope      BreadthSPXResult `json:"envelope"`
	Notes         string           `json:"notes,omitempty"`
	FieldsMissing []string         `json:"fields_missing,omitempty"`
	// PctAbove50DMA / PctAbove200DMA / NewHighsToday / NewLowsToday /
	PctAbove50DMA  float64 `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighsToday  int     `json:"new_highs_today,omitempty"`
	NewLowsToday   int     `json:"new_lows_today,omitempty"`
	NetNewHighsPct float64 `json:"net_new_highs_pct,omitempty"`
	// Per-scalar provenance for the breadth percentage. firm-live or
	// because constituent coverage fell below the safety threshold.
	ValueQuality *Quality `json:"value_quality,omitempty"`
	// Streak counts consecutive sessions in the current band. See
	Streak *StreakInfo `json:"streak,omitempty"`
}

// RegimeSnapshotParams is the input for MethodRegimeSnapshot. Empty
type RegimeSnapshotParams struct{}

// RegimeSnapshotResult is the wire payload for the dashboard
// Compatibility note for renderers: the daemon never returns nil for
// pointers so "not arrived yet" vs "exactly zero" stays
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
	// tape terms keep full effect (fail-open). Excluded from the regime
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
	WarningDetails []RegimeWarning `json:"warning_details,omitempty"`
	// DataQuality carries the same high-level data-quality summary used by
	// status.health: degraded gamma and stale regime clusters. It is coarser
	// than WarningDetails by design, so humans and agents can decide whether
	// to interpret the regime read carefully without walking every row.
	DataQuality []DataQualityHealth `json:"data_quality,omitempty"`
	// SourceHealth is the orchestration-facing freshness/readiness summary
	SourceHealth []SourceHealth `json:"source_health,omitempty"`
	// SpecDoc points consumers (especially LLM-driven ones) at the
	// canonical methodology + threshold reference so they don't
	// hallucinate band edges. It is the published URL rather than a
	// repository path: a remote MCP client can fetch the former and
	// cannot open the latter. Same value on every response.
	SpecDoc string `json:"spec_doc"`
}

// RegimeSummary is the compact, agent-first reading of a regime snapshot.
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
type RegimeWarning struct {
	Code     string `json:"code"`
	Scope    string `json:"scope"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Impact   string `json:"impact"`
	Action   string `json:"action"`
}

// RegimeComposite is the daemon-side rollup of the regime rows.
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
	// survived the isolated-red downgrades. Only these reds may confirm
	// visible, early-warning evidence, never confirmation.
	ClusterEligibleRedCount    int `json:"cluster_eligible_red_count"`
	ClusterProvisionalRedCount int `json:"cluster_provisional_red_count,omitempty"`
}

// Quote is the daemon's snapshot result.
// as the legacy selected-price pair and mirror QuotePrice when an indicative
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

// Frame is a single streaming tick. DataType carries the gateway's
// compatible because of omitempty — older consumers parsing tick frames
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
// DayChange / DayChangePct describe how far the account valuation mark sits
// we don't track contract-level prev close) is distinct from "exactly flat".
// MarketValue is serialized as market_value_ccy: the broker's own market value
// roughly 100×; the two agree only for equities and options, which is what made
// populated when the daemon knows the account base currency and either the row
// unavailable, never zero-substituted.
// (illiquid leg, OOH model abstention, subscribe slot churn) — never zero-
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
	// on options (IBKR's averageCost is multiplier-inclusive on OPT).
	Multiplier    int     `json:"multiplier"`
	AvgCost       float64 `json:"avg_cost"`
	Mark          float64 `json:"mark"`
	ValuationMark float64 `json:"valuation_mark,omitempty"`
	DataType      string  `json:"data_type,omitempty"`
	// PriceSource names the quote input that produced the row's quote
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
	// The position stays account truth either way.
	QuoteExpectation       string `json:"quote_expectation,omitempty"`
	QuoteExpectationReason string `json:"quote_expectation_reason,omitempty"`
	// SessionContext explains the trading-calendar state behind PriceAt.
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
	// sentinel". Never zero-substituted. For options, the daily figure
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
	// budget expired without delivering the tick — never zero-substituted.
	OptionBid         *float64 `json:"option_bid,omitempty"`
	OptionAsk         *float64 `json:"option_ask,omitempty"`
	OptionPrevClose   *float64 `json:"option_prev_close,omitempty"`
	IV                *float64 `json:"iv,omitempty"`
	MarkOutsideBidAsk bool     `json:"mark_outside_bid_ask,omitempty"`

	// Underlying is the model-computation underlying spot IBKR sent alongside
	// — never zero-substituted.
	Underlying *float64 `json:"underlying,omitempty"`
}

// PositionsResult wraps the array so the daemon can attach metadata later.
type PositionsResult struct {
	// DataType reflects the per-position mark-price feed when the daemon
	DataType           string                     `json:"data_type,omitempty"`
	AsOf               time.Time                  `json:"as_of"`
	Stocks             []PositionView             `json:"stocks"`
	Options            []PositionView             `json:"options"`
	ByUnderlying       []PositionGroup            `json:"by_underlying"`
	Strategies         []PositionStrategy         `json:"strategies,omitempty"`
	StrategyIssues     []StrategyGroupingIssue    `json:"strategy_issues,omitempty"`
	Portfolio          *PositionsPortfolio        `json:"portfolio,omitempty"`
	ProtectionCoverage *ProtectionCoverageSummary `json:"protection_coverage,omitempty"`
	AccountID          string                     `json:"account_id,omitempty"`
	// Authority is the account scope and portfolio-stream receipt contract.
	Authority *AccountDataAuthority `json:"authority,omitempty"`
}

// Position strategy sources, states, and operations form the typed contract
// shared by daemon-owned grouping and read-only adapters.
const (
	PositionStrategySourceCanary   = "canary_lineage"
	PositionStrategySourceBroker   = "broker_combo"
	PositionStrategySourceInferred = "inferred"
	PositionStrategySourceOperator = "operator_confirmed"

	PositionStrategyStatusCurrent = "current"
	PositionStrategyStatusReview  = "review_required"
	PositionStrategyStatusClosed  = "closed"

	StrategyOperationClose  = "close"
	StrategyOperationReduce = "reduce"
)

// PositionStrategy is the daemon-owned grouping of exact held contracts.
// Units are whole strategy units; signed ratios describe the held direction.
type PositionStrategy struct {
	ID                  string                `json:"id"`
	Revision            int64                 `json:"revision"`
	Underlying          string                `json:"underlying"`
	Kind                string                `json:"kind,omitempty"`
	Source              string                `json:"source"`
	Status              string                `json:"status"`
	Units               int                   `json:"units"`
	Legs                []PositionStrategyLeg `json:"legs"`
	PositionFingerprint string                `json:"position_fingerprint"`
	GuaranteedCombo     bool                  `json:"guaranteed_combo"`
	Actionable          bool                  `json:"actionable"`
	Reason              string                `json:"reason,omitempty"`
}

// PositionStrategyLeg allocates an exact current position to one strategy.
// Ratio is positive for a held long leg and negative for a held short leg.
type PositionStrategyLeg struct {
	Contract ContractParams `json:"contract"`
	Quantity float64        `json:"quantity"`
	Ratio    int            `json:"ratio"`
}

// StrategyGroupingIssue explains why current option legs remain standalone.
type StrategyGroupingIssue struct {
	Underlying string `json:"underlying"`
	LegCount   int    `json:"leg_count"`
	Reason     string `json:"reason"`
}

// PositionsPortfolio is the daemon-side aggregator across all open legs.
// it with the AccountResult.CurrencyExposure FX rate.
// DailyTheta is Σ (theta × signed contract qty × multiplier). IBKR
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
	FXSensitivityPerPct *float64 `json:"fx_sensitivity_per_pct,omitempty"`
	FXBaseCurrency      string   `json:"fx_base_currency,omitempty"`
}

// PositionGroup aggregates the stock leg (if any) and option legs per
// names because they are local/security-currency sums across all legs. *_base
// fields are filled only when every contributing row can be converted to the
// account base currency. GroupEffectiveDelta / GroupDollarDelta are coherent
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
// Legacy scalar fields remain float64 for wire compatibility. Authority.Fields
// is the source of truth for whether each one was observed: an available field
// with value zero is a genuine zero, while an unavailable field's numeric zero
// is only the Go zero value and must render as missing.
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
	DataType string    `json:"data_type,omitempty"`
	AsOf     time.Time `json:"as_of"`
	// Authority carries the concrete account/mode, producer, freshness, and
	Authority *AccountDataAuthority `json:"authority,omitempty"`
}

// DailyPnLObservation is the value-free health record for the account Daily
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
// units 1 unit of the named currency converts to" — matches IBKR's
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
// spot × IV × √(DTE/365). Populated only when IV and spot are both
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

// BackgroundTaskStatus names a daemon-internal long-running task that
// idle/ready/cold are omitted entirely, keeping the wire payload
// forward-compatible by design.
type BackgroundTaskStatus struct {
	// Name is a stable token identifying the task. Stable across
	Name       string    `json:"name"`
	Status     string    `json:"status,omitempty"`
	Scope      string    `json:"scope,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	EtaSeconds int       `json:"eta_seconds,omitempty"`
	Progress   int       `json:"progress,omitempty"`
}

// DataWarning is the common structured warning shape used by price-level
type DataWarning struct {
	Code     string `json:"code"`
	Scope    string `json:"scope,omitempty"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
	Impact   string `json:"impact,omitempty"`
	Action   string `json:"action,omitempty"`
}

// SubsystemHealth is a compact status.health diagnostic for tool families
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

// BackendLinkHealth counts TWS↔IBKR upstream-link losses (code 1100) and the
// paired restore outcomes (1101/1102) since the daemon's broker connection
// started. Durations are whole seconds.
type BackendLinkHealth struct {
	Down                 bool      `json:"down"`
	ChangedAt            time.Time `json:"changed_at,omitzero"`
	Losses               int       `json:"losses"`
	LastOutageSeconds    int64     `json:"last_outage_seconds,omitempty"`
	LongestOutageSeconds int64     `json:"longest_outage_seconds,omitempty"`
}

// MarketDataAccessHealth reports one route key the gateway is currently
// This is an observation of a time-windowed rejection, never an entitlement
// rejection for a name the account does hold, and a name never requested during
// deliberately absent: broker free text is untrusted and never reaches a typed
type MarketDataAccessHealth struct {
	// RouteKey is the connector's own subscription key — a bare symbol, or
	RouteKey string `json:"route_key"`
	// Symbol is RouteKey's leading symbol component, for display.
	Symbol     string    `json:"symbol,omitempty"`
	Code       int       `json:"code"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observed_at,omitzero"`
	// RetryAt is when the suppression window lifts and the next request for
	// this key reaches the gateway again.
	RetryAt time.Time `json:"retry_at,omitzero"`
}

// Gateway phases distinguish the local TWS/Gateway API socket from the
// gateway's own upstream broker link. Connected remains the compatibility
// authority and must not be inferred from LastError prose.
const (
	GatewayPhaseConnecting      = "connecting"
	GatewayPhasePortDown        = "port_down"
	GatewayPhaseAPINotReady     = "api_not_ready"
	GatewayPhaseBackendLinkDown = "backend_link_down"
	GatewayPhaseReady           = "ready"
)

// Market-data access reasons classify a rejection by IBKR code alone.
const (
	MarketDataAccessNotSubscribed = "not_subscribed"
	MarketDataAccessRejected      = "rejected"
)

// MarketDataAccessReason maps an IBKR rejection code to its typed reason.
func MarketDataAccessReason(code int) string {
	if code == 354 {
		return MarketDataAccessNotSubscribed
	}
	return MarketDataAccessRejected
}

// Account modes classify the connected broker account; they do not describe
// market-data freshness or grant broker-write authority.
const (
	AccountModeUnknown = "unknown"
	AccountModePaper   = "paper"
	AccountModeLive    = "live"
)

// HealthResult is the response to MethodStatusHealth.
// ports that responded during discovery but lost the first-hit race.
type HealthResult struct {
	DaemonVersion string    `json:"daemon_version"`
	DaemonStarted time.Time `json:"daemon_started"`
	UptimeSeconds int64     `json:"uptime_seconds"`
	Account       string    `json:"account,omitempty"`
	// ConnectedAccount is the one account the connected TWS/Gateway session is
	// scoped to, never a list. It is the code the session advertises via
	// managedAccounts / accountSummary when that is a single account, and the
	// configured [gateway].account pin when the login holds several and the
	// is one concrete account — an unpinned multi-account login has no account
	// to name. It differs from Account when [gateway].account is empty and the
	// daemon auto-detected the account after handshake. The session's full
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
	// GatewayPhase classifies which connectivity boundary currently blocks
	// local API session is ready while TWS reports its IBKR backend link lost.
	GatewayPhase   string    `json:"gateway_phase"`
	GatewayPhaseAt time.Time `json:"gateway_phase_at,omitzero"`
	// BackendLink summarizes TWS↔IBKR upstream-link flapping since the daemon
	// connected: loss count plus last/longest outage. Present once at least
	// one loss was observed or the link is currently down, so chronic
	// flapping is one counted observation instead of scattered warnings.
	BackendLink   *BackendLinkHealth `json:"backend_link,omitempty"`
	DataType      string             `json:"data_type,omitempty"`
	ServerVersion int                `json:"server_version,omitempty"`
	LastError     string             `json:"last_error,omitempty"`
	// BackgroundTasks lists daemon-internal long-running computes that
	// is active. Always present on the wire (never omitted) so
	BackgroundTasks []BackgroundTaskStatus `json:"background_tasks"`
	Subsystems      []SubsystemHealth      `json:"subsystems,omitempty"`
	DataQuality     []DataQualityHealth    `json:"data_quality,omitempty"`
	DataFarms       []DataFarmHealth       `json:"data_farms,omitempty"`
	// MarketDataAccess lists route keys the gateway is currently refusing
	// market data for. Empty is the normal case and the only claim absence
	MarketDataAccess []MarketDataAccessHealth `json:"market_data_access,omitempty"`
	// Members carries the runtime SPX-membership state: source
	// know yet (engine construction failed); the CLI hides the row
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
type TradingStatus struct {
	Mode           string           `json:"mode"`
	Endpoint       string           `json:"endpoint,omitempty"`
	GatewayHost    string           `json:"gateway_host,omitempty"`
	GatewayPort    int              `json:"gateway_port,omitempty"`
	PortOrigin     string           `json:"port_origin,omitempty"`
	Account        string           `json:"account,omitempty"`
	AccountOrigin  string           `json:"account_origin,omitempty"`
	ClientID       int              `json:"client_id,omitempty"`
	ClientIDOrigin string           `json:"client_id_origin,omitempty"`
	MCPTrading     string           `json:"mcp_trading"`
	CanPreview     bool             `json:"can_preview"`
	CanWrite       bool             `json:"can_write"`
	WriteBlockers  []TradingBlocker `json:"write_blockers,omitempty"`
	OpenOrders     int              `json:"open_orders,omitempty"`
	LastOrderEvent string           `json:"last_order_event,omitempty"`
	LiveOverride   string           `json:"live_override,omitempty"`
	Blocked        bool             `json:"blocked"`
	Blockers       []TradingBlocker `json:"blockers,omitempty"`
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
	// OrderTokenScopeExercise binds a preview token to one reduce-only option exercise.
	OrderTokenScopeExercise = "exercise"
	// OrderTokenScopeStrategy binds a preview token to one proportional close
	// or reduction of a daemon-owned strategy group.
	OrderTokenScopeStrategy = "strategy"

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
	// ResolvedStrategy is daemon-internal. External RPC callers can identify a
	// strategy only through StrategyPreviewParams and cannot author combo legs.
	ResolvedStrategy *StrategyOrderDraft `json:"-"`
}

// StrategyPreviewParams requests one constrained group close or reduction.
// The daemon resolves ID and revision against current positions and constructs
// every combo leg; adapters never submit client-authored legs.
type StrategyPreviewParams struct {
	StrategyID       string   `json:"strategy_id"`
	ExpectedRevision int64    `json:"expected_revision"`
	Operation        string   `json:"operation"`
	Units            int      `json:"units,omitempty"`
	LimitPrice       *float64 `json:"limit_price,omitempty"`
	TIF              string   `json:"tif,omitempty"`
	TimeoutMs        int      `json:"timeout_ms,omitempty"`
	Source           string   `json:"source,omitempty"`
}

// StrategyOrderDraft is the complete group operation bound into an order
// preview token and journaled with the parent BAG order.
type StrategyOrderDraft struct {
	StrategyID          string             `json:"strategy_id"`
	StrategyRevision    int64              `json:"strategy_revision"`
	PositionFingerprint string             `json:"position_fingerprint"`
	Operation           string             `json:"operation"`
	Units               int                `json:"units"`
	UnitsBefore         int                `json:"units_before"`
	UnitsAfter          int                `json:"units_after"`
	GuaranteedCombo     bool               `json:"guaranteed_combo"`
	Legs                []StrategyOrderLeg `json:"legs"`
}

// StrategyOrderLeg records one proportional before/after position change.
type StrategyOrderLeg struct {
	Contract ContractParams `json:"contract"`
	Ratio    int            `json:"ratio"`
	Action   string         `json:"action"`
	Quantity int            `json:"quantity"`
	Before   float64        `json:"before"`
	After    float64        `json:"after"`
}

// OrderDraft is the canonical local intent bound into a preview token.
type OrderDraft struct {
	Action        string              `json:"action"`
	Contract      ContractParams      `json:"contract"`
	Quantity      int                 `json:"quantity"`
	OrderType     string              `json:"order_type"`
	LimitPrice    float64             `json:"limit_price"`
	Trail         *OrderTrailSpec     `json:"trail,omitempty"`
	TriggerMethod int                 `json:"trigger_method,omitempty"`
	TIF           string              `json:"tif"`
	OutsideRTH    bool                `json:"outside_rth"`
	Strategy      string              `json:"strategy"`
	OrderRef      string              `json:"order_ref"`
	OpenClose     string              `json:"open_close,omitempty"`
	Source        string              `json:"source,omitempty"`
	StrategyGroup *StrategyOrderDraft `json:"strategy_group,omitempty"`
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
	Before      float64 `json:"before"`
	After       float64 `json:"after"`
	Effect      string  `json:"effect"`
	AverageCost float64 `json:"average_cost,omitempty"`
	Multiplier  int     `json:"multiplier,omitempty"`
}

// OrderMarginImpact is populated from IBKR WhatIf once the raw preview-only
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
type OrderCancelParams struct {
	ID        string `json:"id"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	// Origin is journaled for audit. Cancel is exempt from the live
	// agent-origin block: refusing a cancel can never reduce risk less than
	// protection — see SECURITY.md.
	Origin string `json:"origin,omitempty"`
}

// OrderPlaceResult reports the durable local and broker-send state of a gated
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
// "network_failed", "parse_failed", "disabled (config)", "disabled
type MembersHealth struct {
	Source       string    `json:"source"`
	AsOf         time.Time `json:"as_of"`
	Count        int       `json:"count"`
	RefreshState string    `json:"refresh_state"`
}
