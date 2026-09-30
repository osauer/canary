package rpc

import "time"

// Proposal readiness codes. Readiness names, in one owner-facing code, whether
// a proposal row or a refused preview, prepare or submit could be sent now
// and, when it cannot, what it waits for. It is derived evidence for display
// and for queued authorisation; it never grants or revokes order authority,
// and every preview and broker-write gate still decides at send time.
const (
	// ReadinessReady means nothing Canary knows of stops a preview now.
	ReadinessReady = "ready"
	// ReadinessMarketClosed means the market's regular session is closed and
	// the only blockers are the ones a closed session causes.
	ReadinessMarketClosed = "market_closed"
	// ReadinessOpeningWindow means the session opened less than the opening
	// offset ago; a preview can run, but the default send time is later.
	ReadinessOpeningWindow = "opening_window"
	// ReadinessQuoteUnusable means no live, fresh, two-sided quote is at hand.
	ReadinessQuoteUnusable = "quote_unusable"
	// ReadinessSpreadTooWide means the quoted spread exceeds its policy limit.
	ReadinessSpreadTooWide = "spread_too_wide"
	// ReadinessHalted means a regulatory, news or LULD halt is active.
	ReadinessHalted = "halted"
	// ReadinessBrokerUnavailable means the broker link or session is not ready.
	ReadinessBrokerUnavailable = "broker_unavailable"
	// ReadinessTradingFrozen means the runtime trading freeze stops writes.
	ReadinessTradingFrozen = "trading_frozen"
	// ReadinessNotExecutable covers every other blocker; Message carries
	// Canary's own sentence for it.
	ReadinessNotExecutable = "not_executable"
)

// Readiness session states: the regular session of the readiness market at
// the instant readiness was computed.
const (
	ReadinessSessionOpen       = "open"
	ReadinessSessionPreOpen    = "pre_open"
	ReadinessSessionBreak      = "break"
	ReadinessSessionAfterClose = "after_close"
	ReadinessSessionClosed     = "closed"
	ReadinessSessionHoliday    = "holiday"
	ReadinessSessionUnknown    = "unknown"
)

// TradeProposalReadiness classifies a proposal row or a refusal against the
// regular session of its market. It is served at read time and never
// persisted, so it cannot change a proposal revision.
type TradeProposalReadiness struct {
	Code string `json:"code"`
	// Market and MarketLabel name the official calendar of the contract's
	// venue (us_options, us_equity, de_xetra, ...); both are empty when
	// Canary has no calendar for the contract.
	Market       string `json:"market,omitempty"`
	MarketLabel  string `json:"market_label,omitempty"`
	SessionState string `json:"session_state"`
	// OpensAt is the regular open of the session readiness refers to, in
	// UTC: the next open while the market is closed, the current session's
	// open while it is open. Omitted when the calendar cannot date it.
	OpensAt *time.Time `json:"opens_at,omitempty"`
	// DefaultSendAt is OpensAt plus the opening offset (15 minutes for
	// options, 30 for a discretionary-scale options row at a stress open, 5
	// for stocks): the earliest time a queued authorisation would send. Set
	// only for market_closed and opening_window.
	DefaultSendAt *time.Time `json:"default_send_at,omitempty"`
	// StressOpen reports that DefaultSendAt is dated at a stress open: the
	// latched regime stage in force reads confirmed stress, so a
	// discretionary-scale options row (budget_reduction, theta_hygiene,
	// risk_reduction, cash_sweep) waits 30 minutes after the open, and
	// Message says so with the time.
	StressOpen bool `json:"stress_open,omitempty"`
	// StressOpenExempt reports that the regime reads confirmed stress but
	// the row is not discretionary-scale (a loss exit, expiry close or
	// trailing stop among others), so it keeps the 15-minute options offset,
	// and Message says so with the time.
	StressOpenExempt bool `json:"stress_open_exempt,omitempty"`
	// Queueable reports that the row's kind and state fit a queued
	// authorisation: a governor, theta or issuer-trim row waiting for the open
	// or its opening window. No queue exists yet; nothing is ever queued from
	// this flag.
	Queueable bool `json:"queueable"`
	// CanaryCodes lists every blocker code the classification read, in order.
	CanaryCodes []string `json:"canary_codes,omitempty"`
	// Message is Canary's own sentence for the deciding blocker, if one decided.
	Message string    `json:"message,omitempty"`
	AsOf    time.Time `json:"as_of"`
}
