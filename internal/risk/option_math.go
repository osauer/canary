package risk

import (
	"math"
	"strings"
)

// Shared option arithmetic for the proposal engine and the trading rulebook.
// One copy: the theta-hygiene bucket and rulebook rule 4 must agree on what
// "extrinsic" means or their verdicts drift apart on the same leg. Pure
// scalar signatures — internal/rpc imports this package, so no rpc types.

// OptionIntrinsicPerShare is the per-share in-the-money amount; 0 for an
// out-of-the-money option or an unrecognized right.
func OptionIntrinsicPerShare(right string, underlying, strike float64) float64 {
	switch strings.ToUpper(strings.TrimSpace(right)) {
	case "C", "CALL":
		return math.Max(0, underlying-strike)
	case "P", "PUT":
		return math.Max(0, strike-underlying)
	default:
		return 0
	}
}

// OptionExtrinsicPerShare is mark minus intrinsic, floored at zero (a stale
// mark below intrinsic means the quote, not negative time value). ok=false
// when the underlying spot is unavailable or the mark is non-positive —
// callers must treat that as uncomputable, never as zero extrinsic.
func OptionExtrinsicPerShare(right string, underlying *float64, strike, mark float64) (float64, bool) {
	if underlying == nil || mark <= 0 {
		return 0, false
	}
	return math.Max(0, mark-OptionIntrinsicPerShare(right, *underlying, strike)), true
}
