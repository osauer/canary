package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"maps"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The owner sleeve's drawdown stop (mandate-risk-budget.md, "The forced
// reduction"; the owner's delegation of 2026-10-18 21:41 CEST, the agent's
// setting: 25% of the sleeve's own high-water mark, reduce-only to half the
// sleeve's risk), witnessed on synthetic books.

// Without owner_stop_pct nothing changes: no stop on the verdict, no state
// in the capital document, and a within verdict never counts toward the
// reduction's streak.
func TestMandateOwnerStopAbsentKeyChangesNothing(t *testing.T) {
	c := testMandateConstitution()
	srv, st := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin || res.Verdict.OwnerStop != nil || res.Policy.Sleeves.OwnerStopPct != nil {
		t.Fatalf("verdict %s stop %+v policy %+v", res.Verdict.Status, res.Verdict.OwnerStop, res.Policy.Sleeves)
	}
	if st.state.OwnerSleeve != nil {
		t.Fatalf("state written without the key: %+v", st.state.OwnerSleeve)
	}
	if kept := srv.mandateVerdicts.kept(); kept.overStreak != 0 || kept.input.OwnerStop != nil {
		t.Fatalf("streak %d input stop %+v", kept.overStreak, kept.input.OwnerStop)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil {
		t.Fatalf("an addition within budget: %+v", blocker)
	}
}

// ownerStopServer is the core test server with the owner's stop written: the
// core's fund is held, so the machine has marks of its own, and the book is
// small against the budget, so only the stop can drive the reduction.
func ownerStopServer(t *testing.T, stopPct float64, pos *rpc.PositionsResult) (*Server, *risk.Constitution) {
	t.Helper()
	c := testCoreConstitution()
	c.Mandate.Sleeves.OwnerStopPct = new(stopPct)
	srv := coreTestServer(t, c, pos, 0.25)
	return srv, c
}

// stepOwnerStop moves the clock, observes one equity reading with the
// broker's P&L explaining the move, and evaluates.
func stepOwnerStop(t *testing.T, srv *Server, c *risk.Constitution, at time.Time, equity, pnl float64) *rpc.MandateRiskResult {
	t.Helper()
	srv.now = func() time.Time { return at }
	srv.riskCapital.now = srv.now
	observeWithPnL(srv.riskCapital, c, equity, pnl, at.Add(-30*time.Second))
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status == mandate.StatusUnknown {
		t.Fatalf("verdict at %s: %+v", at, res.Verdict)
	}
	return res
}

// The sleeve's measure is the book's cash-flow-adjusted risk base less the
// machine's marked P&L, seeded from the book's own high-water mark: a loss on
// the core's fund is the machine's and leaves the owner's drawdown at zero,
// a loss elsewhere is the owner's; a new high is confirmed as the book's is,
// so one spike never raises the mark.
func TestMandateOwnerStopSeriesFollowsTheBookLessTheMachine(t *testing.T) {
	pos := withIBCF(mandateTestPositions(100), 1000, 100)
	srv, c := ownerStopServer(t, 25, pos)
	at := mandateTestNow
	res := stepOwnerStop(t, srv, c, at, 300000, 0)
	s := res.Verdict.OwnerStop
	if s == nil || !s.Known || s.Engaged || s.HWMRiskBase != 230000 || s.RiskBase != 230000 || s.DrawdownPct != 0 || s.StopPct != 25 || s.StartedAt != c.Mandate.StartedAt {
		t.Fatalf("seeded stop %+v", s)
	}
	if st := srv.riskCapital.state.OwnerSleeve; st == nil || !st.Seeded || len(st.Marks) != 1 || st.Marks[mandateConLegID(coreTestConID)].Quantity != 1000 {
		t.Fatalf("state %+v", st)
	}
	// The fund falls 10%: 10,000 of machine P&L, the whole move of the equity.
	// The book's drawdown is 10,000; the owner's sleeve has lost nothing.
	pos.Stocks[len(pos.Stocks)-1].Mark = 90
	at = at.Add(10 * time.Minute)
	res = stepOwnerStop(t, srv, c, at, 290000, -10000)
	s = res.Verdict.OwnerStop
	if math.Abs(s.MachinePnLBase+10000) > 1e-6 || math.Abs(s.RiskBase-230000) > 1e-6 || s.DrawdownPct != 0 || res.Verdict.Capital.DrawdownBase != 10000 {
		t.Fatalf("machine loss: stop %+v book drawdown %g", s, res.Verdict.Capital.DrawdownBase)
	}
	// The owner's names fall by 23,000 with the fund unchanged: all the owner's.
	at = at.Add(10 * time.Minute)
	res = stepOwnerStop(t, srv, c, at, 267000, -33000)
	s = res.Verdict.OwnerStop
	if math.Abs(s.DrawdownBase-23000) > 1e-6 || math.Abs(s.DrawdownPct-10) > 1e-9 || s.Engaged {
		t.Fatalf("owner loss: %+v", s)
	}
	// A glitch above the mark never raises it; a high confirmed five minutes
	// later does, at the lower of the two, and the drawdown is then from it.
	at = at.Add(10 * time.Minute)
	stepOwnerStop(t, srv, c, at, 400000, 100000)
	at = at.Add(time.Minute)
	if s := stepOwnerStop(t, srv, c, at, 267000, -33000).Verdict.OwnerStop; s.HWMRiskBase != 230000 {
		t.Fatalf("a spike moved the mark: %+v", s)
	}
	at = at.Add(10 * time.Minute)
	stepOwnerStop(t, srv, c, at, 312000, 12000)
	at = at.Add(6 * time.Minute)
	if s := stepOwnerStop(t, srv, c, at, 310000, 10000).Verdict.OwnerStop; math.Abs(s.HWMRiskBase-250000) > 1e-6 || s.DrawdownBase != 0 {
		t.Fatalf("a confirmed high: %+v", s)
	}
	// The state is persisted inside the capital document and round-trips.
	raw, err := json.Marshal(srv.riskCapital.state)
	if err != nil || !strings.Contains(string(raw), `"mandate_owner_sleeve"`) {
		t.Fatalf("document %v: %s", err, raw)
	}
}

// stepOwnerStopAt moves the clock and reads the equity that puts the owner
// sleeve's drawdown at pct of its 230,000 risk base (equity 300,000 less the
// 70,000 reserve) with the machine's marks unchanged, the broker's P&L
// explaining the move.
func stepOwnerStopAt(t *testing.T, srv *Server, c *risk.Constitution, at time.Time, pct float64) *rpc.MandateRiskResult {
	t.Helper()
	return stepOwnerStop(t, srv, c, at, 300000-pct*2300, -pct*2300)
}

// At the stop the sleeve engages: the gate refuses the owner's additions,
// the verdict pins half the owner's use as the target, two scheduled
// readings build the streak while the book itself is within the budget, a
// drawdown back under the stop but inside the release band holds the stop
// and its pin, and one under the release lets go, the gate open again. A
// stale capital state holds the stop as it was.
func TestMandateOwnerStopEngagesHoldsAndReleases(t *testing.T) {
	pos := withIBCF(mandateTestPositions(100), 100, 100)
	srv, c := ownerStopServer(t, 10, pos)
	at := mandateTestNow
	stepOwnerStop(t, srv, c, at, 300000, 0)
	// A 12% fall of the risk base (230,000): past the 10% stop, with the
	// room at 60% the book is still far inside the budget.
	at = at.Add(10 * time.Minute)
	res := stepOwnerStop(t, srv, c, at, 272400, -27600)
	v := res.Verdict
	s := v.OwnerStop
	owner, _ := v.Allocation(mandate.SleeveOwner)
	if v.Status != mandate.StatusWithin || s == nil || !s.Engaged || !s.Over || math.Abs(s.DrawdownPct-12) > 1e-9 || s.EngagedAt != at || s.ReleasePct != 5 ||
		math.Abs(s.UseAtStopBase-owner.UseBase) > 1e-9 || math.Abs(s.TargetUseBase-owner.UseBase/2) > 1e-9 || s.UseBase != owner.UseBase {
		t.Fatalf("engaged: %s %+v owner %+v", v.Status, s, owner)
	}
	if !res.Gate.RefusesAdditions || !strings.Contains(res.Gate.Reason, "past its 10% stop") {
		t.Fatalf("gate view %+v", res.Gate)
	}
	blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen)
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "reduce-only until its drawdown is back under 5%, 5 points under the stop") || !strings.Contains(blocker.Action, "back under 5%") {
		t.Fatalf("the owner adds nothing while stopped: %+v", blocker)
	}
	coreBuy := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 10, LimitPrice: 100, Contract: rpc.ContractParams{ConID: coreTestConID, Symbol: "IBCF", SecType: "STK", Currency: "EUR"}}
	if blocker := srv.mandateBudgetGate(coreBuy, mandateOpen); blocker != nil {
		t.Fatalf("the core is not the owner's sleeve: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), rpc.OrderPositionImpact{Before: 100, After: 90, Effect: rpc.OrderPositionEffectReduce}); blocker != nil {
		t.Fatalf("a reduction passes: %+v", blocker)
	}
	// The streak: the within verdict with the stop over counts as a breach
	// reading on the scheduled cadence.
	if kept := srv.mandateVerdicts.kept(); kept.overStreak != 1 {
		t.Fatalf("streak after the engaging reading: %d", kept.overStreak)
	}
	at = at.Add(6 * time.Minute)
	stepOwnerStop(t, srv, c, at, 272400, -27600)
	if kept := srv.mandateVerdicts.kept(); kept.overStreak != 2 || kept.input.OwnerStop == nil || !kept.input.OwnerStop.Engaged {
		t.Fatalf("streak %d, kept input stop %+v", kept.overStreak, kept.input.OwnerStop)
	}
	// The target is pinned for the engagement: a deeper fall does not move it.
	at = at.Add(6 * time.Minute)
	if s := stepOwnerStop(t, srv, c, at, 260000, -40000).Verdict.OwnerStop; s.TargetUseBase != v.OwnerStop.TargetUseBase || !s.Engaged {
		t.Fatalf("target moved: %+v", s)
	}
	// Stale equity: the capital is unknown, the verdict unknown, the stop
	// held as last persisted.
	stale := at.Add(time.Duration(*c.Capital.MaxEquityAgeMinutes+5) * time.Minute)
	srv.now = func() time.Time { return stale }
	srv.riskCapital.now = srv.now
	held := srv.evaluateMandate(t.Context(), false).Verdict
	if held.Status != mandate.StatusUnknown || held.OwnerStop == nil || held.OwnerStop.Known || !held.OwnerStop.Engaged || held.OwnerStop.Over || held.OwnerStop.TargetUseBase != v.OwnerStop.TargetUseBase {
		t.Fatalf("stale: %s %+v", held.Status, held.OwnerStop)
	}
	if kept := srv.mandateVerdicts.kept(); kept.overStreak != 0 {
		t.Fatalf("an unknown reading resets the streak: %d", kept.overStreak)
	}
	// Recovered to 8% below the mark, under the stop but inside the release
	// band: held, the pin kept, the sleeve still reduce-only and the gate
	// naming the release.
	at = stale.Add(time.Minute)
	res = stepOwnerStop(t, srv, c, at, 281600, -18400)
	if s := res.Verdict.OwnerStop; !s.Known || !s.Engaged || !s.Over || s.TargetUseBase != v.OwnerStop.TargetUseBase || s.EngagedAt != v.OwnerStop.EngagedAt || math.Abs(s.DrawdownPct-8) > 1e-9 {
		t.Fatalf("held: %+v", s)
	}
	if !res.Gate.RefusesAdditions || !strings.Contains(res.Gate.Reason, "under its 10% stop but not yet under the 5% that releases it; the sleeve is reduce-only until its drawdown is back under 5%") {
		t.Fatalf("gate view held %+v", res.Gate)
	}
	// Recovered to 4%, under the release: released, the pin cleared, the
	// gate open.
	at = at.Add(6 * time.Minute)
	res = stepOwnerStopAt(t, srv, c, at, 4)
	if s := res.Verdict.OwnerStop; !s.Known || s.Engaged || s.Over || s.TargetUseBase != 0 || s.UseAtStopBase != 0 || math.Abs(s.DrawdownPct-4) > 1e-9 {
		t.Fatalf("released: %+v", s)
	}
	if res.Gate.RefusesAdditions {
		t.Fatalf("gate after the release %+v", res.Gate)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil {
		t.Fatalf("an addition after the release: %+v", blocker)
	}
}

// The stop releases with hysteresis, so it never ratchets: with the sleeve
// cut to its target, a drawdown oscillating around the stop on the
// schedule's five-minute readings holds the first engagement's pin (the
// stop stays engaged, the target never moves, nothing is over) instead of
// releasing and re-engaging at half of the halved use. Only a reading under
// the release clears the pin, and the next fall past the stop is a new
// engagement that pins half the use of its own verdict. The journal has
// engage, one hold and release with the pinned figures.
func TestMandateOwnerStopOscillationCutsOnceAndAFullReleasePinsAnew(t *testing.T) {
	pos := withIBCF(mandateTestPositions(100), 100, 100)
	srv, c := ownerStopServer(t, 10, pos)
	at := mandateTestNow
	stepOwnerStop(t, srv, c, at, 300000, 0)
	at = at.Add(10 * time.Minute)
	first := stepOwnerStopAt(t, srv, c, at, 12).Verdict.OwnerStop
	if !first.Engaged || !first.Over || first.ReleasePct != 5 {
		t.Fatalf("engaged: %+v", first)
	}
	// The reduction has run: the owner's names at a third of their size, the
	// short call gone. The sleeve's use is under its target.
	pos.Stocks[0].Quantity, pos.Stocks[1].Quantity, pos.Options = 30, 30, nil
	for i, pct := range []float64{9, 11, 9.5, 10.5, 9} {
		at = at.Add(5 * time.Minute)
		res := stepOwnerStopAt(t, srv, c, at, pct)
		s := res.Verdict.OwnerStop
		if !s.Engaged || s.Over || s.EngagedAt != first.EngagedAt || s.TargetUseBase != first.TargetUseBase || s.UseAtStopBase != first.UseAtStopBase || s.UseBase > s.TargetUseBase {
			t.Fatalf("reading %d at %g%%: the pin moved: %+v (first %+v)", i, pct, s, first)
		}
		if !res.Gate.RefusesAdditions {
			t.Fatalf("reading %d at %g%%: additions open while the stop holds: %+v", i, pct, res.Gate)
		}
		if kept := srv.mandateVerdicts.kept(); kept.overStreak != 0 {
			t.Fatalf("reading %d at %g%%: a sleeve at its target builds a streak: %d", i, pct, kept.overStreak)
		}
	}
	// Under the release: the pin is cleared.
	at = at.Add(5 * time.Minute)
	if s := stepOwnerStopAt(t, srv, c, at, 4).Verdict.OwnerStop; s.Engaged || s.TargetUseBase != 0 || s.UseAtStopBase != 0 || !s.EngagedAt.IsZero() {
		t.Fatalf("released: %+v", s)
	}
	// A new fall past the stop pins half the use now, the use after the cut.
	at = at.Add(5 * time.Minute)
	v := stepOwnerStopAt(t, srv, c, at, 12).Verdict
	owner, _ := v.Allocation(mandate.SleeveOwner)
	if s := v.OwnerStop; !s.Engaged || !s.Over || s.EngagedAt != at || math.Abs(s.UseAtStopBase-owner.UseBase) > 1e-9 || math.Abs(s.TargetUseBase-owner.UseBase/2) > 1e-9 || s.TargetUseBase >= first.TargetUseBase {
		t.Fatalf("re-engaged: %+v owner %+v first %+v", s, owner, first)
	}
	kinds, released := ownerStopJournal(t)
	want := []string{"mandate_owner_stop_started", "mandate_owner_stop_engaged", "mandate_owner_stop_held", "mandate_owner_stop_released", "mandate_owner_stop_engaged"}
	if strings.Join(kinds, " ") != strings.Join(want, " ") {
		t.Fatalf("journal %v, want %v", kinds, want)
	}
	if released["target_use_base"] != first.TargetUseBase || released["use_at_stop_base"] != first.UseAtStopBase || released["release_pct"] != 5.0 || released["held"] != true {
		t.Fatalf("the release entry lacks the pinned figures: %v", released)
	}
}

// ownerStopJournal reads the owner stop's entries off the risk policy
// journal: their kinds in order and the last release entry.
func ownerStopJournal(t *testing.T) ([]string, map[string]any) {
	t.Helper()
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var kinds []string
	var released map[string]any
	for sc := bufio.NewScanner(f); sc.Scan(); {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			t.Fatal(err)
		}
		kind, _ := entry["kind"].(string)
		if !strings.HasPrefix(kind, "mandate_owner_stop_") {
			continue
		}
		kinds = append(kinds, kind)
		if kind == "mandate_owner_stop_released" {
			released = entry
		}
	}
	return kinds, released
}

// ownerStopFixture is the allocations fixture with the book inside the budget
// and the owner's stop engaged at half the owner's use.
func ownerStopFixture(t *testing.T, pos *rpc.PositionsResult, ratio float64, coreConIDs ...int) mandateReductionInput {
	t.Helper()
	in := mandateAllocationedFixture(t, pos, ratio, 0.3, coreConIDs...)
	book := *in.Input
	owner, _ := in.Result.Verdict.Allocation(mandate.SleeveOwner)
	book.OwnerStop = &mandate.SleeveStop{Sleeve: mandate.SleeveOwner, StopPct: 25, ReleasePct: 20, Known: true, StartedAt: "2026-10-01", RiskBase: 170000, HWMRiskBase: 230000, DrawdownBase: 60000, DrawdownPct: 26.1,
		Engaged: true, EngagedAt: book.Now.Add(-7 * time.Minute), UseAtStopBase: owner.UseBase, TargetUseBase: mandate.SleeveStopHalf * owner.UseBase}
	return mandateReductionVerdict(book, 2)
}

// With the book inside the budget and the stop engaged, the batch cuts the
// owner's sleeve alone to half its risk at the stop, nothing of the core's
// fund, every row reduce-only and lowering the risk; the same book without
// the stop proposes nothing.
func TestMandateOwnerStopCutsHalfTheOwnersRiskAndNothingOfTheMachine(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SYNA", 8601, 900, 100), mandateReductionStock("SYNB", 8602, 400, 100), mandateReductionStock("IBCF", 8603, 1500, 100))
	plain := mandateAllocationedFixture(t, pos, 0.8, 0.3, 8603)
	if rows, st := mandateReductionRun(t, plain, pos); len(rows) != 0 || st.State != rpc.MandateReductionWithin {
		t.Fatalf("fixture: without the stop the book within proposes %d rows, %s", len(rows), st.State)
	}
	in := ownerStopFixture(t, pos, 0.8, 8603)
	v := in.Result.Verdict
	if v.Status != mandate.StatusWithin || v.OwnerStop == nil || !v.OwnerStop.Over {
		t.Fatalf("fixture: %s %+v", v.Status, v.OwnerStop)
	}
	rows, st := mandateReductionRun(t, in, pos)
	if st.State != rpc.MandateReductionReducing || len(rows) == 0 || !st.TargetReached || st.Episode == "" {
		t.Fatalf("state %s reached %v: %s", st.State, st.TargetReached, st.Reason)
	}
	assertMandateRowsReduceOnly(t, rows, pos)
	cuts := map[int]float64{}
	for _, row := range rows {
		if row.Contract.ConID == 8603 || row.Mandate.Sleeve != mandate.SleeveOwner || row.Mandate.Phase != rpc.MandatePhaseStop {
			t.Fatalf("a row outside the owner's sleeve or phase: %+v", row.Mandate)
		}
		if !strings.Contains(row.Reason, "past its 25% stop") || !strings.Contains(row.Reason, "half the sleeve's risk at the stop") {
			t.Fatalf("row reason: %s", row.Reason)
		}
		cuts[row.Contract.ConID] = float64(row.Mandate.Units)
	}
	if !strings.Contains(st.Reason, "the loss measure is within the mandate's maximum loss, but your sleeve is 26% below its high-water mark") || !strings.Contains(st.Reason, "the sleeve's risk to its stop target") {
		t.Fatalf("status reason: %s", st.Reason)
	}
	// After the whole batch the owner's use is at or under half, by the
	// smallest lots: one lot less on any line leaves it over.
	after := mandateOwnerUseAfter(t, in, cuts)
	if after > v.OwnerStop.TargetUseBase+1e-6 {
		t.Fatalf("owner use after the batch %.0f, target %.0f", after, v.OwnerStop.TargetUseBase)
	}
	for conID, units := range cuts {
		less := maps.Clone(cuts)
		lot := float64(mandateShareLot(int(math.Abs(mandatePositionRows(pos)[mandateLegID(rpc.PositionView{ConID: conID})].Quantity))))
		less[conID] = units - lot
		if mandateOwnerUseAfter(t, in, less) <= v.OwnerStop.TargetUseBase+1e-6 {
			t.Fatalf("one lot less on %d still reaches the target: the cut is not the smallest", conID)
		}
	}
	// Every row alone lowers the book's risk.
	before := mandateRiskAfter(t, in, nil)
	for _, row := range rows {
		if mandateRiskAfter(t, in, map[int]float64{row.Contract.ConID: float64(row.Quantity)}) >= before {
			t.Fatalf("row on %d does not lower the risk", row.Contract.ConID)
		}
	}
}

// mandateOwnerUseAfter is the owner sleeve's use with the cuts applied.
func mandateOwnerUseAfter(t *testing.T, in mandateReductionInput, cuts map[int]float64) float64 {
	t.Helper()
	book := *in.Input
	book.Legs = append([]mandate.Leg(nil), book.Legs...)
	for i := range book.Legs {
		for conID, units := range cuts {
			if book.Legs[i].ID == mandateLegID(rpc.PositionView{ConID: conID}) {
				book.Legs[i].Quantity -= units
			}
		}
	}
	v := mandate.Evaluate(book)
	if v.Risk == nil {
		t.Fatalf("unmeasured after cuts: %v", v.Reasons)
	}
	return mandateOwnerUse(&v)
}

// Over the loss room and stopped at once, the batch takes the loss measure
// to its target by the largest premium at risk first and then carries the
// owner's sleeve on to the stop's target by its volatility.
func TestMandateOwnerStopInsideABudgetBreach(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SYNA", 8701, 900, 100), mandateReductionStock("SYNB", 8702, 400, 100), mandateReductionStock("IBCF", 8703, 1500, 100))
	in := ownerStopFixture(t, pos, 1.3, 8703)
	v := in.Result.Verdict
	if v.Status == mandate.StatusWithin || !v.OwnerStop.Over {
		t.Fatalf("fixture: %s %+v", v.Status, v.OwnerStop)
	}
	rows, st := mandateReductionRun(t, in, pos)
	if st.State != rpc.MandateReductionReducing || !st.TargetReached {
		t.Fatalf("state %s reached %v: %s", st.State, st.TargetReached, st.Reason)
	}
	assertMandateRowsReduceOnly(t, rows, pos)
	cuts := map[int]float64{}
	for _, row := range rows {
		cuts[row.Contract.ConID] = float64(row.Mandate.Units)
	}
	if after := mandateOwnerUseAfter(t, in, cuts); after > v.OwnerStop.TargetUseBase+1e-6 {
		t.Fatalf("owner use after the batch %.0f, target %.0f", after, v.OwnerStop.TargetUseBase)
	}
	if loss := mandateLossAfter(t, in, cuts); loss > mandateReductionTargetFactor*mandateLossRoom(in)+1e-6 {
		t.Fatalf("loss after the batch %.0f, room %.0f", loss, mandateLossRoom(in))
	}
}

// Released, or cut to its target, the stop no longer sizes: a within verdict
// with the stop engaged at its target keeps the episode open and withdraws
// the rows; one with the stop released ends the episode.
func TestMandateOwnerStopReleaseWithdrawsAndEndsTheEpisode(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SYNA", 8801, 900, 100), mandateReductionStock("SYNB", 8802, 400, 100), mandateReductionStock("IBCF", 8803, 1500, 100))
	first := ownerStopFixture(t, pos, 0.8, 8803)
	rows, st := mandateReductionRun(t, first, pos)
	if len(rows) == 0 || st.Episode == "" {
		t.Fatalf("fixture: %s", st.Reason)
	}
	next := func(edit func(*mandate.SleeveStop)) mandateReductionInput {
		book := *first.Input
		book.Now = book.Now.Add(2 * time.Minute)
		stop := *book.OwnerStop
		edit(&stop)
		book.OwnerStop = &stop
		in := mandateReductionVerdict(book, 0)
		in.Previous, in.Episode = rows, st.Episode
		return in
	}
	// At the target (the owner reduced by hand): the episode stays, the rows go.
	atTarget := next(func(s *mandate.SleeveStop) { s.TargetUseBase = 1e12 })
	if out, got := mandateReductionRun(t, atTarget, pos); len(out) != 0 || got.State != rpc.MandateReductionWithin || got.Withdrawn != len(rows) || got.Episode != st.Episode ||
		!strings.Contains(got.Reason, "stays reduce-only until its drawdown is back under 20%, 5 points under the stop") {
		t.Fatalf("at the target: %d rows, %s episode %q: %s", len(out), got.State, got.Episode, got.Reason)
	}
	// Released: the episode ends.
	released := next(func(s *mandate.SleeveStop) { s.Engaged, s.TargetUseBase, s.DrawdownPct = false, 0, 20 })
	if out, got := mandateReductionRun(t, released, pos); len(out) != 0 || got.State != rpc.MandateReductionWithin || got.Withdrawn != len(rows) || got.Episode != "" {
		t.Fatalf("released: %d rows, %s episode %q", len(out), got.State, got.Episode)
	}
	// An unknown verdict holds the standing rows with their sizes.
	unknown := next(func(s *mandate.SleeveStop) { s.Known = false })
	unknown.Input.Capital = mandate.Capital{Reasons: []string{"equity observation is older than capital.max_equity_age_minutes"}}
	unknown.Result.Verdict = new(mandate.Evaluate(*unknown.Input))
	if out, got := mandateReductionRun(t, unknown, pos); got.State != rpc.MandateReductionHeld || len(out) != len(rows) || !out[0].Mandate.Held {
		t.Fatalf("unknown: %s %d rows", got.State, len(out))
	}
}

// The key is optional and validated: absent it stays out of the fingerprint
// projection; written it is adopted, shown and refused at or above the
// maximum loss.
func TestMandateOwnerStopKey(t *testing.T) {
	withTable := strings.Replace(schema2ConstitutionTOML(), "policy_version = 5", "policy_version = 6", 1) + mandateTableTOML
	m, path := newTestRiskPolicyManager(t, withTable)
	without := m.snapshot().policy
	if raw, _ := json.Marshal(without.Mandate.Sleeves); strings.Contains(string(raw), "owner_stop_pct") {
		t.Fatalf("an absent key enters the fingerprint projection: %s", raw)
	}
	if _, ok := without.Mandate.OwnerStopPct(); ok {
		t.Fatal("absent key read as written")
	}
	written := strings.Replace(withTable, "policy_version = 6", "policy_version = 7", 1)
	written = strings.Replace(written, "overlay_vol_pct = 4.0\n", "overlay_vol_pct = 4.0\nowner_stop_pct = 25.0\n", 1)
	writePolicyTestFile(t, path, written)
	m.reload()
	got := m.snapshot()
	if stop, ok := got.policy.Mandate.OwnerStopPct(); got.status != rpc.RiskPolicyStatusActive || !ok || stop != 25 {
		t.Fatalf("written key: %s %q %+v", got.status, got.message, got.policy.Mandate.Sleeves)
	}
	if got.policy.FingerprintKey() == without.FingerprintKey() {
		t.Fatal("writing owner_stop_pct left the fingerprint unchanged")
	}
	if v := mandatePolicyView(got.policy.Mandate); v.Sleeves == nil || v.Sleeves.OwnerStopPct == nil || *v.Sleeves.OwnerStopPct != 25 {
		t.Fatalf("view %+v", v.Sleeves)
	}
	good := got.policy
	writePolicyTestFile(t, path, strings.Replace(strings.Replace(written, "policy_version = 7", "policy_version = 8", 1), "owner_stop_pct = 25.0", "owner_stop_pct = 30.0", 1))
	m.reload()
	if got := m.snapshot(); got.status != rpc.RiskPolicyStatusError || got.policy != good || !strings.Contains(got.message, "owner_stop_pct") || !strings.Contains(got.message, "backstop") {
		t.Fatalf("a stop at the maximum loss: %s %q", got.status, got.message)
	}
	if !strings.Contains(string(ConstitutionPolicyTemplate("test")), "# owner_stop_pct = 25.0") {
		t.Fatal("the template lacks owner_stop_pct")
	}
}
