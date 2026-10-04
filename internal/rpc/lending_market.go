package rpc

import (
	"errors"
	"math"
	"slices"
	"time"
)

// MethodLendingMarket reads bounded, asynchronously refreshed stock context.
const MethodLendingMarket = "lending.market"

// LendingMarketParams selects explicit symbols from the US borrowing feed.
type LendingMarketParams struct {
	Symbols []string `json:"symbols"`
}

// LendingMarketRow preserves independent price, volume and history clocks.
type LendingMarketRow struct {
	Symbol             string         `json:"symbol"`
	Contract           ContractParams `json:"contract"`
	Status             string         `json:"status"`
	CheckedAt          time.Time      `json:"checked_at,omitzero"`
	ValidUntil         time.Time      `json:"valid_until,omitzero"`
	Price              *float64       `json:"price,omitempty"`
	PriceAt            time.Time      `json:"price_at,omitzero"`
	PriceKind          string         `json:"price_kind,omitempty"`
	DayChangePct       *float64       `json:"day_change_pct,omitempty"`
	Volume             *int64         `json:"volume,omitempty"`
	VolumeDate         string         `json:"volume_date,omitempty"`
	AvgDollarVolume20D *float64       `json:"avg_dollar_volume_20d,omitempty"`
	LiquidityAsOf      string         `json:"liquidity_as_of,omitempty"`
	YTDChangePct       *float64       `json:"ytd_change_pct,omitempty"`
	YTDBaseDate        string         `json:"ytd_base_date,omitempty"`
	YTDAsOf            string         `json:"ytd_as_of,omitempty"`
	Detail             string         `json:"detail,omitempty"`
}

// LendingMarketResult is market context, never an execution or lending promise.
type LendingMarketResult struct {
	Kind    string             `json:"kind"`
	Symbols []string           `json:"symbols"`
	Rows    []LendingMarketRow `json:"rows"`
}

// ValidateLendingMarketResult rejects mismatched and malformed adapter responses.
func ValidateLendingMarketResult(r LendingMarketResult, symbols []string) error {
	bad := func() error { return errors.New("invalid lending market evidence") }
	validDate := func(v string) bool { _, err := time.Parse(time.DateOnly, v); return err == nil }
	if r.Kind != "lending_market" || !slices.Equal(r.Symbols, symbols) || len(r.Rows) != len(symbols) {
		return bad()
	}
	for i, row := range r.Rows {
		if row.Symbol != symbols[i] {
			return bad()
		}
		switch row.Status {
		case "pending", "available", "partial", "unavailable":
		default:
			return bad()
		}
		for _, v := range []*float64{row.Price, row.DayChangePct, row.AvgDollarVolume20D, row.YTDChangePct} {
			if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
				return bad()
			}
		}
		if row.Price != nil && (*row.Price <= 0 || row.PriceAt.IsZero() || (row.PriceKind != "close" && row.PriceKind != "intraday" && row.PriceKind != "delayed")) {
			return bad()
		}
		if row.Volume != nil && (*row.Volume < 0 || !validDate(row.VolumeDate)) {
			return bad()
		}
		if row.AvgDollarVolume20D != nil && (*row.AvgDollarVolume20D < 0 || !validDate(row.LiquidityAsOf)) {
			return bad()
		}
		if row.YTDChangePct != nil && (!validDate(row.YTDBaseDate) || !validDate(row.YTDAsOf) || row.YTDBaseDate >= row.YTDAsOf) {
			return bad()
		}
		if row.Price != nil || row.Volume != nil || row.AvgDollarVolume20D != nil || row.YTDChangePct != nil || row.DayChangePct != nil {
			if row.Status != "available" && row.Status != "partial" {
				return bad()
			}
			if row.Contract.Symbol != row.Symbol || row.Contract.ConID <= 0 || row.Contract.Currency != "USD" || row.Contract.SecType != "STK" || row.CheckedAt.IsZero() || (!row.ValidUntil.After(row.CheckedAt) || row.ValidUntil.Sub(row.CheckedAt) > 5*time.Minute) {
				return bad()
			}
		}
	}
	return nil
}
