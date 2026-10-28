package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// keepMandateEvaluationAt keeps a verdict of pos whose loss measure reads
// ratio times the loss room (300,000 at a budget base of 1,000,000), with
// the USD rate the hedge's future needs.
func keepMandateEvaluationAt(t *testing.T, s *Server, pos *rpc.PositionsResult, ratio float64) {
	t.Helper()
	keepMandateEvaluation(t, s, pos)
	fixture := mandateReductionFixture(t, pos, ratio, 1000000, 0)
	fixture.Input.FX["USD"] = mandateSeriesInput(mandateTestSeries("fx:USD", 0.08, 9, 0), mandateTestNow)
	if fixture.Input.FXNow == nil {
		fixture.Input.FXNow = map[string]float64{}
	}
	fixture.Input.FXNow["USD"] = 0.9
	s.mandateVerdicts.set(fixture.Result, fixture.Input, mandateTestNow, s.riskPolicies.snapshot().policy.FingerprintKey(), false, s.captureMandateBookBinding())
}

func hedgeSale(qty int) rpc.OrderDraft {
	return rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: qty, LimitPrice: 6000, Source: hedgeOrderSource,
		Contract: rpc.ContractParams{ConID: hedgeTestConID, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218", LocalSymbol: "MESZ6", Multiplier: 5}}
}

// A SELL of the index future is a reduction for the drawdown brake and the
// mandate gate only when, on the kept book with the order, the 10% fall's
// loss is lower and not below zero, the loss measure does not rise and
// stays within the loss room, and no gap cap is breached (owner decision 2026-10-28 20:07 CET).
// 3,000 index shares lose 15,000 in the fall; a contract offsets 2,700.
func TestIndexHedgeSaleIsAReductionOnlyTowardNeutral(t *testing.T) {
	open := rpc.OrderPositionImpact{Before: 0, After: -5, Effect: rpc.OrderPositionEffectOpenShort}
	book := mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50))
	for _, tc := range []struct {
		name   string
		ratio  float64
		qty    int
		adding bool
	}{
		{"to neutral but spending loss room", 0.5, 5, true},
		{"one contract still adds loss", 0.5, 1, true},
		{"past neutral: the book would gain in the fall", 0.5, 6, true},
		{"the hedge's rise loss would pass the loss room", 0.97, 5, true},
	} {
		s := &Server{}
		keepMandateEvaluationAt(t, s, book, tc.ratio)
		if got := s.riskAddingOrder(hedgeSale(tc.qty), open); got != tc.adding {
			t.Errorf("%s: risk-adding = %v, want %v", tc.name, got, tc.adding)
		}
	}
	// Without a current measurement the normal gate applies.
	if !(&Server{}).riskAddingOrder(hedgeSale(5), open) {
		t.Error("an unmeasured hedge sale passed as a reduction")
	}
	// A BUY that closes or shrinks a held hedge is a close.
	s := &Server{}
	keepMandateEvaluationAt(t, s, book, 0.5)
	buy := hedgeSale(2)
	buy.Action, buy.Source = rpc.OrderActionBuy, reduceOrderSource
	if s.riskAddingOrder(buy, rpc.OrderPositionImpact{Before: -2, After: 0, Effect: rpc.OrderPositionEffectClose}) {
		t.Error("buying back the hedge read as risk-adding")
	}
}

// A hedge sale the classification does not pass is judged by the normal
// gate, which prices it as the mandate's own index line rather than
// refusing it unmeasured.
func TestIndexHedgeSaleIsPricedAsTheIndexLine(t *testing.T) {
	s := &Server{}
	book := mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50))
	keepMandateEvaluationAt(t, s, book, 0.5)
	input, _, why := s.mandateCurrentInput()
	if input == nil {
		t.Fatal(why)
	}
	leg, ok := mandateOrderLeg(hedgeSale(5), input)
	if !ok || leg.Underlying != mandate.MicroSP500Future || leg.Kind != mandate.KindStock || leg.Quantity != -5 || leg.Multiplier != 5 || leg.Spot != 6000 || leg.Currency != "USD" || leg.Symbol != "MESZ6" {
		t.Fatalf("leg = %+v ok %v, want the MES FUT line short 5 at 6,000 × 5", leg, ok)
	}
	other := hedgeSale(1)
	other.Contract.Symbol = "ES"
	if _, ok := mandateOrderLeg(other, input); ok {
		t.Fatal("another future was priced")
	}
	if got := mandateOrderUnderlying(hedgeSale(1)); got != mandate.MicroSP500Future {
		t.Fatalf("underlying = %q", got)
	}
}
