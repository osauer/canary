package risk

import (
	"slices"
	"testing"
)

func TestMandateIVFallbackValidatesAndNamesUnwrittenKeys(t *testing.T) {
	m := &ConstitutionMandate{IVFallback: &ConstitutionMandateIVFallback{ShortBandPct: new(65.0), RangePct: new(20.0)}}
	if got := m.IVFallbackUnapprovedKeys(); !slices.Equal(got, []string{"mandate.iv_fallback.month_band_pct", "mandate.iv_fallback.long_band_pct", "mandate.iv_fallback.measured_pct"}) {
		t.Fatalf("unwritten = %v", got)
	}
	if slices.ContainsFunc(m.UnapprovedKeys(), func(k string) bool { return k == "mandate.iv_fallback.measured_pct" }) {
		t.Fatal("a half-written fallback table must not hold the whole mandate")
	}
	for name, f := range map[string]ConstitutionMandateIVFallback{
		"band at 100":         {ShortBandPct: new(100.0)},
		"negative band":       {LongBandPct: new(-1.0)},
		"tier above 100":      {RangePct: new(101.0)},
		"measured over range": {MeasuredPct: new(30.0), RangePct: new(20.0)},
	} {
		if err := f.validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	ok := ConstitutionMandateIVFallback{ShortBandPct: new(65.0), MonthBandPct: new(15.0), LongBandPct: new(8.0), MeasuredPct: new(5.0), RangePct: new(20.0)}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
}
