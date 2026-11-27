package rpc

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Edge level replay: the owner's level rule for a losing call (trim, cut,
// stop level, last-hour sale, buy back) replayed on the stock's five-minute
// bars of every past session he held a call on the stock, beside holding and
// reducing at once. Stock prices only; option values and his own fills are a
// later slice. Read-only: it never touches the order path.
const (
	MethodEdgeLevels        = "edge.levels"
	EdgeLevelsSchemaVersion = "edge-levels-v1"

	// EdgeLevelsReadingPriorLow and EdgeLevelsReadingDownOnDay are the two
	// readings of which calls get the rule: the stock under the prior
	// session's low at the look (the design's default), or down on the day.
	EdgeLevelsReadingPriorLow  = "prior_low"
	EdgeLevelsReadingDownOnDay = "down_on_day"

	// EdgeLevelsRowLosing and the three after it are the kinds of row a day
	// drafts at the first-hour look: under the prior low (losing), inside the
	// prior range (watched), above the prior high (working); and a watched
	// row whose stock closed a bar under the prior low later, before the
	// last hour, drafted then (EdgeLevelsDay.Later).
	EdgeLevelsRowLosing       = "losing"
	EdgeLevelsRowWatched      = "watched"
	EdgeLevelsRowWorking      = "working"
	EdgeLevelsRowWatchedLater = "watched_then_losing"

	// EdgeLevelsSizeHalves and EdgeLevelsSizeThirds are the two sizes.
	EdgeLevelsSizeHalves = "halves"
	EdgeLevelsSizeThirds = "thirds"

	// EdgeLevelsVersionHeld and the four after it are the results each
	// drafted day is valued under.
	EdgeLevelsVersionHeld    = "held"
	EdgeLevelsVersionAtOnce  = "reduced_at_once"
	EdgeLevelsVersionRule    = "rule"
	EdgeLevelsVersionBuyBack = "rule_buy_back"
	EdgeLevelsVersionIndex   = "rule_index_cut"

	// EdgeLevelsHalfAll and the two after it name the sample's halves.
	EdgeLevelsHalfAll    = "all"
	EdgeLevelsHalfFirst  = "first"
	EdgeLevelsHalfSecond = "second"
)

// EdgeLevelsVersions lists the versions in the order they are reported.
var EdgeLevelsVersions = []string{EdgeLevelsVersionHeld, EdgeLevelsVersionAtOnce, EdgeLevelsVersionRule, EdgeLevelsVersionBuyBack, EdgeLevelsVersionIndex}

// EdgeLevelsParams bounds the sessions replayed; empty means every session
// the retained statements and Canary's calendar cover.
type EdgeLevelsParams struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// NormalizeEdgeLevelsParams checks the optional session dates.
func NormalizeEdgeLevelsParams(p EdgeLevelsParams) (EdgeLevelsParams, error) {
	p.From, p.To = strings.TrimSpace(p.From), strings.TrimSpace(p.To)
	for _, day := range []string{p.From, p.To} {
		if day == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, day); err != nil {
			return p, fmt.Errorf("dates are YYYY-MM-DD")
		}
	}
	if p.From != "" && p.To != "" && p.From > p.To {
		return p, fmt.Errorf("from is after to")
	}
	return p, nil
}

// EdgeLevelsLookMinutes are the looks the replay is run at, in minutes after
// the open: half an hour and the design's first-hour look. The look sets
// the levels (the cut level is the session's low by then), so each is a
// version of its own.
var EdgeLevelsLookMinutes = []int{30, 60}

// EdgeLevelsResult is the replay: how it was run and, per look, the sample,
// a summary per reading, row, half and size, the counts beside the money,
// and one row per stock-day.
type EdgeLevelsResult struct {
	SchemaVersion string `json:"schema_version"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	NotExecution  bool   `json:"not_execution"`
	// From and To are the first and last session in the sample.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Pending counts the stock-days whose five-minute bars Canary is still
	// reading from the broker; Incomplete is true while any is pending. The
	// read goes on after the answer, and a later run includes those days.
	Pending       int              `json:"pending"`
	PendingReason string           `json:"pending_reason,omitempty"`
	Incomplete    bool             `json:"incomplete"`
	Method        EdgeLevelsMethod `json:"method"`
	Looks         []EdgeLevelsLook `json:"looks"`
}

// EdgeLevelsLook is the replay at one look. Split is the first session of
// the sample's second half.
type EdgeLevelsLook struct {
	LookMinutes int               `json:"look_minutes"`
	Split       string            `json:"split,omitempty"`
	Sample      EdgeLevelsSample  `json:"sample"`
	Summary     []EdgeLevelsSum   `json:"summary"`
	Counts      []EdgeLevelsTally `json:"counts"`
	Days        []EdgeLevelsDay   `json:"days"`
}

// EdgeLevelsMethod says how the replay runs, in words a reader can check.
type EdgeLevelsMethod struct {
	Rule    []string `json:"rule"`
	Prices  string   `json:"prices"`
	Sample  string   `json:"sample"`
	Index   string   `json:"index"`
	History string   `json:"history"`
}

// EdgeLevelsSample counts the sample and what was left out, by reason.
type EdgeLevelsSample struct {
	StockDays int            `json:"stock_days"`
	Sessions  int            `json:"sessions"`
	Names     int            `json:"names"`
	Rows      map[string]int `json:"rows"`
	Excluded  map[string]int `json:"excluded"`
}

// EdgeLevelsSum is one group's results: the mean result per version at the
// session's close and five sessions later, in percent of the position's
// value at the moment, and on how many days each version beat holding.
type EdgeLevelsSum struct {
	Reading   string                 `json:"reading"`
	Row       string                 `json:"row"`
	Half      string                 `json:"half"`
	Size      string                 `json:"size"`
	StockDays int                    `json:"stock_days"`
	Sessions  int                    `json:"sessions"`
	Names     int                    `json:"names"`
	Results   []EdgeLevelsResultMean `json:"results"`
}

// EdgeLevelsResultMean is one version's mean result in a group.
type EdgeLevelsResultMean struct {
	Version        string   `json:"version"`
	CloseMeanPct   float64  `json:"close_mean_pct"`
	CloseBeatsHeld int      `json:"close_beats_held"`
	CloseDays      int      `json:"close_days"`
	H5MeanPct      *float64 `json:"h5_mean_pct,omitempty"`
	H5BeatsHeld    int      `json:"h5_beats_held"`
	H5Days         int      `json:"h5_days"`
}

// EdgeLevelsTally is what the review counts beside the money, for one
// reading and half, on the rule in halves (the buy back's round trips on the
// rule with its buy back).
type EdgeLevelsTally struct {
	Reading string `json:"reading"`
	Half    string `json:"half"`
	Days    int    `json:"days"`
	// Cuts fired, and how many closed the session above the cut's sale, with
	// the mean of the close against the sale in percent.
	Cuts              int     `json:"cuts"`
	CutsClosedAbove   int     `json:"cuts_closed_above"`
	CutCloseVsSalePct float64 `json:"cut_close_vs_sale_pct"`
	// Crossings of the cut level (a bar traded under it), those that held
	// for two closes and those that came back first; for the held ones the
	// mean of the second close against the level in percent: what the wait
	// cost (negative) or saved against a sale at the touch.
	Crossings        int     `json:"crossings"`
	CrossingsHeld    int     `json:"crossings_held"`
	CrossingsNotHeld int     `json:"crossings_not_held"`
	WaitVsTouchPct   float64 `json:"wait_vs_touch_pct"`
	// Days the stop level was touched, of them those that closed back above
	// the cut level and those that closed under the stop level.
	StopTouches         int `json:"stop_touches"`
	StopClosedAboveCut  int `json:"stop_closed_above_cut"`
	StopClosedBelowStop int `json:"stop_closed_below_stop"`
	// Days the low broke after the moment, of them those whose best bounce
	// before the break stalled short of the nearer trim level, with the mean
	// shortfall in percent of the trim level.
	LowBreaks      int     `json:"low_breaks"`
	BouncesStalled int     `json:"bounces_stalled"`
	StallShortPct  float64 `json:"stall_short_pct"`
	// Buy backs, and the mean of the buy against the sale in percent: what a
	// round trip cost (positive bought dearer).
	RoundTrips       int     `json:"round_trips"`
	RoundTripCostPct float64 `json:"round_trip_cost_pct"`
	// Trims and last-hour sales that fired.
	Trims     int `json:"trims"`
	TimeSales int `json:"time_sales"`
}

// EdgeLevelsDay is one stock-day: the key a later slice joins on (symbol,
// session, contracts held at the prior close), the row at the first-hour
// look, the draft at the look and, for a watched row whose stock went under
// the prior low later, the draft then.
type EdgeLevelsDay struct {
	Symbol    string  `json:"symbol"`
	Session   string  `json:"session"`
	Contracts float64 `json:"contracts"`
	Half      string  `json:"half"`
	Short     bool    `json:"short_session,omitempty"`
	// Row is the kind of row at the look (losing, watched or working);
	// DownOnDay is the look's price under the prior close (the second
	// reading).
	Row       string          `json:"row"`
	DownOnDay bool            `json:"down_on_day"`
	Prior     MarketReadPrior `json:"prior"`
	ATR14     float64         `json:"atr_14"`
	// Close is the session's daily close; Close5 the close five sessions
	// later, absent when that session has not closed.
	Close     float64          `json:"close"`
	Close5    *float64         `json:"close_5,omitempty"`
	Close5Day string           `json:"close_5_day,omitempty"`
	Look      EdgeLevelsDraft  `json:"look"`
	Later     *EdgeLevelsDraft `json:"later,omitempty"`
}

// EdgeLevelsDraft is a row's levels frozen at its moment and the results
// valued from that moment. A row the rule does not draft carries the held
// result only.
type EdgeLevelsDraft struct {
	Moment      time.Time             `json:"moment"`
	MomentPrice float64               `json:"moment_price"`
	DayHigh     float64               `json:"day_high"`
	DayLow      float64               `json:"day_low"`
	DayMid      float64               `json:"day_mid"`
	Cut         float64               `json:"cut"`
	Stop        float64               `json:"stop"`
	Trims       []float64             `json:"trims"`
	Results     []EdgeLevelsDayResult `json:"results"`
}

// EdgeLevelsDayResult is one version and size on one day.
type EdgeLevelsDayResult struct {
	Version  string            `json:"version"`
	Size     string            `json:"size"`
	ClosePct float64           `json:"close_pct"`
	H5Pct    *float64          `json:"h5_pct,omitempty"`
	Events   []EdgeLevelsEvent `json:"events"`
}

// EdgeLevelsEvent is one sale or buy the version made, a fraction of the
// position at the moment, at a modelled price.
type EdgeLevelsEvent struct {
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Fraction float64   `json:"fraction"`
	Price    float64   `json:"price"`
}

// ValidateEdgeLevelsResult refuses a result that is not marked
// not_execution, carries another schema or a non-finite figure.
func ValidateEdgeLevelsResult(r EdgeLevelsResult) error {
	if r.SchemaVersion != EdgeLevelsSchemaVersion {
		return fmt.Errorf("edge levels schema %q is not %s", r.SchemaVersion, EdgeLevelsSchemaVersion)
	}
	if !r.NotExecution {
		return fmt.Errorf("edge levels result must be marked not_execution")
	}
	if r.Incomplete != (r.Pending > 0) {
		return fmt.Errorf("edge levels incomplete flag disagrees with its pending days")
	}
	finite := func(v ...float64) bool {
		for _, x := range v {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return false
			}
		}
		return true
	}
	for _, look := range r.Looks {
		if err := validateEdgeLevelsLook(look, finite); err != nil {
			return err
		}
	}
	return nil
}

func validateEdgeLevelsLook(r EdgeLevelsLook, finite func(...float64) bool) error {
	for _, s := range r.Summary {
		for _, m := range s.Results {
			if !finite(m.CloseMeanPct) || m.H5MeanPct != nil && !finite(*m.H5MeanPct) {
				return fmt.Errorf("edge levels summary carries a non-finite figure")
			}
		}
	}
	for _, d := range r.Days {
		drafts := []EdgeLevelsDraft{d.Look}
		if d.Later != nil {
			drafts = append(drafts, *d.Later)
		}
		for _, draft := range drafts {
			for _, res := range draft.Results {
				if !finite(res.ClosePct) || res.H5Pct != nil && !finite(*res.H5Pct) {
					return fmt.Errorf("edge levels day %s %s carries a non-finite figure", d.Symbol, d.Session)
				}
			}
		}
	}
	return nil
}
