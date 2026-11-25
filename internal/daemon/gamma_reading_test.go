package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The summary's primary statement is the reading a monitor prints under the
// signal's name: figures, in a line short enough to print whole. The
// methodology (why SPY and SPX keep separate levels) belongs to the row's
// notes, which already carry it.
func TestGammaPrimaryStatementIsAShortFigureLine(t *testing.T) {
	t.Parallel()
	crossing := func(scope string, spot, zero, gapPct float64) *rpc.GammaZeroComputed {
		return &rpc.GammaZeroComputed{Scope: scope, SpotUnderlying: spot, ZeroGamma: new(zero), GapPct: new(gapPct), GammaSign: "positive", LegCount: 400, GammaTotalAbs: 1e9}
	}
	oneSided := func(scope, sign string, lo, hi float64) *rpc.GammaZeroComputed {
		return &rpc.GammaZeroComputed{Scope: scope, SpotUnderlying: (lo + hi) / 2, GammaSign: sign, SweepLowAbs: lo, SweepHighAbs: hi, LegCount: 400, GammaTotalAbs: 1e9}
	}
	unusable := func(scope string) *rpc.GammaZeroComputed {
		return &rpc.GammaZeroComputed{Scope: scope, GammaSign: "no_data", LegCount: 400}
	}
	combined := func(spx, spy *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
		c := &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeCombined, PerIndex: map[string]*rpc.GammaZeroComputed{}}
		if spx != nil {
			c.PerIndex["SPX"] = spx
		}
		if spy != nil {
			c.PerIndex["SPY"] = spy
		}
		return c
	}
	spx, spy := rpc.GammaZeroScopeSPX, rpc.GammaZeroScopeSPY
	for _, tc := range []struct {
		name    string
		in      *rpc.GammaZeroComputed
		figures []string
	}{
		{"one-sided SPX, SPY crossing", combined(oneSided(spx, "negative", 6119.42, 8279.22), crossing(spy, 731.10, 733.73, -0.36)), []string{"SPX short gamma", "$6,119-$8,279", "SPY zero-gamma $733.73"}},
		{"both crossing", combined(crossing(spx, 7350, 7150.37, 2.8), crossing(spy, 733, 713.05, 2.8)), []string{"SPX zero-gamma $7,150", "SPY zero-gamma $713.05", "long gamma"}},
		{"both one-sided long", combined(oneSided(spx, "positive", 6119.42, 8279.22), oneSided(spy, "positive", 610.5, 826.25)), []string{"SPX long gamma $6,119-$8,279", "SPY long gamma $610.50-$826.25"}},
		{"both unusable", combined(unusable(spx), unusable(spy)), []string{"SPX unavailable", "SPY unavailable"}},
		{"SPX alone", crossing(spx, 7350, 7150.37, 2.8), []string{"SPX zero-gamma $7,150"}},
		{"nothing landed", combined(nil, nil), []string{"unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			summary := buildGammaSummary(tc.in)
			got := summary.PrimaryStatement
			if n := len([]rune(got)); n == 0 || n > 100 {
				t.Fatalf("statement is %d characters, want 1-100: %q", n, got)
			}
			for _, figure := range tc.figures {
				if !strings.Contains(got, figure) {
					t.Errorf("statement %q lacks %q", got, figure)
				}
			}
			if strings.Contains(got, "price scales") || strings.HasSuffix(got, ".") {
				t.Errorf("statement carries prose, not figures: %q", got)
			}
			// The words a statement leaves out stay on the per-index summary.
			for key, item := range summary.PerIndex {
				if item.ZeroGammaStatus == "unavailable" && item.Interpretation == "" {
					t.Errorf("%s: unavailable without its reason", key)
				}
			}
		})
	}
	if !strings.Contains(gammaNotes, "price scales differ") {
		t.Fatal("the gamma row's notes no longer explain why SPY and SPX keep separate levels")
	}
}
