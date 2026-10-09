package risk

import (
	"math"
	"testing"
	"time"
)

func stockAddFixture() StockAddInput {
	p := DefaultRulebookPolicy()
	p.SingleNameWatchPct, p.SingleNameActPct = 80, 90
	p.IlliquidWatchPct, p.IlliquidActPct = 80, 90
	return StockAddInput{Policy: &StockAddPolicy{AdmissionContract: StockAddAdmissionV1, MaxStockPctNLV: new(60.), MaxUnderlyingStockPctNLV: new(10.)}, Symbol: "SYNA", ConID: 101, Price: 100, FX: 1, FreeCash: 20000, Fee: 5, OrderCapBase: 20000,
		Rules: RuleInputs{AsOf: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), BaseCurrency: "USD", Account: SourceState{Healthy: true}, Positions: SourceState{Healthy: true}, NLVBase: new(100000.), ExcessLiquidityBase: new(50000.), RiskCapital: &RiskCapitalInput{EffectiveBase: new(100000.)}}, Rulebook: p}
}

func TestStockAddOpenAndIncreaseShareOneSizing(t *testing.T) {
	in := stockAddFixture()
	got := SizeStockAdd(in)
	if got.Quantity != 100 || got.After != 100 || got.Effect != "open_long" {
		t.Fatalf("new position: %+v", got)
	}
	in.CurrentQuantity = 40
	in.StockValueBase = 4000
	in.UnderlyingStockBase = 4000
	in.Rules.Names = []NameInput{{Symbol: "SYNA", StockConID: 101, StockSecType: "STK", UnderlyingSecType: "STK", HasStockLeg: true, StockQuantity: 40, StockMark: 100, StockFXToBase: new(1.), ExposureBase: 4000, ExposureBaseComplete: true, MarketValueBase: 4000}}
	got = SizeStockAdd(in)
	if got.Quantity != 60 || got.After != 100 || got.Effect != "increase_long" {
		t.Fatalf("add: %+v", got)
	}
	if in.Rules.Names[0].StockQuantity != 40 {
		t.Fatal("sizing mutated source positions")
	}
}

func TestStockAddAllCapsAndFees(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*StockAddInput)
		want   int
	}{
		{"cash includes fee", func(in *StockAddInput) { in.FreeCash = 1000 }, 9},
		{"order cap", func(in *StockAddInput) { in.OrderCapBase = 550 }, 5},
		{"allocation", func(in *StockAddInput) { in.StockValueBase = 59600 }, 4},
		{"underlying", func(in *StockAddInput) { in.UnderlyingStockBase = 9800 }, 2},
		{"native FX", func(in *StockAddInput) { in.FX = 2 }, 50},
		{"manual smaller", func(in *StockAddInput) { in.Requested = 3 }, 3},
		{"manual above max", func(in *StockAddInput) { in.Requested = 101 }, 0},
		{"cash below fee", func(in *StockAddInput) { in.FreeCash = 4 }, 0},
		{"not one share", func(in *StockAddInput) { in.FreeCash = 104.99 }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := stockAddFixture()
			tc.change(&in)
			got := SizeStockAdd(in)
			if got.Quantity != tc.want {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestStockAddNeverTreatsMissingOrInvalidAsZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*StockAddInput)
	}{
		{"policy", func(in *StockAddInput) { in.Policy = nil }},
		{"unknown positions", func(in *StockAddInput) { in.Rules.Positions.Healthy = false }},
		{"unknown account", func(in *StockAddInput) { in.Rules.Account.Healthy = false }},
		{"NLV", func(in *StockAddInput) { in.Rules.NLVBase = nil }},
		{"short position", func(in *StockAddInput) { in.CurrentQuantity = -1 }},
		{"nonfinite price", func(in *StockAddInput) { in.Price = math.NaN() }},
		{"overflow FX", func(in *StockAddInput) { in.FX = math.MaxFloat64 }},
		{"missing risk capital", func(in *StockAddInput) { in.Rules.RiskCapital = nil }},
		{"negative fee", func(in *StockAddInput) { in.Fee = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := stockAddFixture()
			tc.change(&in)
			got := SizeStockAdd(in)
			if got.Quantity != 0 || len(got.Blockers) == 0 {
				t.Fatalf("unsafe plan: %+v", got)
			}
		})
	}
}

// Compare the search against every admissible whole quantity. This is an
// independent maximality witness, including a hedge whose risk is nonlinear.
func TestStockAddMaximumMatchesExhaustiveRiskSearch(t *testing.T) {
	for _, kind := range []string{"unhedged", "put", "sibling issuer", "risk capital", "margin", "sell only"} {
		t.Run(kind, func(t *testing.T) {
			in := stockAddFixture()
			in.FreeCash = 100000
			in.Fee = 0
			switch kind {
			case "put":
				in.Rules.Earnings = map[string]EarningsInput{in.Symbol: {Known: true, Date: in.Rules.AsOf.AddDate(0, 1, 0)}}
				in.Rules.Names = []NameInput{{Symbol: in.Symbol, ExposureBaseComplete: true, ExposureBase: -2000, MarketValueBase: 200, Legs: []LegInput{{Right: "P", Strike: 90, Quantity: 1, Multiplier: 100, Mark: 2, Underlying: new(100.), Delta: new(-.2), FXToBase: new(1.), MarketValueBase: 200, Expiry: in.Rules.AsOf.AddDate(0, 6, 0), DTE: 180}}}}
			case "sibling issuer":
				in.Rulebook.IssuerGroups = map[string][]string{"SYN issuer": {in.Symbol, "SYNB"}}
				in.Rulebook.SingleNameWatchPct = 12
				in.Rulebook.IlliquidWatchPct = 12
				in.Rules.Names = []NameInput{stockLine("SYNB", 100, 100)}
			case "risk capital":
				in.Rules.RiskCapital.EffectiveBase = new(4500.)
			case "margin":
				in.Rules.ExcessLiquidityBase = new(15000.)
			case "sell only":
				in.Rulebook.RegimeCalm.NetExposureWatchPct = 6
			}
			want := 0
			for q := 1; q <= 100; q++ {
				if stockAddRiskReason(in, q) == "" {
					want = q
				}
			}
			got := SizeStockAdd(in)
			if got.MaxQuantity != want || got.Quantity != want {
				t.Fatalf("search=%+v exhaustive=%d", got, want)
			}
			if kind != "unhedged" && kind != "put" && want >= 100 {
				t.Fatal("fixture did not exercise a risk limit")
			}
		})
	}
}

func TestStockAddOptionsConsumeIssuerBudgetAndUnknownHedgeCannotCreateRoom(t *testing.T) {
	in := stockAddFixture()
	in.Rulebook.SingleNameWatchPct = 8
	in.Rulebook.IlliquidWatchPct = 8
	plain := SizeStockAdd(in)
	in.Rules.Names = []NameInput{{Symbol: in.Symbol, ExposureBaseComplete: true, ExposureBase: 5000, MarketValueBase: 1000, Legs: []LegInput{{Right: "C", Strike: 100, Quantity: 1, Multiplier: 100, Mark: 10, Underlying: new(100.), Delta: new(.5), FXToBase: new(1.), MarketValueBase: 1000, Expiry: in.Rules.AsOf.AddDate(0, 6, 0), DTE: 180}}}}
	withOption := SizeStockAdd(in)
	if withOption.Quantity >= plain.Quantity {
		t.Fatalf("option premium created room: plain=%+v option=%+v", plain, withOption)
	}
	in.Rules.Names[0].Legs[0].FXToBase = nil
	in.Rules.Names[0].ExposureBaseComplete = false
	if got := SizeStockAdd(in); got.Quantity != 0 || len(got.Blockers) == 0 {
		t.Fatalf("unknown exposure passed: %+v", got)
	}
}

func TestStockAddPolicyIsOptionalExplicitAndFingerprintBound(t *testing.T) {
	c := approvedConstitution()
	before := c.FingerprintKey()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.PositionAdd = &StockAddPolicy{}
	if before == c.FingerprintKey() {
		t.Fatal("enabling Add failed to invalidate policy identity")
	}
	found := false
	for _, k := range c.UnapprovedKeys() {
		if k == "position_add.max_stock_pct_nlv" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing allocation is not disclosed")
	}
	for _, v := range []float64{0, -1, 101, math.NaN(), math.Inf(1)} {
		c.PositionAdd.MaxStockPctNLV = new(v)
		if c.Validate() == nil {
			t.Fatalf("accepted %v", v)
		}
	}
	c.PositionAdd = &StockAddPolicy{AdmissionContract: StockAddAdmissionV1, MaxStockPctNLV: new(60.), MaxUnderlyingStockPctNLV: new(10.)}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	fp := c.FingerprintKey()
	c.PositionAdd.MaxUnderlyingStockPctNLV = new(11.)
	if fp == c.FingerprintKey() {
		t.Fatal("allocation edit did not invalidate review")
	}
	c.PositionAdd = nil
	if before != c.FingerprintKey() {
		t.Fatal("absent Add altered legacy policy identity")
	}
}

func TestStockAddAllocationRoomIsSeparateFromOrderQuantityBound(t *testing.T) {
	in := stockAddFixture()
	in.Price = 0.001
	in.FreeCash = 1e6
	in.Fee = 0
	got := SizeStockAdd(in)
	if got.AllocationRoom != 10000000 || got.OrderUpperBound > 1000000 || len(got.Blockers) != 0 {
		t.Fatalf("position capacity conflated with order bound: %+v", got)
	}
}

func TestStockAddAdmissionApprovalIsDistinctFromAllocationValues(t *testing.T) {
	in := stockAddFixture()
	in.Policy.AdmissionContract = ""
	c := approvedConstitution()
	c.PositionAdd = in.Policy
	before := c.FingerprintKey()
	found := false
	for _, key := range c.UnapprovedKeys() {
		if key == "position_add.admission_contract" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing admission approval was not disclosed")
	}
	if got := SizeStockAdd(in); got.Quantity != 0 || len(got.Blockers) == 0 || got.Blockers[0].Kind != "policy" {
		t.Fatal("allocation values activated admission")
	}
	in.Policy.AdmissionContract = StockAddAdmissionV1
	if before == c.FingerprintKey() {
		t.Fatal("admission approval did not invalidate policy identity")
	}
	in.Policy.AdmissionContract = "guess"
	if c.Validate() == nil {
		t.Fatal("unrecognized admission contract accepted")
	}
}

func TestStockAddAdmissionExplanationPreservesApprovalSource(t *testing.T) {
	c := approvedConstitution()
	c.PositionAdd = &StockAddPolicy{MaxStockPctNLV: new(60.), MaxUnderlyingStockPctNLV: new(10.)}
	for _, approved := range []bool{false, true} {
		want := "unapproved"
		if approved {
			c.PositionAdd.AdmissionContract = StockAddAdmissionV1
			want = "file"
		}
		found := false
		for _, row := range ConstitutionLimits(&c) {
			if row.Key != "position_add.admission_contract" {
				continue
			}
			found = true
			if row.Source != want || row.Value == "" {
				t.Fatalf("approval source lost: %+v", row)
			}
		}
		if !found {
			t.Fatal("admission decision missing from explanation")
		}
	}
}
