package daemon

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Allocations (internal-docs/design/mandate-risk-budget.md, "Risk allocations"; owner
// decision 2026-10-18 08:06 CEST), witnessed on synthetic books.

// The machine's allocation is only as big as what it runs: a core in shadow
// reserves nothing, and the owner has the whole budget; a live core, whose
// rows this build serves, reserves its scale. The core's contract is placed
// with the core either way; a book without [mandate.core] places nothing
// (strategy-register.md, section 2).
func TestMandateAllocationPlanGivesTheMachineWhatItRuns(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.Core.Stage = risk.StageShadow
	srv := &Server{}
	plan := mandateAllocationPlan(c, srv.resolvedRegister(c), []int{coreTestConID}, nil)
	if len(plan.Machine) != 2 || plan.Machine[0].Sleeve != mandate.SleeveCore || plan.Machine[0].TargetPct != 6 || plan.Machine[0].StagePct != 0 ||
		!strings.Contains(plan.Machine[0].Note, "sends no orders") {
		t.Fatalf("core allocation in shadow: %+v", plan.Machine)
	}
	if o := plan.Machine[1]; o.Sleeve != mandate.SleeveOverlay || o.TargetPct != 4 || o.StagePct != 0 || !strings.Contains(o.Note, "no strategy registered") {
		t.Fatalf("overlay allocation: %+v", o)
	}
	if plan.SleeveOf("con:9030") != mandate.SleeveCore || plan.SleeveOf("con:9001") != mandate.SleeveOwner {
		t.Fatalf("placement %+v", plan.Placed)
	}
	c.Mandate.Core.Stage = risk.StageLive
	if running := mandateAllocationPlan(c, srv.resolvedRegister(c), []int{coreTestConID}, nil); running.Machine[0].StagePct != 100 {
		t.Fatalf("a live core reserves its scale: %+v", running.Machine[0])
	}
	bare := testMandateConstitution()
	if plan := mandateAllocationPlan(bare, srv.resolvedRegister(bare), []int{coreTestConID}, nil); len(plan.Placed) != 0 || plan.Machine[0].StagePct != 0 || !strings.Contains(plan.Machine[0].Note, "inactive") {
		t.Fatalf("no core table: %+v", plan)
	}
	if rule, _ := register.Find(srv.registerRules(), core.RuleID); !rule.Sends {
		t.Fatal("this build serves the core's rows, so a live core runs")
	}
}

// The register decides the reservation (strategy-register.md, section 2):
// the core's first form, without a stage, runs in shadow and reserves
// nothing even where its rule sends; shadow and suspended reserve nothing; a
// live core on hold keeps its reservation only while it holds positions; a
// later entry reserves its own allocation at its scale.
func TestMandateAllocationPlanFollowsTheRegister(t *testing.T) {
	sends := &Server{}
	held := []mandate.Leg{{ID: mandateConLegID(coreTestConID), Quantity: 10}}
	plan := func(c *risk.Constitution, legs []mandate.Leg) mandate.MachineAllocation {
		return mandateAllocationPlan(c, sends.resolvedRegister(c), []int{coreTestConID}, legs).Machine[0]
	}
	with := func(edit func(*risk.ConstitutionMandateCore)) *risk.Constitution {
		c := testCoreConstitution()
		edit(c.Mandate.Core)
		return c
	}
	for name, tc := range map[string]struct {
		c     *risk.Constitution
		legs  []mandate.Leg
		stage float64
		note  string
	}{
		"first form, no stage":         {with(func(k *risk.ConstitutionMandateCore) { k.Stage, k.SpecHash = "", "" }), held, 0, "stage unwritten"},
		"shadow":                       {with(func(k *risk.ConstitutionMandateCore) { k.Stage = risk.StageShadow }), held, 0, "runs in shadow"},
		"suspended":                    {with(func(k *risk.ConstitutionMandateCore) { k.Stage = risk.StageSuspended }), held, 0, "suspended"},
		"live":                         {testCoreConstitution(), nil, 100, "is live"},
		"live, hash mismatch, holding": {with(func(k *risk.ConstitutionMandateCore) { k.SpecHash = strings.Repeat("0", 64) }), held, 100, "keeps its scale"},
		"live, hash mismatch, flat":    {with(func(k *risk.ConstitutionMandateCore) { k.SpecHash = strings.Repeat("0", 64) }), nil, 0, "holds:"},
		"live, no spec_hash, holding":  {with(func(k *risk.ConstitutionMandateCore) { k.SpecHash = "" }), held, 100, "mandate.core.spec_hash unwritten"},
	} {
		t.Run(name, func(t *testing.T) {
			got := plan(tc.c, tc.legs)
			if got.StagePct != tc.stage || !strings.Contains(got.Note, tc.note) {
				t.Fatalf("core line %+v, want stage %g and a note with %q", got, tc.stage, tc.note)
			}
		})
	}
	// A later entry is its own sleeve with its own allocation.
	c := testCoreConstitution()
	c.Mandate.Strategies = map[string]*risk.ConstitutionMandateStrategy{"s9": {Rule: "not-compiled-v1", SpecHash: strings.Repeat("ab", 32), Sleeve: "s9", RiskAllocationPct: new(2.0), Stage: risk.StageShadow, ScalePct: new(25.0)}}
	p := mandateAllocationPlan(c, sends.resolvedRegister(c), []int{coreTestConID}, nil)
	if len(p.Machine) != 3 || p.Machine[2].Sleeve != "s9" || p.Machine[2].TargetPct != 2 || p.Machine[2].StagePct != 0 || !strings.Contains(p.Machine[2].Note, "is not compiled") {
		t.Fatalf("a later entry: %+v", p.Machine)
	}
}

// With the core running at its full scale (6 of the 15 points), the owner's
// additions stop at the owner's allocation, the rest of the budget, even while the
// whole book has room; inside the allocation they pass. With the core running
// nothing, the same order passes: the owner has the whole budget, as today.
func TestMandateGateHoldsTheOwnerToTheOwnersAllocation(t *testing.T) {
	c := testCoreConstitution()
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	owner, _ := res.Verdict.Allocation(mandate.SleeveOwner)
	coreAllocation, _ := res.Verdict.Allocation(mandate.SleeveCore)
	if budget := res.Verdict.Capital.BudgetAnnualSD; math.Abs(coreAllocation.AllocationBase-0.4*budget) > 1e-6 || math.Abs(owner.AllocationBase-0.6*budget) > 1e-6 || owner.Over {
		t.Fatalf("allocations owner %+v core %+v budget %g", owner, coreAllocation, budget)
	}
	// The first size that takes the owner over its allocation still fits the
	// whole budget, so only the allocation refuses it.
	_, kept, _, _ := srv.mandateVerdicts.current()
	over := 0
	for q := 10; q <= 5000 && over == 0; q += 10 {
		book, _ := srv.mandateOrderBook(mandateBuy(q), kept, mandateTestNow)
		leg, _ := mandateOrderLeg(mandateBuy(q), &book)
		book.Allocations = book.Allocations.WithPlaced(leg.ID, mandate.SleeveOwner)
		with := mandate.Evaluate(mandate.WithLeg(book, leg))
		if s, _ := with.Allocation(mandate.SleeveOwner); s.Over {
			if with.Risk.SDAnnualBase > with.Capital.BudgetAnnualSD {
				t.Fatalf("fixture: the book is over budget before the owner is over its allocation at %d", q)
			}
			over = q
		}
	}
	if over == 0 {
		t.Fatal("fixture: no size takes the owner over its allocation")
	}
	blocker := srv.mandateBudgetGate(mandateBuy(over), mandateOpen)
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "your sleeve would carry") || !strings.Contains(blocker.Message, "less the machine's allocations") {
		t.Fatalf("owner over its allocation: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(over-10), mandateOpen); blocker != nil {
		t.Fatalf("inside the owner's allocation: %+v", blocker)
	}
	// The core running nothing (in shadow) gives the owner the whole budget back.
	c.Mandate.Core.Stage = risk.StageShadow
	srv.evaluateMandate(t.Context(), false)
	if blocker := srv.mandateBudgetGate(mandateBuy(over), mandateOpen); blocker != nil {
		t.Fatalf("with no machine allocation the owner has the whole budget: %+v", blocker)
	}
}

// Every unit of the core's pinned contract is the core's, whoever buys it:
// while the core runs nothing its allocation is zero, so a hand buy of the fund is
// refused, by contract id or by its symbol without one; a sale passes. With
// the core running, a buy inside its allocation passes and one past it is refused.
func TestMandateGatePlacesTheCoreFundWithTheCore(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.Core.Stage = risk.StageShadow
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	buy := func(q int, conID int) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: q, LimitPrice: 100, Contract: rpc.ContractParams{ConID: conID, Symbol: core.Symbol, SecType: "STK", Currency: core.Currency, PrimaryExch: "IBIS"}}
	}
	blocker := srv.mandateBudgetGate(buy(300, coreTestConID), mandateOpen)
	if blocker == nil || !strings.Contains(blocker.Message, "the core's sleeve would carry") || !strings.Contains(blocker.Message, "sends no orders") ||
		!strings.Contains(blocker.Message, "every unit of the core's fund is the core's") {
		t.Fatalf("a hand buy of the core's fund while the core runs nothing: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(buy(10, coreTestConID), rpc.OrderPositionImpact{Before: 10, After: 0, Effect: rpc.OrderPositionEffectClose}); blocker != nil {
		t.Fatalf("a sale of the core's fund: %+v", blocker)
	}
	if mandateOrderSleeve(srv.mandateCoreContracts(t.Context(), c), buy(10, 4242)) != mandate.SleeveOwner {
		t.Fatal("another listing under another contract id is the owner's")
	}

	c.Mandate.Core.Stage = risk.StageLive
	res := srv.evaluateMandate(t.Context(), false)
	allocation, _ := res.Verdict.Allocation(mandate.SleeveCore)
	if allocation.AllocationBase <= 0 {
		t.Fatalf("a running core's allocation: %+v", allocation)
	}
	if blocker := srv.mandateBudgetGate(buy(300, coreTestConID), mandateOpen); blocker != nil {
		t.Fatalf("a buy inside the core's allocation: %+v", blocker)
	}
	// Far more than the allocation holds: the allocation refuses before the budget (the
	// core's allocation is 40% of it here, and the owner holds the rest).
	big := int(math.Ceil(2 * allocation.AllocationBase / (0.16 * 100)))
	blocker = srv.mandateBudgetGate(buy(big, coreTestConID), mandateOpen)
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "the core's sleeve would carry") && !strings.Contains(blocker.Message, "over the budget") {
		t.Fatalf("a buy past the core's allocation: %+v", blocker)
	}
}

// An option buy the evaluator cannot price was once bounded by its sleeve's
// allocation; since the owner's decision of 2026-10-23 18:22 CEST it is
// judged by the premium limits, and the allocation bound is its warning:
// the owner's allocation, smaller than the budget once the core runs, is
// named on the preview, and the per-name premium cap refuses the buy.
func TestMandateGateBoundsAnUnpricedOptionBuyByItsAllocation(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.Core.Stage = risk.StageLive
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	option := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 12, LimitPrice: 40, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Currency: "USD", Right: "C", Strike: 50, Expiry: "20270115", Multiplier: 100}}
	res := srv.evaluateMandate(t.Context(), false)
	owner, _ := res.Verdict.Allocation(mandate.SleeveOwner)
	_, kept, _, _ := srv.mandateVerdicts.current()
	b, why := mandateLongOptionBound(option, *kept, res.Verdict)
	if why != "" {
		t.Fatal(why)
	}
	if worst := owner.StandaloneBase + b.riskAnnual - res.Verdict.Risk.SDAnnualBase; worst <= owner.AllocationBase {
		t.Fatalf("fixture: the allocation does not bind (%.0f of %.0f); the test proves nothing", worst, owner.AllocationBase)
	}
	// Twelve contracts at 40 USD: 43,200 EUR of premium, 14.4% of the
	// 300,000 EUR net liquidation, inside the 15% per-name limit: the buy
	// passes, and the allocation bound is its warning.
	blocker, warning := srv.mandateBudgetJudgement(option, mandateOpen)
	if blocker != nil || warning == nil || !strings.Contains(warning.Message, "could") || !strings.Contains(warning.Message, "premium limits") {
		t.Fatalf("twelve contracts: blocker %+v warning %+v", blocker, warning)
	}
	// Thirteen: 46,800 EUR, 15.6%, over the limit for one name.
	option.Quantity = 13
	blocker, warning = srv.mandateBudgetJudgement(option, mandateOpen)
	if blocker == nil || blocker.Code != rpc.MandatePremiumBlockerCode || warning != nil || !strings.Contains(blocker.Message, "premium at risk in SYNA would be 46,800 EUR, 15.6% of net liquidation 300,000 EUR, over the 15.0% limit for one name") {
		t.Fatalf("thirteen contracts: blocker %+v warning %+v", blocker, warning)
	}
}

// mandateAllocationedFixture measures the reduction fixture's book with an allocation plan:
// the core's lines placed with the core, its allocation coreAllocation of the budget.
func mandateAllocationedFixture(t *testing.T, pos *rpc.PositionsResult, ratio float64, coreAllocation float64, coreConIDs ...int) mandateReductionInput {
	t.Helper()
	in := mandateReductionFixture(t, pos, ratio, 1e9, 2)
	book := *in.Input
	placed := map[string]string{}
	for _, id := range coreConIDs {
		placed[mandateLegID(rpc.PositionView{ConID: id})] = mandate.SleeveCore
	}
	target := coreAllocation * book.Capital.BudgetAnnualSD / book.Capital.BudgetBase * 100
	book.Allocations = &mandate.AllocationPlan{Machine: []mandate.MachineAllocation{{Sleeve: mandate.SleeveCore, TargetPct: target, StagePct: 100}, {Sleeve: mandate.SleeveOverlay, TargetPct: 4}}, Placed: placed}
	return mandateReductionVerdict(book, 2)
}

// The reduction is ordered by premium at risk per name, whatever the sleeve
// (spec TM-04.12, owner decision 2026-10-27 19:50 CET; until then the sleeve
// furthest over its allocation was cut first, owner 2026-10-18 08:06 CEST):
// here the core's fund holds the largest loss to its stop and goes first,
// inside its allocation or not, then the owner's lines by theirs. The sleeve
// is still carried on every row and plan order.
func TestMandateReductionCutsTheLargestPremiumAtRiskWhateverTheSleeve(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SYNA", 8101, 900, 100), mandateReductionStock("SYNB", 8102, 300, 100), mandateReductionStock("IBCF", 8103, 1100, 100))
	for _, coreAllocation := range []float64{0, 0.9} {
		in := mandateAllocationedFixture(t, pos, 1.3, coreAllocation, 8103)
		v := in.Result.Verdict
		core, _ := v.Allocation(mandate.SleeveCore)
		if (coreAllocation == 0) != core.Over {
			t.Fatalf("fixture: core allocation %.1f, core %+v", coreAllocation, core)
		}
		rows, st := mandateReductionRun(t, in, pos)
		if st.State != rpc.MandateReductionReducing || len(rows) == 0 {
			t.Fatalf("state %s rows %d: %s", st.State, len(rows), st.Reason)
		}
		assertMandateRowsReduceOnly(t, rows, pos)
		first := rows[0]
		if first.Contract.ConID != 8103 || first.Mandate.Rank != 1 || first.Mandate.Sleeve != mandate.SleeveCore || first.Mandate.Phase != rpc.MandatePhasePremium ||
			!strings.Contains(first.Reason, "the cheapest relief left, IBCF in the core's sleeve") {
			t.Fatalf("core allocation %.1f: first row %d %+v: %s", coreAllocation, first.Contract.ConID, first.Mandate, first.Reason)
		}
		for i, row := range rows[1:] {
			if row.Mandate.Sleeve != mandate.SleeveOwner || mandateRowRelief(in, row) > mandateRowRelief(in, rows[i])+1e-9 {
				t.Fatalf("after the core, the owner's lines cheapest relief first: %+v", row.Mandate)
			}
		}
		if st.Plan[0].Sleeve != mandate.SleeveCore || st.Plan[0].LossBase != first.Mandate.LegLossBase {
			t.Fatalf("plan %+v", st.Plan)
		}
		// The same order without any allocation plan.
		plain, _ := mandateReductionRun(t, mandateReductionFixture(t, pos, 1.3, 1e9, 2), pos)
		if len(plain) != len(rows) || plain[0].Contract.ConID != 8103 || plain[0].Mandate.Units != first.Mandate.Units {
			t.Fatalf("without allocations: %+v", plain)
		}
	}
}

// With the machine holding nothing, as today, the batch is the old one.
func TestMandateReductionWithAnEmptyMachineIsUnchanged(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SYNA", 8401, 900, 100), mandateReductionStock("SYNB", 8402, 400, 100), mandateReductionStock("SYNC", 8403, 500, 100))
	old := mandateReductionFixture(t, pos, 1.4, 1e9, 2)
	allocationed := mandateAllocationedFixture(t, pos, 1.4, 0)
	a, _ := mandateReductionRun(t, old, pos)
	b, _ := mandateReductionRun(t, allocationed, pos)
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("rows %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Contract.ConID != b[i].Contract.ConID || a[i].Quantity != b[i].Quantity || a[i].Mandate.Units != b[i].Mandate.Units {
			t.Fatalf("row %d: %d×%d vs %d×%d", i, a[i].Contract.ConID, a[i].Mandate.Units, b[i].Contract.ConID, b[i].Mandate.Units)
		}
	}
}

// Property: on synthetic books with a machine allocation, the batch reaches
// the target wherever the plain book's does, and every row the batch may
// send alone lowers the loss measure and the book's risk.
func TestMandateReductionAllocationsNeverStopShort(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	for trial := range 16 {
		pos := mandateReductionBook(mandateReductionStock("SYNA", 8501, float64(100+r.IntN(900)), 100), mandateReductionStock("SYNB", 8502, float64(100+r.IntN(600)), 100),
			mandateReductionStock("SYNC", 8503, float64(100+r.IntN(900)), 100), mandateReductionStock("IBCF", 8504, float64(200+r.IntN(2800)), 100))
		ratio := 1.05 + r.Float64()
		probe := mandateAllocationedFixture(t, pos, ratio, 0, 8504)
		coreUse, _ := probe.Result.Verdict.Allocation(mandate.SleeveCore)
		allocation := r.Float64() * 1.5 * coreUse.UseBase / probe.Result.Verdict.Capital.BudgetAnnualSD
		in := mandateAllocationedFixture(t, pos, ratio, min(allocation, 0.9), 8504)
		_, st := mandateReductionRun(t, in, pos)
		_, old := mandateReductionRun(t, mandateReductionFixture(t, pos, ratio, 1e9, 2), pos)
		if old.TargetReached && !st.TargetReached {
			t.Fatalf("trial %d: the old order reaches the target, the allocations' does not: %s", trial, st.Reason)
		}
		rows, _ := mandateReductionRun(t, in, pos)
		before, lossBefore := mandateRiskAfter(t, in, nil), mandateLossAfter(t, in, nil)
		for _, row := range rows {
			cut := map[int]float64{row.Contract.ConID: float64(row.Quantity)}
			if after := mandateRiskAfter(t, in, cut); after >= before {
				t.Fatalf("trial %d: selling %d of %d alone leaves the risk %.0f, not under %.0f", trial, row.Quantity, row.Contract.ConID, after, before)
			}
			if after := mandateLossAfter(t, in, cut); after >= lossBefore {
				t.Fatalf("trial %d: selling %d of %d alone leaves the loss measure %.0f, not under %.0f", trial, row.Quantity, row.Contract.ConID, after, lossBefore)
			}
		}
	}
}

// Owner decision 2026-10-18 15:31 CEST, to "Should a sleeve inside its own
// allocation keep adding while another sleeve is over its own?": "yes, it
// should. unless you advise differently". The book over budget no longer
// stops a sleeve inside its own allocation: here the core adds while the
// owner's sleeve is far over its own. The owner's sleeve over its
// allocation still adds nothing, and the core still stops at its own.
func TestMandateGateLetsASleeveInsideItsAllocationAddWhileAnotherIsOver(t *testing.T) {
	srv := coreTestServer(t, testCoreConstitution(), mandateTestPositions(3000), 0.25)
	res := srv.evaluateMandate(t.Context(), false)
	v := res.Verdict
	owner, _ := v.Allocation(mandate.SleeveOwner)
	coreLine, _ := v.Allocation(mandate.SleeveCore)
	if v.Status != mandate.StatusOver && v.Status != mandate.StatusReduce || !owner.Over || coreLine.Over || coreLine.AllocationBase <= 0 {
		t.Fatalf("fixture: %s, owner %+v, core %+v", v.Status, owner, coreLine)
	}
	buy := func(q int) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: q, LimitPrice: 100, Contract: rpc.ContractParams{ConID: coreTestConID, Symbol: core.Symbol, SecType: "STK", Currency: core.Currency}}
	}
	if blocker := srv.mandateBudgetGate(buy(10), mandateOpen); blocker != nil {
		t.Fatalf("the core inside its allocation, with the owner's sleeve over its own: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "your sleeve carries") || !strings.Contains(blocker.Message, "over its allocation") {
		t.Fatalf("the owner's sleeve over its allocation adds nothing: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(buy(int(math.Ceil(2*coreLine.AllocationBase/(0.16*100)))), mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "the core's sleeve would carry") {
		t.Fatalf("the core past its own allocation: %+v", blocker)
	}
	// The gate view reads the owner's additions: refused, naming the sleeve.
	if !res.Gate.RefusesAdditions || !strings.Contains(res.Gate.Reason, "your sleeve carries") {
		t.Fatalf("gate view %+v", res.Gate)
	}
}

// And the other way round: the owner's sleeve inside its allocation adds
// while the core holds its fund past its own.
func TestMandateGateLetsTheOwnerAddWhileTheCoreIsOver(t *testing.T) {
	c := testCoreConstitution()
	c.Mandate.Core.ScalePct = new(25.0)
	srv := coreTestServer(t, c, withIBCF(mandateTestPositions(100), 2200, 100), 0.25)
	res := srv.evaluateMandate(t.Context(), false)
	v := res.Verdict
	owner, _ := v.Allocation(mandate.SleeveOwner)
	coreLine, _ := v.Allocation(mandate.SleeveCore)
	if v.Status != mandate.StatusOver && v.Status != mandate.StatusReduce || owner.Over || !coreLine.Over {
		t.Fatalf("fixture: %s, owner %+v, core %+v", v.Status, owner, coreLine)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil {
		t.Fatalf("the owner inside its allocation, with the core over its own: %+v", blocker)
	}
	if res.Gate.RefusesAdditions || !strings.Contains(res.Gate.Reason, "your sleeve is inside its allocation") {
		t.Fatalf("gate view %+v", res.Gate)
	}
}

// What still holds every sleeve while the book is over budget: an unknown
// verdict, a gap-cap breach the order would create, and the maximum loss,
// where the book goes to its hedges.
func TestMandateGateOverBudgetStillHolds(t *testing.T) {
	buy := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 10, LimitPrice: 100, Contract: rpc.ContractParams{ConID: coreTestConID, Symbol: core.Symbol, SecType: "STK", Currency: core.Currency}}
	t.Run("unknown", func(t *testing.T) {
		srv := coreTestServer(t, testCoreConstitution(), mandateTestPositions(3000), 0.25)
		srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return nil, errors.New("synthetic read failure") }
		if v := srv.evaluateMandate(t.Context(), false).Verdict; v.Status != mandate.StatusUnknown {
			t.Fatalf("fixture: %s", v.Status)
		}
		if blocker := srv.mandateBudgetGate(buy, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "cannot be measured") {
			t.Fatalf("unknown: %+v", blocker)
		}
	})
	t.Run("gap cap", func(t *testing.T) {
		c := testCoreConstitution()
		c.Mandate.IndexGapShockPct = new(60.0) // the fund's gap binds before its allocation
		srv := coreTestServer(t, c, mandateTestPositions(3000), 0.25)
		v := srv.evaluateMandate(t.Context(), false).Verdict
		if v.Status != mandate.StatusOver && v.Status != mandate.StatusReduce {
			t.Fatalf("fixture: %s", v.Status)
		}
		big := buy
		big.Quantity = 400
		blocker := srv.mandateBudgetGate(big, mandateOpen)
		if blocker == nil || !strings.Contains(blocker.Message, "gap in "+core.Symbol) {
			t.Fatalf("a gap-cap breach the order creates: %+v", blocker)
		}
	})
	t.Run("maximum loss", func(t *testing.T) {
		c := testCoreConstitution()
		srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
		observeWithPnL(srv.riskCapital, c, 300000, 0, mandateTestNow.Add(-90*time.Second))
		observeWithPnL(srv.riskCapital, c, 220000, -80000, mandateTestNow.Add(-time.Minute))
		v := srv.evaluateMandate(t.Context(), false).Verdict
		if coreLine, _ := v.Allocation(mandate.SleeveCore); v.Capital.Room != 0 || coreLine.Over {
			t.Fatalf("fixture: room %g, core %+v", v.Capital.Room, coreLine)
		}
		if blocker := srv.mandateBudgetGate(buy, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "at the maximum loss no sleeve adds") {
			t.Fatalf("at the maximum loss: %+v", blocker)
		}
	})
}
