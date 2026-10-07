package risk

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func ownerOrderLimits() *ConstitutionOrderLimits {
	return &ConstitutionOrderLimits{
		MaxOrderFloorBase: new(10000.0), MaxOrderPctNLV: new(5.0), MaxOrderCeilingBase: new(100000.0),
		MaxOptionContracts: new(5), AllowStockShort: new(false), AllowOptionSellToOpen: new(false),
		MaxBondMaturityYears: new(30),
	}
}

// The owner's worked check (2026-10-05 19:56 CEST): cap in force =
// min(ceiling, max(floor, pct × NLV)), the floor when NLV is unreadable.
func TestEvaluateOrderLimitsWorkedCheck(t *testing.T) {
	asOf := time.Date(2026, 10, 5, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		nlv     OrderLimitsNLV
		cap     float64
		bound   string
		summary string
	}{
		{"pct binds", OrderLimitsNLV{Base: 240000, AsOf: asOf}, 12000, OrderCapBoundPctNLV, "12,000 EUR (5% of NLV 240,000 EUR; [order_limits])"},
		{"floor binds", OrderLimitsNLV{Base: 150000, AsOf: asOf}, 10000, OrderCapBoundFloor, "10,000 EUR (the floor; 5% of NLV 150,000 EUR is 7,500 EUR; [order_limits])"},
		{"ceiling binds", OrderLimitsNLV{Base: 2500000, AsOf: asOf}, 100000, OrderCapBoundCeiling, "100,000 EUR (the ceiling; 5% of NLV 2,500,000 EUR would be 125,000 EUR; [order_limits])"},
		{"NLV unreadable fails toward the floor", OrderLimitsNLV{Unavailable: "no current account reading of net liquidation value"}, 10000, OrderCapBoundFloor,
			"10,000 EUR (the floor: no current account reading of net liquidation value, so 5% of NLV cannot apply and the smaller cap holds; [order_limits])"},
		{"a zero NLV is unreadable", OrderLimitsNLV{}, 10000, OrderCapBoundFloor, "net liquidation value cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateOrderLimits(ownerOrderLimits(), "eur", tc.nlv, nil, "")
			if !got.Complete || got.CapBase != tc.cap || got.CapBound != tc.bound || !strings.Contains(got.Summary, tc.summary) {
				t.Fatalf("limits = %+v, want cap %v bound %s summary containing %q", got, tc.cap, tc.bound, tc.summary)
			}
			if got.BaseCurrency != "EUR" || got.MaxOptionContracts != 5 || got.AllowStockShort || got.AllowOptionSellToOpen || got.MaxBondMaturityYears != 30 {
				t.Fatalf("limits = %+v, want the table's gates in EUR", got)
			}
			if (got.NLVBase == nil) != (tc.nlv.Base <= 0 || tc.nlv.Unavailable != "") || (got.NLVBase == nil && got.NLVUnavailable == "") {
				t.Fatalf("NLV disclosure = %v %q", got.NLVBase, got.NLVUnavailable)
			}
		})
	}
}

// A missing key is never defaulted: the limits are incomplete and name it.
func TestEvaluateOrderLimitsMissingKeyFailsClosed(t *testing.T) {
	table := ownerOrderLimits()
	table.MaxOrderPctNLV = nil
	got := EvaluateOrderLimits(table, "EUR", OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
	if got.Complete || got.CapBase != 0 || len(got.Missing) != 1 || got.Missing[0] != "order_limits.max_order_pct_nlv" ||
		!strings.Contains(got.Summary, "does not write order_limits.max_order_pct_nlv; every order preview is refused") {
		t.Fatalf("limits = %+v, want incomplete naming max_order_pct_nlv", got)
	}
	none := EvaluateOrderLimits(nil, "EUR", OrderLimitsNLV{}, nil, "no risk policy is loaded")
	if none.Complete || len(none.Missing) != len(OrderLimitKeys())-1 || !none.BondMaturityUnset || !strings.HasPrefix(none.Summary, "no risk policy is loaded; every order preview is refused") {
		t.Fatalf("no table = %+v", none)
	}
}

// A floor override lifts the cap in force to the ceiling until it expires.
func TestEvaluateOrderLimitsFloorOverrideLiftsToCeiling(t *testing.T) {
	until := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	got := EvaluateOrderLimits(ownerOrderLimits(), "EUR", OrderLimitsNLV{Unavailable: "stale"}, &OrderLimitsOverride{ID: "ov-1", ExpiresAt: until}, "")
	if got.CapBase != 100000 || got.CapBound != OrderCapBoundOverride || got.OverrideID != "ov-1" || !strings.Contains(got.Summary, "override ov-1 lifts the floor to the ceiling") {
		t.Fatalf("override limits = %+v", got)
	}
}

func TestOrderLimitsValidation(t *testing.T) {
	base := func() Constitution {
		return Constitution{Kind: ConstitutionKind, SchemaVersion: 2, PolicyID: "t", PolicyVersion: 1, OrderLimits: ownerOrderLimits()}
	}
	if err := (Constitution{Kind: ConstitutionKind, SchemaVersion: 2, PolicyID: "t", PolicyVersion: 1}).Validate(); err != nil {
		t.Fatalf("a constitution without [order_limits] loads (orders are refused, the file is not): %v", err)
	}
	for name, edit := range map[string]func(*ConstitutionOrderLimits){
		"zero floor":              func(o *ConstitutionOrderLimits) { o.MaxOrderFloorBase = new(0.0) },
		"pct above 100":           func(o *ConstitutionOrderLimits) { o.MaxOrderPctNLV = new(101.0) },
		"negative ceiling":        func(o *ConstitutionOrderLimits) { o.MaxOrderCeilingBase = new(-1.0) },
		"floor above ceiling":     func(o *ConstitutionOrderLimits) { o.MaxOrderFloorBase = new(200000.0) },
		"zero option contracts":   func(o *ConstitutionOrderLimits) { o.MaxOptionContracts = new(0) },
		"zero bond maturity":      func(o *ConstitutionOrderLimits) { o.MaxBondMaturityYears = new(0) },
		"bond maturity above 100": func(o *ConstitutionOrderLimits) { o.MaxBondMaturityYears = new(101) },
	} {
		c := base()
		edit(c.OrderLimits)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "order_limits.") {
			t.Errorf("%s: err = %v, want an order_limits validation error", name, err)
		}
	}
}

// Adding the table to the schema must not move an existing policy's
// fingerprint; writing it does.
func TestOrderLimitsFingerprintOnlyMovesWhenWritten(t *testing.T) {
	c := Constitution{Kind: ConstitutionKind, SchemaVersion: 2, PolicyID: "t", PolicyVersion: 1}
	before, beforeEffective := c.FingerprintKey(), c.EffectiveFingerprintKey()
	c.OrderLimits = ownerOrderLimits()
	if c.FingerprintKey() == before || c.EffectiveFingerprintKey() == beforeEffective {
		t.Fatal("writing [order_limits] left the fingerprint unchanged")
	}
	c.OrderLimits = nil
	if c.FingerprintKey() != before || c.EffectiveFingerprintKey() != beforeEffective {
		t.Fatal("an absent [order_limits] moved the fingerprint")
	}
}

// Adding max_bond_maturity_years to the schema must not move the
// fingerprint of a table written before it existed: an unset key stays out
// of the JSON projection both fingerprints hash; writing it moves them.
func TestBondMaturityKeyOnlyMovesTheFingerprintWhenWritten(t *testing.T) {
	table := ownerOrderLimits()
	table.MaxBondMaturityYears = nil
	if raw, err := json.Marshal(table); err != nil || strings.Contains(string(raw), OrderLimitMaxBondMaturityYears) {
		t.Fatalf("an unset bond maturity key reaches the projection: %s %v", raw, err)
	}
	c := Constitution{Kind: ConstitutionKind, SchemaVersion: 2, PolicyID: "t", PolicyVersion: 1, OrderLimits: table}
	before, beforeEffective := c.FingerprintKey(), c.EffectiveFingerprintKey()
	written := *table
	written.MaxBondMaturityYears = new(30)
	c.OrderLimits = &written
	if c.FingerprintKey() == before || c.EffectiveFingerprintKey() == beforeEffective {
		t.Fatal("writing max_bond_maturity_years left the fingerprint unchanged")
	}
}

// Owner decision 2026-10-07 08:27 CEST: a table without the bond maturity
// key refuses bond buys only. Every other order is judged as usual, so the
// missing key never blocks an exit or a stop.
func TestEvaluateOrderLimitsMissingBondMaturityRefusesBondBuysOnly(t *testing.T) {
	table := ownerOrderLimits()
	table.MaxBondMaturityYears = nil
	got := EvaluateOrderLimits(table, "EUR", OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
	if !got.Complete || len(got.Missing) != 0 || !got.BondMaturityUnset || got.CapBase != 12000 {
		t.Fatalf("limits = %+v, want complete with the bond limit unset", got)
	}
	if err := got.CheckBondMaturity("2030-01-01", time.Now()); err == nil || !strings.Contains(err.Error(), "max_bond_maturity_years is not set: bond buys are refused") {
		t.Fatalf("an unset limit judged a maturity: %v", err)
	}
	if keys := table.MissingKeys(); len(keys) != 1 || keys[0] != "order_limits.max_bond_maturity_years" {
		t.Fatalf("the migration's list = %v, want the bond key", keys)
	}
	// A written limit is reported even while another key is missing.
	table = ownerOrderLimits()
	table.MaxOrderPctNLV = nil
	if partial := EvaluateOrderLimits(table, "EUR", OrderLimitsNLV{}, nil, ""); partial.Complete || partial.MaxBondMaturityYears != 30 || partial.MaxOptionContracts != 5 || partial.BondMaturityUnset {
		t.Fatalf("partial = %+v, want the written limits reported", partial)
	}
}

// Owner decision B2 (2026-10-06 20:17 CEST): a bond buy may mature at most
// 30 years ahead. The limit date itself passes; one day later refuses.
func TestCheckBondMaturity(t *testing.T) {
	now := time.Date(2026, 10, 6, 22, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	l := EvaluateOrderLimits(ownerOrderLimits(), "EUR", OrderLimitsNLV{}, nil, "")
	for _, tc := range []struct {
		maturity, refusal string
	}{
		{"2026-11-15", ""},
		{"2056-10-06", ""},
		{"2056-10-07", "the bond matures 2056-10-07, beyond the 30-year limit in force (2056-10-06; [order_limits].max_bond_maturity_years)"},
		{"2061-05-15", "the bond matures 2061-05-15, beyond the 30-year limit in force (2056-10-06; [order_limits].max_bond_maturity_years)"},
		{"15/05/2061", "is not a YYYY-MM-DD date, so [order_limits].max_bond_maturity_years cannot be applied"},
		{"", "is not a YYYY-MM-DD date"},
	} {
		err := l.CheckBondMaturity(tc.maturity, now)
		if tc.refusal == "" {
			if err != nil {
				t.Errorf("%s: %v, want it admitted", tc.maturity, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.refusal) {
			t.Errorf("%s: err = %v, want %q", tc.maturity, err, tc.refusal)
		}
	}
	if err := (OrderLimitsInForce{Complete: true}).CheckBondMaturity("2030-01-01", now); err == nil || !strings.Contains(err.Error(), "max_bond_maturity_years is not set") {
		t.Fatalf("a zero limit judged a maturity: %v", err)
	}
}

func TestOrderLimitRowsMarkMissingKeys(t *testing.T) {
	rows := ConstitutionLimits(&Constitution{Kind: ConstitutionKind, SchemaVersion: 2, PolicyID: "t", PolicyVersion: 1, Capital: ConstitutionCapital{BaseCurrency: "EUR"}, OrderLimits: ownerOrderLimits()})
	found := map[string]ConstitutionLimit{}
	for _, r := range rows {
		found[r.Key] = r
	}
	if r := found["order_limits.max_order_floor_base"]; r.Value != "10,000 EUR" || r.Source != "file" || r.Enforcement != EnforcementHard {
		t.Fatalf("floor row = %+v", r)
	}
	if r := found["order_limits.max_order_pct_nlv"]; r.Value != "5% of NLV" {
		t.Fatalf("pct row = %+v", r)
	}
	if r := found["order_limits.max_bond_maturity_years"]; r.Value != "30 years" || r.Source != "file" || r.Enforcement != EnforcementHard {
		t.Fatalf("bond maturity row = %+v", r)
	}
	missing := ConstitutionLimits(nil)
	for _, r := range missing {
		if strings.HasPrefix(r.Key, "order_limits.") && (r.Source != "unapproved" || !strings.Contains(r.Meaning, "every order preview is refused")) {
			t.Fatalf("missing row = %+v", r)
		}
	}
}

func TestFormatOrderMoney(t *testing.T) {
	for v, want := range map[float64]string{12000: "12,000 EUR", 2500000: "2,500,000 EUR", 999.5: "999.50 EUR", 0: "0 EUR"} {
		if got := FormatOrderMoney(v, "EUR"); got != want {
			t.Errorf("FormatOrderMoney(%v) = %q, want %q", v, got, want)
		}
	}
}
