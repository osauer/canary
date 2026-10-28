package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestIndexHedgeProjectedRiskCountsPendingSales(t *testing.T) {
	input := func(ratio float64, shares float64) *mandate.Input {
		s := &Server{}
		keepMandateEvaluationAt(t, s, mandateReductionBook(mandateReductionStock("SPY", 9101, shares, 50)), ratio)
		in, _, why := s.mandateCurrentInput()
		if in == nil {
			t.Fatal(why)
		}
		return in
	}
	pending := func(n int, conID int) mandate.Leg {
		return mandate.Leg{ID: mandateConLegID(conID), Symbol: "MESZ6", Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock, Quantity: -float64(n), Multiplier: 5, Currency: "USD", Spot: 6000}
	}
	for _, tc := range []struct {
		name    string
		held    *mandate.Input
		pending []mandate.Leg
		draft   rpc.OrderDraft
		want    string
	}{
		{"no pending", input(0.5, 3000), nil, hedgeSale(5), ""},
		{"remaining units fit", input(0.5, 3000), []mandate.Leg{pending(3, hedgeTestConID)}, hedgeSale(2), ""},
		{"same month passes neutral", input(0.5, 3000), []mandate.Leg{pending(5, hedgeTestConID)}, hedgeSale(1), "index_hedge_past_neutral"},
		{"other month passes neutral", input(0.5, 3000), []mandate.Leg{pending(5, hedgeTestConID+1)}, hedgeSale(1), "index_hedge_past_neutral"},
		{"combined loss exceeds room", input(0.96, 30000), []mandate.Leg{pending(4, hedgeTestConID+1)}, hedgeSale(1), "index_hedge_over_loss_room"},
		{"missing book", nil, nil, hedgeSale(1), "index_hedge_risk_unmeasured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := indexHedgeProjectedRiskRefusal(tc.held, tc.draft, tc.pending)
			if tc.want == "" {
				if b != nil {
					t.Fatalf("unexpected refusal %+v", b)
				}
			} else if b == nil || b.Code != tc.want {
				t.Fatalf("blocker %+v want %s", b, tc.want)
			}
		})
	}
	t.Run("unconfirmed buy cannot release capacity", func(t *testing.T) {
		buy := pending(5, hedgeTestConID)
		buy.Quantity = 5
		if b := indexHedgeProjectedRiskRefusal(input(0.5, 3000), hedgeSale(5), []mandate.Leg{buy}); b == nil {
			t.Fatal("pending buy released hedge capacity")
		}
	})
	t.Run("unknown capital", func(t *testing.T) {
		in := input(0.5, 3000)
		in.Capital.Known = false
		if b := indexHedgeProjectedRiskRefusal(in, hedgeSale(1), nil); b == nil {
			t.Fatal("unknown capital admitted hedge")
		}
	})
	t.Run("gap cap", func(t *testing.T) {
		in := input(0.5, 3000)
		in.Policy.GapRoomPct = 1
		if b := indexHedgeProjectedRiskRefusal(in, hedgeSale(1), nil); b == nil || b.Code != "index_hedge_gap" {
			t.Fatalf("gap result %+v", b)
		}
	})
}
