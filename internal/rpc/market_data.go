package rpc

import "time"

// Market data methods expose daemon-owned observations without broker writes.
const (
	MethodMarketSnapshot = "market.snapshot"
	MethodMarketHistory  = "market.history"
)

// MarketInstrument is a named exact quote with explicit per-instrument failure.
type MarketInstrument struct {
	Key        string `json:"key"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Underlying string `json:"underlying,omitempty"`
	Quote      *Quote `json:"quote,omitempty"`
	Error      string `json:"error,omitempty"`
}

// MarketSnapshotResult separates public quotes from the scope of held underlyings.
type MarketSnapshotResult struct {
	AsOf           time.Time             `json:"as_of"`
	Authority      *AccountDataAuthority `json:"authority,omitempty"`
	Instruments    []MarketInstrument    `json:"instruments"`
	Underlyings    []MarketInstrument    `json:"underlyings"`
	CoverageStatus string                `json:"coverage_status"`
	Truncated      bool                  `json:"truncated,omitempty"`
	// Delayed names, by row name, the covered rows whose selected price
	// comes from IBKR's delayed feed, including a delayed-frozen previous
	// close. Each quote's data_type, feed_type, price_source, quote_quality
	// and delayed_feed warning say which delayed value it is.
	Delayed []string `json:"delayed,omitempty"`
}

// MarketHistoryParams selects a bounded observed price series, not an analysis.
type MarketHistoryParams struct {
	Contract ContractParams `json:"contract"`
	Range    string         `json:"range"`
}

// MarketHistoryPoint is an observed bar close; volume is absent for midpoint data.
type MarketHistoryPoint struct {
	At     time.Time `json:"at"`
	Value  float64   `json:"value"`
	Volume *int64    `json:"volume,omitempty"`
}

// MarketHistoryResult preserves actual acquisition, range and pricing basis.
type MarketHistoryResult struct {
	TimestampKind    string               `json:"timestamp_kind"`
	PriceBasis       string               `json:"price_basis"`
	RegularHoursOnly bool                 `json:"regular_hours_only"`
	RequestedStart   time.Time            `json:"requested_start"`
	Contract         ContractParams       `json:"contract"`
	Range            string               `json:"range"`
	Interval         string               `json:"interval"`
	Source           string               `json:"source"`
	AsOf             time.Time            `json:"as_of"`
	Start            time.Time            `json:"start"`
	End              time.Time            `json:"end"`
	Points           []MarketHistoryPoint `json:"points"`
	Reference        *float64             `json:"reference,omitempty"`
	ReferenceName    string               `json:"reference_name,omitempty"`
	CoverageStatus   string               `json:"coverage_status"`
	Cache            *MarketHistoryCache  `json:"cache,omitempty"`
}

// MarketHistoryCache identifies the records actually selected for display.
// StoredAt is a durability receipt, not a market clock. Coverage never implies
// that every interval traded or that a daily futures settlement is final.
type MarketHistoryCache struct {
	Selected        string    `json:"selected"`
	StoredAt        time.Time `json:"stored_at,omitzero"`
	FetchedAt       time.Time `json:"fetched_at"`
	CoveredThrough  time.Time `json:"covered_through"`
	Coverage        string    `json:"coverage"`
	MissingSessions int       `json:"missing_sessions,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	RefreshFailed   bool      `json:"refresh_failed,omitempty"`
	RefreshDue      bool      `json:"refresh_due,omitempty"`
	PreviousWindow  bool      `json:"previous_window,omitempty"`
}
