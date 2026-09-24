package rpc

import (
	"errors"
	"time"
)

// MethodMarketTape reads an aligned, retrospective broad-market tape.
const MethodMarketTape = "market.tape"

// MarketTapeParams bounds the completed US equity sessions shown.
type MarketTapeParams struct {
	Sessions int `json:"sessions,omitempty"`
}

// NormalizeMarketTapeParams applies the default and rejects unbounded reads.
func NormalizeMarketTapeParams(p MarketTapeParams) (MarketTapeParams, error) {
	if p.Sessions == 0 {
		p.Sessions = 20
	}
	if p.Sessions < 5 || p.Sessions > 60 {
		return p, errors.New("tape sessions must be between 5 and 60")
	}
	return p, nil
}

// MarketTapePrice contains observed prices and daemon-derived comparisons.
// Changes require consecutive official sessions. WindowChangePct uses the
// first displayed session, never a different base for each instrument.
type MarketTapePrice struct {
	Close            float64  `json:"close"`
	ChangePct        *float64 `json:"change_pct"`
	WindowChangePct  *float64 `json:"window_change_pct"`
	Volume           *int64   `json:"volume"`
	RelativeVolume20 *float64 `json:"relative_volume_20"`
}

// MarketTapeBreadth preserves separate measurement denominators. Nil values
// are unknown, while a measured zero remains a valid observation.
type MarketTapeBreadth struct {
	Participation     *BreadthParticipation `json:"participation,omitempty"`
	PctAbove50DMA     *float64              `json:"pct_above_50dma"`
	PctAbove200DMA    *float64              `json:"pct_above_200dma"`
	Change50PP        *float64              `json:"change_50_pp"`
	NewHighs          *int                  `json:"new_highs"`
	NewLows           *int                  `json:"new_lows"`
	MemberCount       int                   `json:"member_count"`
	Coverage50        int                   `json:"coverage_50"`
	Coverage200       int                   `json:"coverage_200"`
	CoverageHighsLows int                   `json:"coverage_highs_lows"`
}

// MarketTapeSession is one official session, including explicit missing legs.
type MarketTapeSession struct {
	Date    string             `json:"date"`
	SPX     *MarketTapePrice   `json:"spx"`
	QQQ     *MarketTapePrice   `json:"qqq"`
	Breadth *MarketTapeBreadth `json:"breadth"`
	Reading *MarketTapeReading `json:"reading,omitempty"`
}

// BreadthParticipation carries separately covered constituent measurements.
// RecordedAt is this revision's computation, not historical availability.
type BreadthParticipation struct {
	Method          string    `json:"method"`
	RecordedAt      time.Time `json:"recorded_at"`
	InputObservedAt time.Time `json:"input_observed_at,omitzero"`
	MembershipID    string    `json:"membership_id"`
	PctAbove20DMA   *float64  `json:"pct_above_20dma"`
	Coverage20      int       `json:"coverage_20"`
	Advancing       int       `json:"advancing"`
	Declining       int       `json:"declining"`
	Unchanged       int       `json:"unchanged"`
	CoverageAD      int       `json:"coverage_ad"`
	AdvancePct      *float64  `json:"advance_pct"`
	AdvancingVolume float64   `json:"advancing_volume"`
	DecliningVolume float64   `json:"declining_volume"`
	UnchangedVolume float64   `json:"unchanged_volume"`
	CoverageVolume  int       `json:"coverage_volume"`
	UpVolumePct     *float64  `json:"up_volume_pct"`
}

// MarketTapeReading explains observed relationships. It carries no probability,
// risk posture, alert eligibility or order recommendation.
type MarketTapeReading struct {
	Headline string               `json:"headline"`
	Summary  string               `json:"summary"`
	Evidence []MarketTapeEvidence `json:"evidence"`
	WatchFor []string             `json:"watch_for"`
	Limits   []string             `json:"limits"`
	Rally    *MarketTapeRally     `json:"rally,omitempty"`
}

// MarketTapeEvidence pairs a measurement with its plain-language interpretation.
// The corresponding machine-readable values remain on MarketTapeSession.
type MarketTapeEvidence struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Value   string `json:"value"`
	Meaning string `json:"meaning"`
}

// MarketTapeRally compares the latest non-rising close with the most recent
// prior up session within five sessions. GivebackPct is not capped at 100.
type MarketTapeRally struct {
	Session     string  `json:"session"`
	GainPct     float64 `json:"gain_pct"`
	GivebackPct float64 `json:"giveback_pct"`
}

// MarketTapeSource retains producer clocks independently of composition time.
// Status describes displayed-session coverage: available, partial, unavailable.
// MissingSessions counts missing price (or 50-day breadth) rows. MissingMetrics
// separately counts missing optional measures, including the volume baseline.
type MarketTapeSource struct {
	Key             string              `json:"key"`
	Status          string              `json:"status"`
	Source          string              `json:"source"`
	AsOf            time.Time           `json:"as_of,omitzero"`
	CoveredThrough  string              `json:"covered_through,omitempty"`
	MissingSessions int                 `json:"missing_sessions"`
	MissingMetrics  map[string]int      `json:"missing_metrics,omitempty"`
	Detail          string              `json:"detail"`
	Cache           *MarketHistoryCache `json:"cache,omitempty"`
}

// MarketTapeResult is descriptive evidence, never a prediction or risk verdict.
// HistoricalAvailability is unknown until first-availability is collected;
// source AsOf fields must not be relabelled as historical publication times.
type MarketTapeResult struct {
	SchemaVersion          string              `json:"schema_version"`
	AsOf                   time.Time           `json:"as_of"`
	Timezone               string              `json:"timezone"`
	LatestSession          string              `json:"latest_session"`
	CoverageStatus         string              `json:"coverage_status"`
	HistoricalAvailability string              `json:"historical_availability"`
	NotPredictive          bool                `json:"not_predictive"`
	Sessions               []MarketTapeSession `json:"sessions"`
	Sources                []MarketTapeSource  `json:"sources"`
	Notes                  []string            `json:"notes"`
}
