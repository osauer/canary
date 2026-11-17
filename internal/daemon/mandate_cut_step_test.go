package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The step's bound prices at the touch, lands on the worst price where the
// touch is beyond it, and beyond the spread cap goes no further from the mid
// than its give. A bound without the two additions refuses as before.
func TestBoundedLimitStepsInsideTheWorstPriceAndTheGive(t *testing.T) {
	t.Parallel()
	stock := rpc.ContractParams{Symbol: "SYN", SecType: "STK", Currency: "USD"}
	quote := func(bid, ask float64) rpc.OrderQuoteSnapshot {
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Symbol: "SYN", Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}
	}
	for name, tc := range map[string]struct {
		action   string
		bound    rpc.OrderBoundedLimit
		bid, ask float64
		want     float64
		refused  string
	}{
		"a sale at the bid":                        {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 18.75, MaxSpreadPctOfMid: 2, ClampToWorst: true}, 24.9, 25.1, 24.9, ""},
		"a sale lands on its worst price":          {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 18.75, MaxSpreadPctOfMid: 2, ClampToWorst: true}, 18.4, 18.5, 18.75, ""},
		"without the clamp it is refused":          {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 18.75, MaxSpreadPctOfMid: 2}, 18.4, 18.5, 0, previewBoundedWorstCode},
		"a wide sale goes no further than a give":  {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 18.75, MaxSpreadPctOfMid: 2, MaxGive: 0.3, ClampToWorst: true}, 24, 26, 24.7, ""},
		"a wide sale whose bid is inside the give": {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 18.75, MaxSpreadPctOfMid: 2, MaxGive: 5, ClampToWorst: true}, 24, 26, 24, ""},
		"a wide sale with no give is refused":      {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 18.75, MaxSpreadPctOfMid: 2, ClampToWorst: true}, 24, 26, 0, previewBoundedSpreadCode},
		"the worst price beats a larger give":      {rpc.OrderActionSell, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 24.5, MaxSpreadPctOfMid: 2, MaxGive: 0.8, ClampToWorst: true}, 24, 26, 24.5, ""},
		"a purchase mirrors the sale":              {rpc.OrderActionBuy, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 31, MaxSpreadPctOfMid: 2, MaxGive: 0.3, ClampToWorst: true}, 24, 26, 25.3, ""},
		"a purchase lands on its worst price":      {rpc.OrderActionBuy, rpc.OrderBoundedLimit{Concession: 1, WorstPrice: 25, MaxSpreadPctOfMid: 2, ClampToWorst: true}, 25.9, 26.1, 25, ""},
	} {
		got, err := boundedLimitPrice(tc.action, &tc.bound, nil, stock, quote(tc.bid, tc.ask))
		switch {
		case tc.refused != "" && !previewRefusedWith(err, tc.refused):
			t.Fatalf("%s: price %v, error %v; want a %s refusal", name, got, err, tc.refused)
		case tc.refused == "" && (err != nil || got != tc.want):
			t.Fatalf("%s: price %v, error %v; want %v", name, got, err, tc.want)
		}
	}
}

// The stepping pass takes a cut as still wanted when the breach's row for
// its line is served with the key and episode the order was placed under.
// The planner keeps exactly that row while the line's own sale works: the
// same key, the same episode, behind a part fill and a later verdict.
func TestTheBreachRowStaysServedBehindItsOwnWorkingSale(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	in := mandateReductionFixture(t, pos, 1.1, 1e7, 2)
	rows, _ := mandateReductionRun(t, in, pos)
	placed, ok := mandateRowFor(rows, 9001)
	if !ok || placed.Mandate == nil || placed.Mandate.Episode == "" {
		t.Fatalf("fixture: no row for the first line: %+v", rows)
	}
	filled := mandateReductionBook(mandateReductionStock("SYNA", 9001, 2900, 100), mandateReductionStock("SYNB", 9002, 500, 80))
	next := mandateReductionFixture(t, filled, 1.1, 1e7, 3)
	next.Input.Capital = in.Input.Capital
	next = mandateReductionVerdict(*next.Input, 3)
	next.Previous, next.Episode = rows, placed.Mandate.Episode
	next.WorkingOrder = func(row rpc.PositionView, _ string) (bool, bool) { return row.ConID == 9001, false }
	rows, _ = mandateReductionRun(t, next, filled)
	behind, ok := mandateRowFor(rows, 9001)
	if !ok || behind.Key != placed.Key || behind.Mandate == nil || behind.Mandate.Episode != placed.Mandate.Episode || behind.Bucket != rpc.TradeProposalBucketMandateReduction {
		t.Fatalf("the row behind its working sale: %+v (mandate %+v); placed under key %s, episode %s", behind, behind.Mandate, placed.Key, placed.Mandate.Episode)
	}
}

// The two stepping numbers count only when both are written and the steps
// are not zero, and each has its range.
func TestUnfilledCutStepsReadsBothNumbers(t *testing.T) {
	t.Parallel()
	read := func(after, steps *int) (int, int, bool) {
		m := &risk.ConstitutionMandate{Reduction: &risk.ConstitutionMandateReduction{UnfilledStepAfterMinutes: after, UnfilledSteps: steps}}
		wait, n, ok := m.UnfilledCutSteps()
		return int(wait.Minutes()), n, ok
	}
	if wait, n, ok := read(new(10), new(2)); !ok || wait != 10 || n != 2 {
		t.Fatalf("10 and 2 read as %d, %d, %v", wait, n, ok)
	}
	for name, tc := range map[string][2]*int{"no minutes": {nil, new(2)}, "no steps": {new(10), nil}, "zero steps": {new(10), new(0)}} {
		if _, _, ok := read(tc[0], tc[1]); ok {
			t.Fatalf("%s: stepping reads as written", name)
		}
	}
	var none *risk.ConstitutionMandate
	if _, _, ok := none.UnfilledCutSteps(); ok {
		t.Fatal("no mandate reads as stepping")
	}
}

// Owner decision 2026-11-17 09:33 CET: where the file writes
// [mandate.reduction], daemon start adds the two stepping numbers once, with
// their provenance and a higher policy_version, and changes nothing else. A
// number the owner commented out stays out, and a deleted table stays
// deleted.
func TestMigrationAddsTheStepNumbersToAWrittenReductionTable(t *testing.T) {
	written, _ := migrateMandateDefaultsForTest(t, mandateDefaultsBase())
	c := decodeTestConstitution(t, []byte(written))
	if wait, steps, ok := c.Mandate.UnfilledCutSteps(); !ok || wait.Minutes() != 10 || steps != 2 {
		t.Fatalf("a fresh file reads %v, %d, %v:\n%s", wait, steps, ok, written)
	}
	if v, ok := c.Mandate.CheapLegGivePct(); !ok || v != 2 {
		t.Fatalf("cheap leg give = %g %v", v, ok)
	}

	// The file as the 3 November start left it: the table, one key.
	const stepLines = "unfilled_step_after_minutes = 10  # owner decision 2026-11-17 09:33 CET\nunfilled_steps = 2  # owner decision 2026-11-17 09:33 CET\n"
	if !strings.Contains(written, stepLines) {
		t.Fatalf("the written keys lack their provenance:\n%s", written)
	}
	earlier := strings.Replace(written, stepLines, "", 1)
	earlier = strings.Replace(earlier, "; mandate defaults of 2026-11-17 written once (owner decision 2026-11-17 09:33 CET; a table you delete stays deleted)", "", 1)
	before := decodeTestConstitution(t, []byte(earlier))
	if _, _, ok := before.Mandate.UnfilledCutSteps(); ok || strings.Contains(earlier, "2026-11-17") {
		t.Fatalf("fixture: the earlier file still carries the step numbers:\n%s", earlier)
	}
	out, changes := migrateMandateDefaultsForTest(t, earlier)
	after := decodeTestConstitution(t, []byte(out))
	if wait, steps, ok := after.Mandate.UnfilledCutSteps(); !ok || wait.Minutes() != 10 || steps != 2 {
		t.Fatalf("an existing table gained %v, %d, %v; changes %q", wait, steps, ok, changes)
	}
	if after.PolicyVersion != before.PolicyVersion+1 {
		t.Fatalf("policy_version %d -> %d, want one higher", before.PolicyVersion, after.PolicyVersion)
	}
	if v, ok := after.Mandate.CheapLegGivePct(); !ok || v != 2 {
		t.Fatalf("the written give bound changed: %g %v", v, ok)
	}
	if again, changes := migrateMandateDefaultsForTest(t, out); again != out || len(changes) != 0 {
		t.Fatalf("a second start migrated again: %q", changes)
	}

	off := strings.Replace(earlier, "cheap_leg_give_pct_of_loss = 2.0", "cheap_leg_give_pct_of_loss = 2.0\n# unfilled_steps = 2", 1)
	out, _ = migrateMandateDefaultsForTest(t, off)
	if c := decodeTestConstitution(t, []byte(out)); c.Mandate.Reduction.UnfilledSteps != nil || c.Mandate.Reduction.UnfilledStepAfterMinutes == nil {
		t.Fatalf("a number the owner commented out was written, or the other was not:\n%s", out)
	}

	deleted := strings.Replace(earlier, "[mandate.reduction]\ncheap_leg_give_pct_of_loss = 2.0  # owner decision 2026-11-03 13:29 CET\n", "", 1)
	if deleted == earlier {
		t.Fatalf("fixture: the reduction table is not where expected:\n%s", earlier)
	}
	if out, changes := migrateMandateDefaultsForTest(t, deleted); strings.Contains(out, "unfilled_step") {
		t.Fatalf("the step numbers were written without their table: %q", changes)
	}
}
