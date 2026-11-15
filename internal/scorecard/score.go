package scorecard

import (
	"math"
	"sort"
	"time"

	"github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

// History is the daily-close evidence behind the scores: each underlying by
// exact contract id, the market proxy, and one series per sector fund with
// the sector each underlying belongs to.
type History struct {
	Underlying   map[int64][]edge.DailyBar
	Market       []edge.DailyBar
	MarketSymbol string
	// Sector holds the sector funds' closes by fund symbol; SectorFunds names
	// each underlying's fund and SectorNames its GICS sector, both by
	// underlying contract id. An underlying absent from SectorFunds is
	// adjusted on the market alone.
	Sector      map[string][]edge.DailyBar
	SectorFunds map[int64]string
	SectorNames map[int64]string
}

// series is one normalized daily-close series with a day index.
type series struct {
	bars  []edge.DailyBar
	index map[string]int
}

func newSeries(bars []edge.DailyBar) series {
	byDay := map[string]edge.DailyBar{}
	for _, bar := range bars {
		if bar.Close <= 0 || bar.Day.IsZero() || math.IsNaN(bar.Close) || math.IsInf(bar.Close, 0) {
			continue
		}
		byDay[dayKey(bar.Day)] = edge.DailyBar{ConID: bar.ConID, Day: dayStart(bar.Day), Close: bar.Close}
	}
	keys := make([]string, 0, len(byDay))
	for key := range byDay {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := series{bars: make([]edge.DailyBar, 0, len(keys)), index: make(map[string]int, len(keys))}
	for i, key := range keys {
		out.bars = append(out.bars, byDay[key])
		out.index[key] = i
	}
	return out
}

func (s series) at(day time.Time) (int, bool) {
	i, ok := s.index[dayKey(day)]
	return i, ok
}

func (s series) firstOnOrAfter(day time.Time) (int, bool) {
	target := dayStart(day)
	i := sort.Search(len(s.bars), func(i int) bool { return !s.bars[i].Day.Before(target) })
	if i >= len(s.bars) {
		return 0, false
	}
	return i, true
}

// entryIndex finds the execution session's bar: the first bar on or after the
// execution day, within EntrySessionSlack.
func (s series) entryIndex(executedAt time.Time) (int, bool) {
	i, ok := s.firstOnOrAfter(executedAt)
	if !ok || s.bars[i].Day.Sub(dayStart(executedAt)) > EntrySessionSlack {
		return 0, false
	}
	return i, true
}

// entryFacts derives the entry facts of one decision from history.
func entryFacts(d Decision, underlying series) (rpc.ScorecardEntry, int, bool) {
	var out rpc.ScorecardEntry
	switch {
	case d.UnderlyingConID == 0:
		out.Reason = ReasonUnderlyingUnknown
		return out, 0, false
	case len(underlying.bars) == 0:
		out.Reason = ReasonHistoryUnavailable
		return out, 0, false
	}
	idx, ok := underlying.entryIndex(d.ExecutedAt)
	if !ok {
		out.Reason = ReasonEntrySessionMissing
		return out, 0, false
	}
	bar := underlying.bars[idx]
	day, close := bar.Day, bar.Close
	out.SessionDay, out.UnderlyingClose = &day, &close
	if d.Instrument != InstrumentOption {
		return out, idx, true
	}
	switch {
	case d.Option == nil:
		out.Reason = ReasonInstrumentMetadata
	case d.FillPrice == nil:
		out.Reason = ReasonFillUnknown
	default:
		t := yearsToExpiry(d.Option.Expiry, day)
		if t <= 0 {
			out.Reason = ReasonImpliedVolUnknown
			break
		}
		call := d.Option.Right == RightCall
		iv := ImpliedVol(*d.FillPrice, close, d.Option.Strike, t, RiskFreeRate, DividendYield, call)
		if iv <= 0 {
			out.Reason = ReasonImpliedVolUnknown
			break
		}
		delta := Delta(close, d.Option.Strike, t, iv, RiskFreeRate, DividendYield, call)
		out.ImpliedVol, out.Delta = &iv, &delta
	}
	return out, idx, true
}

func yearsToExpiry(expiry string, day time.Time) float64 {
	parsed, err := time.Parse(time.DateOnly, expiry)
	if err != nil {
		return 0
	}
	return parsed.Sub(dayStart(day)).Hours() / 24 / 365
}

// exposure is the pre-decision regression of daily underlying log returns on
// the market, and on the market and the name's sector fund together, over at
// most LookbackSessions sessions ending the session before entry. beta,
// rSquared and totalVol come from the market-only fit (the index class reads
// them); when the sector fund's history covers the window, sectorAdjusted is
// true and beta, sectorBeta and idioVol come from the two-factor fit.
// fitted is false when the market did not move across the window.
type exposure struct {
	beta, sectorBeta, idioVol, totalVol, rSquared float64
	fitted, sectorAdjusted                        bool
	sectorReason                                  string
	sessions                                      int
}

func estimateExposure(underlying, market, sector series, entryIdx int) (exposure, string) {
	start := max(0, entryIdx-LookbackSessions)
	if entryIdx-start < MinLookbackSessions {
		return exposure{}, ReasonLookbackShort
	}
	var x, y, xs, ys, s []float64
	for i := start + 1; i < entryIdx; i++ {
		prev, cur := underlying.bars[i-1], underlying.bars[i]
		mPrev, okPrev := market.at(prev.Day)
		mCur, okCur := market.at(cur.Day)
		if !okPrev || !okCur {
			continue
		}
		rm, ru := math.Log(market.bars[mCur].Close/market.bars[mPrev].Close), math.Log(cur.Close/prev.Close)
		x = append(x, rm)
		y = append(y, ru)
		sPrev, okSPrev := sector.at(prev.Day)
		sCur, okSCur := sector.at(cur.Day)
		if okSPrev && okSCur {
			xs = append(xs, rm)
			ys = append(ys, ru)
			s = append(s, math.Log(sector.bars[sCur].Close/sector.bars[sPrev].Close))
		}
	}
	if len(x) < MinLookbackSessions {
		return exposure{}, ReasonMarketHistoryUnavailable
	}
	f := ols(x, y)
	out := exposure{beta: f.beta, idioVol: f.residualSD, totalVol: f.totalSD, rSquared: f.rSquared, fitted: f.ok, sessions: f.n}
	switch {
	case len(sector.bars) == 0:
		out.sectorReason = ReasonSectorUnknown
	case len(s) < MinLookbackSessions:
		out.sectorReason = ReasonSectorHistoryMissing
	default:
		if two := ols2(xs, s, ys); two.ok && two.residualSD > 0 {
			out.beta, out.sectorBeta, out.idioVol, out.sectorAdjusted = two.beta1, two.beta2, two.residualSD, true
		} else {
			// The fund did not move, or moves with the market exactly: the
			// market alone carries the adjustment.
			out.sectorReason = ReasonSectorHistoryMissing
		}
	}
	return out, ""
}

// indexBasis classifies a decision's underlying before any score: list when
// it is on the fixed index list, r_squared when its pre-entry regression on
// the market fits at or above IndexRSquared, empty for a single name. Only
// pre-entry sessions enter, so the class never looks ahead.
func indexBasis(d Decision, exp exposure, estimated bool) string {
	switch {
	case IndexUnderlying(d.Underlying):
		return rpc.ScorecardIndexBasisList
	case estimated && exp.fitted && exp.totalVol > 0 && exp.rSquared >= IndexRSquared:
		return rpc.ScorecardIndexBasisRSquared
	default:
		return ""
	}
}

// scoreContext is what every horizon score of one decision shares.
type scoreContext struct {
	decision   Decision
	entry      rpc.ScorecardEntry
	entryIdx   int
	entryOK    bool
	underlying series
	market     series
	sector     series
	// exposure is estimated once per decision from pre-entry sessions;
	// exposureReason says why it could not be. index scores z on the
	// underlying's total volatility.
	exposure       exposure
	exposureReason string
	index          bool
	marks          []edge.ContractMark
	events         []edge.ContractEvent
	exits          []exitFill
	cost           *FillCost
	costReason     string
}

// exitFill is a later fill on the same contract that left it flat.
type exitFill struct {
	day   time.Time
	price float64
}

func scoreHorizon(c scoreContext, h int) rpc.ScorecardHorizonScore {
	// The half-spread does not depend on the horizon; the deprecated
	// per-horizon keys repeat the decision's slippage for one release.
	out := rpc.ScorecardHorizonScore{Sessions: h, SlippageReason: c.costReason}
	if c.cost != nil {
		out.SlippageBp = c.cost.Bp
	}
	if !c.entryOK {
		out.Reason = c.entry.Reason
		return out
	}
	hIdx := c.entryIdx + h
	if hIdx >= len(c.underlying.bars) {
		out.Reason = ReasonImmature
		return out
	}
	out.Matured = true
	if c.decision.Sign == 0 {
		out.Reason = ReasonInstrumentMetadata
		return out
	}
	entryBar, horizonBar := c.underlying.bars[c.entryIdx], c.underlying.bars[hIdx]
	day, close := horizonBar.Day, horizonBar.Close
	out.HorizonDay, out.HorizonClose = &day, &close
	ru := math.Log(horizonBar.Close / entryBar.Close)
	out.UnderlyingReturn = &ru
	mEntry, okEntry := c.market.at(entryBar.Day)
	mHorizon, okHorizon := c.market.at(horizonBar.Day)
	if !okEntry || !okHorizon {
		out.Reason = ReasonMarketHistoryUnavailable
		return out
	}
	rm := math.Log(c.market.bars[mHorizon].Close / c.market.bars[mEntry].Close)
	out.MarketReturn = &rm
	if c.exposureReason != "" {
		out.Reason = c.exposureReason
		return out
	}
	exp := c.exposure
	if exp.fitted {
		beta := exp.beta
		out.Beta = &beta
	}
	var z float64
	if c.index {
		// The index is the market, or near enough: its decisions time the
		// market or hedge it, so z is its whole move over its whole noise.
		if exp.totalVol <= 0 {
			out.Reason = ReasonTotalVolZero
			return out
		}
		total := exp.totalVol
		out.TotalVolDaily = &total
		z = float64(c.decision.Sign) * ru / (total * math.Sqrt(float64(h)))
	} else {
		// A single name left with no residual noise has a flat series or a
		// flat market across the window; a perfect fit was routed to index.
		if !exp.fitted || exp.idioVol <= 0 {
			out.Reason = ReasonIdioVolZero
			return out
		}
		idio := exp.idioVol
		out.IdioVolDaily = &idio
		adjusted := ru - exp.beta*rm
		if exp.sectorAdjusted {
			sEntry, okSEntry := c.sector.at(entryBar.Day)
			sHorizon, okSHorizon := c.sector.at(horizonBar.Day)
			if !okSEntry || !okSHorizon {
				out.Reason = ReasonSectorHistoryMissing
				return out
			}
			rs := math.Log(c.sector.bars[sHorizon].Close / c.sector.bars[sEntry].Close)
			sectorBeta := exp.sectorBeta
			out.SectorReturn, out.SectorBeta = &rs, &sectorBeta
			adjusted -= sectorBeta * rs
		}
		z = float64(c.decision.Sign) * adjusted / (idio * math.Sqrt(float64(h)))
	}
	out.Z = &z
	if c.decision.Instrument != InstrumentOption {
		out.RVIReason, out.WrapperReason = ReasonNotOption, ReasonNotOption
		return out
	}
	if c.entry.ImpliedVol == nil {
		out.RVIReason, out.WrapperReason = c.entry.Reason, c.entry.Reason
		return out
	}
	rvi := math.Abs(ru) / (*c.entry.ImpliedVol * math.Sqrt(float64(h)/252))
	out.RealisedVsImplied = &rvi
	out.WrapperBase, out.WrapperReason = wrapper(c, entryBar, horizonBar)
	return out
}

// wrapper is the option's P&L to the horizon (or to its exit or lifecycle
// event when earlier) minus the P&L of the delta-equivalent stock position,
// in base currency at execution FX.
func wrapper(c scoreContext, entryBar, horizonBar edge.DailyBar) (*float64, string) {
	d := c.decision
	if d.FillPrice == nil || d.Multiplier == nil || c.entry.Delta == nil {
		return nil, ReasonFillUnknown
	}
	fx := 1.0
	if d.ExecutionFX != nil {
		fx = *d.ExecutionFX
	} else if d.Currency != "" && d.Currency != "USD" {
		// The base currency is unknown to the pure core; a stated FX is
		// required whenever the leg is not in the same currency as the
		// scores' base.
		return nil, ReasonFXUnknown
	}
	valueDay, value, ok := optionValueBy(c, entryBar.Day, horizonBar.Day)
	if !ok {
		return nil, ReasonMarkUnavailable
	}
	sIdx, sOK := c.underlying.at(valueDay)
	if !sOK {
		return nil, ReasonHistoryUnavailable
	}
	signedQty := d.Quantity
	if d.Side == SideSell {
		signedQty = -signedQty
	}
	optionPnL := signedQty * *d.Multiplier * (value - *d.FillPrice) * fx
	stockPnL := *c.entry.Delta * *d.Multiplier * signedQty * (c.underlying.bars[sIdx].Close - entryBar.Close) * fx
	out := optionPnL - stockPnL
	return &out, ""
}

// optionValueBy returns the option's value on the earliest of: a fill that
// left the contract flat, a lifecycle event, or the statement mark on the
// horizon day. Nothing is interpolated.
func optionValueBy(c scoreContext, entryDay, horizonDay time.Time) (time.Time, float64, bool) {
	best, bestDay, found := 0.0, time.Time{}, false
	consider := func(day time.Time, value float64) {
		if !found || day.Before(bestDay) {
			best, bestDay, found = value, day, true
		}
	}
	for _, exit := range c.exits {
		if exit.day.After(entryDay) && !exit.day.After(horizonDay) {
			consider(exit.day, exit.price)
		}
	}
	for _, event := range c.events {
		if !event.Day.After(entryDay) || event.Day.After(horizonDay) {
			continue
		}
		switch event.EventType {
		case "expiration":
			consider(event.Day, 0)
		case "exercise", "assignment":
			if event.Price != nil && *event.Price > 0 {
				consider(event.Day, *event.Price)
				continue
			}
			if c.decision.Option == nil {
				continue
			}
			if idx, ok := c.underlying.at(event.Day); ok {
				spot := c.underlying.bars[idx].Close
				intrinsic := math.Max(0, spot-c.decision.Option.Strike)
				if c.decision.Option.Right == RightPut {
					intrinsic = math.Max(0, c.decision.Option.Strike-spot)
				}
				consider(event.Day, intrinsic)
			}
		}
	}
	if found {
		return bestDay, best, true
	}
	for _, mark := range c.marks {
		if dayKey(mark.Day) == dayKey(horizonDay) && mark.Price > 0 {
			return mark.Day, mark.Price, true
		}
	}
	return time.Time{}, 0, false
}

func dayKey(v time.Time) string { return v.UTC().Format(time.DateOnly) }

func dayStart(v time.Time) time.Time {
	y, m, d := v.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}
