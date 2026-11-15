package scorecard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestImpliedVolRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		spot, strike, t, vol float64
		call                 bool
	}{
		{100, 100, 0.25, 0.30, true}, {100, 100, 0.25, 0.30, false},
		{100, 120, 0.08, 0.45, true}, {100, 80, 0.08, 0.45, false},
		{100, 90, 1.0, 0.20, true}, {100, 110, 1.0, 0.60, false},
		{50, 55, 0.02, 0.80, true}, {250, 200, 0.5, 0.15, false},
	} {
		price := Price(tc.spot, tc.strike, tc.t, tc.vol, RiskFreeRate, DividendYield, tc.call)
		got := ImpliedVol(price, tc.spot, tc.strike, tc.t, RiskFreeRate, DividendYield, tc.call)
		if math.Abs(got-tc.vol) > 1e-5 {
			t.Fatalf("iv(%+v) = %.6f, want %.6f", tc, got, tc.vol)
		}
	}
	// A fill at or below intrinsic has no solution and stays unknown.
	if got := ImpliedVol(20, 120, 100, 0.25, RiskFreeRate, DividendYield, true); got != 0 {
		t.Fatalf("below-intrinsic iv = %v, want 0", got)
	}
	if got := ImpliedVol(1, 100, 100, 0, RiskFreeRate, DividendYield, true); got != 0 {
		t.Fatalf("expired iv = %v, want 0", got)
	}
}

func TestDirectionSignForPutsAndShortCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		right, side string
		want        int
	}{{RightCall, SideBuy, 1}, {RightCall, SideSell, -1}, {RightPut, SideBuy, -1}, {RightPut, SideSell, 1}, {"", SideBuy, 0}} {
		if got := DirectionSign(tc.right, tc.side); got != tc.want {
			t.Fatalf("sign(%s %s) = %d, want %d", tc.side, tc.right, got, tc.want)
		}
	}
	ev := edge.ChangeEvidence{Instruments: map[int64]flexstmt.Instrument{
		456: {ConID: 456, UnderlyingConID: 123, UnderlyingSymbol: "ACME", AssetClass: "OPT", Multiplier: new(100.0), Strike: new(100.0), Expiry: "20260320", PutCall: "P"},
		457: {ConID: 457, UnderlyingConID: 123, UnderlyingSymbol: "ACME", AssetClass: "OPT", Multiplier: new(100.0), Strike: new(100.0), Expiry: "20260320", PutCall: "C"},
	}}
	ev.Changes = []edge.PositionChange{
		{Key: "a", ConID: 456, UnderlyingConID: 123, Symbol: "ACME PUT", AssetClass: "OPT", Action: "open", ExecutedAt: day("2026-01-12"), DeltaQuantity: 2, PositionAfter: 2, ExecutionVWAP: new(3.0), Multiplier: new(100.0)},
		{Key: "b", ConID: 457, UnderlyingConID: 123, Symbol: "ACME CALL", AssetClass: "OPT", Action: "open", ExecutedAt: day("2026-01-12"), DeltaQuantity: -1, PositionAfter: -1, ExecutionVWAP: new(3.0), Multiplier: new(100.0)},
		{Key: "c", ConID: 123, Symbol: "ACME", AssetClass: "STK", Action: "open", ExecutedAt: day("2026-01-13"), DeltaQuantity: -10, PositionAfter: -10, ExecutionVWAP: new(100.0), Multiplier: new(1.0)},
		{Key: "d", ConID: 999, Symbol: "BILL", AssetClass: "BILL", Action: "open", ExecutedAt: day("2026-01-13"), DeltaQuantity: 10000},
	}
	ledger, skipped := BuildLedger(ev, nil)
	if len(ledger) != 3 || skipped[ReasonUnsupportedAsset] != 1 {
		t.Fatalf("ledger = %d rows, skipped = %v", len(ledger), skipped)
	}
	if ledger[0].Sign != -1 || ledger[0].Underlying != "ACME" || ledger[0].Option == nil || ledger[0].Option.Right != RightPut {
		t.Fatalf("long put = %+v", ledger[0])
	}
	if ledger[1].Sign != -1 || ledger[1].Side != SideSell || ledger[1].Option.Right != RightCall {
		t.Fatalf("short call = %+v", ledger[1])
	}
	if ledger[2].Sign != -1 || ledger[2].Instrument != InstrumentStock || ledger[2].UnderlyingConID != 123 {
		t.Fatalf("short stock = %+v", ledger[2])
	}
	if ledger[0].ClusterKey != "owner:2026-W03" || ledger[0].ID == ledger[1].ID || ledger[0].Sleeve != SleeveOwner {
		t.Fatalf("cluster or id = %+v", ledger[0])
	}
}

func TestUnitCountsANameOnceAWeek(t *testing.T) {
	t.Parallel()
	// Three decisions on one name in one week are one unit; a second name
	// that week is a second unit; the first name again a week later is a
	// third, half-overlapping the first at a 10-session horizon.
	decisions := []Decision{
		{ID: "1", Sleeve: "owner", UnderlyingConID: 1, ExecutedAt: weekDay(1)}, {ID: "2", Sleeve: "owner", UnderlyingConID: 1, ExecutedAt: weekDay(1).Add(24 * time.Hour)}, {ID: "3", Sleeve: "owner", UnderlyingConID: 1, ExecutedAt: weekDay(1).Add(48 * time.Hour)},
		{ID: "4", Sleeve: "owner", UnderlyingConID: 2, ExecutedAt: weekDay(1)},
		{ID: "5", Sleeve: "owner", UnderlyingConID: 1, ExecutedAt: weekDay(2)},
	}
	scores := []rpc.ScorecardHorizonScore{
		{Sessions: 10, Matured: true, Z: new(3.0), MarketReturn: new(0.01)}, {Sessions: 10, Matured: true, Z: new(1.0), MarketReturn: new(0.01)}, {Sessions: 10, Matured: true, Z: new(-1.0), MarketReturn: new(0.01)},
		{Sessions: 10, Matured: true, Z: new(2.0), MarketReturn: new(-0.02)},
		{Sessions: 10, Matured: true, Z: new(1.0), MarketReturn: new(0.01)},
	}
	got := sleeveEvidence("owner", rpc.ScorecardStreamEntries, 10, decisions, scores)
	if got.Units != 3 || got.Clusters != 3 || got.Weeks != 2 || got.Scored != 5 || math.Abs(got.SumZ-4) > 1e-12 || got.DirectionalScore == nil || math.Abs(*got.DirectionalScore-4.0/3) > 1e-12 {
		t.Fatalf("evidence = %+v", got)
	}
	// V = 3 + 2·(0.1 same-week cross-name + 0.5 same-name adjacent week +
	// 0.1·0.5 cross-name adjacent week) = 4.3.
	if math.Abs(got.EffectiveUnits-9/4.3) > 1e-9 {
		t.Fatalf("effective units = %v, want %v", got.EffectiveUnits, 9/4.3)
	}
	if got.Gates.UpUnits != 2 || got.Gates.DownUnits != 1 || got.Gate != rpc.ScorecardGateAccruing || got.Placebo == nil || got.Placebo.Replicates != PlaceboReplicates {
		t.Fatalf("gates = %+v gate=%s placebo=%+v", got.Gates, got.Gate, got.Placebo)
	}
	for _, d := range decisions[:3] {
		if _, key := unitKeys(d); key != "owner:2020-W03:1" {
			t.Fatalf("unit key = %s", key)
		}
	}
}

// TestOverlapShrinksTheEffectiveSample: the same name every week at a
// 21-session horizon shares most of its path with the weeks around it, so
// the effective sample is a fraction of the unit count; names a horizon
// apart are independent and count in full.
func TestOverlapShrinksTheEffectiveSample(t *testing.T) {
	t.Parallel()
	var dense, sparse []unit
	for week := range 52 {
		dense = append(dense, unit{conid: 1, day: weekDay(week), sumZ: 1, count: 1})
		sparse = append(sparse, unit{conid: int64(week), day: weekDay(week * 6), sumZ: 1, count: 1})
	}
	d, s := EffectiveSample(dense, MaxHorizon), EffectiveSample(sparse, MaxHorizon)
	if d.Units != 52 || s.Units != 52 || d.Sum != 52 || s.Sum != 52 {
		t.Fatalf("units = %+v / %+v", d, s)
	}
	if s.NEff != 52 || s.SEff != 52 {
		t.Fatalf("independent units lost information: %+v", s)
	}
	if d.NEff > 16 || d.NEff < 10 {
		t.Fatalf("weekly units of one name at 21 sessions: n_eff = %.1f, want about a quarter of 52", d.NEff)
	}
	if math.Abs(d.SEff/d.NEff-1) > 1e-12 {
		t.Fatalf("the score per unit moved: %v", d.SEff/d.NEff)
	}
}

// TestPlaceboRefusesEvidenceFromMagnitudes: units whose z are large and
// all positive read as evidence, but the same magnitudes with shuffled
// signs must not; a stream whose shuffles reach the bar too often is held
// at accruing however loud it is.
func TestPlaceboRefusesEvidenceFromMagnitudes(t *testing.T) {
	t.Parallel()
	var decisions []Decision
	var scores []rpc.ScorecardHorizonScore
	for week := range 60 {
		for name := range 3 {
			decisions = append(decisions, Decision{ID: fmt.Sprintf("%d-%d", week, name), Sleeve: "owner", UnderlyingConID: int64(name), ExecutedAt: weekDay(week)})
			scores = append(scores, rpc.ScorecardHorizonScore{Sessions: 10, Matured: true, Z: new(1.5), MarketReturn: new(float64(1-2*(week%2)) * 0.01)})
		}
	}
	got := sleeveEvidence("owner", rpc.ScorecardStreamEntries, 10, decisions, scores)
	if got.Placebo == nil || got.EValue < EstablishedEValue {
		t.Fatalf("evidence = %+v", got)
	}
	// Fair coins over 180 units of 1.5 against the null covariance V of the
	// real sample (about 2.3 per unit with the weekly overlap): the shuffled
	// sums spread about as the null says and the bar sits far out.
	if got.Placebo.Rate > PlaceboMaxRate || !got.Gates.Placebo || got.Placebo.StandardisedSD > PlaceboMaxStandardisedSD || got.Placebo.StandardisedSD < 0.6 {
		t.Fatalf("placebo = %+v", got.Placebo)
	}
	if got.Gate != rpc.ScorecardGateEstablished {
		t.Fatalf("gate = %s gates = %+v", got.Gate, got.Gates)
	}
}

// TestPlaceboRefusesAnUnderstatedNull: units whose |z| is 1.6 with no
// overlap (one name every three weeks, so V = n exactly) are evidence only
// if the unit noise were 1; the shuffled sums spread 1.6 times wider than
// the null covariance says, so the e-value and the posterior both read an
// understated noise. The share of shuffles reaching the bar alone stays
// under 5% (the bar near 3σ is 1.9σ on this spread); the calibration bound
// holds the stream at accruing however loud the sum. The same units at 1.0
// pass.
func TestPlaceboRefusesAnUnderstatedNull(t *testing.T) {
	t.Parallel()
	build := func(magnitude float64) rpc.ScorecardSleeve {
		var decisions []Decision
		var scores []rpc.ScorecardHorizonScore
		for i := range 80 {
			decisions = append(decisions, Decision{ID: fmt.Sprintf("%d", i), Sleeve: "owner", UnderlyingConID: int64(i), ExecutedAt: weekDay(3 * i)})
			scores = append(scores, rpc.ScorecardHorizonScore{Sessions: 10, Matured: true, Z: new(magnitude), MarketReturn: new(float64(1-2*(i%2)) * 0.01)})
		}
		return sleeveEvidence("owner", rpc.ScorecardStreamEntries, 10, decisions, scores)
	}
	loud := build(1.6)
	if loud.EffectiveUnits != 80 || loud.EValue < EstablishedEValue || loud.Placebo == nil {
		t.Fatalf("evidence = %+v", loud)
	}
	// 200 seeded shuffles read the spread to within about 5%.
	if loud.Placebo.StandardisedSD < 1.45 || loud.Placebo.StandardisedSD > 1.75 {
		t.Fatalf("standardised sd = %v, want about the magnitude 1.6 when V = n", loud.Placebo.StandardisedSD)
	}
	if loud.Placebo.Rate > PlaceboMaxRate {
		t.Fatalf("rate = %v: the rate alone should let this spread through", loud.Placebo.Rate)
	}
	if loud.Gates.Placebo || loud.Gate != rpc.ScorecardGateAccruing {
		t.Fatalf("an understated null passed: placebo gate %v, gate %s, placebo %+v", loud.Gates.Placebo, loud.Gate, loud.Placebo)
	}
	calibrated := build(1.0)
	if calibrated.Placebo.StandardisedSD < 0.9 || calibrated.Placebo.StandardisedSD > 1.1 || !calibrated.Gates.Placebo || calibrated.Gate != rpc.ScorecardGateEstablished {
		t.Fatalf("calibrated units were refused: gate %s, placebo %+v", calibrated.Gate, calibrated.Placebo)
	}
}

// TestIndexStreamUnitsAreOneName: two index-stream units in one week on
// different contracts (an SPX put and a SPY put) are one position on the
// market and overlap fully, so the pair carries one effective unit; the
// same two contracts in the entries stream would be discounted as two
// names.
func TestIndexStreamUnitsAreOneName(t *testing.T) {
	t.Parallel()
	decisions := []Decision{
		{ID: "spx", Sleeve: "owner", UnderlyingConID: 1, ExecutedAt: weekDay(1)},
		{ID: "spy", Sleeve: "owner", UnderlyingConID: 2, ExecutedAt: weekDay(1)},
	}
	scores := []rpc.ScorecardHorizonScore{{Sessions: 10, Matured: true, Z: new(1.0)}, {Sessions: 10, Matured: true, Z: new(1.0)}}
	index := sleeveEvidence("owner", rpc.ScorecardStreamIndexEntries, 10, decisions, scores)
	if index.Units != 2 || math.Abs(index.EffectiveUnits-1) > 1e-12 {
		t.Fatalf("index entries: units %d effective %v, want 2 units carrying 1", index.Units, index.EffectiveUnits)
	}
	names := sleeveEvidence("owner", rpc.ScorecardStreamEntries, 10, decisions, scores)
	if want := 4 / (2 + 2*CrossNameResidualCorrelation); math.Abs(names.EffectiveUnits-want) > 1e-12 {
		t.Fatalf("entries: effective %v, want %v", names.EffectiveUnits, want)
	}
}

// TestDecisionIDStaysOnTheStatementClock: the id hashes the statement's own
// wall clock, the value the ids of 2026-10-14 were hashed from when the
// parser still read it as UTC, so the append-only ledger keeps its ids now
// that the execution time is the true instant (13:45:30Z is 09:45:30 on
// the statement's Eastern clock in summer).
func TestDecisionIDStaysOnTheStatementClock(t *testing.T) {
	t.Parallel()
	d := Decision{ConID: 900001, Side: SideBuy, Action: "open", Quantity: 10, ExecutedAt: time.Date(2026, 6, 15, 13, 45, 30, 0, time.UTC), Lineage: Lineage{RecordIDs: []string{"trade-synth-a", "trade-synth-b"}}}
	h := sha256.New()
	h.Write([]byte("canary.scorecard.decision.v1"))
	for _, value := range []string{"900001", "2026-06-15T09:45:30Z", SideBuy, "open", "10", "trade-synth-a,trade-synth-b"} {
		h.Write([]byte{0})
		h.Write([]byte(value))
	}
	if want := "decision_" + hex.EncodeToString(h.Sum(nil)[:12]); decisionID(d) != want {
		t.Fatalf("id = %s, want %s", decisionID(d), want)
	}
}

func TestMissingHistoryIsUnknownNotZero(t *testing.T) {
	t.Parallel()
	d := Decision{ID: "x", Sleeve: "owner", Underlying: "ACME", UnderlyingConID: 123, ConID: 123, Symbol: "ACME", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: day("2026-01-12"), ClusterKey: "owner:2026-W03"}
	res := Evaluate(Input{AsOf: day("2026-03-01"), Decisions: []Decision{d}, History: History{Market: syntheticBars(0, "2025-01-01", 400, 1, 500)}})
	if len(res.Decisions) != 1 || len(res.Decisions[0].Scores) != 3 {
		t.Fatalf("result = %+v", res)
	}
	for _, score := range res.Decisions[0].Scores {
		if score.Z != nil || score.Reason != ReasonHistoryUnavailable || score.Matured {
			t.Fatalf("score = %+v, want unknown", score)
		}
	}
	if res.Coverage.UnknownByReason[ReasonHistoryUnavailable] != 3 || len(res.Coverage.ScoredByHorizon) != 0 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	var sleeve rpc.ScorecardSleeve
	for _, s := range res.Sleeves {
		if s.Horizon == PrimaryHorizon && s.Stream == rpc.ScorecardStreamEntries {
			sleeve = s
		}
	}
	if sleeve.Scored != 0 || sleeve.Clusters != 0 || sleeve.MeanZ != nil || !sleeve.EValueAgainst || sleeve.Gate != rpc.ScorecardGateAccruing || sleeve.Unknown[ReasonHistoryUnavailable] != 1 {
		t.Fatalf("sleeve = %+v", sleeve)
	}
}

func TestExposureUsesOnlyPreDecisionSessions(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 7, 400)
	clean := syntheticBars(123, "2025-01-01", 420, 11, 100)
	d := Decision{ID: "x", Sleeve: "owner", Underlying: "ACME", UnderlyingConID: 123, ConID: 123, Symbol: "ACME", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: clean[300].Day, ClusterKey: "owner:w"}
	base := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{123: clean}}})
	// A sentinel after entry: every later close is a wild multiple.
	wild := append([]edge.DailyBar(nil), clean...)
	for i := 301; i < len(wild); i++ {
		wild[i].Close *= 2 + 5*float64(i%3)
	}
	shifted := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{123: wild}}})
	for i := range base.Decisions[0].Scores {
		a, b := base.Decisions[0].Scores[i], shifted.Decisions[0].Scores[i]
		if a.Z == nil || b.Z == nil || a.Beta == nil || b.Beta == nil {
			t.Fatalf("scores unknown: %+v / %+v", a, b)
		}
		if *a.Beta != *b.Beta || *a.IdioVolDaily != *b.IdioVolDaily {
			t.Fatalf("post-entry bars changed the estimate: beta %v/%v sigma %v/%v", *a.Beta, *b.Beta, *a.IdioVolDaily, *b.IdioVolDaily)
		}
		if *a.Z == *b.Z {
			t.Fatalf("horizon %d z did not move with the realised path", a.Sessions)
		}
	}
	entry := base.Decisions[0].Entry
	if entry.SessionDay == nil || !entry.SessionDay.Equal(clean[300].Day) || entry.UnderlyingClose == nil || *entry.UnderlyingClose != clean[300].Close {
		t.Fatalf("entry = %+v", entry)
	}
}

func TestZeroSkillKeepsEValueSmallAndReachesFutility(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 11))
	sum, futileAt, maxE := 0.0, 0, 0.0
	for n := 1; n <= 6000; n++ {
		sum += rng.NormFloat64()
		e, _ := EValue(float64(n), sum)
		maxE = math.Max(maxE, e)
		if futileAt == 0 && float64(n) >= FutileUnits && FutilityEValue(float64(n), sum) >= FutileEValue {
			futileAt = n
		}
	}
	if maxE >= EstablishedEValue {
		t.Fatalf("zero skill reached e-value %.1f", maxE)
	}
	if futileAt == 0 {
		t.Fatal("zero skill never reached futility")
	}
}

// TestScorePointTenIsEstablishedWithinYears is the gate's reason to exist:
// a selector with a directional score of 0.10 on six names a week (about
// the owner's pace) reads established within four years on the expected
// path, and the closed-form projection agrees with the brute-force count.
func TestScorePointTenIsEstablishedWithinYears(t *testing.T) {
	t.Parallel()
	closedForm, ok := UnitsToEstablish(0.10)
	if !ok {
		t.Fatal("no projection for a score of 0.10")
	}
	brute := 0
	for n := 1; n <= 5000; n++ {
		if e, _ := EValue(float64(n), 0.10*float64(n)); e >= EstablishedEValue {
			brute = n
			break
		}
	}
	if closedForm != brute || closedForm < 600 || closedForm > 1200 {
		t.Fatalf("closed form %d, brute %d", closedForm, brute)
	}
	// Twelve names a week from a rotating pool of sixty, unit means drawn
	// independently ~ N(0.10, 1) while the covariance model charges for
	// overlap (so the read is conservative): the whole gate (e-value,
	// sample, weeks, posterior, placebo, halves and markets) passes within
	// ten years and never before 26 weeks. The power table in the design
	// record puts the median at about four years for this pace.
	rng := rand.New(rand.NewPCG(3, 5))
	var decisions []Decision
	var scores []rpc.ScorecardHorizonScore
	established := 0
	for week := 1; week <= 10*52; week++ {
		for name := range 12 {
			id := fmt.Sprintf("d%d-%d", week, name)
			decisions = append(decisions, Decision{ID: id, Sleeve: "sim", UnderlyingConID: int64((week*12+name)%60 + 1), ExecutedAt: weekDay(week)})
			scores = append(scores, rpc.ScorecardHorizonScore{Sessions: PrimaryHorizon, Matured: true, Z: new(0.10 + rng.NormFloat64()), MarketReturn: new(float64(1-2*(week%2)) * 0.01)})
		}
		if week%8 != 0 && week != EstablishedWeeks-1 {
			continue
		}
		got := sleeveEvidence("sim", rpc.ScorecardStreamEntries, PrimaryHorizon, decisions, scores)
		if got.Gate == rpc.ScorecardGateEstablished {
			if week < EstablishedWeeks {
				t.Fatalf("established at %d weeks", week)
			}
			established = week
			break
		}
		if got.Gate == rpc.ScorecardGateFutile {
			t.Fatalf("a score of 0.10 read futile at week %d: %+v", week, got)
		}
	}
	if established == 0 {
		t.Fatal("a score of 0.10 on twelve rotating names a week was not established within ten years")
	}
	t.Logf("established after %d weeks (%.1f years)", established, float64(established)/52)
}

// weekDay is the Monday of the n-th simulated week.
func weekDay(week int) time.Time {
	return time.Date(2020, 1, 6, 15, 0, 0, 0, time.UTC).AddDate(0, 0, 7*week)
}

func TestHolmStepDown(t *testing.T) {
	t.Parallel()
	got := Holm([]float64{0.01, 0.04, 0.03})
	want := []float64{0.03, 0.06, 0.06}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("holm = %v, want %v", got, want)
		}
	}
}

func TestOptionWrapperAndRealisedVsImplied(t *testing.T) {
	t.Parallel()
	underlying := syntheticBars(123, "2025-01-01", 420, 13, 100)
	market := syntheticBars(0, "2025-01-01", 420, 17, 400)
	entryIdx := 320
	entryDay := underlying[entryIdx].Day
	spot := underlying[entryIdx].Close
	expiry := entryDay.AddDate(0, 0, 60)
	tYears := expiry.Sub(entryDay).Hours() / 24 / 365
	fill := Price(spot, 100, tYears, 0.35, RiskFreeRate, DividendYield, true)
	d := Decision{ID: "opt", Sleeve: "owner", Underlying: "ACME", UnderlyingConID: 123, ConID: 456, Symbol: "ACME CALL", Instrument: InstrumentOption,
		Option: &OptionContract{Expiry: expiry.Format(time.DateOnly), Strike: 100, Right: RightCall, Multiplier: 100}, Action: "open", Side: SideBuy, Sign: 1, Quantity: 2,
		FillPrice: &fill, Multiplier: new(100.0), Currency: "USD", ExecutionFX: new(0.9), ExecutedAt: entryDay.Add(15 * time.Hour), ClusterKey: "owner:w"}
	horizonDay := underlying[entryIdx+PrimaryHorizon].Day
	mark := 9.0
	res := Evaluate(Input{AsOf: day("2026-06-01"), BaseCurrency: "EUR", Decisions: []Decision{d},
		History: History{Market: market, Underlying: map[int64][]edge.DailyBar{123: underlying}},
		Marks:   map[int64][]edge.ContractMark{456: {{Day: horizonDay, Price: mark}}}})
	entry := res.Decisions[0].Entry
	if entry.ImpliedVol == nil || math.Abs(*entry.ImpliedVol-0.35) > 1e-5 || entry.Delta == nil || *entry.Delta <= 0 || *entry.Delta >= 1 {
		t.Fatalf("entry = %+v", entry)
	}
	var score rpc.ScorecardHorizonScore
	for _, s := range res.Decisions[0].Scores {
		if s.Sessions == PrimaryHorizon {
			score = s
		}
	}
	if score.Z == nil || score.RealisedVsImplied == nil || score.WrapperBase == nil {
		t.Fatalf("score = %+v", score)
	}
	ru := math.Log(underlying[entryIdx+PrimaryHorizon].Close / spot)
	wantRVI := math.Abs(ru) / (0.35 * math.Sqrt(float64(PrimaryHorizon)/252))
	if math.Abs(*score.RealisedVsImplied-wantRVI) > 1e-9 {
		t.Fatalf("rvi = %v, want %v", *score.RealisedVsImplied, wantRVI)
	}
	optionPnL := 2 * 100 * (mark - fill) * 0.9
	stockPnL := *entry.Delta * 100 * 2 * (underlying[entryIdx+PrimaryHorizon].Close - spot) * 0.9
	if math.Abs(*score.WrapperBase-(optionPnL-stockPnL)) > 1e-6 {
		t.Fatalf("wrapper = %v, want %v", *score.WrapperBase, optionPnL-stockPnL)
	}
	// Without a mark on the horizon day the wrapper is unknown, the
	// direction score stands.
	bare := Evaluate(Input{AsOf: day("2026-06-01"), BaseCurrency: "EUR", Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{123: underlying}}})
	for _, s := range bare.Decisions[0].Scores {
		if s.Sessions == PrimaryHorizon && (s.Z == nil || s.WrapperBase != nil || s.WrapperReason != ReasonMarkUnavailable) {
			t.Fatalf("bare score = %+v", s)
		}
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	t.Parallel()
	underlying := syntheticBars(123, "2025-01-01", 420, 19, 100)
	market := syntheticBars(0, "2025-01-01", 420, 23, 400)
	var decisions []Decision
	for i := range 30 {
		idx := 280 + i*3
		decisions = append(decisions, Decision{ID: string(rune('a' + i)), Sleeve: "owner", Underlying: "ACME", UnderlyingConID: 123, ConID: 123, Symbol: "ACME", Instrument: InstrumentStock, Action: "add", Side: SideBuy, Sign: 1, Quantity: 1, ExecutedAt: underlying[idx].Day, ClusterKey: clusterKeyFor("owner", i/2)})
	}
	in := Input{AsOf: day("2026-06-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{123: underlying}}}
	a, _ := json.Marshal(Evaluate(in))
	b, _ := json.Marshal(Evaluate(in))
	if string(a) != string(b) {
		t.Fatal("evaluation differed between runs")
	}
	res := Evaluate(in)
	if res.DecisionsTotal != 30 || !res.Decisions[0].ExecutedAt.After(res.Decisions[29].ExecutedAt) {
		t.Fatalf("decisions = %d, order newest first = %v", res.DecisionsTotal, res.Decisions[0].ExecutedAt)
	}
}

// syntheticBars is a seeded random walk of positive closes on weekdays.
// Each unknown reason names the series that is missing: the market proxy
// when the name has history and SPY has none, the underlying contract when
// the statement named none (so no history was ever asked for it), and the
// name's own history only when that is what is missing.
func TestUnknownReasonsNameTheMissingSeries(t *testing.T) {
	t.Parallel()
	named := Decision{ID: "n", Sleeve: "owner", Underlying: "ACME", UnderlyingConID: 123, ConID: 123, Symbol: "ACME", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: day("2026-01-12"), ClusterKey: "owner:2026-W03"}
	noUnderlying := Decision{ID: "u", Sleeve: "owner", Underlying: "ACME", ConID: 456, Symbol: "ACME 260320C00100000", Instrument: InstrumentOption, Action: "open", Side: SideBuy, Sign: 1, Quantity: 1, ExecutedAt: day("2026-01-12"), ClusterKey: "owner:2026-W03"}
	res := Evaluate(Input{AsOf: day("2026-03-01"), Decisions: []Decision{named, noUnderlying}, History: History{Underlying: map[int64][]edge.DailyBar{123: syntheticBars(123, "2025-01-01", 400, 2, 50)}}})
	want := map[string]string{"n": ReasonMarketHistoryUnavailable, "u": ReasonUnderlyingUnknown}
	for _, d := range res.Decisions {
		for _, score := range d.Scores {
			if score.Z != nil || score.Reason != want[d.ID] {
				t.Fatalf("decision %s score = %+v, want reason %s", d.ID, score, want[d.ID])
			}
		}
	}
	if res.Coverage.UnknownByReason[ReasonMarketHistoryUnavailable] != 3 || res.Coverage.UnknownByReason[ReasonUnderlyingUnknown] != 3 || res.Coverage.UnknownByReason[ReasonHistoryUnavailable] != 0 {
		t.Fatalf("coverage = %+v", res.Coverage.UnknownByReason)
	}
}

func syntheticBars(conid int64, start string, count int, seed uint64, first float64) []edge.DailyBar {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	out := make([]edge.DailyBar, 0, count)
	current := day(start)
	close := first
	for len(out) < count {
		if current.Weekday() != time.Saturday && current.Weekday() != time.Sunday {
			close *= math.Exp(0.015*rng.NormFloat64() + 0.0002)
			out = append(out, edge.DailyBar{ConID: conid, Day: current, Close: close})
		}
		current = current.AddDate(0, 0, 1)
	}
	return out
}

func clusterKeyFor(sleeve string, week int) string {
	return sleeve + ":" + time.Date(2020, 1, 6, 0, 0, 0, 0, time.UTC).AddDate(0, 0, 7*week).Format("2006-01-02")
}

func day(v string) time.Time {
	parsed, err := time.Parse(time.DateOnly, v)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestExitsNeverMoveTheEntriesGate(t *testing.T) {
	t.Parallel()
	underlying := syntheticBars(123, "2023-01-01", 900, 29, 100)
	market := syntheticBars(0, "2023-01-01", 900, 31, 400)
	var decisions []Decision
	// Two entries and three hundred exits, the exits all in distinct weeks.
	for i := range 302 {
		action, stream, side, sign := "exit", rpc.ScorecardStreamExits, SideSell, -1
		if i < 2 {
			action, stream, side, sign = "open", rpc.ScorecardStreamEntries, SideBuy, 1
		}
		idx := 300 + (i % 500)
		decisions = append(decisions, Decision{ID: "d" + string(rune('0'+i%10)) + string(rune('a'+i/10%26)) + string(rune('a'+i/260)), Sleeve: "owner", Stream: stream, Underlying: "ACME", UnderlyingConID: 123, ConID: 123, Symbol: "ACME", Instrument: InstrumentStock, Action: action, Side: side, Sign: sign, Quantity: 1, ExecutedAt: underlying[idx].Day, ClusterKey: clusterKeyFor("owner", i)})
	}
	res := Evaluate(Input{AsOf: day("2026-12-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{123: underlying}}})
	var entries, exits rpc.ScorecardSleeve
	for _, s := range res.Sleeves {
		if s.Horizon != PrimaryHorizon {
			continue
		}
		switch s.Stream {
		case rpc.ScorecardStreamEntries:
			entries = s
		case rpc.ScorecardStreamExits:
			exits = s
		}
	}
	if !entries.Gated || entries.Decisions != 2 || entries.Clusters > 2 || entries.Gate != rpc.ScorecardGateAccruing {
		t.Fatalf("entries = %+v", entries)
	}
	// Three hundred daily exits on one name span about sixty ISO weeks:
	// sixty name-week units, each counted once.
	if exits.Gated || exits.Gate != "" || exits.Decisions != 300 || exits.Units < 55 || exits.Units > 65 || exits.Clusters != exits.Units || exits.MeanZ == nil {
		t.Fatalf("exits = %+v", exits)
	}
	if res.Coverage.Entries != 2 || res.Coverage.Exits != 300 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	// Even an exits stream with overwhelming evidence stays ungated.
	var exitDecisions []Decision
	var exitScores []rpc.ScorecardHorizonScore
	for n := range 400 {
		exitDecisions = append(exitDecisions, Decision{Sleeve: "owner", Stream: rpc.ScorecardStreamExits, UnderlyingConID: int64(n % 7), ExecutedAt: weekDay(n)})
		exitScores = append(exitScores, rpc.ScorecardHorizonScore{Sessions: PrimaryHorizon, Matured: true, Z: new(2.0), MarketReturn: new(float64(1-2*(n%2)) * 0.01)})
	}
	loud := sleeveEvidence("owner", rpc.ScorecardStreamExits, PrimaryHorizon, exitDecisions, exitScores)
	if loud.Gated || loud.Gate != "" || loud.EValue < EstablishedEValue || loud.HolmAdjustedP != nil {
		t.Fatalf("loud exits = %+v", loud)
	}
}

func TestRuleOriginatedFillsNeverCountForOwner(t *testing.T) {
	t.Parallel()
	ev := edge.ChangeEvidence{Instruments: map[int64]flexstmt.Instrument{}, LifecycleTradeIDs: map[string]bool{"t-assign": true}}
	ev.Changes = []edge.PositionChange{
		{Key: "a", ConID: 123, Symbol: "ACME", AssetClass: "STK", Action: "open", ExecutedAt: day("2026-01-12"), DeltaQuantity: 10, PositionAfter: 10, ExecutionVWAP: new(100.0), Multiplier: new(1.0), ExecutionIDs: []string{"e-hand"}},
		{Key: "b", ConID: 123, Symbol: "ACME", AssetClass: "STK", Action: "exit", ExecutedAt: day("2026-01-20"), DeltaQuantity: -10, PositionBefore: 10, ExecutionVWAP: new(95.0), Multiplier: new(1.0), ExecutionIDs: []string{"e-stop"}},
		{Key: "c", ConID: 123, Symbol: "ACME", AssetClass: "STK", Action: "open", ExecutedAt: day("2026-02-02"), DeltaQuantity: 100, PositionAfter: 100, ExecutionVWAP: new(90.0), Multiplier: new(1.0), TradeIDs: []string{"t-assign"}},
	}
	resolve := func(change edge.PositionChange) Sleeve {
		if slices.Contains(change.ExecutionIDs, "e-stop") {
			return Sleeve{Name: rpc.ScorecardSleeveRule, Origin: "proposal:trailing_stop"}
		}
		return Sleeve{Name: SleeveOwner}
	}
	ledger, _ := BuildLedger(ev, resolve)
	if len(ledger) != 3 || ledger[0].Sleeve != SleeveOwner || ledger[0].Stream != rpc.ScorecardStreamEntries {
		t.Fatalf("ledger = %+v", ledger)
	}
	if ledger[1].Sleeve != rpc.ScorecardSleeveRule || ledger[1].Origin != "proposal:trailing_stop" || ledger[1].Stream != rpc.ScorecardStreamExits {
		t.Fatalf("stop fill = %+v", ledger[1])
	}
	if ledger[2].Sleeve != rpc.ScorecardSleeveRule || ledger[2].Origin != OriginLifecycle {
		t.Fatalf("assignment fill = %+v", ledger[2])
	}
	// The id names the fill, not the sleeve: a reclassified fill keeps it.
	owned, _ := BuildLedger(ev, OwnerSleeve)
	if owned[1].ID != ledger[1].ID || owned[1].Sleeve != SleeveOwner {
		t.Fatalf("reclassified id changed: %s vs %s", owned[1].ID, ledger[1].ID)
	}
	res := Evaluate(Input{AsOf: day("2026-03-01"), Decisions: ledger, History: History{Market: syntheticBars(0, "2025-01-01", 400, 1, 500)}})
	for _, s := range res.Sleeves {
		if s.Sleeve == rpc.ScorecardSleeveRule {
			t.Fatalf("rule sleeve was scored: %+v", s)
		}
		want := 0
		if s.Stream == rpc.ScorecardStreamEntries {
			want = 1
		}
		if s.Sleeve == SleeveOwner && s.Decisions != want {
			t.Fatalf("owner %s stream counts %d decisions, want 1 entry and 0 exits", s.Stream, s.Decisions)
		}
	}
	if res.Coverage.RuleOriginated["proposal:trailing_stop"] != 1 || res.Coverage.RuleOriginated[OriginLifecycle] != 1 || res.Coverage.Entries != 1 || res.Coverage.Exits != 0 || res.Coverage.Ledger != 3 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	if len(res.Decisions) != 3 || res.Decisions[0].Sleeve != rpc.ScorecardSleeveRule || res.Decisions[0].Origin != OriginLifecycle {
		t.Fatalf("decisions = %+v", res.Decisions)
	}
}

// derivedBars follows the market's daily log returns with a loading and adds
// seeded noise: loading 1 with little noise is an ETF that is effectively the
// market, a small loading with large noise a single name.
func derivedBars(conid int64, market []edge.DailyBar, loading, noise float64, seed uint64) []edge.DailyBar {
	rng := rand.New(rand.NewPCG(seed, seed+1))
	out := make([]edge.DailyBar, len(market))
	close := 50.0
	for i, bar := range market {
		if i > 0 {
			close *= math.Exp(loading*math.Log(bar.Close/market[i-1].Close) + noise*rng.NormFloat64())
		}
		out[i] = edge.DailyBar{ConID: conid, Day: bar.Day, Close: close}
	}
	return out
}

// preEntryLogReturns are the daily log returns the estimate may use: the
// LookbackSessions window ending the session before entry.
func preEntryLogReturns(bars []edge.DailyBar, entryIdx int) []float64 {
	var out []float64
	for i := max(0, entryIdx-LookbackSessions) + 1; i < entryIdx; i++ {
		out = append(out, math.Log(bars[i].Close/bars[i-1].Close))
	}
	return out
}

func sampleSD(values []float64) float64 {
	m := mean(values)
	sum := 0.0
	for _, v := range values {
		sum += (v - m) * (v - m)
	}
	return math.Sqrt(sum / float64(len(values)-1))
}

func scoreAt(d rpc.ScorecardDecision, h int) rpc.ScorecardHorizonScore {
	for _, s := range d.Scores {
		if s.Sessions == h {
			return s
		}
	}
	return rpc.ScorecardHorizonScore{}
}

func sleeveAt(res rpc.ScorecardResult, sleeve, stream string, h int) (rpc.ScorecardSleeve, bool) {
	for _, s := range res.Sleeves {
		if s.Sleeve == sleeve && s.Stream == stream && s.Horizon == h {
			return s, true
		}
	}
	return rpc.ScorecardSleeve{}, false
}

func decisionByID(res rpc.ScorecardResult, id string) rpc.ScorecardDecision {
	for _, d := range res.Decisions {
		if d.ID == id {
			return d
		}
	}
	return rpc.ScorecardDecision{}
}

func TestIndexOptionIsScoredOnTotalVol(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 17, 400)
	// The market proxy as an option underlying: regressed on itself it leaves
	// no residual noise, so the single-name z is undefined.
	spy := make([]edge.DailyBar, len(market))
	for i, bar := range market {
		spy[i] = edge.DailyBar{ConID: 9001, Day: bar.Day, Close: bar.Close}
	}
	if f := ols(preEntryLogReturns(market, 300), preEntryLogReturns(spy, 300)); !f.ok || f.residualSD != 0 || f.rSquared != 1 {
		t.Fatalf("self-regression = %+v, want zero residual and R² 1", f)
	}
	entryIdx := 300
	entryDay, spot := spy[entryIdx].Day, spy[entryIdx].Close
	expiry := entryDay.AddDate(0, 0, 45)
	strike := math.Round(spot * 0.95)
	fill := Price(spot, strike, expiry.Sub(entryDay).Hours()/24/365, 0.18, RiskFreeRate, DividendYield, false)
	d := Decision{ID: "spy-put", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "SPY", UnderlyingConID: 9001, ConID: 9002, Symbol: "SPY PUT", Instrument: InstrumentOption,
		Option: &OptionContract{Expiry: expiry.Format(time.DateOnly), Strike: strike, Right: RightPut, Multiplier: 100}, Action: "open", Side: SideBuy, Sign: -1, Quantity: 2,
		FillPrice: &fill, Multiplier: new(100.0), Currency: "USD", ExecutedAt: entryDay.Add(15 * time.Hour), ClusterKey: "owner:2026-W01"}
	res := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{9001: spy}}})

	got := res.Decisions[0]
	if got.Stream != rpc.ScorecardStreamIndexEntries || got.Entry.IndexBasis != rpc.ScorecardIndexBasisList || got.Entry.MarketRSquared == nil || *got.Entry.MarketRSquared != 1 {
		t.Fatalf("decision = stream %q basis %q r2 %v", got.Stream, got.Entry.IndexBasis, got.Entry.MarketRSquared)
	}
	sigma := sampleSD(preEntryLogReturns(spy, entryIdx))
	for _, h := range Horizons {
		score := scoreAt(got, h)
		if score.Z == nil || score.TotalVolDaily == nil || score.IdioVolDaily != nil {
			t.Fatalf("horizon %d score = %+v", h, score)
		}
		want := -math.Log(spy[entryIdx+h].Close/spot) / (sigma * math.Sqrt(float64(h)))
		if math.Abs(*score.TotalVolDaily-sigma) > 1e-15 || math.Abs(*score.Z-want) > 1e-12 {
			t.Fatalf("horizon %d z = %v sigma %v, want %v and %v", h, *score.Z, *score.TotalVolDaily, want, sigma)
		}
	}
	if res.Coverage.UnknownByReason[ReasonIdioVolZero] != 0 || res.Coverage.IndexEntries != 1 || res.Coverage.Entries != 0 || res.Coverage.ScoredByHorizon[PrimaryHorizon] != 1 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
	index, ok := sleeveAt(res, SleeveOwner, rpc.ScorecardStreamIndexEntries, PrimaryHorizon)
	if !ok || index.Gated || index.Gate != "" || index.Scored != 1 || index.MeanZ == nil {
		t.Fatalf("index sleeve = %+v", index)
	}
	rvi := scoreAt(got, PrimaryHorizon).RealisedVsImplied
	if rvi == nil || index.LongPutRealisedVsImplied == nil || index.LongPutRealisedVsImplied.Count != 1 || index.LongPutRealisedVsImplied.Median != *rvi {
		t.Fatalf("long-put realised/implied = %+v, decision %v", index.LongPutRealisedVsImplied, rvi)
	}
	if entries, _ := sleeveAt(res, SleeveOwner, rpc.ScorecardStreamEntries, PrimaryHorizon); entries.Decisions != 0 || !entries.Gated {
		t.Fatalf("entries sleeve took the index decision: %+v", entries)
	}
}

func TestHighRSquaredETFIsRoutedToIndex(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 23, 400)
	broad := derivedBars(201, market, 1.0, 0.002, 41) // effectively the market, off the list
	single := derivedBars(202, market, 0.5, 0.02, 43) // a name with its own noise
	entryIdx := 300
	at := market[entryIdx].Day.Add(15 * time.Hour)
	decisions := []Decision{
		{ID: "broad", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "BRDX", UnderlyingConID: 201, ConID: 201, Symbol: "BRDX", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: at, ClusterKey: "owner:2026-W01"},
		{ID: "broad-exit", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamExits, Underlying: "BRDX", UnderlyingConID: 201, ConID: 201, Symbol: "BRDX", Instrument: InstrumentStock, Action: "exit", Side: SideSell, Sign: -1, Quantity: 10, ExecutedAt: market[entryIdx+3].Day, ClusterKey: "owner:2026-W02"},
		{ID: "single", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "SNGL", UnderlyingConID: 202, ConID: 202, Symbol: "SNGL", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: at, ClusterKey: "owner:2026-W01"},
		{ID: "qqq", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "qqq", UnderlyingConID: 202, ConID: 203, Symbol: "QQQ", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: at, ClusterKey: "owner:2026-W01"},
	}
	res := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{201: broad, 202: single}}})

	b := decisionByID(res, "broad")
	if b.Stream != rpc.ScorecardStreamIndexEntries || b.Entry.IndexBasis != rpc.ScorecardIndexBasisRSquared || b.Entry.MarketRSquared == nil || *b.Entry.MarketRSquared < IndexRSquared {
		t.Fatalf("broad ETF = stream %q basis %q r2 %v", b.Stream, b.Entry.IndexBasis, b.Entry.MarketRSquared)
	}
	score := scoreAt(b, PrimaryHorizon)
	want := math.Log(broad[entryIdx+PrimaryHorizon].Close/broad[entryIdx].Close) / (sampleSD(preEntryLogReturns(broad, entryIdx)) * math.Sqrt(PrimaryHorizon))
	if score.Z == nil || math.Abs(*score.Z-want) > 1e-12 || score.IdioVolDaily != nil || score.Beta == nil {
		t.Fatalf("broad ETF score = %+v, want z %v", score, want)
	}
	if exit := decisionByID(res, "broad-exit"); exit.Stream != rpc.ScorecardStreamIndexExits {
		t.Fatalf("broad ETF exit stream = %q", exit.Stream)
	}
	// A list member is an index whatever its fit: here it shares the
	// single name's noisy series.
	if q := decisionByID(res, "qqq"); q.Stream != rpc.ScorecardStreamIndexEntries || q.Entry.IndexBasis != rpc.ScorecardIndexBasisList || *q.Entry.MarketRSquared >= IndexRSquared {
		t.Fatalf("list member = stream %q basis %q", q.Stream, q.Entry.IndexBasis)
	}
	if s := decisionByID(res, "single"); s.Stream != rpc.ScorecardStreamEntries || s.Entry.IndexBasis != "" || s.Entry.MarketRSquared == nil || *s.Entry.MarketRSquared >= IndexRSquared {
		t.Fatalf("single name = stream %q basis %q r2 %v", s.Stream, s.Entry.IndexBasis, s.Entry.MarketRSquared)
	}
	entries, _ := sleeveAt(res, SleeveOwner, rpc.ScorecardStreamEntries, PrimaryHorizon)
	indexEntries, _ := sleeveAt(res, SleeveOwner, rpc.ScorecardStreamIndexEntries, PrimaryHorizon)
	indexExits, _ := sleeveAt(res, SleeveOwner, rpc.ScorecardStreamIndexExits, PrimaryHorizon)
	if entries.Decisions != 1 || !entries.Gated || indexEntries.Decisions != 2 || indexEntries.Gated || indexEntries.Gate != "" || indexExits.Decisions != 1 || indexExits.Gated {
		t.Fatalf("streams: entries %d, index entries %d (gated %v), index exits %d", entries.Decisions, indexEntries.Decisions, indexEntries.Gated, indexExits.Decisions)
	}
	if indexEntries.LongPutRealisedVsImplied != nil {
		t.Fatalf("stock index entries reported a long-put median: %+v", indexEntries.LongPutRealisedVsImplied)
	}
	if res.Coverage.Entries != 1 || res.Coverage.Exits != 0 || res.Coverage.IndexEntries != 2 || res.Coverage.IndexExits != 1 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}

func TestSingleNameScoreIsUnchanged(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 29, 400)
	name := derivedBars(301, market, 1.3, 0.018, 47)
	entryIdx := 310
	d := Decision{ID: "acme", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "ACME", UnderlyingConID: 301, ConID: 301, Symbol: "ACME", Instrument: InstrumentStock, Action: "open", Side: SideSell, Sign: -1, Quantity: 10, ExecutedAt: market[entryIdx].Day, ClusterKey: "owner:2026-W01"}
	res := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{301: name}}})
	got := res.Decisions[0]
	if got.Stream != rpc.ScorecardStreamEntries || got.Entry.IndexBasis != "" {
		t.Fatalf("single name routed: stream %q basis %q", got.Stream, got.Entry.IndexBasis)
	}
	// The market-adjusted z, computed here from first principles.
	x, y := preEntryLogReturns(market, entryIdx), preEntryLogReturns(name, entryIdx)
	mx, my := mean(x), mean(y)
	var sxx, sxy float64
	for i := range x {
		sxx += (x[i] - mx) * (x[i] - mx)
		sxy += (x[i] - mx) * (y[i] - my)
	}
	beta := sxy / sxx
	var sse float64
	for i := range x {
		e := y[i] - (my - beta*mx) - beta*x[i]
		sse += e * e
	}
	idio := math.Sqrt(sse / float64(len(x)-2))
	for _, h := range Horizons {
		score := scoreAt(got, h)
		ru := math.Log(name[entryIdx+h].Close / name[entryIdx].Close)
		rm := math.Log(market[entryIdx+h].Close / market[entryIdx].Close)
		want := -(ru - beta*rm) / (idio * math.Sqrt(float64(h)))
		if score.Z == nil || score.TotalVolDaily != nil || score.IdioVolDaily == nil || math.Abs(*score.IdioVolDaily-idio) > 1e-12 || math.Abs(*score.Beta-beta) > 1e-9 || math.Abs(*score.Z-want) > 1e-9 {
			t.Fatalf("horizon %d score = %+v, want z %v beta %v idio %v", h, score, want, beta, idio)
		}
	}
}

func TestIndexClassAndScaleNeverLookAhead(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 31, 400)
	broad := derivedBars(401, market, 1.0, 0.002, 53)
	single := derivedBars(402, market, 0.4, 0.02, 59)
	entryIdx := 300
	decisions := []Decision{
		{ID: "broad", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "BRDX", UnderlyingConID: 401, ConID: 401, Symbol: "BRDX", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 1, ExecutedAt: market[entryIdx].Day, ClusterKey: "owner:w"},
		{ID: "single", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "SNGL", UnderlyingConID: 402, ConID: 402, Symbol: "SNGL", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 1, ExecutedAt: market[entryIdx].Day, ClusterKey: "owner:w"},
	}
	base := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{401: broad, 402: single}}})
	// After entry the broad ETF turns into noise and the single name into
	// the market itself: a look-ahead would swap their classes.
	noisy, tracking := append([]edge.DailyBar(nil), broad...), append([]edge.DailyBar(nil), single...)
	rng := rand.New(rand.NewPCG(61, 67))
	for i := entryIdx + 1; i < len(market); i++ {
		noisy[i].Close = noisy[i-1].Close * math.Exp(0.05*rng.NormFloat64())
		tracking[i].Close = tracking[i-1].Close * market[i].Close / market[i-1].Close
	}
	shifted := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{401: noisy, 402: tracking}}})
	for _, id := range []string{"broad", "single"} {
		a, b := decisionByID(base, id), decisionByID(shifted, id)
		if a.Stream != b.Stream || a.Entry.IndexBasis != b.Entry.IndexBasis || *a.Entry.MarketRSquared != *b.Entry.MarketRSquared {
			t.Fatalf("%s: post-entry bars changed the class: %q/%q r2 %v/%v", id, a.Stream, b.Stream, *a.Entry.MarketRSquared, *b.Entry.MarketRSquared)
		}
		sa, sb := scoreAt(a, PrimaryHorizon), scoreAt(b, PrimaryHorizon)
		if sa.Z == nil || sb.Z == nil || *sa.Z == *sb.Z {
			t.Fatalf("%s: z did not move with the realised path: %v / %v", id, sa.Z, sb.Z)
		}
		if id == "broad" && (sa.TotalVolDaily == nil || *sa.TotalVolDaily != *sb.TotalVolDaily) {
			t.Fatalf("broad: post-entry bars changed sigma_total: %v / %v", sa.TotalVolDaily, sb.TotalVolDaily)
		}
	}
	if decisionByID(base, "broad").Stream != rpc.ScorecardStreamIndexEntries || decisionByID(base, "single").Stream != rpc.ScorecardStreamEntries {
		t.Fatal("classes are not the ones the pre-entry fit implies")
	}
}

func TestOnlyFlatSeriesLeaveTheVolatilityUndefined(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 37, 400)
	flat := make([]edge.DailyBar, len(market))
	for i, bar := range market {
		flat[i] = edge.DailyBar{ConID: 501, Day: bar.Day, Close: 25}
	}
	at := market[300].Day
	decisions := []Decision{
		{ID: "stale", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "STAL", UnderlyingConID: 501, ConID: 501, Symbol: "STAL", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 1, ExecutedAt: at, ClusterKey: "owner:w"},
		{ID: "stale-index", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "SPX", UnderlyingConID: 501, ConID: 502, Symbol: "SPX", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 1, ExecutedAt: at, ClusterKey: "owner:w"},
	}
	res := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{501: flat}}})
	if s := decisionByID(res, "stale"); s.Stream != rpc.ScorecardStreamEntries || s.Entry.MarketRSquared != nil || scoreAt(s, PrimaryHorizon).Reason != ReasonIdioVolZero {
		t.Fatalf("flat single name = stream %q reason %q", s.Stream, scoreAt(s, PrimaryHorizon).Reason)
	}
	if s := decisionByID(res, "stale-index"); s.Stream != rpc.ScorecardStreamIndexEntries || scoreAt(s, PrimaryHorizon).Reason != ReasonTotalVolZero {
		t.Fatalf("flat index = stream %q reason %q", s.Stream, scoreAt(s, PrimaryHorizon).Reason)
	}
}

// TestSectorAdjustmentRemovesTheSectorBeta: a name that moves with its
// sector fund beyond the market is scored net of both when the fund's
// history is given, with less residual noise than the market alone leaves;
// without the fund it is adjusted on the market alone and says why.
func TestSectorAdjustmentRemovesTheSectorBeta(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 420, 29, 400)
	sectorOwn := syntheticBars(0, "2025-01-01", 420, 53, 100)
	// The sector fund is 0.8 market plus its own factor; the name is 1.0
	// market plus 1.5 of the sector's own factor plus noise, so on the two
	// regressors it loads 1.5 on the fund and 1.0 − 1.5 × 0.8 = −0.2 on the
	// market.
	sector := make([]edge.DailyBar, len(market))
	name := make([]edge.DailyBar, len(market))
	rng := rand.New(rand.NewPCG(61, 67))
	sClose, nClose := 100.0, 50.0
	for i, bar := range market {
		if i > 0 {
			rm := math.Log(bar.Close / market[i-1].Close)
			own := math.Log(sectorOwn[i].Close / sectorOwn[i-1].Close)
			sClose *= math.Exp(0.8*rm + own)
			nClose *= math.Exp(rm + 1.5*own + 0.01*rng.NormFloat64())
		}
		sector[i] = edge.DailyBar{ConID: 0, Day: bar.Day, Close: sClose}
		name[i] = edge.DailyBar{ConID: 301, Day: bar.Day, Close: nClose}
	}
	entryIdx := 310
	d := Decision{ID: "syna", Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "SYNA", UnderlyingConID: 301, ConID: 301, Symbol: "SYNA", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: market[entryIdx].Day}
	with := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{301: name},
		Sector: map[string][]edge.DailyBar{"XLK": sector}, SectorFunds: map[int64]string{301: "XLK"}, SectorNames: map[int64]string{301: "Information Technology"}}})
	without := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{301: name}}})
	a, b := with.Decisions[0], without.Decisions[0]
	if a.Entry.MarketAdjustment != MarketAdjustmentSPYSector || a.Entry.Sector != "Information Technology" || a.Entry.SectorFund != "XLK" || a.Entry.SectorReason != "" {
		t.Fatalf("with sector: entry = %+v", a.Entry)
	}
	if b.Entry.MarketAdjustment != MarketAdjustmentSPY || b.Entry.SectorReason != ReasonSectorUnknown || b.Entry.SectorFund != "" {
		t.Fatalf("without sector: entry = %+v", b.Entry)
	}
	for _, h := range Horizons {
		sa, sb := scoreAt(a, h), scoreAt(b, h)
		if sa.Z == nil || sb.Z == nil || sa.SectorBeta == nil || sa.SectorReturn == nil || sb.SectorBeta != nil {
			t.Fatalf("horizon %d: with %+v without %+v", h, sa, sb)
		}
		if math.Abs(*sa.SectorBeta-1.5) > 0.15 || math.Abs(*sa.Beta+0.2) > 0.15 {
			t.Fatalf("horizon %d: betas %v (market) %v (sector), want about −0.2 and 1.5", h, *sa.Beta, *sa.SectorBeta)
		}
		if *sa.IdioVolDaily >= *sb.IdioVolDaily*0.5 {
			t.Fatalf("horizon %d: the sector left idio vol %v against %v on the market alone", h, *sa.IdioVolDaily, *sb.IdioVolDaily)
		}
		// From first principles: the residual over the horizon in units of
		// the two-factor residual noise.
		ru := math.Log(name[entryIdx+h].Close / name[entryIdx].Close)
		rm := math.Log(market[entryIdx+h].Close / market[entryIdx].Close)
		rs := math.Log(sector[entryIdx+h].Close / sector[entryIdx].Close)
		want := (ru - *sa.Beta*rm - *sa.SectorBeta*rs) / (*sa.IdioVolDaily * math.Sqrt(float64(h)))
		if math.Abs(*sa.Z-want) > 1e-9 || math.Abs(*sa.SectorReturn-rs) > 1e-12 {
			t.Fatalf("horizon %d: z %v, want %v", h, *sa.Z, want)
		}
	}
	sleeve, _ := sleeveAt(with, SleeveOwner, rpc.ScorecardStreamEntries, PrimaryHorizon)
	if sleeve.SectorAdjusted != 1 || sleeve.Scored != 1 {
		t.Fatalf("sleeve = %+v", sleeve)
	}
	// A fund whose history does not cover the window leaves the market
	// alone, with the reason.
	short := Evaluate(Input{AsOf: day("2026-06-01"), Decisions: []Decision{d}, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{301: name},
		Sector: map[string][]edge.DailyBar{"XLK": sector[300:]}, SectorFunds: map[int64]string{301: "XLK"}}})
	if e := short.Decisions[0].Entry; e.MarketAdjustment != MarketAdjustmentSPY || e.SectorReason != ReasonSectorHistoryMissing || e.SectorFund != "XLK" {
		t.Fatalf("short sector history: entry = %+v", e)
	}
}

// TestSlippageNamesTheRealCause: a decision with a retained cost carries
// it and the stream sums it; a fill before the daemon retained quotes reads
// no_fill_quote and a later fill without a record execution_not_observed;
// a record without a usable quote keeps its own reason. The deprecated
// per-horizon keys repeat the decision's slippage.
func TestSlippageNamesTheRealCause(t *testing.T) {
	t.Parallel()
	market := syntheticBars(0, "2025-01-01", 700, 29, 400)
	name := derivedBars(301, market, 1.1, 0.015, 47)
	before, after := FillQuoteCaptureSince.AddDate(0, 0, -30), FillQuoteCaptureSince.AddDate(0, 0, 3)
	mk := func(id string, at time.Time) Decision {
		return Decision{ID: id, Sleeve: SleeveOwner, Stream: rpc.ScorecardStreamEntries, Underlying: "SYNA", UnderlyingConID: 301, ConID: 301, Symbol: "SYNA", Instrument: InstrumentStock, Action: "open", Side: SideBuy, Sign: 1, Quantity: 10, ExecutedAt: at}
	}
	decisions := []Decision{mk("old", before), mk("unseen", after), mk("quoted", after.Add(time.Hour)), mk("quoted-2", after.Add(2*time.Hour)), mk("timeout", after.Add(3*time.Hour)), mk("no-fx", after.Add(4*time.Hour))}
	costs := map[string]FillCost{
		"quoted":   {Match: "exec_id", Fills: 1, Quoted: 1, HalfSpread: new(13.0), Currency: "USD", HalfSpreadBase: new(11.7), Bp: new(400.0), GapMS: new(int64(2000))},
		"quoted-2": {Match: "time", Fills: 1, Quoted: 1, HalfSpread: new(-2.0), Currency: "USD", HalfSpreadBase: new(-1.8), Bp: new(-50.0)},
		"timeout":  {Match: "exec_id", Fills: 1, Reason: "timeout"},
		// Measured in the contract currency, no FX rate at the quote: the
		// decision is measured, the stream counts it under fx_unavailable
		// and its basis points still enter the mean.
		"no-fx": {Match: "exec_id", Fills: 1, Quoted: 1, HalfSpread: new(4.0), Currency: "USD", Bp: new(100.0), Reason: ReasonFXUnavailable},
	}
	res := Evaluate(Input{AsOf: day("2027-01-01"), Decisions: decisions, History: History{Market: market, Underlying: map[int64][]edge.DailyBar{301: name}}, Costs: costs})
	want := map[string]rpc.ScorecardSlippage{
		"old":      {Status: "unknown", Reason: ReasonNoFillQuote},
		"unseen":   {Status: "unknown", Reason: ReasonExecutionNotObserved},
		"timeout":  {Status: "unknown", Reason: "timeout", Match: "exec_id", Fills: 1},
		"quoted":   {Status: "measured", Match: "exec_id", Fills: 1, Quoted: 1, Currency: "USD"},
		"quoted-2": {Status: "measured", Match: "time", Fills: 1, Quoted: 1, Currency: "USD"},
		"no-fx":    {Status: "measured", Match: "exec_id", Fills: 1, Quoted: 1, Currency: "USD", Reason: ReasonFXUnavailable},
	}
	for id, w := range want {
		got := decisionByID(res, id)
		if got.Slippage == nil || got.Slippage.Status != w.Status || got.Slippage.Reason != w.Reason || got.Slippage.Match != w.Match || got.Slippage.Fills != w.Fills || got.Slippage.Quoted != w.Quoted {
			t.Fatalf("%s slippage = %+v, want %+v", id, got.Slippage, w)
		}
		for _, score := range got.Scores {
			if score.SlippageReason != w.Reason || (w.Status == "measured") != (score.SlippageBp != nil) {
				t.Fatalf("%s horizon %d repeats %q / %v, want %q", id, score.Sessions, score.SlippageReason, score.SlippageBp, w.Reason)
			}
		}
	}
	if q := decisionByID(res, "quoted").Slippage; *q.HalfSpreadBase != 11.7 || *q.Bp != 400 || *q.GapMS != 2000 {
		t.Fatalf("quoted = %+v", q)
	}
	sleeve, _ := sleeveAt(res, SleeveOwner, rpc.ScorecardStreamEntries, PrimaryHorizon)
	if s := sleeve.Slippage; s == nil || s.Decisions != 6 || s.Quoted != 2 || math.Abs(s.HalfSpreadBase-9.9) > 1e-9 || s.MeanBp == nil || *s.MeanBp != 150 || s.Unquoted[ReasonNoFillQuote] != 1 || s.Unquoted[ReasonExecutionNotObserved] != 1 || s.Unquoted["timeout"] != 1 || s.Unquoted[ReasonFXUnavailable] != 1 || s.Unquoted[ReasonFXUnknown] != 0 {
		t.Fatalf("summary = %+v", s)
	}
	if res.Coverage.SlippageQuoted != 3 || res.Coverage.SlippageByReason[ReasonNoFillQuote] != 1 || res.Coverage.SlippageByReason["timeout"] != 1 {
		t.Fatalf("coverage = %+v", res.Coverage)
	}
}
