package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// When the owner trades by hand while a batch waits, the batch keeps only
// the cut that is still needed: the verdict on the old book
// holds what it can still stand behind and drops a row whose position
// moved; the next verdict on the book as it now is serves exactly the cut a
// fresh batch would, never the standing size.
func TestMandateReductionHandSalesLeaveOnlyTheRemainingCut(t *testing.T) {
	t.Parallel()
	book := func(syna, synb float64) *rpc.PositionsResult {
		var rows []rpc.PositionView
		if syna != 0 {
			rows = append(rows, mandateReductionStock("SYNA", 9001, syna, 100))
		}
		rows = append(rows, mandateReductionStock("SYNB", 9002, synb, 80))
		return mandateReductionBook(rows...)
	}
	pos := book(3000, 2500)
	over := mandateReductionFixture(t, pos, 3, 1e7, 2)
	standing, _ := mandateReductionRun(t, over, pos)
	standingSyna, okA := mandateRowFor(standing, 9001)
	if _, okB := mandateRowFor(standing, 9002); !okA || !okB {
		t.Fatalf("the fixture batch needs rows on both lines: %d rows", len(standing))
	}

	for name, tc := range map[string]struct {
		after    *rpc.PositionsResult
		heldSyna bool // the old verdict still holds the SYNA row
	}{
		"part of a batch line sold": {book(2600, 2500), false},
		"part of another line sold": {book(3000, 1500), true},
		"a batch line sold whole":   {book(0, 2500), false},
	} {
		// On the old verdict: the batch holds, a moved line's row leaves.
		in := over
		in.Previous = standing
		rows, st := mandateReductionRun(t, in, tc.after)
		if _, ok := mandateRowFor(rows, 9001); ok != tc.heldSyna {
			t.Fatalf("%s: SYNA row on the old verdict %v, want %v (status %+v)", name, ok, tc.heldSyna, st)
		}
		for _, row := range rows {
			if !row.Mandate.Held {
				t.Fatalf("%s: a row was sized on a verdict of another book: %+v", name, row.Mandate)
			}
		}

		// The next verdict measures the book held: the served cut is the
		// fresh cut, within the one-lot damping a standing row keeps.
		next := mandateReductionVerdict(mandateReductionRelegs(t, over, tc.after), 3)
		fresh, _ := mandateReductionRun(t, next, tc.after)
		next.Previous = standing
		served, _ := mandateReductionRun(t, next, tc.after)
		assertMandateRowsReduceOnly(t, served, tc.after)
		if len(served) != len(fresh) {
			t.Fatalf("%s: served %d rows, a fresh batch has %d", name, len(served), len(fresh))
		}
		for _, row := range served {
			want, ok := mandateRowFor(fresh, row.Contract.ConID)
			if !ok {
				t.Fatalf("%s: served a row a fresh batch does not have: %s", name, row.Symbol)
			}
			if row.Mandate.Units != want.Mandate.Units &&
				!(row.Contract.ConID == 9001 && row.Mandate.Units == standingSyna.Mandate.Units && row.Mandate.Units-want.Mandate.Units <= max(1, want.Mandate.Lot)) {
				t.Fatalf("%s: %s served %d units, fresh %d, standing %d", name, row.Symbol, row.Mandate.Units, want.Mandate.Units, standingSyna.Mandate.Units)
			}
			if row.PositionQuantity != positionQuantity(tc.after, row.Contract.ConID) {
				t.Fatalf("%s: %s sized on position %g, held %g", name, row.Symbol, row.PositionQuantity, positionQuantity(tc.after, row.Contract.ConID))
			}
		}
		if _, ok := mandateRowFor(served, 9001); !ok && tc.after.Stocks[0].ConID == 9001 && len(fresh) > 0 {
			if _, freshOK := mandateRowFor(fresh, 9001); freshOK {
				t.Fatalf("%s: SYNA dropped though a fresh batch cuts it", name)
			}
		}
	}
}

func positionQuantity(pos *rpc.PositionsResult, conID int) float64 {
	for _, row := range append(append([]rpc.PositionView(nil), pos.Stocks...), pos.Options...) {
		if row.ConID == conID {
			return row.Quantity
		}
	}
	return 0
}

// A hand sale in the middle of the veto window shrinks a waiting row: the
// re-sized row keeps the clock its notice started, so the owner's sale
// neither restarts the 30 minutes nor sends early; the row of a line sold
// whole is superseded unsent.
func TestMandateReductionHandSaleMidWindowKeepsTheClock(t *testing.T) {
	t.Parallel()
	rig := newAutomaticTestRig(t, automaticMandateAuthority)
	a := rig.mandateProposal(automaticTestStockRow(), 20, 1, 2)
	b := rig.mandateProposal(automaticTestStockRowB(), 10, 2, 2)
	rev := rig.install(a, b)
	rig.cycle()
	recA, recB := rig.record(a.Key, rev), rig.record(b.Key, rev)
	rig.notice(recA)
	rig.notice(recB)
	recA = rig.record(a.Key, rev)

	rig.advance(10 * time.Minute)
	sold := automaticTestStockRow()
	sold.Quantity = 32 // eight sold by hand; the other line sold whole
	shrunk := rig.mandateProposal(sold, 12, 1, 1)
	rev2 := rig.install(shrunk)
	rig.cycle()

	if old := rig.record(a.Key, rev); old.State != rpc.TradeProposalAutomaticSuperseded || old.Reason != automaticEpisodeResizedReason {
		t.Fatalf("the standing row's record: %s %q", old.State, old.Reason)
	}
	if gone := rig.record(b.Key, rev); gone.State != rpc.TradeProposalAutomaticSuperseded {
		t.Fatalf("the record of the line sold whole: %s %q", gone.State, gone.Reason)
	}
	rec := rig.record(shrunk.Key, rev2)
	if rec.State != rpc.TradeProposalAutomaticPending || !rec.SubmitAt.Equal(recA.SubmitAt) || !rec.NoticedAt.Equal(recA.NoticedAt) || rec.Quantity != 12 {
		t.Fatalf("the shrunk row's record %+v, want the clock of %+v", rec, recA)
	}
	rig.cycle()
	if rig.revalidations != 0 {
		t.Fatalf("a row was attempted before its window ended: %d", rig.revalidations)
	}
}
