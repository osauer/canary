package rpc

import "time"

// MethodPortfolioSnapshot is the daemon-owned current valuation projection.
const MethodPortfolioSnapshot = "portfolio.snapshot"

// PortfolioAllocation preserves signed values and missing inputs; it is not risk exposure.
type PortfolioAllocation struct {
	Name       string   `json:"name"`
	ValueBase  *float64 `json:"value_base"`
	PercentNLV *float64 `json:"percent_nlv"`
	Observed   int      `json:"observed"`
	Missing    int      `json:"missing"`
}

// PortfolioLookThrough names a held fund whose value was spread over
// published sector weights, and how old those weights are.
type PortfolioLookThrough struct {
	Symbol string `json:"symbol"`
	AsOf   string `json:"as_of"`
	Source string `json:"source"`
}

// AllocationMeasureMarketValue names the measure of both tables: signed
// market value, where the money sits. Directional risk lives in the
// positions projection's dollar delta, not here.
const AllocationMeasureMarketValue = "market_value"

// PortfolioSnapshotResult separates current holdings valuation from statement returns.
type PortfolioSnapshotResult struct {
	AsOf              time.Time              `json:"as_of"`
	AccountAsOf       time.Time              `json:"account_as_of"`
	PositionsAsOf     time.Time              `json:"positions_as_of"`
	Authority         *AccountDataAuthority  `json:"authority"`
	BaseCurrency      string                 `json:"base_currency"`
	NetLiquidation    *float64               `json:"net_liquidation"`
	AssetClasses      []PortfolioAllocation  `json:"asset_classes"`
	AssetClassMeasure string                 `json:"asset_class_measure"`
	Sectors           []PortfolioAllocation  `json:"sectors"`
	SectorMeasure     string                 `json:"sector_measure"`
	SectorBasis       string                 `json:"sector_basis"`
	LookThrough       []PortfolioLookThrough `json:"look_through,omitempty"`
	// DefunctExcluded counts holdings the broker no longer quotes; they carry
	// no exposure and appear in neither table. UnquotedExcluded counts
	// zero-mark, zero-value stock rows the broker has not yet ruled on: they
	// remain account position truth elsewhere but have nothing to move here.
	DefunctExcluded   int      `json:"defunct_excluded"`
	UnquotedExcluded  int      `json:"unquoted_excluded"`
	CostBasisBase     *float64 `json:"cost_basis_base"`
	CostBasisObserved int      `json:"cost_basis_observed"`
	CostBasisMissing  int      `json:"cost_basis_missing"`
	CoverageStatus    string   `json:"coverage_status"`
}
