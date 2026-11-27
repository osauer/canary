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
	// Held reads the underlyings of the account's current stock and option
	// positions against their own prior session. It makes the read
	// account-scoped: it names held underlyings, never quantities, values or
	// account ids.
	Held *MarketReadHeld `json:"held,omitempty"`
	// Indicated is what pre-market prices indicate for the open, in state pre
	// only. Nothing in it has held: pre-market trade is thin and a price
	// before the open is an indication, not a level the session has reached.
	Indicated *MarketReadIndicated `json:"indicated,omitempty"`
	// Indications name what the pre-market prices indicate; they are not tags
	// and nothing in them has held. Empty outside state pre.
	Indications []MarketReadIndication `json:"indications"`
	// Relative is the Nasdaq 100 beside the S&P 500 while the session is open,
	// from brief quote looks.
	Relative            *MarketReadRelative `json:"relative,omitempty"`
	RelativeUnavailable string              `json:"relative_unavailable,omitempty"`
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
	// NearLowPct is how far above its prior low, in percent, an indicated
	// price counts as near it; NearLowShare the share of quoted leaders near
	// or below their prior lows that raises leaders_near_prior_low.
	NearLowPct   float64 `json:"near_low_pct"`
	NearLowShare float64 `json:"near_low_share"`
	// RelativeGapPct is how many percentage points the Nasdaq's change must
	// differ from the S&P 500's for a relative indication or tag.
	RelativeGapPct float64 `json:"relative_gap_pct"`
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

// A held stock's price against the prior session's range, as the market
// read's held rows and the level replay (edge.levels) both read it.
const (
	MarketReadAbovePriorHigh = "above_prior_high"
	MarketReadInsidePrior    = "inside"
	MarketReadBelowPriorLow  = "below_prior_low"
)

// MarketReadHeldPosition places last against the prior session's range: above
// its high, under its low, else inside (the boundaries count as inside).
func MarketReadHeldPosition(last float64, prior MarketReadPrior) string {
	switch {
	case last > prior.High:
		return MarketReadAbovePriorHigh
	case last < prior.Low:
		return MarketReadBelowPriorLow
	}
	return MarketReadInsidePrior
}

// MarketReadDayMid is the midpoint of a session's range so far, the held
// row's day_mid and the level rule's first trim level.
func MarketReadDayMid(high, low float64) float64 { return (high + low) / 2 }

// MarketReadIndex is the S&P 500 index against its prior session.
type MarketReadIndex struct {
	Symbol           string           `json:"symbol"`
	Prior            *MarketReadPrior `json:"prior,omitempty"`
	PriorUnavailable string           `json:"prior_unavailable,omitempty"`
	Last             *float64         `json:"last,omitempty"`
	LastAt           time.Time        `json:"last_at,omitzero"`
	LastUnavailable  string           `json:"last_unavailable,omitempty"`
	// LastSource says what Last is: trade (with the broker's trade time),
	// quote (a current indication, timed at receipt), prior_close (before
	// the open, the prior session's close at its close time) or bar (the
	// latest completed bar's close).
	LastSource      string          `json:"last_source,omitempty"`
	Bars            []MarketReadBar `json:"bars"`
	BarsUnavailable string          `json:"bars_unavailable,omitempty"`
	// BarsAsOf is when the bars were read. BarsStale says they were read
	// longer ago than stale_after_minutes while the session is open: they are
	// served for the chart, draw no tags, and BarsStaleReason says why.
	BarsAsOf            time.Time         `json:"bars_as_of,omitzero"`
	BarsStale           bool              `json:"bars_stale,omitempty"`
	BarsStaleReason     string            `json:"bars_stale_reason,omitempty"`
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
	// BarsAsOf is when the bars were read. BarsStale says they were read
	// longer ago than stale_after_minutes while the session is open: they are
	// served for the chart, draw no tags, and BarsStaleReason says why.
	BarsAsOf        time.Time `json:"bars_as_of,omitzero"`
	BarsStale       bool      `json:"bars_stale,omitempty"`
	BarsStaleReason string    `json:"bars_stale_reason,omitempty"`
}

// MarketReadVIXBar is one completed VIX bar.
type MarketReadVIXBar struct {
	At    time.Time `json:"at"`
	Close float64   `json:"close"`
}

// MarketReadVIX is VIX against its line and its prior close.
type MarketReadVIX struct {
	Last            *float64  `json:"last,omitempty"`
	LastAt          time.Time `json:"last_at,omitzero"`
	LastUnavailable string    `json:"last_unavailable,omitempty"`
	// LastSource says what Last is: trade (with the broker's trade time),
	// quote (a current indication, timed at receipt), prior_close (before
	// the open, the prior session's close at its close time) or bar (the
	// latest completed bar's close).
	LastSource            string             `json:"last_source,omitempty"`
	PriorClose            *float64           `json:"prior_close,omitempty"`
	PriorCloseUnavailable string             `json:"prior_close_unavailable,omitempty"`
	ChangePct             *float64           `json:"change_pct,omitempty"`
	Line                  float64            `json:"line"`
	Bars                  []MarketReadVIXBar `json:"bars"`
	BarsUnavailable       string             `json:"bars_unavailable,omitempty"`
	// BarsAsOf is when the bars were read. BarsStale says they were read
	// longer ago than stale_after_minutes while the session is open: they are
	// served for the chart, draw no tags, and BarsStaleReason says why.
	BarsAsOf        time.Time `json:"bars_as_of,omitzero"`
	BarsStale       bool      `json:"bars_stale,omitempty"`
	BarsStaleReason string    `json:"bars_stale_reason,omitempty"`
	Position        string    `json:"position,omitempty"`
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
	// PriorCloseSource names where the prior close came from: the prior
	// session's daily bar, or the quote's previous close when that bar is
	// not retained.
	PriorCloseSource string   `json:"prior_close_source,omitempty"`
	PriorLow         *float64 `json:"prior_low,omitempty"`
	Open             *float64 `json:"open,omitempty"`
	// OpenSource says where the open came from: the session's opening tick or
	// the first regular-session bar.
	OpenSource string    `json:"open_source,omitempty"`
	Last       *float64  `json:"last,omitempty"`
	LastAt     time.Time `json:"last_at,omitzero"`
	GapPct     *float64  `json:"gap_pct,omitempty"`
	// ChangePct is Last against the prior close, in percent: in the session
	// the current price, before the open a pre-market price of the same day
	// (absent when the price is still the prior session's), after the close
	// the session's close.
	ChangePct         *float64 `json:"change_pct,omitempty"`
	ChangeUnavailable string   `json:"change_unavailable,omitempty"`
	// IndicatedGapPct is, before the open, the pre-market price against the
	// prior close. A price that is still the prior close is no indication.
	IndicatedGapPct         *float64 `json:"indicated_gap_pct,omitempty"`
	IndicatedGapUnavailable string   `json:"indicated_gap_unavailable,omitempty"`
	State                   string   `json:"state,omitempty"`
	StateUnavailable        string   `json:"state_unavailable,omitempty"`
}

// MarketReadLeaders reads one share line per company of a fixed basket.
// GappedUp counts the companies that opened up by at least the gap minimum
// and have a state; HoldingGapUp those of them that hold it.
type MarketReadLeaders struct {
	Method       string             `json:"method"`
	Rows         []MarketReadLeader `json:"rows"`
	GappedUp     int                `json:"gapped_up"`
	HoldingGapUp int                `json:"holding_gap_up"`
	// BelowPriorLow counts the companies with a state under their prior
	// session's low, whatever their gap.
	BelowPriorLow int `json:"below_prior_low"`
	// GappedDown counts the companies that opened down by at least the gap
	// minimum and have a state; HoldingGapDown those of them that hold it.
	GappedDown     int `json:"gapped_down"`
	HoldingGapDown int `json:"holding_gap_down"`
}

// MarketReadHeld is one row per held underlying, in the order the positions
// list them, at most market.snapshot's cap; Truncated says the cap cut the
// list. RowsUnavailable says why there are no rows.
type MarketReadHeld struct {
	Rows            []MarketReadHeldRow `json:"rows"`
	RowsUnavailable string              `json:"rows_unavailable,omitempty"`
	Truncated       bool                `json:"truncated,omitempty"`
}

// MarketReadHeldRow is one held underlying against its prior regular session
// and today's range so far. Open, Last and State follow the leader rules,
// and a held leader carries the leader row's values.
type MarketReadHeldRow struct {
	Symbol           string           `json:"symbol"`
	Prior            *MarketReadPrior `json:"prior,omitempty"`
	PriorUnavailable string           `json:"prior_unavailable,omitempty"`
	// OpenSource is session_open (the opening tick), first_bar (the first
	// regular bar) or, after the close, daily_bar.
	Open       *float64 `json:"open,omitempty"`
	OpenSource string   `json:"open_source,omitempty"`
	// DayHigh and DayLow are today's regular session so far, DayMid their
	// midpoint. DaySource is bars (today's completed five-minute bars),
	// quote (the quote's day high and low) or, after the close, daily_bar
	// (the session's daily bar). While the session is open the range is
	// widened to the current price, which can lie past the last completed
	// bar.
	DayHigh        *float64  `json:"day_high,omitempty"`
	DayLow         *float64  `json:"day_low,omitempty"`
	DayMid         *float64  `json:"day_mid,omitempty"`
	DaySource      string    `json:"day_source,omitempty"`
	DayUnavailable string    `json:"day_unavailable,omitempty"`
	Last           *float64  `json:"last,omitempty"`
	LastAt         time.Time `json:"last_at,omitzero"`
	// LastSource is trade, quote, prior_close (before the open) or, after
	// the close, daily_bar (the session's close).
	LastSource      string `json:"last_source,omitempty"`
	LastUnavailable string `json:"last_unavailable,omitempty"`
	// ChangePct is Last against the prior close, in percent: in the session
	// the current price, before the open a pre-market price of the same day
	// (absent when the price is still the prior session's), after the close
	// the session's close.
	ChangePct         *float64 `json:"change_pct,omitempty"`
	ChangeUnavailable string   `json:"change_unavailable,omitempty"`
	// Position is Last against the prior range: above_prior_high, inside or
	// below_prior_low; absent before the open.
	Position            string `json:"position,omitempty"`
	PositionUnavailable string `json:"position_unavailable,omitempty"`
	// State is one of the leader states.
	State            string `json:"state,omitempty"`
	StateUnavailable string `json:"state_unavailable,omitempty"`
}

// MarketReadIndicated is what pre-market prices indicate for the open.
type MarketReadIndicated struct {
	AsOf               time.Time                   `json:"as_of"`
	Index              *MarketReadIndicatedIndex   `json:"index,omitempty"`
	IndexUnavailable   string                      `json:"index_unavailable,omitempty"`
	Nasdaq             *MarketReadIndicatedFuture  `json:"nasdaq,omitempty"`
	NasdaqUnavailable  string                      `json:"nasdaq_unavailable,omitempty"`
	Leaders            *MarketReadIndicatedLeaders `json:"leaders,omitempty"`
	LeadersUnavailable string                      `json:"leaders_unavailable,omitempty"`
}

// MarketReadIndicatedFuture is an index future's last price against its
// prior close. Source names the dated contract.
type MarketReadIndicatedFuture struct {
	Source     string    `json:"source"`
	Last       float64   `json:"last"`
	LastAt     time.Time `json:"last_at,omitzero"`
	PriorClose float64   `json:"prior_close"`
	ChangePct  float64   `json:"change_pct"`
}

// MarketReadIndicatedIndex is the S&P 500 future's change applied to the
// index's prior close, and where that level would open against the index's
// prior range.
type MarketReadIndicatedIndex struct {
	Source             string    `json:"source"`
	Last               float64   `json:"last"`
	LastAt             time.Time `json:"last_at,omitzero"`
	PriorClose         float64   `json:"prior_close"`
	ChangePct          float64   `json:"change_pct"`
	ImpliedLevel       *float64  `json:"implied_level,omitempty"`
	ImpliedPosition    string    `json:"implied_position,omitempty"`
	ImpliedUnavailable string    `json:"implied_unavailable,omitempty"`
}

// MarketReadIndicatedLeaders counts the leaders with a pre-market price by
// their indicated gap at the gap minimum, and against their prior lows.
type MarketReadIndicatedLeaders struct {
	Quoted        int `json:"quoted"`
	Up            int `json:"up"`
	Down          int `json:"down"`
	Flat          int `json:"flat"`
	NearPriorLow  int `json:"near_prior_low"`
	BelowPriorLow int `json:"below_prior_low"`
}

// MarketReadIndication names what pre-market prices indicate, with the
// numbers it rests on. It has not held and carries no since.
type MarketReadIndication struct {
	Code     string             `json:"code"`
	Scope    string             `json:"scope"`
	Tone     string             `json:"tone"`
	Evidence map[string]float64 `json:"evidence"`
}

// MarketReadRelative is the Nasdaq 100 beside the S&P 500 today.
type MarketReadRelative struct {
	Nasdaq                 *MarketReadRelativeLine `json:"nasdaq,omitempty"`
	NasdaqUnavailable      string                  `json:"nasdaq_unavailable,omitempty"`
	SP500ChangePct         *float64                `json:"sp500_change_pct,omitempty"`
	SP500ChangeUnavailable string                  `json:"sp500_change_unavailable,omitempty"`
	SpreadPct              *float64                `json:"spread_pct,omitempty"`
	SpreadUnavailable      string                  `json:"spread_unavailable,omitempty"`
}

// MarketReadRelativeLine is the Nasdaq 100's day change from a brief quote
// look: the index, or when it does not quote, QQQ, named in Source.
type MarketReadRelativeLine struct {
	Symbol     string    `json:"symbol"`
	Source     string    `json:"source"`
	Last       float64   `json:"last"`
	LastAt     time.Time `json:"last_at,omitzero"`
	PriorClose float64   `json:"prior_close"`
	ChangePct  float64   `json:"change_pct"`
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
