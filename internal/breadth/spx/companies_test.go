package spx

import (
	"slices"
	"testing"
	"time"
)

func TestCompaniesOneVoteAndNoMissingRepresentativeFallback(t *testing.T) {
	now := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	w := func(close float64) ConstituentWindow {
		v := slices.Repeat([]float64{100}, 50)
		v[49] = close
		return ConstituentWindow{LastBarAt: "2026-09-24", Closes: v, Bars: []Bar{{Date: "2026-09-23", Close: 100, ObservedAt: now}, {Date: "2026-09-24", Close: close, ObservedAt: now}}}
	}
	members := []string{"GOOG", "GOOGL", "SYN"}
	windows := map[string]ConstituentWindow{"GOOG": w(90), "GOOGL": w(101), "SYN": w(100)}
	p := computeCompanies(members, windows, "2026-09-24", now)
	if p.CompanyCount != 2 || p.Coverage50 != 2 || *p.PctAbove50DMA != 100 || p.Rising != 1 || p.Unchanged != 1 || *p.RisingPct != 50 {
		t.Fatalf("double count or unchanged excluded: %+v", p)
	}
	delete(windows, "GOOGL")
	p = computeCompanies(members, windows, "2026-09-24", now)
	if p.CompanyCount != 2 || p.Coverage50 != 1 || p.CoverageAD != 1 || p.Falling != 0 {
		t.Fatal("missing representative replaced by another class")
	}
	p = computeCompanies(members, windows, "2026-09-23", now)
	if p.PctAbove50DMA != nil || p.CoverageAD != 0 {
		t.Fatal("undated closes or missing previous session fabricated historical inputs")
	}
}
