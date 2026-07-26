package daemon

import (
	"fmt"
	"sort"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func buildGammaLegDiagnostics(underlying string, legs []legData, spot float64) *rpc.GammaLegDiagnostics {
	out := &rpc.GammaLegDiagnostics{
		ByUnderlying:   make(map[string]rpc.GammaLegDiagnosticCounts),
		ByTradingClass: make(map[string]rpc.GammaLegDiagnosticCounts),
	}
	underlying = gammaDiagnosticKey(underlying, "UNKNOWN")
	for _, leg := range legs {
		gamma := bsGamma(spot, leg.strike, leg.dte, leg.iv, 0, 0)
		abs := absGEX(gamma, float64(leg.oi), 100, spot)
		counts := gammaLegDiagnosticCounts(leg, gamma, abs)

		out.Total = addGammaLegDiagnosticCounts(out.Total, counts)
		out.ByUnderlying[underlying] = addGammaLegDiagnosticCounts(out.ByUnderlying[underlying], counts)
		classKey := gammaDiagnosticKey(leg.tradingClass, underlying)
		out.ByTradingClass[classKey] = addGammaLegDiagnosticCounts(out.ByTradingClass[classKey], counts)
	}
	if len(out.ByUnderlying) == 0 {
		out.ByUnderlying = nil
	}
	if len(out.ByTradingClass) == 0 {
		out.ByTradingClass = nil
	}
	return out
}

func gammaLegDiagnosticCounts(leg legData, gamma, absGEX float64) rpc.GammaLegDiagnosticCounts {
	counts := rpc.GammaLegDiagnosticCounts{PricedLegs: 1}
	switch leg.ivSource {
	case gammaIVSourceLiveMid:
		counts.DerivedLiveMidLegs = 1
	case gammaIVSourcePrevClose:
		counts.DerivedPrevCloseLegs = 1
	default:
		counts.ModelTickLegs = 1
	}
	if leg.oiObserved {
		counts.OpenInterestObservedLegs = 1
	}
	if leg.oiLive {
		counts.OILiveObservedLegs = 1
	}
	if leg.oiCarried {
		counts.OICarriedForwardLegs = 1
	}
	if leg.oi > 0 {
		counts.OpenInterestLegs = 1
	}
	if gamma > 0 {
		counts.GammaPositiveLegs = 1
	}
	if absGEX > 0 {
		counts.AbsGEXLegs = 1
	}
	return counts
}

func combineGammaLegDiagnostics(inputs ...*rpc.GammaLegDiagnostics) *rpc.GammaLegDiagnostics {
	out := &rpc.GammaLegDiagnostics{}
	for _, in := range inputs {
		if in == nil {
			continue
		}
		out.Total = addGammaLegDiagnosticCounts(out.Total, in.Total)
		out.ByUnderlying = mergeGammaLegDiagnosticMap(out.ByUnderlying, in.ByUnderlying)
		out.ByTradingClass = mergeGammaLegDiagnosticMap(out.ByTradingClass, in.ByTradingClass)
	}
	if out.Total == (rpc.GammaLegDiagnosticCounts{}) && len(out.ByUnderlying) == 0 && len(out.ByTradingClass) == 0 {
		return nil
	}
	return out
}

func mergeGammaLegDiagnosticMap(dst, src map[string]rpc.GammaLegDiagnosticCounts) map[string]rpc.GammaLegDiagnosticCounts {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]rpc.GammaLegDiagnosticCounts, len(src))
	}
	for key, counts := range src {
		dst[key] = addGammaLegDiagnosticCounts(dst[key], counts)
	}
	return dst
}

func addGammaLegDiagnosticCounts(a, b rpc.GammaLegDiagnosticCounts) rpc.GammaLegDiagnosticCounts {
	return rpc.GammaLegDiagnosticCounts{
		PricedLegs:               a.PricedLegs + b.PricedLegs,
		ModelTickLegs:            a.ModelTickLegs + b.ModelTickLegs,
		DerivedLiveMidLegs:       a.DerivedLiveMidLegs + b.DerivedLiveMidLegs,
		DerivedPrevCloseLegs:     a.DerivedPrevCloseLegs + b.DerivedPrevCloseLegs,
		OpenInterestObservedLegs: a.OpenInterestObservedLegs + b.OpenInterestObservedLegs,
		OILiveObservedLegs:       a.OILiveObservedLegs + b.OILiveObservedLegs,
		OICarriedForwardLegs:     a.OICarriedForwardLegs + b.OICarriedForwardLegs,
		OpenInterestLegs:         a.OpenInterestLegs + b.OpenInterestLegs,
		GammaPositiveLegs:        a.GammaPositiveLegs + b.GammaPositiveLegs,
		AbsGEXLegs:               a.AbsGEXLegs + b.AbsGEXLegs,
	}
}

func formatGammaLegDiagnostics(d *rpc.GammaLegDiagnostics) string {
	if d == nil {
		return "diagnostics unavailable"
	}
	parts := []string{"total " + formatGammaLegDiagnosticCounts(d.Total)}
	if len(d.ByUnderlying) > 0 {
		parts = append(parts, "by_underlying "+formatGammaLegDiagnosticMap(d.ByUnderlying))
	}
	if len(d.ByTradingClass) > 0 {
		parts = append(parts, "by_trading_class "+formatGammaLegDiagnosticMap(d.ByTradingClass))
	}
	return strings.Join(parts, "; ")
}

func formatGammaLegDiagnosticMap(m map[string]rpc.GammaLegDiagnosticCounts) string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s %s", key, formatGammaLegDiagnosticCounts(m[key])))
	}
	return strings.Join(parts, ", ")
}

func formatGammaLegDiagnosticCounts(c rpc.GammaLegDiagnosticCounts) string {
	return fmt.Sprintf("priced=%d model_tick_iv=%d derived_mid_iv=%d derived_close_iv=%d oi_seen=%d oi>0=%d gamma>0=%d abs_gex>0=%d oi_live=%d oi_carried=%d",
		c.PricedLegs, c.ModelTickLegs, c.DerivedLiveMidLegs, c.DerivedPrevCloseLegs,
		c.OpenInterestObservedLegs, c.OpenInterestLegs, c.GammaPositiveLegs, c.AbsGEXLegs,
		c.OILiveObservedLegs, c.OICarriedForwardLegs)
}

func gammaOIMissingCount(d *rpc.GammaLegDiagnostics) int {
	if d == nil {
		return 0
	}
	observed := max(d.Total.OpenInterestObservedLegs, d.Total.OpenInterestLegs)
	missing := d.Total.PricedLegs - observed
	if missing < 0 {
		return 0
	}
	return missing
}

func gammaDiagnosticKey(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		value = "UNKNOWN"
	}
	return strings.ToUpper(value)
}
