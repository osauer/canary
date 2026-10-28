package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The mandate's forced reduction, witnessed on synthetic books: the daemon's
// own evaluator over seeded random-walk histories (mandateTestSeries), the
// verdict built from the same position rows the engine sizes on.

var mandateReductionVols = map[string]struct {
	vol  float64
	seed uint64
}{"SPY": {0.16, 1}, "SYNA": {0.30, 2}, "SYNB": {0.40, 3}, "SYNC": {0.25, 5}, "IBCF": {0.15, 8}}

func mandateReductionStock(symbol string, conID int, shares, mark float64) rpc.PositionView {
	return rpc.PositionView{Symbol: symbol, SecType: rpc.SecTypeStock, ConID: conID, Exchange: "SMART", Currency: "EUR",
		Quantity: shares, Multiplier: 1, Mark: mark, DataType: rpc.MarketDataLive}
}

// mandateReductionCall is an option line priced at its mark, the price paid
// the mark too, so a bought line's premium at risk is its value.
func mandateReductionCall(symbol string, conID int, contracts, strike, spot, mark float64, expiry string) rpc.PositionView {
	iv := 0.4
	mv := contracts * 100 * mark
	return rpc.PositionView{Symbol: symbol, SecType: rpc.SecTypeOption, ConID: conID, Exchange: "SMART", Currency: "EUR",
		Quantity: contracts, Multiplier: 100, Mark: mark, AvgCost: mark * 100, MarketValue: mv, MarketValueBase: &mv,
		Expiry: expiry, Strike: strike, Right: "C", IV: &iv, Underlying: new(spot),
		TradingClass: symbol, DataType: rpc.MarketDataLive}
}

func mandateReductionBook(rows ...rpc.PositionView) *rpc.PositionsResult {
	pos := &rpc.PositionsResult{AsOf: mandateTestNow, Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"}}
	for _, row := range rows {
		if row.SecType == rpc.SecTypeOption {
			pos.Options = append(pos.Options, row)
		} else {
			pos.Stocks = append(pos.Stocks, row)
		}
	}
	return pos
}

// mandateReductionFixture measures pos with the evaluator and arms a stop
// under every share line so the loss measure reads ratio times the loss
// room (0.3 × budgetBase at full room): within under 1, reduce above.
// Bought options count at their premium (mandateReductionCall prices the
// line at its mark). The volatility budget is set to the same ratio, so the
// sleeve allocations and the owner sleeve's stop read beside it as before.
// budgetBase sets the gap cap (a tenth of it, at full room); a large one
// keeps every name inside its cap.
func mandateReductionFixture(t *testing.T, pos *rpc.PositionsResult, ratio, budgetBase float64, streak int) mandateReductionInput {
	t.Helper()
	now := mandateTestNow
	legs, notModelled, fxNow, reasons := mandateLegsFromPositions(pos, "EUR", now)
	if len(reasons) > 0 {
		t.Fatalf("fixture legs: %v", reasons)
	}
	in := mandate.Input{Now: now, BaseCurrency: "EUR", Policy: mandate.Policy{MaxLossPct: 30, VolTargetPct: 15, GapShockPct: 25, GapRoomPct: 33.3},
		Capital: mandate.Capital{Known: true, BaseCurrency: "EUR", BudgetBase: budgetBase, Room: 1, BudgetAnnualSD: 1},
		Legs:    legs, NotModelled: notModelled, FXNow: fxNow, Series: map[string]mandate.Series{}, FX: map[string]mandate.Series{}}
	for sym, s := range mandateReductionVols {
		in.Series[sym] = mandateSeriesInput(mandateTestSeries("u:"+sym, s.vol, s.seed, 0), now)
	}
	if _, ok := fxNow["USD"]; ok {
		// A USD line (the hedge's future) needs the rate's history too.
		in.FX["USD"] = mandateSeriesInput(mandateTestSeries("fx:USD", 0.08, 9, 0), now)
	}
	probe := mandate.Evaluate(in)
	if probe.Risk == nil || probe.Loss == nil {
		t.Fatalf("fixture unmeasured: %v", probe.Reasons)
	}
	in.Capital.BudgetAnnualSD = probe.Risk.SDAnnualBase / ratio
	mandateArmTestStops(t, in.Legs, pos, ratio*mandate.LossRoomBase(in.Policy, in.Capital)-probe.Loss.TotalBase)
	return mandateReductionVerdict(in, streak)
}

// mandateArmTestStops arms a stop under every share line of legs, the loss
// to it the same share of each line's value, so the lines together lose
// want in base currency (every fixture row is in EUR).
func mandateArmTestStops(t *testing.T, legs []mandate.Leg, pos *rpc.PositionsResult, want float64) {
	t.Helper()
	marks := map[string]float64{}
	value := 0.0
	for _, row := range pos.Stocks {
		if positionIsFuture(row) {
			continue // a future counts its index-move loss, never a test stop
		}
		marks[mandateLegID(row)] = row.Mark
		value += math.Abs(row.Quantity) * row.Mark
	}
	if want < -1e-6 {
		t.Fatalf("fixture: the bought options' premium alone is %.0f over ratio × room", -want)
	}
	if value == 0 {
		return
	}
	f := want / value
	for i := range legs {
		if legs[i].Kind != mandate.KindStock || mandate.IsIndexFuture(legs[i]) {
			continue
		}
		legs[i].LossPerUnitBase, legs[i].StopArmed = f*marks[legs[i].ID], true
	}
}

// mandateReductionRelegs is the verdict's book with its legs read afresh
// from pos, every line keeping the stop the fixture armed under it.
func mandateReductionRelegs(t *testing.T, in mandateReductionInput, pos *rpc.PositionsResult) mandate.Input {
	t.Helper()
	book := *in.Input
	legs, _, _, reasons := mandateLegsFromPositions(pos, "EUR", book.Now)
	if len(reasons) > 0 {
		t.Fatalf("fixture legs: %v", reasons)
	}
	for i := range legs {
		if j := slices.IndexFunc(book.Legs, func(l mandate.Leg) bool { return l.ID == legs[i].ID }); j >= 0 && legs[i].Kind == mandate.KindStock {
			legs[i].LossPerUnitBase, legs[i].StopArmed = book.Legs[j].LossPerUnitBase, book.Legs[j].StopArmed
		}
	}
	book.Legs = legs
	return book
}

// mandateLossRoom is the fixture's loss room.
func mandateLossRoom(in mandateReductionInput) float64 {
	return mandate.LossRoomBase(in.Input.Policy, in.Input.Capital)
}

// mandateLossAfter is the evaluator's loss measure with the given cuts
// applied to the verdict's book.
func mandateLossAfter(t *testing.T, in mandateReductionInput, cuts map[int]float64) float64 {
	t.Helper()
	v := mandate.Evaluate(mandateBookCut(in, cuts))
	if v.Loss == nil || v.Status == mandate.StatusUnknown {
		t.Fatalf("unmeasured after cuts: %v", v.Reasons)
	}
	return v.Loss.TotalBase
}

// mandateBookCut is the verdict's book with the cuts applied.
func mandateBookCut(in mandateReductionInput, cuts map[int]float64) mandate.Input {
	book := *in.Input
	book.Legs = slices.Clone(book.Legs)
	for i := range book.Legs {
		for conID, units := range cuts {
			if book.Legs[i].ID == mandateLegID(rpc.PositionView{ConID: conID}) {
				if book.Legs[i].Quantity > 0 {
					book.Legs[i].Quantity -= units
				} else {
					book.Legs[i].Quantity += units
				}
			}
		}
	}
	return book
}

// mandateReductionVerdict evaluates in as the daemon's kept verdict.
func mandateReductionVerdict(in mandate.Input, streak int) mandateReductionInput {
	v := mandate.Evaluate(in)
	res := &rpc.MandateRiskResult{AsOf: in.Now, Active: true, Verdict: &v,
		PolicyFingerprint: &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: "sha256:synthetic"}}
	return mandateReductionInput{Active: true, Result: res, Input: &in, EvaluatedAt: in.Now.Add(-time.Minute), OverStreak: streak}
}

func mandateReductionRun(t *testing.T, in mandateReductionInput, pos *rpc.PositionsResult) ([]rpc.TradeProposal, *rpc.TradeProposalMandateStatus) {
	t.Helper()
	engine := &proposalEngine{}
	return engine.mandateReductionProposals(defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, in, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, mandateTestNow)
}

// mandateRiskAfter is the evaluator's risk with the given cuts applied to the
// verdict's book.
func mandateRiskAfter(t *testing.T, in mandateReductionInput, cuts map[int]float64) float64 {
	t.Helper()
	v := mandate.Evaluate(mandateBookCut(in, cuts))
	if v.Risk == nil {
		t.Fatalf("unmeasured after cuts: %v", v.Reasons)
	}
	return v.Risk.SDAnnualBase
}

func assertMandateRowsReduceOnly(t *testing.T, rows []rpc.TradeProposal, pos *rpc.PositionsResult) {
	t.Helper()
	held := map[int]float64{}
	for _, row := range slices.Concat(pos.Stocks, pos.Options) {
		held[row.ConID] = row.Quantity
	}
	for _, row := range rows {
		q := held[row.Contract.ConID]
		wantAction := rpc.OrderActionSell
		if q < 0 {
			wantAction = rpc.OrderActionBuy
		}
		if row.Bucket != rpc.TradeProposalBucketMandateReduction || row.Action != wantAction || !proposalCloseReduceEffect(row.PositionEffect) ||
			row.Quantity < 1 || float64(row.Quantity) > math.Abs(q) || !row.NeverSkipVeto || row.Mandate == nil || row.Mandate.Units < row.Quantity ||
			float64(row.Mandate.Units) > math.Abs(q) || row.PositionQuantity != q {
			t.Fatalf("row would not reduce within the position (held %g): %+v mandate %+v", q, row, row.Mandate)
		}
		if (row.PositionEffect == rpc.OrderPositionEffectClose) != (float64(row.Quantity) == math.Abs(q)) {
			t.Fatalf("effect %s disagrees with %d of %g", row.PositionEffect, row.Quantity, q)
		}
	}
}

func mandateRowFor(rows []rpc.TradeProposal, conID int) (rpc.TradeProposal, bool) {
	for _, row := range rows {
		if row.Contract.ConID == conID {
			return row, true
		}
	}
	return rpc.TradeProposal{}, false
}

// The name with the largest premium at risk (here the loss to its stop) is
// ranked first and cut by the smallest share lot that takes the loss measure
// to 0.95 × the loss room; the second name gets no row while the first
// suffices, and does when it does not (spec TM-04.12).
func TestMandateReductionDominantFirstSizedToTargetSecondOnlyWhenNeeded(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	in := mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	if v := in.Result.Verdict; v.Status != mandate.StatusReduce || v.Loss == nil || math.Abs(v.Loss.RatioToRoom-1.1) > 1e-6 {
		t.Fatalf("fixture verdict %s loss %+v, want reduce at 110%% of the room", v.Status, v.Loss)
	}
	rows, st := mandateReductionRun(t, in, pos)
	assertMandateRowsReduceOnly(t, rows, pos)
	assertMandateStepsLowerRisk(t, in, rows)
	if st == nil || st.State != rpc.MandateReductionReducing || !st.TargetReached || st.Rows != 1 || len(rows) != 1 || len(st.Plan) != 1 {
		t.Fatalf("status %+v rows %d", st, len(rows))
	}
	syna := rows[0]
	m := syna.Mandate
	room := mandateLossRoom(in)
	target := 0.95 * room
	if syna.Contract.ConID != 9001 || m.Rank != 1 || m.BatchRows != 1 || m.Phase != rpc.MandatePhasePremium || syna.Quantity != m.Units || m.Units%100 != 0 ||
		syna.PositionEffect != rpc.OrderPositionEffectReduce || m.LegLossBase < 0.5*m.LossBase || math.Abs(m.LossRoomBase-room) > 1e-6 || math.Abs(m.LossTargetBase-target) > 1e-6 {
		t.Fatalf("SYNA row %+v mandate %+v", syna, m)
	}
	if after := mandateLossAfter(t, in, map[int]float64{9001: float64(m.Units)}); after > target+1e-6 || math.Abs(after-m.LossAfterBase) > 1e-6 || math.Abs(m.LossBase-after-m.LossCutBase) > 1e-6 {
		t.Fatalf("loss after %d shares = %.2f (row says %.2f, cut %.2f of %.2f), target %.2f", m.Units, after, m.LossAfterBase, m.LossCutBase, m.LossBase, target)
	}
	if less := mandateLossAfter(t, in, map[int]float64{9001: float64(m.Units - 100)}); less <= target {
		t.Fatalf("one lot less (%d shares) already reaches the target: %.2f <= %.2f", m.Units-100, less, target)
	}
	if m.VerdictFingerprint != mandateVerdictFingerprint(in.Result) || m.VerdictFingerprint == "" || m.VerdictStatus != mandate.StatusReduce ||
		!m.VerdictAt.Equal(in.Result.Verdict.AsOf) || st.VerdictFingerprint != m.VerdictFingerprint || m.Held {
		t.Fatalf("row does not carry its verdict: %+v status %+v", m, st)
	}
	if !strings.Contains(syna.Reason, "the largest premium at risk left, SYNA") || !strings.Contains(syna.Reason, "inside the") ||
		!slices.ContainsFunc(syna.Details, func(d string) bool { return strings.Contains(d, "sized on the reduce verdict of") }) {
		t.Fatalf("reason %q details %q", syna.Reason, syna.Details)
	}
	// The volatility is reported beside the measure, no longer the headline.
	if m.RiskAnnualBase <= 0 || m.BudgetAnnualSD <= 0 || m.RiskCutAnnualBase <= 0 || st.LossBase != m.LossBase || st.LossRoomBase != room || st.ProjectedLossBase == nil || *st.ProjectedLossBase != m.LossAfterBase ||
		st.Plan[0].LossCutBase != m.LossCutBase || !strings.Contains(st.Reason, "sized to take the loss measure to") {
		t.Fatalf("volatility beside the loss: mandate %+v status %+v", m, st)
	}

	// A room the first name alone cannot meet: SYNA is closed (the cut that
	// takes the most off the measure), then SYNB is cut to the target.
	in = mandateReductionFixture(t, pos, 10, 1e7, 2)
	rows, st = mandateReductionRun(t, in, pos)
	assertMandateRowsReduceOnly(t, rows, pos)
	assertMandateStepsLowerRisk(t, in, rows)
	if len(rows) != 2 || !st.TargetReached {
		t.Fatalf("rows %d status %+v", len(rows), st)
	}
	first, _ := mandateRowFor(rows, 9001)
	second, _ := mandateRowFor(rows, 9002)
	if first.Mandate == nil || second.Mandate == nil || first.Mandate.Rank != 1 || second.Mandate.Rank != 2 || first.PositionEffect != rpc.OrderPositionEffectClose ||
		first.Quantity != 3000 || second.PositionEffect != rpc.OrderPositionEffectReduce || second.Mandate.Units%10 != 0 {
		t.Fatalf("SYNA %+v / SYNB %+v", first.Mandate, second.Mandate)
	}
	target = 0.95 * mandateLossRoom(in)
	if after := mandateLossAfter(t, in, map[int]float64{9001: 3000, 9002: float64(second.Mandate.Units)}); after > target+1e-6 {
		t.Fatalf("batch leaves %.2f over the target %.2f", after, target)
	}
	if less := mandateLossAfter(t, in, map[int]float64{9001: 3000, 9002: float64(second.Mandate.Units - 10)}); less <= target {
		t.Fatalf("SYNB cut is not the smallest lot: %d - 10 already reaches it", second.Mandate.Units)
	}
}

// mandateExercisedRisk is the evaluator's risk of the verdict's book with
// the cuts applied and the remaining contracts of one option exercised.
func mandateExercisedRisk(t *testing.T, in mandateReductionInput, cuts map[int]float64, optConID int) float64 {
	t.Helper()
	book := *in.Input
	book.Legs = slices.Clone(book.Legs)
	var opt mandate.Leg
	for i := range book.Legs {
		for conID, units := range cuts {
			if book.Legs[i].ID == mandateLegID(rpc.PositionView{ConID: conID}) {
				if book.Legs[i].Quantity > 0 {
					book.Legs[i].Quantity -= units
				} else {
					book.Legs[i].Quantity += units
				}
			}
		}
		if book.Legs[i].ID == mandateLegID(rpc.PositionView{ConID: optConID}) {
			opt = book.Legs[i]
		}
	}
	v := mandate.Evaluate(mandateExercise(book, opt, int(opt.Quantity)))
	if v.Risk == nil {
		t.Fatalf("unmeasured exercised book: %v", v.Reasons)
	}
	return v.Risk.SDAnnualBase
}

// An in-the-money long call two sessions before expiry is cut first only
// where its exercise alone would push the planned book over budget, and by
// that need: a near-the-money call whose delivered shares would add a third
// to its risk is cut until the exercised book is inside the target; a deep
// call whose exercise adds next to nothing is left to the variance; the same
// call a month out never passes the gate.
func TestMandateReductionExpiryOptionCutByItsOwnNeed(t *testing.T) {
	t.Parallel()
	// mandateTestNow is Wednesday 14 October 2026: Thursday and Friday are
	// the two sessions to a Friday 16 October expiry.
	call := mandateReductionCall("SYNB", 9005, 20, 79, 80, 2.4, "20261016")
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), call)
	in := mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	rows, st := mandateReductionRun(t, in, pos)
	assertMandateRowsReduceOnly(t, rows, pos)
	assertMandateStepsLowerRisk(t, in, rows)
	opt, ok := mandateRowFor(rows, 9005)
	if !ok || opt.Mandate.Rank != 1 || opt.Mandate.Phase != rpc.MandatePhaseExpiry || opt.Mandate.SessionsToExpiry == nil || *opt.Mandate.SessionsToExpiry != 2 ||
		!strings.Contains(opt.Reason, "2 sessions before expiry") {
		t.Fatalf("expiry row %+v mandate %+v (status %+v)", opt, opt.Mandate, st)
	}
	cuts := map[int]float64{9005: float64(opt.Mandate.Units)}
	if syna, ok := mandateRowFor(rows, 9001); ok {
		cuts[9001] = float64(syna.Mandate.Units)
	}
	// Sized by its own need, not closed: the exercised book, after the whole
	// batch, stays within the volatility budget (delivered shares carry no
	// premium, so the exercise is judged by the sizing input), and without
	// the expiry cut it would not.
	budget := in.Result.Verdict.Capital.BudgetAnnualSD
	if risk := mandateExercisedRisk(t, in, cuts, 9005); risk > budget+1e-6 || !st.TargetReached || opt.Mandate.Units >= 20 {
		t.Fatalf("exercised after the batch %.0f, budget %.0f, %d of 20 cut (status %+v)", risk, budget, opt.Mandate.Units, st)
	}
	without := maps.Clone(cuts)
	delete(without, 9005)
	if risk := mandateExercisedRisk(t, in, without, 9005); risk <= budget {
		t.Fatalf("the exercise alone does not push the reduced book over budget (%.0f <= %.0f); the gate had no need", risk, budget)
	}

	// A deep call: its exercise adds next to nothing to the reduced book.
	deep := mandateReductionCall("SYNB", 9005, 5, 60, 80, 20, "20261016")
	pos = mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), deep)
	in = mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	rows, _ = mandateReductionRun(t, in, pos)
	for _, row := range rows {
		if row.Mandate.Phase == rpc.MandatePhaseExpiry {
			t.Fatalf("a deep call whose exercise does not push the book over went through the gate: %+v", row.Mandate)
		}
	}

	// The near call a month out: no expiry gate.
	later := mandateReductionCall("SYNB", 9005, 20, 79, 80, 6, "20261120")
	pos = mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), later)
	in = mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	rows, _ = mandateReductionRun(t, in, pos)
	for _, row := range rows {
		if row.Mandate.Phase == rpc.MandatePhaseExpiry {
			t.Fatalf("a call a month out went through the expiry gate: %+v", row.Mandate)
		}
	}
}

// The batch starts on two consecutive scheduled verdicts over or reduce, at
// least five minutes apart; one reading, even reduce or a gap-cap breach,
// watches; a running episode keeps it going. The cache counts only scheduled readings, never a read's, and any
// reading neither over nor reduce resets it, as do a gap of more than
// fifteen minutes and a new policy.
func TestMandateReductionStartsOnConfirmedVerdicts(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	for _, tc := range []struct {
		name    string
		ratio   float64
		streak  int
		episode string
		state   string
	}{
		{"one reading just over", 1.1, 1, "", rpc.MandateReductionWatching},
		{"two readings just over", 1.1, 2, "", rpc.MandateReductionReducing},
		{"one reading far over", 1.5, 1, "", rpc.MandateReductionWatching},
		{"two readings far over", 1.5, 2, "", rpc.MandateReductionReducing},
		{"no scheduled reading, episode running", 1.1, 0, "mandate-ep", rpc.MandateReductionReducing},
	} {
		in := mandateReductionFixture(t, pos, tc.ratio, 1e7, tc.streak)
		in.Episode = tc.episode
		rows, st := mandateReductionRun(t, in, pos)
		if st.State != tc.state || (tc.state == rpc.MandateReductionWatching) != (len(rows) == 0) {
			t.Fatalf("%s: rows %d status %+v", tc.name, len(rows), st)
		}
		if tc.state == rpc.MandateReductionWatching && !strings.Contains(st.Reason, "a confirming scheduled verdict at least 5m0s later starts the batch") {
			t.Fatalf("%s: reason %q", tc.name, st.Reason)
		}
		if tc.state == rpc.MandateReductionReducing && (st.Episode == "" || rows[0].Mandate.Episode != st.Episode || tc.episode != "" && st.Episode != tc.episode) {
			t.Fatalf("%s: episode %q row %q", tc.name, st.Episode, rows[0].Mandate.Episode)
		}
	}
	// A gap-cap breach needs the confirming reading too.
	gap := mandateReductionFixture(t, pos, 0.5, 600000, 1)
	if rows, st := mandateReductionRun(t, gap, pos); len(rows) != 0 || st.State != rpc.MandateReductionWatching {
		t.Fatalf("gap breach on one reading: rows %d status %+v", len(rows), st)
	}
	gap.OverStreak = 2
	if rows, st := mandateReductionRun(t, gap, pos); len(rows) == 0 || st.State != rpc.MandateReductionReducing {
		t.Fatalf("gap breach on two readings: rows %d status %+v", len(rows), st)
	}

	verdict := func(status string) *rpc.MandateRiskResult {
		return &rpc.MandateRiskResult{Verdict: &mandate.Verdict{Status: status}}
	}
	var c mandateVerdictCache
	at := mandateTestNow
	for i, step := range []struct {
		status    string
		gap       time.Duration
		fp        string
		scheduled bool
		want      int
	}{
		{mandate.StatusOver, 0, "a", true, 1},
		{mandate.StatusOver, 2 * time.Minute, "a", true, 1},    // too soon: neither counts nor resets
		{mandate.StatusReduce, 2 * time.Minute, "a", false, 1}, // a read never counts
		{mandate.StatusOver, 2 * time.Minute, "a", true, 2},    // six minutes after the first
		{mandate.StatusReduce, 6 * time.Minute, "a", true, 3},
		{mandate.StatusWithin, time.Minute, "a", false, 0}, // any reading within resets
		{mandate.StatusOver, time.Minute, "a", true, 1},
		{mandate.StatusOver, 16 * time.Minute, "a", true, 1}, // too long a gap starts again
		{mandate.StatusUnknown, time.Minute, "a", true, 0},
		{mandate.StatusOver, time.Minute, "a", true, 1},
		{mandate.StatusOver, 5 * time.Minute, "b", true, 1}, // a new policy starts again
		{mandate.StatusOver, 5 * time.Minute, "b", true, 2},
	} {
		at = at.Add(step.gap)
		c.set(verdict(step.status), &mandate.Input{}, at, step.fp, step.scheduled, daemonBrokerEvidenceBinding{})
		if got := c.kept().overStreak; got != step.want {
			t.Fatalf("step %d (%s after %s, policy %s, scheduled %v): streak %d, want %d", i, step.status, step.gap, step.fp, step.scheduled, got, step.want)
		}
	}
}

// Unknown never sizes: without a standing batch nothing is proposed; a
// standing batch is held, unsent, with its key, size and effect (so its
// revision and veto window stand), and preview and submit refuse it. A stale
// verdict and a verdict of another book hold the same way.
func TestMandateReductionUnknownHoldsStandingRows(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	over := mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	standing, _ := mandateReductionRun(t, over, pos)
	if len(standing) == 0 {
		t.Fatal("no batch to hold")
	}

	unknownBook := *over.Input
	unknownBook.Series = map[string]mandate.Series{"SPY": unknownBook.Series["SPY"], "SYNA": unknownBook.Series["SYNA"]} // SYNB history missing
	unknown := mandateReductionVerdict(unknownBook, 0)
	if unknown.Result.Verdict.Status != mandate.StatusUnknown {
		t.Fatalf("fixture %s, want unknown", unknown.Result.Verdict.Status)
	}
	if rows, st := mandateReductionRun(t, unknown, pos); len(rows) != 0 || st.State != rpc.MandateReductionUnknown || !strings.Contains(st.Reason, "SYNB") {
		t.Fatalf("unknown without a batch: rows %d status %+v", len(rows), st)
	}
	stale := over
	stale.EvaluatedAt = mandateTestNow.Add(-6 * time.Minute)
	changed := over
	moved := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 400, 80))
	for name, tc := range map[string]struct {
		in    mandateReductionInput
		pos   *rpc.PositionsResult
		state string
		why   string
	}{
		"unknown":      {unknown, pos, rpc.MandateReductionHeld, "cannot be measured"},
		"stale":        {stale, pos, rpc.MandateReductionHeld, "old; the batch trusts one up to 5m0s"},
		"book changed": {changed, moved, rpc.MandateReductionHeld, "positions changed since the verdict"},
	} {
		in := tc.in
		in.Previous = standing
		rows, st := mandateReductionRun(t, in, tc.pos)
		if len(rows) != len(standing) || st.State != tc.state || !st.Held || !strings.Contains(st.HoldReason, tc.why) || st.Rows != len(rows) {
			t.Fatalf("%s: rows %d status %+v", name, len(rows), st)
		}
		for i, row := range rows {
			prev := standing[i]
			if row.Key != prev.Key || row.Quantity != prev.Quantity || row.PositionEffect != prev.PositionEffect || !row.Mandate.Held ||
				!strings.Contains(row.Mandate.HoldReason, tc.why) || row.Mandate.VerdictFingerprint != prev.Mandate.VerdictFingerprint || !row.NeverSkipVeto ||
				len(row.Blockers) != 0 || !row.AutomaticEligible() {
				t.Fatalf("%s: held row %+v mandate %+v, standing %+v", name, row, row.Mandate, prev)
			}
			if slices.Equal(proposalRevisionTerms(row), proposalRevisionTerms(prev)) == false {
				t.Fatalf("%s: holding changed the revision terms %v → %v", name, proposalRevisionTerms(prev), proposalRevisionTerms(row))
			}
			if b := mandateHeldBlockers(row); len(b) != 1 || b[0].Code != mandateSizingHeldCode {
				t.Fatalf("%s: held blockers %+v", name, b)
			}
		}
		if counts := proposalCounts(rows, "EUR"); counts.Actionable != 0 || counts.MandateReduction != len(rows) {
			t.Fatalf("%s: counts %+v", name, counts)
		}
	}
	// A held row whose own position moved since it was sized is dropped.
	moved = mandateReductionBook(mandateReductionStock("SYNA", 9001, 2900, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	in := unknown
	in.Previous = standing
	if rows, st := mandateReductionRun(t, in, moved); len(rows) != 0 || st.State != rpc.MandateReductionUnknown {
		t.Fatalf("a moved position stayed held: rows %d status %+v", len(rows), st)
	}

	// Preview and submit refuse a held row before any broker call.
	in = unknown
	in.Previous = standing
	held, _ := mandateReductionRun(t, in, pos)
	engine := &proposalEngine{server: newTestServer(t), store: &proposalStore{}, now: func() time.Time { return mandateTestNow }}
	engine.resolve = func(_ context.Context, _, _ string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
		return held[0], nil, nil
	}
	prev, err := engine.preview(t.Context(), rpc.TradeProposalPreviewParams{Key: held[0].Key, Revision: "r"}, nil, nil)
	if err != nil || prev.Accepted || len(prev.Blockers) != 1 || prev.Blockers[0].Code != mandateSizingHeldCode {
		t.Fatalf("preview of a held row: %+v err %v", prev, err)
	}
	sub, err := engine.submit(t.Context(), rpc.TradeProposalSubmitParams{Key: held[0].Key, Revision: "r", FastPath: true}, proposalSubmitOptions{})
	if err != nil || sub.Accepted || len(sub.Blockers) != 1 || sub.Blockers[0].Code != mandateSizingHeldCode {
		t.Fatalf("submit of a held row: %+v err %v", sub, err)
	}
}

// Back within budget, every standing row leaves the set: nothing waits, and
// nothing is cancelled at the broker (the engine has no such path).
func TestMandateReductionWithinWithdrawsStandingRows(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	standing, _ := mandateReductionRun(t, mandateReductionFixture(t, pos, 1.1, 1e7, 2), pos)
	within := mandateReductionFixture(t, pos, 0.9, 1e7, 0)
	within.Previous = standing
	rows, st := mandateReductionRun(t, within, pos)
	if len(rows) != 0 || st.State != rpc.MandateReductionWithin || st.Withdrawn != len(standing) || !strings.Contains(st.Reason, "orders already at the broker are untouched") {
		t.Fatalf("within: rows %d status %+v", len(rows), st)
	}
}

// mandateReductionNext is a later verdict on the same book, two minutes on,
// with the last generation standing.
func mandateReductionNext(t *testing.T, pos *rpc.PositionsResult, ratio float64, standing []rpc.TradeProposal) mandateReductionInput {
	t.Helper()
	next := mandateReductionFixture(t, pos, ratio, 1e7, 3)
	next.Result.Verdict.AsOf = mandateTestNow.Add(2 * time.Minute)
	next.Input.Now = next.Result.Verdict.AsOf
	next.EvaluatedAt = mandateTestNow
	next.Previous = standing
	if len(standing) > 0 {
		next.Episode = standing[0].Mandate.Episode
	}
	return next
}

// Every verdict sizes the batch afresh: once the loss measure eases, a standing cut
// shrinks to what the new verdict needs (a reduce-sized sale never goes out
// on a book that is now only just over); once it rises, the cut grows.
func TestMandateReductionStandingBatchShrinksAndGrows(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	big, _ := mandateReductionRun(t, mandateReductionFixture(t, pos, 1.6, 1e7, 2), pos)
	eased := mandateReductionNext(t, pos, 1.02, nil)
	fresh, _ := mandateReductionRun(t, eased, pos)
	eased = mandateReductionNext(t, pos, 1.02, big)
	kept, st := mandateReductionRun(t, eased, pos)
	bigSyna, _ := mandateRowFor(big, 9001)
	freshSyna, _ := mandateRowFor(fresh, 9001)
	keptSyna, ok := mandateRowFor(kept, 9001)
	if !ok || keptSyna.Mandate.Units != freshSyna.Mandate.Units || keptSyna.Mandate.Units >= bigSyna.Mandate.Units || !st.TargetReached {
		t.Fatalf("eased: standing %d, fresh %d, served %d (status %+v)", bigSyna.Mandate.Units, freshSyna.Mandate.Units, keptSyna.Mandate.Units, st)
	}
	target := 0.95 * mandateLossRoom(eased)
	if over := mandateLossAfter(t, eased, map[int]float64{9001: float64(keptSyna.Mandate.Units - 100)}); over <= target {
		t.Fatalf("the served cut is larger than the eased verdict needs: %d", keptSyna.Mandate.Units)
	}
	if keptSyna.Mandate.Episode != bigSyna.Mandate.Episode || keptSyna.Mandate.VerdictFingerprint != mandateVerdictFingerprint(eased.Result) {
		t.Fatalf("episode %q → %q, verdict %q", bigSyna.Mandate.Episode, keptSyna.Mandate.Episode, keptSyna.Mandate.VerdictFingerprint)
	}

	small, _ := mandateReductionRun(t, mandateReductionFixture(t, pos, 1.05, 1e7, 2), pos)
	risen := mandateReductionNext(t, pos, 1.2, small)
	grown, _ := mandateReductionRun(t, risen, pos)
	smallSyna, _ := mandateRowFor(small, 9001)
	grownSyna, ok := mandateRowFor(grown, 9001)
	if !ok || grownSyna.Mandate.Units <= smallSyna.Mandate.Units || slices.Equal(proposalRevisionTerms(smallSyna), proposalRevisionTerms(grownSyna)) {
		t.Fatalf("risen: standing %d, served %d", smallSyna.Mandate.Units, grownSyna.Mandate.Units)
	}
}

// A small ease of the loss measure does not re-size a standing cut: the fresh cut
// one lot smaller keeps the standing order (and so its revision); a larger
// ease takes the fresh one.
func TestMandateReductionStandingCutDampsOneLot(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	standing, _ := mandateReductionRun(t, mandateReductionFixture(t, pos, 1.12, 1e7, 2), pos)
	before, _ := mandateRowFor(standing, 9001)
	seen := false
	for ratio := 1.119; ratio > 1.0; ratio -= 0.001 {
		fresh, _ := mandateReductionRun(t, mandateReductionNext(t, pos, ratio, nil), pos)
		freshSyna, _ := mandateRowFor(fresh, 9001)
		if freshSyna.Mandate == nil || freshSyna.Mandate.Units >= before.Mandate.Units {
			continue
		}
		served, _ := mandateReductionRun(t, mandateReductionNext(t, pos, ratio, standing), pos)
		row, _ := mandateRowFor(served, 9001)
		switch before.Mandate.Units - freshSyna.Mandate.Units {
		case 100:
			seen = true
			if row.Mandate.Units != before.Mandate.Units || !slices.Equal(proposalRevisionTerms(row), proposalRevisionTerms(before)) {
				t.Fatalf("ratio %.3f: one lot smaller fresh (%d) re-sized the standing %d to %d", ratio, freshSyna.Mandate.Units, before.Mandate.Units, row.Mandate.Units)
			}
		default:
			if row.Mandate.Units != freshSyna.Mandate.Units {
				t.Fatalf("ratio %.3f: fresh %d, standing %d, served %d", ratio, freshSyna.Mandate.Units, before.Mandate.Units, row.Mandate.Units)
			}
			if !seen {
				t.Fatal("no ratio gave a fresh cut exactly one lot smaller")
			}
			return
		}
	}
	t.Fatal("the loss measure never eased enough to shrink the cut")
}

// Every row is reduce-only and never flips: a short line is bought back, by
// at most what is held, and a long line sold, by at most what is held.
func TestMandateReductionReduceOnlyNeverFlips(t *testing.T) {
	t.Parallel()
	for _, ratio := range []float64{1.1, 1.5, 4, 50} {
		pos := mandateReductionBook(mandateReductionStock("SYNC", 9003, -4000, 50), mandateReductionStock("SYNA", 9001, 800, 100), mandateReductionStock("SYNB", 9002, 300, 80))
		in := mandateReductionFixture(t, pos, ratio, 1e7, 2)
		rows, st := mandateReductionRun(t, in, pos)
		if len(rows) == 0 {
			t.Fatalf("ratio %g: no rows (%+v)", ratio, st)
		}
		assertMandateRowsReduceOnly(t, rows, pos)
		assertMandateStepsLowerRisk(t, in, rows)
		short, ok := mandateRowFor(rows, 9003)
		if !ok || short.Action != rpc.OrderActionBuy || short.Mandate.Rank != 1 {
			t.Fatalf("ratio %g: the short line (the largest loss to its stop) is not bought back first: %+v", ratio, short)
		}
	}
}

// One order is held to the order caps in force; the rest of the cut waits
// for the next cycle, which sizes the remainder from the book as it then is
// (after a verdict on it: the old verdict holds nothing it no longer sees).
func TestMandateReductionOrderCapsCarryTheRemainder(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	in := mandateReductionFixture(t, pos, 1.2, 1e7, 2)
	in.MaxOrderNotional = 20000 // 200 shares at 100
	rows, st := mandateReductionRun(t, in, pos)
	syna, ok := mandateRowFor(rows, 9001)
	if !ok || syna.Quantity != 200 || syna.Mandate.Units <= 200 || syna.PositionEffect != rpc.OrderPositionEffectReduce ||
		st.Plan[0].Units != syna.Mandate.Units || st.Plan[0].Quantity != 200 || !strings.Contains(syna.Reason, "the order caps hold one order to 200") {
		t.Fatalf("capped row %+v mandate %+v plan %+v", syna, syna.Mandate, st.Plan)
	}
	units := syna.Mandate.Units
	// The [order_limits] cap binds when it is the tighter one.
	capped := in
	capped.OrderCapBase = 15000
	if rows, _ := mandateReductionRun(t, capped, pos); rows[0].Quantity != 150 {
		t.Fatalf("order_limits cap: quantity %d, want 150", rows[0].Quantity)
	}
	// The first order fills: the old verdict's book is not the book held, so
	// the standing row (its position moved) leaves and nothing is sized.
	filled := mandateReductionBook(mandateReductionStock("SYNA", 9001, 2800, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	in.Previous = rows
	if rows, st := mandateReductionRun(t, in, filled); len(rows) != 0 || st.State != rpc.MandateReductionBookChanged {
		t.Fatalf("after the fill on the old verdict: rows %d status %+v", len(rows), st)
	}
	// The next verdict measures the book as it is: the remainder is cut.
	next := mandateReductionVerdict(mandateReductionRelegs(t, in, filled), 3)
	next.MaxOrderNotional = 20000
	next.Previous = rows
	rows, _ = mandateReductionRun(t, next, filled)
	rest, ok := mandateRowFor(rows, 9001)
	if !ok || rest.Quantity != 200 || rest.Mandate.Units != units-200 {
		t.Fatalf("remainder: %+v mandate %+v, want units %d", rest, rest.Mandate, units-200)
	}
}

// A name whose one-name gap would lose more than its cap is cut until it
// does not, even while the loss measure is within the target.
func TestMandateReductionGapBreachCutToCap(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	// The gap cap is about a tenth of the budget base at full room; this
	// base puts SYNA's 25% gap over it while the loss measure is inside target.
	in := mandateReductionFixture(t, pos, 0.5, 600000, 2)
	v := in.Result.Verdict
	if v.Status != mandate.StatusReduce || v.Stresses == nil || !slices.Contains(v.Stresses.GapBreaches, "SYNA") || mandateLoss(*v) > 0.95*mandateLossRoom(in) {
		t.Fatalf("fixture %s breaches %+v", v.Status, v.Stresses)
	}
	rows, st := mandateReductionRun(t, in, pos)
	assertMandateStepsLowerRisk(t, in, rows)
	syna, ok := mandateRowFor(rows, 9001)
	if !ok || syna.Mandate.Phase != rpc.MandatePhaseGap || syna.Mandate.GapLossBase == nil || syna.Mandate.GapCapBase == nil || !st.TargetReached {
		t.Fatalf("gap row %+v status %+v", syna.Mandate, st)
	}
	capBase := *syna.Mandate.GapCapBase
	keep := 3000 - float64(syna.Mandate.Units)
	if loss := keep * 100 * 0.25; loss > capBase+1e-6 {
		t.Fatalf("after the cut SYNA's gap loses %.0f, over the cap %.0f", loss, capBase)
	}
	if loss := (keep + 100) * 100 * 0.25; loss <= capBase {
		t.Fatalf("one lot less already meets the cap: %.0f <= %.0f", loss, capBase)
	}
}

// A broad index is cut to its cap at the index gap the verdict stressed it
// by (D4: 12%), not at the one-name gap: the reduction sells what the
// owner's stress requires and no more.
func TestMandateReductionBroadIndexCutAtTheIndexGap(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("IBCF", 9010, 6000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	fixture := mandateReductionFixture(t, pos, 0.5, 600000, 2)
	book := *fixture.Input
	book.Policy.IndexGapShockPct = 12
	in := mandateReductionVerdict(book, 2)
	v := in.Result.Verdict
	if v.Status != mandate.StatusReduce || !slices.Equal(v.Stresses.GapBreaches, []string{"IBCF"}) {
		t.Fatalf("fixture %s breaches %+v", v.Status, v.Stresses)
	}
	rows, _ := mandateReductionRun(t, in, pos)
	row, ok := mandateRowFor(rows, 9010)
	if !ok || row.Mandate.Phase != rpc.MandatePhaseGap || row.Mandate.GapCapBase == nil {
		t.Fatalf("index row %+v", row.Mandate)
	}
	capBase := *row.Mandate.GapCapBase
	keep := 6000 - float64(row.Mandate.Units)
	if loss := keep * 100 * 0.12; loss > capBase+1e-6 {
		t.Fatalf("after the cut IBCF's 12%% gap loses %.0f, over the cap %.0f", loss, capBase)
	}
	if loss := (keep + 100) * 100 * 0.12; loss <= capBase {
		t.Fatalf("the cut sold more than the index gap needs: one lot less loses %.0f <= %.0f", loss, capBase)
	}
}

// Without [mandate] in the constitution nothing changes anywhere: no rows,
// no status, no snapshot field.
func TestMandateReductionAbsentMandateChangesNothing(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100))
	in := mandateReductionFixture(t, pos, 1.5, 1e7, 2)
	in.Active = false
	if rows, st := mandateReductionRun(t, in, pos); rows != nil || st != nil {
		t.Fatalf("inactive: rows %d status %+v", len(rows), st)
	}
	srv, _ := mandateTestServer(t, testConstitutionV3())
	engine := &proposalEngine{server: srv}
	if got := engine.mandateReductionInput(t.Context(), defaultProtectionPolicy(), pos, brokerStateScope{}); got.Active || got.Result != nil {
		t.Fatalf("server without [mandate]: %+v", got)
	}
	engine.mandateInput = func(time.Time) mandateReductionInput { return mandateReductionInput{} }
	rows, _, _, _, st, _ := engine.generateBook(t.Context(), defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, mandateTestNow)
	if st != nil || slices.ContainsFunc(rows, func(p rpc.TradeProposal) bool { return p.Bucket == rpc.TradeProposalBucketMandateReduction }) {
		t.Fatalf("generateBook without [mandate]: status %+v", st)
	}
	raw, err := json.Marshal(rpc.TradeProposalSnapshot{Proposals: rows})
	if err != nil || strings.Contains(string(raw), "mandate") {
		t.Fatalf("snapshot names the mandate: %s (%v)", raw, err)
	}
}

// A kept order says why it is below the cut, in the figures of this cycle:
// under a cap that no longer binds, the kept size is named and the clause is
// never empty; under a cap that still binds but has moved, the order is the
// kept size and the clause names it, not the cap alone (quality loop,
// 2026-10-19: "sell 94 of 150 contracts now (the batch asks for 150; )" and
// "hold one order to 111" beside an order of 110).
func TestMandateCycleQuantityKeptOrderNamesItsOwnSize(t *testing.T) {
	t.Parallel()
	row := mandateReductionCall("SYNA", 9701, 150, 100, 100, 10, "20270115")
	in := mandateReductionInput{MaxOrderNotional: 1e9}
	// The cap no longer binds (a notional of 1e9 admits the whole cut): the
	// order kept from last cycle must still say so.
	c := &mandateCut{row: row, full: 150, step: 1, units: 150, prevUnits: 150, prevQty: 94}
	qty, waits := mandateCycleQuantity(c, in, "EUR")
	if qty != 94 || !strings.Contains(waits, "kept at 94") {
		t.Fatalf("kept under a cap that no longer binds: %d %q", qty, waits)
	}
	// The cap binds at 111 now (111 × 1,000 per contract) and the order is
	// kept at 110: the clause names 110.
	in.MaxOrderNotional = 111 * 1000
	c = &mandateCut{row: row, full: 150, step: 1, units: 150, prevUnits: 150, prevQty: 110}
	qty, waits = mandateCycleQuantity(c, in, "EUR")
	if qty != 110 || !strings.Contains(waits, "hold one order to 111") || !strings.Contains(waits, "kept at 110") {
		t.Fatalf("kept under a moved cap: %d %q", qty, waits)
	}
	// Nothing kept: the cap alone speaks; a whole cut says nothing.
	c = &mandateCut{row: row, full: 150, step: 1, units: 150}
	if qty, waits = mandateCycleQuantity(c, in, "EUR"); qty != 111 || !strings.Contains(waits, "hold one order to 111") || strings.Contains(waits, "kept") {
		t.Fatalf("capped: %d %q", qty, waits)
	}
	in.MaxOrderNotional = 1e9
	if qty, waits = mandateCycleQuantity(c, in, "EUR"); qty != 150 || waits != "" {
		t.Fatalf("whole cut: %d %q", qty, waits)
	}
	// The row carries the clause: never an empty parenthesis.
	engine := &proposalEngine{}
	kept := &mandateCut{id: mandateLegID(row), leg: mandate.Leg{ID: mandateLegID(row), Kind: mandate.KindOption, Quantity: 150}, row: row, full: 150, step: 1, units: 150, prevUnits: 150, prevQty: 94, rank: 1, phase: rpc.MandatePhasePremium}
	fixture := mandateReductionFixture(t, mandateReductionBook(row), 1.3, 1e9, 2)
	qty, waits = mandateCycleQuantity(kept, fixture, "EUR")
	p := engine.mandateReductionRow(defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, mandateTestNow, fixture, &rpc.TradeProposalMandateStatus{}, mandateBatch{cuts: []*mandateCut{kept}}, kept, qty, waits, "EUR")
	if strings.Contains(p.Reason, "; )") || !strings.Contains(p.Reason, "the batch asks for 150; this order is kept at 94") {
		t.Fatalf("row reason: %s", p.Reason)
	}
}
