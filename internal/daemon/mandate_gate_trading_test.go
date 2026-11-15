//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The final wire guard reads the mandate gate on a risk-bound binding, after
// the drawdown brake, and refuses the addition before any broker call.
func TestMandateWireGuardRefusesAdditionsOverBudget(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(3000), nil }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusReduce {
		t.Fatalf("verdict %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	calls := 0
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error { calls++; return nil }
	auth, binding, err := srv.authorizeBrokerWriteTransaction("", false)
	if err != nil {
		t.Fatal(err)
	}
	eurBuy := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 10, LimitPrice: 80, Contract: rpc.ContractParams{ConID: 9002, Symbol: "SYNB", SecType: "STK", Currency: "EUR"}}
	binding.riskBound, binding.riskDraft, binding.riskPosition = true, eurBuy, mandateOpen
	binding.riskBaseCurrency = "EUR"
	binding.riskNotional = orderNotionalAuthority{QuoteNotional: 800, ContractCurrency: "EUR", BaseNotional: 800, BaseCurrency: "EUR", BasePerContract: 1, EvidenceAt: srv.orderNow(), Source: orderFXSourceIdentity}
	guard, release := srv.brokerWireGuard(binding, auth.Status, false)
	defer release()
	if err := guard(); err == nil || !strings.Contains(err.Error(), "Mandate budget") {
		t.Fatalf("wire guard over budget: %v", err)
	}
	if calls != 0 {
		t.Fatal("the refused order reached the broker")
	}
	// A reduction passes the guard's mandate read.
	sell := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 10, LimitPrice: 80, Contract: rpc.ContractParams{ConID: 9002, Symbol: "SYNB", SecType: "STK", Currency: "EUR"}}
	binding.riskDraft, binding.riskPosition = sell, rpc.OrderPositionImpact{Before: 100, After: 90, Effect: rpc.OrderPositionEffectReduce}
	if err := srv.mandateBudgetGateError(binding.riskDraft, binding.riskPosition); err != nil {
		t.Fatalf("reduction at the guard: %v", err)
	}
}
