package scorecard

import (
	"maps"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Input is everything Evaluate needs; the daemon assembles it from the
// persisted ledger, the history cache, the retained statements and the
// retained fill quotes.
type Input struct {
	AsOf         time.Time
	EvidenceTo   time.Time
	BaseCurrency string
	Decisions    []Decision
	History      History
	Marks        map[int64][]edge.ContractMark
	Events       map[int64][]edge.ContractEvent
	// Costs is the half-spread per decision id from the retained fill
	// quotes; a decision absent from it carries the reason its fill has
	// none (before capture began, or never observed).
	Costs        map[string]FillCost
	Skipped      map[string]int
	Superseded   int
	HistoryState rpc.ScorecardHistory
}

// Evaluate scores every decision at every horizon and accumulates the
// evidence per sleeve. It is deterministic: the same input yields the same
// result, including the bootstrap intervals and the placebo.
func Evaluate(in Input) rpc.ScorecardResult {
	out := rpc.ScorecardResult{
		SchemaVersion: SchemaVersion, State: rpc.ScorecardStateCurrent, AsOf: in.AsOf.UTC(), EvidenceTo: in.EvidenceTo.UTC(), BaseCurrency: strings.ToUpper(strings.TrimSpace(in.BaseCurrency)),
		PrimaryHorizon: PrimaryHorizon, Horizons: append([]int(nil), Horizons[:]...),
		Sleeves: []rpc.ScorecardSleeve{}, Decisions: []rpc.ScorecardDecision{},
		Coverage: rpc.ScorecardCoverage{RuleOriginated: map[string]int{}, Skipped: map[string]int{}, ScoredByHorizon: map[int]int{}, UnknownByReason: map[string]int{}, SlippageByReason: map[string]int{}, Superseded: in.Superseded},
		History:  in.HistoryState, Method: defaultMethod(), NotExecution: true,
	}
	out.History.MarketSymbol = firstNonEmpty(in.History.MarketSymbol, "SPY")
	out.History.MarketBars = len(in.History.Market)
	if len(in.History.Sector) > 0 {
		out.History.SectorBars = map[string]int{}
		for symbol, bars := range in.History.Sector {
			out.History.SectorBars[symbol] = len(bars)
		}
	}
	maps.Copy(out.Coverage.Skipped, in.Skipped)
	market := newSeries(in.History.Market)
	underlyingSeries := map[int64]series{}
	seriesFor := func(conid int64) series {
		if s, ok := underlyingSeries[conid]; ok {
			return s
		}
		s := newSeries(in.History.Underlying[conid])
		underlyingSeries[conid] = s
		return s
	}
	sectorSeries := map[string]series{}
	sectorFor := func(symbol string) series {
		if symbol == "" {
			return series{}
		}
		if s, ok := sectorSeries[symbol]; ok {
			return s
		}
		s := newSeries(in.History.Sector[symbol])
		sectorSeries[symbol] = s
		return s
	}
	exits := exitsByContract(in.Decisions)

	type scored struct {
		decision Decision
		entry    rpc.ScorecardEntry
		scores   []rpc.ScorecardHorizonScore
		slippage *rpc.ScorecardSlippage
	}
	rows := make([]scored, 0, len(in.Decisions))
	for _, d := range in.Decisions {
		out.Coverage.Ledger++
		if d.Instrument == InstrumentOption {
			out.Coverage.Options++
		} else {
			out.Coverage.Stocks++
		}
		if d.Stream == "" {
			d.Stream = StreamOf(d.Action)
		}
		if d.ClusterKey == "" || d.UnitKey == "" {
			d.ClusterKey, d.UnitKey = unitKeys(d)
		}
		underlying := seriesFor(d.UnderlyingConID)
		sectorFund := in.History.SectorFunds[d.UnderlyingConID]
		sector := sectorFor(sectorFund)
		entry, entryIdx, entryOK := entryFacts(d, underlying)
		var exp exposure
		expReason, estimated := "", false
		if entryOK {
			exp, expReason = estimateExposure(underlying, market, sector, entryIdx)
			estimated = expReason == ""
			if estimated && exp.fitted && exp.totalVol > 0 {
				r2 := exp.rSquared
				entry.MarketRSquared = &r2
			}
		}
		// The stream is decided at evaluation, from pre-entry history: the
		// ledger row keeps its action stream, an index decision is published
		// in the index counterpart.
		entry.IndexBasis = indexBasis(d, exp, estimated)
		if entry.IndexBasis != "" {
			d.Stream = indexStream(d.Stream)
		} else if estimated {
			entry.Sector, entry.SectorFund = in.History.SectorNames[d.UnderlyingConID], sectorFund
			if exp.sectorAdjusted {
				entry.MarketAdjustment = MarketAdjustmentSPYSector
			} else {
				entry.MarketAdjustment, entry.SectorReason = MarketAdjustmentSPY, exp.sectorReason
			}
		}
		switch {
		case d.Sleeve == rpc.ScorecardSleeveRule:
			out.Coverage.RuleOriginated[firstNonEmpty(d.Origin, "rule")]++
		case d.Stream == rpc.ScorecardStreamEntries:
			out.Coverage.Entries++
		case d.Stream == rpc.ScorecardStreamExits:
			out.Coverage.Exits++
		case d.Stream == rpc.ScorecardStreamIndexEntries:
			out.Coverage.IndexEntries++
		default:
			out.Coverage.IndexExits++
		}
		slippage := decisionSlippage(d, in.Costs)
		if slippage.Status == "measured" {
			out.Coverage.SlippageQuoted++
		} else {
			out.Coverage.SlippageByReason[slippage.Reason]++
		}
		c := scoreContext{decision: d, entry: entry, entryIdx: entryIdx, entryOK: entryOK, underlying: underlying, market: market, sector: sector,
			exposure: exp, exposureReason: expReason, index: entry.IndexBasis != "", marks: in.Marks[d.ConID], events: in.Events[d.ConID], exits: exits[d.ConID], costReason: slippage.Reason}
		if cost, ok := in.Costs[d.ID]; ok && slippage.Status == "measured" {
			c.cost = &cost
		}
		scores := make([]rpc.ScorecardHorizonScore, 0, len(Horizons))
		for _, h := range Horizons {
			score := scoreHorizon(c, h)
			if score.Z != nil {
				out.Coverage.ScoredByHorizon[h]++
			} else if score.Reason != "" {
				out.Coverage.UnknownByReason[score.Reason]++
			}
			scores = append(scores, score)
		}
		rows = append(rows, scored{decision: d, entry: entry, scores: scores, slippage: &slippage})
	}

	// Rule-originated fills are ledger rows, never evidence for a sleeve:
	// a protection stop or an assignment is not a decision anyone made.
	sleeves := map[string]bool{}
	for _, row := range rows {
		if row.decision.Sleeve != rpc.ScorecardSleeveRule {
			sleeves[row.decision.Sleeve] = true
		}
	}
	names := make([]string, 0, len(sleeves))
	for name := range sleeves {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, h := range Horizons {
		for _, name := range names {
			for _, stream := range scorecardStreams {
				var decisions []Decision
				var scores []rpc.ScorecardHorizonScore
				var slippages []*rpc.ScorecardSlippage
				for _, row := range rows {
					if row.decision.Sleeve != name || row.decision.Stream != stream {
						continue
					}
					decisions = append(decisions, row.decision)
					scores = append(scores, row.scores[horizonIndex(h)])
					slippages = append(slippages, row.slippage)
				}
				sleeve := sleeveEvidence(name, stream, h, decisions, scores)
				sleeve.Slippage = slippageSummary(slippages)
				out.Sleeves = append(out.Sleeves, sleeve)
			}
		}
	}
	applyHolm(out.Sleeves, PrimaryHorizon)

	for _, row := range rows {
		out.Decisions = append(out.Decisions, publicDecision(row.decision, row.entry, row.scores, row.slippage))
	}
	sort.SliceStable(out.Decisions, func(i, j int) bool {
		if !out.Decisions[i].ExecutedAt.Equal(out.Decisions[j].ExecutedAt) {
			return out.Decisions[i].ExecutedAt.After(out.Decisions[j].ExecutedAt)
		}
		return out.Decisions[i].ID < out.Decisions[j].ID
	})
	out.DecisionsTotal = len(out.Decisions)
	return out
}

// decisionSlippage is the served half-spread of one decision: the retained
// cost when the daemon found one, else the reason none exists. A fill
// before the daemon retained quotes has none and never will; a later fill
// without a record was never observed by the daemon.
func decisionSlippage(d Decision, costs map[string]FillCost) rpc.ScorecardSlippage {
	cost, ok := costs[d.ID]
	switch {
	case !ok && d.ExecutedAt.Before(FillQuoteCaptureSince):
		return rpc.ScorecardSlippage{Status: "unknown", Reason: ReasonNoFillQuote}
	case !ok:
		return rpc.ScorecardSlippage{Status: "unknown", Reason: ReasonExecutionNotObserved}
	}
	out := rpc.ScorecardSlippage{Match: cost.Match, Fills: cost.Fills, Quoted: cost.Quoted, HalfSpread: cost.HalfSpread, Currency: cost.Currency, HalfSpreadBase: cost.HalfSpreadBase, Bp: cost.Bp, GapMS: cost.GapMS, Reason: cost.Reason}
	if cost.HalfSpread == nil {
		out.Status = "unknown"
		if out.Reason == "" {
			out.Reason = "quote_unavailable"
		}
		return out
	}
	out.Status = "measured"
	return out
}

// slippageSummary sums a stream's measured half-spreads in base currency
// and counts the decisions without one by reason. Quoted counts the
// decisions whose half-spread enters the base sum; a half-spread measured
// without an FX rate is counted under fx_unavailable instead, so quoted and
// unquoted together are the decisions, while its basis points (which need
// no rate) still enter the mean.
func slippageSummary(slippages []*rpc.ScorecardSlippage) *rpc.ScorecardSlippageSummary {
	if len(slippages) == 0 {
		return nil
	}
	out := &rpc.ScorecardSlippageSummary{Decisions: len(slippages), Unquoted: map[string]int{}}
	var bps []float64
	for _, s := range slippages {
		if s == nil || s.Status != "measured" {
			reason := "unknown"
			if s != nil && s.Reason != "" {
				reason = s.Reason
			}
			out.Unquoted[reason]++
			continue
		}
		if s.Bp != nil {
			bps = append(bps, *s.Bp)
		}
		if s.HalfSpreadBase == nil {
			out.Unquoted[firstNonEmpty(s.Reason, ReasonFXUnavailable)]++
			continue
		}
		out.Quoted++
		out.HalfSpreadBase += *s.HalfSpreadBase
	}
	if len(bps) > 0 {
		m := mean(bps)
		out.MeanBp = &m
	}
	return out
}

// scorecardStreams is the publication order of a sleeve's streams: the
// single-name entries and exits, then the index entries and exits.
var scorecardStreams = [...]string{rpc.ScorecardStreamEntries, rpc.ScorecardStreamExits, rpc.ScorecardStreamIndexEntries, rpc.ScorecardStreamIndexExits}

func horizonIndex(h int) int {
	for i, value := range Horizons {
		if value == h {
			return i
		}
	}
	return 0
}

// exitsByContract lists, per contract, the fills that left it flat.
func exitsByContract(decisions []Decision) map[int64][]exitFill {
	out := map[int64][]exitFill{}
	for _, d := range decisions {
		if d.FillPrice == nil || !almostZero(d.PositionAfter) {
			continue
		}
		out[d.ConID] = append(out[d.ConID], exitFill{day: dayStart(d.ExecutedAt), price: *d.FillPrice})
	}
	for conid, fills := range out {
		sort.Slice(fills, func(i, j int) bool { return fills[i].day.Before(fills[j].day) })
		out[conid] = fills
	}
	return out
}

// unit is one name-week of evidence: the decisions of one underlying in one
// sleeve, stream and ISO entry week, counted once through their mean z.
// market marks a unit of an index stream: every such unit is a position on
// the market, whatever contract carried it, and overlaps the others fully.
type unit struct {
	key    string
	week   string
	conid  int64
	market bool
	day    time.Time
	sumZ   float64
	sumMkt float64
	count  int
}

func (u unit) mean() float64 { return u.sumZ / float64(u.count) }

// Sample is the effective sample behind a stream's evidence: n units whose
// sum is S, with the null covariance V of the sum from overlapping horizons
// (V ≥ n). NEff = n²/V independent unit-variance units carry the same
// information; SEff = S·n/V is their sum, so the score per unit S/n is
// SEff/NEff.
type Sample struct {
	Units      int
	Sum        float64
	Covariance float64
	NEff, SEff float64
}

// EffectiveSample reduces the units to their effective sample at horizon h
// sessions. Under no information, two units' z share the residual returns
// of the sessions their horizons overlap, so their covariance is the
// overlapping share of h: 1 − gap/h on the same name, scaled by
// CrossNameResidualCorrelation across names. Index-stream units are all
// the same name, the market: SPY, SPX, XSP and QQQ options sit on
// different contract ids but move together, so a cross-name discount would
// overstate their effective sample. Only entry days and h enter, never
// anything after entry. The gap is measured in calendar days against a
// horizon of 7h/5 days.
func EffectiveSample(units []unit, h int) Sample {
	n := len(units)
	out := Sample{Units: n}
	if n == 0 {
		return out
	}
	span := float64(h) * 7 / 5
	v := float64(n)
	for i, a := range units {
		out.Sum += a.mean()
		for j := i + 1; j < n; j++ {
			b := units[j]
			gap := math.Abs(a.day.Sub(b.day).Hours() / 24)
			if gap >= span {
				continue
			}
			rho := CrossNameResidualCorrelation
			if a.conid == b.conid || (a.market && b.market) {
				rho = 1
			}
			v += 2 * rho * (1 - gap/span)
		}
	}
	out.Covariance = v
	out.NEff = float64(n) * float64(n) / v
	out.SEff = out.Sum * float64(n) / v
	return out
}

// sleeveEvidence accumulates one sleeve stream's evidence at one horizon:
// each name-week unit counts once, through the mean z of its decisions, and
// the units are reduced to their effective sample by the overlap covariance.
// Only the entries stream is gated; the index entries stream also reports
// the realised-vs-implied median of its bought puts.
func sleeveEvidence(name, stream string, h int, decisions []Decision, scores []rpc.ScorecardHorizonScore) rpc.ScorecardSleeve {
	gated := stream == rpc.ScorecardStreamEntries
	market := stream == rpc.ScorecardStreamIndexEntries || stream == rpc.ScorecardStreamIndexExits
	out := rpc.ScorecardSleeve{Sleeve: name, Stream: stream, Horizon: h, Gated: gated, Decisions: len(decisions), Unknown: map[string]int{}}
	if gated {
		out.Gate = rpc.ScorecardGateAccruing
	}
	byKey := map[string]*unit{}
	var wrappers, rvis, longPutRVIs []float64
	for i, score := range scores {
		if score.Z == nil {
			if score.Reason != "" {
				out.Unknown[score.Reason]++
			}
			continue
		}
		out.Scored++
		if score.SectorBeta != nil {
			out.SectorAdjusted++
		}
		d := decisions[i]
		if d.ClusterKey == "" || d.UnitKey == "" {
			d.ClusterKey, d.UnitKey = unitKeys(d)
		}
		u := byKey[d.UnitKey]
		if u == nil {
			u = &unit{key: d.UnitKey, week: d.ClusterKey, conid: d.UnderlyingConID, market: market, day: dayStart(d.ExecutedAt)}
			byKey[d.UnitKey] = u
		}
		if day := dayStart(d.ExecutedAt); day.Before(u.day) {
			u.day = day
		}
		u.sumZ += *score.Z
		u.count++
		if score.MarketReturn != nil {
			u.sumMkt += *score.MarketReturn
		}
		if score.WrapperBase != nil {
			wrappers = append(wrappers, *score.WrapperBase)
		}
		if score.RealisedVsImplied != nil {
			rvis = append(rvis, *score.RealisedVsImplied)
			if stream == rpc.ScorecardStreamIndexEntries && longPut(d) {
				longPutRVIs = append(longPutRVIs, *score.RealisedVsImplied)
			}
		}
	}
	units := make([]unit, 0, len(byKey))
	for _, u := range byKey {
		units = append(units, *u)
	}
	// Chronological: by entry day, then by key for a stable order.
	sort.Slice(units, func(i, j int) bool {
		if !units[i].day.Equal(units[j].day) {
			return units[i].day.Before(units[j].day)
		}
		return units[i].key < units[j].key
	})
	weeks := map[string]bool{}
	means := make([]float64, 0, len(units))
	var up, down []float64
	for _, u := range units {
		weeks[u.week] = true
		m := u.mean()
		means = append(means, m)
		if u.sumMkt > 0 {
			up = append(up, m)
		} else {
			down = append(down, m)
		}
	}
	n := len(units)
	out.Units, out.Clusters, out.Weeks = n, n, len(weeks)
	if len(wrappers) > 0 {
		low, high := bootstrapMeanCI(wrappers, BootstrapResamples)
		out.Wrapper = &rpc.ScorecardWrapperSummary{Count: len(wrappers), MeanBase: mean(wrappers), CILow: low, CIHigh: high}
	}
	if len(rvis) > 0 {
		out.RealisedVsImplied = &rpc.ScorecardRealisedVsImplied{Count: len(rvis), Median: median(rvis)}
	}
	if len(longPutRVIs) > 0 {
		out.LongPutRealisedVsImplied = &rpc.ScorecardRealisedVsImplied{Count: len(longPutRVIs), Median: median(longPutRVIs)}
	}
	if n == 0 {
		out.EValueAgainst = true
		return out
	}
	sample := EffectiveSample(units, h)
	score := sample.Sum / float64(n)
	out.SumZ, out.EffectiveUnits = sample.Sum, sample.NEff
	out.DirectionalScore, out.MeanZ = &score, &score
	pm, psd := PosteriorScore(sample.NEff, sample.SEff)
	p0, p005, p010 := PosteriorAbove(sample.NEff, sample.SEff, 0), PosteriorAbove(sample.NEff, sample.SEff, 0.05), PosteriorAbove(sample.NEff, sample.SEff, 0.10)
	out.PosteriorMean, out.PosteriorSD = &pm, &psd
	out.PScoreAbove0, out.PScoreAbove005, out.PScoreAbove010 = &p0, &p005, &p010
	out.PAbove005, out.PAbove010 = &p005, &p010
	out.EValue, out.EValueAgainst = EValue(sample.NEff, sample.SEff)
	out.FutilityEValue = FutilityEValue(sample.NEff, sample.SEff)
	if projected, ok := UnitsToEstablish(score); ok {
		out.UnitsToEstablish, out.ClustersToEstablish = &projected, &projected
		if pace := sample.NEff / float64(len(weeks)); pace > 0 {
			weeksNeeded := int(math.Ceil(float64(projected) / pace))
			out.WeeksToEstablish = &weeksNeeded
		}
	}
	gates := rpc.ScorecardGates{EValue: out.EValue >= EstablishedEValue, Units: sample.NEff >= EstablishedUnits, Weeks: len(weeks) >= EstablishedWeeks, Posterior: p0 >= EstablishedPosterior, UpUnits: len(up), DownUnits: len(down)}
	gates.Clusters, gates.UpClusters, gates.DownClusters = gates.Units, gates.UpUnits, gates.DownUnits
	if n >= 2 {
		first, second := mean(means[:n/2]), mean(means[n/2:])
		gates.FirstHalfMeanZ, gates.SecondHalfMeanZ = &first, &second
		gates.HalvesAgree = sameSign(first, second)
	}
	if len(up) > 0 && len(down) > 0 {
		upMean, downMean := mean(up), mean(down)
		gates.UpMarketMeanZ, gates.DownMarketMeanZ = &upMean, &downMean
		gates.MarketsAgree = sameSign(upMean, downMean)
	}
	placebo := placeboTest(units, sample)
	out.Placebo = &placebo
	gates.Placebo = placebo.Rate <= PlaceboMaxRate && placebo.StandardisedSD <= PlaceboMaxStandardisedSD
	out.Gates = gates
	if !gated {
		return out
	}
	switch {
	case gates.EValue && gates.Units && gates.Weeks && gates.Posterior && gates.Placebo && gates.HalvesAgree && gates.MarketsAgree:
		out.Gate = rpc.ScorecardGateEstablished
	case sample.NEff >= FutileUnits && out.FutilityEValue >= FutileEValue:
		out.Gate = rpc.ScorecardGateFutile
	default:
		out.Gate = rpc.ScorecardGateAccruing
	}
	return out
}

// placeboTest redraws every unit's sign at random PlaceboReplicates times
// (seeded, so the same evidence yields the same placebo) and re-reads the
// statistical bar on the same effective sample. Under no information the
// sign choice is a coin, so the bar must stay out of reach; a sleeve whose
// shuffles reach it more often than PlaceboMaxRate, or whose shuffled sums
// spread wider than the null covariance by more than
// PlaceboMaxStandardisedSD, draws its evidence from the magnitudes, not the
// directions, and is not established.
func placeboTest(units []unit, sample Sample) rpc.ScorecardPlacebo {
	out := rpc.ScorecardPlacebo{Replicates: PlaceboReplicates}
	n := len(units)
	if n == 0 || sample.Covariance <= 0 {
		return out
	}
	rng := rand.New(rand.NewPCG(0x9e3779b9, uint64(n)))
	var sumSq float64
	for range PlaceboReplicates {
		s := 0.0
		for _, u := range units {
			if rng.IntN(2) == 0 {
				s += u.mean()
			} else {
				s -= u.mean()
			}
		}
		standardised := s / math.Sqrt(sample.Covariance)
		sumSq += standardised * standardised
		sEff := s * float64(n) / sample.Covariance
		e, _ := EValue(sample.NEff, sEff)
		if e >= EstablishedEValue && sample.NEff >= EstablishedUnits && PosteriorAbove(sample.NEff, sEff, 0) >= EstablishedPosterior {
			out.Established++
		}
	}
	out.Rate = float64(out.Established) / float64(PlaceboReplicates)
	out.StandardisedSD = math.Sqrt(sumSq / float64(PlaceboReplicates))
	return out
}

// longPut reports whether a decision buys a put: in the index entries
// stream, a hedge that pays when the market falls.
func longPut(d Decision) bool {
	return d.Instrument == InstrumentOption && d.Option != nil && d.Option.Right == RightPut && d.Side == SideBuy
}

func sameSign(a, b float64) bool {
	return a != 0 && b != 0 && math.Signbit(a) == math.Signbit(b)
}

// applyHolm corrects the sleeves' p-values (the 1/E bound) across live
// sleeves' entries streams at the primary horizon; with one sleeve nothing
// is adjusted.
func applyHolm(sleeves []rpc.ScorecardSleeve, h int) {
	var idx []int
	var p []float64
	for i := range sleeves {
		if sleeves[i].Horizon != h || !sleeves[i].Gated || sleeves[i].Units == 0 {
			continue
		}
		idx = append(idx, i)
		value := 1.0
		if sleeves[i].EValue > 0 {
			value = math.Min(1, 1/sleeves[i].EValue)
		}
		p = append(p, value)
	}
	if len(idx) < 2 {
		return
	}
	adjusted := Holm(p)
	for k, i := range idx {
		value := adjusted[k]
		sleeves[i].HolmAdjustedP = &value
	}
}

func publicDecision(d Decision, entry rpc.ScorecardEntry, scores []rpc.ScorecardHorizonScore, slippage *rpc.ScorecardSlippage) rpc.ScorecardDecision {
	out := rpc.ScorecardDecision{
		ID: d.ID, Sleeve: d.Sleeve, Stream: firstNonEmpty(d.Stream, StreamOf(d.Action)), Origin: d.Origin, StrategyRef: d.StrategyRef, Underlying: d.Underlying, Symbol: d.Symbol, Instrument: d.Instrument,
		Action: d.Action, Side: d.Side, Sign: d.Sign, Quantity: d.Quantity, FillPrice: d.FillPrice, Currency: d.Currency, NotionalBase: d.NotionalBase,
		ExecutedAt: d.ExecutedAt, ClusterKey: d.ClusterKey, UnitKey: d.UnitKey, Entry: entry, Scores: scores, Slippage: slippage,
	}
	if d.Option != nil {
		out.Option = &rpc.ScorecardOptionContract{Expiry: d.Option.Expiry, Strike: d.Option.Strike, Right: d.Option.Right, Multiplier: d.Option.Multiplier}
	}
	return out
}

func defaultMethod() rpc.ScorecardMethod {
	units := "One unit of evidence is a name-week: the decisions of one underlying in one sleeve, stream and ISO entry week count once, through their mean z. Several names in one week are several units."
	return rpc.ScorecardMethod{
		Direction:           "Single names: z = sign × (r_u − β·r_SPY − β_s·r_sector) / (σ_idio·√h), the sector term dropped when the name's sector fund is unknown or its history short; index underlyings: z = sign × r_u / (σ_total·√h). Log returns run from the execution-session close to the close h sessions later, so the intraday move after the fill is excluded; sign is +1 for long exposure and −1 for short (a long put or short call is −1).",
		DirectionalScore:    "The directional score is the mean z per unit: how far, in units of its own daily noise, the name moved the decision's way net of the market and its sector. It is not an information coefficient (no correlation is estimated).",
		Index:               "An underlying is an index when it is SPY, SPX, SPXW, XSP, QQQ, NDX, IWM, RUT or DIA, or when its pre-entry regression on SPY has R² ≥ 0.90: it is the market, or near enough, so its decisions are market timing and hedges and a market-adjusted residual is noise. σ_total is the standard deviation of its own daily log returns over the same pre-entry window.",
		Lookback:            "β, β_s, σ_idio, σ_total and R² from OLS of daily log returns on SPY (and on SPY and the sector fund together) over the 252 sessions ending the session before entry; at least 60 sessions; nothing after entry enters. The sector fund is the S&P 500 sector fund of the name's GICS sector (XLK, XLF, XLV, XLY, XLP, XLE, XLI, XLB, XLU, XLRE, XLC); a name off the S&P 500 is adjusted on SPY alone.",
		Streams:             "Entries (open, add) ask whether the name moves with the decision and carry the gates and the Holm correction; exits (trim, exit) ask whether it moves against the position afterwards and are reported with the same statistics, never gated. Decisions on an index underlying form index_entries and index_exits (did the index move the way the decision positioned for?), reported with the same statistics plus the realised-vs-implied median of bought puts, never gated. Fills Canary's rules or the broker's lifecycle produced are the rule sleeve and count for no owner stream.",
		Units:               units,
		Clusters:            units,
		Overlap:             "Units whose horizons overlap share returns: under no information the covariance of two units' z is the overlapping share of the horizon (1 − gap/h, the gap in calendar days against 7h/5), fully on the same name (every index-stream unit is the market, one name) and at 0.1 across names. The null covariance V of the sum of n units gives n_eff = n²/V effective units and S_eff = S·n/V; every sequential statistic reads the effective sample.",
		Posterior:           "Posterior on the directional score with prior N(0, 0.1²) and unit variance per effective unit: P(score > 0), P(score > 0.05) and P(score > 0.10).",
		EValue:              "Mixture-SPRT e-value E = (1+n_eff·τ²)^(−1/2)·exp(τ²·S_eff²/(2(1+n_eff·τ²))), τ = 0.2, reported only while S_eff > 0, else 0 (against). It is the weight of evidence for information in the decisions and bounds the chance of a false established at 1/20 however often it is read.",
		Futility:            "Futility e-value exp(−0.05·S_eff + 0.05²·n_eff/2): the likelihood of no information against a score of 0.05, the smallest worth a risk allocation. It grows while the decisions carry none and calls a sleeve with a score of 0.05 or more futile with probability at most 1/20.",
		Placebo:             "The units' signs are redrawn at random 200 times (seeded) and the statistical bar re-read on the same effective sample; the share of shuffles reaching it must not exceed 5%, and the shuffled sums' standard deviation against the null covariance must not exceed 1.2 (about 1 when the unit noise is calibrated), else the evidence comes from the magnitudes, not the directions.",
		Gates:               "established: E ≥ 20, n_eff ≥ 60 effective units over ≥ 26 entry weeks, P(score > 0) ≥ 0.975, the placebo within 5% and calibrated within 1.2, and the same sign of mean z in both chronological halves and in up- and down-market units; futile: n_eff ≥ 60 and futility e-value ≥ 20; otherwise accruing. Holm correction across sleeves when more than one is live. Proposed 2026-10-19; the owner decides the thresholds.",
		Wrapper:             "Option P&L to the horizon, or to the exit or lifecycle event when earlier, minus the P&L of a static delta-equivalent stock position (delta at entry × multiplier × signed quantity), in base currency at execution FX; the option value is a statement mark on the horizon day, the closing fill, zero at expiration or intrinsic at exercise/assignment, never interpolated.",
		RealisedVsImplied:   "|r_u| / (IV·√(h/252)), IV inverted from the fill by Black-Scholes at the execution-session close.",
		Slippage:            "Half-spread paid, (fill − mid) × side × quantity × multiplier, from the quote the daemon retained at the fill: by execution id, else the retained fill on the same contract nearest in time within 5 minutes; summed per sleeve and stream in base currency. A fill before 2026-10-18 has no quote (no_fill_quote); a later fill without one was not observed by the daemon (execution_not_observed) or its quote was unavailable (the record's reason).",
		RiskFreeRate:        RiskFreeRate,
		DividendYield:       DividendYield,
		NoCausalClaim:       true,
		NotInvestmentAdvice: true,
	}
}
