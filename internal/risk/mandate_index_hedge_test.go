package risk

import (
	"strings"
	"testing"
)

// The table's presence is the permission (owner decision 2026-10-28 20:07
// CET): absent, or without max_contracts, placing a hedge is refused with
// the table named; a count below one refuses the file; the written key is
// listed as a hard limit.
func TestMandateIndexHedgeTableIsThePermission(t *testing.T) {
	var none *ConstitutionMandate
	if n, why := none.IndexHedgeMaxContracts(); n != 0 || !strings.Contains(why, "[mandate.index_hedge]") {
		t.Fatalf("no mandate: %d %q", n, why)
	}
	m := &ConstitutionMandate{}
	if n, why := m.IndexHedgeMaxContracts(); n != 0 || !strings.Contains(why, "[mandate.index_hedge]") {
		t.Fatalf("no table: %d %q", n, why)
	}
	m.IndexHedge = &ConstitutionMandateIndexHedge{}
	if n, why := m.IndexHedgeMaxContracts(); n != 0 || !strings.Contains(why, "max_contracts") {
		t.Fatalf("no key: %d %q", n, why)
	}
	if err := m.IndexHedge.validate(); err != nil {
		t.Fatalf("an empty table must load (it refuses the hedge, not the file): %v", err)
	}
	m.IndexHedge.MaxContracts = new(0)
	if err := m.IndexHedge.validate(); err == nil {
		t.Fatal("zero contracts accepted")
	}
	m.IndexHedge.MaxContracts = new(25)
	if err := m.IndexHedge.validate(); err != nil {
		t.Fatal(err)
	}
	if n, why := m.IndexHedgeMaxContracts(); n != 25 || why != "" {
		t.Fatalf("written: %d %q", n, why)
	}
	limits := indexHedgeLimits(m, func(key, value, source, meaning, enforcement string) ConstitutionLimit {
		return ConstitutionLimit{Key: key, Value: value, Source: source, Enforcement: enforcement}
	})
	if len(limits) != 1 || limits[0].Key != "mandate.index_hedge.max_contracts" || limits[0].Value != "25" || limits[0].Source != "file" || limits[0].Enforcement != "hard" {
		t.Fatalf("limits = %+v", limits)
	}
}
