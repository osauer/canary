package mandate

import (
	"fmt"
	"math"
	"time"
)

// A missing implied volatility on one option no longer has to blind the
// whole book (owner, 2026-10-28 08:51 CET). The daemon prices such a leg at
// the volatility its own mark implies (IVSourceMark). The evaluator accepts
// that leg only under the owner's [mandate.iv_fallback] table: it runs the
// book at both edges of an uncertainty band per leg, sized by time to
// expiry, and judges the width of the S&P-fall range against the loss room.
// Without the table such a leg is unknown, as before. With the loss over the
// room, or no room at all, the width cannot change the answer, so only the
// verdict at both edges decides (owner, 2026-11-07 07:33 and 08:03 CET).

// IVSourceMark marks an option leg priced at the volatility implied by its
// own mark because the position row carried none.
const IVSourceMark = "mark"

// Fallback tiers on the verdict.
const (
	IVTierMeasured = "measured"
	IVTierRange    = "range"
	IVTierUnknown  = "unknown"
)

// Expiry classes of the band, in days to expiry: under a week the IV of a
// weekly moves most between sessions, past a month least (measured on the
// recorded chains, 2026-10-28). The bounds define the classes the owner's
// three band numbers apply to.
const (
	ivShortDays = 8
	ivMonthDays = 35
)

// IVFallback is the owner's [mandate.iv_fallback] table: the band per
// expiry class, as a share of the leg's fallback volatility, and the two
// tiers, as shares of the loss room the S&P-fall uncertainty may take.
type IVFallback struct {
	ShortBandPct, MonthBandPct, LongBandPct float64
	MeasuredPct, RangePct                   float64
}

// bandPct is the band for a leg's time to expiry.
func (f *IVFallback) bandPct(years float64) float64 {
	switch days := years * 365; {
	case days < ivShortDays:
		return f.ShortBandPct
	case days <= ivMonthDays:
		return f.MonthBandPct
	default:
		return f.LongBandPct
	}
}

// IVFallbackReport says which legs were priced on a fallback volatility, the
// S&P-fall loss at both edges of their bands and how the verdict judged the
// width. RatioToRoom is nil when the loss room is zero.
type IVFallbackReport struct {
	Legs              []IVFallbackLeg `json:"legs"`
	IndexLossLowBase  float64         `json:"index_loss_low_base"`
	IndexLossHighBase float64         `json:"index_loss_high_base"`
	// UncertaintyBase is half the spread between the two IV-band-edge
	// outcomes. It is not an error bound centered on the base figure.
	UncertaintyBase float64  `json:"uncertainty_base"`
	RatioToRoom     *float64 `json:"ratio_to_room,omitempty"`
	Tier            string   `json:"tier"`
	Reason          string   `json:"reason,omitempty"`
}

// IVFallbackLeg is one leg priced on a fallback volatility.
type IVFallbackLeg struct {
	Symbol       string    `json:"symbol"`
	Source       string    `json:"source"`
	AsOf         time.Time `json:"as_of,omitzero"`
	DaysToExpiry int       `json:"days_to_expiry"`
	IV           float64   `json:"iv"`
	BandPct      float64   `json:"band_pct"`
}

// fallbackLegs are the option legs priced on a fallback volatility.
func fallbackLegs(legs []Leg) []int {
	var out []int
	for i, l := range legs {
		if l.Kind == KindOption && l.IVSource == IVSourceMark && l.IV != nil {
			out = append(out, i)
		}
	}
	return out
}

// withIV is the leg at another volatility.
func withIV(l Leg, iv float64) Leg {
	l.IV = &iv
	return l
}

// judgeIVFallback runs the book at both edges of every fallback leg's band:
// each leg at the edge that loses more in the S&P fall (the pessimistic
// book) and at the other (the optimistic book). It returns nil when no leg
// is on a fallback. The tier is measured while the half-range is within
// measured_pct of the loss room, range within range_pct when the verdict
// is the same at both edges, unknown otherwise. Once the loss is over the room,
// or no room is left, range_pct no longer applies: an unknown verdict would hold
// the reduction and the hedge, and an answer both edges give cannot be changed
// by the band.
func judgeIVFallback(in Input, legs []Leg, returns map[string][]float64, sim simulation, loss *LossMeasure, status string) *IVFallbackReport {
	idx := fallbackLegs(legs)
	if len(idx) == 0 || in.Policy.IVFallback == nil {
		return nil
	}
	f := in.Policy.IVFallback
	betas := map[string]float64{}
	for _, u := range sim.underlyings {
		betas[u.Underlying] = u.Beta
	}
	pess, opt := append([]Leg(nil), legs...), append([]Leg(nil), legs...)
	r := &IVFallbackReport{}
	for _, i := range idx {
		l := legs[i]
		band := f.bandPct(l.TimeToExpiryYears)
		lo, hi := withIV(l, *l.IV*(1-band/100)), withIV(l, *l.IV*(1+band/100))
		shock := 1 - IndexShockPct/100*betas[l.Underlying]
		fallLoss := func(x Leg) float64 {
			return legValue(x, x.Spot, x.TimeToExpiryYears) - legValue(x, x.Spot*math.Max(shock, 0), x.TimeToExpiryYears)
		}
		if fallLoss(hi) >= fallLoss(lo) {
			pess[i], opt[i] = hi, lo
		} else {
			pess[i], opt[i] = lo, hi
		}
		r.Legs = append(r.Legs, IVFallbackLeg{Symbol: l.Symbol, Source: l.IVSource, AsOf: l.IVAsOf, DaysToExpiry: int(math.Round(l.TimeToExpiryYears * 365)), IV: *l.IV, BandPct: band})
	}
	high, low := simulate(in, pess, returns), simulate(in, opt, returns)
	r.IndexLossLowBase, r.IndexLossHighBase = low.stresses.IndexLossBase, high.stresses.IndexLossBase
	if r.IndexLossLowBase > r.IndexLossHighBase {
		r.IndexLossLowBase, r.IndexLossHighBase = r.IndexLossHighBase, r.IndexLossLowBase
	}
	r.UncertaintyBase = (r.IndexLossHighBase - r.IndexLossLowBase) / 2
	room := LossRoomBase(in.Policy, in.Capital)
	share := math.Inf(1)
	if room > 0 {
		v := r.UncertaintyBase / room
		r.RatioToRoom, share = &v, v
	} else if r.UncertaintyBase <= lossEpsilon {
		share = 0
	}
	judged := func(s simulation) string {
		if len(s.stresses.GapBreaches) > 0 || lossOverRoom(loss) {
			return StatusReduce
		}
		return StatusWithin
	}
	named := fmt.Sprintf("%d %s priced on the volatility implied by %s mark", len(idx), plural(len(idx), "option", "options"), plural(len(idx), "its", "their"))
	tier, tooWide := f.tier(room > 0 && !lossOverRoom(loss), share, judged(high) == status && judged(low) == status)
	r.Tier = tier
	switch {
	case tier != IVTierUnknown:
	case tooWide:
		r.Reason = fmt.Sprintf("%s: the S&P-fall figure is uncertain by more than %g%% of the loss room", named, f.RangePct)
	default:
		r.Reason = named + ": the verdict differs across their uncertainty band"
	}
	return r
}

// tier judges the half-range's share of the loss room. widthDecides is false
// once the loss is over the room or none is left: then only the verdict at both
// edges does.
func (f *IVFallback) tier(widthDecides bool, share float64, sameAtEdges bool) (tier string, tooWide bool) {
	switch {
	case share*100 <= f.MeasuredPct+1e-9:
		return IVTierMeasured, false
	case widthDecides && share*100 > f.RangePct+1e-9:
		return IVTierUnknown, true
	case sameAtEdges:
		return IVTierRange, false
	}
	return IVTierUnknown, false
}

// HedgeFallBase is the S&P-fall loss a hedge may offset: with an option on a
// fallback volatility the edge that loses less, so no end of the band leaves
// the hedge past neutral (owner, 2026-11-07 07:33 CET). What it leaves unhedged
// the next plan sells once the option has its own volatility. It reads
// v.Stresses, which an unknown verdict does not carry.
func (v Verdict) HedgeFallBase() float64 {
	if r := v.IVFallback; r != nil {
		return min(r.IndexLossLowBase, v.Stresses.IndexLossBase)
	}
	return v.Stresses.IndexLossBase
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
