package risk

import (
	"math"
	"slices"
	"testing"
	"time"
)

func sweepObservationFixture() CashSweepOperationalInput {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	d := &CashSweepDeliverable{ConID: 201, UnderlyingConID: 101, Currency: "USD", Source: "frozen_synthetic", Right: "P", Expiry: "20261218", Strike: 100, Multiplier: 100,
		AsOf: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), EarliestSettlement: now.AddDate(0, 0, 3), SharesPerContract: 100, ExerciseCashPerContract: 10000}
	return CashSweepOperationalInput{AsOf: now, Source: "frozen_synthetic", PolicyFingerprint: "synthetic-approved-rulebook", TakeoverGapPct: 100, Currencies: []string{"EUR", "USD"},
		Positions: []CashSweepFundingPosition{{ConID: 201, Symbol: "SYNTH", Currency: "USD", SecType: "OPT", Right: "P", Expiry: "20261218", Quantity: -5, Strike: 100, Multiplier: 100, Deliverable: d},
			{ConID: 202, Symbol: "SYNTH", Currency: "USD", SecType: "OPT", Right: "P", Expiry: "20261218", Quantity: 5, Strike: 90, Multiplier: 100}}}
}

func TestCashSweepOperationalExactAssignmentNoProtectiveExerciseCredit(t *testing.T) {
	in := sweepObservationFixture()
	out := ObserveCashSweepOperationalFunding(in)
	if len(out.Obligations) != 2 || out.Obligations[0].GrossPrincipal == nil || *out.Obligations[0].GrossPrincipal != 50000 || out.State != "partial" || out.Obligations[1].GrossPrincipal != nil || !slices.Contains(out.Obligations[1].Gaps, "automatic_exercise_policy_unavailable") {
		t.Fatal(out)
	}
	if !slices.Contains(out.Gaps, "stressed_margin_unavailable") {
		t.Fatal("operational observation masqueraded as reserve calibration")
	}
	in.Positions[0].Deliverable = nil
	out = ObserveCashSweepOperationalFunding(in)
	if out.Obligations[0].GrossPrincipal != nil || out.Obligations[0].IndicativePrincipal == nil || *out.Obligations[0].IndicativePrincipal != 50000 {
		t.Fatal("nominal option principal became verified", out)
	}
}

func TestCashSweepOperationalExactIdentityDeadlinesAndSourceRequired(t *testing.T) {
	for _, kind := range []string{"currency", "expiry", "right", "stale", "deadline", "live-synthetic", "scope", "duplicate", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			in := sweepObservationFixture()
			d := in.Positions[0].Deliverable
			switch kind {
			case "currency":
				d.Currency = "EUR"
			case "expiry":
				d.Expiry = "20261219"
			case "right":
				d.Right = "C"
			case "stale":
				d.ValidUntil = in.AsOf
			case "deadline":
				d.EarliestSettlement = time.Time{}
			case "live-synthetic":
				in.Source = "live_partial"
			case "scope":
				in.ScopeReason = "synthetic wrong account"
			case "duplicate":
				in.Positions = append(in.Positions, in.Positions[0])
			case "overflow":
				d.ExerciseCashPerContract = math.MaxFloat64
			}
			out := ObserveCashSweepOperationalFunding(in)
			for _, c := range out.Currencies {
				if c.Currency == "USD" && c.GrossPrincipal != nil {
					t.Fatal("uncertified assignment presented as complete", out)
				}
			}
		})
	}
}

func TestCashSweepOperationalCoveredSharesConsumedOnce(t *testing.T) {
	in := sweepObservationFixture()
	d := *in.Positions[0].Deliverable
	d.Right = "C"
	first := in.Positions[0]
	first.Quantity = -1
	first.Right = "C"
	first.Deliverable = &d
	second := first
	second.ConID = 203
	d2 := d
	d2.ConID = 203
	second.Deliverable = &d2
	in.Positions = []CashSweepFundingPosition{{ConID: 101, Symbol: "SYNTH", Currency: "USD", SecType: "STK", Quantity: 100, SettledAvailableShares: new(100.), Price: new(100.), PriceAt: in.AsOf.Add(-time.Second)}, first, second}
	out := ObserveCashSweepOperationalFunding(in)
	if len(out.Obligations) != 2 || *out.Obligations[0].CoveredShares != 100 || *out.Obligations[1].CoveredShares != 0 || *out.Obligations[1].GrossPrincipal != 20000 {
		t.Fatal("reused covered shares or credited assignment proceeds", out)
	}
	in.Positions[0].Currency = "EUR"
	out = ObserveCashSweepOperationalFunding(in)
	if out.Obligations[0].GrossPrincipal != nil || !slices.Contains(out.Obligations[0].Gaps, "underlying_currency_conflict") {
		t.Fatal(out)
	}
}
