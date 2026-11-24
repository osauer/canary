package rpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// MethodMarketRead reads what the current US cash session is doing against
// the previous one: the S&P 500 index against the prior session's range, SPY
// volume against its usual pace, VIX against its line, and the large-company
// leaders against their opening gaps, with tags that name a condition only
// once it holds. Descriptive and read-only; it feeds no decision.
const MethodMarketRead = "market.read"

// ValidateMarketReadParams permits no caller-supplied symbols or levels.
func ValidateMarketReadParams(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var empty struct{}
	if err := d.Decode(&empty); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("one empty market read object is required")
	}
	return nil
}

// The session states of a market read.
const (
	MarketReadPre    = "pre"
	MarketReadOpen   = "open"
	MarketReadClosed = "closed"
)

// MarketReadResult is one read of the US cash session. A value that could not
// be measured is absent and its *_unavailable field says why; nothing is
// zero-filled or carried from another session.
type MarketReadResult struct {
	AsOf               time.Time            `json:"as_of"`
	Session            MarketReadSession    `json:"session"`
	Thresholds         MarketReadThresholds `json:"thresholds"`
	Index              *MarketReadIndex     `json:"index,omitempty"`
	IndexUnavailable   string               `json:"index_unavailable,omitempty"`
	VIX                *MarketReadVIX       `json:"vix,omitempty"`
	VIXUnavailable     string               `json:"vix_unavailable,omitempty"`
	Leaders            *MarketReadLeaders   `json:"leaders,omitempty"`
	LeadersUnavailable string               `json:"leaders_unavailable,omitempty"`
	// Tags name the conditions that hold at the session's latest completed
	// bar: now while it is open, at its last bar once it has closed; none
	// before the open.
	Tags []MarketReadTag `json:"tags"`
	// TagEvents is the timeline of the bar-based tags (index, volume, vix)
	// over the session, oldest first.
	TagEvents []MarketReadTagEvent `json:"tag_events"`
}

// MarketReadTagEvent is one unbroken run of a bar-based tag: From is the
// close of its first bar, To the close of the bar that ended it, absent while
// the run still stands at the latest bar.
type MarketReadTagEvent struct {
	Code  string    `json:"code"`
	Scope string    `json:"scope"`
	Tone  string    `json:"tone"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to,omitzero"`
}

// MarketReadSession is the official US cash session the read describes:
// today's, or on a day without one, the last that traded.
type MarketReadSession struct {
	Date  string    `json:"date"`
	State string    `json:"state"`
	Open  time.Time `json:"open,omitzero"`
	Close time.Time `json:"close,omitzero"`
}

// MarketReadThresholds are the levels the tags are drawn at. They are
// heuristics of this read, not policy, and are returned so a reader can see
// what a tag rests on.
type MarketReadThresholds struct {
	Heuristic bool `json:"heuristic"`
	// BarMinutes is the bar length; HoldBars completed closes beyond a level
	// make it hold.
	BarMinutes int `json:"bar_minutes"`
	HoldBars   int `json:"hold_bars"`
	// StaleAfterMinutes bounds the age of the bars' last fetch during the
	// session; older bars draw no tags.
	StaleAfterMinutes int `json:"stale_after_minutes"`
	// BaselineMinSessions prior sessions must share a volume bucket for it to
	// have a baseline.
	BaselineMinSessions int `json:"baseline_min_sessions"`
	// Index tags.
	ContinuationRelativeVolume float64 `json:"continuation_relative_volume"`
	ThinRelativeVolume         float64 `json:"thin_relative_volume"`
	// Volume tags compare the last LookbackMinutes with their baseline.
	LookbackMinutes      int     `json:"lookback_minutes"`
	FadingWindowVolume   float64 `json:"fading_window_volume"`
	FadingRelativeVolume float64 `json:"fading_relative_volume"`
	SellingWindowVolume  float64 `json:"selling_window_volume"`
	VIXLine              float64 `json:"vix_line"`
	VIXRisingPct         float64 `json:"vix_rising_pct"`
	VIXBand              float64 `json:"vix_band"`
	GapMinPct            float64 `json:"gap_min_pct"`
	GapHoldFraction      float64 `json:"gap_hold_fraction"`
	GapAdvanceFraction   float64 `json:"gap_advance_fraction"`
	LeadersMinGapped     int     `json:"leaders_min_gapped"`
	LeadersHoldShare     float64 `json:"leaders_hold_share"`
	LeadersFadeShare     float64 `json:"leaders_fade_share"`
}

// MarketReadBar is one completed regular-session bar.
type MarketReadBar struct {
	At    time.Time `json:"at"`
	Open  float64   `json:"open"`
	High  float64   `json:"high"`
	Low   float64   `json:"low"`
	Close float64   `json:"close"`
}

// MarketReadPrior is the prior regular session's daily range.
type MarketReadPrior struct {
	Date  string  `json:"date"`
	High  float64 `json:"high"`
	Low   float64 `json:"low"`
	Close float64 `json:"close"`
}

// MarketReadIndex is the S&P 500 index against its prior session.
type MarketReadIndex struct {
	Symbol              string            `json:"symbol"`
	Prior               *MarketReadPrior  `json:"prior,omitempty"`
	PriorUnavailable    string            `json:"prior_unavailable,omitempty"`
	Last                *float64          `json:"last,omitempty"`
	LastAt              time.Time         `json:"last_at,omitzero"`
	LastUnavailable     string            `json:"last_unavailable,omitempty"`
	Bars                []MarketReadBar   `json:"bars"`
	BarsUnavailable     string            `json:"bars_unavailable,omitempty"`
	Position            string            `json:"position,omitempty"`
	PositionUnavailable string            `json:"position_unavailable,omitempty"`
	Volume              *MarketReadVolume `json:"volume,omitempty"`
	VolumeUnavailable   string            `json:"volume_unavailable,omitempty"`
}

// MarketReadVolumeBar is one completed bucket of the volume proxy. Baseline
// is the mean of the same bucket over the prior sessions recorded.
type MarketReadVolumeBar struct {
	At       time.Time `json:"at"`
	Volume   int64     `json:"volume"`
	Baseline *float64  `json:"baseline,omitempty"`
}

// MarketReadVolume is the index's volume, read from an ETF that tracks it:
// an index bar carries none. Relative compares today's completed buckets with
// the same buckets of the prior sessions, so it means the same at any time of
// day.
type MarketReadVolume struct {
	Proxy               string                `json:"proxy"`
	Bars                []MarketReadVolumeBar `json:"bars"`
	Cumulative          int64                 `json:"cumulative"`
	BaselineCumulative  *float64              `json:"baseline_cumulative,omitempty"`
	Relative            *float64              `json:"relative,omitempty"`
	RelativeUnavailable string                `json:"relative_unavailable,omitempty"`
	// BaselineSessions is the fewest prior sessions any of today's buckets
	// rests on.
	BaselineSessions int `json:"baseline_sessions"`
}

// MarketReadVIXBar is one completed VIX bar.
type MarketReadVIXBar struct {
	At    time.Time `json:"at"`
	Close float64   `json:"close"`
}

// MarketReadVIX is VIX against its line and its prior close.
type MarketReadVIX struct {
	Last                  *float64           `json:"last,omitempty"`
	LastAt                time.Time          `json:"last_at,omitzero"`
	LastUnavailable       string             `json:"last_unavailable,omitempty"`
	PriorClose            *float64           `json:"prior_close,omitempty"`
	PriorCloseUnavailable string             `json:"prior_close_unavailable,omitempty"`
	ChangePct             *float64           `json:"change_pct,omitempty"`
	Line                  float64            `json:"line"`
	Bars                  []MarketReadVIXBar `json:"bars"`
	BarsUnavailable       string             `json:"bars_unavailable,omitempty"`
	Position              string             `json:"position,omitempty"`
}

// The leader states.
const (
	LeaderHoldsGapUp       = "holds_gap_up"
	LeaderGapUpFilled      = "gap_up_filled"
	LeaderHoldsGapDown     = "holds_gap_down"
	LeaderGapDownReclaimed = "gap_down_reclaimed"
	LeaderBelowPriorLow    = "below_prior_low"
	LeaderNoGap            = "no_gap"
)

// MarketReadLeader is one large company against its opening gap.
type MarketReadLeader struct {
	Symbol     string   `json:"symbol"`
	PriorClose *float64 `json:"prior_close,omitempty"`
	PriorLow   *float64 `json:"prior_low,omitempty"`
	Open       *float64 `json:"open,omitempty"`
	// OpenSource says where the open came from: the session's opening tick or
	// the first regular-session bar.
	OpenSource       string    `json:"open_source,omitempty"`
	Last             *float64  `json:"last,omitempty"`
	LastAt           time.Time `json:"last_at,omitzero"`
	GapPct           *float64  `json:"gap_pct,omitempty"`
	State            string    `json:"state,omitempty"`
	StateUnavailable string    `json:"state_unavailable,omitempty"`
}

// MarketReadLeaders reads one share line per company of a fixed basket.
// GappedUp counts the companies that opened up by at least the gap minimum
// and have a state; HoldingGapUp those of them that hold it.
type MarketReadLeaders struct {
	Method       string             `json:"method"`
	Rows         []MarketReadLeader `json:"rows"`
	GappedUp     int                `json:"gapped_up"`
	HoldingGapUp int                `json:"holding_gap_up"`
}

// MarketReadTag names one condition that holds. Since is when it began to
// hold without a break, as far as the session's bars show; Evidence carries
// the numbers it rests on.
type MarketReadTag struct {
	Code             string             `json:"code"`
	Scope            string             `json:"scope"`
	Tone             string             `json:"tone"`
	Since            time.Time          `json:"since,omitzero"`
	SinceUnavailable string             `json:"since_unavailable,omitempty"`
	Evidence         map[string]float64 `json:"evidence"`
}
