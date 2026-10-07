package daemon

import (
	"math"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A synthetic EUR 40,000 fixed-coupon buy: clean price 100, annual coupon
// 5%, and the preview's full EUR 2,000 accrued-interest bound.
func syntheticWorkingBondCommitment() (brokerStateScope, ibkrlib.OrderLifecycleEvent, orderJournalEvent, cashSweepFeeEvidence) {
	scope, order, event, evidence := syntheticWorkingBuyFee()
	order.SecType, order.Currency = "BOND", "EUR"
	order.TotalQuantity, order.Filled, order.Remaining = 40000, 0, 40000
	event.SecType, event.Currency, event.Quantity = order.SecType, order.Currency, order.TotalQuantity
	event.FeeCurrency = order.Currency
	event.Bond = &rpc.OrderBondTerms{Instrument: rpc.OrderBondInstrumentByIdentifier,
		QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100,
		FaceValue: 40000, Coupon: new(5.0), AccruedBound: 2000}
	evidence.Events = []orderJournalEvent{event}
	return scope, order, event, evidence
}

func TestCashCommitmentBondSurvivesRestartAndPartialFill(t *testing.T) {
	scope, order, event, evidence := syntheticWorkingBondCommitment()
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	journal := newTestOrderJournalStore(t, path)
	if err := journal.Append(event); err != nil {
		t.Fatal(err)
	}
	if err := journal.authority.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := newTestOrderJournalStore(t, path)
	events, err := reopened.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Bond == nil || events[0].Bond.Coupon == nil || *events[0].Bond.Coupon != 5 || events[0].Bond.AccruedBound != 2000 {
		t.Fatalf("restart lost the signed bond's cash obligation: %+v", events)
	}
	evidence.Events = events
	order.Filled, order.Remaining = 20000, 20000
	orders := []ibkrlib.OrderLifecycleEvent{order}
	sweep := cashSweepCommitmentsFrom(orders, nil, scope, evidence)
	leveling, unknown := currencyLevelingCommitted(orders, nil, scope, evidence)
	// Already-paid interest is not credited back without settlement proof:
	// EUR 20,000 remaining principal still keeps the full EUR 2,000 bound.
	if len(sweep.Unknown) != 0 || sweep.ByCurrency["EUR"] != 22010 || len(unknown) != 0 || leveling["EUR"] != 22000 {
		t.Fatalf("partial fill lost interest or commission: sweep=%+v leveling=%v unknown=%v", sweep, leveling, unknown)
	}
}

func TestCashCommitmentBondUsesSignedUSDUnits(t *testing.T) {
	scope, order, event, evidence := syntheticWorkingBondCommitment()
	order.Currency, order.TotalQuantity, order.Remaining, order.LimitPrice = "USD", 10, 10, 99.5
	event.Currency, event.FeeCurrency, event.Quantity, event.LimitPrice = "USD", "USD", 10, 99.5
	event.Bond.QuantityUnit, event.Bond.FacePerUnit, event.Bond.FaceValue = rpc.BondQuantityUnitFace1000, 1000, 10000
	event.Bond.Coupon, event.Bond.AccruedBound = new(0.0), 0
	evidence.Events = []orderJournalEvent{event}
	orders := []ibkrlib.OrderLifecycleEvent{order}
	sweep := cashSweepCommitmentsFrom(orders, nil, scope, evidence)
	leveling, unknown := currencyLevelingCommitted(orders, nil, scope, evidence)
	if len(sweep.Unknown) != 0 || sweep.ByCurrency["USD"] != 9960 || len(unknown) != 0 || leveling["USD"] != 9950 {
		t.Fatalf("signed USD face units were not preserved: sweep=%+v leveling=%v unknown=%v", sweep, leveling, unknown)
	}
}

func TestCashCommitmentBondPreservesPlannerFunding(t *testing.T) {
	scope, order, _, evidence := syntheticWorkingBondCommitment()
	orders := []ibkrlib.OrderLifecycleEvent{order}
	in := levelingInput(-50000, 60000)
	in.Committed, in.CommittedUnknown = currencyLevelingCommitted(orders, nil, scope, evidence)
	rows := levelingRows(t, in, "USD")
	spent := 0.0
	for _, row := range rows {
		if !currencyLevelingAdmitted(row, levelingPreview(t, row, 1.16995, 1.17005)) {
			t.Fatal("the bounded repayment should still pass its typed preview gate")
		}
		spent += row.CurrencyLeveling.Spent
	}
	// The old clean-only calculation spent EUR 19,689 and could create a
	// fresh EUR debit when the bond also charged accrued interest.
	if after := 60000 - 42000 - spent; after < *levelingPolicy().CushionBase {
		t.Fatalf("repayment spends %.2f, leaving %.2f after the bond's full cash obligation", spent, after)
	}
	sweepInput := cashSweepTestInput(map[string]float64{"EUR": 60000})
	sweepInput.Commitments = cashSweepCommitmentsFrom(orders, nil, scope, evidence)
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9), sweepInput, cashSweepTestNow()), "EUR")
	if cp.side != rpc.CashSweepSideInvest || cp.quantity != 12990 {
		t.Fatalf("sweep did not leave its EUR 5,000 floor after principal, interest and commission: %+v", cp)
	}
}

func TestCashCommitmentBondRequiresExactDurableTerms(t *testing.T) {
	for name, change := range map[string]func(*ibkrlib.OrderLifecycleEvent, *cashSweepFeeEvidence){
		"manual order": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events = nil },
		"legacy send":  func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events[0].Bond = nil },
		"unused preview": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) {
			e.Events[0].Type = orderJournalEventPreviewed
		},
		"changed quantity":    func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.TotalQuantity++ },
		"changed limit":       func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.LimitPrice++ },
		"different client":    func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.ClientID++ },
		"different contract":  func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.ConID++ },
		"different reference": func(o *ibkrlib.OrderLifecycleEvent, _ *cashSweepFeeEvidence) { o.OrderRef += "-other" },
		"old day":             func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Now = e.Now.AddDate(0, 0, 1) },
		"other endpoint":      func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Endpoint = "127.0.0.1:4002" },
		"missing coupon":      func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events[0].Bond.Coupon = nil },
		"nonfinite coupon": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) {
			e.Events[0].Bond.Coupon = new(math.NaN())
		},
		"negative coupon":      func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events[0].Bond.Coupon = new(-1.0) },
		"short interest bound": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events[0].Bond.AccruedBound = 1000 },
		"nonfinite interest": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) {
			e.Events[0].Bond.AccruedBound = math.Inf(1)
		},
		"wrong face convention":  func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events[0].Bond.FacePerUnit = 1000 },
		"unsupported instrument": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) { e.Events[0].Bond.Instrument = "unknown" },
		"newer modify lost terms": func(_ *ibkrlib.OrderLifecycleEvent, e *cashSweepFeeEvidence) {
			latest := e.Events[0]
			latest.Type, latest.Bond = orderJournalEventModifyRequested, nil
			e.Events = append(e.Events, latest)
		},
	} {
		t.Run(name, func(t *testing.T) {
			scope, order, _, evidence := syntheticWorkingBondCommitment()
			change(&order, &evidence)
			orders := []ibkrlib.OrderLifecycleEvent{order}
			in := levelingInput(-50000, 60000)
			in.Committed, in.CommittedUnknown = currencyLevelingCommitted(orders, nil, scope, evidence)
			if in.CommittedUnknown["EUR"] == "" && in.CommittedUnknown[""] == "" {
				t.Fatalf("unproven bond interest became spendable: %v", in.Committed)
			}
			if b := levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD"); b != nil {
				t.Fatalf("repayment spent unproven bond funding: %+v", b)
			}
			sweepInput := cashSweepTestInput(map[string]float64{"EUR": 60000})
			sweepInput.Commitments = cashSweepCommitmentsFrom(orders, nil, scope, evidence)
			cp := cashSweepCurrencyOf(t, cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9), sweepInput, cashSweepTestNow()), "EUR")
			if cp.side != "" || len(sweepInput.Commitments.Unknown) == 0 {
				t.Fatalf("sweep spent unproven bond funding: %+v", cp)
			}
		})
	}
}

func TestCashCommitmentBondZeroCouponBillAndFeeIndependence(t *testing.T) {
	scope, order, _, evidence := syntheticWorkingBondCommitment()
	order.SecType, evidence.Events[0].SecType = "BILL", "BILL"
	bond := evidence.Events[0].Bond
	bond.Instrument, bond.Coupon, bond.AccruedBound = cashSweepInstrumentDEBubill, nil, 0
	orders := []ibkrlib.OrderLifecycleEvent{order}
	sweep := cashSweepCommitmentsFrom(orders, nil, scope, evidence)
	leveling, unknown := currencyLevelingCommitted(orders, nil, scope, evidence)
	if len(sweep.Unknown) != 0 || sweep.ByCurrency["EUR"] != 40010 || len(unknown) != 0 || leveling["EUR"] != 40000 {
		t.Fatalf("known zero-coupon bill lost support: sweep=%+v leveling=%v unknown=%v", sweep, leveling, unknown)
	}
	// Leveling deliberately treats commission separately from the bond's
	// principal and accrued interest; its existing contract needs no fee proof.
	evidence.Events[0].FeeUpper = nil
	leveling, unknown = currencyLevelingCommitted(orders, nil, scope, evidence)
	if len(unknown) != 0 || leveling["EUR"] != 40000 {
		t.Fatalf("missing commission erased proven bond principal: %v %v", leveling, unknown)
	}
	if sweep := cashSweepCommitmentsFrom(orders, nil, scope, evidence); len(sweep.Unknown) == 0 {
		t.Fatal("sweep still requires a commission upper bound")
	}
}

func TestCashCommitmentBondQueuedOrUnacknowledgedHoldsFunding(t *testing.T) {
	scope, _, _, evidence := syntheticWorkingBondCommitment()
	queued := queuedAuthRecord{State: rpc.QueuedAuthArmed, Terms: rpc.QueuedAuthTerms{
		AccountID: scope.Account, AccountMode: scope.Mode, Action: rpc.OrderActionBuy,
		Currency: "EUR", MaxQuantity: 40000, WorstPrice: 100, Contract: rpc.ContractParams{SecType: "BOND", Currency: "EUR"}}}
	for _, tc := range []struct {
		name     string
		queued   []queuedAuthRecord
		evidence []cashSweepFeeEvidence
	}{
		{"queued without interest terms", []queuedAuthRecord{queued}, nil},
		{"journal send not acknowledged", nil, []cashSweepFeeEvidence{evidence}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := levelingInput(-50000, 60000)
			in.Committed, in.CommittedUnknown = currencyLevelingCommitted(nil, tc.queued, scope, tc.evidence...)
			if len(in.CommittedUnknown) == 0 || levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD") != nil {
				t.Fatalf("leveling lost an unbounded or unseen bond buy: %v %v", in.Committed, in.CommittedUnknown)
			}
			if sweep := cashSweepCommitmentsFrom(nil, tc.queued, scope, tc.evidence...); len(sweep.Unknown) == 0 {
				t.Fatal("sweep lost an unbounded or unseen bond buy")
			}
		})
	}
}
