package risk

import (
	"math"
	"strings"
	"testing"
	"time"
)

func fundingFixture(now time.Time) *CashSweepFundingEvidence {
	return &CashSweepFundingEvidence{AsOf: now.Add(-time.Minute), ValidUntil: now.Add(time.Minute), ScenarioFingerprint: "synthetic-calibration-v1",
		FundingNative: map[string]float64{"EUR": 2000, "USD": 10000}, NAVFloorPassed: true, MarginPassed: true}
}

func TestCashSweepReserveSingleCushionAndOverlappingFloors(t *testing.T) {
	now := time.Date(2099, 1, 5, 12, 0, 0, 0, time.UTC)
	rates, floors := map[string]float64{"EUR": 1, "USD": .8}, map[string]float64{"EUR": 5000, "USD": 1000}
	e := fundingFixture(now)
	buffer, reserve, reason := CashSweepReserveAllocation(10000, rates, floors, e, now)
	if reason != "" || buffer["EUR"] != 2000 || buffer["USD"] != 10000 || reserve["EUR"] != 7000 || reserve["USD"] != 20000 {
		t.Fatalf("allocation: buffer=%v reserve=%v reason=%s", buffer, reserve, reason)
	}
	if math.Abs(buffer["EUR"]+buffer["USD"]*.8-10000) > 1e-8 {
		t.Fatal("cushion duplicated")
	}
	// A large existing native floor is preserved, not added to the demand.
	floors["USD"] = 30000
	_, reserve, reason = CashSweepReserveAllocation(10000, rates, floors, e, now)
	if reason != "" || reserve["USD"] != 40000 {
		t.Fatal("existing floor double counted or relaxed")
	}
}

func TestCashSweepReserveAggregateMonotonicWithOverlappingFloors(t *testing.T) {
	now := time.Now().UTC()
	rates := map[string]float64{"EUR": 1, "USD": 1}
	floors := map[string]float64{"EUR": 0, "USD": 100000}
	e := fundingFixture(now)
	e.FundingNative = map[string]float64{"EUR": 100, "USD": 100}
	_, before, _ := CashSweepReserveAllocation(10000, rates, floors, e, now)
	e.FundingNative["USD"] = 1000
	_, after, why := CashSweepReserveAllocation(10000, rates, floors, e, now)
	if why != "" || math.Abs(after["EUR"]+after["USD"]-before["EUR"]-before["USD"]) > 1e-8 {
		t.Fatal("higher need released aggregate cash", before, after, why)
	}
	for eurNeed := 0.; eurNeed < 15000; eurNeed += 1000 {
		for usdFloor := 0.; usdFloor < 15000; usdFloor += 1000 {
			e.FundingNative = map[string]float64{"EUR": eurNeed, "USD": 100}
			floors = map[string]float64{"EUR": 20000, "USD": usdFloor}
			_, reserve, reason := CashSweepReserveAllocation(10000, rates, floors, e, now)
			want := math.Max(floors["EUR"], eurNeed) + math.Max(floors["USD"], 100) + 10000
			if reason != "" || math.Abs(reserve["EUR"]+reserve["USD"]-want) > 1e-8 {
				t.Fatal("additional total cushion absorbed or duplicated", reserve, want, reason)
			}
		}
	}
}

func TestCashSweepReserveUnavailableEvidenceNeverZero(t *testing.T) {
	now := time.Now().UTC()
	rates, floors := map[string]float64{"EUR": 1, "USD": .8}, map[string]float64{"EUR": 5000, "USD": 5000}
	cases := []struct {
		name   string
		mutate func(*CashSweepFundingEvidence)
		code   string
	}{
		{"stale", func(e *CashSweepFundingEvidence) { e.ValidUntil = now }, "reserve_evidence_unavailable"},
		{"future", func(e *CashSweepFundingEvidence) { e.AsOf = now.Add(time.Second) }, "reserve_evidence_unavailable"},
		{"missing provenance", func(e *CashSweepFundingEvidence) { e.ScenarioFingerprint = "" }, "reserve_evidence_unavailable"},
		{"missing USD", func(e *CashSweepFundingEvidence) { delete(e.FundingNative, "USD") }, "reserve_funding_unavailable"},
		{"unobserved funding currency", func(e *CashSweepFundingEvidence) { e.FundingNative["JPY"] = 10000 }, "reserve_funding_unavailable"},
		{"NaN", func(e *CashSweepFundingEvidence) { e.FundingNative["USD"] = math.NaN() }, "reserve_funding_unavailable"},
		{"negative", func(e *CashSweepFundingEvidence) { e.FundingNative["USD"] = -1 }, "reserve_funding_unavailable"},
		{"NAV", func(e *CashSweepFundingEvidence) { e.NAVFloorPassed = false }, "reserve_safety_hold"},
		{"margin", func(e *CashSweepFundingEvidence) { e.MarginPassed = false }, "reserve_safety_hold"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := fundingFixture(now)
			c.mutate(e)
			b, r, why := CashSweepReserveAllocation(10000, rates, floors, e, now)
			if b != nil || r != nil || !strings.HasPrefix(why, c.code) {
				t.Fatalf("unavailable became money: %v %v %s", b, r, why)
			}
		})
	}
	if _, _, reason := CashSweepReserveAllocation(10000, rates, floors, nil, now); !strings.HasPrefix(reason, "reserve_calibration_required") {
		t.Fatal(reason)
	}
	delete(rates, "USD")
	if _, _, reason := CashSweepReserveAllocation(10000, rates, floors, fundingFixture(now), now); !strings.HasPrefix(reason, "reserve_funding_unavailable") {
		t.Fatal(reason)
	}
}

func TestCashSweepReserveZeroDemandAndMonotonicFunding(t *testing.T) {
	now := time.Now().UTC()
	rates, floors := map[string]float64{"EUR": 1, "USD": .8}, map[string]float64{"EUR": 0, "USD": 0}
	e := fundingFixture(now)
	e.FundingNative = map[string]float64{"EUR": 0, "USD": 0}
	b, r, reason := CashSweepReserveAllocation(10000, rates, floors, e, now)
	if reason != "" || b["EUR"] != 10000 || b["USD"] != 0 || r["EUR"] != 10000 {
		t.Fatal(b, r, reason)
	}
	e = fundingFixture(now)
	_, old, _ := CashSweepReserveAllocation(10000, rates, floors, e, now)
	for need := 10000.; need < 20000; need += 100 {
		e.FundingNative["USD"] = need
		_, next, why := CashSweepReserveAllocation(10000, rates, floors, e, now)
		if why != "" || next["USD"] < old["USD"] {
			t.Fatal("greater USD need lowered its own reserve", next, why)
		}
		old = next
	}
}
