// Package spx computes S&P 500 breadth measurements locally from a validated
// constituent universe and daily closes obtained through the daemon's broker
// the cold fallback; JSON file paths remain only for explicit legacy import and
package spx

import "time"

// Method is the methodology token stamped on every snapshot so renderers
// The token changes whenever the compute methodology or snapshot payload shape
// becomes incompatible. LoadSnapshot treats a mismatch as a cold start rather
const methodConstituentFanout = "constituent-fanout-50/200dma+nh-v2"

// MethodConstituentFanout is the exported form of the current breadth
// methodology token for daemon wire envelopes and documentation.
const MethodConstituentFanout = methodConstituentFanout

// MinCoverageFraction is the minimum fraction of MemberCount that a
// refresh must cover before the engine will persist its result.
// — typical causes: a connector-not-ready race at cold-start (where
// rejecting catastrophic fan-out failures.
const MinCoverageFraction = 0.80

// RefreshFailure is a redacted machine-readable reason for the latest breadth
// refresh problem. Raw broker and transport errors remain local logs.
type RefreshFailure string

// Breadth refresh failure values keep raw broker and storage text local.
const (
	RefreshFailureFetch     RefreshFailure = "fetch_failed"
	RefreshFailurePersist   RefreshFailure = "persist_failed"
	RefreshFailureCancelled RefreshFailure = "cancelled"
)

// RefreshProgress is the current or most recently completed fan-out attempt.
// Processed includes both successful and failed symbol fetches; Total is the
type RefreshProgress struct {
	SessionKey  string
	StartedAt   time.Time
	Processed   int
	Total       int
	Deadline    time.Time
	LastFailure RefreshFailure
}

// WindowSize is the 50-day SMA lookback (S&P DJI's S5FI is the
const WindowSize = 50

// WindowSize200 is the 200-day SMA lookback ($SPXA200R). Catches
// (IBKR's pacing limit is per-request, not per-bar; pulling 200 days
const WindowSize200 = 200

// RollingMaxBars is the lookback for the per-constituent rolling max/min
const RollingMaxBars = 252

// Snapshot is one breadth reading: the computed values, represented trading
type Snapshot struct {
	// Value is the 50-DMA reading: percentage of constituents trading
	Value float64 `json:"value"`
	// PctAbove50DMA is the 50-day reading exposed under the canonical
	// alongside Value so the wire shape is self-documenting.
	PctAbove50DMA float64 `json:"pct_above_50dma"`
	// PctAbove200DMA is the 200-day reading. Below 40% = red /
	PctAbove200DMA float64 `json:"pct_above_200dma"`
	// NewHighsToday is the count of S&P 500 constituents whose latest
	NewHighsToday int `json:"new_highs_today"`
	// NewLowsToday is the symmetric count for new 252-bar lows.
	NewLowsToday int `json:"new_lows_today"`
	// NetNewHighsPct is (NewHighs - NewLows) / coverage × 100. A
	NetNewHighsPct float64 `json:"net_new_highs_pct"`
	// AsOf is the wall-clock instant the compute finished. Distinct
	AsOf time.Time `json:"as_of"`
	// SessionKey is the New-York date of the trading session the
	SessionKey string `json:"session_key"`
	// Method is a stable token identifying the compute methodology.
	Method string `json:"method"`
	// MemberCount is the size of the membership list used in the
	MemberCount int `json:"member_count"`
	// Coverage is the count of members that had enough 50-DMA history
	Coverage int `json:"coverage"`
	// Coverage200 is the analogous denominator for PctAbove200DMA —
	Coverage200 int `json:"coverage_200"`
	// CoverageHighsLows is the denominator for the new-highs/lows
	CoverageHighsLows int `json:"coverage_highs_lows"`
	// Excluded lists members dropped from the compute and the reason
	Excluded []ExcludedMember `json:"excluded,omitempty"`
}

// ExcludedMember explains why a constituent did not contribute to the
type ExcludedMember struct {
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

// ConstituentWindow holds the sliding window of daily closes for one
type ConstituentWindow struct {
	Symbol    string    `json:"symbol"`
	Closes    []float64 `json:"closes"`
	LastBarAt string    `json:"last_bar_at"`
	// HighWindow is the trailing 252-bar rolling max of close
	HighRollingMax     float64 `json:"high_rolling_max,omitempty"`
	HighRollingBarsHad int     `json:"high_rolling_bars_had,omitempty"`
	LowRollingMin      float64 `json:"low_rolling_min,omitempty"`
	LowRollingBarsHad  int     `json:"low_rolling_bars_had,omitempty"`
}

// WindowSet is the versioned persistence shape for constituent windows. An
// incompatible Version is treated as no state and triggers a cold rebuild.
type WindowSet struct {
	Version int                          `json:"version"`
	AsOf    time.Time                    `json:"as_of"`
	Windows map[string]ConstituentWindow `json:"windows"`
}

// CurrentWindowSetVersion is the constituent-window schema version written by
const CurrentWindowSetVersion = 2

// HistoryPoint is one session's breadth reading in rolling history. The
type HistoryPoint struct {
	Date           string  `json:"date"`
	PctAbove50DMA  float64 `json:"pct_above_50dma"`
	PctAbove200DMA float64 `json:"pct_above_200dma,omitempty"`
	NewHighs       int     `json:"new_highs,omitempty"`
	NewLows        int     `json:"new_lows,omitempty"`
}

// HistorySet is the versioned rolling-history persistence shape. Points are
type HistorySet struct {
	Version int            `json:"version"`
	Points  []HistoryPoint `json:"points"`
}

// CurrentHistorySetVersion is the history schema version written by the engine.
const CurrentHistorySetVersion = 2

// MaxHistoryPoints caps how many days of S5FI history the engine retains. The
const MaxHistoryPoints = 60
