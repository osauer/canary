package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderTechnicalTextShowsScreeningFields(t *testing.T) {
	t.Parallel()
	price := 100.0
	sma50 := 95.0
	sma200 := 80.0
	ext200 := 0.25
	rs63 := 0.12
	rs126 := 0.18
	atrPct := 0.04
	adv := int64(2_000_000)
	advDollar := 200_000_000.0
	res := &rpc.TechnicalResult{
		Benchmark:    "SPY",
		LookbackDays: 420,
		Rows: []rpc.TechnicalRow{
			{
				Symbol:             "TEST",
				Price:              &price,
				SMA50:              &sma50,
				SMA200:             &sma200,
				PctAbove200DMA:     &ext200,
				RS63D:              &rs63,
				RS126D:             &rs126,
				ATRPct:             &atrPct,
				AvgVolume20D:       &adv,
				AvgDollarVolume20D: &advDollar,
				TrendState:         "uptrend",
				DataQuality:        "ok",
			},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderTechnicalText(env, res); code != 0 {
		t.Fatalf("render code = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"Technical screen", "ADV$20", "RS63", "TEST", "25.0%", "12.0%", "200.0M", "uptrend"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
