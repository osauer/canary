package cli

import (
	"math"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/scorecard"
)

// goldenScorecard is a synthetic accruing scorecard: one owner sleeve with
// its entries and exits streams and an index entries stream (its empty index
// exits stream is not rendered), one rule-originated fill, stock, option and
// index decisions, a few unknowns, and one decision with a measured
// half-spread beside the fills before quote capture and one the daemon
// never observed. Each stream's statistics are derived from its stated unit
// means by goldenStream, so the fixture carries no number the pure core's
// arithmetic could not produce.
func goldenScorecard() rpc.ScorecardResult {
	at := goldenAt
	entryDay := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	horizonDay := entryDay.AddDate(0, 0, 14)
	entries := func(h int) rpc.ScorecardSleeve {
		return goldenStream(rpc.ScorecardSleeve{
			Sleeve: "owner", Stream: rpc.ScorecardStreamEntries, Horizon: h, Gated: true, Decisions: 14, Scored: 11, SectorAdjusted: 8, Weeks: 7,
			Slippage:          &rpc.ScorecardSlippageSummary{Decisions: 14, Quoted: 3, HalfSpreadBase: 41.3, MeanBp: new(6.2), Unquoted: map[string]int{"no_fill_quote": 10, "execution_not_observed": 1}},
			Wrapper:           &rpc.ScorecardWrapperSummary{Count: 5, MeanBase: -412.5, CILow: -1280.0, CIHigh: 310.0},
			RealisedVsImplied: &rpc.ScorecardRealisedVsImplied{Count: 5, Median: 0.84},
			Unknown:           map[string]int{"immature": 2, "history_unavailable": 1},
		}, 7.4, []goldenUnit{{0.9, true}, {0.3, false}, {0.5, true}, {0.1, false}, {0.6, true}, {-0.2, false}, {0.3, true}, {0.4, true}, {-0.11, false}})
	}
	exits := func(h int) rpc.ScorecardSleeve {
		return goldenStream(rpc.ScorecardSleeve{
			Sleeve: "owner", Stream: rpc.ScorecardStreamExits, Horizon: h, Decisions: 7, Scored: 5, SectorAdjusted: 4, Weeks: 4,
			Slippage: &rpc.ScorecardSlippageSummary{Decisions: 7, Quoted: 0, Unquoted: map[string]int{"no_fill_quote": 7}},
			Unknown:  map[string]int{"immature": 2},
		}, 3.6, []goldenUnit{{-0.5, true}, {-0.1, false}, {0.22, true}, {-0.10, false}})
	}
	indexEntries := func(h int) rpc.ScorecardSleeve {
		return goldenStream(rpc.ScorecardSleeve{
			Sleeve: "owner", Stream: rpc.ScorecardStreamIndexEntries, Horizon: h, Decisions: 4, Scored: 3, Weeks: 3,
			Slippage:                 &rpc.ScorecardSlippageSummary{Decisions: 4, Quoted: 1, HalfSpreadBase: 4.3, MeanBp: new(41.2), Unquoted: map[string]int{"no_fill_quote": 3}},
			Wrapper:                  &rpc.ScorecardWrapperSummary{Count: 3, MeanBase: -220.0, CILow: -610.0, CIHigh: 140.0},
			RealisedVsImplied:        &rpc.ScorecardRealisedVsImplied{Count: 3, Median: 0.71},
			LongPutRealisedVsImplied: &rpc.ScorecardRealisedVsImplied{Count: 2, Median: 0.63},
			Unknown:                  map[string]int{"immature": 1},
		}, 2.1, []goldenUnit{{0.90, true}, {-0.35, true}, {0.71, false}})
	}
	indexExits := func(h int) rpc.ScorecardSleeve {
		return rpc.ScorecardSleeve{Sleeve: "owner", Stream: rpc.ScorecardStreamIndexExits, Horizon: h, EValueAgainst: true, Unknown: map[string]int{}}
	}
	return rpc.ScorecardResult{
		SchemaVersion: "canary-scorecard-v2", State: rpc.ScorecardStateCurrent, AsOf: at, EvidenceTo: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC), BaseCurrency: "EUR",
		PrimaryHorizon: 10, Horizons: []int{5, 10, 21},
		Sleeves: []rpc.ScorecardSleeve{entries(5), exits(5), indexEntries(5), indexExits(5), entries(10), exits(10), indexEntries(10), indexExits(10), entries(21), exits(21), indexEntries(21), indexExits(21)},
		Decisions: []rpc.ScorecardDecision{
			{ID: "decision_4e5f607182930a1b", Sleeve: "owner", Stream: rpc.ScorecardStreamIndexEntries, Underlying: "SPY", Symbol: "SPY 260930P00600000", Instrument: "option", Option: &rpc.ScorecardOptionContract{Expiry: "2026-09-30", Strike: 600, Right: "put", Multiplier: 100},
				Action: "open", Side: "buy", Sign: -1, Quantity: 2, FillPrice: new(6.10), Currency: "USD", NotionalBase: new(1098.0), ExecutedAt: entryDay.Add(3*24*time.Hour + 15*time.Hour), ClusterKey: "owner:2026-W35", UnitKey: "owner:2026-W35:756733",
				Entry:  rpc.ScorecardEntry{SessionDay: new(entryDay.AddDate(0, 0, 3)), UnderlyingClose: new(641.2), MarketRSquared: new(1.0), IndexBasis: "list", ImpliedVol: new(0.17), Delta: new(-0.21)},
				Scores: []rpc.ScorecardHorizonScore{{Sessions: 5, Matured: true, Z: new(-0.35), TotalVolDaily: new(0.009), SlippageBp: new(41.2)}, {Sessions: 10, Matured: true, Z: new(-0.48), TotalVolDaily: new(0.009), RealisedVsImplied: new(0.52), WrapperBase: new(-310.4), SlippageBp: new(41.2)}, {Sessions: 21, Reason: "immature", SlippageBp: new(41.2)}},
				// Two contracts at 6.10 against a mid of 6.075: 0.025 × 2 × 100 = 5 USD,
				// 41.2 bp of the mid, 4.30 EUR at 0.86.
				Slippage: &rpc.ScorecardSlippage{Status: "measured", Match: "exec_id", Fills: 1, Quoted: 1, HalfSpread: new(5.0), Currency: "USD", HalfSpreadBase: new(4.3), Bp: new(41.2), GapMS: new(int64(2600))}},
			{ID: "decision_3d4e5f6071829304", Sleeve: "rule", Stream: rpc.ScorecardStreamExits, Origin: "proposal:trailing_stop", Underlying: "SYND", Symbol: "SYND", Instrument: "stock",
				Action: "exit", Side: "sell", Sign: -1, Quantity: 25, FillPrice: new(31.40), Currency: "USD", NotionalBase: new(706.5), ExecutedAt: entryDay.Add(2 * 24 * time.Hour), ClusterKey: "rule:2026-W35", UnitKey: "rule:2026-W35:4",
				Entry:    rpc.ScorecardEntry{SessionDay: new(entryDay.AddDate(0, 0, 2)), UnderlyingClose: new(31.2), MarketAdjustment: "spy", SectorReason: "sector_unknown"},
				Scores:   []rpc.ScorecardHorizonScore{{Sessions: 5, Matured: true, Z: new(0.4), SlippageReason: "no_fill_quote"}, {Sessions: 10, Matured: true, Z: new(0.9), RVIReason: "not_option", WrapperReason: "not_option", SlippageReason: "no_fill_quote"}, {Sessions: 21, Reason: "immature", SlippageReason: "no_fill_quote"}},
				Slippage: &rpc.ScorecardSlippage{Status: "unknown", Reason: "no_fill_quote"}},
			{ID: "decision_0a1b2c3d4e5f6071", Sleeve: "owner", Stream: rpc.ScorecardStreamEntries, Underlying: "SYNA", Symbol: "SYNA 260918C00120000", Instrument: "option", Option: &rpc.ScorecardOptionContract{Expiry: "2026-09-18", Strike: 120, Right: "call", Multiplier: 100},
				Action: "open", Side: "buy", Sign: 1, Quantity: 3, FillPrice: new(4.20), Currency: "USD", NotionalBase: new(1134.0), ExecutedAt: entryDay.Add(15 * time.Hour), ClusterKey: "owner:2026-W35", UnitKey: "owner:2026-W35:1",
				Entry:    rpc.ScorecardEntry{SessionDay: &entryDay, UnderlyingClose: new(118.4), MarketAdjustment: "spy_sector", Sector: "Information Technology", SectorFund: "XLK", ImpliedVol: new(0.42), Delta: new(0.47)},
				Scores:   []rpc.ScorecardHorizonScore{{Sessions: 5, Matured: true, Z: new(0.8), SectorBeta: new(1.2), SlippageReason: "execution_not_observed"}, {Sessions: 10, Matured: true, HorizonDay: &horizonDay, HorizonClose: new(124.1), Z: new(1.12), SectorBeta: new(1.2), SectorReturn: new(0.021), RealisedVsImplied: new(0.56), WrapperBase: new(-96.3), SlippageReason: "execution_not_observed"}, {Sessions: 21, Reason: "immature", SlippageReason: "execution_not_observed"}},
				Slippage: &rpc.ScorecardSlippage{Status: "unknown", Reason: "execution_not_observed"}},
			{ID: "decision_1b2c3d4e5f607182", Sleeve: "owner", Stream: rpc.ScorecardStreamExits, Underlying: "SYNB", Symbol: "SYNB", Instrument: "stock",
				Action: "trim", Side: "sell", Sign: -1, Quantity: 40, FillPrice: new(52.10), Currency: "USD", NotionalBase: new(1875.6), ExecutedAt: entryDay.Add(-3 * 24 * time.Hour), ClusterKey: "owner:2026-W34", UnitKey: "owner:2026-W34:2",
				Entry:    rpc.ScorecardEntry{SessionDay: new(entryDay.AddDate(0, 0, -3)), UnderlyingClose: new(51.9), MarketAdjustment: "spy_sector", Sector: "Health Care", SectorFund: "XLV"},
				Scores:   []rpc.ScorecardHorizonScore{{Sessions: 5, Matured: true, Z: new(-0.3), SlippageReason: "no_fill_quote"}, {Sessions: 10, Matured: true, Z: new(-0.62), RVIReason: "not_option", WrapperReason: "not_option", SlippageReason: "no_fill_quote"}, {Sessions: 21, Matured: true, Z: new(0.1), SlippageReason: "no_fill_quote"}},
				Slippage: &rpc.ScorecardSlippage{Status: "unknown", Reason: "no_fill_quote"}},
			{ID: "decision_2c3d4e5f60718293", Sleeve: "owner", Stream: rpc.ScorecardStreamEntries, Underlying: "SYNC", Symbol: "SYNC", Instrument: "stock",
				Action: "open", Side: "buy", Sign: 1, Quantity: 15, FillPrice: new(210.0), Currency: "USD", ExecutedAt: entryDay.Add(-10 * 24 * time.Hour), ClusterKey: "owner:2026-W33", UnitKey: "owner:2026-W33:3",
				Entry:    rpc.ScorecardEntry{Reason: "history_unavailable"},
				Scores:   []rpc.ScorecardHorizonScore{{Sessions: 5, Reason: "history_unavailable", SlippageReason: "no_fill_quote"}, {Sessions: 10, Reason: "history_unavailable", SlippageReason: "no_fill_quote"}, {Sessions: 21, Reason: "history_unavailable", SlippageReason: "no_fill_quote"}},
				Slippage: &rpc.ScorecardSlippage{Status: "unknown", Reason: "no_fill_quote"}},
		},
		DecisionsTotal: 28,
		Coverage:       rpc.ScorecardCoverage{Ledger: 28, Stocks: 15, Options: 13, Entries: 14, Exits: 7, IndexEntries: 4, RuleOriginated: map[string]int{"proposal:trailing_stop": 2, "lifecycle": 1}, Skipped: map[string]int{"unsupported_asset": 2}, ScoredByHorizon: map[int]int{5: 23, 10: 21, 21: 14}, UnknownByReason: map[string]int{"immature": 16, "history_unavailable": 6}, SlippageQuoted: 4, SlippageByReason: map[string]int{"no_fill_quote": 23, "execution_not_observed": 1}},
		History: rpc.ScorecardHistory{Names: 11, Missing: 1, MarketSymbol: "SPY", MarketBars: 530, SectorBars: map[string]int{"XLK": 530, "XLV": 530}, LastFetchedAt: at.Add(-time.Hour), Failures: []rpc.ScorecardHistoryFailure{
			{Symbol: "SPY", Bars: 530, Failure: "historical data timeout for SPY after 45s: context deadline exceeded", FailedAt: at.Add(-time.Hour), RetryAt: at.Add(time.Hour)},
			{Symbol: "SYNC", ConID: 3, Failure: "contract details unresolved for SYNC: no security definition has been found", FailedAt: at.Add(-3 * time.Hour), RetryAt: at.Add(5 * time.Hour)},
		}},
		NotExecution: true,
	}
}

// goldenScorecardBackfilling is the state-only result before the first
// evaluation has history to score.
func goldenScorecardBackfilling() rpc.ScorecardResult {
	return rpc.ScorecardResult{
		SchemaVersion: "canary-scorecard-v2", State: rpc.ScorecardStateBackfilling, Reason: "history_backfill_pending", AsOf: goldenAt,
		PrimaryHorizon: 10, Horizons: []int{5, 10, 21}, Sleeves: []rpc.ScorecardSleeve{}, Decisions: []rpc.ScorecardDecision{},
		Coverage: rpc.ScorecardCoverage{RuleOriginated: map[string]int{}, Skipped: map[string]int{}, ScoredByHorizon: map[int]int{}, UnknownByReason: map[string]int{}},
		History:  rpc.ScorecardHistory{MarketSymbol: "SPY"}, NotExecution: true,
	}
}

// goldenUnit is one name-week unit of a fixture stream: its mean z and
// whether the market rose over its horizon.
type goldenUnit struct {
	meanZ float64
	up    bool
}

// goldenStream fills a fixture stream's statistics from its unit means in
// chronological order and its effective sample, through the same functions
// the pure core uses (scorecard.PosteriorScore, PosteriorAbove, EValue,
// FutilityEValue, UnitsToEstablish), so the rendered numbers are the
// arithmetic of the stated units: the running sum and directional score,
// the halves and market splits, the gates and the projection. The placebo's
// standardised sd is the expectation of a sign shuffle, √(Σm²/V) with V =
// n²/n_eff; no shuffle at this sample can reach the bar (n_eff < 60).
func goldenStream(s rpc.ScorecardSleeve, effective float64, units []goldenUnit) rpc.ScorecardSleeve {
	n := len(units)
	s.Units, s.Clusters, s.EffectiveUnits = n, n, effective
	if n == 0 {
		s.EValueAgainst = true
		return s
	}
	var sum, sumSq float64
	means := make([]float64, 0, n)
	var up, down []float64
	for _, u := range units {
		sum += u.meanZ
		sumSq += u.meanZ * u.meanZ
		means = append(means, u.meanZ)
		if u.up {
			up = append(up, u.meanZ)
		} else {
			down = append(down, u.meanZ)
		}
	}
	covariance := float64(n) * float64(n) / effective
	sEff := sum * float64(n) / covariance
	score := sum / float64(n)
	s.SumZ = sum
	s.DirectionalScore, s.MeanZ = &score, &score
	pm, psd := scorecard.PosteriorScore(effective, sEff)
	p0, p005, p010 := scorecard.PosteriorAbove(effective, sEff, 0), scorecard.PosteriorAbove(effective, sEff, 0.05), scorecard.PosteriorAbove(effective, sEff, 0.10)
	s.PosteriorMean, s.PosteriorSD = &pm, &psd
	s.PScoreAbove0, s.PScoreAbove005, s.PScoreAbove010, s.PAbove005, s.PAbove010 = &p0, &p005, &p010, &p005, &p010
	s.EValue, s.EValueAgainst = scorecard.EValue(effective, sEff)
	s.FutilityEValue = scorecard.FutilityEValue(effective, sEff)
	if projected, ok := scorecard.UnitsToEstablish(score); ok {
		s.UnitsToEstablish, s.ClustersToEstablish = &projected, &projected
		if pace := effective / float64(s.Weeks); pace > 0 {
			weeks := int(math.Ceil(float64(projected) / pace))
			s.WeeksToEstablish = &weeks
		}
	}
	gates := rpc.ScorecardGates{EValue: s.EValue >= scorecard.EstablishedEValue, Units: effective >= scorecard.EstablishedUnits, Weeks: s.Weeks >= scorecard.EstablishedWeeks, Posterior: p0 >= scorecard.EstablishedPosterior, UpUnits: len(up), DownUnits: len(down)}
	gates.Clusters, gates.UpClusters, gates.DownClusters = gates.Units, gates.UpUnits, gates.DownUnits
	if n >= 2 {
		first, second := goldenMean(means[:n/2]), goldenMean(means[n/2:])
		gates.FirstHalfMeanZ, gates.SecondHalfMeanZ = &first, &second
		gates.HalvesAgree = first != 0 && second != 0 && math.Signbit(first) == math.Signbit(second)
	}
	if len(up) > 0 && len(down) > 0 {
		upMean, downMean := goldenMean(up), goldenMean(down)
		gates.UpMarketMeanZ, gates.DownMarketMeanZ = &upMean, &downMean
		gates.MarketsAgree = upMean != 0 && downMean != 0 && math.Signbit(upMean) == math.Signbit(downMean)
	}
	sd := math.Sqrt(sumSq / covariance)
	s.Placebo = &rpc.ScorecardPlacebo{Replicates: scorecard.PlaceboReplicates, Established: 0, Rate: 0, StandardisedSD: sd}
	gates.Placebo = sd <= scorecard.PlaceboMaxStandardisedSD
	s.Gates = gates
	if s.Gated {
		s.Gate = rpc.ScorecardGateAccruing
	}
	return s
}

func goldenMean(values []float64) float64 {
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total / float64(len(values))
}
