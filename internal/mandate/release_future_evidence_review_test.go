package mandate

import "testing"

func TestReviewFutureLossRequiresEvidence(t *testing.T) {
	base := Leg{ID: "con:9", Symbol: "MESZ6", Underlying: MicroSP500Future, Kind: KindStock, Quantity: -2, Multiplier: 5, Currency: "USD", Spot: 6000}
	for _, name := range []string{"fx", "spot", "multiplier"} {
		t.Run(name, func(t *testing.T) {
			l := base
			in := testInput(t, nil)
			switch name {
			case "fx":
				delete(in.FXNow, "USD")
			case "spot":
				l.Spot = 0
			case "multiplier":
				l.Multiplier = 0
			}
			in.Legs = []Leg{l}
			m, why := MeasureLoss(in)
			if len(why) == 0 {
				t.Fatalf("missing %s was measured as %+v without reasons; Evaluate=%s", name, m, Evaluate(in).Status)
			}
		})
	}
}

func TestReviewFutureAdditionMatchesHeldBook(t *testing.T) {
	held := Leg{ID: "con:9", Symbol: "MESZ6", Underlying: MicroSP500Future, Kind: KindStock, Quantity: -1, Multiplier: 5, Currency: "USD", Spot: 6000, StopArmed: true, LossPerUnitBase: 450}
	add := held
	add.StopArmed = false
	add.LossPerUnitBase = 0
	scenario, _ := MeasureLoss(testInput(t, []Leg{held, add}))
	after := held
	after.Quantity = -2
	after.StopArmed = false
	after.LossPerUnitBase = 225
	actual, _ := MeasureLoss(testInput(t, []Leg{after}))
	if scenario.TotalBase != actual.TotalBase {
		t.Fatalf("same resulting two-contract book: scenario=%g actual=%g", scenario.TotalBase, actual.TotalBase)
	}
}
